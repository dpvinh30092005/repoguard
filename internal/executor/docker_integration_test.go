//go:build integration

package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/ci"
)

// Run with: REPOGUARD_TEST_IMAGE=repoguard-ci:test go test -tags integration -run Docker ./internal/executor/
func dockerSandbox(t *testing.T, timeout time.Duration) Docker {
	image := os.Getenv("REPOGUARD_TEST_IMAGE")
	if image == "" {
		t.Skip("REPOGUARD_TEST_IMAGE not set")
	}
	// No network: also proves the sandbox works fully offline for dependency-free builds.
	return Docker{Image: image, CPUs: "1", Memory: "1g", PidsLimit: 512, Network: "none", Timeout: timeout}
}

func goProject(t *testing.T, testBody string) string {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":      "module sample\n\ngo 1.22\n",
		"sum.go":      "package sample\n\nfunc Sum(a, b int) int { return a + b }\n",
		"sum_test.go": "package sample\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) {\n" + testBody + "\n}\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runIn(t *testing.T, d Docker, dir string) (Result, ci.Report) {
	t.Helper()
	res, err := d.Run(context.Background(), dir)
	if err != nil {
		t.Fatalf("sandbox: %v\nlogs:\n%s", err, res.Logs)
	}
	var report ci.Report
	if res.Report != nil {
		if err := json.Unmarshal(res.Report, &report); err != nil {
			t.Fatalf("report: %v", err)
		}
	}
	return res, report
}

func TestDockerSandboxPassingProject(t *testing.T) {
	// The fixture must be gofmt-clean: the sandbox runs the lint stage too.
	res, report := runIn(t, dockerSandbox(t, 10*time.Minute), goProject(t, "\tif Sum(2, 2) != 4 {\n\t\tt.Fatal(\"bad\")\n\t}"))
	if res.TimedOut || res.Report == nil {
		t.Fatalf("no report (exit %d, timed out %v):\n%s", res.ExitCode, res.TimedOut, res.Logs)
	}
	if report.Failed() || len(report.Projects) != 1 || report.Projects[0].Project.Type != "go" {
		t.Fatalf("report = %+v\nlogs:\n%s", report, res.Logs)
	}
}

func TestDockerSandboxFailingProject(t *testing.T) {
	res, report := runIn(t, dockerSandbox(t, 10*time.Minute), goProject(t, "\tt.Fatal(\"intentional failure\")"))
	if res.ExitCode != 1 || !report.Failed() {
		t.Fatalf("exit=%d failed=%v\nlogs:\n%s", res.ExitCode, report.Failed(), res.Logs)
	}
}

func TestDockerSandboxTimeout(t *testing.T) {
	res, _ := runIn(t, dockerSandbox(t, 5*time.Second), goProject(t, "\tselect {}"))
	if !res.TimedOut {
		t.Fatalf("expected timeout, got exit %d\nlogs:\n%s", res.ExitCode, res.Logs)
	}
}
