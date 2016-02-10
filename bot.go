// Copyright 2016 The Go Authors: https://golang.org/AUTHORS
// Licensed under the same terms as Go itself: https://golang.org/LICENSE

// The gerritbot command is the the start of a Github Pull Request to
// Gerrit code review bot.
//
// It is incomplete.
//
// The idea is that users won't need to use Gerrit for all changes.
// Github can continue to be the canonical Git repo for projects
// but users sending PRs (or more likely: the people reviewing the PRs)
// can use Gerrit selectively for some reviews. This bot mirrors the PR
// to Gerrit for review there.
//
// Again, it is incomplete.
package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/google/go-github/github"
	"golang.org/x/build/gerrit"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
)

var githubUser, githubToken string

type Repo struct {
	Owner string
	Repo  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Repo }

type PullRequest struct {
	Repo
	Number int
}

func (pr PullRequest) ChangeID() string {
	d := sha1.New()
	fmt.Fprintf(d, "%s/%d", pr.Repo, pr.Number)
	return fmt.Sprintf("I%x", d.Sum(nil))
}

type GerritProject struct {
	// Instance is the googlesource.com Gerrit instance
	// hostname. ("go", "code", "camlistore", etc.)
	Instance string

	// Project is the Gerrit project name on Instance.
	Project string
}

func (gp GerritProject) Client() *gerrit.Client {
	cookieFile := filepath.Join(homeDir(), "keys", "gerrit-letsusegerrit.cookies")
	if _, err := os.Stat(cookieFile); err != nil {
		log.Fatalf("Can't stat cookie file for Gerrit: %v", cookieFile)
	}
	return gerrit.NewClient("https://"+gp.Instance+"-review.googlesource.com", gerrit.GitCookieFileAuth(cookieFile))
}

func (pr PullRequest) GerritProject() GerritProject {
	if pr.Repo.Owner == "camlistore" && pr.Repo.Repo == "go4" {
		return GerritProject{"camlistore", "review-go4"}
	}
	if pr.Repo.Owner == "grpc" && pr.Repo.Repo == "grpc-go" {
		return GerritProject{"go", "grpc-review"}
	}
	return GerritProject{"code", strings.ToLower(fmt.Sprintf("review-github-%s-%s", pr.Repo.Owner, pr.Repo.Repo))}
}

type logConn struct {
	net.Conn
}

func (c logConn) Write(p []byte) (n int, err error) {
	log.Printf("Write: %q", p)
	return c.Conn.Write(p)
}

func (c logConn) Read(p []byte) (n int, err error) {
	n, err = c.Conn.Read(p)
	log.Printf("Read: %q, %v", p[:n], err)
	return
}

var logClient = &http.Client{
	Transport: &http.Transport{
		DialTLS: func(netw, addr string) (net.Conn, error) {
			log.Printf("need to dial %s, %s", netw, addr)
			c, err := tls.Dial(netw, addr, &tls.Config{ServerName: "api.github.com"})
			if err != nil {
				return nil, err
			}
			return logConn{c}, nil
		},
	},
}

var verboseHTTP = flag.Bool("verbose_http", false, "Verbose HTTP debugging")

type Bot struct {
	gh *github.Client
}

func NewBot() *Bot {
	baseHTTP := http.DefaultClient
	if *verboseHTTP {
		baseHTTP = logClient
	}
	gh := github.NewClient(oauth2.NewClient(
		context.WithValue(context.Background(), oauth2.HTTPClient, baseHTTP),
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: githubToken}),
	))

	return &Bot{
		gh: gh,
	}
}

func closeRes(res *github.Response) {
	if res != nil && res.Body != nil {
		res.Body.Close()
	}
}

func (b *Bot) CheckNotifications() error {
	notifs, res, err := b.gh.Activity.ListNotifications(&github.NotificationListOptions{All: true})
	defer closeRes(res)
	if err != nil {
		return err
	}

	log.Printf("Notifs: %d", len(notifs))
	for _, n := range notifs {
		log.Printf("Notif: %v, repo:%v, %+v", fs(n.ID), fs(n.Repository.FullName), fs(n.Subject.Title))
	}
	return nil
}

func (b *Bot) CheckPulls(owner, repo string) error {
	pulls, res, err := b.gh.PullRequests.List(owner, repo, nil)
	defer closeRes(res)
	if err != nil {
		return err
	}
	log.Printf("%d pulls", len(pulls))
	for _, pr := range pulls {
		log.Printf("PR: %v", github.Stringify(pr))
	}
	return nil
}

func (b *Bot) CommentGithub(owner, repo string, number int, comment string) error {
	prc, res, err := b.gh.Issues.CreateComment(owner, repo, number, &github.IssueComment{
		Body: &comment,
	})
	defer closeRes(res)
	if err != nil {
		return err
	}
	log.Printf("Got: %v, %v, %v", github.Stringify(prc), res, err)
	return nil
}

func (b *Bot) CommentGithubNoDup(owner, repo string, number int, comment string) error {
	comments, res, err := b.gh.Issues.ListComments(owner, repo, number, &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{
			PerPage: 1000,
		},
	})
	defer closeRes(res)
	if err != nil {
		return err
	}
	for _, ic := range comments {
		if ic.Body != nil && *ic.Body == comment {
			return nil
		}
	}
	return b.CommentGithub(owner, repo, number, comment)
}

func (b *Bot) CommentGerrit(gr *gerrit.Client, number int, comment string) error {
	return gr.SetReview(fmt.Sprint(number), "current", gerrit.ReviewInput{
		Message: comment,
	})
}

var (
	changeIdRx = regexp.MustCompile(`(?m)^Change-Id: (I\w+)\b`)

	// parses out:
	// remote: New Changes:
	// remote:   https://camlistore-review.googlesource.com/5991 README: whitespace cleanup
	gitNewChangeRx = regexp.MustCompile(`New Changes:.+\n.+https://\w+-review\.googlesource\.com/(\d+)`)
)

func (b *Bot) Sync(pr PullRequest) error {
	prd, res, err := b.gh.PullRequests.Get(pr.Owner, pr.Repo.Repo, pr.Number)
	defer closeRes(res)
	if err != nil {
		return err
	}
	if prd.Head == nil || prd.Base == nil || prd.State == nil || prd.Title == nil || prd.Commits == nil {
		return errors.New("nil fields from github API")
	}
	if *prd.Commits == 0 {
		// Um, nothing to do?
		return nil
	}
	if *prd.Commits > 1 {
		return b.CommentGithubNoDup(pr.Owner, pr.Repo.Repo, pr.Number,
			fmt.Sprintf("Head %v has %d commits. Please squash your commits into one. @LetsUseGerrit only supports syncing a pull request with a single commit, as that is how Gerrit is typically used.",
				*prd.Head.SHA, *prd.Commits))
	}

	state := *prd.State
	title := *prd.Title
	log.Printf("State %s, title %q, commits %d", state, title, *prd.Commits)

	baseSHA := *prd.Base.SHA
	headSHA := *prd.Head.SHA
	log.Printf("Base: %s  Head: %s", baseSHA, headSHA)

	gp := pr.GerritProject()
	grInst := gp.Instance
	proj := gp.Project
	//log.Fatalf("Got: %+v %v, %v", gp, grInst, proj)

	gr := gp.Client()

	pi, err := gr.GetProjectInfo(proj)
	if err != nil {
		log.Printf("gerrit project %s: %v", proj, err)
		if err == gerrit.ErrProjectNotExist {
			pi, err = gr.CreateProject(proj)
			if err != nil {
				return fmt.Errorf("error creating gerrit project %s: %v", proj, err)
			}
		}
	}
	log.Printf("Gerrit project: %v", pi)

	gitDir := filepath.Join(homeDir(), "var", "letsusegerrit", "git-tmp-"+proj)
	if err := os.MkdirAll(gitDir, 0700); err != nil {
		return err
	}

	branch := fmt.Sprintf("PR/%d", pr.Number)

	q := "project:" + proj + " branch:" + branch
	log.Printf("Running search query: %q", q)
	cis, err := gr.QueryChanges(q, gerrit.QueryChangesOpt{Fields: []string{"CURRENT_REVISION"}})
	log.Printf("Query %q = %d results, %v", q, len(cis), err)
	if err != nil {
		return fmt.Errorf("Gerrit.QueryChanges = %v", err)
	}

	var changeNum int

	updateGithubComment := func() error {
		return b.CommentGithubNoDup(pr.Owner, pr.Repo.Repo, pr.Number,
			fmt.Sprintf("Gerrit code review: https://%s-review.googlesource.com/%d (at git rev %s)", grInst, changeNum, headSHA[:7]))
	}

	if len(cis) > 0 {
		// If it's more than 1, we pick the lowest.
		var changeInfo *gerrit.ChangeInfo
		for _, ci := range cis {
			if changeNum == 0 || ci.ChangeNumber < changeNum {
				changeNum = ci.ChangeNumber
				changeInfo = ci
			}
		}
		if changeInfo.CurrentRevision == headSHA {
			log.Printf("Gerrit is up-to-date.")
			return updateGithubComment()
		}
	}

	git := func(args ...string) *exec.Cmd {
		args = append([]string{
			"-c", "http.cookiefile=/home/bradfitz/keys/gerrit-letsusegerrit.cookies",
		}, args...)
		cmd := exec.Command("git", args...)
		cmd.Dir = gitDir
		return cmd
	}

	if _, err := os.Stat(filepath.Join(gitDir, ".git")); os.IsNotExist(err) {
		if err := git("init").Run(); err != nil {
			return fmt.Errorf("git init: %v", err)
		}
	}

	// Fetch head
	{
		fetch := func(br *github.PullRequestBranch) error {
			log.Printf("Fetching %s refs/heads/%s", *br.Repo.CloneURL, *br.Ref)
			if out, err := git("fetch", "--update-head-ok", *br.Repo.CloneURL, "refs/heads/"+*br.Ref).CombinedOutput(); err != nil {
				return fmt.Errorf("git fetch from %s: %v, %s", *br.Repo.CloneURL, err, out)
			}
			log.Printf("Fetched.")
			return nil
		}
		if err := fetch(prd.Head); err != nil {
			return err
		}
		if err := fetch(prd.Base); err != nil {
			return err
		}
	}

	var parent string

	// Get raw commit, both to verify that we got it above, and to verify it
	// has exactly 1 parent, and that if it has a Change-Id line at all, it
	// is at least the one we expect.
	{
		cat := git("cat-file", "-p", *prd.Head.SHA)
		var errbuf bytes.Buffer
		cat.Stderr = &errbuf
		out, err := cat.Output()
		if err != nil {
			return fmt.Errorf("git cat-file %s: %v, %s", *prd.Head.SHA, err, errbuf.Bytes())
		}
		hdrs, _ := parseRawGitCommit(out)
		parents := hdrs["parent"]
		if len(parents) != 1 {
			return fmt.Errorf("Head git commit %v has %d parents. LetsUseGerrit does not support reviewing merge commits.",
				*prd.Head.SHA, len(parents))
		}
		parent = parents[0]
	}

	if changeNum == 0 {
		log.Printf("Need to make first commit in refs/heads/%s; pushing parent %s", branch, parent)
		if out, err := git("push", "-f",
			"https://"+grInst+"-review.googlesource.com/"+proj,
			parent+":refs/heads/"+branch).Output(); err != nil {
			return fmt.Errorf("git push of parent %s to refs/heads/%s: %v, %s",
				parent, branch, err, out)
		}

		log.Printf("Pushing PR's head commit %s to refs/for/%s", headSHA, branch)
		out, err := git(
			"push",
			"https://"+grInst+"-review.googlesource.com/"+proj,
			headSHA+":refs/for/"+branch).CombinedOutput()
		if err != nil {
			return fmt.Errorf("git push: %v, %s", err, out)
		}
		m := gitNewChangeRx.FindStringSubmatch(string(out))
		if m == nil {
			return fmt.Errorf("git push expected a change number in output; got: %s", out)
		}
		changeNum, err = strconv.Atoi(m[1])
		if err != nil {
			return fmt.Errorf("Atoi(%q) after git push of new change: %v", m[1], err)
		}
	} else {
		log.Printf("Pushing %v to refs/changes/%d", headSHA, changeNum)
		if out, err := git("push",
			"https://"+grInst+"-review.googlesource.com/"+proj,
			headSHA+":refs/changes/"+strconv.Itoa(changeNum)).Output(); err != nil {
			return fmt.Errorf("git push of head commit %s: %v, %s", headSHA, err, out)
		}
	}
	return updateGithubComment()
}

func main() {
	flag.Parse()
	readGithubConfig()

	if flag.NArg() != 1 {
		log.Fatalf("Usage: bot.go <URL of pull request>")
	}

	m := regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/pull/(\d+)`).FindStringSubmatch(flag.Arg(0))
	if m == nil {
		log.Fatalf("Unrecognized pull request URL %q", flag.Arg(0))
	}
	n, _ := strconv.Atoi(m[3])
	pr := PullRequest{
		Repo:   Repo{Owner: m[1], Repo: m[2]},
		Number: n,
	}

	bot := NewBot()
	log.Printf("Sync of %s = %v", flag.Arg(0), bot.Sync(pr))
}

func fs(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func readGithubConfig() {
	file := filepath.Join(homeDir(), "keys", "github-letsusegerrit.token")
	slurp, err := ioutil.ReadFile(file)
	if err != nil {
		log.Fatal(err)
	}
	f := strings.Fields(strings.TrimSpace(string(slurp)))
	if len(f) != 2 {
		log.Fatalf("expected two fields (user and token) in %v; got %d fields", file, len(f))
	}
	githubUser, githubToken = f[0], f[1]
}

func homeDir() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
	}
	return os.Getenv("HOME")
}

func parseRawGitCommit(raw []byte) (hdrs map[string][]string, body string) {
	f := strings.SplitN(string(raw), "\n\n", 2)
	if len(f) != 2 {
		return
	}
	body = f[1]
	hdrs = make(map[string][]string)
	for _, line := range strings.Split(strings.TrimSpace(f[0]), "\n") {
		sp := strings.IndexByte(line, ' ')
		if sp == -1 {
			continue
		}
		k, v := line[:sp], line[sp+1:]
		hdrs[k] = append(hdrs[k], v)
	}
	return
}
