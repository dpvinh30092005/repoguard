// Command repoguard checks code and issues on GitHub and GitLab.
//
//	repoguard serve   run the webhook service (GitHub App / GitLab webhooks)
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
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

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
	"github.com/dpvinh30092005/repoguard/internal/platform/github"
	"github.com/dpvinh30092005/repoguard/internal/platform/gitlab"
	"github.com/dpvinh30092005/repoguard/internal/queue"
	"github.com/dpvinh30092005/repoguard/internal/server"
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

// build wires platform clients, checks and the engine from the environment.
func build(log *slog.Logger, m *metrics.Registry) (config.Service, *engine.Engine, error) {
	cfg, err := config.LoadService()
	if err != nil {
		return cfg, nil, err
	}
	hc := platform.NewHTTPClient()
	var clients []platform.Client
	if cfg.GitHub.Enabled() {
		var app *github.AppAuth
		if cfg.GitHub.AppID != 0 {
			if app, err = github.NewAppAuth(cfg.GitHub.AppID, cfg.GitHub.PrivateKeyPEM, cfg.GitHub.APIURL, hc); err != nil {
				return cfg, nil, err
			}
		}
		clients = append(clients, github.New(cfg.GitHub.APIURL, app, cfg.GitHub.Token, hc))
		log.Info("github enabled", "api", cfg.GitHub.APIURL, "app", cfg.GitHub.AppID != 0)
	}
	if cfg.GitLab.Enabled() {
		clients = append(clients, gitlab.New(cfg.GitLab.URL, cfg.GitLab.Token, hc))
		log.Info("gitlab enabled", "url", cfg.GitLab.URL)
	}

	cks := []checks.Check{prrules.Check{}, secrets.Check{}}
	if cfg.AI.Enabled() {
		cks = append(cks, aireview.Check{Reviewer: aireview.NewClaude(cfg.AI.APIKey, cfg.AI.Model, cfg.AI.Effort)})
		log.Info("ai review enabled", "model", cfg.AI.Model, "effort", cfg.AI.Effort)
	} else {
		log.Info("ai review disabled: ANTHROPIC_API_KEY not set")
	}

	eng := engine.New(clients, cks, log, m)
	eng.PublicURL = cfg.PublicURL
	return cfg, eng, nil
}

func serve(ctx context.Context) error {
	log := newLogger()
	m := metrics.New()
	cfg, eng, err := build(log, m)
	if err != nil {
		return err
	}
	q := queue.New(queue.Options{Workers: cfg.Workers, Size: cfg.QueueSize, MaxRetries: 4, Logger: log, Metrics: m})
	q.Start(ctx)

	x, xq, err := newExecutor(cfg.Executor, eng, log, m)
	if err != nil {
		return err
	}
	if x != nil {
		xq.Start(ctx)
		eng.Executor = x
	}

	if cfg.SweepInterval > 0 {
		go runSweeper(ctx, cfg.SweepInterval, eng, q, log)
	}

	srv := &server.Server{Queue: q, Handle: eng.Handle, Log: log, Metrics: m, Secret: func(k platform.Kind) string {
		switch {
		case k == platform.GitHub && cfg.GitHub.Enabled():
			return cfg.GitHub.WebhookSecret
		case k == platform.GitLab && cfg.GitLab.Enabled():
			return cfg.GitLab.WebhookSecret
		}
		return ""
	}}
	log.Info("repoguard starting", "version", version, "workers", cfg.Workers)
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

// newExecutor builds the CI executor: Jenkins when configured, else a local
// Docker sandbox if Docker is available. It returns nil when there is neither.
func newExecutor(cfg config.Executor, eng *engine.Engine, log *slog.Logger, m *metrics.Registry) (*executor.Executor, *queue.Queue, error) {
	if cfg.Disabled() {
		return nil, nil, nil
	}
	x := &executor.Executor{
		Clients: eng.Clients,
		Allow:   executor.AllowGlobs(cfg.Repos), // nothing is built unless allowed
		Log:     log,
		Metrics: m,
	}
	if cfg.UseJenkins() {
		j := cfg.Jenkins
		x.Remote = &executor.Jenkins{URL: j.URL, User: j.User, Token: j.Token, Job: j.Job, Timeout: cfg.Timeout}
		log.Info("executor enabled (jenkins)", "url", j.URL, "job", j.Job, "repos", cfg.Repos)
	} else {
		for _, bin := range []string{"docker", "git"} {
			if _, err := exec.LookPath(bin); err != nil {
				if cfg.Required() {
					return nil, nil, fmt.Errorf("REPOGUARD_EXECUTOR=docker needs %q in PATH (use deploy/executor.Dockerfile)", bin)
				}
				log.Info("executor unavailable", "missing", bin)
				return nil, nil, nil
			}
		}
		if cfg.Image == "" {
			cfg.Image = defaultExecutorImage
		}
		x.Sandbox = executor.Docker{
			Image: cfg.Image, CPUs: cfg.CPUs, Memory: cfg.Memory, PidsLimit: cfg.PidsLimit,
			Network: cfg.Network, Runtime: cfg.Runtime, Timeout: cfg.Timeout,
		}
		if s := cfg.Sonar; s.HostURL != "" && s.Token != "" {
			x.Env = map[string]string{"SONAR_HOST_URL": s.HostURL, "SONAR_TOKEN": s.Token}
		}
		log.Info("executor enabled (docker)", "repos", cfg.Repos, "image", cfg.Image, "concurrency", cfg.Concurrency, "sonar", x.Env != nil)
	}
	x.Queue = queue.New(queue.Options{Workers: cfg.Concurrency, Size: 200, MaxRetries: 1, JobTimeout: cfg.Timeout + 10*time.Minute, Logger: log, Metrics: m})
	return x, x.Queue, nil
}

// runSweeper enqueues one sweep job per repository every interval.
func runSweeper(ctx context.Context, every time.Duration, eng *engine.Engine, q *queue.Queue, log *slog.Logger) {
	tick := func() {
		repos, err := eng.Repos(ctx)
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
	_, eng, err := build(log, metrics.New())
	if err != nil {
		return err
	}
	repos, err := eng.Repos(ctx)
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
