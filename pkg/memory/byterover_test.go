//go:build !windows

package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// One recorded brv invocation, its subcommand, working directory, home and last argument
type brvCall struct {
	cmd, cwd, home, last string
}

// Installs a fake brv on PATH that records each call to its own file and prints output
func fakeBrv(t *testing.T, output string) (string, func() []brvCall) {
	t.Helper()
	bin := t.TempDir()
	logDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"for a; do last=$a; done\n" +
		"f=$(mktemp \"" + logDir + "/call.XXXXXX\")\n" +
		"{ printf '%s\\n' \"$1\"; pwd -P; printf '%s\\n' \"$ICEBATH_HOME\"; printf '%s' \"$last\"; } > \"$f\"\n" +
		"env > \"$f.env\"\n" +
		"printf '%s' '" + output + "'\n"
	path := filepath.Join(bin, "brv")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", t.TempDir())
	forgetBrvPath()
	t.Cleanup(forgetBrvPath)
	read := func() []brvCall {
		matches, _ := filepath.Glob(filepath.Join(logDir, "call.*"))
		var calls []brvCall
		for _, m := range matches {
			if strings.HasSuffix(m, ".env") {
				continue
			}
			raw, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			parts := strings.SplitN(string(raw), "\n", 4)
			if len(parts) < 4 {
				continue
			}
			calls = append(calls, brvCall{cmd: parts[0], cwd: parts[1], home: parts[2], last: parts[3]})
		}
		return calls
	}
	return logDir, read
}

// Environment the last fake brv call saw
func brvChildEnvOf(t *testing.T, logDir string) map[string]string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(logDir, "call.*.env"))
	if len(matches) != 1 {
		t.Fatalf("expected one brv call, got %d", len(matches))
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

func resolved(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Profile secret source with no managed or source supplied names
type brvSecretSource struct{}

func (brvSecretSource) ManagedDotenvKeys() []string                      { return nil }
func (brvSecretSource) SourceSuppliedNames() []string                    { return nil }
func (brvSecretSource) BuildProfileSecretScope(string) map[string]string { return map[string]string{} }
func (brvSecretSource) CurrentSecretScope(ctx context.Context) map[string]string {
	return provider.CurrentSecretScope(ctx)
}

// Launch root with a default profile env file and a served secondary profile b
func brvTwoProfiles(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".icebath")
	profB := filepath.Join(root, "profiles", "b")
	if err := os.MkdirAll(profB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("BRV_API_KEY=DEFAULT-PROFILE-KEY\nICEBATH_MODEL=default-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("BRV_API_KEY", "DEFAULT-PROFILE-KEY")
	t.Setenv("ICEBATH_MODEL", "default-model")
	prev := tools.ProfileSecrets
	tools.ProfileSecrets = brvSecretSource{}
	t.Cleanup(func() { tools.ProfileSecrets = prev })
	return root, profB
}

// Context of a served turn for a profile with its secret scope on a multiplexed process
func brvServedTurn(profHome string, scope map[string]string) context.Context {
	ctx := provider.WithMultiplexContext(context.Background(), true)
	ctx = provider.WithSecretScope(ctx, scope, profHome)
	return home.WithOverride(ctx, profHome)
}

func TestByteRoverAutoExtractFalseSkipsSyncTurn(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	_, calls := fakeBrv(t, "ok")
	ctx := context.Background()
	p := NewByteRoverProvider(ctx, nil, map[string]any{"auto_extract": false})
	if err := p.Initialize(ctx, "session-1", InitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := p.SyncTurn(ctx, "please remember this detail", "acknowledged", SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("brv ran %v", got)
	}
	if p.syncThread != nil {
		t.Fatal("sync thread was started")
	}
}

func TestByteRoverSecondaryProfileChildUsesItsOwnKeyNotDefaults(t *testing.T) {
	_, profB := brvTwoProfiles(t)
	logDir, _ := fakeBrv(t, "")
	ctx := brvServedTurn(profB, map[string]string{"BRV_API_KEY": "PROFILE-B-KEY"})
	if _, err := runBrv(ctx, nil, []any{"query", "--", "hello"}, brvQueryTimeout, filepath.Join(profB, "byterover")); err != nil {
		t.Fatal(err)
	}
	env := brvChildEnvOf(t, logDir)
	if env["BRV_API_KEY"] != "PROFILE-B-KEY" {
		t.Fatalf("BRV_API_KEY = %q", env["BRV_API_KEY"])
	}
	if env["ICEBATH_HOME"] != profB {
		t.Fatalf("ICEBATH_HOME = %q, want %q", env["ICEBATH_HOME"], profB)
	}
	if _, ok := env["ICEBATH_MODEL"]; ok {
		t.Fatal("launch profile env residue reached the child")
	}
	if !strings.HasPrefix(env["PATH"], filepath.Dir(resolveBrvPath())) {
		t.Fatalf("PATH %q does not lead with the brv directory", env["PATH"])
	}
}

func TestByteRoverSecondaryWithoutKeyGetsNoKeyNeverDefaults(t *testing.T) {
	_, profB := brvTwoProfiles(t)
	logDir, _ := fakeBrv(t, "")
	ctx := brvServedTurn(profB, map[string]string{"OTHER": "x"})
	if _, err := runBrv(ctx, nil, []any{"curate", "--", "note"}, brvCurateTimeout, filepath.Join(profB, "byterover")); err != nil {
		t.Fatal(err)
	}
	if v, ok := brvChildEnvOf(t, logDir)["BRV_API_KEY"]; ok {
		t.Fatalf("child got BRV_API_KEY %q", v)
	}
}

func TestByteRoverBindsTheHomeInitializeReceives(t *testing.T) {
	alpha, _ := alphaBetaHomes(t)
	ctx := context.Background()
	p := NewByteRoverProvider(ctx, nil, map[string]any{})
	if err := p.Initialize(ctx, "s", InitOptions{Home: alpha}); err != nil {
		t.Fatal(err)
	}
	if p.cwd != filepath.Join(alpha, "byterover") {
		t.Fatalf("cwd = %q", p.cwd)
	}
}

func TestByteRoverTwoProfilesInterleavingStayInTheirOwnTrees(t *testing.T) {
	alpha, beta := alphaBetaHomes(t)
	_, calls := fakeBrv(t, strings.Repeat("x", 40))
	managers := map[string]*MemoryManager{}
	for _, h := range []string{alpha, beta} {
		ctx := home.WithOverride(context.Background(), h)
		m := NewMemoryManager(ctx, nil, nil)
		if err := m.AddProvider(ctx, NewByteRoverProvider(ctx, nil, map[string]any{})); err != nil {
			t.Fatal(err)
		}
		m.InitializeAll(ctx, "s-"+filepath.Base(h), InitOptions{Platform: "api_server", Home: h})
		managers[h] = m
	}
	t.Cleanup(func() {
		for _, m := range managers {
			m.ShutdownAll(context.Background())
		}
	})
	runInterleaved(t, managers, "brv_curate", "content", 5)
	waitUntil(t, "every brv call from both profiles", func() bool { return len(calls()) >= 2*3*5 })
	for _, h := range []string{alpha, beta} {
		var mine []brvCall
		for _, c := range calls() {
			if strings.Contains(c.last, filepath.Base(h)+"-") {
				mine = append(mine, c)
			}
		}
		if len(mine) != 15 {
			t.Fatalf("%s made %d calls, want 15", h, len(mine))
		}
		for _, c := range mine {
			if c.cwd != resolved(t, filepath.Join(h, "byterover")) {
				t.Fatalf("%s call ran in %s", h, c.cwd)
			}
		}
	}
}

func TestByteRoverBackgroundThreadSeesSpawnerProfileHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "default"))
	profileHome := filepath.Join(tmp, "profiles", "b")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	_, calls := fakeBrv(t, "ok")
	ctx := home.WithOverride(context.Background(), profileHome)
	p := NewByteRoverProvider(ctx, nil, nil)
	thread := p.curateInBackground(ctx, "content", "brv-test", "test", "")
	thread.Join(10 * time.Second)
	got := calls()
	if len(got) != 1 {
		t.Fatalf("calls = %v", got)
	}
	if got[0].cwd != resolved(t, filepath.Join(profileHome, "byterover")) || got[0].home != profileHome {
		t.Fatalf("background curate resolved cwd %s home %s", got[0].cwd, got[0].home)
	}
}

func TestByteRoverToolCalls(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	_, calls := fakeBrv(t, "short")
	ctx := context.Background()
	p := NewByteRoverProvider(ctx, nil, map[string]any{})
	if err := p.Initialize(ctx, "s", InitOptions{}); err != nil {
		t.Fatal(err)
	}
	decode := func(raw string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	out, err := p.HandleToolCall(ctx, "brv_nope", map[string]any{}, ToolCallOptions{})
	if err != nil || decode(out)["error"] != "Unknown tool: brv_nope" {
		t.Fatalf("unknown tool %q %v", out, err)
	}
	out, _ = p.HandleToolCall(ctx, "brv_query", map[string]any{"query": ""}, ToolCallOptions{})
	if decode(out)["error"] != "query is required" {
		t.Fatalf("missing query %q", out)
	}
	if len(calls()) != 0 {
		t.Fatal("a refused call reached brv")
	}
	out, _ = p.HandleToolCall(ctx, "brv_query", map[string]any{"query": "what did we decide"}, ToolCallOptions{})
	if decode(out)["result"] != "No relevant memories found." {
		t.Fatalf("short output %q", out)
	}
	out, _ = p.HandleToolCall(ctx, "brv_curate", map[string]any{"content": "remember this"}, ToolCallOptions{})
	if decode(out)["result"] != "Memory curated successfully." {
		t.Fatalf("curate %q", out)
	}
	out, _ = p.HandleToolCall(ctx, "brv_status", map[string]any{}, ToolCallOptions{})
	if decode(out)["status"] != "short" {
		t.Fatalf("status %q", out)
	}
	if _, err := p.HandleToolCall(ctx, "brv_query", map[string]any{"query": 5}, ToolCallOptions{}); err == nil ||
		err.Error() != "'int' object has no attribute 'strip'" {
		t.Fatalf("non string query error %v", err)
	}
	out, _ = p.HandleToolCall(ctx, "brv_curate", map[string]any{"content": 5}, ToolCallOptions{})
	if decode(out)["error"] != "expected str, bytes or os.PathLike object, not int" {
		t.Fatalf("non string curate %q", out)
	}
}

func TestByteRoverQueryOutputFormatting(t *testing.T) {
	if got := formatBrvQueryOutput("  tiny  ")["result"]; got != "No relevant memories found." {
		t.Fatalf("tiny = %v", got)
	}
	long := strings.Repeat("y", 8001)
	if got := formatBrvQueryOutput(long)["result"]; got != strings.Repeat("y", 8000)+"\n\n[... truncated]" {
		t.Fatal("long output not truncated")
	}
	body := strings.Repeat("z", 30)
	if got := formatBrvQueryOutput(body)["result"]; got != body {
		t.Fatalf("body = %v", got)
	}
}

func TestByteRoverPrefetchAndPreCompress(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	_, calls := fakeBrv(t, strings.Repeat("k", 25))
	ctx := context.Background()
	p := NewByteRoverProvider(ctx, nil, map[string]any{"auto_extract": "yes"})
	if err := p.Initialize(ctx, "s", InitOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Prefetch(ctx, "short", ""); got != "" {
		t.Fatalf("short query prefetched %q", got)
	}
	if got, _ := p.Prefetch(ctx, "what is the deploy plan", ""); got != "## ByteRover Context\n"+strings.Repeat("k", 25) {
		t.Fatalf("prefetch %q", got)
	}
	var msgs []map[string]any
	for i := 0; i < 12; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": "m" + itoa(i)})
	}
	msgs = append(msgs, map[string]any{"role": "tool", "content": "skip"}, map[string]any{"role": "assistant", "content": []any{"x"}})
	if got, err := p.OnPreCompress(ctx, msgs, false); got != "" || err != nil {
		t.Fatalf("pre compress %q %v", got, err)
	}
	waitUntil(t, "the pre compression curate", func() bool {
		for _, c := range calls() {
			if c.cmd == "curate" {
				return true
			}
		}
		return false
	})
	for _, c := range calls() {
		if c.cmd != "curate" {
			continue
		}
		want := "[Pre-compression context]\nuser: m4\nuser: m5\nuser: m6\nuser: m7\nuser: m8\nuser: m9\nuser: m10\nuser: m11"
		if c.last != want {
			t.Fatalf("flush content %q", c.last)
		}
	}
}

func TestByteRoverMissingCLI(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	forgetBrvPath()
	t.Cleanup(forgetBrvPath)
	ctx := context.Background()
	p := NewByteRoverProvider(ctx, nil, map[string]any{})
	if p.Available(ctx) {
		t.Fatal("available without brv")
	}
	if block, _ := p.SystemPromptBlock(ctx); block != "" {
		t.Fatalf("block %q", block)
	}
	res, err := runBrv(ctx, nil, []any{"status"}, brvStatusTimeout, t.TempDir())
	if err != nil || res.Success || res.Error != "brv CLI not found. Install: npm install -g byterover-cli" {
		t.Fatalf("missing cli %+v %v", res, err)
	}
}

func TestByteRoverConfigBlocks(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("memory:\n  byterover:\n    auto_extract: 'off'\n")
	if p := NewByteRoverProvider(context.Background(), nil, nil); p.autoExtract {
		t.Fatal("memory.byterover auto_extract off ignored")
	}
	write("memory:\n  byterover: {}\n  provider_config:\n    auto_extract: false\n")
	if p := NewByteRoverProvider(context.Background(), nil, nil); p.autoExtract {
		t.Fatal("legacy provider_config ignored when byterover block is empty")
	}
}

func TestByteRoverDecodeReplaceMatchesPython(t *testing.T) {
	r := string(rune(0xFFFD))
	for raw, want := range map[string]string{
		"\xe2\x82A": r + "A", "\xed\xa0\x80": r + r + r, "\xff\xfe": r + r,
		"\xf0\x9f\x98": r, "ok\xc3": "ok" + r, "\xc0\xaf": r + r,
		"\xf4\x90\x80\x80": r + r + r + r, "\xe0\x80\xaf": r + r + r, "plain": "plain",
	} {
		if got := decodeReplace([]byte(raw)); got != want {
			t.Fatalf("decode %q = %q, want %q", raw, got, want)
		}
	}
}

func TestByteRoverRegisteredWithManifestDescription(t *testing.T) {
	r := FindProvider(context.Background(), nil, "byterover")
	if r == nil || r.Source != SourceBundled {
		t.Fatalf("byterover registration %+v", r)
	}
	if r.Description() != "ByteRover — persistent knowledge tree with tiered retrieval via the brv CLI." {
		t.Fatalf("description %q", r.Description())
	}
}
