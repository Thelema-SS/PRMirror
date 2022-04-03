package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/github"
)

// MirroredPR contains the upstream and downstream PR ids
type MirroredPR struct {
	DownstreamID int
	UpstreamID   int
}

// PRMirror contains various different variables
type PRMirror struct {
	GitHubClient  *github.Client
	Context       *context.Context
	Configuration *Config
	Database      *Database
	GitLock       *SpinLock
}

// GitHubEventMonitor passes in an instance of the PRMirror struct to all HTTP calls to the webhook listener
type GitHubEventMonitor struct {
	Mirrorer PRMirror
}

// HandleEvent handles github events and acts like an event handler
func (p PRMirror) HandleEvent(event *github.Event) {
	eventType := event.GetType()
	if eventType == "PullRequestEvent" && event.GetRepo().GetName() == p.Configuration.UpstreamOwner+"/"+p.Configuration.UpstreamRepo {
		seenEvent, _ := p.Database.SeenPrEvent(event.GetID())
		if seenEvent {
			return
		}

		prEvent := github.PullRequestEvent{}
		err := json.Unmarshal(event.GetRawPayload(), &prEvent)
		if err != nil {
			panic(err)
		}

		p.HandlePREvent(&prEvent)
		p.Database.AddPrEvent(event.GetID())
	} else if eventType == "IssueCommentEvent" && event.GetRepo().GetName() == p.Configuration.DownstreamOwner+"/"+p.Configuration.DownstreamRepo {
		seenEvent, _ := p.Database.SeenCommentEvent(event.GetID())
		if seenEvent {
			return
		}

		prComment := github.IssueCommentEvent{}
		err := json.Unmarshal(event.GetRawPayload(), &prComment)
		if err != nil {
			panic(err)
		}

		if !prComment.GetIssue().IsPullRequest() {
			return
		}

		p.HandlePRComment(&prComment)
		p.Database.AddCommentEvent(event.GetID())
	}
}

// HandlePREvent handles PR events
func (p PRMirror) HandlePREvent(prEvent *github.PullRequestEvent) {
	//repoName := prEvent.Repo.GetName()
	//repoOwner := prEvent.Repo.Owner.GetName()
	prEventURL := prEvent.PullRequest.GetURL()

	//if p.Configuration.UseWebhook repoName != p.Configuration.DownstreamRepo {
	//	log.Warningf("Ignoring PR Event: %s because %s != %s\n", prEventURL, repoName, p.Configuration.UpstreamRepo)
	//	return
	//} //else if repoOwner != p.Configuration.DownstreamOwner {
	//log.Warningf("Ignoring PR Event: %s because %s != %s\n", prEventURL, repoOwner, p.Configuration.UpstreamOwner)
	//return
	//}

	log.Debugf("Handling PR Event: %s\n", prEventURL)

	prAction := prEvent.GetAction()
	if prAction == "closed" && prEvent.PullRequest.GetMerged() == true && prEvent.PullRequest.Base.GetRef() == "master" {
		prID, err := p.MirrorPR(prEvent.PullRequest)
		if err != nil {
			log.Errorf("Error while creating a new PR: %s\n", err.Error())
		} else {
			if p.Configuration.AddLabel {
				p.AddLabels(prID, []string{"Upstream PR Merged"})
			}
			p.Database.StoreMirror(prID, prEvent.PullRequest.GetNumber())
		}
	}
}

// HandlePRComment handles comment events
func (p PRMirror) HandlePRComment(prComment *github.IssueCommentEvent) {
	prCommentURL := prComment.GetIssue().GetURL()

	log.Debugf("Handling PR Comment: %s\n", prCommentURL)

	comment := prComment.GetComment()
	rank := comment.GetAuthorAssociation()
	if !(rank == "COLLABORATOR" || rank == "MEMBER" || rank == "OWNER") {
		return
	}

	body := comment.GetBody()

	if strings.HasPrefix(body, "remirror") {
		id := prComment.GetIssue().GetNumber()
		pr, _, err := p.GitHubClient.PullRequests.Get(*p.Context, p.Configuration.DownstreamOwner, p.Configuration.DownstreamRepo, id)
		if err != nil {
			log.Errorf("Error while getting downstream PR for remirror: %s\n", err.Error())
			return
		}
		body := pr.GetBody()
		temp := strings.Split(body, "/")
		temp2 := strings.Split(temp[6], "\n")
		id, err = strconv.Atoi(temp2[0])
		if err != nil {
			log.Errorf("Error while parsing downstream PR body, incorrect format: %s\n", err.Error())
			return
		}

		pr, _, err = p.GitHubClient.PullRequests.Get(*p.Context, p.Configuration.UpstreamOwner, p.Configuration.UpstreamRepo, id)
		if err != nil {
			log.Errorf("Error while getting upstream PR to remirror: %s\n", err.Error())
			return
		}

		_, err = p.RemirrorPR(pr)
		if err != nil {
			log.Errorf("Error while remirroring PR: %s\n", err.Error())
		}
	} else if strings.HasPrefix(body, "domirror") {
		// Split into pieces and grab the second arg
		pieces := strings.Split(body, " ")
		if !(len(pieces) == 2) {
			log.Errorf("Incorrect number of arguments while mirroring custom PR id")
			return
		}

		id, err := strconv.Atoi(pieces[1])
		if err != nil {
			log.Errorf("Error while parsing comment body, incorrect format: %s\n", err.Error())
			return
		}

		pr, _, err := p.GitHubClient.PullRequests.Get(*p.Context, p.Configuration.UpstreamOwner, p.Configuration.UpstreamRepo, id)
		if err != nil {
			log.Errorf("Error while getting upstream PR to mirror: %s\n", err.Error())
			return
		}

		_, err = p.MirrorPR(pr)
		if err != nil {
			log.Errorf("Error while mirroring PR: %s\n", err.Error())
		}

	}
}

// RunEventScraper runs the GitHub repo event API scraper
func (p PRMirror) RunEventScraper() {
	for {
		events, pollInterval, err := p.GetRepoEvents()
		if err == nil {
			for _, event := range events {
				p.HandleEvent(event)
			}
		}

		// Handle downstream events
		events, pollInterval, err = p.GetDownstreamRepoEvents()
		if err == nil {
			for _, event := range events {
				p.HandleEvent(event)
			}
		}

		log.Debugf("Sleeping for %d as specified by GitHub\n", pollInterval)
		time.Sleep(time.Duration(pollInterval) * time.Second)
	}
}

// ServeHTTP handles HTTP requests to the webhook endpoint
func (s GitHubEventMonitor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	payload, err := github.ValidatePayload(r, []byte(s.Mirrorer.Configuration.WebhookSecret))
	if err != nil {
		log.Errorf("Error validating the payload\n")
		return
	}
	event, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		log.Errorf("Error parsing the payload\n")
	}

	switch event := event.(type) {
	case *github.PullRequestEvent:
		s.Mirrorer.HandlePREvent(event)
	}
}

// RunWebhookListener acts a webhook listener which GitHub will call with events
func (p PRMirror) RunWebhookListener() {
	server := GitHubEventMonitor{Mirrorer: p}
	err := http.ListenAndServe(fmt.Sprintf(":%d", p.Configuration.WebhookPort), server)
	log.Fatal(err)
}

// MirrorPR will mirror a PR from an upstream to the downstream
func (p PRMirror) MirrorPR(pr *github.PullRequest) (int, error) {
	p.GitLock.Lock()
	defer p.GitLock.Unlock()

	downstreamID, err := p.Database.GetDownstreamID(pr.GetNumber())
	if downstreamID != 0 {
		log.Warningf("Refusing to mirror already existing PR: %s - %s\n", pr.GetTitle(), pr.GetNumber())
		return 0, errors.New("prmirror: tried to mirror a PR which has already been mirrored")
	}

	log.Infof("Mirroring PR [%d]: %s from %s\n", pr.GetNumber(), pr.GetTitle(), pr.User.GetLogin())

	var cmdoutput []byte = nil
	switch runtime.GOOS {
	case "linux":
		cmd := exec.Command(fmt.Sprintf("%s%s", p.Configuration.RepoPath, p.Configuration.ToolPath), strconv.Itoa(pr.GetNumber()), pr.GetTitle())
		cmd.Dir = p.Configuration.RepoPath
		cmdoutput, err = cmd.CombinedOutput()
		if err != nil {
			log.Criticalf("Error while mirroring %d: %s\n details: %s", pr.GetNumber(), err, cmdoutput)
			return 0, err
		}
	case "windows":
		ps, _ := exec.LookPath("powershell.exe")
		cmd := exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", fmt.Sprintf("%s%s", p.Configuration.RepoPath, p.Configuration.ToolPath), strconv.Itoa(pr.GetNumber()), pr.GetTitle(), p.Configuration.RepoPath)
		cmd.Dir = p.Configuration.RepoPath
		cmdoutput, err = cmd.CombinedOutput()
		if err != nil {
			log.Criticalf("Error while mirroring %d: %s\n details: %s", pr.GetNumber(), err, cmdoutput)
			return 0, err
		}
	}

	logpath := fmt.Sprintf("./logs/upstream-merge-%d.log", pr.GetNumber())
	ioutil.WriteFile(logpath, cmdoutput, 0600)
	log.Debugf("Wrote log to %s\n", logpath)

	base := "master"
	head := fmt.Sprintf("upstream-merge-%d", pr.GetNumber())
	maintainerCanModify := true // We are the owner of the PR so we can specify this as true
	title := fmt.Sprintf("[MIRROR] %s", pr.GetTitle())
	body := fmt.Sprintf("Original PR: %s\n--------------------\n%s", pr.GetHTMLURL(), strings.Replace(pr.GetBody(), "@", "@ ", -1))
	re := regexp.MustCompile(`(?i)\b((fix(|es|ed))|(close(|s|d))|(resolve(|s|d)))\s#\d+\b`)
	body = re.ReplaceAllString(body, "[issue link stripped]")

	newPR := github.NewPullRequest{}
	newPR.Title = &title
	newPR.Body = &body
	newPR.Base = &base
	newPR.Head = &head
	newPR.MaintainerCanModify = &maintainerCanModify

	pr, _, err = p.GitHubClient.PullRequests.Create(*p.Context, p.Configuration.DownstreamOwner, p.Configuration.DownstreamRepo, &newPR)
	if err != nil {
		return 0, err
	}

	if strings.Contains(string(cmdoutput), "Rejected hunk") {
		p.AddLabels(pr.GetNumber(), []string{"Auto Merge Rejections"})
	}

	return pr.GetNumber(), nil
}

// RemirrorPR will update the downstream mirror branch
func (p PRMirror) RemirrorPR(pr *github.PullRequest) (int, error) {
	p.GitLock.Lock()
	defer p.GitLock.Unlock()

	//downstreamID, err := p.Database.GetDownstreamID(pr.GetNumber())
	/*if downstreamID != 0 {
		log.Warningf("Refusing to mirror already existing PR: %s - %s\n", pr.GetTitle(), pr.GetNumber())
		return 0, errors.New("prmirror: tried to mirror a PR which has already been mirrored")
	}*/

	log.Infof("Remirroring PR [%d]: %s from %s\n", pr.GetNumber(), pr.GetTitle(), pr.User.GetLogin())

	var cmdoutput []byte = nil
	var err error = nil
	switch runtime.GOOS {
	case "linux":
		cmd := exec.Command(fmt.Sprintf("%s%s", p.Configuration.RepoPath, p.Configuration.ToolPath), strconv.Itoa(pr.GetNumber()), pr.GetTitle())
		cmd.Dir = p.Configuration.RepoPath
		cmdoutput, err = cmd.CombinedOutput()
		if err != nil {
			log.Criticalf("Error while mirroring %d: %s\n details: %s", pr.GetNumber(), err, cmdoutput)
			return 0, err
		}
	case "windows":
		ps, _ := exec.LookPath("powershell.exe")
		cmd := exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", fmt.Sprintf("%s%s", p.Configuration.RepoPath, p.Configuration.ToolPath), strconv.Itoa(pr.GetNumber()), pr.GetTitle(), p.Configuration.RepoPath)
		cmd.Dir = p.Configuration.RepoPath
		cmdoutput, err = cmd.CombinedOutput()
		if err != nil {
			log.Criticalf("Error while mirroring %d: %s\n details: %s", pr.GetNumber(), err, cmdoutput)
			return 0, err
		}
	}

	logpath := fmt.Sprintf("./logs/upstream-merge-remirror-%d.log", pr.GetNumber())
	ioutil.WriteFile(logpath, cmdoutput, 0600)
	log.Debugf("Wrote log to %s\n", logpath)

	return pr.GetNumber(), nil
}
