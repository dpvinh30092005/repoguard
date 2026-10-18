// Package queue runs jobs on a bounded worker pool with de-duplication and
// retries. Webhook handlers enqueue and return immediately (GitHub gives a
// delivery 10s), the work happens here.
//
// It is in-memory on purpose: a lost job on restart is recovered by the next
// event or the periodic sweep. For multi-replica HA, put the same interface in
// front of Redis/NATS/SQS.
package queue

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/metrics"
	"github.com/dpvinh30092005/repoguard/internal/platform"
)

type Job struct {
	Key string // jobs with the same key are collapsed while queued or running
	Run func(ctx context.Context) error
}

var (
	ErrDuplicate = errors.New("duplicate job")
	ErrFull      = errors.New("queue full")
	ErrClosed    = errors.New("queue closed")
)

type Options struct {
	Workers    int
	Size       int
	MaxRetries int
	Backoff    time.Duration // first retry delay, doubled each attempt
	JobTimeout time.Duration
	Logger     *slog.Logger
	Metrics    *metrics.Registry
}

type Queue struct {
	opts    Options
	jobs    chan Job
	mu      sync.Mutex
	pending map[string]bool
	closed  bool
	wg      sync.WaitGroup
}

func New(opts Options) *Queue {
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.Size <= 0 {
		opts.Size = 100
	}
	if opts.Backoff <= 0 {
		opts.Backoff = 2 * time.Second
	}
	if opts.JobTimeout <= 0 {
		opts.JobTimeout = 10 * time.Minute
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = metrics.New()
	}
	q := &Queue{opts: opts, jobs: make(chan Job, opts.Size), pending: map[string]bool{}}
	opts.Metrics.Gauge("repoguard_queue_depth", func() int64 { return int64(len(q.jobs)) })
	return q
}

// Start launches the workers; they stop when ctx is cancelled or Close drains the queue.
func (q *Queue) Start(ctx context.Context) {
	for i := 0; i < q.opts.Workers; i++ {
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			for job := range q.jobs {
				q.run(ctx, job)
			}
		}()
	}
}

func (q *Queue) Enqueue(job Job) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrClosed
	}
	if q.pending[job.Key] {
		q.opts.Metrics.Inc("repoguard_jobs_total", "result", "duplicate")
		return ErrDuplicate
	}
	select {
	case q.jobs <- job:
		q.pending[job.Key] = true
		return nil
	default:
		q.opts.Metrics.Inc("repoguard_jobs_total", "result", "dropped")
		return ErrFull
	}
}

// Depth is the number of jobs waiting to run.
func (q *Queue) Depth() int { return len(q.jobs) }

// Saturated reports whether the queue is over 90% full (used by /readyz).
func (q *Queue) Saturated() bool { return len(q.jobs) > cap(q.jobs)*9/10 }

// Close stops accepting jobs and waits for queued ones to finish or ctx to expire.
func (q *Queue) Close(ctx context.Context) error {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		close(q.jobs)
	}
	q.mu.Unlock()
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *Queue) run(ctx context.Context, job Job) {
	defer func() {
		q.mu.Lock()
		delete(q.pending, job.Key)
		q.mu.Unlock()
	}()
	log := q.opts.Logger.With("job", job.Key)
	delay := q.opts.Backoff
	for attempt := 0; ; attempt++ {
		err := q.attempt(ctx, job)
		if err == nil {
			q.opts.Metrics.Inc("repoguard_jobs_total", "result", "ok")
			return
		}
		if attempt >= q.opts.MaxRetries || !platform.IsRetryable(err) {
			q.opts.Metrics.Inc("repoguard_jobs_total", "result", "failed")
			log.Error("job failed", "attempt", attempt+1, "err", err)
			return
		}
		log.Warn("job failed, retrying", "attempt", attempt+1, "in", delay, "err", err)
		q.opts.Metrics.Inc("repoguard_jobs_total", "result", "retried")
		select {
		case <-time.After(delay):
			delay *= 2
		case <-ctx.Done():
			return
		}
	}
}

func (q *Queue) attempt(ctx context.Context, job Job) (err error) {
	ctx, cancel := context.WithTimeout(ctx, q.opts.JobTimeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil { // one bad payload must not kill a worker
			err = errors.New("panic in job")
			q.opts.Logger.Error("job panicked", "job", job.Key, "panic", r)
		}
	}()
	return job.Run(ctx)
}
