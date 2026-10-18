package ci

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func monorepo(t *testing.T) string {
	root := t.TempDir()
	write(t, root, "services/api/package.json", `{}`)
	write(t, root, "services/api/package-lock.json", `{}`)
	write(t, root, "services/api/node_modules/dep/package.json", `{}`) // must be ignored
	write(t, root, "services/billing/go.mod", "module billing\n")
	write(t, root, "workers/etl/pyproject.toml", "[project]\nname='etl'\n")
	write(t, root, "docs/requirements.txt", "mkdocs\n") // docs are not a project
	write(t, root, "too/deep/for/detect/go.mod", "module x\n")
	return root
}

func TestDetect(t *testing.T) {
	got := Detect(monorepo(t), 3, nil)
	var summary []string
	for _, p := range got {
		summary = append(summary, p.Type+":"+p.Path)
	}
	want := []string{"node:services/api", "go:services/billing", "python:workers/etl"}
	if !reflect.DeepEqual(summary, want) {
		t.Fatalf("got %v, want %v", summary, want)
	}
	if got[0].Commands["install"] != "npm ci" {
		t.Errorf("node with lockfile should use npm ci, got %q", got[0].Commands["install"])
	}
}

func TestAffected(t *testing.T) {
	ps := []Project{{Path: "services/api"}, {Path: "services/billing"}}
	names := func(ps []Project) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.Path)
		}
		return out
	}
	if got := names(Affected(ps, []string{"services/api/src/x.ts", "README.md"})); !reflect.DeepEqual(got, []string{"services/api"}) {
		t.Errorf("got %v", got)
	}
	if got := names(Affected(ps, []string{".github/workflows/ci.yml"})); len(got) != 2 {
		t.Errorf("root change should select all, got %v", got)
	}
	if got := names(Affected(ps, []string{"docs/a.md"})); len(got) != 0 {
		t.Errorf("docs only should select none, got %v", got)
	}
}

func TestRunStopsProjectAtFirstFailureAndReports(t *testing.T) {
	root := monorepo(t)
	write(t, root, ".repoguard.yml", "ci:\n  fail_on_vulns: true\n")
	aws := "AKIA" + "IOSFODNN7REALKEY"
	write(t, root, "services/billing/config.go", "package billing\nconst key = \""+aws+"\"\n")

	var ran []string
	report, err := Run(context.Background(), Options{
		Dir: root,
		Runner: func(_ context.Context, dir, command string, out io.Writer) error {
			ran = append(ran, filepath.Base(dir)+":"+command)
			if strings.HasPrefix(command, "go vet") {
				io.WriteString(out, "config.go:2: vet: something odd\n")
				return errors.New("exit 1")
			}
			return nil
		},
		LookCmd: func(string) bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Failed() {
		t.Fatal("report should fail")
	}
	for _, r := range ran {
		if strings.HasPrefix(r, "billing:go build") || strings.HasPrefix(r, "billing:go test") {
			t.Errorf("stages after a failure must not run: %v", ran)
		}
	}
	if len(report.Secrets) != 1 || report.Secrets[0].Path != "services/billing/config.go" || report.Secrets[0].Line != 2 {
		t.Errorf("secrets = %+v", report.Secrets)
	}

	var md, junit bytes.Buffer
	report.WriteMarkdown(&md)
	if err := report.WriteJUnit(&junit); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"❌ failed", "| `services/billing` | go | ✅", "something odd", "osv-scanner not installed"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("markdown missing %q:\n%s", want, md.String())
		}
	}
	if strings.Contains(md.String(), "IOSFODNN7REALKEY") || strings.Contains(junit.String(), "IOSFODNN7REALKEY") {
		t.Fatal("report leaks the secret")
	}
	if !strings.Contains(junit.String(), `<testcase classname="services/billing" name="lint"`) || !strings.Contains(junit.String(), `failures="2"`) {
		t.Errorf("junit:\n%s", junit.String())
	}
}

func TestDeclaredProjectsOverrideDetection(t *testing.T) {
	root := monorepo(t)
	write(t, root, ".repoguard.yml", "ci:\n  projects:\n    - path: services/api\n      type: node\n      commands:\n        test: npm run test:ci\n")
	var ran []string
	_, err := Run(context.Background(), Options{
		Dir: root, NoVuln: true, Stages: []string{"install", "test"},
		Runner: func(_ context.Context, dir, command string, _ io.Writer) error {
			ran = append(ran, command)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ran, []string{"npm ci", "npm run test:ci"}) {
		t.Fatalf("ran %v", ran)
	}
}

func TestCountOSV(t *testing.T) {
	n, err := CountOSV([]byte(`{"results":[{"packages":[{"vulnerabilities":[{"id":"GHSA-1"},{"id":"GHSA-2"}]},{"vulnerabilities":[]}]}]}`))
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
