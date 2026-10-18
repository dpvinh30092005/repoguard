package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dpvinh30092005/repoguard/internal/platform"
)

// BotMarker is embedded in every comment the bot writes; used to ignore our own activity.
const BotMarker = "<!-- repoguard"

var ErrBadSignature = errors.New("invalid webhook signature")

// VerifySignature checks X-Hub-Signature-256 (HMAC-SHA256 of the raw body).
func VerifySignature(secret, signature string, body []byte) error {
	sig, ok := strings.CutPrefix(signature, "sha256=")
	if !ok {
		return ErrBadSignature
	}
	want, err := hex.DecodeString(sig)
	if err != nil {
		return ErrBadSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(want, mac.Sum(nil)) { // constant time
		return ErrBadSignature
	}
	return nil
}

type hookPayload struct {
	Action     string `json:"action"`
	Repository struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Sender      ghUser `json:"sender"`
	PullRequest *struct {
		Number int                  `json:"number"`
		Merged bool                 `json:"merged"`
		Head   struct{ SHA string } `json:"head"`
	} `json:"pull_request"`
	Issue *struct {
		Number      int       `json:"number"`
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Comment *struct {
		Body string `json:"body"`
		User ghUser `json:"user"`
	} `json:"comment"`
	Changes map[string]json.RawMessage `json:"changes"`
}

// ParseWebhook verifies and normalizes a GitHub webhook. It returns no events
// (and no error) for deliveries we don't act on.
func ParseWebhook(r *http.Request, body []byte, secret string) ([]platform.Event, error) {
	if err := VerifySignature(secret, r.Header.Get("X-Hub-Signature-256"), body); err != nil {
		return nil, err
	}
	kind := r.Header.Get("X-GitHub-Event")
	if kind != "pull_request" && kind != "issues" && kind != "issue_comment" {
		return nil, nil // ping, installation, push, ...
	}
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("decode %s payload: %w", kind, err)
	}
	ev := platform.Event{
		Repo: platform.Repo{
			Platform: platform.GitHub, ID: p.Repository.FullName, FullName: p.Repository.FullName,
			DefaultBranch: p.Repository.DefaultBranch, InstallationID: p.Installation.ID,
		},
		Actor:      p.Sender.Login,
		DeliveryID: r.Header.Get("X-GitHub-Delivery"),
	}

	switch kind {
	case "pull_request":
		if p.PullRequest == nil {
			return nil, nil
		}
		ev.Number, ev.HeadSHA = p.PullRequest.Number, p.PullRequest.Head.SHA
		switch p.Action {
		case "opened", "reopened", "ready_for_review":
			ev.Type, ev.CodeChanged = platform.EventCROpened, true
		case "synchronize":
			ev.Type, ev.CodeChanged = platform.EventCRUpdated, true
		case "edited":
			if p.Changes["title"] == nil && p.Changes["body"] == nil {
				return nil, nil
			}
			ev.Type = platform.EventCRUpdated
		case "closed":
			if !p.PullRequest.Merged {
				return nil, nil
			}
			ev.Type = platform.EventCRMerged
		default:
			return nil, nil
		}
	case "issues":
		if p.Issue == nil {
			return nil, nil
		}
		ev.Number = p.Issue.Number
		switch p.Action {
		case "opened":
			ev.Type = platform.EventIssueOpened
		case "edited", "reopened":
			ev.Type = platform.EventIssueEdited
		default:
			return nil, nil
		}
	case "issue_comment":
		if p.Action != "created" || p.Issue == nil || p.Issue.PullRequest != nil || p.Comment == nil {
			return nil, nil
		}
		if p.Comment.User.Type == "Bot" || strings.Contains(p.Comment.Body, BotMarker) {
			return nil, nil
		}
		ev.Type, ev.Number = platform.EventIssueActivity, p.Issue.Number
	}
	return []platform.Event{ev}, nil
}
