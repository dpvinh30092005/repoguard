package triage

import (
	"time"

	"github.com/example/repoguard/internal/config"
	"github.com/example/repoguard/internal/platform"
)

type StaleAction int

const (
	StaleNone StaleAction = iota
	StaleMark
	StaleClose
)

// StaleDecision decides what to do with an open issue. Activity on a stale
// issue removes the label (handled on the comment webhook), so "stale label +
// no update for DaysUntilClose" means nobody reacted to the warning.
func StaleDecision(issue platform.Issue, cfg config.Stale, now time.Time) StaleAction {
	if !cfg.Enabled {
		return StaleNone
	}
	for _, l := range cfg.ExemptLabels {
		if issue.HasLabel(l) {
			return StaleNone
		}
	}
	idle := now.Sub(issue.UpdatedAt)
	day := 24 * time.Hour
	if issue.HasLabel(cfg.Label) {
		if cfg.DaysUntilClose > 0 && idle >= time.Duration(cfg.DaysUntilClose)*day {
			return StaleClose
		}
		return StaleNone
	}
	if cfg.DaysUntilStale > 0 && idle >= time.Duration(cfg.DaysUntilStale)*day {
		return StaleMark
	}
	return StaleNone
}

// SLABreached reports a high-priority issue that nobody picked up in time.
func SLABreached(issue platform.Issue, cfg config.SLA, now time.Time) bool {
	if !cfg.Enabled || len(issue.Assignees) > 0 {
		return false
	}
	critical := false
	for _, l := range cfg.Labels {
		if issue.HasLabel(l) {
			critical = true
		}
	}
	return critical && now.Sub(issue.CreatedAt) >= time.Duration(cfg.HoursWithoutAssignee)*time.Hour
}
