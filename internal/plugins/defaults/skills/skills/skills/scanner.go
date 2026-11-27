package skills

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/skills"
)

// Source a trusted project's own skills are scanned as, which quarantines a dangerous one
const projectLocal = "project-local"

// Scans a skill folder as coming from the source: a trusted project's skill, held back when dangerous, or one the agent wrote, whose
// write is undone unless the guard allows it. A project's skills are scanned once while their content is unchanged
func (h *Hub) Scan(ctx context.Context, cfg *config.Config, dir, source string) (skills.Verdict, error) {
	scope := h.registry.Scope(cfg, "")
	key := ""
	cache := ""
	if source == projectLocal {
		digest, err := contentDigest(dir)
		if err != nil {
			return skills.Verdict{}, err
		}
		key = source + "\x00" + resolved(dir) + "\x00" + digest
		h.mu.Lock()
		verdict, ok := h.verdicts[key]
		h.mu.Unlock()
		if ok {
			return verdict, nil
		}
		cache = filepath.Join(scope.Home(), "cache", "project_skill_scans")
	}
	result, _, err := h.run(scope, nil).scan(ctx, dir, source, "", cache)
	if err != nil {
		return skills.Verdict{}, err
	}
	decision, reason := decide(result, false)
	if decision == "ask" {
		slog.Warn("Agent-created skill blocked (dangerous findings): " + reason)
	}
	verdict := skills.Verdict{Level: result.Verdict, Allowed: decision == "allow", Reason: reason, Report: report(result)}
	if key != "" {
		h.mu.Lock()
		h.verdicts[key] = verdict
		h.mu.Unlock()
	}
	return verdict, nil
}
