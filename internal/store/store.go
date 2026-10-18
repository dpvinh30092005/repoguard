// Package store persists what operators configure in the web UI: credentials,
// connected repositories and generated secrets. It is a single JSON file
// (written atomically, mode 0600) — enough for one repoguard instance.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type Settings struct {
	GitLabURL     string `json:"gitlab_url,omitempty"`
	GitLabToken   string `json:"gitlab_token,omitempty"`
	GitHubAPIURL  string `json:"github_api_url,omitempty"`
	GitHubToken   string `json:"github_token,omitempty"`
	AnthropicKey  string `json:"anthropic_key,omitempty"`
	PublicURL     string `json:"public_url,omitempty"`
	ExecutorImage string `json:"executor_image,omitempty"`

	// Generated once on first start.
	WebhookSecret string `json:"webhook_secret"`
	AdminPassword string `json:"admin_password"`
	SessionKey    string `json:"session_key"`

	Repos map[string]Repo `json:"repos,omitempty"` // key: Repo.Key()
}

// Repo is a repository connected through the UI.
type Repo struct {
	Platform       string `json:"platform"`
	ID             string `json:"id"`
	FullName       string `json:"full_name"`
	DefaultBranch  string `json:"default_branch"`
	InstallationID int64  `json:"installation_id,omitempty"`
	Mode           string `json:"mode"` // poll | webhook
	HookID         string `json:"hook_id,omitempty"`
	Executor       bool   `json:"executor"`
}

func (r Repo) Key() string { return Key(r.Platform, r.FullName) }

func Key(platform, fullName string) string { return platform + ":" + fullName }

type Store struct {
	path string
	mu   sync.RWMutex
	s    Settings
}

// Open loads (or creates) the settings file and fills in generated secrets.
func Open(path string) (*Store, error) {
	st := &Store{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &st.s); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return st, st.Update(func(s *Settings) {
		if s.WebhookSecret == "" {
			s.WebhookSecret = random(24)
		}
		if s.AdminPassword == "" {
			s.AdminPassword = random(8)
		}
		if s.SessionKey == "" {
			s.SessionKey = random(32)
		}
	})
}

// Get returns a copy of the settings.
func (st *Store) Get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	c := st.s
	c.Repos = make(map[string]Repo, len(st.s.Repos))
	for k, v := range st.s.Repos {
		c.Repos[k] = v
	}
	return c
}

// Update applies fn and persists the result atomically.
func (st *Store) Update(fn func(*Settings)) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	next := st.s
	next.Repos = make(map[string]Repo, len(st.s.Repos))
	for k, v := range st.s.Repos {
		next.Repos[k] = v
	}
	fn(&next)
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return err
	}
	st.s = next
	return nil
}

// Repos returns connected repositories sorted by name.
func (st *Store) Repos() []Repo {
	s := st.Get()
	out := make([]Repo, 0, len(s.Repos))
	for _, r := range s.Repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
