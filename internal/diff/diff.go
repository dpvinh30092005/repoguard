// Package diff parses unified-diff hunks (as returned by the GitHub and GitLab
// APIs) into line-addressable form, so findings can be anchored to lines a
// platform will actually accept inline comments on.
package diff

import (
	"bufio"
	"regexp"
	"strconv"
	"strings"
)

type LineKind int

const (
	Context LineKind = iota
	Added
	Removed
)

type Line struct {
	Kind    LineKind
	NewLine int // 0 for removed lines
	OldLine int // 0 for added lines
	Text    string
}

// File is the parsed patch of one file.
type File struct {
	Path  string
	Lines []Line
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// Parse parses the hunks of a single file's patch. Lines before the first hunk
// header (e.g. "diff --git", "+++") are ignored.
func Parse(path, patch string) File {
	f := File{Path: path}
	var oldN, newN int
	inHunk := false
	sc := bufio.NewScanner(strings.NewReader(patch))
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for sc.Scan() {
		text := sc.Text()
		if m := hunkHeader.FindStringSubmatch(text); m != nil {
			oldN, _ = strconv.Atoi(m[1])
			newN, _ = strconv.Atoi(m[2])
			inHunk = true
			continue
		}
		if !inHunk || text == `\ No newline at end of file` {
			continue
		}
		switch {
		case strings.HasPrefix(text, "+"):
			f.Lines = append(f.Lines, Line{Kind: Added, NewLine: newN, Text: text[1:]})
			newN++
		case strings.HasPrefix(text, "-"):
			f.Lines = append(f.Lines, Line{Kind: Removed, OldLine: oldN, Text: text[1:]})
			oldN++
		default: // context line starts with " " (or is empty when trailing whitespace was stripped)
			if len(text) > 0 {
				text = text[1:]
			}
			f.Lines = append(f.Lines, Line{Kind: Context, OldLine: oldN, NewLine: newN, Text: text})
			oldN++
			newN++
		}
	}
	return f
}

// AddedLines returns only the lines introduced by the change.
func (f File) AddedLines() []Line {
	var out []Line
	for _, l := range f.Lines {
		if l.Kind == Added {
			out = append(out, l)
		}
	}
	return out
}

// Commentable reports whether line (new-file numbering) appears on the new
// side of the diff, which is where GitHub/GitLab accept inline comments.
func (f File) Commentable(line int) bool {
	for _, l := range f.Lines {
		if l.Kind != Removed && l.NewLine == line {
			return true
		}
	}
	return false
}

// Annotated renders the patch with new-file line numbers, a format that lets a
// reviewer (human or LLM) cite exact lines:
//
//   - 12 | added line
//     13 | context line
//   - | removed line
func (f File) Annotated() string {
	var b strings.Builder
	for _, l := range f.Lines {
		switch l.Kind {
		case Added:
			b.WriteString("+ " + pad(l.NewLine) + " | " + l.Text + "\n")
		case Removed:
			b.WriteString("-        | " + l.Text + "\n")
		default:
			b.WriteString("  " + pad(l.NewLine) + " | " + l.Text + "\n")
		}
	}
	return b.String()
}

func pad(n int) string {
	s := strconv.Itoa(n)
	for len(s) < 6 {
		s = " " + s
	}
	return s
}
