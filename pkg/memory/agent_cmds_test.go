package memory

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
	"testing/iotest"

	"github.com/nickheyer/icebath/pkg/cliui"
)

func agentCmdsHome(t *testing.T) (context.Context, string) {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	return context.Background(), h
}

// Open descriptors of this process on path, which keeps at most the state header probe descriptor pkg/state never closes
func openFDsTo(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	var open []string
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && strings.HasPrefix(target, path) {
			open = append(open, target)
		}
	}
	return open
}

func TestInsightsSkipsOpenWhenStoreMissing(t *testing.T) {
	ctx, h := agentCmdsHome(t)
	var out bytes.Buffer
	CmdInsights(ctx, &out, 30, "")
	if !strings.Contains(out.String(), "No session data yet.") {
		t.Fatalf("output %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(h, "state.db")); !os.IsNotExist(err) {
		t.Fatal("insights created state.db")
	}
}

func TestSubcommandInsightsReadsWithoutWritingAndCloses(t *testing.T) {
	ctx, h := agentCmdsHome(t)
	path := filepath.Join(h, "state.db")
	db, err := state.Open(path, state.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(state.NewSession{ID: "reader-fixture", Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendMessage(context.Background(), "reader-fixture", map[string]any{"role": "user", "content": "hello"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	CmdInsights(ctx, &out, 30, "tui")
	held := openFDsTo(t, path)
	CmdInsights(ctx, &bytes.Buffer{}, 30, "tui")
	if strings.Contains(out.String(), "Error generating insights:") || !strings.Contains(out.String(), "Icebath Insights") {
		t.Fatalf("output:\n%s", out.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("insights created or mutated state.db")
	}
	if open := openFDsTo(t, path); len(open) > 1 || len(open) != len(held) {
		t.Fatalf("descriptors left open %v after %v", open, held)
	}
}

func TestSubcommandInsightsClosesDatabaseWhenGenerationFails(t *testing.T) {
	ctx, h := agentCmdsHome(t)
	path := filepath.Join(h, "state.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	CmdInsights(ctx, &out, 30, "")
	if !strings.Contains(out.String(), "Error generating insights: ") {
		t.Fatalf("output %q", out.String())
	}
	held := openFDsTo(t, path)
	CmdInsights(ctx, &bytes.Buffer{}, 30, "")
	if open := openFDsTo(t, path); len(open) > 1 || len(open) != len(held) {
		t.Fatalf("descriptors left open after the failure %v after %v", open, held)
	}
	if st, err := os.Stat(path); err != nil || st.Size() != 0 {
		t.Fatal("read only insights wrote the empty store")
	}
}

func TestMemoryOffClearsProviderAndKeepsOtherKeys(t *testing.T) {
	ctx, h := agentCmdsHome(t)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("memory:\n  provider: honcho\n  memory_enabled: true\nmodel: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := MemoryOff(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "\n  ✓ Memory provider: built-in only\n  Saved to config.yaml\n\n" {
		t.Fatalf("output %q", out.String())
	}
	config.ResetCache()
	tree, err := config.Load(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	mem := config.AsMap(tree["memory"])
	if mem["provider"] != "" || mem["memory_enabled"] != true || tree["model"] != "x" {
		t.Fatalf("config after off %v", map[string]any(tree))
	}
}

func TestMemoryResetTargetsConfirmationAndNothingToReset(t *testing.T) {
	ctx, h := agentCmdsHome(t)
	var out bytes.Buffer
	if err := MemoryReset(ctx, &out, strings.NewReader(""), "all", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Nothing to reset — no memory files found in ") || !strings.HasSuffix(out.String(), "/memories/\n\n") {
		t.Fatalf("output %q", out.String())
	}
	mem := filepath.Join(h, "memories")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mem, "MEMORY.md"), bytes.Repeat([]byte("x"), 1234), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mem, "USER.md"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"", "no\n"} {
		out.Reset()
		if err := MemoryReset(ctx, &out, strings.NewReader(in), "all", false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "◆ MEMORY.md (agent notes) — 1,234 bytes") || !strings.Contains(out.String(), "Cancelled.") {
			t.Fatalf("output %q", out.String())
		}
	}
	out.Reset()
	if err := MemoryReset(ctx, &out, strings.NewReader(" YES \n"), "user", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "✓ Deleted USER.md (user profile)") || strings.Contains(out.String(), "MEMORY.md") {
		t.Fatalf("output %q", out.String())
	}
	out.Reset()
	if err := MemoryReset(ctx, &out, strings.NewReader(""), "memory", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "✓ Deleted MEMORY.md (agent notes)") || !strings.Contains(out.String(), "Memory reset complete.") {
		t.Fatalf("output %q", out.String())
	}
	for _, f := range []string{"MEMORY.md", "USER.md"} {
		if _, err := os.Stat(filepath.Join(mem, f)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the reset", f)
		}
	}
}

func TestMemoryResetCancelsOnInterruptAtThePrompt(t *testing.T) {
	ctx, h := agentCmdsHome(t)
	mem := filepath.Join(h, "memories")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mem, "USER.md"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := MemoryReset(ctx, &out, iotest.ErrReader(cliui.ErrInterrupt), "all", false); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "\n  Type 'yes' to confirm: \n  Cancelled.\n\n") {
		t.Fatalf("output %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(mem, "USER.md")); err != nil {
		t.Fatalf("USER.md removed after an interrupt %v", err)
	}
}
