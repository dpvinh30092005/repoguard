// Package glob matches slash-separated paths against patterns supporting
// "*" (within one segment), "**" (any number of segments) and "?".
package glob

import (
	"regexp"
	"strings"
	"sync"
)

var cache sync.Map // pattern -> *regexp.Regexp

// Match reports whether path matches pattern. A pattern without "/" matches
// the base name anywhere in the tree ("*.lock" matches "a/b/c.lock").
func Match(pattern, path string) bool {
	if !strings.Contains(pattern, "/") {
		pattern = "**/" + pattern
	}
	re, ok := cache.Load(pattern)
	if !ok {
		re, _ = cache.LoadOrStore(pattern, compile(pattern))
	}
	return re.(*regexp.Regexp).MatchString(path)
}

// Any reports whether path matches at least one pattern.
func Any(patterns []string, path string) bool {
	for _, p := range patterns {
		if Match(p, path) {
			return true
		}
	}
	return false
}

func compile(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch {
		case c == '*' && i+1 < len(pattern) && pattern[i+1] == '*':
			i++
			if i+1 < len(pattern) && pattern[i+1] == '/' {
				i++
				b.WriteString("(?:.*/)?") // "**/" = zero or more directories
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
