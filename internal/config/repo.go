// Package config holds the per-repository policy (.repoguard.yml) and the
// service settings (environment variables).
package config

import (
	"bytes"
	"fmt"
	"regexp"

	"gopkg.in/yaml.v3"
)

// FileName is read from the repository's DEFAULT branch, never from the change
// request head — otherwise a PR could simply switch its own checks off.
const FileName = ".repoguard.yml"

type Repo struct {
	PullRequest PullRequest `yaml:"pull_request"`
	Secrets     Secrets     `yaml:"secrets"`
	AIReview    AIReview    `yaml:"ai_review"`
	Issues      Issues      `yaml:"issues"`
	CI          CI          `yaml:"ci"`
}

type PullRequest struct {
	Enabled              bool   `yaml:"enabled"`
	SkipDrafts           bool   `yaml:"skip_drafts"`
	TitlePattern         string `yaml:"title_pattern"`
	RequireDescription   bool   `yaml:"require_description"`
	MinDescriptionLength int    `yaml:"min_description_length"`
	RequireLinkedIssue   bool   `yaml:"require_linked_issue"`
	MaxChangedLines      int    `yaml:"max_changed_lines"` // 0 = unlimited; exceeding is a warning
	MaxChangedFiles      int    `yaml:"max_changed_files"`
}

type Secrets struct {
	Enabled     bool     `yaml:"enabled"`
	IgnorePaths []string `yaml:"ignore_paths"`
}

type AIReview struct {
	Enabled        bool     `yaml:"enabled"`
	MaxComments    int      `yaml:"max_comments"`
	MaxDiffBytes   int      `yaml:"max_diff_bytes"`
	IgnorePaths    []string `yaml:"ignore_paths"`
	Instructions   string   `yaml:"instructions"`     // repo-specific review guidance
	FailOnCritical bool     `yaml:"fail_on_critical"` // AI is advisory by default
}

type Issues struct {
	Enabled          bool                `yaml:"enabled"`
	BugLabels        []string            `yaml:"bug_labels"`
	RequiredSections []string            `yaml:"required_sections"`
	NeedsInfoLabel   string              `yaml:"needs_info_label"`
	AutoLabels       map[string][]string `yaml:"auto_labels"` // label -> keywords
	Duplicates       Duplicates          `yaml:"duplicates"`
	Stale            Stale               `yaml:"stale"`
	SLA              SLA                 `yaml:"sla"`
	Link             Link                `yaml:"link"`
}

type Duplicates struct {
	Enabled   bool    `yaml:"enabled"`
	Threshold float64 `yaml:"threshold"` // cosine similarity 0..1
	Label     string  `yaml:"label"`
}

type Stale struct {
	Enabled        bool     `yaml:"enabled"`
	DaysUntilStale int      `yaml:"days_until_stale"`
	DaysUntilClose int      `yaml:"days_until_close"` // 0 = never close
	Label          string   `yaml:"label"`
	ExemptLabels   []string `yaml:"exempt_labels"`
}

type SLA struct {
	Enabled              bool     `yaml:"enabled"`
	Labels               []string `yaml:"labels"`
	HoursWithoutAssignee int      `yaml:"hours_without_assignee"`
}

type Link struct {
	Enabled         bool   `yaml:"enabled"`
	InProgressLabel string `yaml:"in_progress_label"`
	CloseOnMerge    bool   `yaml:"close_on_merge"`
}

// CI configures `repoguard ci` (the part that runs inside the repo's own pipeline).
type CI struct {
	Projects    []CIProject `yaml:"projects"` // empty = auto-detect
	FailOnVulns bool        `yaml:"fail_on_vulns"`
	SecretsScan bool        `yaml:"secrets_scan"`
	MaxDepth    int         `yaml:"max_depth"`
	IgnorePaths []string    `yaml:"ignore_paths"`
}

type CIProject struct {
	Path     string            `yaml:"path"`
	Type     string            `yaml:"type"` // go | node | python | custom
	Commands map[string]string `yaml:"commands"`
}

// Default returns the policy used when a repository has no .repoguard.yml.
func Default() Repo {
	return Repo{
		PullRequest: PullRequest{
			Enabled:              true,
			SkipDrafts:           true,
			TitlePattern:         `^(feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert)(\([\w./-]+\))?!?: .{3,}`,
			RequireDescription:   true,
			MinDescriptionLength: 20,
			RequireLinkedIssue:   true,
			MaxChangedLines:      600,
			MaxChangedFiles:      40,
		},
		Secrets: Secrets{Enabled: true, IgnorePaths: []string{"**/testdata/**"}},
		AIReview: AIReview{
			Enabled:      true,
			MaxComments:  15,
			MaxDiffBytes: 300_000,
			IgnorePaths: []string{
				"*.lock", "package-lock.json", "pnpm-lock.yaml", "go.sum",
				"vendor/**", "node_modules/**", "dist/**", "*.min.js", "*.snap",
			},
		},
		Issues: Issues{
			Enabled:          true,
			BugLabels:        []string{"bug"},
			RequiredSections: []string{"Steps to reproduce", "Expected", "Actual"},
			NeedsInfoLabel:   "needs-info",
			AutoLabels: map[string][]string{
				"bug":         {"bug", "crash", "error", "exception", "broken", "fails", "lỗi"},
				"enhancement": {"feature request", "would be nice", "support for", "add option", "tính năng"},
				"question":    {"how do i", "how to", "question", "is it possible"},
			},
			Duplicates: Duplicates{Enabled: true, Threshold: 0.55, Label: "possible-duplicate"},
			Stale: Stale{
				Enabled: true, DaysUntilStale: 60, DaysUntilClose: 14, Label: "stale",
				ExemptLabels: []string{"pinned", "security", "roadmap"},
			},
			SLA:  SLA{Enabled: true, Labels: []string{"critical", "priority/critical", "P0"}, HoursWithoutAssignee: 24},
			Link: Link{Enabled: true, InProgressLabel: "in-progress", CloseOnMerge: true},
		},
		CI: CI{FailOnVulns: true, SecretsScan: true, MaxDepth: 3, IgnorePaths: []string{"**/testdata/**"}},
	}
}

// ParseRepo overlays the YAML on top of Default(): a repo only states what differs.
func ParseRepo(data []byte) (Repo, error) {
	cfg := Default()
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // typos fail loudly instead of being silently ignored
	if err := dec.Decode(&cfg); err != nil {
		return Default(), fmt.Errorf("%s: %w", FileName, err)
	}
	if err := cfg.validate(); err != nil {
		return Default(), fmt.Errorf("%s: %w", FileName, err)
	}
	return cfg, nil
}

func (c Repo) validate() error {
	if c.PullRequest.TitlePattern != "" {
		if _, err := regexp.Compile(c.PullRequest.TitlePattern); err != nil {
			return fmt.Errorf("pull_request.title_pattern: %w", err)
		}
	}
	if t := c.Issues.Duplicates.Threshold; t <= 0 || t > 1 {
		return fmt.Errorf("issues.duplicates.threshold must be in (0, 1], got %v", t)
	}
	for _, p := range c.CI.Projects {
		switch p.Type {
		case "go", "node", "python", "custom":
		default:
			return fmt.Errorf("ci.projects[%s].type %q: want go|node|python|custom", p.Path, p.Type)
		}
	}
	return nil
}
