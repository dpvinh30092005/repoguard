package executor

import (
	"context"

	"github.com/dpvinh30092005/repoguard/internal/platform"
)

// Remote builds a change request on an external CI system. The remote job
// fetches the sources itself, writes Policy as .repoguard.yml, runs
// `repoguard ci --json repoguard-report.json` and keeps the report as an artifact.
type Remote interface {
	Build(ctx context.Context, req RemoteBuild) (Result, error)
}

type RemoteBuild struct {
	Repo     platform.Repo
	CR       *platform.ChangeRequest
	CloneURL string // https URL without credentials; the remote system has its own
	Ref      string // e.g. refs/merge-requests/7/head
	SHA      string // the build must check out exactly this commit
	Policy   []byte // default-branch .repoguard.yml; nil = none
}
