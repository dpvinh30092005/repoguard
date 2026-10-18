// Package engine turns platform events into actions: it loads the repo policy,
// runs checks, and writes statuses, comments and labels back. It is the only
// place that combines checks/triage decisions with platform API calls.
package engine

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/activity"
	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/metrics"
	"github.com/dpvinh30092005/repoguard/internal/platform"
	"github.com/dpvinh30092005/repoguard/internal/queue"
	"github.com/dpvinh30092005/repoguard/internal/triage"
)

// Comment markers: hidden HTML comments that identify the bot's sticky comments.
const (
	markerSummary    = "<!-- repoguard:summary -->"
	markerNeedsInfo  = "<!-- repoguard:needs-info -->"
	markerDuplicates = "<!-- repoguard:duplicates -->"
	markerStale      = "<!-- repoguard:stale -->"
	markerSLA        = "<!-- repoguard:sla -->"
	markerClosed     = "<!-- repoguard:closed-by-merge -->"
	statusPrefix     = "repoguard/"
	maxLinkedIssues  = 5
	duplicateCorpus  = 300
	sweepIssueLimit  = 1000
)

type Engine struct {
	Clients   *platform.Registry
	Activity  *activity.Log // optional: feeds the web UI
	Checks    []checks.Check
	Log       *slog.Logger
	Metrics   *metrics.Registry
	PublicURL string
	Now       func() time.Time
	// Executor, when set, builds change requests for repos without working CI.
	Executor Submitter
}

// Submitter queues a sandboxed `repoguard ci` run (see package executor).
type Submitter interface {
	Allowed(repo platform.Repo) bool
	Submit(repo platform.Repo, cr *platform.ChangeRequest) error
}

func New(clients []platform.Client, cks []checks.Check, log *slog.Logger, m *metrics.Registry) *Engine {
	return &Engine{Clients: platform.NewRegistry(clients...), Checks: cks, Log: log, Metrics: m, Now: time.Now}
}

// Handle processes one event. Returned errors are retried by the queue when
// platform.IsRetryable says so.
func (e *Engine) Handle(ctx context.Context, ev platform.Event) error {
	c, ok := e.Clients.Get(ev.Repo.Platform)
	if !ok {
		return fmt.Errorf("no client for platform %q", ev.Repo.Platform)
	}
	log := e.Log.With("repo", ev.Repo.String(), "event", ev.Type, "number", ev.Number, "delivery", ev.DeliveryID)
	start := e.Now()
	var err error
	var out outcome
	switch ev.Type {
	case platform.EventCROpened, platform.EventCRUpdated:
		err = e.handleChangeRequest(ctx, c, ev, &out)
	case platform.EventCRMerged:
		err = e.handleMerged(ctx, c, ev)
	case platform.EventIssueOpened, platform.EventIssueEdited:
		err = e.handleIssue(ctx, c, ev)
	case platform.EventIssueActivity:
		err = e.handleActivity(ctx, c, ev)
	default:
		return nil
	}
	result := "ok"
	if err != nil {
		result = "error"
	}
	e.Metrics.Inc("repoguard_events_total", "platform", string(ev.Repo.Platform), "type", string(ev.Type), "result", result)
	log.Info("event handled", "result", result, "took", e.Now().Sub(start).Round(time.Millisecond), "err", err)
	if err == nil && out.result != "" {
		result = out.result
	}
	e.record(c, ev, result, out.detail, err)
	return err
}

// outcome is what a change request's checks concluded, for the activity log.
type outcome struct {
	result string // passed | failed | skipped
	detail string
}

func (e *Engine) record(c platform.Client, ev platform.Event, result, detail string, err error) {
	if e.Activity == nil {
		return
	}
	kind, target := "Issue", platform.IssueT(ev.Number)
	switch ev.Type {
	case platform.EventCROpened, platform.EventCRUpdated, platform.EventCRMerged:
		kind, target = "MR", platform.CR(ev.Number)
	}
	entry := activity.Entry{
		Platform: string(ev.Repo.Platform), Repo: ev.Repo.FullName, Kind: kind, Number: ev.Number,
		Event: string(ev.Type), Result: result, Detail: detail, URL: c.WebURL(ev.Repo, target),
	}
	if err != nil {
		entry.Detail = firstLine(err.Error())
	}
	e.Activity.Add(entry)
}

// loadConfig reads the policy from the default branch. A broken file falls back
// to defaults and returns a warning to show users instead of failing silently.
func (e *Engine) loadConfig(ctx context.Context, c platform.Client, repo platform.Repo) (config.Repo, string, error) {
	branch := repo.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	data, err := c.GetFile(ctx, repo, branch, config.FileName)
	if errors.Is(err, platform.ErrNotFound) {
		return config.Default(), "", nil
	}
	if err != nil {
		return config.Repo{}, "", err
	}
	cfg, perr := config.ParseRepo(data)
	if perr != nil {
		return cfg, fmt.Sprintf("Invalid `%s` on `%s`, using defaults: %v", config.FileName, branch, perr), nil
	}
	return cfg, "", nil
}

// ---------------------------------------------------------------------------
// Change requests
// ---------------------------------------------------------------------------

func (e *Engine) handleChangeRequest(ctx context.Context, c platform.Client, ev platform.Event, out *outcome) error {
	repo := ev.Repo
	cr, err := c.GetChangeRequest(ctx, repo, ev.Number) // fresh state, not the payload
	if err != nil {
		return err
	}
	if cr.State != "open" {
		*out = outcome{"skipped", "no longer open"}
		return nil
	}
	cfg, cfgWarning, err := e.loadConfig(ctx, c, repo)
	if err != nil {
		return err
	}
	if cr.Draft && cfg.PullRequest.SkipDrafts {
		*out = outcome{"skipped", "draft"}
		return nil
	}
	if e.Executor != nil && ev.CodeChanged && e.Executor.Allowed(repo) {
		// Runs on its own queue, in parallel with the checks below; duplicates collapse.
		if err := e.Executor.Submit(repo, cr); err != nil && !errors.Is(err, queue.ErrDuplicate) {
			e.Log.Warn("executor submit failed", "repo", repo.String(), "number", cr.Number, "err", err)
		}
	}
	files, err := c.ListChanges(ctx, repo, cr)
	if err != nil {
		return err
	}

	var selected, skipped []checks.Check
	for _, ch := range e.Checks {
		if !ch.Enabled(cfg) {
			continue
		}
		if co, ok := ch.(checks.CodeOnly); ok && co.CodeOnly() && !ev.CodeChanged {
			skipped = append(skipped, ch)
			continue
		}
		selected = append(selected, ch)
	}
	for _, ch := range selected {
		if err := e.setStatus(ctx, c, repo, cr.HeadSHA, platform.Status{Context: statusPrefix + ch.Name(), State: platform.StatePending, Description: "Running…", TargetURL: e.PublicURL}); err != nil {
			return err
		}
	}

	results := e.runChecks(ctx, selected, checks.Input{Repo: repo, CR: cr, Files: files, Config: cfg})
	*out = summarize(results)

	// Idempotent writes first (statuses, sticky comment), the non-idempotent
	// inline review last, so a retry after a partial failure can't double-post.
	for _, r := range results {
		st := platform.Status{Context: statusPrefix + r.Check, State: r.State, Description: r.Summary, TargetURL: e.PublicURL}
		if err := e.setStatus(ctx, c, repo, cr.HeadSHA, st); err != nil {
			return err
		}
	}
	if err := c.UpsertComment(ctx, repo, platform.CR(cr.Number), markerSummary, renderSummary(cr, results, skipped, cfgWarning)); err != nil {
		return err
	}
	if ev.Type == platform.EventCROpened && cfg.Issues.Link.Enabled {
		if err := e.linkIssues(ctx, c, repo, cr, cfg.Issues.Link); err != nil {
			return err
		}
	}
	return e.postInline(ctx, c, repo, cr, results)
}

// setStatus tolerates 403: GitLab may refuse commit statuses when CI/CD is
// disabled for a project; the summary comment still carries every result.
func (e *Engine) setStatus(ctx context.Context, c platform.Client, repo platform.Repo, sha string, st platform.Status) error {
	err := c.SetStatus(ctx, repo, sha, st)
	if platform.IsForbidden(err) {
		e.Log.Warn("commit status refused (403); results are in the summary comment", "repo", repo.String(), "context", st.Context)
		return nil
	}
	return err
}

// summarize turns check results into one activity line: the failing checks, or a pass count.
func summarize(results []checks.Result) outcome {
	var failing []string
	for _, r := range results {
		if r.State == platform.StateFailure || r.State == platform.StateError {
			failing = append(failing, r.Check+": "+r.Summary)
		}
	}
	if len(failing) > 0 {
		return outcome{"failed", strings.Join(failing, " · ")}
	}
	return outcome{"passed", fmt.Sprintf("%d check(s) passed", len(results))}
}

func (e *Engine) runChecks(ctx context.Context, cks []checks.Check, in checks.Input) []checks.Result {
	results := make([]checks.Result, len(cks))
	var wg sync.WaitGroup
	for i, ch := range cks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := ch.Run(ctx, in)
			if err != nil {
				// A broken check reports "error" on its own status; the others still land.
				e.Log.Error("check failed", "check", ch.Name(), "repo", in.Repo.String(), "err", err)
				res = checks.Result{Check: ch.Name(), State: platform.StateError, Summary: "Check errored: " + firstLine(err.Error())}
			}
			res.Check = ch.Name()
			e.Metrics.Inc("repoguard_checks_total", "check", ch.Name(), "state", string(res.State))
			results[i] = res
		}()
	}
	wg.Wait()
	return results
}

// postInline posts anchored findings as one review, skipping comments already
// present from earlier pushes (matched by a content fingerprint, not line).
func (e *Engine) postInline(ctx context.Context, c platform.Client, repo platform.Repo, cr *platform.ChangeRequest, results []checks.Result) error {
	var comments []platform.ReviewComment
	for _, r := range results {
		for _, f := range r.Findings {
			if f.Inline() {
				comments = append(comments, platform.ReviewComment{Path: f.Path, Line: f.Line, Body: inlineBody(r.Check, f)})
			}
		}
	}
	if len(comments) == 0 {
		return nil
	}
	existing, err := c.ListReviewComments(ctx, repo, cr)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, b := range existing {
		if fp := fingerprintIn(b); fp != "" {
			seen[fp] = true
		}
	}
	fresh := comments[:0]
	for _, cm := range comments {
		if fp := fingerprintIn(cm.Body); !seen[fp] {
			seen[fp] = true
			fresh = append(fresh, cm)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	return c.CreateReview(ctx, repo, cr, fmt.Sprintf("repoguard: %d new finding(s) on this push.", len(fresh)), fresh)
}

func inlineBody(check string, f checks.Finding) string {
	sum := sha1.Sum([]byte(check + "|" + f.Path + "|" + f.Message))
	fp := hex.EncodeToString(sum[:6])
	return fmt.Sprintf("<!-- repoguard:fp=%s -->\n**repoguard · %s**\n\n%s", fp, check, f.Message)
}

func fingerprintIn(body string) string {
	_, rest, ok := strings.Cut(body, "<!-- repoguard:fp=")
	if !ok {
		return ""
	}
	fp, _, _ := strings.Cut(rest, " ")
	return fp
}

var stateIcon = map[platform.State]string{
	platform.StateSuccess: "✅", platform.StateFailure: "❌", platform.StateError: "⚠️", platform.StatePending: "⏳",
}

var severityIcon = map[checks.Severity]string{
	checks.SeverityError: "❌", checks.SeverityWarning: "⚠️", checks.SeverityInfo: "ℹ️",
}

func renderSummary(cr *platform.ChangeRequest, results []checks.Result, skipped []checks.Check, cfgWarning string) string {
	var b strings.Builder
	b.WriteString(markerSummary + "\n### 🛡️ repoguard\n\n")
	if cfgWarning != "" {
		b.WriteString("> [!WARNING]\n> " + cfgWarning + "\n\n")
	}
	b.WriteString("| Check | Result | Details |\n|---|---|---|\n")
	for _, r := range results {
		fmt.Fprintf(&b, "| `%s` | %s %s | %s |\n", r.Check, stateIcon[r.State], r.State, r.Summary)
	}
	for _, ch := range skipped {
		fmt.Fprintf(&b, "| `%s` | ⏭️ skipped | Runs when new commits are pushed |\n", ch.Name())
	}
	for _, r := range results {
		var lines []string
		for _, f := range r.Findings {
			if f.Inline() {
				continue // shown as inline review comments
			}
			lines = append(lines, fmt.Sprintf("- %s %s", severityIcon[f.Severity], f.Message))
		}
		if len(lines) == 0 && r.Details == "" {
			continue
		}
		fmt.Fprintf(&b, "\n#### %s\n", r.Check)
		if len(lines) > 0 {
			b.WriteString(strings.Join(lines, "\n") + "\n")
		}
		if r.Details != "" {
			b.WriteString("\n" + r.Details + "\n")
		}
	}
	fmt.Fprintf(&b, "\n<sub>Commit %s · policy from `%s` on the default branch · build/test/vulnerability scan run in your CI pipeline (`repoguard ci`).</sub>\n", short(cr.HeadSHA), config.FileName)
	return b.String()
}

func (e *Engine) linkIssues(ctx context.Context, c platform.Client, repo platform.Repo, cr *platform.ChangeRequest, cfg config.Link) error {
	refs := triage.ParseRefs(cr.Title, cr.Body, cr.HeadRef)
	for i, n := range refs.All {
		if i >= maxLinkedIssues {
			break
		}
		issue, err := c.GetIssue(ctx, repo, n)
		if errors.Is(err, platform.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if issue.IsPullRequest || issue.State != "open" {
			continue
		}
		marker := fmt.Sprintf("<!-- repoguard:linked-cr-%d -->", cr.Number)
		body := fmt.Sprintf("%s\n🔗 Being worked on in %s [%s](%s) by @%s.", marker, crNoun(repo), cr.Title, cr.URL, cr.Author)
		if err := c.UpsertComment(ctx, repo, platform.IssueT(n), marker, body); err != nil {
			return err
		}
		if cfg.InProgressLabel != "" && !issue.HasLabel(cfg.InProgressLabel) {
			if err := c.AddLabels(ctx, repo, platform.IssueT(n), []string{cfg.InProgressLabel}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) handleMerged(ctx context.Context, c platform.Client, ev platform.Event) error {
	repo := ev.Repo
	cfg, _, err := e.loadConfig(ctx, c, repo)
	if err != nil || !cfg.Issues.Link.Enabled {
		return err
	}
	cr, err := c.GetChangeRequest(ctx, repo, ev.Number)
	if err != nil {
		return err
	}
	refs := triage.ParseRefs(cr.Title, cr.Body, cr.HeadRef)
	closing := map[int]bool{}
	for _, n := range refs.Closing {
		closing[n] = true
	}
	for i, n := range refs.All {
		if i >= maxLinkedIssues {
			break
		}
		issue, err := c.GetIssue(ctx, repo, n)
		if errors.Is(err, platform.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if issue.IsPullRequest {
			continue
		}
		if l := cfg.Issues.Link.InProgressLabel; l != "" && issue.HasLabel(l) {
			if err := c.RemoveLabel(ctx, repo, platform.IssueT(n), l); err != nil {
				return err
			}
		}
		// Platforms auto-close only on merges into the default branch; this covers release branches too.
		if closing[n] && cfg.Issues.Link.CloseOnMerge && issue.State == "open" {
			body := fmt.Sprintf("%s\n✅ Closed by merged %s [%s](%s).", markerClosed, crNoun(repo), cr.Title, cr.URL)
			if err := c.UpsertComment(ctx, repo, platform.IssueT(n), markerClosed, body); err != nil {
				return err
			}
			if err := c.CloseIssue(ctx, repo, n, platform.ClosedCompleted); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

func (e *Engine) handleIssue(ctx context.Context, c platform.Client, ev platform.Event) error {
	repo := ev.Repo
	issue, err := c.GetIssue(ctx, repo, ev.Number)
	if err != nil {
		return err
	}
	if issue.State != "open" || issue.IsPullRequest {
		return nil
	}
	cfg, _, err := e.loadConfig(ctx, c, repo)
	if err != nil || !cfg.Issues.Enabled {
		return err
	}
	ic := cfg.Issues
	target := platform.IssueT(issue.Number)
	a := triage.Assess(*issue, ic)

	// Auto-label only on open, so we never fight a human who removed a label later.
	if ev.Type == platform.EventIssueOpened && len(a.SuggestedLabels) > 0 {
		if err := c.AddLabels(ctx, repo, target, a.SuggestedLabels); err != nil {
			return err
		}
	}

	switch {
	case a.IsBug && len(a.MissingSections) > 0:
		body := fmt.Sprintf("%s\n👋 Thanks for the report, @%s! To help us triage it, please edit the issue and add:\n\n%s\n\n"+
			"I'll update this comment once the information is there.", markerNeedsInfo, issue.Author, bullets(a.MissingSections))
		if err := c.UpsertComment(ctx, repo, target, markerNeedsInfo, body); err != nil {
			return err
		}
		if ic.NeedsInfoLabel != "" && !issue.HasLabel(ic.NeedsInfoLabel) {
			if err := c.AddLabels(ctx, repo, target, []string{ic.NeedsInfoLabel}); err != nil {
				return err
			}
		}
	case ic.NeedsInfoLabel != "" && issue.HasLabel(ic.NeedsInfoLabel):
		if err := c.RemoveLabel(ctx, repo, target, ic.NeedsInfoLabel); err != nil {
			return err
		}
		if err := c.UpsertComment(ctx, repo, target, markerNeedsInfo, markerNeedsInfo+"\n✅ Thanks, the report now has everything we need."); err != nil {
			return err
		}
	}

	if ev.Type == platform.EventIssueOpened && ic.Duplicates.Enabled {
		corpus, err := c.ListOpenIssues(ctx, repo, duplicateCorpus)
		if err != nil {
			return err
		}
		if dups := triage.FindDuplicates(*issue, corpus, ic.Duplicates.Threshold, 3); len(dups) > 0 {
			var lines []string
			for _, d := range dups {
				lines = append(lines, fmt.Sprintf("- #%d %s (%.0f%% similar)", d.Issue.Number, d.Issue.Title, d.Score*100))
			}
			body := fmt.Sprintf("%s\n🔎 This looks similar to:\n\n%s\n\nIf it's the same problem, please add details there and close this one.",
				markerDuplicates, strings.Join(lines, "\n"))
			if err := c.UpsertComment(ctx, repo, target, markerDuplicates, body); err != nil {
				return err
			}
			if ic.Duplicates.Label != "" {
				if err := c.AddLabels(ctx, repo, target, []string{ic.Duplicates.Label}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// handleActivity un-stales an issue when a human comments on it.
func (e *Engine) handleActivity(ctx context.Context, c platform.Client, ev platform.Event) error {
	cfg, _, err := e.loadConfig(ctx, c, ev.Repo)
	if err != nil || !cfg.Issues.Stale.Enabled {
		return err
	}
	issue, err := c.GetIssue(ctx, ev.Repo, ev.Number)
	if err != nil {
		return err
	}
	if issue.HasLabel(cfg.Issues.Stale.Label) {
		return c.RemoveLabel(ctx, ev.Repo, platform.IssueT(issue.Number), cfg.Issues.Stale.Label)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Periodic sweep (stale + SLA)
// ---------------------------------------------------------------------------

// Repos lists every connected repository on every platform.
func (e *Engine) Repos(ctx context.Context) ([]platform.Repo, error) {
	var all []platform.Repo
	var errs []error
	for _, c := range e.Clients.All() {
		repos, err := c.ListRepos(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.Kind(), err))
			continue
		}
		all = append(all, repos...)
	}
	return all, errors.Join(errs...)
}

// SweepRepo applies stale and SLA policies to one repository's open issues.
func (e *Engine) SweepRepo(ctx context.Context, repo platform.Repo) error {
	c, ok := e.Clients.Get(repo.Platform)
	if !ok {
		return fmt.Errorf("no client for %s", repo.Platform)
	}
	cfg, _, err := e.loadConfig(ctx, c, repo)
	if err != nil || !cfg.Issues.Enabled || (!cfg.Issues.Stale.Enabled && !cfg.Issues.SLA.Enabled) {
		return err
	}
	issues, err := c.ListOpenIssues(ctx, repo, sweepIssueLimit)
	if err != nil {
		return err
	}
	now := e.Now()
	st, sla := cfg.Issues.Stale, cfg.Issues.SLA
	var marked, closed, breached int
	for _, issue := range issues {
		t := platform.IssueT(issue.Number)
		switch triage.StaleDecision(issue, st, now) {
		case triage.StaleMark:
			body := fmt.Sprintf("%s\n💤 No activity for %d days, so this is now marked `%s`.", markerStale, st.DaysUntilStale, st.Label)
			if st.DaysUntilClose > 0 {
				body += fmt.Sprintf(" It will be closed in %d days unless someone comments.", st.DaysUntilClose)
			}
			if err := c.UpsertComment(ctx, repo, t, markerStale, body); err != nil {
				return err
			}
			if err := c.AddLabels(ctx, repo, t, []string{st.Label}); err != nil {
				return err
			}
			marked++
		case triage.StaleClose:
			body := fmt.Sprintf("%s\n🔒 Closed after %d more days without activity. Comment or reopen if it's still relevant.", markerStale, st.DaysUntilClose)
			if err := c.UpsertComment(ctx, repo, t, markerStale, body); err != nil {
				return err
			}
			if err := c.CloseIssue(ctx, repo, issue.Number, platform.ClosedNotPlanned); err != nil {
				return err
			}
			closed++
		}
		if triage.SLABreached(issue, sla, now) {
			// Deterministic body: re-sweeping finds an identical comment and does nothing.
			body := fmt.Sprintf("%s\n🚨 High-priority issue without an assignee for more than %dh. Maintainers, please pick it up.", markerSLA, sla.HoursWithoutAssignee)
			if err := c.UpsertComment(ctx, repo, t, markerSLA, body); err != nil {
				return err
			}
			breached++
		}
	}
	e.Log.Info("sweep done", "repo", repo.String(), "open", len(issues), "marked_stale", marked, "closed", closed, "sla_breached", breached)
	return nil
}

func crNoun(repo platform.Repo) string {
	if repo.Platform == platform.GitLab {
		return "merge request"
	}
	return "pull request"
}

func bullets(items []string) string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = "- **" + s + "**"
	}
	return strings.Join(out, "\n")
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
