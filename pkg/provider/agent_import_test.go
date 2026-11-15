package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

)

type agentImportHarness struct {
	userHome, icebathHome string
	calls                 []string
	yes                   bool
	unpick                map[string]bool
	childCode             int
}

// The hermes homes fixture: a fake HOME, a temp ICEBATH_HOME and recorders standing in for the prompts, the install and the child
func newAgentImportHarness(t *testing.T) *agentImportHarness {
	t.Helper()
	root := t.TempDir()
	h := &agentImportHarness{userHome: filepath.Join(root, "user"), icebathHome: filepath.Join(root, "user", ".icebath"), yes: true, unpick: map[string]bool{}}
	if err := os.MkdirAll(h.icebathHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", h.userHome)
	t.Setenv("USERPROFILE", h.userHome)
	t.Setenv("ICEBATH_HOME", h.icebathHome)
	t.Setenv("ICEBATH_NONINTERACTIVE", "")
	prevAsk, prevPick := agentImportAsk, agentImportChecklist
	t.Cleanup(func() {
		agentImportAsk, agentImportChecklist = prevAsk, prevPick
	})
	agentImportAsk = func(question string, def bool, description string) bool {
		h.calls = append(h.calls, fmt.Sprintf("ask|%s|%v", question, def))
		return h.yes
	}
	agentImportChecklist = func(title string, items []string, pre []int) []int {
		h.calls = append(h.calls, fmt.Sprintf("pick|%s|%v", strings.Join(items, ","), pre))
		var out []int
		for _, i := range pre {
			if !h.unpick[items[i]] {
				out = append(out, i)
			}
		}
		return out
	}
	restore := WithSetupOutput(io.Discard)
	t.Cleanup(restore)
	return h
}

func (h *agentImportHarness) mkdir(t *testing.T, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(h.userHome, rel), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (h *agentImportHarness) pluginsExist() bool {
	_, err := os.Stat(filepath.Join(h.icebathHome, "plugins"))
	return err == nil
}

func offer(t *testing.T, h *agentImportHarness) string {
	t.Helper()
	got, err := OfferAgentImport(context.Background(), h.icebathHome, true)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDecliningInstallsNothing(t *testing.T) {
	h := newAgentImportHarness(t)
	h.mkdir(t, ".codex")
	h.yes = false
	if got := offer(t, h); got != "" {
		t.Fatalf("got %q", got)
	}
	if len(h.calls) != 1 || h.calls[0] != "ask|Import your setup from Codex CLI?|true" || h.pluginsExist() {
		t.Fatalf("calls %v plugins %v", h.calls, h.pluginsExist())
	}
}

func TestHeadlessOrNothingFoundAsksNothing(t *testing.T) {
	for _, headless := range []bool{true, false} {
		t.Run(fmt.Sprintf("headless=%v", headless), func(t *testing.T) {
			h := newAgentImportHarness(t)
			if headless {
				h.mkdir(t, ".claude")
				t.Setenv("ICEBATH_NONINTERACTIVE", "1")
			}
			if got := offer(t, h); got != "" || len(h.calls) != 0 || h.pluginsExist() {
				t.Fatalf("got %q calls %v plugins %v", got, h.calls, h.pluginsExist())
			}
		})
	}
}

func TestSetupMenuShowsWhatWasFoundWithTheQuestion(t *testing.T) {
	h := newAgentImportHarness(t)
	for _, d := range []string{".openclaw", ".claude", ".codex"} {
		h.mkdir(t, d)
	}
	agentImportAsk = PromptYesNo
	var menus []string
	restore := SwapSetupRadiolist(func(question string, choices []string, def int, description string) int {
		menus = append(menus, question+"|"+strings.Join(choices, ",")+"|"+strings.Join(strings.Fields(description), " "))
		return 1
	})
	defer restore()
	var got string
	var err error
	SetupNavigationScope(func() { got, err = OfferAgentImport(context.Background(), h.icebathHome, true) })
	if err != nil || got != "" {
		t.Fatalf("got %q err %v", got, err)
	}
	if len(menus) != 1 || !strings.HasPrefix(menus[0], "Import your setup from any of them?|Yes,No|") {
		t.Fatalf("menus %v", menus)
	}
	if !strings.Contains(menus[0], "Found on this computer: OpenClaw (~/.openclaw), Claude Code (~/.claude), Codex CLI (~/.codex).") {
		t.Fatalf("description %q", menus[0])
	}
	if len(h.calls) != 0 || h.pluginsExist() {
		t.Fatalf("calls %v plugins %v", h.calls, h.pluginsExist())
	}
}

func TestMigrateWithoutATypeAndNothingFoundListsTheImporters(t *testing.T) {
	h := newAgentImportHarness(t)
	var out bytes.Buffer
	restore := WithSetupOutput(&out)
	defer restore()
	code, err := MigrateCommand(context.Background(), h.icebathHome, map[string]any{"migrate_type": nil}, &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if !strings.Contains(out.String(), "No other agent setup found on this computer. Import one with: icebath migrate openclaw, icebath migrate claude-code") {
		t.Fatalf("output %q", out.String())
	}
}

func TestMigrateUnknownTypePrintsUsage(t *testing.T) {
	h := newAgentImportHarness(t)
	var errOut bytes.Buffer
	code, err := MigrateCommand(context.Background(), h.icebathHome, map[string]any{"migrate_type": "bogus"}, io.Discard, &errOut)
	if err != nil || code != 2 || errOut.String() != "usage: icebath migrate [<agent> | xai [--apply] [--no-backup]]\n" {
		t.Fatalf("code %d err %v stderr %q", code, err, errOut.String())
	}
}

func TestMigrateXaiDryRunReportsIssuesWithoutWriting(t *testing.T) {
	h := newAgentImportHarness(t)
	body := "principal:\n  provider: xai\n  model: grok-3\n"
	path := filepath.Join(h.icebathHome, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := MigrateCommand(context.Background(), h.icebathHome, map[string]any{"migrate_type": "xai", "apply": false}, &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	text := out.String()
	if !strings.Contains(text, "xAI Model Retirement Migration") || !strings.Contains(text, "retired xAI model reference(s)") || !strings.Contains(text, "Dry-run mode — no changes written.") {
		t.Fatalf("output %q", text)
	}
	if raw, _ := os.ReadFile(path); string(raw) != body {
		t.Fatal("dry run changed config.yaml")
	}
}

func TestMigrateXaiApplyRewritesConfigAndBacksItUp(t *testing.T) {
	h := newAgentImportHarness(t)
	path := filepath.Join(h.icebathHome, "config.yaml")
	if err := os.WriteFile(path, []byte("principal:\n  provider: xai\n  model: grok-3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := MigrateCommand(context.Background(), h.icebathHome, map[string]any{"migrate_type": "xai", "apply": true}, &out, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v output %s", code, err, out.String())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "grok-3\n") || !strings.Contains(out.String(), "Backup: ") || !strings.Contains(out.String(), "Updated 1 slot(s) in ") {
		t.Fatalf("config %q output %q", raw, out.String())
	}
	clean := out.String()
	out.Reset()
	code, err = MigrateCommand(context.Background(), h.icebathHome, map[string]any{"migrate_type": "xai", "apply": true}, &out, io.Discard)
	if err != nil || code != 0 || !strings.Contains(out.String(), "No retired xAI models in config — nothing to migrate.") {
		t.Fatalf("second run code %d err %v output %q (first %q)", code, err, out.String(), clean)
	}
}
