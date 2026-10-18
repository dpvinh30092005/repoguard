package triage

import (
	"reflect"
	"testing"
	"time"

	"github.com/example/repoguard/internal/config"
	"github.com/example/repoguard/internal/platform"
)

func TestAssessBugMissingInfo(t *testing.T) {
	cfg := config.Default().Issues
	a := Assess(platform.Issue{
		Title: "App crash when uploading avatar",
		Body:  "## Steps to reproduce\n1. open profile\n2. upload png\n\n## Expected\n<!-- what should happen -->\n\n## Actual\n",
	}, cfg)
	if !a.IsBug || !reflect.DeepEqual(a.SuggestedLabels, []string{"bug"}) {
		t.Fatalf("assessment = %+v", a)
	}
	if !reflect.DeepEqual(a.MissingSections, []string{"Expected", "Actual"}) {
		t.Fatalf("missing = %v", a.MissingSections)
	}
}

func TestAssessCompleteBugAndInlineLabels(t *testing.T) {
	cfg := config.Default().Issues
	a := Assess(platform.Issue{
		Title:  "Login fails",
		Labels: []string{"bug"},
		Body:   "**Steps to reproduce**\nclick login\n\nExpected: dashboard opens\nActual: HTTP 500",
	}, cfg)
	if !a.IsBug || len(a.MissingSections) != 0 || len(a.SuggestedLabels) != 0 {
		t.Fatalf("assessment = %+v", a)
	}
}

func TestAssessFeatureIsNotChecked(t *testing.T) {
	a := Assess(platform.Issue{Title: "Feature request: dark mode", Body: "would be nice"}, config.Default().Issues)
	if a.IsBug || !reflect.DeepEqual(a.SuggestedLabels, []string{"enhancement"}) {
		t.Fatalf("assessment = %+v", a)
	}
}

func TestFindDuplicates(t *testing.T) {
	corpus := []platform.Issue{
		{Number: 1, Title: "Avatar upload crashes on large PNG files", Body: "Uploading a 5MB png avatar crashes the profile page."},
		{Number: 2, Title: "Add dark mode", Body: "Theme support for night usage."},
		{Number: 3, Title: "Password reset email never arrives", Body: "SMTP timeout in logs."},
	}
	got := FindDuplicates(platform.Issue{Number: 9, Title: "Crash when uploading PNG avatar", Body: "profile page crashes after avatar upload"}, corpus, 0.3, 3)
	if len(got) != 1 || got[0].Issue.Number != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestStaleDecision(t *testing.T) {
	cfg := config.Default().Issues.Stale // 60 days stale, 14 days close
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	ago := func(days int) time.Time { return now.AddDate(0, 0, -days) }
	cases := []struct {
		name  string
		issue platform.Issue
		want  StaleAction
	}{
		{"fresh", platform.Issue{UpdatedAt: ago(5)}, StaleNone},
		{"idle", platform.Issue{UpdatedAt: ago(61)}, StaleMark},
		{"exempt", platform.Issue{UpdatedAt: ago(400), Labels: []string{"security"}}, StaleNone},
		{"stale, warning ignored", platform.Issue{UpdatedAt: ago(15), Labels: []string{"stale"}}, StaleClose},
		{"stale, recently marked", platform.Issue{UpdatedAt: ago(3), Labels: []string{"stale"}}, StaleNone},
	}
	for _, c := range cases {
		if got := StaleDecision(c.issue, cfg, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSLABreached(t *testing.T) {
	cfg := config.Default().Issues.SLA
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	if !SLABreached(platform.Issue{Labels: []string{"P0"}, CreatedAt: old}, cfg, now) {
		t.Error("unassigned P0 after 48h should breach")
	}
	if SLABreached(platform.Issue{Labels: []string{"P0"}, CreatedAt: old, Assignees: []string{"ann"}}, cfg, now) {
		t.Error("assigned issue should not breach")
	}
	if SLABreached(platform.Issue{Labels: []string{"P0"}, CreatedAt: now.Add(-time.Hour)}, cfg, now) {
		t.Error("too early to breach")
	}
}
