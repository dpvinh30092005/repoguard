package poller

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/platform"
)

type fake struct {
	platform.Client
	crs    []platform.ChangeRequest
	issues []platform.Issue
	merged map[int]bool
}

func (f *fake) Kind() platform.Kind { return platform.GitLab }
func (f *fake) ListOpenChangeRequests(context.Context, platform.Repo, int) ([]platform.ChangeRequest, error) {
	return f.crs, nil
}
func (f *fake) ListOpenIssues(context.Context, platform.Repo, int) ([]platform.Issue, error) {
	return f.issues, nil
}
func (f *fake) GetChangeRequest(_ context.Context, _ platform.Repo, n int) (*platform.ChangeRequest, error) {
	state := "closed"
	if f.merged[n] {
		state = "merged"
	}
	return &platform.ChangeRequest{Number: n, State: state}, nil
}

func TestPollDetectsChanges(t *testing.T) {
	f := &fake{merged: map[int]bool{}}
	repo := platform.Repo{Platform: platform.GitLab, FullName: "g/app"}
	var got []platform.Event
	p := &Poller{
		Clients: platform.NewRegistry(f),
		Repos:   func() []platform.Repo { return []platform.Repo{repo} },
		Enqueue: func(e platform.Event) { got = append(got, e) },
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	poll := func() []platform.Event {
		got = nil
		p.PollOnce(context.Background())
		return got
	}

	t0 := time.Now()
	f.crs = []platform.ChangeRequest{{Number: 1, HeadSHA: "a", Title: "x"}}
	f.issues = []platform.Issue{{Number: 9, UpdatedAt: t0}}
	if evs := poll(); len(evs) != 0 {
		t.Fatalf("first poll must only seed state, got %+v", evs)
	}

	f.crs = append(f.crs, platform.ChangeRequest{Number: 2, HeadSHA: "b"})
	f.issues = append(f.issues, platform.Issue{Number: 10, UpdatedAt: t0})
	assertTypes(t, poll(), platform.EventCROpened, platform.EventIssueOpened)

	f.crs[0].HeadSHA = "a2"
	f.crs[1].Title = "renamed"
	evs := poll()
	assertTypes(t, evs, platform.EventCRUpdated, platform.EventCRUpdated)
	if !evs[0].CodeChanged || evs[1].CodeChanged {
		t.Errorf("push must be a code change, rename must not: %+v", evs)
	}

	f.issues[0].UpdatedAt = t0.Add(time.Minute)
	assertTypes(t, poll(), platform.EventIssueEdited)

	f.merged[1] = true
	f.crs = f.crs[1:] // #1 merged, gone from the open list
	assertTypes(t, poll(), platform.EventCRMerged)

	if evs := poll(); len(evs) != 0 {
		t.Fatalf("nothing changed, got %+v", evs)
	}

	n, err := p.CheckNow(context.Background(), repo)
	if err != nil || n != 1 || len(got) != 1 || !got[0].CodeChanged {
		t.Fatalf("check now: n=%d err=%v events=%+v", n, err, got)
	}
}

func assertTypes(t *testing.T, evs []platform.Event, want ...platform.EventType) {
	t.Helper()
	if len(evs) != len(want) {
		t.Fatalf("got %d events %+v, want %v", len(evs), evs, want)
	}
	for i, w := range want {
		if evs[i].Type != w {
			t.Fatalf("event %d = %s, want %s (all: %+v)", i, evs[i].Type, w, evs)
		}
	}
}
