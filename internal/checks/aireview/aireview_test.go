package aireview

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/platform"
)

type fakeReviewer struct {
	got    Request
	review Review
}

func (f *fakeReviewer) Review(_ context.Context, req Request) (*Review, error) {
	f.got = req
	return &f.review, nil
}

var files = []platform.FileChange{
	{Path: "api/handler.go", Status: "modified", Patch: "@@ -10,3 +10,4 @@\n func h() {\n-\tdo()\n+\tgo do()\n+\treturn\n }"},
	{Path: "go.sum", Status: "modified", Patch: "@@ -1 +1 @@\n-a\n+b"},
}

func TestCheckFiltersAndRanksComments(t *testing.T) {
	fake := &fakeReviewer{review: Review{
		Summary: "Risky goroutine.",
		Comments: []Comment{
			{Path: "api/handler.go", Line: 12, Severity: "nit", Message: "drop return"},
			{Path: "api/handler.go", Line: 11, Severity: "critical", Message: "goroutine leaks request context"},
			{Path: "api/handler.go", Line: 99, Severity: "major", Message: "line not in diff"},
			{Path: "other.go", Line: 1, Severity: "major", Message: "file not in diff"},
		},
	}}
	cfg := config.Default()
	cfg.AIReview.MaxComments = 5
	res, err := Check{Reviewer: fake}.Run(context.Background(), checks.Input{
		Repo: platform.Repo{FullName: "acme/api"}, CR: &platform.ChangeRequest{Title: "fix: x"}, Files: files, Config: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(fake.got.Diff, "go.sum") || !strings.Contains(fake.got.Diff, "+     11 | \tgo do()") {
		t.Errorf("unexpected diff sent to model:\n%s", fake.got.Diff)
	}
	if len(res.Findings) != 2 || res.Findings[0].Line != 11 || res.Findings[1].Line != 12 {
		t.Fatalf("findings = %+v", res.Findings)
	}
	if res.State != platform.StateSuccess {
		t.Errorf("AI review must be advisory by default, got %s", res.State)
	}
	if !strings.Contains(res.Details, "go.sum") || !strings.Contains(res.Details, "2 comment(s) dropped") {
		t.Errorf("details should disclose skipped files and dropped comments: %s", res.Details)
	}
}

func TestFailOnCritical(t *testing.T) {
	fake := &fakeReviewer{review: Review{Comments: []Comment{{Path: "api/handler.go", Line: 11, Severity: "critical", Message: "bug"}}}}
	cfg := config.Default()
	cfg.AIReview.FailOnCritical = true
	res, _ := Check{Reviewer: fake}.Run(context.Background(), checks.Input{CR: &platform.ChangeRequest{}, Files: files, Config: cfg})
	if res.State != platform.StateFailure || res.Findings[0].Severity != checks.SeverityError {
		t.Fatalf("got %+v", res)
	}
}

// TestClaudeRequestShape checks the wire request against a fake Messages API.
func TestClaudeRequestShape(t *testing.T) {
	var body map[string]any
	var beta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		beta = r.Header.Get("anthropic-beta")
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5",
			"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5},
			"content":[{"type":"text","text":"{\"summary\":\"ok\",\"comments\":[{\"path\":\"a.go\",\"line\":3,\"severity\":\"minor\",\"message\":\"m\"}]}"}]}`)
	}))
	defer srv.Close()

	c := NewClaude("test-key", "claude-opus-5-5", "high", option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
	review, err := c.Review(context.Background(), Request{Repo: "acme/api", Title: "t", Diff: "+ 3 | x\n", MaxComments: 3})
	if err != nil {
		t.Fatal(err)
	}
	if review.Summary != "ok" || len(review.Comments) != 1 || review.Comments[0].Line != 3 {
		t.Fatalf("review = %+v", review)
	}

	if body["model"] != "claude-opus-5-5" || body["fallbacks"] != "default" {
		t.Errorf("model/fallbacks: %v / %v", body["model"], body["fallbacks"])
	}
	if !strings.Contains(beta, "server-side-fallback-2026-07-01") {
		t.Errorf("beta header = %q", beta)
	}
	sys := body["system"].([]any)[0].(map[string]any)
	if sys["cache_control"] == nil {
		t.Error("system prompt should be cacheable")
	}
	oc := body["output_config"].(map[string]any)
	if oc["effort"] != "high" || oc["format"].(map[string]any)["type"] != "json_schema" {
		t.Errorf("output_config = %v", oc)
	}
}
