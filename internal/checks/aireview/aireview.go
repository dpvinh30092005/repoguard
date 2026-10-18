// Package aireview asks an LLM to review a change request's diff and turns the
// answer into inline comments. It is advisory by default: findings never fail
// the status unless the repo opts into fail_on_critical.
package aireview

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/diff"
	"github.com/dpvinh30092005/repoguard/internal/glob"
	"github.com/dpvinh30092005/repoguard/internal/platform"
)

// Request is what the model sees. Diff is pre-rendered with line numbers.
type Request struct {
	Repo         string
	Title        string
	Description  string
	Diff         string
	Instructions string
	MaxComments  int
}

type Comment struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"` // critical | major | minor | nit
	Message  string `json:"message"`
}

type Review struct {
	Summary  string    `json:"summary"`
	Comments []Comment `json:"comments"`
}

// Reviewer is implemented by Claude (claude.go) and by fakes in tests.
type Reviewer interface {
	Review(ctx context.Context, req Request) (*Review, error)
}

type Check struct {
	Reviewer Reviewer // nil => check disabled (no API key configured)
}

func (c Check) Name() string { return "ai-review" }

func (c Check) CodeOnly() bool { return true }

func (c Check) Enabled(cfg config.Repo) bool { return c.Reviewer != nil && cfg.AIReview.Enabled }

func (c Check) Run(ctx context.Context, in checks.Input) (checks.Result, error) {
	cfg := in.Config.AIReview
	rendered, parsed, skipped := renderDiff(in.Files, cfg)
	if len(parsed) == 0 {
		return checks.Result{Check: c.Name(), State: platform.StateSuccess, Summary: "Nothing reviewable (only ignored/binary files)"}, nil
	}

	review, err := c.Reviewer.Review(ctx, Request{
		Repo:         in.Repo.FullName,
		Title:        in.CR.Title,
		Description:  in.CR.Body,
		Diff:         rendered,
		Instructions: cfg.Instructions,
		MaxComments:  cfg.MaxComments,
	})
	if err != nil {
		return checks.Result{}, err
	}

	findings, dropped := toFindings(review.Comments, parsed, cfg.MaxComments)
	var critical int
	for _, cm := range review.Comments {
		if cm.Severity == "critical" {
			critical++
		}
	}

	details := review.Summary
	if len(skipped) > 0 {
		// Never silently review a partial diff: say exactly what was left out.
		details += fmt.Sprintf("\n\n_Not reviewed (ignored, binary or over the %d-byte budget): %s_", cfg.MaxDiffBytes, strings.Join(skipped, ", "))
	}
	if dropped > 0 {
		details += fmt.Sprintf("\n\n_%d comment(s) dropped: outside the diff or over the limit._", dropped)
	}

	state := platform.StateSuccess
	if cfg.FailOnCritical && critical > 0 {
		state = platform.StateFailure
		for i := range findings {
			if strings.HasPrefix(findings[i].Message, "**critical**") {
				findings[i].Severity = checks.SeverityError
			}
		}
	}
	return checks.Result{
		Check:    c.Name(),
		State:    state,
		Summary:  fmt.Sprintf("%d suggestion(s), %d critical", len(findings), critical),
		Findings: findings,
		Details:  details,
	}, nil
}

// renderDiff builds the annotated diff within the byte budget, smallest files first
// so one generated monster file can't crowd out the real changes.
func renderDiff(files []platform.FileChange, cfg config.AIReview) (string, map[string]diff.File, []string) {
	sorted := append([]platform.FileChange(nil), files...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].Patch) < len(sorted[j].Patch) })

	var b strings.Builder
	parsed := map[string]diff.File{}
	var skipped []string
	for _, f := range sorted {
		if f.Status == "removed" || f.Patch == "" || glob.Any(cfg.IgnorePaths, f.Path) {
			skipped = append(skipped, f.Path)
			continue
		}
		pf := diff.Parse(f.Path, f.Patch)
		block := fmt.Sprintf("### %s (%s)\n%s\n", f.Path, f.Status, pf.Annotated())
		if cfg.MaxDiffBytes > 0 && b.Len()+len(block) > cfg.MaxDiffBytes {
			skipped = append(skipped, f.Path)
			continue
		}
		b.WriteString(block)
		parsed[f.Path] = pf
	}
	sort.Strings(skipped)
	return b.String(), parsed, skipped
}

var severityRank = map[string]int{"critical": 0, "major": 1, "minor": 2, "nit": 3}

// toFindings keeps only comments anchored on lines the platform will accept,
// most severe first, capped at max.
func toFindings(comments []Comment, parsed map[string]diff.File, max int) ([]checks.Finding, int) {
	valid := make([]Comment, 0, len(comments))
	for _, c := range comments {
		pf, ok := parsed[c.Path]
		if !ok || !pf.Commentable(c.Line) || strings.TrimSpace(c.Message) == "" {
			continue
		}
		valid = append(valid, c)
	}
	sort.SliceStable(valid, func(i, j int) bool { return rank(valid[i].Severity) < rank(valid[j].Severity) })
	dropped := len(comments) - len(valid)
	if max > 0 && len(valid) > max {
		dropped += len(valid) - max
		valid = valid[:max]
	}
	out := make([]checks.Finding, 0, len(valid))
	for _, c := range valid {
		out = append(out, checks.Finding{
			Severity: checks.SeverityWarning,
			Path:     c.Path,
			Line:     c.Line,
			Message:  fmt.Sprintf("**%s** · %s", c.Severity, c.Message),
		})
	}
	return out, dropped
}

func rank(sev string) int {
	if r, ok := severityRank[sev]; ok {
		return r
	}
	return len(severityRank)
}
