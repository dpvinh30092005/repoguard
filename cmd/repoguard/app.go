package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/checks/aireview"
	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/engine"
	"github.com/dpvinh30092005/repoguard/internal/executor"
	"github.com/dpvinh30092005/repoguard/internal/platform"
	"github.com/dpvinh30092005/repoguard/internal/platform/github"
	"github.com/dpvinh30092005/repoguard/internal/platform/gitlab"
	"github.com/dpvinh30092005/repoguard/internal/results"
	"github.com/dpvinh30092005/repoguard/internal/store"
	"github.com/dpvinh30092005/repoguard/internal/web"
)

// app holds what can change while repoguard runs: credentials and backends
// entered in the web UI. Environment variables always win over UI settings,
// so a deployment can pin anything it wants to manage itself.
type app struct {
	cfg      config.Service
	store    *store.Store
	log      *slog.Logger
	hc       *http.Client
	reg      *platform.Registry
	reviewer *aireview.Switchable
	exec     *executor.Executor // nil when REPOGUARD_EXECUTOR=off
	results  *results.Store

	dockerOK  bool
	dockerWhy string

	mu sync.Mutex // serializes apply
}

func newApp(cfg config.Service, st *store.Store, log *slog.Logger) *app {
	a := &app{cfg: cfg, store: st, log: log, hc: platform.NewHTTPClient(), reg: platform.NewRegistry(), reviewer: &aireview.Switchable{}}
	a.dockerOK = true
	for _, bin := range []string{"docker", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			a.dockerOK, a.dockerWhy = false, "repoguard is not running next to Docker (use deploy/docker-compose.executor.yml or deploy/stack)"
		}
	}
	return a
}

// apply (re)builds platform clients, the AI reviewer and the executor backend
// from the current settings and reports each platform's connection status.
func (a *app) apply(ctx context.Context) map[platform.Kind]web.Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.store.Get()
	status := map[platform.Kind]web.Status{}

	if c, err := a.githubClient(s); err != nil {
		status[platform.GitHub] = web.Status{Configured: true, Err: err.Error()}
		a.reg.Remove(platform.GitHub)
	} else if c != nil {
		a.reg.Set(c)
		status[platform.GitHub] = check(ctx, c)
	} else {
		a.reg.Remove(platform.GitHub)
	}

	switch url, token := a.gitlabCreds(s); {
	case token != "":
		c := gitlab.New(url, token, a.hc)
		a.reg.Set(c)
		status[platform.GitLab] = check(ctx, c)
	default:
		a.reg.Remove(platform.GitLab)
	}

	if key := firstNonEmpty(a.cfg.AI.APIKey, s.AnthropicKey); key != "" {
		a.reviewer.Set(aireview.NewClaude(key, a.cfg.AI.Model, a.cfg.AI.Effort))
	} else {
		a.reviewer.Set(nil)
	}

	if a.exec != nil {
		info := a.executorInfoLocked(s)
		switch {
		case !info.Available:
			a.exec.Configure(nil, nil, nil)
		case info.Backend == "jenkins":
			j := a.jenkinsSettings(s)
			a.exec.Configure(nil, &executor.Jenkins{URL: j.URL, User: j.User, Token: j.Token, Job: j.Job, Timeout: a.cfg.Executor.Timeout}, nil)
		default:
			x := a.cfg.Executor
			a.exec.Configure(executor.Docker{
				Image: info.Image, CPUs: x.CPUs, Memory: x.Memory, PidsLimit: x.PidsLimit,
				Network: x.Network, Runtime: x.Runtime, Timeout: x.Timeout,
			}, nil, a.sonarEnv(s))
		}
		a.log.Info("executor configured", "available", info.Available, "backend", info.Backend, "sonar", info.Sonar, "reason", info.Reason)
	}
	return status
}

func check(ctx context.Context, c platform.Client) web.Status {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	user, err := c.CurrentUser(ctx)
	if err != nil {
		return web.Status{Configured: true, Err: err.Error()}
	}
	return web.Status{Configured: true, User: user}
}

func (a *app) githubClient(s store.Settings) (platform.Client, error) {
	g := a.cfg.GitHub
	switch {
	case g.AppID != 0:
		app, err := github.NewAppAuth(g.AppID, g.PrivateKeyPEM, g.APIURL, a.hc)
		if err != nil {
			return nil, err
		}
		return github.New(g.APIURL, app, "", a.hc), nil
	case g.Token != "":
		return github.New(g.APIURL, nil, g.Token, a.hc), nil
	case s.GitHubToken != "":
		return github.New(firstNonEmpty(s.GitHubAPIURL, "https://api.github.com"), nil, s.GitHubToken, a.hc), nil
	}
	return nil, nil
}

func (a *app) gitlabCreds(s store.Settings) (url, token string) {
	if a.cfg.GitLab.Token != "" {
		return a.cfg.GitLab.URL, a.cfg.GitLab.Token
	}
	return firstNonEmpty(s.GitLabURL, "https://gitlab.com"), s.GitLabToken
}

func (a *app) jenkinsSettings(s store.Settings) config.Jenkins {
	if a.cfg.Executor.UseJenkins() {
		return a.cfg.Executor.Jenkins
	}
	return config.Jenkins{URL: s.JenkinsURL, User: s.JenkinsUser, Token: s.JenkinsToken, Job: firstNonEmpty(s.JenkinsJob, "repoguard-ci")}
}

func (a *app) sonarEnv(s store.Settings) map[string]string {
	url := firstNonEmpty(a.cfg.Executor.Sonar.HostURL, s.SonarURL)
	token := firstNonEmpty(a.cfg.Executor.Sonar.Token, s.SonarToken)
	if url == "" || token == "" {
		return nil
	}
	return map[string]string{"SONAR_HOST_URL": url, "SONAR_TOKEN": token}
}

// executorInfo is shown in the UI; it mirrors the decision apply makes.
func (a *app) executorInfo(ctx context.Context) web.ExecutorInfo {
	info := a.executorInfoLocked(a.store.Get())
	if info.Available && info.Backend == "docker" {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		info.ImagePresent = exec.CommandContext(ctx, "docker", "image", "inspect", info.Image).Run() == nil
	}
	return info
}

func (a *app) executorInfoLocked(s store.Settings) web.ExecutorInfo {
	x := a.cfg.Executor
	if x.Disabled() || a.exec == nil {
		return web.ExecutorInfo{Reason: "disabled with REPOGUARD_EXECUTOR=off"}
	}
	backend := "docker"
	if x.UseJenkins() || (!x.Required() && s.BuildBackend == "jenkins") {
		backend = "jenkins"
	}
	info := web.ExecutorInfo{Backend: backend, Sonar: a.sonarEnv(s) != nil}
	if backend == "jenkins" {
		j := a.jenkinsSettings(s)
		if j.URL == "" || j.User == "" || j.Token == "" {
			info.Reason = "Jenkins is not fully configured (URL, user and token are required)."
			return info
		}
		info.Available, info.Detail = true, j.URL+" · job "+j.Job
		return info
	}
	if !a.dockerOK {
		info.Reason = a.dockerWhy
		return info
	}
	info.Available = true
	info.Image = firstNonEmpty(x.Image, s.ExecutorImage, defaultExecutorImage)
	return info
}

// allow: repositories matching REPOGUARD_EXECUTOR_REPOS, or switched on in the UI.
func (a *app) allow(r platform.Repo) bool {
	if executor.AllowGlobs(a.cfg.Executor.Repos)(r) {
		return true
	}
	rec, ok := a.store.Get().Repos[store.Key(string(r.Platform), r.FullName)]
	return ok && rec.Executor
}

// secret is the webhook secret per platform (env first, then the generated one).
func (a *app) secret(k platform.Kind) string {
	if _, ok := a.reg.Get(k); !ok {
		return ""
	}
	env := map[platform.Kind]string{platform.GitHub: a.cfg.GitHub.WebhookSecret, platform.GitLab: a.cfg.GitLab.WebhookSecret}[k]
	return firstNonEmpty(env, a.store.Get().WebhookSecret)
}

func (a *app) publicURL() string { return firstNonEmpty(a.cfg.PublicURL, a.store.Get().PublicURL) }

func (a *app) password() string {
	return firstNonEmpty(os.Getenv("REPOGUARD_ADMIN_PASSWORD"), a.store.Get().AdminPassword)
}

func (a *app) locked() map[string]bool {
	x := a.cfg.Executor
	return map[string]bool{
		"github":     a.cfg.GitHub.AppID != 0 || a.cfg.GitHub.Token != "",
		"gitlab":     a.cfg.GitLab.Token != "",
		"ai":         a.cfg.AI.APIKey != "",
		"public_url": a.cfg.PublicURL != "",
		"executor":   x.UseJenkins() || x.Required(),
		"sonar":      x.Sonar.HostURL != "" && x.Sonar.Token != "",
	}
}

// watchedRepos returns repositories connected in the UI; mode "" means any.
func (a *app) watchedRepos(mode string) []platform.Repo {
	var out []platform.Repo
	for _, r := range a.store.Repos() {
		if mode != "" && r.Mode != mode {
			continue
		}
		out = append(out, platform.Repo{
			Platform: platform.Kind(r.Platform), ID: r.ID, FullName: r.FullName,
			DefaultBranch: r.DefaultBranch, InstallationID: r.InstallationID,
		})
	}
	return out
}

// sweepTargets limits stale/SLA sweeps to repositories the operator chose.
// Without any chosen in the UI, it falls back to everything an env-configured
// integration can see (e.g. a GitHub App's installations), as before.
func (a *app) sweepTargets(ctx context.Context, eng *engine.Engine) ([]platform.Repo, error) {
	if repos := a.watchedRepos(""); len(repos) > 0 {
		return repos, nil
	}
	if a.locked()["github"] || a.locked()["gitlab"] {
		return eng.Repos(ctx)
	}
	return nil, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
