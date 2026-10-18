package config

import "testing"

func TestParseRepoOverlaysDefaults(t *testing.T) {
	cfg, err := ParseRepo([]byte(`
pull_request:
  max_changed_lines: 1000
  require_linked_issue: false
issues:
  stale:
    days_until_stale: 10
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PullRequest.MaxChangedLines != 1000 || cfg.PullRequest.RequireLinkedIssue {
		t.Errorf("overrides not applied: %+v", cfg.PullRequest)
	}
	if !cfg.PullRequest.RequireDescription || cfg.PullRequest.TitlePattern == "" {
		t.Errorf("defaults lost: %+v", cfg.PullRequest)
	}
	if cfg.Issues.Stale.DaysUntilStale != 10 || cfg.Issues.Stale.DaysUntilClose != 14 || cfg.Issues.Stale.Label != "stale" {
		t.Errorf("nested defaults lost: %+v", cfg.Issues.Stale)
	}
}

func TestParseRepoRejectsTyposAndBadValues(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown key":   "pull_request:\n  max_lines: 3\n",
		"bad regex":     "pull_request:\n  title_pattern: '('\n",
		"bad threshold": "issues:\n  duplicates:\n    threshold: 3\n",
		"bad ci type":   "ci:\n  projects:\n    - {path: x, type: cobol}\n",
	} {
		if _, err := ParseRepo([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestEmptyFileIsDefault(t *testing.T) {
	cfg, err := ParseRepo([]byte("  \n"))
	if err != nil || !cfg.AIReview.Enabled {
		t.Fatalf("cfg=%+v err=%v", cfg.AIReview, err)
	}
}
