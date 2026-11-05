package memory

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

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

// Tool aware memory guidance, empty unless the memory tool is loaded
func MemoryGuidanceFor(validToolNames map[string]bool, memoryEnabled, profileEnabled bool) string {
	if !validToolNames["memory"] {
		return ""
	}
	return BuildMemoryGuidance(memoryEnabled, profileEnabled, validToolNames["skill_manage"])
}

// Built in MEMORY.md and USER.md blocks plus the external provider block, gated like the provider tool injection
func MemoryParts(ctx context.Context, env *tools.Env, store *Store, memoryEnabled, profileEnabled bool, manager *MemoryManager, surface []map[string]any) []string {
	var parts []string
	if store != nil {
		for _, kind := range []struct {
			enabled bool
			target  string
		}{{memoryEnabled, "memory"}, {profileEnabled, "user"}} {
			if !kind.enabled {
				continue
			}
			if block := store.FormatForSystemPrompt(kind.target); block != "" {
				parts = append(parts, block)
			}
		}
	}
	if manager != nil && MemoryProviderToolsExposed(ctx, env, surface) {
		var block string
		if err := callProvider(func() error { block = manager.BuildSystemPrompt(ctx); return nil }); err == nil && block != "" {
			parts = append(parts, block)
		}
	}
	return parts
}

// Memory prompt blocks register nothing at import
func registerMemoryPromptBlocks() {}
