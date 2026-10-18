// Package web is repoguard's admin UI: connect GitLab/GitHub, pick which
// repositories to watch, switch the CI executor on per repository and watch
// recent activity. Server-rendered HTML embedded in the binary; no JS build.
package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/activity"
	"github.com/dpvinh30092005/repoguard/internal/platform"
	"github.com/dpvinh30092005/repoguard/internal/poller"
	"github.com/dpvinh30092005/repoguard/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

const cookieName = "repoguard_session"

// ExecutorInfo describes where CI builds would run right now.
type ExecutorInfo struct {
	Available    bool
	Backend      string // "docker" | "jenkins"
	Reason       string // why it is not available
	Image        string // docker: build image
	ImagePresent bool
	Detail       string // jenkins: URL and job
	Sonar        bool   // builds will analyse with SonarQube
}

// Status of one platform connection, shown on the overview.
type Status struct {
	Configured bool
	User       string
	Err        string
}

type UI struct {
	Store    *store.Store
	Clients  *platform.Registry
	Activity *activity.Log
	Poller   *poller.Poller
	Log      *slog.Logger
	Version  string

	Password   func() string
	Apply      func(ctx context.Context) map[platform.Kind]Status // rebuild clients from settings
	Secret     func(platform.Kind) string                         // webhook secret per platform
	PublicURL  func() string
	QueueDepth func() int
	AIEnabled  func() bool
	Locked     map[string]bool // settings fixed by environment variables

	Executor func(ctx context.Context) ExecutorInfo

	mu     sync.RWMutex
	status map[platform.Kind]Status
	tpl    map[string]*template.Template
}

// Init parses templates and checks the configured connections once.
func (u *UI) Init(ctx context.Context) error {
	funcs := template.FuncMap{
		"ago": ago,
		"eventLabel": func(ev string) string {
			if l, ok := eventLabels[ev]; ok {
				return l
			}
			return ev
		},
		"lower": strings.ToLower,
		"title": func(s string) string {
			if s == "" {
				return s
			}
			return strings.ToUpper(s[:1]) + s[1:]
		},
	}
	u.tpl = map[string]*template.Template{}
	for _, page := range []string{"overview", "connect", "repos", "guide", "login"} {
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", "templates/"+page+".html")
		if err != nil {
			return fmt.Errorf("template %s: %w", page, err)
		}
		u.tpl[page] = t
	}
	u.refresh(ctx)
	return nil
}

func (u *UI) refresh(ctx context.Context) {
	st := u.Apply(ctx)
	u.mu.Lock()
	u.status = st
	u.mu.Unlock()
}

func (u *UI) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", u.loginPage)
	mux.HandleFunc("POST /login", u.login)
	mux.HandleFunc("POST /logout", u.logout)
	mux.Handle("GET /{$}", u.auth(u.overview))
	mux.Handle("GET /connect", u.auth(u.connectPage))
	mux.Handle("POST /connect", u.auth(u.saveConnect))
	mux.Handle("GET /repos", u.auth(u.reposPage))
	mux.Handle("POST /repos/watch", u.auth(u.watch))
	mux.Handle("POST /repos/unwatch", u.auth(u.unwatch))
	mux.Handle("POST /repos/executor", u.auth(u.toggleExecutor))
	mux.Handle("POST /repos/check", u.auth(u.checkNow))
	mux.Handle("GET /guide", u.auth(u.guide))
	return mux
}

// ---------------------------------------------------------------------------
// Auth: one admin password, HMAC-signed session cookie, same-origin POSTs.
// ---------------------------------------------------------------------------

func (u *UI) sign(expiry int64) string {
	mac := hmac.New(sha256.New, []byte(u.Store.Get().SessionKey))
	fmt.Fprintf(mac, "admin|%d", expiry)
	return strconv.FormatInt(expiry, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (u *UI) validSession(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	exp, _, ok := strings.Cut(c.Value, ".")
	expiry, err := strconv.ParseInt(exp, 10, 64)
	if !ok || err != nil || time.Now().Unix() > expiry {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(u.sign(expiry)))
}

func (u *UI) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !u.validSession(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "cross-site request blocked", http.StatusForbidden)
			return
		}
		h(w, r)
	})
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return true // non-browser client with a valid session cookie
	}
	o, err := url.Parse(origin)
	return err == nil && o.Host == r.Host
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "login", map[string]any{})
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(u.Password())) != 1 {
		time.Sleep(500 * time.Millisecond) // slow down guessing
		u.render(w, r, "login", map[string]any{"Error": "Wrong password."})
		return
	}
	expiry := time.Now().Add(7 * 24 * time.Hour).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: u.sign(expiry), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil, Expires: time.Unix(expiry, 0),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------------

func (u *UI) statuses() map[platform.Kind]Status {
	u.mu.RLock()
	defer u.mu.RUnlock()
	out := map[platform.Kind]Status{}
	for k, v := range u.status {
		out[k] = v
	}
	return out
}

func (u *UI) overview(w http.ResponseWriter, r *http.Request) {
	st := u.statuses()
	ex := u.Executor(r.Context())
	repos := u.Store.Repos()
	executorRepos := 0
	for _, rp := range repos {
		if rp.Executor {
			executorRepos++
		}
	}
	connected := st[platform.GitLab].User != "" || st[platform.GitHub].User != ""
	u.render(w, r, "overview", map[string]any{
		"Status":        st,
		"GitLab":        st[platform.GitLab],
		"GitHub":        st[platform.GitHub],
		"AI":            u.AIEnabled(),
		"Connected":     connected,
		"Repos":         repos,
		"ExecutorRepos": executorRepos,
		"Ex":            ex,
		"Queue":         u.QueueDepth(),
		"Activity":      u.Activity.List(),
		"Refresh":       true,
	})
}

func (u *UI) connectPage(w http.ResponseWriter, r *http.Request) {
	s := u.Store.Get()
	u.render(w, r, "connect", map[string]any{
		"S":         s,
		"Status":    u.statuses(),
		"GitLab":    u.statuses()[platform.GitLab],
		"GitHub":    u.statuses()[platform.GitHub],
		"Locked":    u.Locked,
		"PublicURL": u.PublicURL(),
		"AI":        u.AIEnabled(),
		"Secret":    s.WebhookSecret,
		"Ex":        u.Executor(r.Context()),
	})
}

func (u *UI) saveConnect(w http.ResponseWriter, r *http.Request) {
	form := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	err := u.Store.Update(func(s *store.Settings) {
		switch r.FormValue("section") {
		case "gitlab":
			s.GitLabURL = strings.TrimRight(form("gitlab_url"), "/")
			if t := form("gitlab_token"); t != "" {
				s.GitLabToken = t
			}
			if r.FormValue("clear") == "1" {
				s.GitLabToken = ""
			}
		case "github":
			s.GitHubAPIURL = strings.TrimRight(form("github_api_url"), "/")
			if t := form("github_token"); t != "" {
				s.GitHubToken = t
			}
			if r.FormValue("clear") == "1" {
				s.GitHubToken = ""
			}
		case "ai":
			if k := form("anthropic_key"); k != "" {
				s.AnthropicKey = k
			}
			if r.FormValue("clear") == "1" {
				s.AnthropicKey = ""
			}
		case "build":
			s.BuildBackend = form("build_backend")
			s.ExecutorImage = form("executor_image")
			s.JenkinsURL = strings.TrimRight(form("jenkins_url"), "/")
			s.JenkinsUser = form("jenkins_user")
			s.JenkinsJob = form("jenkins_job")
			if t := form("jenkins_token"); t != "" {
				s.JenkinsToken = t
			}
		case "sonar":
			s.SonarURL = strings.TrimRight(form("sonar_url"), "/")
			if t := form("sonar_token"); t != "" {
				s.SonarToken = t
			}
			if r.FormValue("clear") == "1" {
				s.SonarURL, s.SonarToken = "", ""
			}
		case "advanced":
			s.PublicURL = strings.TrimRight(form("public_url"), "/")
		}
	})
	if err != nil {
		redirect(w, r, "/connect", "", "Could not save: "+err.Error())
		return
	}
	u.refresh(r.Context())
	st := u.statuses()
	switch r.FormValue("section") {
	case "gitlab", "github":
		kind := platform.Kind(r.FormValue("section"))
		switch s := st[kind]; {
		case s.User != "":
			redirect(w, r, "/repos", fmt.Sprintf("Connected to %s as @%s. Now pick the projects to check.", title(kind), s.User), "")
		case s.Err != "":
			redirect(w, r, "/connect", "", fmt.Sprintf("Could not connect to %s: %s", title(kind), s.Err))
		default:
			redirect(w, r, "/connect", "Connection removed.", "")
		}
	default:
		redirect(w, r, "/connect", "Saved.", "")
	}
}

type repoRow struct {
	Repo    platform.Repo
	Watched *store.Repo
}

type platformRepos struct {
	Kind  platform.Kind
	Rows  []repoRow
	Err   string
	Total int
}

func (u *UI) reposPage(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	watched := u.Store.Get().Repos
	var groups []platformRepos
	for _, c := range u.Clients.All() {
		g := platformRepos{Kind: c.Kind()}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		repos, err := c.ListRepos(ctx)
		cancel()
		if err != nil {
			g.Err = err.Error()
		}
		g.Total = len(repos)
		for _, rp := range repos {
			if q != "" && !strings.Contains(strings.ToLower(rp.FullName), q) {
				continue
			}
			row := repoRow{Repo: rp}
			if wr, ok := watched[store.Key(string(rp.Platform), rp.FullName)]; ok {
				row.Watched = &wr
			}
			g.Rows = append(g.Rows, row)
		}
		// Watched repositories first, then alphabetical.
		sort.SliceStable(g.Rows, func(i, j int) bool {
			if (g.Rows[i].Watched != nil) != (g.Rows[j].Watched != nil) {
				return g.Rows[i].Watched != nil
			}
			return g.Rows[i].Repo.FullName < g.Rows[j].Repo.FullName
		})
		groups = append(groups, g)
	}
	u.render(w, r, "repos", map[string]any{
		"Groups":    groups,
		"Q":         r.URL.Query().Get("q"),
		"PublicURL": u.PublicURL(),
		"Ex":        u.Executor(r.Context()),
	})
}

func repoFromForm(r *http.Request) platform.Repo {
	inst, _ := strconv.ParseInt(r.FormValue("installation_id"), 10, 64)
	return platform.Repo{
		Platform:       platform.Kind(r.FormValue("platform")),
		ID:             r.FormValue("id"),
		FullName:       r.FormValue("full_name"),
		DefaultBranch:  r.FormValue("default_branch"),
		InstallationID: inst,
	}
}

func (u *UI) watch(w http.ResponseWriter, r *http.Request) {
	repo := repoFromForm(r)
	c, ok := u.Clients.Get(repo.Platform)
	if !ok || repo.FullName == "" {
		redirect(w, r, "/repos", "", "Invalid project.")
		return
	}
	rec := store.Repo{
		Platform: string(repo.Platform), ID: repo.ID, FullName: repo.FullName,
		DefaultBranch: repo.DefaultBranch, InstallationID: repo.InstallationID, Mode: "poll",
	}
	msg := fmt.Sprintf("Now watching %s (repoguard polls %s every 30 seconds). Open or push to a merge request to try it.", repo.FullName, title(repo.Platform))
	if pub := u.PublicURL(); pub != "" {
		id, err := c.CreateWebhook(r.Context(), repo, pub+"/webhooks/"+string(repo.Platform), u.Secret(repo.Platform))
		if err == nil {
			rec.Mode, rec.HookID = "webhook", id
			msg = fmt.Sprintf("Webhook created for %s. New merge requests are checked immediately.", repo.FullName)
		} else {
			msg = fmt.Sprintf("Could not create a webhook (%v), so repoguard falls back to polling. %s", err, msg)
		}
	}
	if err := u.Store.Update(func(s *store.Settings) { s.Repos[rec.Key()] = rec }); err != nil {
		redirect(w, r, "/repos", "", err.Error())
		return
	}
	redirect(w, r, "/repos", msg, "")
}

func (u *UI) unwatch(w http.ResponseWriter, r *http.Request) {
	repo := repoFromForm(r)
	key := store.Key(string(repo.Platform), repo.FullName)
	rec, ok := u.Store.Get().Repos[key]
	if !ok {
		redirect(w, r, "/repos", "", "This project is not being watched.")
		return
	}
	if rec.HookID != "" {
		if c, ok := u.Clients.Get(repo.Platform); ok {
			if err := c.DeleteWebhook(r.Context(), repo, rec.HookID); err != nil {
				u.Log.Warn("ui: delete webhook failed", "repo", key, "err", err)
			}
		}
	}
	if err := u.Store.Update(func(s *store.Settings) { delete(s.Repos, key) }); err != nil {
		redirect(w, r, "/repos", "", err.Error())
		return
	}
	redirect(w, r, "/repos", "Stopped checking "+repo.FullName+".", "")
}

func (u *UI) toggleExecutor(w http.ResponseWriter, r *http.Request) {
	repo := repoFromForm(r)
	key := store.Key(string(repo.Platform), repo.FullName)
	var on bool
	err := u.Store.Update(func(s *store.Settings) {
		rec, ok := s.Repos[key]
		if !ok {
			return
		}
		rec.Executor = !rec.Executor
		on = rec.Executor
		s.Repos[key] = rec
	})
	switch {
	case err != nil:
		redirect(w, r, "/repos", "", err.Error())
	case on:
		redirect(w, r, "/repos", "CI builds turned on for "+repo.FullName+". Every new commit in a merge request will be built and tested.", "")
	default:
		redirect(w, r, "/repos", "CI builds turned off for "+repo.FullName+".", "")
	}
}

func (u *UI) checkNow(w http.ResponseWriter, r *http.Request) {
	repo := repoFromForm(r)
	n, err := u.Poller.CheckNow(r.Context(), repo)
	switch {
	case err != nil:
		redirect(w, r, "/repos", "", "Could not list merge requests: "+err.Error())
	case n == 0:
		redirect(w, r, "/repos", repo.FullName+" has no open merge requests.", "")
	default:
		redirect(w, r, "/", fmt.Sprintf("Queued %d merge request(s) of %s for checking. Results appear below and on each merge request.", n, repo.FullName), "")
	}
}

func (u *UI) guide(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, "guide", map[string]any{"PublicURL": u.PublicURL()})
}

// ---------------------------------------------------------------------------

func (u *UI) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	data["Page"] = page
	data["Version"] = u.Version
	data["OK"] = r.URL.Query().Get("ok")
	data["Err"] = r.URL.Query().Get("err")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'unsafe-inline'")
	if err := u.tpl[page].ExecuteTemplate(w, "layout.html", data); err != nil {
		u.Log.Error("ui: render", "page", page, "err", err)
	}
}

func redirect(w http.ResponseWriter, r *http.Request, to, ok, errMsg string) {
	v := url.Values{}
	if ok != "" {
		v.Set("ok", ok)
	}
	if errMsg != "" {
		v.Set("err", errMsg)
	}
	if len(v) > 0 {
		to += "?" + v.Encode()
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func title(k platform.Kind) string {
	if k == platform.GitLab {
		return "GitLab"
	}
	return "GitHub"
}

var eventLabels = map[string]string{
	"change_request.opened":  "New MR / new commits",
	"change_request.updated": "MR edited",
	"change_request.merged":  "MR merged",
	"issue.opened":           "New issue",
	"issue.edited":           "Issue edited",
	"issue.activity":         "New comment",
	"ci.build":               "CI build",
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Format("Jan 2 15:04")
}
