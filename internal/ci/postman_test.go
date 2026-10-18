package ci

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const newmanReport = `{
  "collection": {"info": {"name": "Orders API"}},
  "run": {
    "stats": {"requests": {"total": 3, "failed": 0}, "assertions": {"total": 5, "failed": 1}},
    "failures": [{"error": {"name": "AssertionError", "test": "status is 200", "message": "expected 500 to equal 200"},
                  "source": {"name": "GET /orders"}}]
  }
}`

var exportFlag = regexp.MustCompile(`--reporter-json-export '([^']+)'`)

func TestPostmanRunsCollectionsAfterStartingTheAPI(t *testing.T) {
	root := t.TempDir()
	write(t, root, "api/postman/orders.postman_collection.json", `{}`)
	write(t, root, "api/node_modules/x/ignored.postman_collection.json", `{}`) // vendored: skipped
	write(t, root, ".repoguard.yml", `
ci:
  secrets_scan: false
  postman:
    environment: api/postman/local.postman_environment.json
    start: npm start
    wait_url: `+mustReadyServer(t)+`
`)

	var commands []string
	var started, stopped bool
	report, err := Run(context.Background(), Options{
		Dir: root, NoVuln: true, Stages: []string{"test"},
		LookCmd: func(string) bool { return true },
		Start: func(_ context.Context, _, cmd string, _ io.Writer) (func(), error) {
			started = cmd == "npm start"
			return func() { stopped = true }, nil
		},
		Runner: func(_ context.Context, _, cmd string, _ io.Writer) error {
			commands = append(commands, cmd)
			if m := exportFlag.FindStringSubmatch(cmd); m != nil {
				_ = os.WriteFile(m[1], []byte(newmanReport), 0o644)
				return &exitError{} // newman exits 1 when an assertion fails
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !started || !stopped {
		t.Errorf("API should be started before and stopped after the tests (started=%v stopped=%v)", started, stopped)
	}
	var newman []string
	for _, c := range commands {
		if strings.HasPrefix(c, "newman run") {
			newman = append(newman, c)
		}
	}
	if len(newman) != 1 || !strings.Contains(newman[0], "'api/postman/orders.postman_collection.json'") ||
		!strings.Contains(newman[0], "--environment 'api/postman/local.postman_environment.json'") {
		t.Fatalf("newman commands = %v", newman)
	}

	a := report.APITests
	if !a.Ran || len(a.Results) != 1 || a.Results[0].Name != "Orders API" || a.Results[0].Assertions != 5 || a.Results[0].Failed != 1 {
		t.Fatalf("api tests = %+v", a)
	}
	if !report.Failed() || !strings.Contains(report.Summary(), "API tests failed") {
		t.Errorf("a failed assertion must fail the report: %s", report.Summary())
	}

	var md, junit bytes.Buffer
	report.WriteMarkdown(&md)
	_ = report.WriteJUnit(&junit)
	if !strings.Contains(md.String(), "**GET /orders** › status is 200: expected 500 to equal 200") {
		t.Errorf("markdown:\n%s", md.String())
	}
	if !strings.Contains(junit.String(), `classname="postman" name="Orders API"`) {
		t.Errorf("junit:\n%s", junit.String())
	}
}

func TestPostmanSkippedWithoutNewmanOrCollections(t *testing.T) {
	root := t.TempDir()
	run := func() APITests {
		r, err := Run(context.Background(), Options{Dir: root, NoVuln: true, Stages: []string{"test"},
			LookCmd: func(string) bool { return false }, Runner: func(context.Context, string, string, io.Writer) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		return r.APITests
	}
	if a := run(); a.Ran || a.Note != "" {
		t.Fatalf("no collections: %+v", a)
	}
	write(t, root, "c.postman_collection.json", `{}`)
	if a := run(); a.Ran || !strings.Contains(a.Note, "newman not installed") {
		t.Fatalf("no newman: %+v", a)
	}
}

func TestPostmanAPINeverReady(t *testing.T) {
	root := t.TempDir()
	write(t, root, "c.postman_collection.json", `{}`)
	write(t, root, ".repoguard.yml", "ci:\n  postman:\n    start: ./serve\n    wait_url: http://127.0.0.1:1/health\n    wait_seconds: 1\n")
	r, _ := Run(context.Background(), Options{Dir: root, NoVuln: true, Stages: []string{"test"},
		LookCmd: func(string) bool { return true },
		Start:   func(context.Context, string, string, io.Writer) (func(), error) { return func() {}, nil },
		Runner:  func(context.Context, string, string, io.Writer) error { return nil }})
	if len(r.APITests.Results) != 1 || !strings.Contains(r.APITests.Results[0].Error, "did not answer") || !r.Failed() {
		t.Fatalf("api tests = %+v", r.APITests)
	}
}

func TestStartBackgroundStopsChildren(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "alive")
	stop, err := startBackground(context.Background(), dir, "sleep 30 & echo $! > "+marker+"; wait", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	stop() // must return promptly and not leave the child sleeping
}

type exitError struct{}

func (*exitError) Error() string { return "exit status 1" }

func mustReadyServer(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	t.Cleanup(srv.Close)
	return srv.URL + "/health"
}
