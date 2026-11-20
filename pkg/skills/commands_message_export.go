package skills

import "context"

// Skill payload message for a loaded skill, the agent lane's slash invoked skill path calls it
func (s *Scope) BuildSkillMessage(ctx context.Context, loaded map[string]any, skillDir, activationNote, userInstruction, runtimeNote, sessionID string) string {
	return s.buildSkillMessage(ctx, loaded, skillDir, activationNote, userInstruction, runtimeNote, sessionID)
}
