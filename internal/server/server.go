// Package server exposes the webhook endpoints and operational probes.
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/example/repoguard/internal/metrics"
	"github.com/example/repoguard/internal/platform"
	"github.com/example/repoguard/internal/platform/github"
	"github.com/example/repoguard/internal/platform/gitlab"
	"github.com/example/repoguard/internal/queue"
)

const maxBody = 25 << 20 // GitHub caps payloads at 25 MB

// Handler is called for every accepted event; the engine provides it.
type Handler func(ctx context.Context, ev platform.Event) error

type Server struct {
	GitHubSecret string // empty = endpoint disabled
	GitLabSecret string
	Queue        *queue.Queue
	Handle       Handler
	Log          *slog.Logger
	Metrics      *metrics.Registry
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhooks/github", s.webhook(platform.GitHub, func(r *http.Request, b []byte) ([]platform.Event, error) {
		return github.ParseWebhook(r, b, s.GitHubSecret)
	}, s.GitHubSecret != ""))
	mux.HandleFunc("POST /webhooks/gitlab", s.webhook(platform.GitLab, func(r *http.Request, b []byte) ([]platform.Event, error) {
		return gitlab.ParseWebhook(r, b, s.GitLabSecret)
	}, s.GitLabSecret != ""))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.Queue.Saturated() {
			http.Error(w, "queue saturated", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ready\n")
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.Metrics.Write(w)
	})
	return mux
}

type parser func(r *http.Request, body []byte) ([]platform.Event, error)

func (s *Server) webhook(kind platform.Kind, parse parser, enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !enabled {
			http.Error(w, string(kind)+" integration not configured", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		events, err := parse(r, body)
		switch {
		case errors.Is(err, github.ErrBadSignature), errors.Is(err, gitlab.ErrBadToken):
			s.Metrics.Inc("repoguard_webhooks_total", "platform", string(kind), "result", "unauthorized")
			s.Log.Warn("webhook rejected: bad signature", "platform", kind, "remote", r.RemoteAddr)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		case err != nil:
			s.Metrics.Inc("repoguard_webhooks_total", "platform", string(kind), "result", "bad_request")
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(events) == 0 {
			s.Metrics.Inc("repoguard_webhooks_total", "platform", string(kind), "result", "ignored")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		for _, ev := range events {
			err := s.Queue.Enqueue(queue.Job{Key: ev.Key(), Run: func(ctx context.Context) error { return s.Handle(ctx, ev) }})
			switch {
			case errors.Is(err, queue.ErrDuplicate):
				// Redelivery of work already queued: fine.
			case err != nil:
				// Tell the platform to redeliver later rather than silently dropping.
				s.Metrics.Inc("repoguard_webhooks_total", "platform", string(kind), "result", "overloaded")
				w.Header().Set("Retry-After", "30")
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
		}
		s.Metrics.Inc("repoguard_webhooks_total", "platform", string(kind), "result", "accepted")
		w.WriteHeader(http.StatusAccepted)
	}
}

// ListenAndServe runs the HTTP server until ctx is cancelled, then shuts down gracefully.
func ListenAndServe(ctx context.Context, addr string, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
