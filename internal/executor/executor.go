// Package executor runs `repoguard ci` on behalf of repositories whose own CI
// is unavailable (e.g. pipelines disabled on GitLab). It is opt-in and fenced:
// only allow-listed repositories, only change requests whose author could
// already push to the repository, never forks, each run in a throwaway,
// resource-limited sandbox that receives no credentials.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/dpvinh30092005/repoguard/internal/activity"
	"github.com/dpvinh30092005/repoguard/internal/ci"
	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/glob"
	"github.com/dpvinh30092005/repoguard/internal/metrics"
	"github.com/dpvinh30092005/repoguard/internal/platform"
	"github.com/dpvinh30092005/repoguard/internal/queue"
)

const (
	StatusContext = "repoguard/ci"
	Marker        = "<!-- repoguard:executor -->"
)

var ErrNotAllowed = errors.New("repository is not allowed to use the executor")

type Executor struct {
	Clients  *platform.Registry
	Sandbox  Sandbox
	Queue    *queue.Queue // dedicated queue: slow builds must not delay comments/reviews
	Allow    func(platform.Repo) bool
	Log      *slog.Logger
	Metrics  *metrics.Registry
	Activity *activity.Log // optional: feeds the web UI

	fetch func(ctx context.Context, src platform.GitSource, sha string) (string, error)
}

// Allowed reports whether repo may be built. Nothing is allowed by default.
func (x *Executor) Allowed(repo platform.Repo) bool { return x.Allow != nil && x.Allow(repo) }

// AllowGlobs allows repositories whose full name matches one of the globs (e.g. "team/*").
func AllowGlobs(globs []string) func(platform.Repo) bool {
	return func(r platform.Repo) bool { return glob.Any(globs, r.FullName) }
}

// Submit queues a run for the change request's current head.
func (x *Executor) Submit(repo platform.Repo, cr *platform.ChangeRequest) error {
	if !x.Allowed(repo) {
		return ErrNotAllowed
	}
	snapshot := *cr
	return x.Queue.Enqueue(queue.Job{
		Key: fmt.Sprintf("exec|%s|%d|%s", repo, cr.Number, cr.HeadSHA),
		Run: func(ctx context.Context) error { return x.Run(ctx, repo, &snapshot) },
	})
}

// Run fetches, builds and reports one change request.
func (x *Executor) Run(ctx context.Context, repo platform.Repo, cr *platform.ChangeRequest) error {
	c, ok := x.Clients.Get(repo.Platform)
	if !ok {
		return fmt.Errorf("no client for %s", repo.Platform)
	}
	record := func(result, detail string) {
		x.Activity.Add(activity.Entry{
			Platform: string(repo.Platform), Repo: repo.FullName, Kind: "CI", Number: cr.Number,
			Event: "sandbox build " + short(cr.HeadSHA), Result: result, Detail: detail,
			URL: c.WebURL(repo, platform.CR(cr.Number)),
		})
	}
	target := platform.CR(cr.Number)
	log := x.Log.With("repo", repo.String(), "number", cr.Number, "sha", cr.HeadSHA)

	if cr.FromFork {
		x.count("skipped_fork")
		record("skipped", "fork")
		return c.UpsertComment(ctx, repo, target, Marker, skipped(cr, "it comes from a fork"))
	}
	trusted, err := c.AuthorCanPush(ctx, repo, cr)
	if err != nil {
		return err
	}
	if !trusted {
		x.count("skipped_untrusted")
		record("skipped", "author lacks Developer access")
		return c.UpsertComment(ctx, repo, target, Marker, skipped(cr, "@"+cr.Author+" does not have Developer/write access"))
	}

	if err := x.setStatus(ctx, c, repo, cr, platform.StatePending, "Building in sandbox…"); err != nil {
		return err
	}
	src, err := c.GitSource(ctx, repo, cr)
	if err != nil {
		return err
	}
	fetchFn := x.fetch
	if fetchFn == nil {
		fetchFn = fetch
	}
	dir, err := fetchFn(ctx, src, cr.HeadSHA)
	if errors.Is(err, ErrStale) {
		log.Info("executor: head moved, skipping stale run")
		return nil
	}
	if err != nil {
		_ = x.setStatus(ctx, c, repo, cr, platform.StateError, "Could not fetch sources")
		return err
	}
	defer os.RemoveAll(dir)

	if err := pinPolicy(ctx, c, repo, dir); err != nil {
		return err
	}

	log.Info("executor: running")
	res, err := x.Sandbox.Run(ctx, dir)
	if err != nil {
		x.count("sandbox_error")
		record("error", err.Error())
		_ = x.setStatus(ctx, c, repo, cr, platform.StateError, "Sandbox error")
		_ = c.UpsertComment(ctx, repo, target, Marker, fmt.Sprintf("%s\n### 🧪 repoguard ci\n⚠️ The sandbox failed before the build could run: `%v`. A maintainer should check the repoguard logs.", Marker, err))
		return err
	}

	state, summary, body := outcome(cr, res)
	x.count(string(state))
	result := "passed"
	if state != platform.StateSuccess {
		result = "failed"
	}
	record(result, summary)
	log.Info("executor: done", "state", state, "summary", summary)
	if err := x.setStatus(ctx, c, repo, cr, state, summary); err != nil {
		return err
	}
	return c.UpsertComment(ctx, repo, target, Marker, body)
}

// pinPolicy replaces the checkout's .repoguard.yml with the default branch's,
// so a change request cannot loosen its own CI policy.
func pinPolicy(ctx context.Context, c platform.Client, repo platform.Repo, dir string) error {
	branch := repo.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	path := filepath.Join(dir, config.FileName)
	data, err := c.GetFile(ctx, repo, branch, config.FileName)
	switch {
	case errors.Is(err, platform.ErrNotFound):
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return rmErr
		}
		return nil
	case err != nil:
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func outcome(cr *platform.ChangeRequest, res Result) (platform.State, string, string) {
	header := fmt.Sprintf("%s\n### 🧪 repoguard ci · commit %s\n<sub>Run by the repoguard executor because this project's own pipelines are disabled.</sub>\n\n", Marker, short(cr.HeadSHA))
	logs := func() string {
		return "<details><summary>Build log (tail)</summary>\n\n```\n" + strings.TrimSpace(res.Logs) + "\n```\n</details>\n"
	}
	if res.TimedOut {
		return platform.StateFailure, "Timed out", header + "❌ The build exceeded the time limit and was stopped.\n\n" + logs()
	}
	if res.Report == nil {
		return platform.StateFailure, fmt.Sprintf("Build crashed (exit %d)", res.ExitCode),
			header + fmt.Sprintf("❌ `repoguard ci` exited with code %d without producing a report.\n\n", res.ExitCode) + logs()
	}
	var report ci.Report
	if err := json.Unmarshal(res.Report, &report); err != nil {
		return platform.StateError, "Unreadable report", header + "⚠️ Could not parse the build report.\n\n" + logs()
	}
	var md strings.Builder
	report.WriteMarkdown(&md)
	state := platform.StateSuccess
	if report.Failed() {
		state = platform.StateFailure
	}
	return state, report.Summary(), header + md.String()
}

func skipped(cr *platform.ChangeRequest, why string) string {
	return fmt.Sprintf("%s\n### 🧪 repoguard ci · commit %s\n⏸️ Not built automatically because %s. "+
		"Running code from outside contributors on shared infrastructure is not allowed; a maintainer can push the branch to this repository to get it built.",
		Marker, short(cr.HeadSHA), why)
}

// setStatus tolerates 403: with pipelines disabled GitLab may refuse commit
// statuses, and the sticky comment still carries the result.
func (x *Executor) setStatus(ctx context.Context, c platform.Client, repo platform.Repo, cr *platform.ChangeRequest, st platform.State, desc string) error {
	err := c.SetStatus(ctx, repo, cr.HeadSHA, platform.Status{Context: StatusContext, State: st, Description: desc})
	if platform.IsForbidden(err) {
		x.Log.Warn("executor: commit status refused (403), reporting via comment only", "repo", repo.String())
		return nil
	}
	return err
}

func (x *Executor) count(result string) {
	if x.Metrics != nil {
		x.Metrics.Inc("repoguard_executor_runs_total", "result", result)
	}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
