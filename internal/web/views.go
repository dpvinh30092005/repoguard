package web

import (
	"fmt"
	"html"
	"html/template"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/ci"
	"github.com/dpvinh30092005/repoguard/internal/platform"
	"github.com/dpvinh30092005/repoguard/internal/results"
	"github.com/dpvinh30092005/repoguard/internal/store"
)

// pill is a status label; an empty Class renders as a muted dash.
type pill struct{ Class, Text string }

func checkPill(state string) pill {
	switch state {
	case "success":
		return pill{"success", "Passed"}
	case "failure":
		return pill{"danger", "Failed"}
	case "error":
		return pill{"danger", "Error"}
	case "pending", "running":
		return pill{"info", "Running…"}
	case "queued":
		return pill{"neutral", "Queued"}
	case results.StateSkipped:
		return pill{"neutral", "Not Run"}
	}
	return pill{}
}

func reviewPill(status string) pill {
	switch status {
	case results.Failing:
		return pill{"danger", "Failing"}
	case results.Passing:
		return pill{"success", "Passing"}
	case results.Running:
		return pill{"info", "Running…"}
	}
	return pill{"neutral", "Not Checked"}
}

func statePill(state string) pill {
	switch state {
	case "merged":
		return pill{"neutral plain", "Merged"}
	case "closed":
		return pill{"neutral plain", "Closed"}
	}
	return pill{}
}

// reviewRow is one change request in a table.
type reviewRow struct {
	results.Review
	Overall pill
	State   pill
	Cells   []pill // one per checks.Catalog entry
	Reason  string // the main problem, inline markdown
	More    int    // other failing checks
}

func newRow(r results.Review) reviewRow {
	row := reviewRow{Review: r, Overall: reviewPill(r.Status()), State: statePill(r.State)}
	for _, info := range checks.Catalog() {
		if info.Name == "ci" {
			switch {
			case r.Build == nil:
				row.Cells = append(row.Cells, pill{})
			case r.BuildOutdated() && r.Build.State != "queued" && r.Build.State != "running":
				row.Cells = append(row.Cells, pill{"neutral", "Older Commit"})
			default:
				row.Cells = append(row.Cells, checkPill(r.Build.State))
			}
			continue
		}
		c, ok := r.Check(info.Name)
		if !ok {
			row.Cells = append(row.Cells, pill{})
			continue
		}
		row.Cells = append(row.Cells, checkPill(c.State))
	}
	row.Reason = r.MainProblem()
	if n := len(r.Failures()); n > 1 {
		row.More = n - 1
	}
	return row
}

func rows(list []results.Review) []reviewRow {
	out := make([]reviewRow, len(list))
	for i, r := range list {
		out[i] = newRow(r)
	}
	return out
}

// counts summarises open change requests.
type counts struct{ Open, Failing, Passing, Running, Closed, Problems, Projects int }

func countReviews(list []results.Review) counts {
	var c counts
	projects := map[string]bool{}
	for _, r := range list {
		if r.State != "open" {
			c.Closed++
			continue
		}
		c.Open++
		projects[r.Platform+":"+r.Repo] = true
		c.Problems += r.Problems()
		switch r.Status() {
		case results.Failing:
			c.Failing++
		case results.Passing:
			c.Passing++
		case results.Running:
			c.Running++
		}
	}
	c.Projects = len(projects)
	return c
}

func (u *UI) reviewsPage(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	all := u.Results.List()
	var shown []results.Review
	for _, rv := range all {
		open := rv.State == "open"
		switch view {
		case "failing":
			if !open || rv.Status() != results.Failing {
				continue
			}
		case "passing":
			if !open || rv.Status() != results.Passing {
				continue
			}
		case "closed":
			if open {
				continue
			}
		default:
			view = "open"
			if !open {
				continue
			}
		}
		if q != "" && !strings.Contains(strings.ToLower(fmt.Sprintf("%s %s %s %d", rv.Title, rv.Repo, rv.Author, rv.Number)), q) {
			continue
		}
		shown = append(shown, rv)
	}
	// Failing first, then most recently updated.
	sort.SliceStable(shown, func(i, j int) bool {
		fi, fj := shown[i].Status() == results.Failing, shown[j].Status() == results.Failing
		if fi != fj && view == "open" {
			return fi
		}
		return shown[i].UpdatedAt.After(shown[j].UpdatedAt)
	})
	u.render(w, r, "reviews", map[string]any{
		"Rows": rows(shown), "View": view, "Q": r.URL.Query().Get("q"), "Counts": countReviews(all),
		"Columns": checks.Catalog(), "Watched": len(u.Store.Repos()), "Refresh": true,
	})
}

// checkView is one check on the detail page.
type checkView struct {
	Info     checks.Info
	Ran      bool
	Check    results.Check
	Pill     pill
	Failed   bool
	Findings []findingView
	Note     string // why it did not run
}

type findingView struct {
	results.Finding
	Tag, TagClass string
}

type stageCell struct {
	pill
	Duration string
}

type buildView struct {
	Build      *results.Build
	Pill       pill
	Outdated   bool
	Stages     []string
	Projects   []buildProject
	Failures   []buildFailure
	Report     *ci.Report
	Sonar      []measure
	SonarPill  pill
	APIFailed  bool
	VulnsPill  pill
	SecretPill pill
}

type buildProject struct {
	Path, Type string
	Cells      []stageCell
}

type buildFailure struct{ Project, Stage, Command, Output string }

type measure struct{ Label, Value string }

func (u *UI) reviewPage(w http.ResponseWriter, r *http.Request) {
	rv, ok := u.Results.Get(r.PathValue("id"))
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		u.render(w, r, "notfound", map[string]any{})
		return
	}
	var views []checkView
	for _, info := range checks.Catalog() {
		if info.Name == "ci" {
			continue
		}
		cv := checkView{Info: info}
		if c, ok := rv.Check(info.Name); ok {
			cv.Ran, cv.Check, cv.Pill = true, c, checkPill(c.State)
			cv.Failed = c.State == "failure" || c.State == "error"
			for _, f := range c.Findings {
				fv := findingView{Finding: f, Tag: "Note", TagClass: "info"}
				switch f.Severity {
				case string(checks.SeverityError):
					fv.Tag, fv.TagClass = "Error", "error"
				case string(checks.SeverityWarning):
					fv.Tag, fv.TagClass = "Warning", "warning"
				}
				cv.Findings = append(cv.Findings, fv)
			}
		} else {
			cv.Pill = pill{"neutral", "Off"}
			cv.Note = "Turned off in this project's .repoguard.yml."
			if info.Name == "ai-review" && !u.AIEnabled() {
				cv.Note = "Add an Anthropic API key in Settings to turn on AI review."
			}
		}
		views = append(views, cv)
	}
	executorOn := false
	if rec, ok := u.Store.Get().Repos[store.Key(rv.Platform, rv.Repo)]; ok {
		executorOn = rec.Executor
	}
	u.render(w, r, "review", map[string]any{
		"R": rv, "Overall": reviewPill(rv.Status()), "State": statePill(rv.State), "Failures": rv.Failures(),
		"Checks": views, "Build": newBuildView(rv), "CIInfo": checks.Lookup("ci"), "ExecutorOn": executorOn,
		"Ex": u.Executor(r.Context()), "Refresh": rv.Status() == results.Running,
	})
}

func newBuildView(rv results.Review) *buildView {
	b := rv.Build
	if b == nil {
		return nil
	}
	v := &buildView{Build: b, Pill: checkPill(b.State), Outdated: rv.BuildOutdated(), Stages: ci.Stages, Report: b.Report}
	rep := b.Report
	if rep == nil {
		return v
	}
	for _, p := range rep.Projects {
		bp := buildProject{Path: p.Project.Path, Type: p.Project.Type}
		for _, name := range ci.Stages {
			cell := stageCell{}
			for _, s := range p.Stages {
				if s.Stage != name {
					continue
				}
				switch s.Status {
				case "passed":
					cell.pill = pill{"success", "Passed"}
				case "failed":
					cell.pill = pill{"danger", "Failed"}
					v.Failures = append(v.Failures, buildFailure{p.Project.Path, s.Stage, s.Command, strings.TrimSpace(s.Output)})
				default:
					cell.pill = pill{"neutral", "Skipped"}
				}
				if s.Status != "skipped" {
					cell.Duration = fmt.Sprintf("%.1fs", s.Duration.Seconds())
				}
			}
			bp.Cells = append(bp.Cells, cell)
		}
		v.Projects = append(v.Projects, bp)
	}
	switch {
	case !rep.SecretsRan:
	case len(rep.Secrets) == 0:
		v.SecretPill = pill{"success", "None Found"}
	default:
		v.SecretPill = pill{"danger", fmt.Sprintf("%d Found", len(rep.Secrets))}
	}
	switch {
	case !rep.Vulns.Ran:
		v.VulnsPill = pill{"neutral", "Not Scanned"}
	case rep.Vulns.Count == 0:
		v.VulnsPill = pill{"success", "None Known"}
	case rep.FailOnVulns:
		v.VulnsPill = pill{"danger", fmt.Sprintf("%d Known", rep.Vulns.Count)}
	default:
		v.VulnsPill = pill{"warning", fmt.Sprintf("%d Known", rep.Vulns.Count)}
	}
	v.APIFailed = rep.APITests.Failed()
	if s := rep.Sonar; s.Ran && s.Error == "" {
		switch s.Gate {
		case "OK":
			v.SonarPill = pill{"success", "Quality Gate Passed"}
		case "ERROR":
			v.SonarPill = pill{"danger", "Quality Gate Failed"}
		case "WARN":
			v.SonarPill = pill{"warning", "Quality Gate Warning"}
		default:
			v.SonarPill = pill{"neutral", "No Quality Gate"}
		}
		for _, m := range ci.SonarMetrics {
			if val, ok := s.Measures[m]; ok {
				v.Sonar = append(v.Sonar, measure{ci.SonarLabel(m), val})
			}
		}
	}
	return v
}

// ---------------------------------------------------------------------------
// Template helpers
// ---------------------------------------------------------------------------

var (
	mdCode = regexp.MustCompile("`([^`]+)`")
	mdBold = regexp.MustCompile(`\*\*([^*]+)\*\*`)
)

// md renders the inline markdown checks use in their messages (code, bold),
// escaping everything else.
func md(s string) template.HTML {
	out := html.EscapeString(s)
	out = mdCode.ReplaceAllString(out, "<code>$1</code>")
	out = mdBold.ReplaceAllString(out, "<b>$1</b>")
	return template.HTML(out) // #nosec G203 -- input escaped above
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func platformName(k string) string {
	if k == string(platform.GitHub) {
		return "GitHub"
	}
	return "GitLab"
}

// ref is how the platform writes a change request number: !12 on GitLab, #12 on GitHub.
func ref(platformKind string, n int) string {
	if platformKind == string(platform.GitHub) {
		return fmt.Sprintf("#%d", n)
	}
	return fmt.Sprintf("!%d", n)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
	return t.Format("2 Jan 2006")
}
