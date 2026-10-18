// Package triage holds the issue logic: classification, completeness,
// duplicate detection, staleness and SLA. Functions here are pure; the engine
// turns their decisions into API calls.
package triage

import (
	"regexp"
	"sort"
	"strings"

	"github.com/example/repoguard/internal/config"
	"github.com/example/repoguard/internal/platform"
)

// Assessment is the outcome of looking at one issue.
type Assessment struct {
	IsBug           bool
	SuggestedLabels []string // auto labels not yet on the issue
	MissingSections []string // only for bugs
}

// Assess classifies an issue and checks it carries the information triage needs.
func Assess(issue platform.Issue, cfg config.Issues) Assessment {
	text := strings.ToLower(issue.Title + "\n" + issue.Body)
	var a Assessment

	for label, keywords := range cfg.AutoLabels {
		if issue.HasLabel(label) {
			continue
		}
		for _, kw := range keywords {
			if containsWord(text, strings.ToLower(kw)) {
				a.SuggestedLabels = append(a.SuggestedLabels, label)
				break
			}
		}
	}
	sort.Strings(a.SuggestedLabels)

	for _, l := range cfg.BugLabels {
		if issue.HasLabel(l) || contains(a.SuggestedLabels, l) {
			a.IsBug = true
		}
	}
	if a.IsBug {
		a.MissingSections = MissingSections(issue.Body, cfg.RequiredSections)
	}
	return a
}

// MissingSections returns required headings that are absent or left empty.
// A section is a markdown heading ("## Steps to reproduce") or a bold/colon
// label ("**Expected:**", "Actual:") followed by some real content.
func MissingSections(body string, required []string) []string {
	var missing []string
	for _, name := range required {
		if !hasFilledSection(body, name) {
			missing = append(missing, name)
		}
	}
	return missing
}

func hasFilledSection(body, name string) bool {
	re := regexp.MustCompile(`(?im)^\s*(?:#{1,6}\s*|\*\*)?` + regexp.QuoteMeta(name) + `[^\n]*$`)
	loc := re.FindStringIndex(body)
	if loc == nil {
		return false
	}
	// Content = text after the heading up to the next heading.
	rest := body[loc[1]:]
	if next := regexp.MustCompile(`(?m)^\s*(#{1,6}\s|\*\*[^*]+\*\*\s*:?\s*$)`).FindStringIndex(rest); next != nil {
		rest = rest[:next[0]]
	}
	rest = htmlComment.ReplaceAllString(rest, "")
	// Also accept "Expected: it works" on the heading line itself.
	line := body[loc[0]:loc[1]]
	if i := strings.Index(line, ":"); i >= 0 && len(strings.Trim(line[i+1:], " *_")) > 2 {
		return true
	}
	return len(strings.TrimSpace(rest)) > 2
}

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

func containsWord(text, kw string) bool {
	re := regexp.MustCompile(`(?:^|\W)` + regexp.QuoteMeta(kw) + `(?:\W|$)`)
	return re.MatchString(text)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
