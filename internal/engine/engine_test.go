package engine

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/checks/aireview"
	"github.com/dpvinh30092005/repoguard/internal/checks/prrules"
	"github.com/dpvinh30092005/repoguard/internal/checks/secrets"
	"github.com/dpvinh30092005/repoguard/internal/metrics"
	"github.com/dpvinh30092005/repoguard/internal/platform"
)

// fakeClient is an in-memory platform that records every write.
type fakeClient struct {
	mu       sync.Mutex
	config   string
	crs      map[int]*platform.ChangeRequest
	files    []platform.FileChange
	issues   map[int]*platform.Issue
	statuses map[string]platform.Status // context -> last status
	comments map[string]string          // "target|marker" -> body
	reviews  [][]platform.ReviewComment
	labels   map[int][]string
	closed   map[int]platform.CloseReason
}

func newFake() *fakeClient {
	return &fakeClient{
		crs: map[int]*platform.ChangeRequest{}, issues: map[int]*platform.Issue{},
		statuses: map[string]platform.Status{}, comments: map[string]string{},
		labels: map[int][]string{}, closed: map[int]platform.CloseReason{},
	}
}

func (f *fakeClient) Kind() platform.Kind { return platform.GitHub }
func (f *fakeClient) ListRepos(context.Context) ([]platform.Repo, error) {
	return []platform.Repo{testRepo}, nil
}
func (f *fakeClient) GetFile(_ context.Context, _ platform.Repo, ref, path string) ([]byte, error) {
	if f.config == "" {
		return nil, platform.ErrNotFound
	}
	return []byte(f.config), nil
}
func (f *fakeClient) GetChangeRequest(_ context.Context, _ platform.Repo, n int) (*platform.ChangeRequest, error) {
	return f.crs[n], nil
}
func (f *fakeClient) ListChanges(context.Context, platform.Repo, *platform.ChangeRequest) ([]platform.FileChange, error) {
	return f.files, nil
}
func (f *fakeClient) SetStatus(_ context.Context, _ platform.Repo, _ string, st platform.Status) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[st.Context] = st
	return nil
}
func (f *fakeClient) UpsertComment(_ context.Context, _ platform.Repo, t platform.Target, marker, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[t.String()+"|"+marker] = body
	return nil
}
func (f *fakeClient) CreateReview(_ context.Context, _ platform.Repo, _ *platform.ChangeRequest, _ string, cs []platform.ReviewComment) error {
	f.reviews = append(f.reviews, cs)
	return nil
}
func (f *fakeClient) ListReviewComments(context.Context, platform.Repo, *platform.ChangeRequest) ([]string, error) {
	var out []string
	for _, r := range f.reviews {
		for _, c := range r {
			out = append(out, c.Body)
		}
	}
	return out, nil
}
func (f *fakeClient) AddLabels(_ context.Context, _ platform.Repo, t platform.Target, ls []string) error {
	f.labels[t.Number] = append(f.labels[t.Number], ls...)
	if i, ok := f.issues[t.Number]; ok {
		i.Labels = append(i.Labels, ls...)
	}
	return nil
}
func (f *fakeClient) RemoveLabel(_ context.Context, _ platform.Repo, t platform.Target, l string) error {
	if i, ok := f.issues[t.Number]; ok {
		var keep []string
		for _, x := range i.Labels {
			if x != l {
				keep = append(keep, x)
			}
		}
		i.Labels = keep
	}
	f.labels[t.Number] = append(f.labels[t.Number], "-"+l)
	return nil
}
func (f *fakeClient) GetIssue(_ context.Context, _ platform.Repo, n int) (*platform.Issue, error) {
	if i, ok := f.issues[n]; ok {
		return i, nil
	}
	return nil, platform.ErrNotFound
}
func (f *fakeClient) ListOpenIssues(context.Context, platform.Repo, int) ([]platform.Issue, error) {
	var out []platform.Issue
	for _, i := range f.issues {
		if i.State == "open" {
			out = append(out, *i)
		}
	}
	return out, nil
}
func (f *fakeClient) CloseIssue(_ context.Context, _ platform.Repo, n int, r platform.CloseReason) error {
	f.closed[n] = r
	f.issues[n].State = "closed"
	return nil
}

func (f *fakeClient) AuthorCanPush(context.Context, platform.Repo, *platform.ChangeRequest) (bool, error) {
	return true, nil
}
func (f *fakeClient) GitSource(context.Context, platform.Repo, *platform.ChangeRequest) (platform.GitSource, error) {
	return platform.GitSource{}, nil
}
func (f *fakeClient) CurrentUser(context.Context) (string, error) { return "bot", nil }
func (f *fakeClient) ListOpenChangeRequests(context.Context, platform.Repo, int) ([]platform.ChangeRequest, error) {
	var out []platform.ChangeRequest
	for _, cr := range f.crs {
		if cr.State == "open" {
			out = append(out, *cr)
		}
	}
	return out, nil
}
func (f *fakeClient) CreateWebhook(context.Context, platform.Repo, string, string) (string, error) {
	return "1", nil
}
func (f *fakeClient) DeleteWebhook(context.Context, platform.Repo, string) error { return nil }
func (f *fakeClient) WebURL(r platform.Repo, t platform.Target) string {
	return "https://example.test/" + r.FullName + "/" + t.String()
}

type stubReviewer struct{ calls int }

func (s *stubReviewer) Review(context.Context, aireview.Request) (*aireview.Review, error) {
	s.calls++
	return &aireview.Review{Summary: "One risky change.", Comments: []aireview.Comment{
		{Path: "app/db.go", Line: 2, Severity: "major", Message: "SQL built with string concatenation: use a parameterized query."},
	}}, nil
}

var testRepo = platform.Repo{Platform: platform.GitHub, ID: "acme/api", FullName: "acme/api", DefaultBranch: "main"}

func newEngine(c *fakeClient, rv aireview.Reviewer) *Engine {
	cks := []checks.Check{prrules.Check{}, secrets.Check{}, aireview.Check{Reviewer: rv}}
	return New([]platform.Client{c}, cks, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.New())
}

func TestPullRequestFlow(t *testing.T) {
	c := newFake()
	awsKey := "AKIA" + "IOSFODNN7REALKEY"
	c.crs[7] = &platform.ChangeRequest{
		Number: 7, Title: "fix(db): handle empty filter", Body: "Avoids a crash when no filter is given.\n\nFixes #3",
		State: "open", HeadSHA: "abc1234567", URL: "https://x/pr/7", Author: "ann",
	}
	c.files = []platform.FileChange{{
		Path: "app/db.go", Status: "modified", Additions: 2, Deletions: 1,
		Patch: "@@ -1,2 +1,3 @@\n package app\n-q := base\n+q := base + filter\n+key := \"" + awsKey + "\"",
	}}
	c.issues[3] = &platform.Issue{Number: 3, Title: "crash", State: "open"}
	rv := &stubReviewer{}
	e := newEngine(c, rv)

	ev := platform.Event{Type: platform.EventCROpened, Repo: testRepo, Number: 7, HeadSHA: "abc1234567", CodeChanged: true}
	if err := e.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}

	if got := c.statuses["repoguard/pr-rules"].State; got != platform.StateSuccess {
		t.Errorf("pr-rules = %s (%s)", got, c.statuses["repoguard/pr-rules"].Description)
	}
	if got := c.statuses["repoguard/secrets"].State; got != platform.StateFailure {
		t.Errorf("secrets = %s", got)
	}
	if got := c.statuses["repoguard/ai-review"].State; got != platform.StateSuccess {
		t.Errorf("ai-review = %s", got)
	}

	summary := c.comments["change_request#7|"+markerSummary]
	for _, want := range []string{"| Secrets | **Failed** |", "One risky change.", "Commit `abc1234`"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q:\n%s", want, summary)
		}
	}
	if strings.Contains(summary, "IOSFODNN7REALKEY") {
		t.Fatal("summary leaks the secret")
	}

	if len(c.reviews) != 1 || len(c.reviews[0]) != 2 {
		t.Fatalf("reviews = %+v", c.reviews)
	}
	if !strings.Contains(c.comments["issue#3|<!-- repoguard:linked-cr-7 -->"], "Being worked on") ||
		strings.Join(c.labels[3], ",") != "in-progress" {
		t.Errorf("issue linking: comments=%v labels=%v", c.comments, c.labels[3])
	}

	// Same findings on the next push must not be reposted.
	ev.Type, ev.HeadSHA = platform.EventCRUpdated, "def"
	if err := e.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if len(c.reviews) != 1 {
		t.Fatalf("duplicate inline comments posted: %d reviews", len(c.reviews))
	}

	// A title edit re-runs cheap checks but not the paid AI review.
	ev.CodeChanged = false
	c.crs[7].Title = "oops no convention"
	if err := e.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if rv.calls != 2 {
		t.Errorf("AI calls = %d, want 2", rv.calls)
	}
	if c.statuses["repoguard/pr-rules"].State != platform.StateFailure {
		t.Error("pr-rules should fail after bad title edit")
	}
	if !strings.Contains(c.comments["change_request#7|"+markerSummary], "| AI Review | Not run |") {
		t.Error("summary should show the skipped AI review")
	}
}

func TestRepoConfigCanDisableChecksAndBadConfigIsReported(t *testing.T) {
	c := newFake()
	c.crs[1] = &platform.ChangeRequest{Number: 1, Title: "whatever", State: "open", HeadSHA: "a"}
	c.config = "pull_request:\n  enabled: false\nai_review:\n  enabled: false\n"
	e := newEngine(c, &stubReviewer{})
	if err := e.Handle(context.Background(), platform.Event{Type: platform.EventCROpened, Repo: testRepo, Number: 1, CodeChanged: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.statuses["repoguard/pr-rules"]; ok {
		t.Error("pr-rules should be disabled")
	}
	if _, ok := c.statuses["repoguard/secrets"]; !ok {
		t.Error("secrets should still run")
	}

	c.config = "pull_request:\n  typo_key: 1\n"
	_ = e.Handle(context.Background(), platform.Event{Type: platform.EventCROpened, Repo: testRepo, Number: 1, CodeChanged: true})
	if !strings.Contains(c.comments["change_request#1|"+markerSummary], "Invalid `.repoguard.yml`") {
		t.Error("broken config should be surfaced in the summary")
	}
}

func TestDraftsAreSkipped(t *testing.T) {
	c := newFake()
	c.crs[1] = &platform.ChangeRequest{Number: 1, State: "open", Draft: true}
	_ = newEngine(c, nil).Handle(context.Background(), platform.Event{Type: platform.EventCROpened, Repo: testRepo, Number: 1, CodeChanged: true})
	if len(c.statuses) != 0 {
		t.Fatalf("draft got statuses: %v", c.statuses)
	}
}

func TestMergeClosesLinkedIssues(t *testing.T) {
	c := newFake()
	c.crs[9] = &platform.ChangeRequest{Number: 9, Title: "fix: x", Body: "Fixes #4, relates to #5", State: "merged"}
	c.issues[4] = &platform.Issue{Number: 4, State: "open", Labels: []string{"in-progress"}}
	c.issues[5] = &platform.Issue{Number: 5, State: "open"}
	if err := newEngine(c, nil).Handle(context.Background(), platform.Event{Type: platform.EventCRMerged, Repo: testRepo, Number: 9}); err != nil {
		t.Fatal(err)
	}
	if c.closed[4] != platform.ClosedCompleted || c.issues[4].HasLabel("in-progress") {
		t.Errorf("issue 4: closed=%v labels=%v", c.closed[4], c.issues[4].Labels)
	}
	if _, ok := c.closed[5]; ok {
		t.Error("non-closing reference must stay open")
	}
}

func TestIssueTriage(t *testing.T) {
	c := newFake()
	c.issues[1] = &platform.Issue{Number: 1, State: "open", Title: "Avatar upload crashes on large PNG", Body: "Uploading a big png avatar crashes the profile page."}
	c.issues[2] = &platform.Issue{Number: 2, State: "open", Author: "bob", Title: "Crash when uploading a PNG avatar", Body: "## Steps to reproduce\nupload png avatar on profile page\n"}
	e := newEngine(c, nil)

	if err := e.Handle(context.Background(), platform.Event{Type: platform.EventIssueOpened, Repo: testRepo, Number: 2}); err != nil {
		t.Fatal(err)
	}
	if !c.issues[2].HasLabel("bug") || !c.issues[2].HasLabel("needs-info") || !c.issues[2].HasLabel("possible-duplicate") {
		t.Fatalf("labels = %v", c.issues[2].Labels)
	}
	if body := c.comments["issue#2|"+markerNeedsInfo]; !strings.Contains(body, "**Expected**") || !strings.Contains(body, "@bob") {
		t.Errorf("needs-info comment = %q", body)
	}
	if body := c.comments["issue#2|"+markerDuplicates]; !strings.Contains(body, "#1 Avatar upload") {
		t.Errorf("duplicates comment = %q", body)
	}

	// Reporter fills in the template: label goes away, comment flips to thanks.
	c.issues[2].Body += "\n## Expected\nimage saved\n## Actual\nHTTP 500\n"
	if err := e.Handle(context.Background(), platform.Event{Type: platform.EventIssueEdited, Repo: testRepo, Number: 2}); err != nil {
		t.Fatal(err)
	}
	if c.issues[2].HasLabel("needs-info") || !strings.Contains(c.comments["issue#2|"+markerNeedsInfo], "everything needed") {
		t.Errorf("after edit: labels=%v comment=%q", c.issues[2].Labels, c.comments["issue#2|"+markerNeedsInfo])
	}
}

func TestSweepStaleAndSLA(t *testing.T) {
	c := newFake()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c.issues[1] = &platform.Issue{Number: 1, State: "open", UpdatedAt: now.AddDate(0, 0, -90), CreatedAt: now.AddDate(-1, 0, 0)}
	c.issues[2] = &platform.Issue{Number: 2, State: "open", Labels: []string{"stale"}, UpdatedAt: now.AddDate(0, 0, -20)}
	c.issues[3] = &platform.Issue{Number: 3, State: "open", Labels: []string{"P0"}, UpdatedAt: now, CreatedAt: now.Add(-30 * time.Hour)}
	e := newEngine(c, nil)
	e.Now = func() time.Time { return now }

	if err := e.SweepRepo(context.Background(), testRepo); err != nil {
		t.Fatal(err)
	}
	if !c.issues[1].HasLabel("stale") {
		t.Error("idle issue should be marked stale")
	}
	if c.closed[2] != platform.ClosedNotPlanned {
		t.Error("stale issue without reaction should close as not planned")
	}
	if !strings.Contains(c.comments["issue#3|"+markerSLA], "no assignee") {
		t.Error("SLA breach should be flagged")
	}

	// A human comment removes the stale label.
	if err := e.Handle(context.Background(), platform.Event{Type: platform.EventIssueActivity, Repo: testRepo, Number: 1}); err != nil {
		t.Fatal(err)
	}
	if c.issues[1].HasLabel("stale") {
		t.Error("activity should un-stale the issue")
	}
}

type recordingExecutor struct{ submitted []int }

func (r *recordingExecutor) Allowed(repo platform.Repo) bool { return repo.FullName == "acme/api" }
func (r *recordingExecutor) Submit(_ platform.Repo, cr *platform.ChangeRequest) error {
	r.submitted = append(r.submitted, cr.Number)
	return nil
}

func TestExecutorRunsOnlyOnNewCommits(t *testing.T) {
	c := newFake()
	c.crs[1] = &platform.ChangeRequest{Number: 1, Title: "feat: x", State: "open", HeadSHA: "a"}
	e := newEngine(c, nil)
	x := &recordingExecutor{}
	e.Executor = x

	_ = e.Handle(context.Background(), platform.Event{Type: platform.EventCROpened, Repo: testRepo, Number: 1, CodeChanged: true})
	_ = e.Handle(context.Background(), platform.Event{Type: platform.EventCRUpdated, Repo: testRepo, Number: 1, CodeChanged: false})
	other := testRepo
	other.FullName = "acme/not-allowed"
	_ = e.Handle(context.Background(), platform.Event{Type: platform.EventCROpened, Repo: other, Number: 1, CodeChanged: true})
	if len(x.submitted) != 1 {
		t.Fatalf("submitted = %v, want exactly one (new commits on an allowed repo)", x.submitted)
	}
}

type forbiddingClient struct{ *fakeClient }

func (f forbiddingClient) SetStatus(context.Context, platform.Repo, string, platform.Status) error {
	return &platform.APIError{Status: 403, Body: "CI/CD disabled"}
}

func TestStatusForbiddenFallsBackToComment(t *testing.T) {
	c := newFake()
	c.crs[1] = &platform.ChangeRequest{Number: 1, Title: "bad title", State: "open", HeadSHA: "a"}
	e := newEngine(c, nil)
	e.Clients.Set(forbiddingClient{c})
	if err := e.Handle(context.Background(), platform.Event{Type: platform.EventCROpened, Repo: testRepo, Number: 1, CodeChanged: true}); err != nil {
		t.Fatalf("403 on statuses must not fail the job: %v", err)
	}
	if !strings.Contains(c.comments["change_request#1|"+markerSummary], "| Conventions | **Failed** |") {
		t.Error("results must still be posted in the summary comment")
	}
}
