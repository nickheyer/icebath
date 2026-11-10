package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
)

type fakeSandbox struct {
	host     bool
	temp     string
	results  []*ExecResult
	errors   []error
	commands []string
	stdins   []string
}

func (f *fakeSandbox) Execute(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error) {
	f.commands = append(f.commands, command)
	stdin := ""
	if opts.Stdin != nil {
		stdin = *opts.Stdin
	}
	f.stdins = append(f.stdins, stdin)
	i := len(f.commands) - 1
	if i < len(f.errors) && f.errors[i] != nil {
		return nil, f.errors[i]
	}
	if i < len(f.results) {
		return f.results[i], nil
	}
	return &ExecResult{ReturnCode: 0}, nil
}
func (f *fakeSandbox) Cleanup(bool) error                                   { return nil }
func (f *fakeSandbox) EnvType() string                                      { return "docker" }
func (f *fakeSandbox) Cwd() string                                          { return "/work" }
func (f *fakeSandbox) SetCwd(string)                                        {}
func (f *fakeSandbox) TempDir() string                                      { return f.temp }
func (f *fakeSandbox) HostCwd() string                                      { return "" }
func (f *fakeSandbox) HostCwdMount() string                                 { return "" }
func (f *fakeSandbox) SandboxHome() string                                  { return "" }
func (f *fakeSandbox) IsPersistent() bool                                   { return false }
func (f *fakeSandbox) IsSessionScoped() bool                                { return false }
func (f *fakeSandbox) UsesHostFilesystem() bool                             { return f.host }
func (f *fakeSandbox) FetchFile(context.Context, string, string, int) error { return nil }
func (f *fakeSandbox) FetchRealpath(context.Context, string) string         { return "" }

func storageEnv(t *testing.T) *Env {
	t.Helper()
	ResetSpilloverPruneState()
	return &Env{Home: t.TempDir(), Config: config.Defaults()}
}

func TestGeneratePreview(t *testing.T) {
	if p, more := GeneratePreview("short result", DefaultPreviewSizeChars); p != "short result" || more {
		t.Fatal("short changed")
	}
	exact := strings.Repeat("x", DefaultPreviewSizeChars)
	if p, more := GeneratePreview(exact, DefaultPreviewSizeChars); p != exact || more {
		t.Fatal("boundary")
	}
}

func TestWriteToSandbox(t *testing.T) {
	env := storageEnv(t)
	ctx := context.Background()
	sb := &fakeSandbox{results: []*ExecResult{{ReturnCode: 0}, {ReturnCode: 0, Output: "11\n"}}}
	if !writeToSandbox(ctx, env, "hello world", "/tmp/icebath results/abc file.txt", sb) {
		t.Fatal("write failed")
	}
	if strings.Contains(sb.commands[0], "hello world") || sb.stdins[0] != "hello world" {
		t.Fatalf("content not via stdin: %q", sb.commands[0])
	}
	if !strings.Contains(sb.commands[0], "'/tmp/icebath results'") || !strings.Contains(sb.commands[0], "'/tmp/icebath results/abc file.txt'") {
		t.Fatalf("quoting: %q", sb.commands[0])
	}
	sb = &fakeSandbox{}
	writeToSandbox(ctx, env, "content", "/tmp/icebath-results/$(whoami).txt", sb)
	if !strings.Contains(sb.commands[0], "'/tmp/icebath-results/$(whoami).txt'") {
		t.Fatalf("metachar: %q", sb.commands[0])
	}
	big := strings.Repeat("x", 200000)
	sb = &fakeSandbox{}
	writeToSandbox(ctx, env, big, "/tmp/icebath-results/big.txt", sb)
	if len(sb.commands[0]) > 1000 || sb.stdins[0] != big {
		t.Fatal("large content not via stdin")
	}
	content := strings.Repeat("héllo wörld ✓", 10)
	for _, tc := range []struct {
		probed string
		ok     bool
	}{{"512\n", false}, {"171\n", false}, {"170\n", true}} {
		sb = &fakeSandbox{results: []*ExecResult{{ReturnCode: 0}, {ReturnCode: 0, Output: tc.probed}, {ReturnCode: 0}}}
		if got := writeToSandbox(ctx, env, content, "/tmp/icebath-results/p.txt", sb); got != tc.ok {
			t.Fatalf("probe %q got %v", tc.probed, got)
		}
		if !tc.ok && !strings.HasPrefix(sb.commands[2], "rm -f ") {
			t.Fatalf("no cleanup: %v", sb.commands)
		}
	}
	sb = &fakeSandbox{results: []*ExecResult{{ReturnCode: 0}, nil}, errors: []error{nil, os.ErrClosed}}
	if !writeToSandbox(ctx, env, "data", "/tmp/icebath-results/np.txt", sb) {
		t.Fatal("unprobeable backend discarded archive")
	}
}

func TestResolveStorageDirAndFilenames(t *testing.T) {
	if resolveStorageDir(nil) != StorageDir() {
		t.Fatal("nil sandbox")
	}
	if resolveStorageDir(&fakeSandbox{temp: "/var/host/tmp"}) != "/var/host/tmp/icebath-results" {
		t.Fatal("temp dir")
	}
	if safeResultFilename("tc_456") != "tc_456.txt" {
		t.Fatal("normal id")
	}
	name := safeResultFilename("../outside/$(whoami);x")
	if !strings.HasPrefix(name, "outside_whoami_x_") || !strings.HasSuffix(name, ".txt") || strings.ContainsAny(name, "/$;") {
		t.Fatalf("got %q", name)
	}
}

func TestBuildPersistedMessage(t *testing.T) {
	msg := BuildPersistedMessage("first 100 chars...", true, 50000, "/tmp/icebath-results/test123.txt")
	if !strings.HasPrefix(msg, PersistedOutputTag) || !strings.HasSuffix(msg, PersistedOutputClosingTag) {
		t.Fatal("tags")
	}
	for _, want := range []string{"/tmp/icebath-results/test123.txt", "read_file", "first 100 chars...", "...", "50,000 characters", "inspect the current state before retrying"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(msg, "re-run the original tool") {
		t.Fatal("promise present")
	}
	if ExtractPersistedPath(msg) != "/tmp/icebath-results/test123.txt" {
		t.Fatal("extract path")
	}
}

func TestMaybePersistToolResult(t *testing.T) {
	env := storageEnv(t)
	ctx := context.Background()
	if got := MaybePersistToolResult(ctx, env, nil, "small result", "terminal", "tc_123", DefaultBudget, 50000); got != "small result" {
		t.Fatal("below threshold changed")
	}
	content := strings.Repeat("x", 60000)
	result := MaybePersistToolResult(ctx, env, nil, content, "tool_call", "tc_mcp_1", DefaultBudget, 30000)
	if !strings.Contains(result, PersistedOutputTag) || strings.Contains(result, "could not be saved") {
		t.Fatalf("host spill: %s", result[:200])
	}
	spill := filepath.Join(SpilloverDir(env), "tc_mcp_1.txt")
	data, err := os.ReadFile(spill)
	if err != nil || string(data) != content || !strings.Contains(result, spill) {
		t.Fatal("spill file")
	}
	local := &fakeSandbox{host: true}
	result = MaybePersistToolResult(ctx, env, local, strings.Repeat("y", 60000), "terminal", "tc_local_1", DefaultBudget, 30000)
	if !strings.Contains(result, PersistedOutputTag) || len(local.commands) != 0 {
		t.Fatal("local env shelled out")
	}
	remote := &fakeSandbox{results: []*ExecResult{{ReturnCode: 0}}}
	result = MaybePersistToolResult(ctx, env, remote, strings.Repeat("z", 60000), "terminal", "tc_remote_1", DefaultBudget, 30000)
	if !strings.Contains(result, PersistedOutputTag) || len(remote.commands) != 1 || !strings.Contains(remote.commands[0], "test -r") {
		t.Fatalf("remote probe: %v", remote.commands)
	}
	remote = &fakeSandbox{temp: "/tmp", results: []*ExecResult{{ReturnCode: 1}, {ReturnCode: 0}, {ReturnCode: 0, Output: "60000\n"}}}
	result = MaybePersistToolResult(ctx, env, remote, strings.Repeat("z", 60000), "terminal", "tc_remote_2", DefaultBudget, 30000)
	if !strings.Contains(result, "/tmp/icebath-results/tc_remote_2.txt") || len(remote.commands) != 3 {
		t.Fatalf("sandbox fallback: %v", remote.commands)
	}
	escape := &fakeSandbox{results: []*ExecResult{{ReturnCode: 1}, {ReturnCode: 0}, {ReturnCode: 1}}}
	result = MaybePersistToolResult(ctx, env, escape, strings.Repeat("x", 60000), "terminal", "../outside/$(whoami);x", DefaultBudget, 30000)
	target := strings.SplitN(escape.commands[1], "cat > ", 2)[1]
	if !strings.Contains(result, "Full output saved to: "+StorageDir()+"/outside_whoami_x_") || strings.Contains(target, "/../") || strings.Contains(target, "$(whoami)") {
		t.Fatalf("escape: %q", target)
	}
	zero := MaybePersistToolResult(ctx, env, nil, "even short content", "terminal", "tc_zero", DefaultBudget, 0)
	if !strings.Contains(zero, PersistedOutputTag) {
		t.Fatal("threshold zero")
	}
	unwritable := &Env{Home: filepath.Join(t.TempDir(), "file-as-home"), Config: config.Defaults()}
	os.WriteFile(unwritable.Home, []byte("x"), 0o644)
	inline := MaybePersistToolResult(ctx, unwritable, nil, strings.Repeat("w", 60000), "tool_call", "tc_fail_1", DefaultBudget, 30000)
	if !strings.Contains(inline, "could not be saved") || strings.Contains(inline, PersistedOutputTag) {
		t.Fatal("write failure fallback")
	}
}

func TestEnforceTurnBudget(t *testing.T) {
	env := storageEnv(t)
	ctx := context.Background()
	msgs := []*state.Message{{Role: "tool", ToolCallID: "t1", Content: "small"}, {Role: "tool", ToolCallID: "t2", Content: "also small"}}
	cfg := DefaultBudget
	cfg.TurnBudget = 200000
	EnforceTurnBudget(ctx, env, nil, msgs, cfg)
	if msgs[0].Content != "small" || msgs[1].Content != "also small" {
		t.Fatal("under budget changed")
	}
	msgs = nil
	for i := 0; i < 6; i++ {
		msgs = append(msgs, &state.Message{Role: "tool", ToolCallID: "t" + string(rune('0'+i)), Content: strings.Repeat("x", 42000)})
	}
	EnforceTurnBudget(ctx, env, nil, msgs, cfg)
	persisted := 0
	for _, m := range msgs {
		if strings.Contains(m.Content.(string), PersistedOutputTag) {
			persisted++
		}
	}
	if persisted < 2 {
		t.Fatalf("persisted %d", persisted)
	}
	if out := EnforceTurnBudget(ctx, env, nil, nil, cfg); len(out) != 0 {
		t.Fatal("empty")
	}
	img := map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64,AAAA"}}
	msgs = nil
	for i := 0; i < 6; i++ {
		msgs = append(msgs, &state.Message{Role: "tool", ToolCallID: "mm" + string(rune('0'+i)), Content: []any{map[string]any{"type": "text", "text": strings.Repeat("x", 60000)}, map[string]any{"type": "text", "text": "scale note"}, img}})
	}
	EnforceTurnBudget(ctx, env, nil, msgs, cfg)
	spilled := 0
	for _, m := range msgs {
		parts := m.Content.([]any)
		if strings.Contains(parts[0].(map[string]any)["text"].(string), PersistedOutputTag) {
			spilled++
		}
		if parts[1].(map[string]any)["text"] != "scale note" || parts[2].(map[string]any)["type"] != "image_url" {
			t.Fatal("other parts changed")
		}
	}
	if spilled < 2 {
		t.Fatalf("multimodal spilled %d", spilled)
	}
}

func TestCleanupSpilloverCache(t *testing.T) {
	env := storageEnv(t)
	dir := SpilloverDir(env)
	os.MkdirAll(dir, 0o755)
	old := filepath.Join(dir, "old.txt")
	newer := filepath.Join(dir, "new.txt")
	os.WriteFile(old, []byte("old"), 0o644)
	os.WriteFile(newer, []byte("new"), 0o644)
	stale := time.Now().Add(-48 * time.Hour)
	os.Chtimes(old, stale, stale)
	if removed := CleanupSpilloverCache(env, 24); removed != 1 {
		t.Fatalf("removed %d", removed)
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("old kept")
	}
	if _, err := os.Stat(newer); err != nil {
		t.Fatal("new removed")
	}
	if CleanupSpilloverCache(&Env{Home: t.TempDir(), Config: config.Defaults()}, -1) != 0 {
		t.Fatal("missing dir")
	}
	retention := &Env{Home: t.TempDir(), Config: config.Tree{"sessions": map[string]any{"retention_days": 90}}}
	if spilloverRetentionHours(retention) != 90*24 {
		t.Fatal("retention days")
	}
	if spilloverRetentionHours(&Env{Home: t.TempDir(), Config: config.Tree{"sessions": map[string]any{"retention_days": 0}}}) != SpilloverMaxAgeHours {
		t.Fatal("retention floor")
	}
	if spilloverRetentionHours(&Env{Home: t.TempDir(), Config: config.Tree{"sessions": map[string]any{"retention_days": "nope"}}}) != SpilloverMaxAgeHours {
		t.Fatal("retention fallback")
	}
	ancient := filepath.Join(dir, "ancient.txt")
	os.WriteFile(ancient, []byte("ancient"), 0o644)
	os.Chtimes(ancient, stale, stale)
	ResetSpilloverPruneState()
	env.Config = config.Tree{"sessions": map[string]any{"retention_days": 1}}
	MaybePersistToolResult(context.Background(), env, nil, strings.Repeat("v", 60000), "tool_call", "tc_prune_1", DefaultBudget, 30000)
	if _, err := os.Stat(ancient); err == nil {
		t.Fatal("first spill did not prune")
	}
	if _, err := os.Stat(filepath.Join(dir, "tc_prune_1.txt")); err != nil {
		t.Fatal("spill missing")
	}
	if fields := ExpiredArchiveFields("/x/cache/spillover/tc_1.txt"); fields["expired"] != true {
		t.Fatal("expired fields")
	}
	if fields := ExpiredArchiveFields("/x/other/tc_1.txt"); len(fields) != 0 {
		t.Fatal("plain path marked expired")
	}
}

func TestPageableText(t *testing.T) {
	typed := pageableText(`{"result": "t", "_meta": {"z": [1, 1.0, 1e2, -0.0, 12345678901234567890123]}, "structuredContent": {"n": 1, "f": 1.0}}`)
	if want := "t\n\n<mcp-result-metadata>\n{\n \"_meta\": {\n  \"z\": [\n   1,\n   1.0,\n   100.0,\n   -0.0,\n   12345678901234567890123\n  ]\n },\n \"structuredContent\": {\n  \"f\": 1.0,\n  \"n\": 1\n }\n}\n</mcp-result-metadata>\n"; typed != want {
		t.Fatalf("typed metadata %q, want %q", typed, want)
	}
	if pageableText(`{"result": "line one\nline two"}`) != "line one\nline two" {
		t.Fatal("single envelope")
	}
	out := pageableText(`{"result": "text\n", "structuredContent": {"answer": 42}}`)
	text, tail, _ := strings.Cut(out, "\n\n<mcp-result-metadata>\n")
	if text != "text\n" {
		t.Fatalf("text %q", text)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(strings.Split(tail, "\n</mcp-result-metadata>")[0]), &meta); err != nil || meta["structuredContent"].(map[string]any)["answer"] != 42.0 {
		t.Fatalf("metadata %q", tail)
	}
	for _, content := range []string{
		`{"output": "line\n", "exit_code": 0}`, `{"result": "line\n", "exit_code": 0}`, `[{"result": "line\n"}]`,
		`{"result": {"blob": "line\n"}}`, `{"structuredContent": {"a": 1}}`, `{"result": ""}`, "not json at all", "plain text result",
	} {
		if pageableText(content) != content {
			t.Fatalf("verbatim: %s", content)
		}
	}
}

func TestMcpEnvelopeSpillover(t *testing.T) {
	env := storageEnv(t)
	ctx := context.Background()
	markdown := "# Research guide\n\n" + strings.Repeat("pageable content\n", 4000)
	envelope := jsonCompact(map[string]any{"result": markdown})
	result := MaybePersistToolResult(ctx, env, nil, envelope, "mcp__archive__read_guide", "tc_mcp_text", DefaultBudget, 30000)
	data, _ := os.ReadFile(filepath.Join(SpilloverDir(env), "tc_mcp_text.txt"))
	if string(data) != markdown {
		t.Fatal("spill not unwrapped")
	}
	preview := strings.SplitN(result, "Preview (first", 2)[1]
	if !strings.Contains(preview, "# Research guide") || strings.Contains(preview, `\n`) {
		t.Fatal("preview escaped")
	}
	body := strings.Repeat("body line\n", 5000)
	MaybePersistToolResult(ctx, env, nil, jsonCompact(map[string]any{"result": body, "structuredContent": map[string]any{"count": 5000}}), "mcp__archive__structured", "tc_mcp_meta", DefaultBudget, 30000)
	spill, _ := os.ReadFile(filepath.Join(SpilloverDir(env), "tc_mcp_meta.txt"))
	text, tail, _ := strings.Cut(string(spill), "\n\n<mcp-result-metadata>\n")
	if text != body || !strings.Contains(tail, `"count": 5000`) {
		t.Fatal("metadata not kept")
	}
	verbatim := jsonCompact(map[string]any{"output": strings.Repeat("line\n", 8000), "exit_code": 0})
	MaybePersistToolResult(ctx, env, nil, verbatim, "terminal", "tc_json_verbatim", DefaultBudget, 30000)
	if data, _ := os.ReadFile(filepath.Join(SpilloverDir(env), "tc_json_verbatim.txt")); string(data) != verbatim {
		t.Fatal("non envelope unwrapped")
	}
	remote := &fakeSandbox{results: []*ExecResult{{ReturnCode: 1}, {ReturnCode: 0}, {ReturnCode: 1}}}
	MaybePersistToolResult(ctx, env, remote, jsonCompact(map[string]any{"result": body}), "mcp__archive__remote", "tc_mcp_remote", DefaultBudget, 30000)
	if remote.stdins[1] != body {
		t.Fatal("sandbox write carried envelope")
	}
	msgs := []*state.Message{{Role: "tool", ToolName: "mcp__archive__read", ToolCallID: "tc_budget_mcp", Content: jsonCompact(map[string]any{"result": strings.Repeat("budgeted line\n", 3000)})}}
	cfg := DefaultBudget
	cfg.TurnBudget = 10000
	EnforceTurnBudget(ctx, env, nil, msgs, cfg)
	if !strings.Contains(msgs[0].Content.(string), PersistedOutputTag) {
		t.Fatal("budget spill")
	}
}

func TestSpillIfOversized(t *testing.T) {
	env := storageEnv(t)
	cfg := SpillConfigFromEnv(env)
	if !cfg.Enabled || cfg.MaxChars != SpillDefaultMaxChars || cfg.PreviewHead != SpillDefaultPreviewHead || cfg.PreviewTail != SpillDefaultPreviewTail {
		t.Fatalf("defaults %+v", cfg)
	}
	if SpillIfOversized(env, "", "s", "hook", nil) != "" || SpillIfOversized(env, "short", "s", "hook", nil) != "short" {
		t.Fatal("noops")
	}
	text := strings.Repeat("a", 12000)
	out := SpillIfOversized(env, text, "sess/1", "hook", nil)
	if !strings.Contains(out, "[hook output truncated — 12,000 chars; full content saved to ") || !strings.Contains(out, "--- head ---") || !strings.Contains(out, "--- tail ---") {
		t.Fatalf("preview %q", out[:120])
	}
	entries, _ := os.ReadDir(filepath.Join(env.Home, "hook_outputs", "sess_1"))
	if len(entries) != 1 {
		t.Fatal("spill file missing")
	}
	info, _ := entries[0].Info()
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	disabled := SpillConfig{Enabled: false, MaxChars: 10}
	if SpillIfOversized(env, text, "", "hook", &disabled) != text {
		t.Fatal("disabled spilled")
	}
}

func TestSpillSafety(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spill")
	if err := EnsureSpillDir(dir, true); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatal("dir mode")
	}
	path := filepath.Join(dir, "a.txt")
	if err := WriteTextExclusive(path, "hello", true, false); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "hello" {
		t.Fatal("content")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatal("file mode")
	}
	if err := WriteTextExclusive(path, "again", true, false); err == nil {
		t.Fatal("exclusive create succeeded on existing")
	}
	if err := WriteTextExclusive(path, "again", true, true); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	os.WriteFile(victim, []byte("keep"), 0o644)
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(victim, link); err == nil {
		if err := WriteTextExclusive(link, "pwned", true, false); err == nil {
			t.Fatal("followed symlink")
		}
		if err := WriteTextExclusive(link, "replaced", true, true); err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(victim); string(data) != "keep" {
			t.Fatal("overwrite reached the symlink target")
		}
		dangling := filepath.Join(dir, "dangling.txt")
		os.Symlink(filepath.Join(dir, "nowhere"), dangling)
		if err := WriteTextExclusive(dangling, "x", true, false); err == nil {
			t.Fatal("dangling symlink accepted")
		}
		leaf := filepath.Join(dir, "leaf")
		os.Symlink(victim, leaf)
		if err := EnsureSpillDir(leaf, true); err == nil {
			t.Fatal("symlinked leaf accepted")
		}
	}
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o700)
	if err := WriteTextExclusive(sub, "x", true, true); err == nil {
		t.Fatal("overwrote a directory")
	}
}
