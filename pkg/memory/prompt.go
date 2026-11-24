package memory

// Store and skill write guidance adapted to the enabled stores without widening what belongs in memory
func BuildMemoryGuidance(memoryEnabled, profileEnabled, skillManageAvailable bool) string {
	if !memoryEnabled && !profileEnabled {
		return ""
	}
	frame := "You have a persistent user profile, carried across sessions and " +
		"loaded into each new session's context; save durable facts about the user with the " +
		"memory tool (target='user') — the built-in notes store is disabled, so never target='memory'. "
	if memoryEnabled {
		frame = "You have persistent memory, carried across sessions and loaded " +
			"into each new session's context; the memory tool's schema defines what belongs there. "
	}
	skillRouting := "Task-specific knowledge — procedures, pitfalls, and the user's preferences " +
		"and corrections for that kind of work — belongs in skills, not in memory, " +
		"even when skill writing is unavailable. "
	if skillManageAvailable {
		skillRouting = "Skills come first: when you learn something while doing a task — a " +
			"procedure, a pitfall, and the user's preferences and corrections " +
			"for that kind of work — record it in the skill you used or built " +
			"for the task (skill_manage), where it loads only when relevant. "
	}
	return frame + skillRouting + "Memory is the narrow exception for facts that apply to EVERY " +
		"session regardless of task (who the user is, environment facts, " +
		"standing conventions with no task home); it has a hard character " +
		"budget, so when it fills, replace or consolidate stale entries " +
		"rather than skipping the save. Write entries as declarative facts, " +
		"not instructions to yourself: 'User prefers concise responses' ✓ — " +
		"'Always respond concisely' ✗ (imperative phrasing gets re-read as " +
		"a directive in later sessions and can override the user's current " +
		"request). A fact stale within a week belongs in session history; " +
		"procedures and workflows belong in skills."
}

// Full guidance with both stores and skill writing available
var MemoryGuidance = BuildMemoryGuidance(true, true, true)

// Guidance for a session whose only store is the user profile
var UserProfileGuidance = BuildMemoryGuidance(false, true, true)
