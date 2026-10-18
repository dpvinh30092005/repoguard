package glob

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"*.lock", "yarn.lock", true},
		{"*.lock", "deep/dir/Cargo.lock", true},
		{"vendor/**", "vendor/a/b.go", true},
		{"vendor/**", "src/vendor/a.go", false},
		{"**/testdata/**", "pkg/x/testdata/key.pem", true},
		{"**/testdata/**", "testdata/key.pem", true},
		{"src/*.go", "src/a.go", true},
		{"src/*.go", "src/sub/a.go", false},
		{"docs/?.md", "docs/a.md", true},
		{"go.sum", "svc/go.sum", true},
		{"a+b.txt", "a+b.txt", true},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}
