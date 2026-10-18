// Package secrets detects credentials in source text. It is used both on
// change-request diffs (only added lines) and by `repoguard ci` on the tree.
package secrets

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/dpvinh30092005/repoguard/internal/checks"
	"github.com/dpvinh30092005/repoguard/internal/config"
	"github.com/dpvinh30092005/repoguard/internal/diff"
	"github.com/dpvinh30092005/repoguard/internal/glob"
)

// AllowMarker on a line suppresses findings for it (for test fixtures, docs).
const AllowMarker = "repoguard:allow"

type rule struct {
	id string
	re *regexp.Regexp
	// group of re holding the secret (0 = whole match)
	group int
	// generic rules also need entropy to avoid flagging `password = "changeme"`.
	minEntropy float64
}

var rules = []rule{
	{id: "aws-access-key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{id: "github-token", re: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{60,})\b`)},
	{id: "gitlab-token", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}`)},
	{id: "slack-token", re: regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`)},
	{id: "google-api-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{id: "stripe-live-key", re: regexp.MustCompile(`\b(?:sk|rk)_live_[0-9a-zA-Z]{24,}\b`)},
	{id: "anthropic-api-key", re: regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}`)},
	{id: "private-key", re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY(?: BLOCK)?-----`)},
	{
		id:         "generic-secret",
		re:         regexp.MustCompile(`(?i)(?:password|passwd|pwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret)["']?\s*[:=]\s*["']([^"'\s]{12,})["']`),
		group:      1,
		minEntropy: 3.5,
	},
}

var placeholder = regexp.MustCompile(`(?i)(example|sample|dummy|changeme|placeholder|your[_-]|xxxx|\*\*\*\*|<[^>]+>|\$\{|\{\{|%\()`)

type Match struct {
	Rule   string
	Line   int
	Masked string
}

// ScanLine returns the secrets found in one line of text.
func ScanLine(text string) []Match {
	if strings.Contains(text, AllowMarker) {
		return nil
	}
	var out []Match
	for _, r := range rules {
		// The generic rule is a fallback: a line already flagged by a specific
		// rule (e.g. `apiKey = "AKIA..."`) is one secret, not two.
		if r.minEntropy > 0 && len(out) > 0 {
			continue
		}
		for _, m := range r.re.FindAllStringSubmatch(text, -1) {
			secret := m[r.group]
			if r.minEntropy > 0 && (entropy(secret) < r.minEntropy || placeholder.MatchString(secret)) {
				continue
			}
			out = append(out, Match{Rule: r.id, Masked: mask(secret)})
		}
	}
	return out
}

// ScanText scans a whole file; Line is 1-based.
func ScanText(text string) []Match {
	var out []Match
	for i, line := range strings.Split(text, "\n") {
		for _, m := range ScanLine(line) {
			m.Line = i + 1
			out = append(out, m)
		}
	}
	return out
}

// mask never echoes a secret back: comments are public and get indexed.
func mask(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + strings.Repeat("*", 8)
}

func entropy(s string) float64 {
	freq := map[rune]float64{}
	for _, r := range s {
		freq[r]++
	}
	var h float64
	n := float64(len([]rune(s)))
	for _, c := range freq {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}

// Check scans only the lines a change request adds.
type Check struct{}

func (Check) Name() string                 { return "secrets" }
func (Check) Enabled(cfg config.Repo) bool { return cfg.Secrets.Enabled }

func (Check) Run(_ context.Context, in checks.Input) (checks.Result, error) {
	var findings []checks.Finding
	for _, f := range in.Files {
		if f.Status == "removed" || f.Patch == "" || glob.Any(in.Config.Secrets.IgnorePaths, f.Path) {
			continue
		}
		for _, l := range diff.Parse(f.Path, f.Patch).AddedLines() {
			for _, m := range ScanLine(l.Text) {
				findings = append(findings, checks.Finding{
					Severity: checks.SeverityError,
					Path:     f.Path,
					Line:     l.NewLine,
					Message: fmt.Sprintf("Possible **%s** (`%s`). Remove it, rotate the credential — it is already in git history — "+
						"and load it from a secret manager / CI variable. False positive? add `%s` on the line.", m.Rule, m.Masked, AllowMarker),
				})
			}
		}
	}
	res := checks.Result{Check: "secrets", State: checks.StateFor(findings), Findings: findings, Summary: "No secrets in added lines"}
	if len(findings) > 0 {
		res.Summary = fmt.Sprintf("%d possible secret(s) in added lines", len(findings))
	}
	return res, nil
}

var _ checks.Check = Check{}
