package gitlab

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/example/repoguard/internal/platform"
)

const botMarker = "<!-- repoguard"

var ErrBadToken = errors.New("invalid webhook token")

type hookPayload struct {
	ObjectKind string `json:"object_kind"`
	User       glUser `json:"user"`
	Project    struct {
		ID                int    `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
		DefaultBranch     string `json:"default_branch"`
	} `json:"project"`
	ObjectAttributes struct {
		IID          int    `json:"iid"`
		Action       string `json:"action"`
		OldRev       string `json:"oldrev"`
		NoteableType string `json:"noteable_type"`
		Note         string `json:"note"`
		System       bool   `json:"system"`
		LastCommit   struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
	Issue *struct {
		IID int `json:"iid"`
	} `json:"issue"`
	Changes map[string]json.RawMessage `json:"changes"`
}

// ParseWebhook verifies X-Gitlab-Token and normalizes Merge Request, Issue and Note hooks.
func ParseWebhook(r *http.Request, body []byte, secret string) ([]platform.Event, error) {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Gitlab-Token")), []byte(secret)) != 1 {
		return nil, ErrBadToken
	}
	kind := r.Header.Get("X-Gitlab-Event")
	if kind != "Merge Request Hook" && kind != "Issue Hook" && kind != "Note Hook" {
		return nil, nil
	}
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("decode %s: %w", kind, err)
	}
	attrs := p.ObjectAttributes
	ev := platform.Event{
		Repo: platform.Repo{
			Platform: platform.GitLab, ID: strconv.Itoa(p.Project.ID),
			FullName: p.Project.PathWithNamespace, DefaultBranch: p.Project.DefaultBranch,
		},
		Number:     attrs.IID,
		Actor:      p.User.Username,
		DeliveryID: r.Header.Get("X-Gitlab-Event-UUID"),
	}
	changed := func(fields ...string) bool {
		for _, f := range fields {
			if p.Changes[f] != nil {
				return true
			}
		}
		return false
	}

	switch kind {
	case "Merge Request Hook":
		ev.HeadSHA = attrs.LastCommit.ID
		switch attrs.Action {
		case "open", "reopen":
			ev.Type, ev.CodeChanged = platform.EventCROpened, true
		case "update":
			switch {
			case attrs.OldRev != "": // new commits pushed
				ev.Type, ev.CodeChanged = platform.EventCRUpdated, true
			case changed("draft", "work_in_progress"): // marked ready
				ev.Type, ev.CodeChanged = platform.EventCROpened, true
			case changed("title", "description"):
				ev.Type = platform.EventCRUpdated
			default:
				return nil, nil // label/assignee changes, including our own: avoid loops
			}
		case "merge":
			ev.Type = platform.EventCRMerged
		default:
			return nil, nil
		}
	case "Issue Hook":
		switch {
		case attrs.Action == "open":
			ev.Type = platform.EventIssueOpened
		case attrs.Action == "reopen", attrs.Action == "update" && changed("title", "description"):
			ev.Type = platform.EventIssueEdited
		default:
			return nil, nil
		}
	case "Note Hook":
		if attrs.NoteableType != "Issue" || attrs.System || p.Issue == nil || strings.Contains(attrs.Note, botMarker) {
			return nil, nil
		}
		ev.Type, ev.Number = platform.EventIssueActivity, p.Issue.IID
	}
	return []platform.Event{ev}, nil
}
