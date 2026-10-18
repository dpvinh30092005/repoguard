// Package ci implements `repoguard ci`: the half of repoguard that runs inside
// the repository's own pipeline (GitHub Actions, GitLab CI, ...). Executing
// untrusted repo code belongs on the repo's runners, not on the shared
// repoguard service — so build, test and dependency scans happen here.
package ci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/checks/secrets"
	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/glob"
)

var Stages = []string{"install", "lint", "build", "test"}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	".venv": true, "venv": true, "__pycache__": true, "testdata": true, "docs": true, "target": true,
}

type Project struct {
	Path     string            `json:"path"` // relative to the repo root, "." for root
	Type     string            `json:"type"`
	Commands map[string]string `json:"commands"`
}

type StageResult struct {
	Stage    string        `json:"stage"`
	Command  string        `json:"command"`
	Status   string        `json:"status"` // passed | failed | skipped
	Duration time.Duration `json:"duration_ns"`
	Output   string        `json:"output,omitempty"` // tail of the output, for failures
}

type ProjectResult struct {
	Project Project       `json:"project"`
	Stages  []StageResult `json:"stages"`
}

func (p ProjectResult) Failed() bool {
	for _, s := range p.Stages {
		if s.Status == "failed" {
			return true
		}
	}
	return false
}

type SecretFinding struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Rule   string `json:"rule"`
	Masked string `json:"masked"`
}

type VulnResult struct {
	Ran   bool   `json:"ran"`
	Count int    `json:"count"`
	Note  string `json:"note,omitempty"`
}

// Report is also the JSON contract between `repoguard ci --json` and the executor.
type Report struct {
	Projects    []ProjectResult `json:"projects"`
	Secrets     []SecretFinding `json:"secrets"`
	SecretsRan  bool            `json:"secrets_ran"`
	Vulns       VulnResult      `json:"vulns"`
	FailOnVulns bool            `json:"fail_on_vulns"`
	APITests    APITests        `json:"api_tests"`
	Sonar       SonarResult     `json:"sonar"`
}

// Summary is a one-line result for a commit status (<= 140 chars).
func (r Report) Summary() string {
	failed := 0
	for _, p := range r.Projects {
		if p.Failed() {
			failed++
		}
	}
	s := fmt.Sprintf("%d project(s): %d passed, %d failed", len(r.Projects), len(r.Projects)-failed, failed)
	if len(r.Secrets) > 0 {
		s += fmt.Sprintf(", %d secret(s)", len(r.Secrets))
	}
	if r.Vulns.Count > 0 {
		s += fmt.Sprintf(", %d vuln(s)", r.Vulns.Count)
	}
	if r.APITests.Failed() {
		s += ", API tests failed"
	}
	if r.Sonar.Failed() {
		s += ", quality gate failed"
	}
	return s
}

func (r Report) Failed() bool {
	for _, p := range r.Projects {
		if p.Failed() {
			return true
		}
	}
	return len(r.Secrets) > 0 || (r.FailOnVulns && r.Vulns.Count > 0) || r.APITests.Failed() || r.Sonar.Failed()
}

type Options struct {
	Dir     string
	Base    string   // only check projects changed since this git ref
	Stages  []string // subset of Stages
	NoVuln  bool
	Out     io.Writer // live command output
	Runner  func(ctx context.Context, dir, command string, out io.Writer) error
	LookCmd func(name string) bool
	// Start launches a long-running command (e.g. the API under test) and returns a stop func.
	Start func(ctx context.Context, dir, command string, out io.Writer) (stop func(), err error)
	// Getenv reads configuration such as SONAR_HOST_URL (default os.Getenv).
	Getenv func(string) string
}

// Run detects projects, runs their stages and the repo-wide scans.
func Run(ctx context.Context, opts Options) (Report, error) {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Runner == nil {
		opts.Runner = runShell
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.Start == nil {
		opts.Start = startBackground
	}
	if opts.LookCmd == nil {
		opts.LookCmd = func(name string) bool { _, err := exec.LookPath(name); return err == nil }
	}
	if len(opts.Stages) == 0 {
		opts.Stages = Stages
	}

	cfg := config.Default()
	if data, err := os.ReadFile(filepath.Join(opts.Dir, config.FileName)); err == nil {
		if cfg, err = config.ParseRepo(data); err != nil {
			return Report{}, err
		}
	}
	ciCfg := cfg.CI

	projects := resolveProjects(opts.Dir, ciCfg)
	var changed []string
	if opts.Base != "" {
		var err error
		if changed, err = changedFiles(ctx, opts.Dir, opts.Base); err != nil {
			fmt.Fprintf(opts.Out, "warning: %v; checking everything\n", err)
			changed = nil
		} else {
			projects = Affected(projects, changed)
		}
	}

	report := Report{FailOnVulns: ciCfg.FailOnVulns}
	for _, p := range projects {
		report.Projects = append(report.Projects, runProject(ctx, opts, p))
	}

	if ciCfg.SecretsScan {
		report.SecretsRan = true
		files := changed
		if files == nil {
			files = trackedFiles(ctx, opts.Dir)
		}
		report.Secrets = scanSecrets(opts.Dir, files, ciCfg.IgnorePaths)
	}

	// API tests run after the builds, against a freshly started app.
	report.APITests = runPostman(ctx, opts, ciCfg.Postman, ciCfg.IgnorePaths)
	report.Sonar = runSonar(ctx, opts, ciCfg.Sonar)

	if opts.NoVuln {
		report.Vulns = VulnResult{Note: "disabled (--no-vuln)"}
	} else {
		report.Vulns = scanVulns(ctx, opts)
	}
	return report, nil
}

// resolveProjects uses the projects declared in .repoguard.yml, or detects them.
// Declared commands override the defaults for that project's type.
func resolveProjects(dir string, c config.CI) []Project {
	if len(c.Projects) == 0 {
		return Detect(dir, c.MaxDepth, c.IgnorePaths)
	}
	out := make([]Project, 0, len(c.Projects))
	for _, p := range c.Projects {
		path := strings.Trim(filepath.ToSlash(filepath.Clean(p.Path)), "/")
		if path == "" {
			path = "."
		}
		cmds := DefaultCommands(p.Type, filepath.Join(dir, path))
		for stage, cmd := range p.Commands {
			cmds[stage] = cmd
		}
		out = append(out, Project{Path: path, Type: p.Type, Commands: cmds})
	}
	return out
}

func runProject(ctx context.Context, opts Options, p Project) ProjectResult {
	res := ProjectResult{Project: p}
	dir := filepath.Join(opts.Dir, p.Path)
	failed := false
	for _, stage := range opts.Stages {
		cmd := p.Commands[stage]
		if cmd == "" || failed {
			res.Stages = append(res.Stages, StageResult{Stage: stage, Command: cmd, Status: "skipped"})
			continue
		}
		fmt.Fprintf(opts.Out, "\n==> [%s] %s: %s\n", p.Path, stage, cmd)
		tail := &tailWriter{max: 8 << 10}
		start := time.Now()
		err := opts.Runner(ctx, dir, cmd, io.MultiWriter(opts.Out, tail))
		sr := StageResult{Stage: stage, Command: cmd, Status: "passed", Duration: time.Since(start)}
		if err != nil {
			sr.Status, sr.Output, failed = "failed", tail.String(), true
		}
		res.Stages = append(res.Stages, sr)
	}
	return res
}

func runShell(ctx context.Context, dir, command string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "bash", "-eo", "pipefail", "-c", command)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, out, out
	return cmd.Run()
}

// ---------------------------------------------------------------------------
// Project detection
// ---------------------------------------------------------------------------

// Detect finds projects by marker files up to maxDepth directories deep.
func Detect(root string, maxDepth int, ignore []string) []Project {
	var out []Project
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if rel != "." {
			if skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") || glob.Any(ignore, rel+"/") || strings.Count(rel, "/")+1 > maxDepth {
				return filepath.SkipDir
			}
		}
		has := func(name string) bool { _, err := os.Stat(filepath.Join(path, name)); return err == nil }
		if has("go.mod") {
			out = append(out, Project{Path: rel, Type: "go", Commands: DefaultCommands("go", path)})
		}
		if has("package.json") {
			out = append(out, Project{Path: rel, Type: "node", Commands: DefaultCommands("node", path)})
		}
		if has("pyproject.toml") || has("setup.py") || has("requirements.txt") {
			out = append(out, Project{Path: rel, Type: "python", Commands: DefaultCommands("python", path)})
		}
		return nil
	})
	return out
}

// DefaultCommands returns the conventional commands for a project type,
// adapted to the lockfiles present in dir.
func DefaultCommands(typ, dir string) map[string]string {
	has := func(name string) bool { _, err := os.Stat(filepath.Join(dir, name)); return err == nil }
	switch typ {
	case "go":
		return map[string]string{
			"install": "go mod download",
			"lint":    `go vet ./... && test -z "$(gofmt -l . | tee /dev/stderr)"`,
			"build":   "go build ./...",
			"test":    "go test -race ./...",
		}
	case "node":
		install := "npm install"
		switch {
		case has("package-lock.json"):
			install = "npm ci"
		case has("pnpm-lock.yaml"):
			install = "corepack enable && pnpm install --frozen-lockfile"
		case has("yarn.lock"):
			install = "corepack enable && yarn install --immutable"
		}
		return map[string]string{
			"install": install,
			"lint":    "npm run lint --if-present",
			"build":   "npm run build --if-present",
			"test":    "npm test --if-present",
		}
	case "python":
		install := "python -m pip install -r requirements.txt"
		if has("pyproject.toml") || has("setup.py") {
			install = `python -m pip install -e ".[dev]"`
		}
		if has("requirements-dev.txt") {
			install += " && python -m pip install -r requirements-dev.txt"
		}
		return map[string]string{
			"install": install,
			"lint":    `if command -v ruff >/dev/null; then ruff check .; else echo "ruff not installed: skipping lint"; fi`,
			"build":   "python -m compileall -q -x '(^|/)(\\.venv|venv|node_modules)/' .",
			"test":    `python -m pytest || [ $? -eq 5 ]`, // 5 = no tests collected
		}
	}
	return map[string]string{}
}

// Affected keeps projects owning at least one changed file. A changed file
// outside every project (shared config at the root) conservatively selects all.
func Affected(projects []Project, changed []string) []Project {
	hit := map[string]bool{}
	for _, f := range changed {
		if glob.Match("*.md", f) {
			continue
		}
		owner := ""
		for _, p := range projects {
			if (p.Path == "." || strings.HasPrefix(f, p.Path+"/")) && (owner == "" || len(p.Path) > len(owner)) {
				owner = p.Path
			}
		}
		if owner == "" {
			return projects
		}
		hit[owner] = true
	}
	var out []Project
	for _, p := range projects {
		if hit[p.Path] {
			out = append(out, p)
		}
	}
	return out
}

func changedFiles(ctx context.Context, dir, base string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "diff", "--name-only", base+"...HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s...HEAD: %w", base, err)
	}
	return nonEmptyLines(string(out)), nil
}

func trackedFiles(ctx context.Context, dir string) []string {
	if out, err := exec.CommandContext(ctx, "git", "-C", dir, "ls-files").Output(); err == nil {
		return nonEmptyLines(string(out))
	}
	var files []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && skipDirs[d.Name()] && d.Name() != "testdata" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	return files
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Scans
// ---------------------------------------------------------------------------

func scanSecrets(dir string, files, ignore []string) []SecretFinding {
	var out []SecretFinding
	for _, f := range files {
		if glob.Any(ignore, f) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil || len(data) > 1<<20 || bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			continue // deleted, huge or binary
		}
		for _, m := range secrets.ScanText(string(data)) {
			out = append(out, SecretFinding{Path: f, Line: m.Line, Rule: m.Rule, Masked: m.Masked})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path+fmt.Sprint(out[i].Line) < out[j].Path+fmt.Sprint(out[j].Line) })
	return out
}

// scanVulns runs osv-scanner (all ecosystems, OSV database) when available.
func scanVulns(ctx context.Context, opts Options) VulnResult {
	if !opts.LookCmd("osv-scanner") {
		return VulnResult{Note: "osv-scanner not installed: skipped"}
	}
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, "osv-scanner", "scan", "source", "-r", "--format", "json", ".")
	cmd.Dir, cmd.Stdout, cmd.Stderr = opts.Dir, &stdout, opts.Out
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !(errors.As(err, &exitErr) && exitErr.ExitCode() == 1) { // 1 = vulnerabilities found
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 {
			return VulnResult{Ran: true, Note: "no package manifests found"}
		}
		return VulnResult{Note: fmt.Sprintf("osv-scanner failed: %v", err)}
	}
	n, perr := CountOSV(stdout.Bytes())
	if perr != nil {
		return VulnResult{Note: fmt.Sprintf("could not parse osv-scanner output: %v", perr)}
	}
	return VulnResult{Ran: true, Count: n}
}

// CountOSV counts vulnerable (package, advisory) pairs in osv-scanner JSON output.
func CountOSV(data []byte) (int, error) {
	var out struct {
		Results []struct {
			Packages []struct {
				Vulnerabilities []json.RawMessage `json:"vulnerabilities"`
			} `json:"packages"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range out.Results {
		for _, p := range r.Packages {
			n += len(p.Vulnerabilities)
		}
	}
	return n, nil
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	buf []byte
	max int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string { return string(t.buf) }
