package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/dpvinh30092005/repoguard/internal/platform"
)

// ErrStale means the change request moved on while we were fetching; the
// newer push has its own job, so this one simply stops.
var ErrStale = fmt.Errorf("change request head moved")

// fetch checks out exactly src.Ref at wantSHA into a new temp directory.
// Credentials travel in GIT_CONFIG_* environment variables — never in argv
// (visible in `ps`) or in .git/config (which is copied into the sandbox).
func fetch(ctx context.Context, src platform.GitSource, wantSHA string) (string, error) {
	dir, err := os.MkdirTemp("", "repoguard-src-")
	if err != nil {
		return "", err
	}
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if src.Password != "" {
		basic := base64.StdEncoding.EncodeToString([]byte(src.Username + ":" + src.Password))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic,
		)
	}
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
		cmd.Env = env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		return strings.TrimSpace(out.String()), err
	}
	steps := [][]string{
		{"init", "--quiet"},
		{"fetch", "--quiet", "--depth", "1", "--no-tags", src.URL, src.Ref},
		{"checkout", "--quiet", "--detach", "FETCH_HEAD"},
	}
	for _, s := range steps {
		if out, err := git(s...); err != nil {
			os.RemoveAll(dir)
			return "", fmt.Errorf("git %s: %v: %s", s[0], err, redact(out, src.Password))
		}
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	if wantSHA != "" && head != wantSHA {
		os.RemoveAll(dir)
		return "", ErrStale
	}
	return dir, nil
}

func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "****")
}
