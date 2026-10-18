package queue

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/metrics"
)

type retryable struct{}

func (retryable) Error() string   { return "flaky" }
func (retryable) Retryable() bool { return true }

func TestRetriesTransientErrorsThenSucceeds(t *testing.T) {
	m := metrics.New()
	q := New(Options{Workers: 1, MaxRetries: 3, Backoff: time.Millisecond, Metrics: m})
	q.Start(context.Background())
	var calls atomic.Int32
	_ = q.Enqueue(Job{Key: "a", Run: func(context.Context) error {
		if calls.Add(1) < 3 {
			return retryable{}
		}
		return nil
	}})
	_ = q.Close(context.Background())
	if calls.Load() != 3 || m.Get("repoguard_jobs_total", "result", "ok") != 1 {
		t.Fatalf("calls=%d ok=%d", calls.Load(), m.Get("repoguard_jobs_total", "result", "ok"))
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	q := New(Options{Workers: 1, MaxRetries: 5, Backoff: time.Millisecond})
	q.Start(context.Background())
	var calls atomic.Int32
	_ = q.Enqueue(Job{Key: "a", Run: func(context.Context) error { calls.Add(1); return errors.New("400 bad request") }})
	_ = q.Close(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestDeduplicatesPendingJobs(t *testing.T) {
	q := New(Options{Workers: 1})
	release := make(chan struct{})
	q.Start(context.Background())
	if err := q.Enqueue(Job{Key: "same", Run: func(context.Context) error { <-release; return nil }}); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(Job{Key: "same", Run: func(context.Context) error { return nil }}); err != ErrDuplicate {
		t.Fatalf("err = %v", err)
	}
	close(release)
	_ = q.Close(context.Background())
	if err := q.Enqueue(Job{Key: "late"}); err != ErrClosed {
		t.Fatalf("after close: %v", err)
	}
}

func TestPanicDoesNotKillWorker(t *testing.T) {
	q := New(Options{Workers: 1})
	q.Start(context.Background())
	var ran atomic.Bool
	_ = q.Enqueue(Job{Key: "boom", Run: func(context.Context) error { panic("bad payload") }})
	_ = q.Enqueue(Job{Key: "next", Run: func(context.Context) error { ran.Store(true); return nil }})
	_ = q.Close(context.Background())
	if !ran.Load() {
		t.Fatal("worker died after panic")
	}
}

func TestFullQueueRejects(t *testing.T) {
	q := New(Options{Workers: 1, Size: 1}) // not started: nothing drains
	_ = q.Enqueue(Job{Key: "1"})
	if err := q.Enqueue(Job{Key: "2"}); err != ErrFull {
		t.Fatalf("err = %v", err)
	}
}
