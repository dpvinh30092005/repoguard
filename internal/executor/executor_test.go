package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/ci"
	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/metrics"
	"github.com/dpvinh30092005/repoguard/internal/platform"
	"github.com/dpvinh30092005/repoguard/internal/queue"
)

// fakeClient implements only what the executor uses; the embedded interface
// panics loudly if anything else is called.
type fakeClient struct {
	platform.Client
	canPush   bool
	policy    string // default-branch .repoguard.yml; "" = none
	statusErr error
	statuses  []platform.Status
	comment   string
}

func (f *fakeClient) Kind() platform.Kind { return platform.GitLab }
func (f *fakeClient) WebURL(platform.Repo, platform.Target) string {
	return "https://gitlab.example/mr"
}
func (f *fakeClient) AuthorCanPush(context.Context, platform.Repo, *platform.ChangeRequest) (bool, error) {
	return f.canPush, nil
}
func (f *fakeClient) GitSource(context.Context, platform.Repo, *platform.ChangeRequest) (platform.GitSource, error) {
	return platform.GitSource{URL: "https://gitlab.example/grp/app.git", Username: "oauth2", Password: "glpat-secret", Ref: "refs/merge-requests/5/head"}, nil
}
func (f *fakeClient) GetFile(context.Context, platform.Repo, string, string) ([]byte, error) {
	if f.policy == "" {
		return nil, platform.ErrNotFound
	}
	return []byte(f.policy), nil
}
func (f *fakeClient) SetStatus(_ context.Context, _ platform.Repo, _ string, st platform.Status) error {
	f.statuses = append(f.statuses, st)
	return f.statusErr
}
func (f *fakeClient) UpsertComment(_ context.Context, _ platform.Repo, _ platform.Target, _, body string) error {
	f.comment = body
	return nil
}

type fakeSandbox struct {
	res     Result
	err     error
	calls   int
	sawFile map[string]string // files present in srcDir when Run was called
	sawEnv  map[string]string
}

func (s *fakeSandbox) Run(_ context.Context, dir string, env map[string]string) (Result, error) {
	s.calls++
	s.sawEnv = env
	s.sawFile = map[string]string{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		s.sawFile[e.Name()] = string(b)
	}
	return s.res, s.err
}

var repo = platform.Repo{Platform: platform.GitLab, ID: "9", FullName: "grp/app", DefaultBranch: "main"}

func newExec(c *fakeClient, sb Sandbox, fetchErr error) *Executor {
	return &Executor{
		Clients: platform.NewRegistry(c),
		Sandbox: sb,
		Allow:   AllowGlobs([]string{"grp/*"}),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: metrics.New(),
		fetch: func(context.Context, platform.GitSource, string) (string, error) {
			if fetchErr != nil {
				return "", fetchErr
			}
			dir, _ := os.MkdirTemp("", "src-")
			// The change request tries to switch its own checks off.
			_ = os.WriteFile(filepath.Join(dir, config.FileName), []byte("ci:\n  secrets_scan: false\n"), 0o644)
			_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)
			return dir, nil
		},
	}
}

func reportJSON(t *testing.T, failed bool) []byte {
	status := "passed"
	if failed {
		status = "failed"
	}
	b, err := json.Marshal(ci.Report{Projects: []ci.ProjectResult{{
		Project: ci.Project{Path: ".", Type: "go"},
		Stages:  []ci.StageResult{{Stage: "test", Command: "go test ./...", Status: status, Duration: time.Second, Output: "--- FAIL: TestX"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cr() *platform.ChangeRequest {
	return &platform.ChangeRequest{Number: 5, Author: "ann", HeadSHA: "abcdef1234567"}
}

func TestRunReportsPassAndPinsPolicy(t *testing.T) {
	c := &fakeClient{canPush: true, policy: "ci:\n  fail_on_vulns: true\n"}
	sb := &fakeSandbox{res: Result{Report: reportJSON(t, false)}}
	x := newExec(c, sb, nil)
	x.Env = map[string]string{"SONAR_TOKEN": "squ_x"}
	if err := x.Run(context.Background(), repo, cr()); err != nil {
		t.Fatal(err)
	}
	if sb.sawEnv["REPOGUARD_REPO"] != "grp/app" || sb.sawEnv["SONAR_TOKEN"] != "squ_x" {
		t.Errorf("sandbox env = %v", sb.sawEnv)
	}
	if got := sb.sawFile[config.FileName]; got != c.policy {
		t.Errorf("sandbox saw policy %q, want the default-branch one", got)
	}
	last := c.statuses[len(c.statuses)-1]
	if c.statuses[0].State != platform.StatePending || last.State != platform.StateSuccess || last.Context != StatusContext {
		t.Errorf("statuses = %+v", c.statuses)
	}
	if !strings.Contains(c.comment, "Build & Tests passed") || !strings.Contains(c.comment, "Commit `abcdef1`") {
		t.Errorf("comment = %s", c.comment)
	}
}

func TestRunRemovesPolicyWhenDefaultBranchHasNone(t *testing.T) {
	sb := &fakeSandbox{res: Result{Report: reportJSON(t, false)}}
	_ = newExec(&fakeClient{canPush: true}, sb, nil).Run(context.Background(), repo, cr())
	if _, ok := sb.sawFile[config.FileName]; ok {
		t.Error("the change request's own .repoguard.yml must not reach the sandbox")
	}
}

func TestRunReportsFailureWithOutput(t *testing.T) {
	c := &fakeClient{canPush: true}
	_ = newExec(c, &fakeSandbox{res: Result{Report: reportJSON(t, true), ExitCode: 1}}, nil).Run(context.Background(), repo, cr())
	if last := c.statuses[len(c.statuses)-1]; last.State != platform.StateFailure || !strings.Contains(last.Description, "1 failed") {
		t.Errorf("status = %+v", last)
	}
	if !strings.Contains(c.comment, "--- FAIL: TestX") {
		t.Errorf("comment should include the failing output: %s", c.comment)
	}
}

func TestUntrustedAndForkAreNeverBuilt(t *testing.T) {
	for name, tc := range map[string]struct {
		canPush bool
		fork    bool
		want    string
	}{
		"untrusted author": {canPush: false, want: "@ann does not have Developer"},
		"fork":             {canPush: true, fork: true, want: "from a fork"},
	} {
		t.Run(name, func(t *testing.T) {
			c := &fakeClient{canPush: tc.canPush}
			sb := &fakeSandbox{}
			r := cr()
			r.FromFork = tc.fork
			if err := newExec(c, sb, nil).Run(context.Background(), repo, r); err != nil {
				t.Fatal(err)
			}
			if sb.calls != 0 || len(c.statuses) != 0 || !strings.Contains(c.comment, tc.want) {
				t.Fatalf("calls=%d statuses=%v comment=%q", sb.calls, c.statuses, c.comment)
			}
		})
	}
}

func TestStaleHeadIsSkippedQuietly(t *testing.T) {
	c := &fakeClient{canPush: true}
	sb := &fakeSandbox{}
	if err := newExec(c, sb, ErrStale).Run(context.Background(), repo, cr()); err != nil {
		t.Fatal(err)
	}
	if sb.calls != 0 || c.comment != "" {
		t.Fatal("stale run must not build or comment")
	}
}

func TestTimeoutCrashAndForbiddenStatus(t *testing.T) {
	c := &fakeClient{canPush: true, statusErr: &platform.APIError{Status: 403}}
	if err := newExec(c, &fakeSandbox{res: Result{TimedOut: true, Logs: "compiling..."}}, nil).Run(context.Background(), repo, cr()); err != nil {
		t.Fatalf("403 on statuses must not fail the run: %v", err)
	}
	if !strings.Contains(c.comment, "time limit") || !strings.Contains(c.comment, "compiling...") {
		t.Errorf("timeout comment = %s", c.comment)
	}

	c = &fakeClient{canPush: true}
	_ = newExec(c, &fakeSandbox{res: Result{ExitCode: 137}}, nil).Run(context.Background(), repo, cr())
	if !strings.Contains(c.comment, "exited with code 137") {
		t.Errorf("crash comment = %s", c.comment)
	}

	c = &fakeClient{canPush: true}
	err := newExec(c, &fakeSandbox{err: errors.New("docker daemon down")}, nil).Run(context.Background(), repo, cr())
	if err == nil || c.statuses[len(c.statuses)-1].State != platform.StateError {
		t.Errorf("sandbox error: err=%v statuses=%v", err, c.statuses)
	}
}

func TestSubmitRespectsAllowList(t *testing.T) {
	q := queue.New(queue.Options{Workers: 1})
	x := newExec(&fakeClient{}, &fakeSandbox{}, nil)
	x.Queue = q
	if err := x.Submit(platform.Repo{FullName: "other/app"}, cr()); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("err = %v", err)
	}
	if err := x.Submit(repo, cr()); err != nil {
		t.Fatal(err)
	}
	if err := x.Submit(repo, cr()); !errors.Is(err, queue.ErrDuplicate) {
		t.Fatalf("same head twice: %v", err)
	}
}

// TestFetchFromRealRepo exercises the git plumbing against a local repository.
func TestFetchFromRealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	origin := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", origin}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(origin, "app.go"), []byte("package app\n"), 0o644)
	run("add", ".")
	run("commit", "-qm", "init")
	sha := run("rev-parse", "HEAD")

	src := platform.GitSource{URL: "file://" + origin, Ref: "refs/heads/main", Username: "u", Password: "s3cret"}
	dir, err := fetch(context.Background(), src, sha)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if b, _ := os.ReadFile(filepath.Join(dir, "app.go")); string(b) != "package app\n" {
		t.Fatalf("checkout content = %q", b)
	}
	cfg, _ := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if bytes.Contains(cfg, []byte("s3cret")) || bytes.Contains(cfg, []byte("Authorization")) {
		t.Fatal("credentials leaked into .git/config (it is copied into the sandbox)")
	}

	if _, err := fetch(context.Background(), src, "0000000000000000000000000000000000000000"); !errors.Is(err, ErrStale) {
		t.Fatalf("moved head: err = %v", err)
	}
}

func TestDockerCreateArgsAreLockedDown(t *testing.T) {
	d := Docker{Image: "img:1", CPUs: "2", Memory: "4g", PidsLimit: 512, Network: "bridge", Runtime: "runsc"}
	args := strings.Join(d.CreateArgs("rg-1", []string{"SONAR_TOKEN"}), " ")
	if !strings.Contains(args, "--env SONAR_TOKEN ") || strings.Contains(args, "SONAR_TOKEN=") {
		t.Errorf("env must be passed by name only: %s", args)
	}
	for _, want := range []string{"--cpus 2", "--memory 4g --memory-swap 4g", "--pids-limit 512", "--cap-drop ALL",
		"--security-opt no-new-privileges", "--runtime runsc", "img:1 repoguard ci --dir /work --json " + ReportFile} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in: %s", want, args)
		}
	}
	for _, banned := range []string{"-v ", "--volume", "--mount", "--privileged", "=squ_", "docker.sock"} {
		if strings.Contains(args, banned) {
			t.Errorf("sandbox args must not contain %q: %s", banned, args)
		}
	}
}

func TestFirstFileFromTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: ReportFile, Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte(`{"x"`))
	_ = tw.Close()
	got, err := firstFileFromTar(buf.Bytes())
	if err != nil || string(got) != `{"x"` {
		t.Fatalf("got %q err %v", got, err)
	}
}
