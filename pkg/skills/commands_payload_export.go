package skills

import "context"

// Loaded skill payload, its directory and resolved name, false when the skill cannot be served
func (s *Scope) LoadSkillPayload(ctx context.Context, identifier, taskID string) (map[string]any, string, string, bool) {
	return s.loadSkillPayload(ctx, identifier, taskID)
}
