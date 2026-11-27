package skills

import (
	"context"
	"log/slog"
	"path/filepath"
)

// Source a trusted project's skills are scanned as
const projectSource = "project-local"

// Whether a trusted project's skill is held back: scanned as dangerous, or the scan failed, which holds it back too. Without a scanner
// the user's trust in the project is what lets its skills load
func (s *Scope) quarantined(skillMD string) bool {
	scanner, ok := capability[Scanner](s.registry)
	if !ok {
		return false
	}
	dir := filepath.Dir(skillMD)
	verdict, err := scanner.Scan(context.Background(), s.Config, dir, projectSource)
	switch {
	case err != nil:
		if s.registry.first("scan failed\x00" + dir) {
			slog.Warn("skills: a project skill could not be scanned, so it is held back", "skill", dir, "err", err)
		}
		return true
	case verdict.Level == "dangerous":
		if s.registry.first("quarantined\x00" + dir) {
			slog.Warn("skills: a project skill scanned as dangerous is held back", "skill", dir, "reason", verdict.Reason)
		}
		return true
	}
	return false
}

// The SKILL.md files of a trusted project that are not held back
func (s *Scope) unquarantined(files []string) []string {
	var out []string
	for _, skillMD := range files {
		if !s.quarantined(skillMD) {
			out = append(out, skillMD)
		}
	}
	return out
}
