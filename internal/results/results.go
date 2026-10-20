// Package results keeps the latest outcome of every change request repoguard
// has checked: each check with its findings, and the CI build. The web console
// reads it to show what is wrong without opening GitLab or GitHub. It is one
// JSON file next to the settings, rewritten atomically after each change.
package results

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/ci"
	"github.com/dpvinh30092005/repoguard/internal/platform"
)

// Overall states of a review, as shown in the console.
const (
	Failing = "failing"
	Passing = "passing"
	Running = "running"
	Skipped = "skipped"
)

// StateSkipped marks a check that did not run on the latest event.
const StateSkipped = "skipped"

const maxOutput = 4 << 10 // per failed build stage

type Finding struct {
	Severity string `json:"severity"` // error | warning | info
	Message  string `json:"message"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
}

type Check struct {
	Name     string    `json:"name"`  // status context suffix: pr-rules, secrets, ai-review
	State    string    `json:"state"` // success | failure | error | pending | skipped
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings,omitempty"`
}

func (c Check) Label() string { return checks.Label(c.Name) }

type Build struct {
	State   string     `json:"state"` // queued | running | success | failure | error | skipped
	Summary string     `json:"summary"`
	SHA     string     `json:"sha"`
	Backend string     `json:"backend,omitempty"`
	URL     string     `json:"url,omitempty"` // the build page on a remote backend
	Report  *ci.Report `json:"report,omitempty"`
	Logs    string     `json:"logs,omitempty"` // log tail when there is no report
	At      time.Time  `json:"at"`
}

// Review is everything known about one change request.
type Review struct {
	ID        string    `json:"id"`
	Platform  string    `json:"platform"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	URL       string    `json:"url"`
	Branch    string    `json:"branch,omitempty"`
	Target    string    `json:"target,omitempty"`
	HeadSHA   string    `json:"head_sha"`
	State     string    `json:"state"` // open | merged | closed
	Draft     bool      `json:"draft,omitempty"`
	Warning   string    `json:"warning,omitempty"` // e.g. an invalid .repoguard.yml
	Checks    []Check   `json:"checks,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Build     *Build    `json:"build,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Status is the overall verdict: failing if any check or the build failed.
func (r Review) Status() string {
	if len(r.Failures()) > 0 {
		return Failing
	}
	if r.Build != nil && (r.Build.State == "queued" || r.Build.State == "running") {
		return Running
	}
	for _, c := range r.Checks {
		if c.State == "success" {
			return Passing
		}
	}
	if r.Build != nil && r.Build.State == "success" {
		return Passing
	}
	return Skipped
}

// Failures lists one line per failing check, for tables and summaries.
func (r Review) Failures() []string {
	var out []string
	for _, c := range r.Checks {
		if c.State == "failure" || c.State == "error" {
			out = append(out, c.Label()+": "+c.Summary)
		}
	}
	if r.Build != nil && r.Build.SHA == r.HeadSHA && (r.Build.State == "failure" || r.Build.State == "error") {
		out = append(out, checks.Label("ci")+": "+r.Build.Summary)
	}
	return out
}

// MainProblem is the most useful single line for a table: the first error of
// the first failing check (its first sentence), or that check's summary.
func (r Review) MainProblem() string {
	for _, c := range r.Checks {
		if c.State != "failure" && c.State != "error" {
			continue
		}
		for _, f := range c.Findings {
			if f.Severity == string(checks.SeverityError) {
				return c.Label() + ": " + firstSentence(f.Message)
			}
		}
		return c.Label() + ": " + c.Summary
	}
	if r.Build != nil && r.Build.SHA == r.HeadSHA && (r.Build.State == "failure" || r.Build.State == "error") {
		return checks.Label("ci") + ": " + r.Build.Summary
	}
	return ""
}

// firstSentence cuts at the first ". " outside inline code.
func firstSentence(s string) string {
	inCode := false
	for i := 0; i < len(s)-1; i++ {
		switch {
		case s[i] == '`':
			inCode = !inCode
		case !inCode && s[i] == '.' && s[i+1] == ' ':
			return s[:i+1]
		}
	}
	return s
}

// Problems counts error findings across checks, plus a failed build.
func (r Review) Problems() int {
	n := 0
	for _, c := range r.Checks {
		for _, f := range c.Findings {
			if f.Severity == string(checks.SeverityError) {
				n++
			}
		}
		if c.State == "error" {
			n++
		}
	}
	if r.Build != nil && r.Build.SHA == r.HeadSHA && (r.Build.State == "failure" || r.Build.State == "error") {
		n++
	}
	return n
}

// Check returns the named check, if it ran.
func (r Review) Check(name string) (Check, bool) {
	for _, c := range r.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// BuildOutdated reports whether the build ran on an older commit.
func (r Review) BuildOutdated() bool { return r.Build != nil && r.Build.SHA != r.HeadSHA }

func Key(platform, repo string, number int) string {
	return fmt.Sprintf("%s:%s#%d", platform, repo, number)
}

func id(key string) string {
	sum := sha1.Sum([]byte(key))
	return hex.EncodeToString(sum[:6])
}

type Store struct {
	path string // empty: memory only
	max  int
	now  func() time.Time

	mu    sync.RWMutex
	items map[string]*Review
}

// Open loads the file at path (created on first write). An empty path keeps
// results in memory only.
func Open(path string) (*Store, error) {
	s := &Store{path: path, max: 300, now: time.Now, items: map[string]*Review{}}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	var list []*Review
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, r := range list {
		s.items[Key(r.Platform, r.Repo, r.Number)] = r
	}
	return s, nil
}

// meta copies the change request's details into r.
func meta(r *Review, repo platform.Repo, cr *platform.ChangeRequest, url string) {
	r.Platform, r.Repo, r.Number = string(repo.Platform), repo.FullName, cr.Number
	r.Title, r.Author, r.Branch, r.Target = cr.Title, cr.Author, cr.HeadRef, cr.BaseRef
	r.HeadSHA, r.Draft = cr.HeadSHA, cr.Draft
	if cr.State != "" {
		r.State = cr.State
	}
	if url != "" {
		r.URL = url
	}
}

// RecordChecks stores the results of one check run. Checks that were skipped
// keep their previous result for the same commit, so editing a title does not
// erase the AI review of the code.
func (s *Store) RecordChecks(repo platform.Repo, cr *platform.ChangeRequest, url string, results []checks.Result, skipped []string, warning string) {
	if s == nil {
		return
	}
	s.update(repo, cr, url, func(r *Review, prevSHA string) {
		prev := map[string]Check{}
		for _, c := range r.Checks {
			prev[c.Name] = c
		}
		var next []Check
		for _, res := range results {
			c := Check{Name: res.Check, State: string(res.State), Summary: res.Summary}
			for _, f := range res.Findings {
				c.Findings = append(c.Findings, Finding{Severity: string(f.Severity), Message: f.Message, Path: f.Path, Line: f.Line})
			}
			next = append(next, c)
		}
		for _, name := range skipped {
			if p, ok := prev[name]; ok && prevSHA == cr.HeadSHA {
				next = append(next, p)
				continue
			}
			next = append(next, Check{Name: name, State: StateSkipped, Summary: "Runs when new commits are pushed"})
		}
		sort.SliceStable(next, func(i, j int) bool { return checks.Order(next[i].Name) < checks.Order(next[j].Name) })
		r.Checks, r.Warning, r.CheckedAt = next, warning, s.now()
	})
}

// RecordBuild stores the latest CI build of a change request.
func (s *Store) RecordBuild(repo platform.Repo, cr *platform.ChangeRequest, url string, b Build) {
	if s == nil {
		return
	}
	if b.At.IsZero() {
		b.At = s.now()
	}
	if b.SHA == "" {
		b.SHA = cr.HeadSHA
	}
	if b.Report != nil {
		trimmed := *b.Report
		trimmed.Projects = append([]ci.ProjectResult(nil), b.Report.Projects...)
		for i, p := range trimmed.Projects {
			stages := append([]ci.StageResult(nil), p.Stages...)
			for j := range stages {
				stages[j].Output = tail(stages[j].Output, maxOutput)
			}
			trimmed.Projects[i].Stages = stages
		}
		b.Report = &trimmed
	}
	b.Logs = tail(b.Logs, maxOutput)
	s.update(repo, cr, url, func(r *Review, _ string) { r.Build = &b })
}

// SetState records that a change request was merged or closed.
func (s *Store) SetState(repo platform.Repo, number int, state string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.items[Key(string(repo.Platform), repo.FullName, number)]
	if !ok || r.State == state {
		return
	}
	r.State, r.UpdatedAt = state, s.now()
	s.saveLocked()
}

func (s *Store) update(repo platform.Repo, cr *platform.ChangeRequest, url string, fn func(r *Review, prevSHA string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(string(repo.Platform), repo.FullName, cr.Number)
	r, ok := s.items[key]
	if !ok {
		r = &Review{ID: id(key), State: "open"}
		s.items[key] = r
	}
	prevSHA := r.HeadSHA
	meta(r, repo, cr, url)
	fn(r, prevSHA)
	r.UpdatedAt = s.now()
	s.pruneLocked()
	s.saveLocked()
}

// List returns every review, most recently updated first.
func (s *Store) List() []Review {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Review, 0, len(s.items))
	for _, r := range s.items {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// Get finds a review by its ID.
func (s *Store) Get(id string) (Review, bool) {
	if s == nil {
		return Review{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.items {
		if r.ID == id {
			return *r, true
		}
	}
	return Review{}, false
}

// pruneLocked drops the oldest closed reviews first, then the oldest open ones.
func (s *Store) pruneLocked() {
	if len(s.items) <= s.max {
		return
	}
	keys := make([]string, 0, len(s.items))
	for k := range s.items {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := s.items[keys[i]], s.items[keys[j]]
		if (a.State == "open") != (b.State == "open") {
			return a.State != "open"
		}
		return a.UpdatedAt.Before(b.UpdatedAt)
	})
	for _, k := range keys[:len(keys)-s.max] {
		delete(s.items, k)
	}
}

// saveLocked writes the file; a failure only costs history, so it is not fatal.
func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	list := make([]*Review, 0, len(s.items))
	for _, r := range s.items {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].UpdatedAt.After(list[j].UpdatedAt) })
	data, err := json.Marshal(list)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, s.path)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < 200 {
		s = s[i+1:]
	}
	return "…\n" + s
}

// IDFor is the console ID of a change request, usable before it is recorded.
func IDFor(platform, repo string, number int) string { return id(Key(platform, repo, number)) }
