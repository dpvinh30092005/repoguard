// Package github implements platform.Client for GitHub.com and GitHub
// Enterprise Server, authenticating as a GitHub App (recommended) or a token.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/platform"
)

const userAgent = "repoguard"

type ctxKey struct{}

type Client struct {
	rest   *platform.REST
	tokens TokenSource
	app    *AppAuth // nil in token mode
}

// New builds a client. Pass app != nil for GitHub App auth, otherwise token is used.
func New(apiURL string, app *AppAuth, token string, hc *http.Client) *Client {
	c := &Client{app: app}
	if app != nil {
		c.tokens = app
	} else {
		c.tokens = StaticToken(token)
	}
	c.rest = &platform.REST{HTTP: hc, BaseURL: strings.TrimRight(apiURL, "/"), UserAgent: userAgent, Authorize: func(r *http.Request) error {
		inst, _ := r.Context().Value(ctxKey{}).(int64)
		tok, err := c.tokens.Token(r.Context(), inst)
		if err != nil {
			return err
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		return nil
	}}
	return c
}

func (c *Client) Kind() platform.Kind { return platform.GitHub }

// with attaches the repo's installation to ctx so Authorize picks the right token.
func with(ctx context.Context, repo platform.Repo) context.Context {
	return context.WithValue(ctx, ctxKey{}, repo.InstallationID)
}

func repoPath(repo platform.Repo) string { return "/repos/" + repo.ID }

// --- wire types ---

type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type ghLabel struct {
	Name string `json:"name"`
}

type ghRepo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Archived      bool   `json:"archived"`
}

type ghPull struct {
	Number  int       `json:"number"`
	Title   string    `json:"title"`
	Body    string    `json:"body"`
	HTMLURL string    `json:"html_url"`
	State   string    `json:"state"`
	Draft   bool      `json:"draft"`
	Merged  bool      `json:"merged"`
	User    ghUser    `json:"user"`
	Labels  []ghLabel `json:"labels"`
	Head    struct {
		SHA  string  `json:"sha"`
		Ref  string  `json:"ref"`
		Repo *ghRepo `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA  string  `json:"sha"`
		Ref  string  `json:"ref"`
		Repo *ghRepo `json:"repo"`
	} `json:"base"`
}

func (p ghPull) toCR() *platform.ChangeRequest {
	state := p.State
	if p.Merged {
		state = "merged"
	}
	return &platform.ChangeRequest{
		Number: p.Number, Title: p.Title, Body: p.Body, Author: p.User.Login, URL: p.HTMLURL,
		Draft: p.Draft, State: state, HeadSHA: p.Head.SHA, BaseSHA: p.Base.SHA,
		HeadRef: p.Head.Ref, BaseRef: p.Base.Ref, Labels: labelNames(p.Labels),
		// A deleted fork has head.repo == null: treat it as a fork.
		FromFork: p.Head.Repo == nil || p.Base.Repo == nil || p.Head.Repo.FullName != p.Base.Repo.FullName,
	}
}

type ghIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	State       string    `json:"state"`
	User        ghUser    `json:"user"`
	Labels      []ghLabel `json:"labels"`
	Assignees   []ghUser  `json:"assignees"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	PullRequest *struct{} `json:"pull_request"`
}

func (i ghIssue) toIssue() platform.Issue {
	var assignees []string
	for _, a := range i.Assignees {
		assignees = append(assignees, a.Login)
	}
	return platform.Issue{
		Number: i.Number, Title: i.Title, Body: i.Body, Author: i.User.Login, URL: i.HTMLURL,
		State: i.State, Labels: labelNames(i.Labels), Assignees: assignees,
		CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt, IsPullRequest: i.PullRequest != nil,
	}
}

func labelNames(ls []ghLabel) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

// --- platform.Client ---

func (c *Client) ListRepos(ctx context.Context) ([]platform.Repo, error) {
	if c.app == nil {
		return c.listRepos(ctx, 0, "/user/repos?affiliation=owner,collaborator,organization_member", func(raw []ghRepo) []ghRepo { return raw })
	}
	ids, err := c.app.Installations(ctx)
	if err != nil {
		return nil, err
	}
	var all []platform.Repo
	for _, id := range ids {
		repos, err := c.listRepos(ctx, id, "/installation/repositories", nil)
		if err != nil {
			return nil, fmt.Errorf("installation %d: %w", id, err)
		}
		all = append(all, repos...)
	}
	return all, nil
}

func (c *Client) listRepos(ctx context.Context, inst int64, path string, unwrap func([]ghRepo) []ghRepo) ([]platform.Repo, error) {
	ctx = context.WithValue(ctx, ctxKey{}, inst)
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var out []platform.Repo
	for page := 1; ; page++ {
		var repos []ghRepo
		p := fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page)
		if unwrap != nil {
			if _, err := c.rest.Do(ctx, http.MethodGet, p, nil, &repos); err != nil {
				return nil, err
			}
		} else {
			var wrapped struct {
				Repositories []ghRepo `json:"repositories"`
			}
			if _, err := c.rest.Do(ctx, http.MethodGet, p, nil, &wrapped); err != nil {
				return nil, err
			}
			repos = wrapped.Repositories
		}
		for _, r := range repos {
			if !r.Archived {
				out = append(out, platform.Repo{Platform: platform.GitHub, ID: r.FullName, FullName: r.FullName, DefaultBranch: r.DefaultBranch, InstallationID: inst})
			}
		}
		if len(repos) < 100 {
			return out, nil
		}
	}
}

func (c *Client) GetFile(ctx context.Context, repo platform.Repo, ref, path string) ([]byte, error) {
	p := fmt.Sprintf("%s/contents/%s?ref=%s", repoPath(repo), escapePath(path), url.QueryEscape(ref))
	return c.rest.Raw(with(ctx, repo), p, "application/vnd.github.raw+json")
}

func (c *Client) GetChangeRequest(ctx context.Context, repo platform.Repo, number int) (*platform.ChangeRequest, error) {
	var p ghPull
	if _, err := c.rest.Do(with(ctx, repo), http.MethodGet, fmt.Sprintf("%s/pulls/%d", repoPath(repo), number), nil, &p); err != nil {
		return nil, err
	}
	return p.toCR(), nil
}

func (c *Client) ListChanges(ctx context.Context, repo platform.Repo, cr *platform.ChangeRequest) ([]platform.FileChange, error) {
	var out []platform.FileChange
	for page := 1; page <= 30; page++ { // API caps at 3000 files
		var files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
			Status           string `json:"status"`
			Patch            string `json:"patch"`
			Additions        int    `json:"additions"`
			Deletions        int    `json:"deletions"`
		}
		p := fmt.Sprintf("%s/pulls/%d/files?per_page=100&page=%d", repoPath(repo), cr.Number, page)
		if _, err := c.rest.Do(with(ctx, repo), http.MethodGet, p, nil, &files); err != nil {
			return nil, err
		}
		for _, f := range files {
			out = append(out, platform.FileChange{
				Path: f.Filename, OldPath: f.PreviousFilename, Status: f.Status,
				Patch: f.Patch, Additions: f.Additions, Deletions: f.Deletions,
			})
		}
		if len(files) < 100 {
			break
		}
	}
	return out, nil
}

func (c *Client) SetStatus(ctx context.Context, repo platform.Repo, sha string, st platform.Status) error {
	body := map[string]string{"state": string(st.State), "context": st.Context, "description": truncate(st.Description, 140)}
	if st.TargetURL != "" {
		body["target_url"] = st.TargetURL
	}
	_, err := c.rest.Do(with(ctx, repo), http.MethodPost, fmt.Sprintf("%s/statuses/%s", repoPath(repo), sha), body, nil)
	return err
}

// Pull requests are issues for comments and labels on GitHub, so Target.Kind doesn't matter here.
func (c *Client) UpsertComment(ctx context.Context, repo platform.Repo, t platform.Target, marker, body string) error {
	ctx = with(ctx, repo)
	if !strings.Contains(body, marker) {
		body = marker + "\n" + body
	}
	for page := 1; page <= 10; page++ {
		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		p := fmt.Sprintf("%s/issues/%d/comments?per_page=100&page=%d", repoPath(repo), t.Number, page)
		if _, err := c.rest.Do(ctx, http.MethodGet, p, nil, &comments); err != nil {
			return err
		}
		for _, cm := range comments {
			if strings.Contains(cm.Body, marker) {
				if cm.Body == body {
					return nil // unchanged: avoid noisy "edited" markers
				}
				_, err := c.rest.Do(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/comments/%d", repoPath(repo), cm.ID), map[string]string{"body": body}, nil)
				return err
			}
		}
		if len(comments) < 100 {
			break
		}
	}
	_, err := c.rest.Do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", repoPath(repo), t.Number), map[string]string{"body": body}, nil)
	return err
}

func (c *Client) CreateReview(ctx context.Context, repo platform.Repo, cr *platform.ChangeRequest, summary string, comments []platform.ReviewComment) error {
	type ghComment struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Side string `json:"side"`
		Body string `json:"body"`
	}
	list := make([]ghComment, 0, len(comments))
	for _, cm := range comments {
		list = append(list, ghComment{Path: cm.Path, Line: cm.Line, Side: "RIGHT", Body: cm.Body})
	}
	body := map[string]any{"commit_id": cr.HeadSHA, "event": "COMMENT", "body": summary, "comments": list}
	_, err := c.rest.Do(with(ctx, repo), http.MethodPost, fmt.Sprintf("%s/pulls/%d/reviews", repoPath(repo), cr.Number), body, nil)
	return err
}

func (c *Client) ListReviewComments(ctx context.Context, repo platform.Repo, cr *platform.ChangeRequest) ([]string, error) {
	var out []string
	for page := 1; page <= 10; page++ {
		var comments []struct {
			Body string `json:"body"`
		}
		p := fmt.Sprintf("%s/pulls/%d/comments?per_page=100&page=%d", repoPath(repo), cr.Number, page)
		if _, err := c.rest.Do(with(ctx, repo), http.MethodGet, p, nil, &comments); err != nil {
			return nil, err
		}
		for _, cm := range comments {
			out = append(out, cm.Body)
		}
		if len(comments) < 100 {
			break
		}
	}
	return out, nil
}

func (c *Client) AddLabels(ctx context.Context, repo platform.Repo, t platform.Target, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	_, err := c.rest.Do(with(ctx, repo), http.MethodPost, fmt.Sprintf("%s/issues/%d/labels", repoPath(repo), t.Number), map[string][]string{"labels": labels}, nil)
	return err
}

func (c *Client) RemoveLabel(ctx context.Context, repo platform.Repo, t platform.Target, label string) error {
	_, err := c.rest.Do(with(ctx, repo), http.MethodDelete, fmt.Sprintf("%s/issues/%d/labels/%s", repoPath(repo), t.Number, url.PathEscape(label)), nil, nil)
	if platformNotFound(err) {
		return nil
	}
	return err
}

func (c *Client) GetIssue(ctx context.Context, repo platform.Repo, number int) (*platform.Issue, error) {
	var i ghIssue
	if _, err := c.rest.Do(with(ctx, repo), http.MethodGet, fmt.Sprintf("%s/issues/%d", repoPath(repo), number), nil, &i); err != nil {
		return nil, err
	}
	issue := i.toIssue()
	return &issue, nil
}

func (c *Client) ListOpenIssues(ctx context.Context, repo platform.Repo, limit int) ([]platform.Issue, error) {
	var out []platform.Issue
	for page := 1; len(out) < limit; page++ {
		var issues []ghIssue
		p := fmt.Sprintf("%s/issues?state=open&sort=updated&direction=desc&per_page=100&page=%d", repoPath(repo), page)
		if _, err := c.rest.Do(with(ctx, repo), http.MethodGet, p, nil, &issues); err != nil {
			return nil, err
		}
		for _, i := range issues {
			if i.PullRequest == nil && len(out) < limit { // the issues API also returns PRs
				out = append(out, i.toIssue())
			}
		}
		if len(issues) < 100 {
			break
		}
	}
	return out, nil
}

func (c *Client) CloseIssue(ctx context.Context, repo platform.Repo, number int, reason platform.CloseReason) error {
	body := map[string]string{"state": "closed", "state_reason": string(reason)}
	_, err := c.rest.Do(with(ctx, repo), http.MethodPatch, fmt.Sprintf("%s/issues/%d", repoPath(repo), number), body, nil)
	return err
}

func (c *Client) AuthorCanPush(ctx context.Context, repo platform.Repo, cr *platform.ChangeRequest) (bool, error) {
	var resp struct {
		Permission string `json:"permission"` // admin | maintain | write | triage | read | none
	}
	p := fmt.Sprintf("%s/collaborators/%s/permission", repoPath(repo), url.PathEscape(cr.Author))
	if _, err := c.rest.Do(with(ctx, repo), http.MethodGet, p, nil, &resp); err != nil {
		if platformNotFound(err) {
			return false, nil
		}
		return false, err
	}
	switch resp.Permission {
	case "admin", "maintain", "write":
		return true, nil
	}
	return false, nil
}

func (c *Client) GitSource(ctx context.Context, repo platform.Repo, cr *platform.ChangeRequest) (platform.GitSource, error) {
	tok, err := c.tokens.Token(ctx, repo.InstallationID)
	if err != nil {
		return platform.GitSource{}, err
	}
	web := "https://github.com"
	if base := c.rest.BaseURL; base != "https://api.github.com" {
		web = strings.TrimSuffix(base, "/api/v3") // GitHub Enterprise Server
	}
	return platform.GitSource{
		URL:      web + "/" + repo.ID + ".git",
		Username: "x-access-token",
		Password: tok,
		Ref:      fmt.Sprintf("refs/pull/%d/head", cr.Number),
	}, nil
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

var _ platform.Client = (*Client)(nil)

func platformNotFound(err error) bool { return errors.Is(err, platform.ErrNotFound) }
