package ci

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpvinh30092005/repoguard/internal/config"
)

func fakeSonar(t *testing.T, gate string) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization")+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/qualitygates/project_status":
			_, _ = io.WriteString(w, `{"projectStatus":{"status":"`+gate+`","conditions":[
				{"status":"ERROR","metricKey":"new_coverage","comparator":"LT","errorThreshold":"80","actualValue":"41.2"},
				{"status":"OK","metricKey":"new_bugs","comparator":"GT","errorThreshold":"0","actualValue":"0"}]}}`)
		case "/api/measures/component":
			_, _ = io.WriteString(w, `{"component":{"measures":[{"metric":"bugs","value":"2"},{"metric":"coverage","value":"41.2"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func sonarRun(t *testing.T, root string, env map[string]string, onRun func(dir, cmd string)) Report {
	t.Helper()
	report, err := Run(context.Background(), Options{
		Dir: root, NoVuln: true, Stages: []string{"test"},
		LookCmd: func(string) bool { return true },
		Getenv:  func(k string) string { return env[k] },
		Runner: func(_ context.Context, dir, cmd string, _ io.Writer) error {
			if onRun != nil {
				onRun(dir, cmd)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestSonarReportsQualityGate(t *testing.T) {
	srv, seen := fakeSonar(t, "ERROR")
	root := t.TempDir()
	env := map[string]string{"SONAR_HOST_URL": srv.URL + "/", "SONAR_TOKEN": "squ_secret", "CI_PROJECT_PATH": "team/billing api"}

	var scanCmd string
	report := sonarRun(t, root, env, func(dir, cmd string) {
		if strings.HasPrefix(cmd, "'sonar-scanner'") {
			scanCmd = cmd
			_ = os.MkdirAll(filepath.Join(dir, ".scannerwork"), 0o755)
			_ = os.WriteFile(filepath.Join(dir, ".scannerwork", "report-task.txt"), []byte("projectKey=team_billing_api\ndashboardUrl="+srv.URL+"/dashboard?id=team_billing_api\n"), 0o644)
		}
	})

	if !strings.Contains(scanCmd, "-Dsonar.projectKey=team_billing_api") || !strings.Contains(scanCmd, "sonar.qualitygate.wait=true") {
		t.Fatalf("scanner command = %q", scanCmd)
	}
	if strings.Contains(scanCmd, "squ_secret") {
		t.Fatal("the token must not appear in the command line")
	}
	for _, s := range *seen {
		if !strings.HasPrefix(s, "Bearer squ_secret ") {
			t.Fatalf("API call without token: %s", s)
		}
	}

	s := report.Sonar
	if !s.Ran || s.Gate != "ERROR" || s.Measures["bugs"] != "2" || !strings.HasSuffix(s.URL, "/dashboard?id=team_billing_api") {
		t.Fatalf("sonar = %+v", s)
	}
	if !report.Failed() || !strings.Contains(report.Summary(), "quality gate failed") {
		t.Errorf("failed gate must fail the report: %s", report.Summary())
	}
	var md bytes.Buffer
	report.WriteMarkdown(&md)
	for _, want := range []string{"**Quality Gate failed**", "Bugs: **2**", "`new_coverage` = 41.2 (limit LT 80)"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("markdown missing %q:\n%s", want, md.String())
		}
	}
}

func TestSonarGateCanBeAdvisory(t *testing.T) {
	srv, _ := fakeSonar(t, "ERROR")
	root := t.TempDir()
	write(t, root, ".repoguard.yml", "ci:\n  sonar:\n    fail_on_gate: false\n    project_key: my-key\n")
	r := sonarRun(t, root, map[string]string{"SONAR_HOST_URL": srv.URL, "SONAR_TOKEN": "t"}, nil)
	if r.Sonar.ProjectKey != "my-key" || r.Failed() {
		t.Fatalf("advisory gate: key=%s failed=%v", r.Sonar.ProjectKey, r.Failed())
	}
}

func TestSonarNotConfigured(t *testing.T) {
	if r := sonarRun(t, t.TempDir(), nil, nil); r.Sonar.Ran || r.Sonar.Note != "" {
		t.Fatalf("no env: %+v", r.Sonar)
	}
	r := sonarRun(t, t.TempDir(), map[string]string{"SONAR_HOST_URL": "http://sonar"}, nil)
	if r.Sonar.Ran || !strings.Contains(r.Sonar.Note, "SONAR_TOKEN") {
		t.Fatalf("half configured: %+v", r.Sonar)
	}
}

func TestSonarServerUnreachable(t *testing.T) {
	r := sonarRun(t, t.TempDir(), map[string]string{"SONAR_HOST_URL": "http://127.0.0.1:1", "SONAR_TOKEN": "t"}, nil)
	if !r.Sonar.Ran || r.Sonar.Error == "" || !r.Failed() {
		t.Fatalf("unreachable server should be reported: %+v", r.Sonar)
	}
}

func TestSonarProjectKey(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if k := sonarProjectKey(config.Sonar{}, "/x/my repo", get(nil)); k != "my_repo" {
		t.Errorf("from dir: %q", k)
	}
	if k := sonarProjectKey(config.Sonar{}, ".", get(map[string]string{"REPOGUARD_REPO": "grp/sub/app"})); k != "grp_sub_app" {
		t.Errorf("from env: %q", k)
	}
}
