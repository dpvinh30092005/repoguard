package prrules

import (
	"context"
	"strings"
	"testing"

	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/platform"
)

func run(t *testing.T, cr platform.ChangeRequest, files []platform.FileChange) checks.Result {
	t.Helper()
	res, err := Check{}.Run(context.Background(), checks.Input{CR: &cr, Files: files, Config: config.Default()})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGoodPullRequestPasses(t *testing.T) {
	res := run(t, platform.ChangeRequest{
		Title: "feat(api): add login endpoint",
		Body:  "Adds POST /login with rate limiting.\n\nCloses #12",
	}, []platform.FileChange{{Path: "a.go", Additions: 10}})
	if res.State != platform.StateSuccess || len(res.Findings) != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestBadPullRequestFails(t *testing.T) {
	res := run(t, platform.ChangeRequest{
		Title: "update stuff",
		Body:  "<!-- Describe your change -->\nwip",
	}, []platform.FileChange{{Path: "a.go", Additions: 900}})
	if res.State != platform.StateFailure {
		t.Fatalf("state = %s", res.State)
	}
	var msgs []string
	for _, f := range res.Findings {
		msgs = append(msgs, string(f.Severity)+": "+f.Message)
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{"error: Title", "error: Description is too short (3 chars", "error: No linked issue", "warning: Large change: 900"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

func TestSizeOnlyIsWarning(t *testing.T) {
	res := run(t, platform.ChangeRequest{
		Title: "refactor: split module", Body: "Mechanical move, no behaviour change.", HeadRef: "refactor/88-split",
	}, []platform.FileChange{{Path: "a.go", Additions: 700}})
	if res.State != platform.StateSuccess || !strings.Contains(res.Summary, "warning") {
		t.Fatalf("got %+v", res)
	}
}
