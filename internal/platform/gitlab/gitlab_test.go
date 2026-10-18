package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dpvinh30092005/repoguard/internal/platform"
	"github.com/dpvinh30092005/repoguard/internal/platform/platformtest"
)

func hook(t *testing.T, event, body, token string) ([]platform.Event, error) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", strings.NewReader(body))
	r.Header.Set("X-Gitlab-Event", event)
	r.Header.Set("X-Gitlab-Token", token)
	return ParseWebhook(r, []byte(body), "tok3n")
}

const projectJSON = `"project":{"id":99,"path_with_namespace":"grp/svc","default_branch":"main"},"user":{"username":"bob"}`

func TestParseWebhook(t *testing.T) {
	cases := []struct {
		name, event, body string
		want              platform.EventType
		code              bool
		number            int
	}{
		{"mr open", "Merge Request Hook", `{"object_attributes":{"iid":5,"action":"open","last_commit":{"id":"abc"}},` + projectJSON + `}`, platform.EventCROpened, true, 5},
		{"mr push", "Merge Request Hook", `{"object_attributes":{"iid":5,"action":"update","oldrev":"111"},` + projectJSON + `}`, platform.EventCRUpdated, true, 5},
		{"mr title", "Merge Request Hook", `{"object_attributes":{"iid":5,"action":"update"},"changes":{"title":{}},` + projectJSON + `}`, platform.EventCRUpdated, false, 5},
		{"mr ready", "Merge Request Hook", `{"object_attributes":{"iid":5,"action":"update"},"changes":{"draft":{}},` + projectJSON + `}`, platform.EventCROpened, true, 5},
		{"mr labels only", "Merge Request Hook", `{"object_attributes":{"iid":5,"action":"update"},"changes":{"labels":{}},` + projectJSON + `}`, "", false, 0},
		{"mr merged", "Merge Request Hook", `{"object_attributes":{"iid":5,"action":"merge"},` + projectJSON + `}`, platform.EventCRMerged, false, 5},
		{"issue open", "Issue Hook", `{"object_attributes":{"iid":8,"action":"open"},` + projectJSON + `}`, platform.EventIssueOpened, false, 8},
		{"issue labels only", "Issue Hook", `{"object_attributes":{"iid":8,"action":"update"},"changes":{"labels":{}},` + projectJSON + `}`, "", false, 0},
		{"issue note", "Note Hook", `{"object_attributes":{"noteable_type":"Issue","note":"any update?"},"issue":{"iid":8},` + projectJSON + `}`, platform.EventIssueActivity, false, 8},
		{"system note", "Note Hook", `{"object_attributes":{"noteable_type":"Issue","system":true},"issue":{"iid":8},` + projectJSON + `}`, "", false, 0},
		{"pipeline", "Pipeline Hook", `{}`, "", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evs, err := hook(t, c.event, c.body, "tok3n")
			if err != nil {
				t.Fatal(err)
			}
			if c.want == "" {
				if len(evs) != 0 {
					t.Fatalf("expected none, got %+v", evs)
				}
				return
			}
			if len(evs) != 1 || evs[0].Type != c.want || evs[0].CodeChanged != c.code || evs[0].Number != c.number {
				t.Fatalf("got %+v", evs)
			}
			if evs[0].Repo.ID != "99" || evs[0].Repo.FullName != "grp/svc" {
				t.Fatalf("repo = %+v", evs[0].Repo)
			}
		})
	}
	if _, err := hook(t, "Issue Hook", `{}`, "nope"); err != ErrBadToken {
		t.Fatalf("bad token: %v", err)
	}
}

func TestClientRequests(t *testing.T) {
	api := platformtest.New(t).
		On("GET", "/api/v4/projects/99/repository/files/", 200, "pull_request: {}").
		On("POST", "/api/v4/projects/99/statuses/abc", 201, `{}`).
		On("POST", "/api/v4/projects/99/merge_requests/5/discussions", 201, `{}`).
		On("PUT", "/api/v4/projects/99/issues/8", 200, `{}`)
	c := New(api.URL, "glpat-test", http.DefaultClient)
	ctx := context.Background()
	repo := platform.Repo{Platform: platform.GitLab, ID: "99"}

	if _, err := c.GetFile(ctx, repo, "main", "config/.repoguard.yml"); err != nil {
		t.Fatal(err)
	}
	if got := api.Requests[0].URI; got != "/api/v4/projects/99/repository/files/config%2F.repoguard.yml/raw?ref=main" {
		t.Errorf("file URI = %s", got)
	}
	if api.Requests[0].Header.Get("PRIVATE-TOKEN") != "glpat-test" {
		t.Error("missing PRIVATE-TOKEN")
	}

	if err := c.SetStatus(ctx, repo, "abc", platform.Status{Context: "repoguard/secrets", State: platform.StateFailure}); err != nil {
		t.Fatal(err)
	}
	var st map[string]string
	_ = json.Unmarshal([]byte(api.Find("POST", "/api/v4/projects/99/statuses/abc")[0].Body), &st)
	if st["state"] != "failed" || st["name"] != "repoguard/secrets" {
		t.Errorf("status body = %v", st)
	}

	cr := &platform.ChangeRequest{Number: 5, BaseSHA: "b", StartSHA: "s", HeadSHA: "h"}
	if err := c.CreateReview(ctx, repo, cr, "", []platform.ReviewComment{{Path: "a.go", Line: 3, Body: "x"}}); err != nil {
		t.Fatal(err)
	}
	var disc struct {
		Position map[string]any `json:"position"`
	}
	_ = json.Unmarshal([]byte(api.Find("POST", "/api/v4/projects/99/merge_requests/5/discussions")[0].Body), &disc)
	if disc.Position["start_sha"] != "s" || disc.Position["new_line"] != float64(3) || disc.Position["new_path"] != "a.go" {
		t.Errorf("position = %v", disc.Position)
	}

	if err := c.AddLabels(ctx, repo, platform.IssueT(8), []string{"bug", "needs-info"}); err != nil {
		t.Fatal(err)
	}
	if body := api.Find("PUT", "/api/v4/projects/99/issues/8")[0].Body; !strings.Contains(body, `"add_labels":"bug,needs-info"`) {
		t.Errorf("labels body = %s", body)
	}
}

func TestAuthorCanPushAndGitSource(t *testing.T) {
	api := platformtest.New(t).
		On("GET", "/api/v4/projects/99/members/all/7", 200, `{"access_level":30}`).
		On("GET", "/api/v4/projects/99/members/all/8", 200, `{"access_level":20}`)
	c := New(api.URL+"/", "glpat-x", http.DefaultClient)
	repo := platform.Repo{Platform: platform.GitLab, ID: "99", FullName: "grp/svc"}
	for id, want := range map[int64]bool{7: true, 8: false, 9: false, 0: false} {
		got, err := c.AuthorCanPush(context.Background(), repo, &platform.ChangeRequest{AuthorID: id})
		if err != nil || got != want {
			t.Errorf("author %d: got %v err %v, want %v", id, got, err, want)
		}
	}
	src, _ := c.GitSource(context.Background(), repo, &platform.ChangeRequest{Number: 5})
	if src.URL != api.URL+"/grp/svc.git" || src.Ref != "refs/merge-requests/5/head" || src.Password != "glpat-x" || strings.Contains(src.URL, "glpat") {
		t.Fatalf("src = %+v", src)
	}
}

func TestMergeRequestForkDetection(t *testing.T) {
	api := platformtest.New(t).On("GET", "/api/v4/projects/99/merge_requests/5", 200,
		`{"iid":5,"state":"opened","author":{"id":7,"username":"ann"},"source_project_id":123,"target_project_id":99}`)
	cr, err := New(api.URL, "t", http.DefaultClient).GetChangeRequest(context.Background(), platform.Repo{ID: "99"}, 5)
	if err != nil || !cr.FromFork || cr.AuthorID != 7 || cr.State != "open" {
		t.Fatalf("cr=%+v err=%v", cr, err)
	}
}
