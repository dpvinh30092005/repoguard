package triage

import (
	"reflect"
	"testing"
)

func TestParseRefs(t *testing.T) {
	cases := []struct {
		name, title, body, branch string
		all, closing              []int
	}{
		{"plain ref", "feat: login", "Related to #12", "main", []int{12}, []int{}},
		{"closing keywords", "fix: crash (#7)", "Fixes #3\nresolves: #4\ncloses https://gitlab.com/g/p/-/issues/5", "x", []int{3, 4, 5, 7}, []int{3, 4, 5}},
		{"branch convention", "chore: x", "", "feature/42-new-ui", []int{42}, []int{}},
		{"gitlab default branch", "chore: x", "", "17-fix-typo", []int{17}, []int{}},
		{"ignores other repos and anchors", "x", "see other/repo#9 and abc#8 and `color: #fff`", "main", []int{}, []int{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseRefs(c.title, c.body, c.branch)
			if !reflect.DeepEqual(got.All, c.all) || !reflect.DeepEqual(got.Closing, c.closing) {
				t.Fatalf("got %+v, want all=%v closing=%v", got, c.all, c.closing)
			}
		})
	}
}
