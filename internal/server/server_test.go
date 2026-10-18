package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/checks/prrules"
	"github.com/dpvinh30092005/repoguard/internal/checks/secrets"
	"github.com/dpvinh30092005/repoguard/internal/engine"
	"github.com/dpvinh30092005/repoguard/internal/metrics"
	"github.com/dpvinh30092005/repoguard/internal/platform"
	"github.com/dpvinh30092005/repoguard/internal/platform/github"
	"github.com/dpvinh30092005/repoguard/internal/platform/platformtest"
	"github.com/dpvinh30092005/repoguard/internal/queue"
)

// TestEndToEndGitHub drives the real stack: signed webhook -> queue -> engine
// -> GitHub client -> fake GitHub API, and asserts what GitHub would receive.
func TestEndToEndGitHub(t *testing.T) {
	gh := platformtest.New(t).
		On("GET", "/repos/acme/api/contents/.repoguard.yml", 404, `{"message":"Not Found"}`).
		On("GET", "/repos/acme/api/pulls/7", 200, `{"number":7,"title":"chore: tidy","body":"short","state":"open","head":{"sha":"abc123","ref":"tidy"},"base":{"ref":"main"},"user":{"login":"ann"}}`).
		// Registered after /pulls/7 so this more specific prefix wins.
		On("GET", "/repos/acme/api/pulls/7/files", 200, `[{"filename":"main.go","status":"modified","additions":1,"deletions":0,"patch":"@@ -1 +1,2 @@\n package main\n+func f() {}"}]`).
		On("POST", "/repos/acme/api/statuses/abc123", 201, `{}`).
		On("GET", "/repos/acme/api/issues/7/comments", 200, `[]`).
		On("POST", "/repos/acme/api/issues/7/comments", 201, `{}`)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New()
	client := github.New(gh.URL, nil, "test-token", http.DefaultClient)
	eng := engine.New([]platform.Client{client}, []checks.Check{prrules.Check{}, secrets.Check{}}, log, m)
	q := queue.New(queue.Options{Workers: 2, Logger: log, Metrics: m})
	q.Start(context.Background())
	srv := httptest.NewServer((&Server{GitHubSecret: "s3cret", Queue: q, Handle: eng.Handle, Log: log, Metrics: m}).Routes())
	defer srv.Close()

	body := `{"action":"opened","pull_request":{"number":7,"head":{"sha":"abc123"}},"repository":{"full_name":"acme/api","default_branch":"main"},"sender":{"login":"ann"}}`
	resp := post(t, srv.URL+"/webhooks/github", body, map[string]string{
		"X-GitHub-Event": "pull_request", "X-Hub-Signature-256": sign("s3cret", body), "X-GitHub-Delivery": "1",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.Close(ctx); err != nil {
		t.Fatal(err)
	}

	final := map[string]string{}
	for _, r := range gh.Find("POST", "/repos/acme/api/statuses/abc123") {
		var st map[string]string
		_ = json.Unmarshal([]byte(r.Body), &st)
		final[st["context"]] = st["state"]
	}
	if final["repoguard/pr-rules"] != "failure" || final["repoguard/secrets"] != "success" {
		t.Fatalf("final statuses = %v", final)
	}
	comments := gh.Find("POST", "/repos/acme/api/issues/7/comments")
	if len(comments) != 1 || !strings.Contains(comments[0].Body, "No linked issue") {
		t.Fatalf("summary comment = %+v", comments)
	}
	if m.Get("repoguard_webhooks_total", "platform", "github", "result", "accepted") != 1 {
		t.Error("accepted webhook not counted")
	}
}

func TestWebhookRejectsUnsignedAndDisabled(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := queue.New(queue.Options{Logger: log})
	srv := httptest.NewServer((&Server{GitHubSecret: "s3cret", Queue: q, Log: log, Metrics: metrics.New()}).Routes())
	defer srv.Close()

	if r := post(t, srv.URL+"/webhooks/github", `{}`, map[string]string{"X-GitHub-Event": "pull_request"}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("unsigned: %d", r.StatusCode)
	}
	if r := post(t, srv.URL+"/webhooks/gitlab", `{}`, nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("gitlab disabled: %d", r.StatusCode)
	}
	ping := `{"zen":"x"}`
	if r := post(t, srv.URL+"/webhooks/github", ping, map[string]string{"X-GitHub-Event": "ping", "X-Hub-Signature-256": sign("s3cret", ping)}); r.StatusCode != http.StatusNoContent {
		t.Errorf("ping: %d", r.StatusCode)
	}
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		r, err := http.Get(srv.URL + path)
		if err != nil || r.StatusCode != http.StatusOK {
			t.Errorf("%s: %v %v", path, r.StatusCode, err)
		}
	}
}

func post(t *testing.T, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
