// Package platform defines a provider-neutral model of code hosting platforms
// (GitHub, GitLab, ...). Everything above this package — checks, triage,
// the engine — only speaks these types, so adding a platform means writing
// one Client implementation and one webhook parser.
package platform

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Kind string

const (
	GitHub Kind = "github"
	GitLab Kind = "gitlab"
)

// Repo identifies a repository on a platform.
type Repo struct {
	Platform      Kind
	ID            string // GitHub: "owner/name"; GitLab: numeric project id
	FullName      string // human readable: owner/name, group/sub/project
	DefaultBranch string
	// InstallationID is the GitHub App installation that grants access (0 for token auth).
	InstallationID int64
}

func (r Repo) String() string { return string(r.Platform) + ":" + r.FullName }

// ChangeRequest is a GitHub pull request or a GitLab merge request.
type ChangeRequest struct {
	Number   int // PR number / MR iid
	Title    string
	Body     string
	Author   string
	URL      string
	Draft    bool
	State    string // open | closed | merged
	HeadSHA  string
	BaseSHA  string
	StartSHA string // GitLab diff_refs.start_sha (needed for inline comments)
	HeadRef  string
	BaseRef  string
	Labels   []string
	AuthorID int64 // GitLab user id (membership lookups)
	FromFork bool  // head lives in another repository
}

type Issue struct {
	Number    int
	Title     string
	Body      string
	Author    string
	URL       string
	State     string // open | closed
	Labels    []string
	Assignees []string
	CreatedAt time.Time
	UpdatedAt time.Time
	// IsPullRequest is true when a GitHub "issue" number is actually a PR.
	IsPullRequest bool
}

func (i Issue) HasLabel(name string) bool { return containsFold(i.Labels, name) }

// FileChange is one file of a change request with its unified-diff hunks.
type FileChange struct {
	Path      string
	OldPath   string
	Status    string // added | modified | removed | renamed
	Patch     string // hunks only ("@@ ... @@" onwards); empty for binary/huge files
	Additions int
	Deletions int
}

type State string

const (
	StatePending State = "pending"
	StateSuccess State = "success"
	StateFailure State = "failure"
	StateError   State = "error"
)

// Status is a commit status shown next to the change request.
type Status struct {
	Context     string // e.g. "repoguard/pr-rules"
	State       State
	Description string
	TargetURL   string
}

// ReviewComment is an inline comment on the new side of a diff.
type ReviewComment struct {
	Path string
	Line int
	Body string
}

type TargetKind string

const (
	TargetChangeRequest TargetKind = "change_request"
	TargetIssue         TargetKind = "issue"
)

// Target is something that can carry comments and labels.
type Target struct {
	Kind   TargetKind
	Number int
}

func CR(n int) Target           { return Target{Kind: TargetChangeRequest, Number: n} }
func IssueT(n int) Target       { return Target{Kind: TargetIssue, Number: n} }
func (t Target) String() string { return fmt.Sprintf("%s#%d", t.Kind, t.Number) }

type CloseReason string

const (
	ClosedCompleted  CloseReason = "completed"
	ClosedNotPlanned CloseReason = "not_planned"
)

// Client talks to one platform. Implementations must be safe for concurrent use.
type Client interface {
	Kind() Kind
	// ListRepos returns every repository the tool is connected to.
	ListRepos(ctx context.Context) ([]Repo, error)
	// GetFile returns a file's content at ref, or ErrNotFound.
	GetFile(ctx context.Context, repo Repo, ref, path string) ([]byte, error)
	GetChangeRequest(ctx context.Context, repo Repo, number int) (*ChangeRequest, error)
	ListChanges(ctx context.Context, repo Repo, cr *ChangeRequest) ([]FileChange, error)
	SetStatus(ctx context.Context, repo Repo, sha string, st Status) error
	// UpsertComment creates a comment, or edits the existing one containing marker,
	// so the bot keeps one "sticky" comment per topic instead of spamming.
	UpsertComment(ctx context.Context, repo Repo, t Target, marker, body string) error
	// CreateReview posts inline comments on the change request's head commit.
	CreateReview(ctx context.Context, repo Repo, cr *ChangeRequest, summary string, comments []ReviewComment) error
	// ListReviewComments returns the bodies of existing inline comments (to avoid reposting).
	ListReviewComments(ctx context.Context, repo Repo, cr *ChangeRequest) ([]string, error)
	AddLabels(ctx context.Context, repo Repo, t Target, labels []string) error
	RemoveLabel(ctx context.Context, repo Repo, t Target, label string) error
	GetIssue(ctx context.Context, repo Repo, number int) (*Issue, error)
	// ListOpenIssues returns up to limit open issues (not PRs), most recently updated first.
	ListOpenIssues(ctx context.Context, repo Repo, limit int) ([]Issue, error)
	CloseIssue(ctx context.Context, repo Repo, number int, reason CloseReason) error
	// AuthorCanPush reports whether the change request's author has write
	// (GitHub) / Developer (GitLab) access — i.e. could already run code in CI.
	AuthorCanPush(ctx context.Context, repo Repo, cr *ChangeRequest) (bool, error)
	// GitSource returns what is needed to fetch the change request's head commit.
	GitSource(ctx context.Context, repo Repo, cr *ChangeRequest) (GitSource, error)

	// CurrentUser returns the login the credentials belong to (verifies them).
	CurrentUser(ctx context.Context) (string, error)
	// ListOpenChangeRequests lists open PRs/MRs, most recently updated first (used by polling).
	ListOpenChangeRequests(ctx context.Context, repo Repo, limit int) ([]ChangeRequest, error)
	// CreateWebhook registers repoguard's webhook on a repository and returns its id.
	CreateWebhook(ctx context.Context, repo Repo, url, secret string) (string, error)
	DeleteWebhook(ctx context.Context, repo Repo, id string) error
	// WebURL links to a change request or issue in the platform's UI.
	WebURL(repo Repo, t Target) string
}

// GitSource describes how to fetch a change request over HTTPS.
type GitSource struct {
	URL      string // https clone URL, without credentials
	Username string
	Password string // short-lived/bot token; never put it in a URL or argv
	Ref      string // e.g. refs/pull/7/head, refs/merge-requests/7/head
}

var ErrNotFound = errors.New("not found")

// APIError is a non-2xx response from a platform API.
type APIError struct {
	Method, URL string
	Status      int
	Body        string
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 300 {
		body = body[:300] + "..."
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, body)
}

// Retryable reports whether the request may succeed if repeated.
func (e *APIError) Retryable() bool { return e.Status == 429 || e.Status >= 500 }

// IsRetryable reports whether err is worth retrying (rate limits, 5xx, network).
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	var r interface{ Retryable() bool }
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return false
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// IsForbidden reports a 403, e.g. GitLab refusing commit statuses when CI/CD is disabled.
func IsForbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == 403
}
