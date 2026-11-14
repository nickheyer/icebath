package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	healthyPrompt = "You are Hermes.\n" +
		"<available_skills>\n  dogfood: exploratory QA of web apps\n</available_skills>\n" +
		agent.SkillsGuidance + "\n"
	degradedPrompt = "You are Hermes.\n(reduced maintenance build without the skills index)\n"
)

// Versioned tools pin carrying one function definition per name
func toolsPin(names ...string) map[string]any {
	tools := []any{}
	for _, name := range names {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": name, "description": name + " test tool", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}})
	}
	return map[string]any{"version": "test-sha", "tools": tools}
}

// Creates a telegram session with a stored prompt and an optional tools pin
func promptSession(t *testing.T, db *state.SessionDB, id, prompt string, pin map[string]any) {
	t.Helper()
	sessionsTestSeed(t, db, state.NewSession{ID: id, Source: "telegram", SystemPrompt: prompt})
	if pin != nil {
		if err := db.UpdateSessionToolNames(id, pin); err != nil {
			t.Fatal(err)
		}
	}
}

// Runs repair-prompts over the store with the given flags and stdin answer
func runRepairPrompts(t *testing.T, h string, db *state.SessionDB, sessionID any, apply, jsonOut bool, stdin string) (int, string) {
	t.Helper()
	r, out := sessionsTestRun(t, h, map[string]any{"session_id": sessionID, "apply": apply, "json": jsonOut}, stdin)
	code, err := cmdRepairPrompts(r, db)
	if err != nil {
		t.Fatal(err)
	}
	return code, out.String()
}

// Applies with JSON output, which must never prompt, and returns the decoded report
func applyJSON(t *testing.T, h string, db *state.SessionDB) map[string]any {
	t.Helper()
	code, out := runRepairPrompts(t, h, db, nil, true, true, "")
	if code != 0 || strings.Contains(out, "[y/N]") {
		t.Fatalf("--apply --json must not prompt, code %d output %s", code, out)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	return payload
}

// Ids of the rows of a report list
func reportIDs(t *testing.T, rows any) []string {
	t.Helper()
	ids := []string{}
	for _, row := range rows.([]any) {
		switch v := row.(type) {
		case string:
			ids = append(ids, v)
		case map[string]any:
			ids = append(ids, v["id"].(string))
		}
	}
	return ids
}

// Stored value of a session column
func sessionColumn(t *testing.T, db *state.SessionDB, id, column string) any {
	t.Helper()
	row, err := db.GetSession(t.Context(), id)
	if err != nil || row == nil {
		t.Fatalf("session %s %v", id, err)
	}
	return rowGet(row, column)
}

func TestUnpinnedRowIsUnverifiableAndApplyJSONClearsOnlyVerified(t *testing.T) {
	h := sessionsTestHome(t)
	db := sessionsTestDB(t, h)
	promptSession(t, db, "old-unpinned", degradedPrompt, nil)
	promptSession(t, db, "verified", degradedPrompt, toolsPin("skills_list", "skill_manage"))
	promptSession(t, db, "healthy-1", healthyPrompt, toolsPin("skills_list", "skill_manage"))
	payload := applyJSON(t, h, db)
	if payload["apply"] != true {
		t.Fatalf("payload %v", payload)
	}
	if got := reportIDs(t, payload["cleared"]); len(got) != 1 || got[0] != "verified" {
		t.Fatalf("cleared %v", got)
	}
	if got := reportIDs(t, payload["unverifiable"]); len(got) != 1 || got[0] != "old-unpinned" {
		t.Fatalf("unverifiable %v", got)
	}
	if got := sessionColumn(t, db, "old-unpinned", "system_prompt"); got != degradedPrompt {
		t.Fatalf("unverifiable prompt %v", got)
	}
	if got := sessionColumn(t, db, "verified", "system_prompt"); got != nil && got != "" {
		t.Fatalf("verified prompt %v", got)
	}
	if got, _ := sessionColumn(t, db, "verified", "tool_names").(string); got == "" {
		t.Fatal("the skill_manage pin did not survive the clear")
	}
	if got := sessionColumn(t, db, "healthy-1", "system_prompt"); got != healthyPrompt {
		t.Fatalf("healthy prompt %v", got)
	}
}

func TestScanLeavesReducedMemoryOnlyZeroSkillsRowsAndSessionIDOverrides(t *testing.T) {
	h := sessionsTestHome(t)
	db := sessionsTestDB(t, h)
	promptSession(t, db, "reduced-1", degradedPrompt, toolsPin("todo", "web_search"))
	promptSession(t, db, "memory-only-1", degradedPrompt, toolsPin("memory"))
	zeroSkills := "You are Hermes.\nNo skills are installed.\n"
	promptSession(t, db, "zero-skills-readonly", zeroSkills, toolsPin("terminal", "skills_list", "skill_view"))
	payload := applyJSON(t, h, db)
	if got := reportIDs(t, payload["cleared"]); len(got) != 0 {
		t.Fatalf("cleared %v", got)
	}
	if got := reportIDs(t, payload["unverifiable"]); len(got) != 1 || got[0] != "memory-only-1" {
		t.Fatalf("unverifiable %v", got)
	}
	for id, want := range map[string]string{"reduced-1": degradedPrompt, "memory-only-1": degradedPrompt, "zero-skills-readonly": zeroSkills} {
		if got := sessionColumn(t, db, id, "system_prompt"); got != want {
			t.Fatalf("%s prompt %v", id, got)
		}
	}
	if got, _ := sessionColumn(t, db, "memory-only-1", "tool_names").(string); got == "" {
		t.Fatal("the memory pin is gone")
	}
	if code, out := runRepairPrompts(t, h, db, "memory-only", true, false, "y\n"); code != 0 {
		t.Fatalf("override code %d output %s", code, out)
	}
	if got := sessionColumn(t, db, "memory-only-1", "system_prompt"); got != nil && got != "" {
		t.Fatalf("override left the prompt %v", got)
	}
	code, out := runRepairPrompts(t, h, db, "memory-only", true, false, "y\n")
	if code != 0 || !strings.Contains(out, "already has no stored prompt") {
		t.Fatalf("second override code %d output %s", code, out)
	}
	code, out = runRepairPrompts(t, h, db, "no-such-session", true, false, "y\n")
	if code != 1 || !strings.Contains(out, "No session matches") {
		t.Fatalf("unknown id code %d output %s", code, out)
	}
}

func TestScanLeavesAHealthyPromptStoredBeforeTheSkillGuidanceRewordings(t *testing.T) {
	h := sessionsTestHome(t)
	db := sessionsTestDB(t, h)
	legacy := "You are Hermes.\n" +
		"<available_skills>\n  dogfood: exploratory QA of web apps\n</available_skills>\n" +
		"After completing a complex task (5+ tool calls), fixing a tricky error, or discovering a non-trivial " +
		"workflow, save the approach as a skill with skill_manage so you can reuse it next time.\n\n" +
		"## Skill Safety Rule\n" +
		"A skill placeholder containing `[SKILL_PRUNED]` lost its content in context compression and is " +
		"inaccessible — reload it with skill_view(name='...') before acting on anything that depends on it.\n"
	promptSession(t, db, "legacy-healthy", legacy, toolsPin("skills_list", "skill_manage"))
	if got := reportIDs(t, applyJSON(t, h, db)["cleared"]); len(got) != 0 {
		t.Fatalf("cleared %v", got)
	}
	if got := sessionColumn(t, db, "legacy-healthy", "system_prompt"); got != legacy {
		t.Fatalf("prompt %v", got)
	}
}
