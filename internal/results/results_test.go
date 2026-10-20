package results

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/ci"
	"github.com/dpvinh30092005/repoguard/internal/platform"
)

var repo = platform.Repo{Platform: platform.GitLab, FullName: "team/api"}

func cr(sha string) *platform.ChangeRequest {
	return &platform.ChangeRequest{Number: 7, Title: "update stuff", Author: "tu", HeadSHA: sha, State: "open", URL: "https://gl/mr/7"}
}

func TestRecordChecksAndStatus(t *testing.T) {
	s, _ := Open("")
	s.RecordChecks(repo, cr("aaa"), "", []checks.Result{
		{Check: "pr-rules", State: platform.StateFailure, Summary: "2 convention problem(s)", Findings: []checks.Finding{
			{Severity: checks.SeverityError, Message: "Title does not match"}, {Severity: checks.SeverityWarning, Message: "Large change"},
		}},
		{Check: "secrets", State: platform.StateSuccess, Summary: "No secrets"},
	}, []string{"ai-review"}, "")

	list := s.List()
	if len(list) != 1 {
		t.Fatalf("list = %d", len(list))
	}
	r := list[0]
	if r.Status() != Failing || r.Problems() != 1 || len(r.Failures()) != 1 || r.Failures()[0] != "Conventions: 2 convention problem(s)" {
		t.Fatalf("status=%s problems=%d failures=%v", r.Status(), r.Problems(), r.Failures())
	}
	if got, _ := s.Get(r.ID); got.Title != "update stuff" || r.ID != IDFor("gitlab", "team/api", 7) {
		t.Fatalf("get by id: %+v", got)
	}
	if c, _ := r.Check("ai-review"); c.State != StateSkipped {
		t.Fatalf("skipped check not recorded: %+v", c)
	}
	if r.Checks[0].Name != "pr-rules" || r.Checks[2].Name != "ai-review" {
		t.Fatalf("checks not in display order: %+v", r.Checks)
	}
}

func TestSkippedCheckKeepsResultForSameCommit(t *testing.T) {
	s, _ := Open("")
	ai := checks.Result{Check: "ai-review", State: platform.StateSuccess, Summary: "1 suggestion"}
	s.RecordChecks(repo, cr("aaa"), "", []checks.Result{ai}, nil, "")
	s.RecordChecks(repo, cr("aaa"), "", nil, []string{"ai-review"}, "") // title edit
	if c, _ := s.List()[0].Check("ai-review"); c.State != "success" {
		t.Fatalf("a title edit must not erase the review of the same commit: %+v", c)
	}
	s.RecordChecks(repo, cr("bbb"), "", nil, []string{"ai-review"}, "") // new commit, review not run yet
	if c, _ := s.List()[0].Check("ai-review"); c.State != StateSkipped {
		t.Fatalf("a new commit must not show the old review: %+v", c)
	}
}

func TestBuildAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	report := &ci.Report{Projects: []ci.ProjectResult{{Project: ci.Project{Path: "svc"}, Stages: []ci.StageResult{
		{Stage: "test", Status: "failed", Output: string(make([]byte, 10000))},
	}}}}
	s.RecordBuild(repo, cr("aaa"), "", Build{State: "failure", Summary: "1 project failed", Report: report})
	s.SetState(repo, 7, "merged")

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	r := again.List()[0]
	if r.State != "merged" || r.Build == nil || r.Build.State != "failure" || r.Status() != Failing {
		t.Fatalf("reloaded: %+v", r)
	}
	if out := r.Build.Report.Projects[0].Stages[0].Output; len(out) > maxOutput+10 {
		t.Fatalf("stage output not trimmed: %d bytes", len(out))
	}
	if len(report.Projects[0].Stages[0].Output) != 10000 {
		t.Fatal("trimming must not modify the caller's report")
	}
	s.RecordChecks(repo, cr("bbb"), "", []checks.Result{{Check: "secrets", State: platform.StateSuccess}}, nil, "")
	if r := s.List()[0]; !r.BuildOutdated() || r.Status() != Passing {
		t.Fatalf("a build of an older commit is outdated and no longer fails the review: %s", r.Status())
	}
}

func TestPruneDropsClosedFirst(t *testing.T) {
	s, _ := Open("")
	s.max = 2
	clock := time.Unix(0, 0)
	s.now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	for n := 1; n <= 3; n++ {
		c := cr("x")
		c.Number = n
		s.RecordChecks(repo, c, "", nil, nil, "")
		if n == 2 {
			s.SetState(repo, 2, "closed")
		}
	}
	for _, r := range s.List() {
		if r.Number == 2 {
			t.Fatal("the closed review should be pruned before open ones")
		}
	}
}
