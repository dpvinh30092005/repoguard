// Package activity keeps the most recent things repoguard did, for the web UI.
package activity

import (
	"sync"
	"time"
)

type Entry struct {
	Time     time.Time
	Platform string
	Repo     string
	Kind     string // MR | Issue | CI | Sweep
	Number   int
	Event    string
	Result   string // ok | error | passed | failed | skipped
	Detail   string
	URL      string
}

// Log is a fixed-size ring buffer.
type Log struct {
	mu      sync.Mutex
	entries []Entry
	next    int
	full    bool
}

func New(size int) *Log { return &Log{entries: make([]Entry, size)} }

func (l *Log) Add(e Entry) {
	if l == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[l.next] = e
	l.next = (l.next + 1) % len(l.entries)
	if l.next == 0 {
		l.full = true
	}
}

// List returns entries newest first.
func (l *Log) List() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.next
	if l.full {
		n = len(l.entries)
	}
	out := make([]Entry, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, l.entries[(l.next-i+len(l.entries))%len(l.entries)])
	}
	return out
}
