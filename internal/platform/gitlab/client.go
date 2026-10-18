// Package gitlab implements platform.Client for GitLab.com and self-managed
// GitLab, authenticating with a bot user / group access token.
package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/example/repoguard/internal/platform"
)

type Client struct {
	rest *platform.REST
}

// New builds a client for baseURL (e.g. https://gitlab.com).
func New(baseURL, token string, hc *http.Client) *Client {
	return &Client{rest: &platform.REST{
		HTTP: hc, BaseURL: strings.TrimRight(baseURL, "/") + "/api/v4", UserAgent: "repoguard",
		Authorize: func(r *http.Request) error { r.Header.Set("PRIVATE-TOKEN", token); return nil },
	}}
}

func (c *Client) Kind() platform.Kind { return platform.GitLab }

func project(repo platform.Repo) string { return "/projects/" + url.PathEscape(repo.ID) }

func noun(t platform.Target) string {
	if t.Kind == platform.TargetChangeRequest {
		return "merge_requests"
	}
	return "issues"
}

// --- wire types ---

type glUser struct {
	Username string `json:"username"`
}

type glMR struct {
	IID          int      `json:"iid"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	WebURL       string   `json:"web_url"`
	State        string   `json:"state"`
	Draft        bool     `json:"draft"`
	Author       glUser   `json:"author"`
	Labels       []string `json:"labels"`
	SHA          string   `json:"sha"`
	SourceBranch string   `json:"source_branch"`
	TargetBranch string   `json:"target_branch"`
	DiffRefs     struct {
		BaseSHA  string `json:"base_sha"`
		HeadSHA  string `json:"head_sha"`
		StartSHA string `json:"start_sha"`
	} `json:"diff_refs"`
}

type glIssue struct {
	IID         int       `json:"iid"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	WebURL      string    `json:"web_url"`
	State       string    `json:"state"`
	Author      glUser    `json:"author"`
	Labels      []string  `json:"labels"`
	Assignees   []glUser  `json:"assignees"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (i glIssue) toIssue() platform.Issue {
	var assignees []string
	for _, a := range i.Assignees {
		assignees = append(assignees, a.Username)
	}
	return platform.Issue{
		Number: i.IID, Title: i.Title, Body: i.Description, Author: i.Author.Username, URL: i.WebURL,
		State: normState(i.State), Labels: i.Labels, Assignees: assignees, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt,
	}
}

func normState(s string) string {
	if s == "opened" {
		return "open"
	}
	return s
}

// --- platform.Client ---

func (c *Client) ListRepos(ctx context.Context) ([]platform.Repo, error) {
	var out []platform.Repo
	for page := 1; ; page++ {
		var projects []struct {
			ID                int    `json:"id"`
			PathWithNamespace string `json:"path_with_namespace"`
			DefaultBranch     string `json:"default_branch"`
		}
		// Developer (30) is the minimum role that can set commit statuses.
		p := fmt.Sprintf("/projects?membership=true&min_access_level=30&archived=false&simple=true&per_page=100&page=%d", page)
		if _, err := c.rest.Do(ctx, http.MethodGet, p, nil, &projects); err != nil {
			return nil, err
		}
		for _, pr := range projects {
			out = append(out, platform.Repo{Platform: platform.GitLab, ID: strconv.Itoa(pr.ID), FullName: pr.PathWithNamespace, DefaultBranch: pr.DefaultBranch})
		}
		if len(projects) < 100 {
			return out, nil
		}
	}
}

func (c *Client) GetFile(ctx context.Context, repo platform.Repo, ref, path string) ([]byte, error) {
	p := fmt.Sprintf("%s/repository/files/%s/raw?ref=%s", project(repo), url.PathEscape(path), url.QueryEscape(ref))
	return c.rest.Raw(ctx, p, "*/*")
}

func (c *Client) GetChangeRequest(ctx context.Context, repo platform.Repo, number int) (*platform.ChangeRequest, error) {
	var mr glMR
	if _, err := c.rest.Do(ctx, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d", project(repo), number), nil, &mr); err != nil {
		return nil, err
	}
	head := mr.DiffRefs.HeadSHA
	if head == "" {
		head = mr.SHA
	}
	return &platform.ChangeRequest{
		Number: mr.IID, Title: mr.Title, Body: mr.Description, Author: mr.Author.Username, URL: mr.WebURL,
		Draft: mr.Draft, State: normState(mr.State), HeadSHA: head, BaseSHA: mr.DiffRefs.BaseSHA,
		StartSHA: mr.DiffRefs.StartSHA, HeadRef: mr.SourceBranch, BaseRef: mr.TargetBranch, Labels: mr.Labels,
	}, nil
}

func (c *Client) ListChanges(ctx context.Context, repo platform.Repo, cr *platform.ChangeRequest) ([]platform.FileChange, error) {
	var out []platform.FileChange
	for page := 1; page <= 30; page++ {
		var diffs []struct {
			OldPath     string `json:"old_path"`
			NewPath     string `json:"new_path"`
			Diff        string `json:"diff"`
			NewFile     bool   `json:"new_file"`
			RenamedFile bool   `json:"renamed_file"`
			DeletedFile bool   `json:"deleted_file"`
		}
		p := fmt.Sprintf("%s/merge_requests/%d/diffs?per_page=100&page=%d", project(repo), cr.Number, page)
		if _, err := c.rest.Do(ctx, http.MethodGet, p, nil, &diffs); err != nil {
			return nil, err
		}
		for _, d := range diffs {
			status := "modified"
			switch {
			case d.NewFile:
				status = "added"
			case d.DeletedFile:
				status = "removed"
			case d.RenamedFile:
				status = "renamed"
			}
			add, del := countLines(d.Diff)
			out = append(out, platform.FileChange{Path: d.NewPath, OldPath: d.OldPath, Status: status, Patch: d.Diff, Additions: add, Deletions: del})
		}
		if len(diffs) < 100 {
			break
		}
	}
	return out, nil
}

func countLines(patch string) (add, del int) {
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++"):
			add++
		case strings.HasPrefix(l, "-") && !strings.HasPrefix(l, "---"):
			del++
		}
	}
	return add, del
}

var glState = map[platform.State]string{
	platform.StatePending: "pending",
	platform.StateSuccess: "success",
	platform.StateFailure: "failed",
	platform.StateError:   "failed",
}

func (c *Client) SetStatus(ctx context.Context, repo platform.Repo, sha string, st platform.Status) error {
	body := map[string]string{"state": glState[st.State], "name": st.Context, "description": st.Description}
	if st.TargetURL != "" {
		body["target_url"] = st.TargetURL
	}
	_, err := c.rest.Do(ctx, http.MethodPost, fmt.Sprintf("%s/statuses/%s", project(repo), sha), body, nil)
	return err
}

func (c *Client) UpsertComment(ctx context.Context, repo platform.Repo, t platform.Target, marker, body string) error {
	if !strings.Contains(body, marker) {
		body = marker + "\n" + body
	}
	base := fmt.Sprintf("%s/%s/%d/notes", project(repo), noun(t), t.Number)
	for page := 1; page <= 10; page++ {
		var notes []struct {
			ID     int    `json:"id"`
			Body   string `json:"body"`
			System bool   `json:"system"`
		}
		if _, err := c.rest.Do(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d&sort=asc", base, page), nil, &notes); err != nil {
			return err
		}
		for _, n := range notes {
			if !n.System && strings.Contains(n.Body, marker) {
				if n.Body == body {
					return nil
				}
				_, err := c.rest.Do(ctx, http.MethodPut, fmt.Sprintf("%s/%d", base, n.ID), map[string]string{"body": body}, nil)
				return err
			}
		}
		if len(notes) < 100 {
			break
		}
	}
	_, err := c.rest.Do(ctx, http.MethodPost, base, map[string]string{"body": body}, nil)
	return err
}

// CreateReview posts one discussion per inline comment (GitLab has no batched review API).
func (c *Client) CreateReview(ctx context.Context, repo platform.Repo, cr *platform.ChangeRequest, summary string, comments []platform.ReviewComment) error {
	var errs []error
	for _, cm := range comments {
		body := map[string]any{
			"body": cm.Body,
			"position": map[string]any{
				"position_type": "text",
				"base_sha":      cr.BaseSHA,
				"start_sha":     cr.StartSHA,
				"head_sha":      cr.HeadSHA,
				"new_path":      cm.Path,
				"new_line":      cm.Line,
			},
		}
		if _, err := c.rest.Do(ctx, http.MethodPost, fmt.Sprintf("%s/merge_requests/%d/discussions", project(repo), cr.Number), body, nil); err != nil {
			errs = append(errs, fmt.Errorf("%s:%d: %w", cm.Path, cm.Line, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Client) ListReviewComments(ctx context.Context, repo platform.Repo, cr *platform.ChangeRequest) ([]string, error) {
	var out []string
	for page := 1; page <= 10; page++ {
		var discussions []struct {
			Notes []struct {
				Body string `json:"body"`
			} `json:"notes"`
		}
		p := fmt.Sprintf("%s/merge_requests/%d/discussions?per_page=100&page=%d", project(repo), cr.Number, page)
		if _, err := c.rest.Do(ctx, http.MethodGet, p, nil, &discussions); err != nil {
			return nil, err
		}
		for _, d := range discussions {
			for _, n := range d.Notes {
				out = append(out, n.Body)
			}
		}
		if len(discussions) < 100 {
			break
		}
	}
	return out, nil
}

func (c *Client) AddLabels(ctx context.Context, repo platform.Repo, t platform.Target, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	_, err := c.rest.Do(ctx, http.MethodPut, fmt.Sprintf("%s/%s/%d", project(repo), noun(t), t.Number), map[string]string{"add_labels": strings.Join(labels, ",")}, nil)
	return err
}

func (c *Client) RemoveLabel(ctx context.Context, repo platform.Repo, t platform.Target, label string) error {
	_, err := c.rest.Do(ctx, http.MethodPut, fmt.Sprintf("%s/%s/%d", project(repo), noun(t), t.Number), map[string]string{"remove_labels": label}, nil)
	return err
}

func (c *Client) GetIssue(ctx context.Context, repo platform.Repo, number int) (*platform.Issue, error) {
	var i glIssue
	if _, err := c.rest.Do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d", project(repo), number), nil, &i); err != nil {
		return nil, err
	}
	issue := i.toIssue()
	return &issue, nil
}

func (c *Client) ListOpenIssues(ctx context.Context, repo platform.Repo, limit int) ([]platform.Issue, error) {
	var out []platform.Issue
	for page := 1; len(out) < limit; page++ {
		var issues []glIssue
		p := fmt.Sprintf("%s/issues?state=opened&order_by=updated_at&sort=desc&per_page=100&page=%d", project(repo), page)
		if _, err := c.rest.Do(ctx, http.MethodGet, p, nil, &issues); err != nil {
			return nil, err
		}
		for _, i := range issues {
			if len(out) < limit {
				out = append(out, i.toIssue())
			}
		}
		if len(issues) < 100 {
			break
		}
	}
	return out, nil
}

func (c *Client) CloseIssue(ctx context.Context, repo platform.Repo, number int, _ platform.CloseReason) error {
	_, err := c.rest.Do(ctx, http.MethodPut, fmt.Sprintf("%s/issues/%d", project(repo), number), map[string]string{"state_event": "close"}, nil)
	return err
}

var _ platform.Client = (*Client)(nil)
