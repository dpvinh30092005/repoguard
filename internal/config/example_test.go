package config

import (
	"os"
	"testing"
)

// The documented example must always parse: docs that lie are worse than no docs.
func TestExampleConfigParses(t *testing.T) {
	data, err := os.ReadFile("../../.repoguard.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseRepo(data); err != nil {
		t.Fatal(err)
	}
}
