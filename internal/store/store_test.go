package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenGeneratesSecretsAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "settings.json")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := st.Get()
	if first.WebhookSecret == "" || first.AdminPassword == "" || first.SessionKey == "" {
		t.Fatalf("secrets not generated: %+v", first)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("settings file mode = %v, want 0600 (it holds tokens)", info.Mode().Perm())
	}

	if err := st.Update(func(s *Settings) {
		s.GitLabToken = "glpat-x"
		s.Repos = map[string]Repo{}
		r := Repo{Platform: "gitlab", FullName: "g/app", Mode: "poll", Executor: true}
		s.Repos[r.Key()] = r
	}); err != nil {
		t.Fatal(err)
	}

	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := again.Get()
	if got.WebhookSecret != first.WebhookSecret || got.GitLabToken != "glpat-x" {
		t.Fatalf("not persisted: %+v", got)
	}
	if repos := again.Repos(); len(repos) != 1 || !repos[0].Executor {
		t.Fatalf("repos = %+v", repos)
	}
}

func TestGetReturnsACopy(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "s.json"))
	_ = st.Update(func(s *Settings) { s.Repos["k"] = Repo{FullName: "a"} })
	s := st.Get()
	s.Repos["k"] = Repo{FullName: "mutated"}
	if st.Get().Repos["k"].FullName != "a" {
		t.Fatal("Get must not expose internal state")
	}
}
