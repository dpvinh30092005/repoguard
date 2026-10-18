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

	ExecutorAvailable bool
	ExecutorReason    string
	ExecutorImage     func(ctx context.Context) (name string, present bool)

	mu     sync.RWMutex
	status map[platform.Kind]Status
	tpl    map[string]*template.Template
}

// Init parses templates and checks the configured connections once.
func (u *UI) Init(ctx context.Context) error {
	funcs := template.FuncMap{
		"ago":   ago,
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
		u.render(w, r, "login", map[string]any{"Error": "Sai mật khẩu."})
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
	image, present := "", false
	if u.ExecutorAvailable {
		image, present = u.ExecutorImage(r.Context())
	}
	repos := u.Store.Repos()
	executorRepos := 0
	for _, rp := range repos {
		if rp.Executor {
			executorRepos++
		}
	}
	connected := st[platform.GitLab].User != "" || st[platform.GitHub].User != ""
	u.render(w, r, "overview", map[string]any{
		"Status":            st,
		"GitLab":            st[platform.GitLab],
		"GitHub":            st[platform.GitHub],
		"AI":                u.AIEnabled(),
		"Connected":         connected,
		"Repos":             repos,
		"ExecutorRepos":     executorRepos,
		"ExecutorAvailable": u.ExecutorAvailable,
		"ExecutorReason":    u.ExecutorReason,
		"Image":             image,
		"ImagePresent":      present,
		"Queue":             u.QueueDepth(),
		"Activity":          u.Activity.List(),
		"Refresh":           true,
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
		case "advanced":
			s.PublicURL = strings.TrimRight(form("public_url"), "/")
			s.ExecutorImage = form("executor_image")
		}
	})
	if err != nil {
		redirect(w, r, "/connect", "", "Không lưu được: "+err.Error())
		return
	}
	u.refresh(r.Context())
	st := u.statuses()
	switch r.FormValue("section") {
	case "gitlab", "github":
		kind := platform.Kind(r.FormValue("section"))
		switch s := st[kind]; {
		case s.User != "":
			redirect(w, r, "/repos", fmt.Sprintf("Đã kết nối %s với tài khoản @%s. Giờ hãy chọn dự án muốn kiểm tra.", title(kind), s.User), "")
		case s.Err != "":
			redirect(w, r, "/connect", "", fmt.Sprintf("Chưa kết nối được %s: %s", title(kind), s.Err))
		default:
			redirect(w, r, "/connect", "Đã xoá kết nối.", "")
		}
	default:
		redirect(w, r, "/connect", "Đã lưu.", "")
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
		"Groups":            groups,
		"Q":                 r.URL.Query().Get("q"),
		"PublicURL":         u.PublicURL(),
		"ExecutorAvailable": u.ExecutorAvailable,
		"ExecutorReason":    u.ExecutorReason,
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
		redirect(w, r, "/repos", "", "Dự án không hợp lệ.")
		return
	}
	rec := store.Repo{
		Platform: string(repo.Platform), ID: repo.ID, FullName: repo.FullName,
		DefaultBranch: repo.DefaultBranch, InstallationID: repo.InstallationID, Mode: "poll",
	}
	msg := fmt.Sprintf("Đang theo dõi %s (hỏi %s mỗi 30 giây). Hãy mở hoặc push một MR để thử.", repo.FullName, title(repo.Platform))
	if pub := u.PublicURL(); pub != "" {
		id, err := c.CreateWebhook(r.Context(), repo, pub+"/webhooks/"+string(repo.Platform), u.Secret(repo.Platform))
		if err == nil {
			rec.Mode, rec.HookID = "webhook", id
			msg = fmt.Sprintf("Đã tạo webhook cho %s. MR mới sẽ được kiểm tra ngay lập tức.", repo.FullName)
		} else {
			msg = fmt.Sprintf("Không tạo được webhook (%v), nên chuyển sang chế độ hỏi định kỳ. %s", err, msg)
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
		redirect(w, r, "/repos", "", "Dự án chưa được bật.")
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
	redirect(w, r, "/repos", "Đã tắt kiểm tra cho "+repo.FullName+".", "")
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
		redirect(w, r, "/repos", "Đã bật tự chạy CI cho "+repo.FullName+". Mỗi lần có commit mới trong MR, repoguard sẽ tự build và test.", "")
	default:
		redirect(w, r, "/repos", "Đã tắt tự chạy CI cho "+repo.FullName+".", "")
	}
}

func (u *UI) checkNow(w http.ResponseWriter, r *http.Request) {
	repo := repoFromForm(r)
	n, err := u.Poller.CheckNow(r.Context(), repo)
	switch {
	case err != nil:
		redirect(w, r, "/repos", "", "Không đọc được danh sách MR: "+err.Error())
	case n == 0:
		redirect(w, r, "/repos", repo.FullName+" chưa có MR nào đang mở.", "")
	default:
		redirect(w, r, "/", fmt.Sprintf("Đã xếp hàng kiểm tra %d MR của %s. Kết quả sẽ hiện ở bảng bên dưới và trên từng MR.", n, repo.FullName), "")
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

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "vừa xong"
	case d < time.Hour:
		return fmt.Sprintf("%d phút trước", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d giờ trước", int(d.Hours()))
	}
	return t.Format("02/01 15:04")
}
