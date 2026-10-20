// Package prrules enforces team conventions on change requests: title format,
// description, linked issue and size.
package prrules

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/triage"
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
			if rules.TitlePattern == config.Default().PullRequest.TitlePattern {
				fail("The title does not follow the convention `type(scope): summary`, for example `feat(cart): add an item to the cart`.")
			} else {
				fail("The title does not match this project's title rule `%s`.", rules.TitlePattern)
			}
		}
	}

	if rules.RequireDescription {
		if n := len(strings.TrimSpace(stripComments(cr.Body))); n < rules.MinDescriptionLength {
			fail("The description is too short (%d characters, at least %d). Say what changed and why.", n, rules.MinDescriptionLength)
		}
	}

	if rules.RequireLinkedIssue {
		if refs := triage.ParseRefs(cr.Title, cr.Body, cr.HeadRef); len(refs.All) == 0 {
			fail("No linked issue. Add `Closes #123` to the description, or start the branch name with the issue number, for example `feature/123-login`.")
		}
	}

	lines, files := 0, len(in.Files)
	for _, f := range in.Files {
		lines += f.Additions + f.Deletions
	}
	if rules.MaxChangedLines > 0 && lines > rules.MaxChangedLines {
		warn("Large change: %d lines changed (guideline %d). Smaller merge requests get better reviews.", lines, rules.MaxChangedLines)
	}
	if rules.MaxChangedFiles > 0 && files > rules.MaxChangedFiles {
		warn("Touches %d files (guideline %d).", files, rules.MaxChangedFiles)
	}

	state := checks.StateFor(findings)
	summary := fmt.Sprintf("All rules met (%s, %d lines changed)", plural(files, "file"), lines)
	if errs := count(findings, checks.SeverityError); errs > 0 {
		summary = plural(errs, "problem")
	} else if w := count(findings, checks.SeverityWarning); w > 0 {
		summary = "Passed with " + plural(w, "warning")
	}
	return checks.Result{Check: "pr-rules", State: state, Summary: summary, Findings: findings}, nil
}

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// stripComments drops template guidance (<!-- ... -->) so an untouched PR template doesn't count as a description.
func stripComments(s string) string { return htmlComment.ReplaceAllString(s, "") }

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func count(fs []checks.Finding, sev checks.Severity) int {
	n := 0
	for _, f := range fs {
		if f.Severity == sev {
			n++
		}
	}
	return n
}
