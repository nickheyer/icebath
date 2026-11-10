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

	"github.com/nickheyer/icebath/pkg/plugins"
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
	prevAsk, prevPick, prevInstall, prevEnable, prevRun, prevExe, prevEnsure := agentImportAsk, agentImportChecklist, agentImportInstall, agentImportEnable, agentImportRunChild, agentImportSelfExe, agentImportEnsure
	t.Cleanup(func() {
		agentImportAsk, agentImportChecklist, agentImportInstall, agentImportEnable, agentImportRunChild, agentImportSelfExe, agentImportEnsure = prevAsk, prevPick, prevInstall, prevEnable, prevRun, prevExe, prevEnsure
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
	agentImportInstall = func(ctx context.Context, name string, out io.Writer) error {
		h.calls = append(h.calls, "install|"+name+"|true|true")
		return os.MkdirAll(filepath.Join(h.icebathHome, "plugins", name), 0o755)
	}
	agentImportSelfExe = func() (string, error) { return "/opt/icebath/bin/icebath", nil }
	agentImportRunChild = func(argv []string, env map[string]string) (int, error) {
		h.calls = append(h.calls, "import|"+strings.Join(argv[1:], " ")+"|"+env["ICEBATH_HOME"])
		if err := os.WriteFile(filepath.Join(h.icebathHome, "config.yaml"), []byte("imported: "+argv[len(argv)-1]+"\n"), 0o644); err != nil {
			return 0, err
		}
		return h.childCode, nil
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

func TestFoundAgentIsOfferedThenInstalledFromTheCatalogAndImported(t *testing.T) {
	h := newAgentImportHarness(t)
	h.mkdir(t, ".openclaw")
	if got := offer(t, h); got != "OpenClaw" {
		t.Fatalf("got %q", got)
	}
	want := []string{"ask|Import your setup from OpenClaw?|true", "install|agent-importers|true|true",
		"import|--profile default import-agent openclaw --migrate-secrets --from-setup|" + h.icebathHome}
	if strings.Join(h.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls %v", h.calls)
	}
}

func TestSeveralFoundAreOfferedAsAChecklistAndOnlyThePicksImport(t *testing.T) {
	h := newAgentImportHarness(t)
	h.mkdir(t, ".claude")
	h.mkdir(t, ".cursor")
	h.unpick["Cursor"] = true
	if got := offer(t, h); got != "Claude Code" {
		t.Fatalf("got %q", got)
	}
	if h.calls[1] != "pick|Claude Code,Cursor|[0 1]" {
		t.Fatalf("pick call %q", h.calls[1])
	}
	want := []string{"install|agent-importers|true|true", "import|--profile default import-agent claude-code --from-setup|" + h.icebathHome}
	if strings.Join(h.calls[2:], "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls %v", h.calls)
	}
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

func TestARefusedEnableSkipsTheImportAndSetupGoesOn(t *testing.T) {
	h := newAgentImportHarness(t)
	h.mkdir(t, ".cursor")
	if err := os.MkdirAll(filepath.Join(h.icebathHome, "plugins", "agent-importers"), 0o755); err != nil {
		t.Fatal(err)
	}
	agentImportEnable = func(ctx context.Context, name string, out io.Writer) error {
		h.calls = append(h.calls, "enable|"+name)
		return &plugins.AdmissionRefused{Msg: "dependency conflict"}
	}
	if got := offer(t, h); got != "" {
		t.Fatalf("got %q", got)
	}
	if strings.Join(h.calls, "\n") != "ask|Import your setup from Cursor?|true\nenable|agent-importers" {
		t.Fatalf("calls %v", h.calls)
	}
}

func TestSetupMenuShowsWhatWasFoundWithTheQuestion(t *testing.T) {
	h := newAgentImportHarness(t)
	for _, d := range []string{".openclaw", ".claude", ".codex"} {
		h.mkdir(t, d)
	}
	agentImportAsk = PromptYesNo
	var menus []string
	restore := StubSetupRadiolist(func(question string, choices []string, def int, description string) int {
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

func TestGrokBotExportInDownloadsIsOfferedLikeTheOtherAgents(t *testing.T) {
	h := newAgentImportHarness(t)
	h.mkdir(t, "Downloads")
	if err := os.WriteFile(filepath.Join(h.userHome, "Downloads", "grok-bot-export.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := offer(t, h); got != "Grok Bot" {
		t.Fatalf("got %q", got)
	}
	if last := h.calls[len(h.calls)-1]; last != "import|--profile default import-agent grok-bot --from-setup|"+h.icebathHome {
		t.Fatalf("last call %q", last)
	}
}

func TestIcebathMigrateNamesAnAgentInstallsThePluginAndForwardsItsFlags(t *testing.T) {
	h := newAgentImportHarness(t)
	ns := map[string]any{"migrate_type": "grok-bot", "migrate_agent": "grok-bot", "source": "~/export.json", "activate_routines": true,
		"bot": []string{"Scout"}, "yes": true, "dry_run": false, "overwrite": false}
	var out, errOut bytes.Buffer
	code, err := MigrateCommand(context.Background(), h.icebathHome, ns, &out, &errOut)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	want := []string{"install|agent-importers|true|true",
		"import|--profile default import-agent grok-bot --yes --activate-routines --source " + filepath.Join(h.userHome, "export.json") + " --bot Scout|" + h.icebathHome}
	if strings.Join(h.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls %v", h.calls)
	}
	h.calls = nil
	agentImportEnsure = func(context.Context, string) (bool, error) { return true, nil }
	code, err = MigrateCommand(context.Background(), h.icebathHome, map[string]any{"migrate_type": "openclaw", "migrate_agent": "openclaw", "yes": true}, &out, &errOut)
	if err != nil || code != 0 || h.calls[len(h.calls)-1] != "import|--profile default import-agent openclaw --yes|"+h.icebathHome {
		t.Fatalf("code %d err %v calls %v", code, err, h.calls)
	}
	h.childCode = 1
	code, err = MigrateCommand(context.Background(), h.icebathHome, ns, &out, &errOut)
	if err != nil || code != 1 {
		t.Fatalf("code %d err %v after a failed import", code, err)
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
