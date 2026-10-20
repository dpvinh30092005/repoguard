// Command repoguard checks code and issues on GitHub and GitLab.
//
//	repoguard serve   run the service: web UI, webhooks, polling, executor
//	repoguard ci      run build/test/secret/vulnerability checks inside a CI job
//	repoguard sweep   apply stale/SLA policies once (for cron jobs)
//	repoguard version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/activity"
	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/checks/aireview"
	"github.com/dpvinh30092005/repoguard/internal/checks/prrules"
	"github.com/dpvinh30092005/repoguard/internal/checks/secrets"
	"github.com/dpvinh30092005/repoguard/internal/ci"
	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/engine"
	"github.com/dpvinh30092005/repoguard/internal/executor"
	"github.com/dpvinh30092005/repoguard/internal/metrics"
	"github.com/dpvinh30092005/repoguard/internal/platform"
	"github.com/dpvinh30092005/repoguard/internal/poller"
	"github.com/dpvinh30092005/repoguard/internal/queue"
	"github.com/dpvinh30092005/repoguard/internal/results"
	"github.com/dpvinh30092005/repoguard/internal/server"
	"github.com/dpvinh30092005/repoguard/internal/store"
	"github.com/dpvinh30092005/repoguard/internal/web"
)

var version = "dev" // set with -ldflags "-X main.version=..."

const defaultExecutorImage = "ghcr.io/dpvinh30092005/repoguard-ci:1"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx)
	case "ci":
		var failed bool
		failed, err = runCI(ctx, os.Args[2:])
		if err == nil && failed {
			os.Exit(1)
		}
	case "sweep":
		err = sweep(ctx)
	case "version", "--version", "-v":
		fmt.Println("repoguard", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "repoguard:", err)
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: repoguard <serve|ci|sweep|version> [flags]")
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// setup builds the long-lived parts of the service from env + stored settings.
func setup(log *slog.Logger, m *metrics.Registry, act *activity.Log) (*app, *engine.Engine, error) {
	cfg, err := config.LoadService()
	if err != nil {
		return nil, nil, err
	}
	dataDir := firstNonEmpty(os.Getenv("REPOGUARD_DATA_DIR"), "data")
	st, err := store.Open(filepath.Join(dataDir, "settings.json"))
	if err != nil {
		return nil, nil, err
	}
	a := newApp(cfg, st, log)
	if cfg.Executor.Required() && !a.dockerOK {
		return nil, nil, fmt.Errorf("REPOGUARD_EXECUTOR=docker: %s", a.dockerWhy)
	}
	cks := []checks.Check{prrules.Check{}, secrets.Check{}, aireview.Check{Reviewer: a.reviewer}}
	eng := engine.New(nil, cks, log, m)
	eng.Clients, eng.Activity, eng.PublicURL = a.reg, act, a.publicURL
	res, err := results.Open(filepath.Join(dataDir, "results.json"))
	if err != nil {
		log.Warn("results history unreadable, starting empty", "err", err)
		res, _ = results.Open("")
	}
	a.results, eng.Results = res, res
	return a, eng, nil
}

func serve(ctx context.Context) error {
	log := newLogger()
	m := metrics.New()
	act := activity.New(200)
	a, eng, err := setup(log, m, act)
	if err != nil {
		return err
	}
	cfg := a.cfg
	q := queue.New(queue.Options{Workers: cfg.Workers, Size: cfg.QueueSize, MaxRetries: 4, Logger: log, Metrics: m})
	q.Start(ctx)

	var xq *queue.Queue
	if !cfg.Executor.Disabled() {
		xq = queue.New(queue.Options{Workers: cfg.Executor.Concurrency, Size: 200, MaxRetries: 1, JobTimeout: cfg.Executor.Timeout + 10*time.Minute, Logger: log, Metrics: m})
		xq.Start(ctx)
		a.exec = &executor.Executor{Clients: a.reg, Queue: xq, Allow: a.allow, Log: log, Metrics: m, Activity: act, Results: a.results}
		eng.Executor = a.exec
	}

	enqueue := func(ev platform.Event) {
		if err := q.Enqueue(queue.Job{Key: ev.Key(), Run: func(ctx context.Context) error { return eng.Handle(ctx, ev) }}); err != nil && !errors.Is(err, queue.ErrDuplicate) {
			log.Warn("enqueue failed", "event", ev.Type, "repo", ev.Repo.String(), "err", err)
		}
	}
	interval, err := time.ParseDuration(firstNonEmpty(os.Getenv("REPOGUARD_POLL_INTERVAL"), "30s"))
	if err != nil {
		return fmt.Errorf("REPOGUARD_POLL_INTERVAL: %w", err)
	}
	p := &poller.Poller{
		Clients: a.reg, Repos: func() []platform.Repo { return a.watchedRepos("poll") }, Enqueue: enqueue, Interval: interval, Log: log,
		Gone: a.results.SetState,
	}

	ui := &web.UI{
		Store: a.store, Clients: a.reg, Activity: act, Results: a.results, Poller: p, Log: log, Version: version,
		Password: a.password, Apply: a.apply, Secret: a.secret, PublicURL: a.publicURL,
		QueueDepth: q.Depth, AIEnabled: a.reviewer.Available, Locked: a.locked(), Executor: a.executorInfo,
	}
	if err := ui.Init(ctx); err != nil { // also applies the stored settings once
		return err
	}
	go p.Run(ctx)
	if cfg.SweepInterval > 0 {
		go runSweeper(ctx, cfg.SweepInterval, a, eng, q, log)
	}

	srv := &server.Server{Queue: q, Handle: eng.Handle, Log: log, Metrics: m, Secret: a.secret, UI: ui.Routes()}
	if os.Getenv("REPOGUARD_ADMIN_PASSWORD") == "" {
		log.Info("web UI admin password (set REPOGUARD_ADMIN_PASSWORD to choose your own)", "admin password", a.password())
	}
	log.Info("repoguard starting", "version", version, "addr", cfg.Addr, "workers", cfg.Workers)
	err = server.ListenAndServe(ctx, cfg.Addr, srv.Routes(), log)

	drain, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if qerr := q.Close(drain); qerr != nil {
		log.Warn("queue not fully drained", "err", qerr)
	}
	if xq != nil {
		if qerr := xq.Close(drain); qerr != nil {
			log.Warn("executor queue not fully drained", "err", qerr)
		}
	}
	log.Info("repoguard stopped")
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// runSweeper enqueues one sweep job per repository every interval.
func runSweeper(ctx context.Context, every time.Duration, a *app, eng *engine.Engine, q *queue.Queue, log *slog.Logger) {
	tick := func() {
		repos, err := a.sweepTargets(ctx, eng)
		if err != nil {
			log.Error("sweep: listing repos", "err", err)
		}
		for _, repo := range repos {
			_ = q.Enqueue(queue.Job{Key: "sweep|" + repo.String(), Run: func(ctx context.Context) error { return eng.SweepRepo(ctx, repo) }})
		}
		log.Info("sweep scheduled", "repos", len(repos))
	}
	t := time.NewTicker(every)
	defer t.Stop()
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

func sweep(ctx context.Context) error {
	log := newLogger()
	a, eng, err := setup(log, metrics.New(), nil)
	if err != nil {
		return err
	}
	a.apply(ctx)
	repos, err := a.sweepTargets(ctx, eng)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	for _, repo := range repos {
		if err := eng.SweepRepo(ctx, repo); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
		}
	}
	return errors.Join(errs...)
}

func runCI(ctx context.Context, args []string) (failed bool, err error) {
	fs := flag.NewFlagSet("ci", flag.ContinueOnError)
	dir := fs.String("dir", ".", "repository root")
	base := fs.String("base", "", "only check projects changed since this git ref (e.g. origin/main)")
	stages := fs.String("stages", strings.Join(ci.Stages, ","), "stages to run")
	noVuln := fs.Bool("no-vuln", false, "skip the dependency vulnerability scan")
	mdPath := fs.String("report", "repoguard-report.md", "markdown report path ('' to disable)")
	junitPath := fs.String("junit", "repoguard-junit.xml", "JUnit XML report path ('' to disable)")
	jsonPath := fs.String("json", "", "machine-readable JSON report path (used by the executor)")
	if err := fs.Parse(args); err != nil {
		return false, err
	}

	report, err := ci.Run(ctx, ci.Options{
		Dir: *dir, Base: *base, Stages: strings.Split(*stages, ","), NoVuln: *noVuln, Out: os.Stdout,
	})
	if err != nil {
		return false, err
	}

	var md strings.Builder
	report.WriteMarkdown(&md)
	fmt.Println("\n" + md.String())
	if *mdPath != "" {
		if err := os.WriteFile(*mdPath, []byte(md.String()), 0o644); err != nil {
			return false, err
		}
	}
	if summary := os.Getenv("GITHUB_STEP_SUMMARY"); summary != "" {
		if f, err := os.OpenFile(summary, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			_, _ = f.WriteString(md.String())
			_ = f.Close()
		}
	}
	if *jsonPath != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return false, err
		}
		if err := os.WriteFile(*jsonPath, data, 0o644); err != nil {
			return false, err
		}
	}
	if *junitPath != "" {
		f, err := os.Create(*junitPath)
		if err != nil {
			return false, err
		}
		defer f.Close()
		if err := report.WriteJUnit(f); err != nil {
			return false, err
		}
	}
	return report.Failed(), nil
}
