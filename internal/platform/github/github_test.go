package github

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example/repoguard/internal/platform"
	"github.com/example/repoguard/internal/platform/platformtest"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func hook(t *testing.T, event, body string, sig string) ([]platform.Event, error) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-Hub-Signature-256", sig)
	r.Header.Set("X-GitHub-Delivery", "d-1")
	return ParseWebhook(r, []byte(body), "s3cret")
}

const repoJSON = `"repository":{"full_name":"acme/api","default_branch":"main"},"installation":{"id":42},"sender":{"login":"ann","type":"User"}`

func TestParseWebhook(t *testing.T) {
	cases := []struct {
		name, event, body string
		want              platform.EventType
		code              bool
	}{
		{"pr opened", "pull_request", `{"action":"opened","pull_request":{"number":7,"head":{"sha":"abc"}},` + repoJSON + `}`, platform.EventCROpened, true},
		{"pr pushed", "pull_request", `{"action":"synchronize","pull_request":{"number":7,"head":{"sha":"def"}},` + repoJSON + `}`, platform.EventCRUpdated, true},
		{"pr title edited", "pull_request", `{"action":"edited","changes":{"title":{"from":"x"}},"pull_request":{"number":7},` + repoJSON + `}`, platform.EventCRUpdated, false},
		{"pr base edited only", "pull_request", `{"action":"edited","changes":{"base":{}},"pull_request":{"number":7},` + repoJSON + `}`, "", false},
		{"pr merged", "pull_request", `{"action":"closed","pull_request":{"number":7,"merged":true},` + repoJSON + `}`, platform.EventCRMerged, false},
		{"pr closed unmerged", "pull_request", `{"action":"closed","pull_request":{"number":7,"merged":false},` + repoJSON + `}`, "", false},
		{"issue opened", "issues", `{"action":"opened","issue":{"number":3},` + repoJSON + `}`, platform.EventIssueOpened, false},
		{"issue labeled", "issues", `{"action":"labeled","issue":{"number":3},` + repoJSON + `}`, "", false},
		{"human comment", "issue_comment", `{"action":"created","issue":{"number":3},"comment":{"body":"still happens","user":{"type":"User"}},` + repoJSON + `}`, platform.EventIssueActivity, false},
		{"own comment", "issue_comment", `{"action":"created","issue":{"number":3},"comment":{"body":"<!-- repoguard:x -->","user":{"type":"User"}},` + repoJSON + `}`, "", false},
		{"comment on PR", "issue_comment", `{"action":"created","issue":{"number":3,"pull_request":{}},"comment":{"body":"lgtm","user":{"type":"User"}},` + repoJSON + `}`, "", false},
		{"ping", "ping", `{"zen":"hi"}`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evs, err := hook(t, c.event, c.body, sign("s3cret", c.body))
			if err != nil {
				t.Fatal(err)
			}
			if c.want == "" {
				if len(evs) != 0 {
					t.Fatalf("expected no event, got %+v", evs)
				}
				return
			}
			if len(evs) != 1 || evs[0].Type != c.want || evs[0].CodeChanged != c.code {
				t.Fatalf("got %+v", evs)
			}
			if r := evs[0].Repo; r.ID != "acme/api" || r.InstallationID != 42 || r.DefaultBranch != "main" {
				t.Fatalf("repo = %+v", r)
			}
		})
	}
}

func TestParseWebhookRejectsBadSignature(t *testing.T) {
	body := `{"action":"opened"}`
	for _, sig := range []string{"", "sha256=00", sign("wrong", body), "sha1=abc"} {
		if _, err := hook(t, "pull_request", body, sig); err != ErrBadSignature {
			t.Errorf("sig %q: err = %v", sig, err)
		}
	}
}

func TestAppAuthJWTAndTokenCache(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	api := platformtest.New(t).On("POST", "/app/installations/42/access_tokens", 201, `{"token":"ghs_inst","expires_at":"2999-01-01T00:00:00Z"}`)

	app, err := NewAppAuth(123, pemKey, api.URL, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		tok, err := app.Token(context.Background(), 42)
		if err != nil || tok != "ghs_inst" {
			t.Fatalf("token=%q err=%v", tok, err)
		}
	}
	reqs := api.Find("POST", "/app/installations/42/access_tokens")
	if len(reqs) != 1 {
		t.Fatalf("token endpoint called %d times, want 1 (cache)", len(reqs))
	}

	// The JWT must verify against the App's public key.
	jwt := strings.TrimPrefix(reqs[0].Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt = %q", jwt)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("jwt signature: %v", err)
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if !strings.Contains(string(claims), `"iss":"123"`) {
		t.Fatalf("claims = %s", claims)
	}
}

func TestUpsertCommentEditsExisting(t *testing.T) {
	api := platformtest.New(t).
		On("GET", "/repos/acme/api/issues/7/comments", 200, `[{"id":1,"body":"hello"},{"id":2,"body":"<!-- repoguard:summary -->\nold"}]`).
		On("PATCH", "/repos/acme/api/issues/comments/2", 200, `{}`)
	c := New(api.URL, nil, "tok", http.DefaultClient)
	repo := platform.Repo{Platform: platform.GitHub, ID: "acme/api"}
	if err := c.UpsertComment(context.Background(), repo, platform.CR(7), "<!-- repoguard:summary -->", "new"); err != nil {
		t.Fatal(err)
	}
	patch := api.Find("PATCH", "/repos/acme/api/issues/comments/2")
	if len(patch) != 1 || !strings.Contains(patch[0].Body, "new") || len(api.Find("POST", "/")) != 0 {
		t.Fatalf("requests = %+v", api.Requests)
	}
	if got := patch[0].Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("auth = %q", got)
	}
}

func TestUpsertCommentCreatesWhenMissing(t *testing.T) {
	api := platformtest.New(t).
		On("GET", "/repos/acme/api/issues/7/comments", 200, `[]`).
		On("POST", "/repos/acme/api/issues/7/comments", 201, `{}`)
	c := New(api.URL, nil, "tok", http.DefaultClient)
	err := c.UpsertComment(context.Background(), platform.Repo{ID: "acme/api"}, platform.IssueT(7), "<!-- m -->", "body")
	if err != nil || len(api.Find("POST", "/repos/acme/api/issues/7/comments")) != 1 {
		t.Fatalf("err=%v requests=%+v", err, api.Requests)
	}
}

func TestListOpenIssuesSkipsPullRequests(t *testing.T) {
	api := platformtest.New(t).On("GET", "/repos/acme/api/issues?state=open", 200,
		`[{"number":1,"title":"a","state":"open"},{"number":2,"title":"pr","pull_request":{}},{"number":3,"title":"b","labels":[{"name":"bug"}]}]`)
	c := New(api.URL, nil, "tok", http.DefaultClient)
	issues, err := c.ListOpenIssues(context.Background(), platform.Repo{ID: "acme/api"}, 10)
	if err != nil || len(issues) != 2 || issues[1].Number != 3 || !issues[1].HasLabel("BUG") {
		t.Fatalf("issues=%+v err=%v", issues, err)
	}
}

func TestNotFoundAndRetryable(t *testing.T) {
	api := platformtest.New(t).On("GET", "/repos/acme/api/pulls/9", 502, `bad gateway`)
	c := New(api.URL, nil, "tok", http.DefaultClient)
	repo := platform.Repo{ID: "acme/api"}
	if _, err := c.GetFile(context.Background(), repo, "main", ".repoguard.yml"); err == nil || !platformNotFound(err) {
		t.Errorf("GetFile missing: %v", err)
	}
	if _, err := c.GetChangeRequest(context.Background(), repo, 9); !platform.IsRetryable(err) {
		t.Errorf("502 should be retryable: %v", err)
	}
}
