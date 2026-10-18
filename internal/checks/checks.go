// Package checks defines what a change-request check is. Concrete checks live
// in sub-packages and never talk to a platform API directly: they get
// everything in Input and return a Result, which keeps them pure and testable.
package checks

import (
	"context"

	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/platform"
)

type Severity string

const (
	SeverityError   Severity = "error"   // fails the status
	SeverityWarning Severity = "warning" // shown, does not fail
	SeverityInfo    Severity = "info"
)

// Finding is one problem. Path/Line are set when it can be anchored to a diff line.
type Finding struct {
	Severity Severity
	Message  string
	Path     string
	Line     int
}

func (f Finding) Inline() bool { return f.Path != "" && f.Line > 0 }

type Result struct {
	Check    string // short name, also the status context suffix
	State    platform.State
	Summary  string // one line, <= 140 chars (GitHub status limit)
	Findings []Finding
	Details  string // optional markdown for the summary comment
}

// Input is everything a check may look at.
type Input struct {
	Repo   platform.Repo
	CR     *platform.ChangeRequest
	Files  []platform.FileChange
	Config config.Repo
}

type Check interface {
	Name() string
	// Enabled lets a check opt out based on repo config.
	Enabled(cfg config.Repo) bool
	Run(ctx context.Context, in Input) (Result, error)
}

// CodeOnly is implemented by checks that only make sense when new commits
// arrive (e.g. the paid AI review), not on title/description edits.
type CodeOnly interface {
	CodeOnly() bool
}

// StateFor returns failure if any finding is an error, success otherwise.
func StateFor(findings []Finding) platform.State {
	for _, f := range findings {
		if f.Severity == SeverityError {
			return platform.StateFailure
		}
	}
	return platform.StateSuccess
}
