// Package prrules enforces team conventions on change requests: title format,
// description, linked issue and size.
package prrules

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/example/repoguard/internal/checks"
	"github.com/example/repoguard/internal/config"
	"github.com/example/repoguard/internal/triage"
)

type Check struct{}

func (Check) Name() string                 { return "pr-rules" }
func (Check) Enabled(cfg config.Repo) bool { return cfg.PullRequest.Enabled }

func (Check) Run(_ context.Context, in checks.Input) (checks.Result, error) {
	rules := in.Config.PullRequest
	cr := in.CR
	var findings []checks.Finding
	fail := func(format string, args ...any) {
		findings = append(findings, checks.Finding{Severity: checks.SeverityError, Message: fmt.Sprintf(format, args...)})
	}
	warn := func(format string, args ...any) {
		findings = append(findings, checks.Finding{Severity: checks.SeverityWarning, Message: fmt.Sprintf(format, args...)})
	}

	if rules.TitlePattern != "" {
		re, err := regexp.Compile(rules.TitlePattern)
		if err != nil {
			return checks.Result{}, fmt.Errorf("title_pattern: %w", err)
		}
		if !re.MatchString(cr.Title) {
			fail("Title `%s` does not match the convention `%s` (e.g. `feat(api): add login endpoint`).", cr.Title, rules.TitlePattern)
		}
	}

	if rules.RequireDescription {
		if n := len(strings.TrimSpace(stripComments(cr.Body))); n < rules.MinDescriptionLength {
			fail("Description is too short (%d chars, minimum %d). Explain what changed and why.", n, rules.MinDescriptionLength)
		}
	}

	if rules.RequireLinkedIssue {
		if refs := triage.ParseRefs(cr.Title, cr.Body, cr.HeadRef); len(refs.All) == 0 {
			fail("No linked issue. Reference one in the description (`Closes #123`) or the branch name (`feature/123-...`).")
		}
	}

	lines, files := 0, len(in.Files)
	for _, f := range in.Files {
		lines += f.Additions + f.Deletions
	}
	if rules.MaxChangedLines > 0 && lines > rules.MaxChangedLines {
		warn("Large change: %d lines changed (guideline %d). Consider splitting it — big reviews hide bugs.", lines, rules.MaxChangedLines)
	}
	if rules.MaxChangedFiles > 0 && files > rules.MaxChangedFiles {
		warn("Touches %d files (guideline %d).", files, rules.MaxChangedFiles)
	}

	state := checks.StateFor(findings)
	summary := fmt.Sprintf("All conventions met (%d files, %d lines)", files, lines)
	if errs := count(findings, checks.SeverityError); errs > 0 {
		summary = fmt.Sprintf("%d convention problem(s)", errs)
	} else if w := count(findings, checks.SeverityWarning); w > 0 {
		summary = fmt.Sprintf("OK with %d warning(s)", w)
	}
	return checks.Result{Check: "pr-rules", State: state, Summary: summary, Findings: findings}, nil
}

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// stripComments drops template guidance (<!-- ... -->) so an untouched PR template doesn't count as a description.
func stripComments(s string) string { return htmlComment.ReplaceAllString(s, "") }

func count(fs []checks.Finding, sev checks.Severity) int {
	n := 0
	for _, f := range fs {
		if f.Severity == sev {
			n++
		}
	}
	return n
}
