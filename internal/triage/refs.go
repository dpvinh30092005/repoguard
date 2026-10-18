package triage

import (
	"regexp"
	"sort"
	"strconv"
)

var (
	// "#123" not preceded by a word char or "/" (so "owner/repo#1" and "abc#1" are skipped).
	issueRef = regexp.MustCompile(`(?:^|[^\w/])#(\d+)\b`)
	// Full issue URLs on GitHub (/issues/N) and GitLab (/-/issues/N).
	issueURL   = regexp.MustCompile(`/(?:-/)?issues/(\d+)\b`)
	closingRef = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+(?:[^\s#]*?/(?:-/)?issues/|#)(\d+)\b`)
	// Branch conventions: "feature/123-login", "123-login" (GitLab default), "fix/GH-123".
	branchRef = regexp.MustCompile(`(?:^|/)(?:[A-Za-z]+-)?(\d+)[-_]`)
)

// Refs is the set of issues a change request points at.
type Refs struct {
	All     []int // every referenced issue
	Closing []int // referenced with a closing keyword ("fixes #12")
}

// ParseRefs extracts issue references from a change request's title, body and branch.
func ParseRefs(title, body, branch string) Refs {
	all := map[int]bool{}
	closing := map[int]bool{}
	text := title + "\n" + body
	for _, re := range []*regexp.Regexp{issueRef, issueURL} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			add(all, m[1])
		}
	}
	for _, m := range closingRef.FindAllStringSubmatch(text, -1) {
		add(all, m[1])
		add(closing, m[1])
	}
	if m := branchRef.FindStringSubmatch(branch); m != nil {
		add(all, m[1])
	}
	return Refs{All: keys(all), Closing: keys(closing)}
}

func add(set map[int]bool, s string) {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		set[n] = true
	}
}

func keys(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
