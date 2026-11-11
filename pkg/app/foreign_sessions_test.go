package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func foreignJSONLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func foreignClaudeLines(t *testing.T) []string {
	msg := func(role string, content any) map[string]any {
		return map[string]any{"type": role, "sessionId": "abc-123", "cwd": "/home/user/proj",
			"message": map[string]any{"role": role, "content": content}}
	}
	entries := []any{
		map[string]any{"type": "summary", "summary": "Fix the flaky test"},
		msg("user", "Please fix the flaky test in CI."),
		msg("assistant", []any{map[string]any{"type": "text", "text": "Looking into it now."},
			map[string]any{"type": "tool_use", "name": "Bash", "id": "t1", "input": map[string]any{}}}),
		msg("user", []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}}),
		msg("assistant", []any{map[string]any{"type": "text", "text": "Fixed — the sleep was too short."}}),
		msg("user", "Great, thanks!"),
		msg("assistant", []any{map[string]any{"type": "text", "text": "Anytime."}}),
	}
	var lines []string
	for _, e := range entries {
		lines = append(lines, foreignJSONLine(t, e))
	}
	return lines
}

func writeForeignFixture(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, name)
	if err := os.WriteFile(f, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func writeClaudeFixture(t *testing.T, tmp string, extra []string) string {
	return writeForeignFixture(t, filepath.Join(tmp, ".claude", "projects", "-home-user-proj"), "abc-123.jsonl",
		append(extra, foreignClaudeLines(t)...))
}

func foreignCodexLines(t *testing.T) []string {
	item := func(payload map[string]any) map[string]any {
		return map[string]any{"timestamp": "2026-08-15T21:35:28Z", "type": "response_item", "payload": payload}
	}
	message := func(role, kind, text string) map[string]any {
		return item(map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": kind, "text": text}}})
	}
	entries := []any{
		map[string]any{"type": "session_meta", "payload": map[string]any{"session_id": "0000-1111", "cwd": "/home/user/repo"}},
		message("developer", "input_text", "<skills_instructions>secret system stuff"),
		message("user", "input_text", "<recommended_plugins>\ninjected wrapper"),
		message("user", "input_text", "Summarize the transcripts please."),
		message("assistant", "output_text", "Reading them one at a time."),
		item(map[string]any{"type": "custom_tool_call", "name": "shell", "call_id": "c1"}),
		item(map[string]any{"type": "custom_tool_call_output", "call_id": "c1", "output": "big output"}),
		message("assistant", "output_text", "Done — here is the summary."),
		message("user", "input_text", "Now write it to a file."),
		message("assistant", "output_text", "Written to summary.md."),
	}
	var lines []string
	for _, e := range entries {
		lines = append(lines, foreignJSONLine(t, e))
	}
	return lines
}

func writeCodexFixture(t *testing.T, tmp string, extra []string) string {
	return writeForeignFixture(t, filepath.Join(tmp, ".codex", "sessions", "2026", "08", "15"),
		"rollout-2026-08-15T21-35-28-0000-1111.jsonl", append(extra, foreignCodexLines(t)...))
}

func assertForeignAlternating(t *testing.T, turns []foreignTurn) {
	t.Helper()
	if len(turns) == 0 {
		t.Fatal("no messages")
	}
	if turns[0].role != "user" {
		t.Fatalf("first role %q", turns[0].role)
	}
	for i := 1; i < len(turns); i++ {
		if turns[i].role == turns[i-1].role {
			t.Fatalf("two consecutive %s messages", turns[i].role)
		}
	}
}

func TestParseClaudeSession(t *testing.T) {
	parsed := parseClaudeSession(writeClaudeFixture(t, t.TempDir(), nil))
	assertForeignAlternating(t, parsed.turns)
	if len(parsed.turns) != 4 {
		t.Fatalf("turns %d", len(parsed.turns))
	}
	if *parsed.cwd != "/home/user/proj" || *parsed.titleGuess != "Fix the flaky test" || *parsed.sessionID != "abc-123" {
		t.Fatalf("meta %q %q %q", *parsed.cwd, *parsed.titleGuess, *parsed.sessionID)
	}
	var assistant []string
	for _, turn := range parsed.turns {
		if turn.role == "assistant" {
			assistant = append(assistant, turn.content)
		}
	}
	if !strings.Contains(strings.Join(assistant, "\n"), "[ran tool: Bash]") {
		t.Fatalf("assistant text %q", assistant)
	}
}

func TestParseCodexSession(t *testing.T) {
	parsed := parseCodexSession(writeCodexFixture(t, t.TempDir(), nil))
	assertForeignAlternating(t, parsed.turns)
	if len(parsed.turns) != 4 || *parsed.cwd != "/home/user/repo" || *parsed.sessionID != "0000-1111" {
		t.Fatalf("parsed %d %q %q", len(parsed.turns), *parsed.cwd, *parsed.sessionID)
	}
	if !strings.HasPrefix(*parsed.titleGuess, "Summarize the transcripts") {
		t.Fatalf("title %q", *parsed.titleGuess)
	}
	var all []string
	for _, turn := range parsed.turns {
		all = append(all, turn.content)
	}
	text := strings.Join(all, "\n")
	if strings.Contains(text, "skills_instructions") || strings.Contains(text, "recommended_plugins") ||
		!strings.Contains(text, "[ran tool: shell]") {
		t.Fatalf("text %q", text)
	}
}

func TestMalformedLinesAreSkipped(t *testing.T) {
	tmp := t.TempDir()
	garbage := []string{"this is not json at all {{{", `"just a string"`,
		foreignJSONLine(t, map[string]any{"type": "user", "message": "not-a-dict-payload... wait, string"}),
		foreignJSONLine(t, map[string]any{"type": "user"}), ""}
	if n := len(parseClaudeSession(writeClaudeFixture(t, tmp, garbage)).turns); n != 4 {
		t.Fatalf("claude turns %d", n)
	}
	g := writeCodexFixture(t, tmp, []string{"not json", foreignJSONLine(t, map[string]any{"type": "response_item"})})
	if n := len(parseCodexSession(g).turns); n != 4 {
		t.Fatalf("codex turns %d", n)
	}
}

func TestListSessions(t *testing.T) {
	tmp := t.TempDir()
	writeClaudeFixture(t, tmp, nil)
	writeCodexFixture(t, tmp, nil)
	claudeRoot, codexRoot := filepath.Join(tmp, ".claude", "projects"), filepath.Join(tmp, ".codex", "sessions")
	claude, err := foreignListSessions("claude", claudeRoot)
	if err != nil {
		t.Fatal(err)
	}
	codex, err := foreignListSessions("codex", codexRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(claude) != 1 || claude[0].Source != "claude" || claude[0].TurnCount != 4 {
		t.Fatalf("claude %+v", claude)
	}
	if len(codex) != 1 || codex[0].Source != "codex" || codex[0].TurnCount != 4 {
		t.Fatalf("codex %+v", codex)
	}
	both, err := gatherForeignSessions("", claudeRoot, codexRoot, 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 2 || both[0].Mtime < both[1].Mtime {
		t.Fatalf("both %+v", both)
	}
}

func TestListSessionsMissingRoots(t *testing.T) {
	tmp := t.TempDir()
	for _, source := range []string{"claude", "codex"} {
		listed, err := foreignListSessions(source, filepath.Join(tmp, "nope"))
		if err != nil || len(listed) != 0 {
			t.Fatalf("%s %v %v", source, listed, err)
		}
	}
}

func foreignSourceSet(t *testing.T) map[string]bool {
	t.Helper()
	both, err := gatherForeignSessions("", "", "", 25)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, s := range both {
		set[s.Source] = true
	}
	return set
}

func TestEnvOverridesRelocateDefaultRoots(t *testing.T) {
	tmp := t.TempDir()
	claudeCfg, codexHome := filepath.Join(tmp, "relocated-claude"), filepath.Join(tmp, "relocated-codex")
	writeClaudeFixture(t, tmp, nil)
	if err := os.Rename(filepath.Join(tmp, ".claude"), claudeCfg); err != nil {
		t.Fatal(err)
	}
	writeCodexFixture(t, tmp, nil)
	if err := os.Rename(filepath.Join(tmp, ".codex"), codexHome); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeCfg)
	t.Setenv("CODEX_HOME", codexHome)
	if set := foreignSourceSet(t); len(set) != 2 || !set["claude"] || !set["codex"] {
		t.Fatalf("sources %v", set)
	}
}

func TestBlankEnvOverridesFallBackToHome(t *testing.T) {
	tmp := t.TempDir()
	writeClaudeFixture(t, tmp, nil)
	writeCodexFixture(t, tmp, nil)
	t.Setenv("HOME", tmp)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "   ")
	if set := foreignSourceSet(t); len(set) != 2 || !set["claude"] || !set["codex"] {
		t.Fatalf("sources %v", set)
	}
}

type foreignRecordingStore struct{ calls [][2]string }

func (s *foreignRecordingStore) Call(_ context.Context, method string, params map[string]any) (any, error) {
	s.calls = append(s.calls, [2]string{method, params["operation"].(string)})
	return nil, nil
}

func TestImportRejectsBadInput(t *testing.T) {
	tmp := t.TempDir()
	store := &foreignRecordingStore{}
	ctx := context.Background()
	cases := []struct{ source, path, want string }{
		{"gemini", filepath.Join(tmp, "x.jsonl"), "Unknown foreign session source"},
		{"claude", filepath.Join(tmp, "missing.jsonl"), "not found"},
	}
	empty := filepath.Join(tmp, "empty.jsonl")
	if err := os.WriteFile(empty, []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases = append(cases, struct{ source, path, want string }{"claude", empty, "No user/assistant conversation"})
	for _, c := range cases {
		_, err := importForeignSession(ctx, store, c.source, c.path)
		if pyrepr.ExceptionName(err) != "ValueError" || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v", c.source, err)
		}
	}
	if len(store.calls) != 0 {
		t.Fatalf("calls %v", store.calls)
	}
}

func TestLeadingAssistantGetsSingleStub(t *testing.T) {
	line := foreignJSONLine(t, map[string]any{"type": "response_item", "payload": map[string]any{"type": "message",
		"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Continuing from before."}}}})
	f := writeForeignFixture(t, filepath.Join(t.TempDir(), ".codex", "sessions", "2026", "01", "01"), "rollout-x.jsonl", []string{line})
	parsed := parseCodexSession(f)
	assertForeignAlternating(t, parsed.turns)
	if len(parsed.turns) != 2 || parsed.turns[0].role != "user" {
		t.Fatalf("turns %+v", parsed.turns)
	}
}

func TestWhitespaceOnlyUserTurnDoesNotBreakDiscovery(t *testing.T) {
	project := filepath.Join(t.TempDir(), ".claude", "projects", "p")
	lines := []string{
		foreignJSONLine(t, map[string]any{"type": "user", "sessionId": "w", "message": map[string]any{"role": "user", "content": "   \n  "}}),
		foreignJSONLine(t, map[string]any{"type": "assistant", "sessionId": "w", "message": map[string]any{"role": "assistant", "content": "hi"}}),
		foreignJSONLine(t, map[string]any{"type": "user", "sessionId": "w", "message": map[string]any{"role": "user", "content": "real question"}}),
	}
	writeForeignFixture(t, project, "blank.jsonl", lines)
	listed, err := foreignListSessions("claude", filepath.Dir(project))
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || *listed[0].TitleGuess != "real question" || listed[0].TurnCount != 3 {
		t.Fatalf("listed %+v", listed)
	}
}
