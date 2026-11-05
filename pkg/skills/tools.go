package skills

import (
	"context"
	"encoding/json"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Schema of the skills_list tool
func SkillsListSchema() map[string]any {
	return map[string]any{
		"name":        "skills_list",
		"description": "List available skills (name + description). Use skill_view(name) to load full content.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"category": map[string]any{"type": "string", "description": "Optional category filter to narrow results"},
			},
			"required": []any{},
		},
	}
}

// Schema of the skill_view tool
func SkillViewSchema() map[string]any {
	return map[string]any{
		"name":        "skill_view",
		"description": "Load a skill's SKILL.md, or one of its linked references, templates or scripts. The first call returns SKILL.md plus a 'linked_files' dict; call again with file_path to read one.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":      map[string]any{"type": "string", "description": "The skill name (use skills_list to see available skills). For plugin-provided skills, use the qualified form 'plugin:skill'."},
				"file_path": map[string]any{"type": "string", "description": "OPTIONAL: Path to a linked file within the skill (e.g., 'references/api.md', 'templates/config.yaml', 'scripts/validate.py'). Omit to get the main SKILL.md content."},
			},
			"required": []any{"name"},
		},
	}
}

// Skills tools are always available because the directory is created on first use
func checkSkillsRequirements(*tools.Env) bool { return true }

// String argument of a call, empty when absent or not a string
func argString(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// Lists skills for the model
func skillsListHandler(ctx context.Context, call *tools.Call) string {
	return ScopeFromEnv(call.Env).SkillsList(ctx, argString(call.Args, "category"))
}

// Views a skill, serving the unchanged stub on a repeat and bumping view and use counters on success
func skillViewHandler(ctx context.Context, call *tools.Call) string {
	if v, ok := call.Args["name"]; ok {
		if _, isStr := v.(string); !isStr {
			return failResult("Skill name must be a string.", map[string]any{"hint": lookupHint})
		}
	}
	name := argString(call.Args, "name")
	filePath := argString(call.Args, "file_path")
	dedupTask := call.TaskID
	if IsBackgroundReview(call.Env) {
		dedupTask = ""
	}
	scope := ScopeFromEnv(call.Env)
	result := scope.SkillView(ctx, name, filePath, call.TaskID, true)
	parsed, ok := decodeObject(result)
	if !ok || !fmTruthy(parsed["success"]) {
		return result
	}
	if stub := dedupSkillView(dedupTask, name, filePath, parsed, result); stub != "" {
		return stub
	}
	resolved := name
	if v := parsed["name"]; fmTruthy(v) {
		resolved = pyStrValue(v)
	}
	if resolved != "" {
		scope.BumpView(ctx, resolved)
		scope.BumpUse(ctx, resolved, call.TaskID, call.SessionID)
	}
	return result
}

// JSON object of a tool result, false when the result is not an object
func decodeObject(result string) (map[string]any, bool) {
	var out map[string]any
	if err := json.Unmarshal([]byte(result), &out); err != nil || out == nil {
		return nil, false
	}
	return out, true
}

func init() {
	tools.Register(tools.Def{Name: "skills_list", Toolset: "skills", Schema: SkillsListSchema(), Handler: skillsListHandler, Check: checkSkillsRequirements, Emoji: "📚"})
	tools.Register(tools.Def{Name: "skill_view", Toolset: "skills", Schema: SkillViewSchema(), Handler: skillViewHandler, Check: checkSkillsRequirements, Emoji: "📚"})
}
