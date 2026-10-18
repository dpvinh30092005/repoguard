// Package poller detects changes by asking the platform periodically instead
// of receiving webhooks. It needs no public URL, so it works on a laptop and
// behind firewalls; webhooks remain the low-latency option in production.
package poller

import (
	"context"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/platform"
)

const (
	crLimit    = 50
	issueLimit = 100
)

type Poller struct {
	Clients  *platform.Registry
	Repos    func() []platform.Repo // repositories in polling mode
	Enqueue  func(platform.Event)
	Interval time.Duration
	Log      *slog.Logger

	mu    sync.Mutex
	state map[string]*repoState
}

type repoState struct {
	crs    map[int]crSnap
	issues map[int]time.Time
}

type crSnap struct {
	sha   string
	text  uint64 // hash of title + body
	draft bool
}

// Run polls until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		p.PollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PollOnce checks every polled repository once.
func (p *Poller) PollOnce(ctx context.Context) {
	for _, repo := range p.Repos() {
		c, ok := p.Clients.Get(repo.Platform)
		if !ok {
			continue
		}
		if err := p.pollRepo(ctx, c, repo); err != nil {
			p.Log.Warn("poll failed", "repo", repo.String(), "err", err)
		}
	}
}

// CheckNow queues a full check of every open change request in repo, ignoring
// what was seen before (the "check now" button in the UI).
func (p *Poller) CheckNow(ctx context.Context, repo platform.Repo) (int, error) {
	c, ok := p.Clients.Get(repo.Platform)
	if !ok {
		return 0, nil
	}
	crs, err := c.ListOpenChangeRequests(ctx, repo, crLimit)
	if err != nil {
		return 0, err
	}
	for _, cr := range crs {
		p.Enqueue(platform.Event{Type: platform.EventCROpened, Repo: repo, Number: cr.Number, HeadSHA: cr.HeadSHA, CodeChanged: true, DeliveryID: "manual"})
	}
	return len(crs), nil
}

func (p *Poller) pollRepo(ctx context.Context, c platform.Client, repo platform.Repo) error {
	crs, err := c.ListOpenChangeRequests(ctx, repo, crLimit)
	if err != nil {
		return err
	}
	issues, err := c.ListOpenIssues(ctx, repo, issueLimit)
	if err != nil {
		return err
	}

	p.mu.Lock()
	if p.state == nil {
		p.state = map[string]*repoState{}
	}
	st, seen := p.state[repo.String()]
	if !seen {
		st = &repoState{crs: map[int]crSnap{}, issues: map[int]time.Time{}}
		p.state[repo.String()] = st
	}
	p.mu.Unlock()

	var events []platform.Event
	emit := func(t platform.EventType, number int, sha string, code bool) {
		events = append(events, platform.Event{Type: t, Repo: repo, Number: number, HeadSHA: sha, CodeChanged: code, DeliveryID: "poll"})
	}

	open := map[int]bool{}
	for _, cr := range crs {
		open[cr.Number] = true
		snap := crSnap{sha: cr.HeadSHA, text: hash(cr.Title + "\x00" + cr.Body), draft: cr.Draft}
		prev, known := st.crs[cr.Number]
		st.crs[cr.Number] = snap
		switch {
		case !seen: // first look at this repo: remember, don't flood
		case !known:
			emit(platform.EventCROpened, cr.Number, cr.HeadSHA, true)
		case prev.draft && !snap.draft:
			emit(platform.EventCROpened, cr.Number, cr.HeadSHA, true) // marked ready
		case prev.sha != snap.sha:
			emit(platform.EventCRUpdated, cr.Number, cr.HeadSHA, true)
		case prev.text != snap.text:
			emit(platform.EventCRUpdated, cr.Number, cr.HeadSHA, false)
		}
	}
	for n := range st.crs {
		if open[n] {
			continue
		}
		delete(st.crs, n)
		// Gone from the open list: merged or closed. Only merges matter.
		if cr, err := c.GetChangeRequest(ctx, repo, n); err == nil && cr.State == "merged" {
			emit(platform.EventCRMerged, n, cr.HeadSHA, false)
		}
	}

	for _, is := range issues {
		prev, known := st.issues[is.Number]
		st.issues[is.Number] = is.UpdatedAt
		switch {
		case !seen:
		case !known:
			emit(platform.EventIssueOpened, is.Number, "", false)
		case !prev.Equal(is.UpdatedAt):
			// Includes our own label/comment changes; the engine's writes are
			// idempotent, so this converges after one extra pass.
			emit(platform.EventIssueEdited, is.Number, "", false)
		}
	}

	for _, ev := range events {
		p.Enqueue(ev)
	}
	return nil
}

func hash(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
