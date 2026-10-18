//go:build integration

package executor

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/ci"
	"github.com/dpvinh30092005/repoguard/internal/platform"
)

// TestJenkinsRealServer triggers the real "repoguard-ci" job of the demo stack.
// The CI workflow sets the REPOGUARD_TEST_JENKINS_* / _REPO_* variables.
func TestJenkinsRealServer(t *testing.T) {
	url := os.Getenv("REPOGUARD_TEST_JENKINS_URL")
	if url == "" {
		t.Skip("REPOGUARD_TEST_JENKINS_URL not set")
	}
	policy, _ := os.ReadFile(os.Getenv("REPOGUARD_TEST_POLICY_FILE"))
	j := &Jenkins{
		URL: url, User: os.Getenv("REPOGUARD_TEST_JENKINS_USER"), Token: os.Getenv("REPOGUARD_TEST_JENKINS_TOKEN"),
		Job: "repoguard-ci", Poll: 3 * time.Second, Timeout: 25 * time.Minute,
	}
	res, err := j.Build(context.Background(), RemoteBuild{
		Repo:     platform.Repo{FullName: os.Getenv("REPOGUARD_TEST_REPO_NAME")},
		CR:       &platform.ChangeRequest{Number: 1},
		CloneURL: os.Getenv("REPOGUARD_TEST_REPO_URL"),
		Ref:      os.Getenv("REPOGUARD_TEST_REPO_REF"),
		SHA:      os.Getenv("REPOGUARD_TEST_REPO_SHA"),
		Policy:   policy,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.TimedOut || res.Report == nil {
		t.Fatalf("no report (timed out %v, exit %d) from %s\nlog tail:\n%s", res.TimedOut, res.ExitCode, res.URL, res.Logs)
	}
	var report ci.Report
	if err := json.Unmarshal(res.Report, &report); err != nil {
		t.Fatal(err)
	}
	var md strings.Builder
	report.WriteMarkdown(&md)
	t.Logf("%s (%s)\n%s", res.Backend, res.URL, md.String())

	if len(report.Projects) < 4 {
		t.Errorf("expected the demo repo's projects, got %d", len(report.Projects))
	}
	if !report.APITests.Ran || len(report.APITests.Results) == 0 {
		t.Errorf("Postman collection should have run: %+v", report.APITests)
	}
	if want := os.Getenv("REPOGUARD_TEST_EXPECT_SONAR"); want != "" && (!report.Sonar.Ran || report.Sonar.Gate == "") {
		t.Errorf("SonarQube analysis should have run: %+v", report.Sonar)
	}
	if report.Failed() || res.ExitCode != 0 {
		t.Errorf("build failed (exit %d): %s\nlog tail:\n%s", res.ExitCode, report.Summary(), res.Logs)
	}
}
