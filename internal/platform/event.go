package platform

import "fmt"

type EventType string

const (
	EventCROpened      EventType = "change_request.opened"  // opened, reopened, ready for review
	EventCRUpdated     EventType = "change_request.updated" // new commits or edited title/body
	EventCRMerged      EventType = "change_request.merged"
	EventIssueOpened   EventType = "issue.opened"
	EventIssueEdited   EventType = "issue.edited"
	EventIssueActivity EventType = "issue.activity" // a human commented
)

// Event is a normalized webhook event. Events are hints: handlers re-read
// current state from the API instead of trusting a possibly stale payload.
type Event struct {
	Type        EventType
	Repo        Repo
	Number      int // change request or issue number
	HeadSHA     string
	CodeChanged bool // CR events: new commits were pushed (vs. only title/body edits)
	Actor       string
	DeliveryID  string
}

// Key identifies equivalent work so duplicate deliveries collapse into one job.
func (e Event) Key() string {
	return fmt.Sprintf("%s|%s|%s|%d|%s|%t", e.Repo.Platform, e.Repo.ID, e.Type, e.Number, e.HeadSHA, e.CodeChanged)
}
