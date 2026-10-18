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

func TestExecutorConfig(t *testing.T) {
	cfg, err := LoadService()
	if err != nil {
		t.Fatalf("an unconfigured service must start (set up from the UI): %v", err)
	}
	if cfg.Executor.Disabled() || cfg.Executor.Required() || len(cfg.Executor.Repos) != 0 {
		t.Fatalf("default executor = %+v, want auto with no repos allowed", cfg.Executor)
	}

	t.Setenv("REPOGUARD_EXECUTOR", "docker")
	t.Setenv("REPOGUARD_EXECUTOR_REPOS", "team/*, org/app")
	if cfg, err = LoadService(); err != nil {
		t.Fatal(err)
	}
	if !cfg.Executor.Required() || len(cfg.Executor.Repos) != 2 || cfg.Executor.Repos[1] != "org/app" || cfg.Executor.Memory != "4g" {
		t.Fatalf("executor = %+v", cfg.Executor)
	}

	t.Setenv("REPOGUARD_EXECUTOR", "jenkins")
	if _, err := LoadService(); err == nil {
		t.Fatal("jenkins mode without credentials must fail")
	}
	t.Setenv("JENKINS_URL", "https://ci.example/")
	t.Setenv("JENKINS_USER", "bot")
	t.Setenv("JENKINS_TOKEN", "tok")
	if cfg, err = LoadService(); err != nil || !cfg.Executor.UseJenkins() || cfg.Executor.Jenkins.URL != "https://ci.example" || cfg.Executor.Jenkins.Job != "repoguard-ci" {
		t.Fatalf("jenkins config = %+v err=%v", cfg.Executor.Jenkins, err)
	}

	t.Setenv("REPOGUARD_EXECUTOR", "kubernetes")
	if _, err := LoadService(); err == nil {
		t.Fatal("unknown executor mode must fail")
	}
}
