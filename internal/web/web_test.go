package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dpvinh30092005/repoguard/internal/activity"
	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/platform"
	"github.com/dpvinh30092005/repoguard/internal/poller"
	"github.com/dpvinh30092005/repoguard/internal/results"
	"github.com/dpvinh30092005/repoguard/internal/store"
)

type fakeClient struct {
	platform.Client
	mu      sync.Mutex
	hooks   []string
	deleted []string
}

func (f *fakeClient) Kind() platform.Kind { return platform.GitLab }
func (f *fakeClient) ListRepos(context.Context) ([]platform.Repo, error) {
	return []platform.Repo{
		{Platform: platform.GitLab, ID: "1", FullName: "team/api", DefaultBranch: "main"},
		{Platform: platform.GitLab, ID: "2", FullName: "team/web", DefaultBranch: "main"},
	}, nil
}
func (f *fakeClient) CreateWebhook(_ context.Context, r platform.Repo, url, secret string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hooks = append(f.hooks, r.FullName+" "+url+" "+secret)
	return "77", nil
}
func (f *fakeClient) DeleteWebhook(_ context.Context, _ platform.Repo, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeClient) ListOpenChangeRequests(context.Context, platform.Repo, int) ([]platform.ChangeRequest, error) {
	return []platform.ChangeRequest{{Number: 5, HeadSHA: "abc"}}, nil
}
func (f *fakeClient) WebURL(platform.Repo, platform.Target) string {
	return "https://gitlab.example/mr/5"
}

type harness struct {
	t       *testing.T
	srv     *httptest.Server
	client  *http.Client
	store   *store.Store
	fake    *fakeClient
	applied int
	events  []platform.Event
	public  string
	results *results.Store
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, fake: &fakeClient{}}
	st, err := store.Open(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	h.store = st
	reg := platform.NewRegistry(h.fake)
	h.results, _ = results.Open("")
	act := activity.New(10)
	act.Add(activity.Entry{Repo: "team/api", Kind: "MR", Number: 5, Event: "change_request.opened", Result: "ok", URL: "https://gitlab.example/mr/5"})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ui := &UI{
		Store: st, Clients: reg, Activity: act, Results: h.results, Log: log, Version: "test",
		Poller:   &poller.Poller{Clients: reg, Enqueue: func(e platform.Event) { h.events = append(h.events, e) }, Log: log},
		Password: func() string { return "s3cret-pass" },
		Apply: func(context.Context) map[platform.Kind]Status {
			h.applied++
			if st.Get().GitLabToken == "" {
				return map[platform.Kind]Status{}
			}
			return map[platform.Kind]Status{platform.GitLab: {Configured: true, User: "bot"}}
		},
		Secret:     func(platform.Kind) string { return "hook-secret" },
		PublicURL:  func() string { return h.public },
		QueueDepth: func() int { return 0 },
		AIEnabled:  func() bool { return false },
		Locked:     map[string]bool{},
		Executor: func(context.Context) ExecutorInfo {
			return ExecutorInfo{Available: true, Backend: "docker", Image: "repoguard-ci:local", ImagePresent: true}
		},
	}
	if err := ui.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.srv = httptest.NewServer(ui.Routes())
	t.Cleanup(h.srv.Close)
	jar, _ := cookiejar.New(nil)
	h.client = &http.Client{Jar: jar}
	return h
}

func (h *harness) get(path string) (int, string) {
	h.t.Helper()
	resp, err := h.client.Get(h.srv.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *harness) post(path string, form url.Values) (int, string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", h.srv.URL)
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *harness) login() {
	h.t.Helper()
	if code, body := h.post("/login", url.Values{"password": {"s3cret-pass"}}); code != 200 || !strings.Contains(body, "Overview") {
		h.t.Fatalf("login: %d\n%s", code, body)
	}
}

var repoForm = url.Values{"platform": {"gitlab"}, "id": {"1"}, "full_name": {"team/api"}, "default_branch": {"main"}}

func TestPagesRequireLogin(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/", "/connect", "/repos", "/guide"} {
		if code, body := h.get(p); code != 200 || !strings.Contains(body, "Sign in to the admin console") {
			t.Errorf("%s without session should land on the login page, got %d", p, code)
		}
	}
	if code, body := h.post("/login", url.Values{"password": {"wrong"}}); code != 200 || !strings.Contains(body, "Wrong password") {
		t.Errorf("wrong password: %d", code)
	}
}

func TestEveryPageRenders(t *testing.T) {
	h := newHarness(t)
	h.login()
	for path, want := range map[string]string{
		"/":               "Checked new commits",
		"/connect":        "Save Connection",
		"/repos":          "team/web",
		"/guide":          "Try It in 5 Minutes",
		"/merge-requests": "No Projects Checked Yet",
	} {
		if code, body := h.get(path); code != 200 || !strings.Contains(body, want) {
			t.Errorf("%s: %d, missing %q", path, code, want)
		}
	}
}

func TestConnectSavesAndApplies(t *testing.T) {
	h := newHarness(t)
	h.login()
	before := h.applied
	code, body := h.post("/connect", url.Values{"section": {"gitlab"}, "gitlab_url": {"https://gitlab.example/"}, "gitlab_token": {"glpat-123"}})
	if code != 200 || !strings.Contains(body, "Connected to GitLab as @bot") {
		t.Fatalf("connect: %d\n%s", code, body)
	}
	s := h.store.Get()
	if s.GitLabToken != "glpat-123" || s.GitLabURL != "https://gitlab.example" || h.applied != before+1 {
		t.Fatalf("settings=%+v applied=%d", s, h.applied)
	}
	// An empty token field keeps the stored token.
	h.post("/connect", url.Values{"section": {"gitlab"}, "gitlab_url": {"https://gitlab.example"}, "gitlab_token": {""}})
	if h.store.Get().GitLabToken != "glpat-123" {
		t.Fatal("blank token field must not erase the saved token")
	}
	if _, body := h.get("/connect"); strings.Contains(body, "glpat-123") {
		t.Fatal("the connect page must never render a saved token")
	}
}

func TestWatchPollingThenWebhook(t *testing.T) {
	h := newHarness(t)
	h.login()

	h.post("/repos/watch", repoForm)
	rec := h.store.Get().Repos["gitlab:team/api"]
	if rec.Mode != "poll" || len(h.fake.hooks) != 0 {
		t.Fatalf("without a public URL repos are polled: %+v hooks=%v", rec, h.fake.hooks)
	}

	h.post("/repos/executor", repoForm)
	if !h.store.Get().Repos["gitlab:team/api"].Executor {
		t.Fatal("executor toggle did not switch on")
	}

	code, body := h.post("/repos/check", repoForm)
	if code != 200 || len(h.events) != 1 || !h.events[0].CodeChanged || !strings.Contains(body, "Checking 1 merge request of team/api") {
		t.Fatalf("check now: %d events=%+v", code, h.events)
	}

	h.post("/repos/unwatch", repoForm)
	if _, ok := h.store.Get().Repos["gitlab:team/api"]; ok {
		t.Fatal("unwatch did not remove the repo")
	}

	h.public = "https://repoguard.example"
	h.post("/repos/watch", repoForm)
	rec = h.store.Get().Repos["gitlab:team/api"]
	if rec.Mode != "webhook" || rec.HookID != "77" || len(h.fake.hooks) != 1 ||
		h.fake.hooks[0] != "team/api https://repoguard.example/webhooks/gitlab hook-secret" {
		t.Fatalf("webhook mode: %+v hooks=%v", rec, h.fake.hooks)
	}
	h.post("/repos/unwatch", repoForm)
	if len(h.fake.deleted) != 1 || h.fake.deleted[0] != "77" {
		t.Fatalf("unwatch must delete the webhook: %v", h.fake.deleted)
	}
}

func TestCrossSitePostIsBlocked(t *testing.T) {
	h := newHarness(t)
	h.login()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/repos/watch", strings.NewReader(repoForm.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(h.store.Get().Repos) != 0 {
		t.Fatalf("cross-site POST: %d, repos=%v", resp.StatusCode, h.store.Get().Repos)
	}
}

func TestTamperedSessionIsRejected(t *testing.T) {
	h := newHarness(t)
	u, _ := url.Parse(h.srv.URL)
	h.client.Jar.SetCookies(u, []*http.Cookie{{Name: cookieName, Value: "9999999999.deadbeef"}})
	if _, body := h.get("/"); !strings.Contains(body, "Sign in to the admin console") {
		t.Fatal("forged cookie must not grant access")
	}
}

func TestReviewPagesShowFindings(t *testing.T) {
	h := newHarness(t)
	h.login()
	repo := platform.Repo{Platform: platform.GitLab, FullName: "team/api"}
	cr := &platform.ChangeRequest{Number: 55, Title: "Event catalogue <script>", Author: "tu", HeadSHA: "abcdef123", State: "open", URL: "https://gitlab.example/mr/55"}
	h.results.RecordChecks(repo, cr, cr.URL, []checks.Result{
		{Check: "pr-rules", State: platform.StateFailure, Summary: "1 convention problem(s)", Findings: []checks.Finding{
			{Severity: checks.SeverityError, Message: "Title `update stuff` does not match <b>"},
		}},
		{Check: "secrets", State: platform.StateSuccess, Summary: "No secrets"},
	}, nil, "")

	_, list := h.get("/merge-requests?view=failing")
	for _, want := range []string{"Event catalogue &lt;script&gt;", "!55", "Conventions: Title <code>update stuff</code> does not match &lt;b&gt;", "Failing <span class=\"n\">1</span>"} {
		if !strings.Contains(list, want) {
			t.Fatalf("list missing %q", want)
		}
	}
	id := results.IDFor("gitlab", "team/api", 55)
	code, page := h.get("/merge-requests/" + id)
	for _, want := range []string{"Title <code>update stuff</code> does not match &lt;b&gt;", "How to fix:", "Open in GitLab", "Commit <code>abcdef1</code>"} {
		if code != 200 || !strings.Contains(page, want) {
			t.Fatalf("detail %d missing %q", code, want)
		}
	}
	if strings.Contains(page, "<script>") && strings.Contains(page, "catalogue <script>") {
		t.Fatal("titles must be escaped")
	}
	if code, _ := h.get("/merge-requests/nope"); code != 404 {
		t.Fatalf("unknown id: %d", code)
	}
	if _, overview := h.get("/"); !strings.Contains(overview, "Needs Attention") || !strings.Contains(overview, "/merge-requests/"+id) {
		t.Fatal("overview should list the failing merge request")
	}
}

func TestStylesheetIsServedWithoutLogin(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Get(h.srv.URL + "/assets/app.css")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") {
		t.Fatalf("css: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}
