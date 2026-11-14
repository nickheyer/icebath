package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Streams one reasoning block and one answer at the given width and returns every print
func runGlyphTurn(t *testing.T, width string, modern bool) []string {
	t.Helper()
	rec := recordPrints(t)
	t.Setenv("COLUMNS", width)
	c := bareCLI()
	c.modernLayout = modern
	c.showReasoning = true
	c.streamReasoningDelta("**Planning the change**\nI will read calc.py first.\n")
	c.streamDelta("Added multiply to calc.py.\n")
	c.flushStream()
	return rec.plain()
}

func TestGlyphLayoutScrollbackIsWidthIndependent(t *testing.T) {
	for _, modern := range []bool{true, false} {
		narrow, wide := runGlyphTurn(t, "60", modern), runGlyphTurn(t, "120", modern)
		boxed := false
		for _, line := range wide {
			boxed = boxed || strings.ContainsAny(line, "╭╮╰╯┌┐└┘")
		}
		if !modern {
			if !boxed || slices.Equal(narrow, wide) {
				t.Fatalf("classic frames must follow the terminal width: %q", wide)
			}
			continue
		}
		if !slices.Equal(narrow, wide) || boxed {
			t.Fatalf("glyph rows depend on the width: %q vs %q", narrow, wide)
		}
		rows := strings.Split(strings.Join(wide, "\n"), "\n")
		var thought []string
		for _, line := range rows {
			if strings.HasPrefix(line, "∴") {
				thought = append(thought, line)
			}
		}
		if len(thought) != 1 || strings.Contains(thought[0], "·") {
			t.Fatalf("thought rows %q in %q", thought, rows)
		}
		bar := strings.Repeat("─", 40)
		start := slices.Index(rows, "**Planning the change**")
		if start < 1 || rows[start-1] != bar || rows[start+1] != "I will read calc.py first." || rows[start+2] != thought[0] {
			t.Fatalf("reasoning rows %q", rows)
		}
		brand := slices.IndexFunc(rows, func(r string) bool { return strings.HasPrefix(r, "☤ Icebath") })
		if brand < 1 || rows[brand-1] != bar {
			t.Fatalf("answer rows %q", rows)
		}
		if !slices.ContainsFunc(rows, func(r string) bool { return strings.Contains(r, "Added multiply to calc.py.") }) {
			t.Fatalf("answer text lost in %q", rows)
		}
	}
}

func TestOneLineReasoningIsShownOnce(t *testing.T) {
	for _, reasoning := range []string{
		"scripted reasoning about gamma zeta-reason",
		"scripted reasoning about gamma zeta-reason that keeps going well past any short title length here",
		"I will check the failure first. Then I will compare it with main.",
	} {
		rec := recordPrints(t)
		t.Setenv("COLUMNS", "102")
		c := bareCLI()
		c.modernLayout, c.showReasoning = true, true
		c.streamReasoningDelta(reasoning)
		c.streamDelta("GAMMA answered.\n")
		c.flushStream()
		rows := strings.Split(strings.Join(rec.plain(), "\n"), "\n")
		shown, thought := 0, []string{}
		for _, row := range rows {
			shown += strings.Count(row, reasoning)
			if strings.HasPrefix(row, "∴") {
				thought = append(thought, row)
			}
		}
		if shown != 1 || len(thought) != 1 || strings.Contains(thought[0], "·") {
			t.Fatalf("reasoning shown %d times, thought %q, rows %q", shown, thought, rows)
		}
	}
}

func TestLayoutAndBusyDefaultThroughTheConfigLoader(t *testing.T) {
	cases := []struct {
		yaml   string
		modern bool
		busy   string
	}{
		{"model: {}\n", true, "interrupt"},
		{"display:\n  cli_layout: classic\n", false, "interrupt"},
		{"display:\n  busy_input_mode: queue\n", true, "queue"},
		{"display:\n  busy_input_mode: steer\n  cli_layout: classic\n", false, "steer"},
	}
	for _, tc := range cases {
		h := filepath.Join(t.TempDir(), ".icebath")
		if err := os.MkdirAll(h, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ICEBATH_HOME", h)
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(tc.yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, _ := LoadCLIConfig(context.Background())["display"].(map[string]any)
		if layoutFromConfig(loaded) != tc.modern || busyInputModeFromConfig(loaded) != tc.busy {
			t.Fatalf("%q gave layout %v busy %q", tc.yaml, layoutFromConfig(loaded), busyInputModeFromConfig(loaded))
		}
	}
}

func TestLongUserMessageSoftWrapsSoAResizeReflowsIt(t *testing.T) {
	longLine := strings.TrimSpace(strings.Repeat("word ", 60))
	for _, text := range []string{longLine, longLine + "\nsecond line"} {
		out := map[string][]string{}
		for _, width := range []string{"60", "140"} {
			rec := recordPrints(t)
			t.Setenv("COLUMNS", width)
			c := bareCLI()
			c.modernLayout = true
			c.printUserMessagePreview(text)
			for _, line := range strings.Split(strings.Join(rec.plain(), "\n"), "\n") {
				if strings.TrimSpace(line) != "" {
					out[width] = append(out[width], line)
				}
			}
		}
		if !slices.Equal(out["60"], out["140"]) {
			t.Fatalf("echo depends on the width: %q vs %q", out["60"], out["140"])
		}
		if !slices.ContainsFunc(out["60"], func(line string) bool {
			return strings.HasSuffix(strings.TrimRight(line, " "), "word") && strings.Count(line, "word") == 60
		}) {
			t.Fatalf("long line was broken: %q", out["60"])
		}
	}
}

func TestToolRowsKeepTheClassicToolEmoji(t *testing.T) {
	recordPrints(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"read_file", map[string]any{"path": "/etc/hostname"}}, {"terminal", map[string]any{"command": "echo ok"}},
		{"execute_code", map[string]any{"code": "import math"}}, {"clarify", map[string]any{"question": "Which?"}},
	} {
		classicEmoji := strings.Fields(tools.GetCuteToolMessage(ctx, nil, tc.name, tc.args, 0.2, nil))[1]
		if row := plainSGR(toolRow(ctx, nil, tc.name, tc.args, 0.2, nil, false)); !strings.HasPrefix(row, classicEmoji+" ") {
			t.Fatalf("%s row %q does not lead with %q", tc.name, row, classicEmoji)
		}
	}
	failed := plainSGR(toolRow(ctx, nil, "terminal", map[string]any{"command": "false"}, 0.2, `{"exit_code": 1, "output": ""}`, false))
	if !strings.HasPrefix(failed, "✗ ") || !strings.Contains(failed, "[exit 1]") {
		t.Fatalf("failed row %q", failed)
	}
}

func TestRefusedSkillWriteReadsNeutralNotFailed(t *testing.T) {
	recordPrints(t)
	ctx := context.Background()
	refused := `{"success": false, "error": "operations[0] (patch on 'hermes-agent-dev') failed: ` +
		`SKILL.md content is 100,193 characters (limit: 100,000).", ` +
		`"file_preview": "Never ignore a ROLLBACK FAILED note."}`
	row := toolRow(ctx, nil, "skill_manage", map[string]any{"operations": []any{map[string]any{"action": "patch", "name": "hermes-agent-dev"}}}, 1.1, refused, false)
	if !strings.HasPrefix(plainSGR(row), tools.GetToolEmoji(nil, "skill_manage", "⚡")+" ") {
		t.Fatalf("row %q", row)
	}
	if !strings.Contains(plainSGR(row), "[operations[0]") || strings.Contains(row, modernRed) {
		t.Fatalf("row %q", row)
	}
	broken := strings.Replace(refused, `(limit: 100,000)."`, `(limit: 100,000). — batch aborted, ROLLBACK FAILED for demo."`, 1)
	if got := plainSGR(toolRow(ctx, nil, "skill_manage", map[string]any{"operations": []any{}}, 1.1, broken, false)); !strings.HasPrefix(got, "✗ ") {
		t.Fatalf("a failed rollback must stay a failure: %q", got)
	}
}

func TestEveryBlockIsFramedByBarsAndBlankLines(t *testing.T) {
	rec := recordPrints(t)
	c := bareCLI()
	c.modernLayout = true
	bar := strings.Repeat("─", 40)
	two := 2.0
	c.printUserMessagePreview("read the hostname")
	for _, row := range thoughtRows(&two, "Plan the read.", true) {
		cprint(row)
	}
	cprint(completedToolLine(c, "read_file", map[string]any{"path": "/etc/hostname"}, 0.2, nil, false))
	cprint(completedToolLine(c, "terminal", map[string]any{"command": "echo hi"}, 0.1, nil, false))
	cprint(answerLabel(""))
	cprint("It is LINUX.")
	cprint(closingSeparator(true))
	cprint(turnFooter("1.0s"))
	var rows []string
	for _, entry := range rec.plain() {
		for _, r := range strings.Split(entry, "\n") {
			rows = append(rows, strings.TrimRight(r, " \t"))
		}
	}
	start := slices.Index(rows, "❯ read the hostname") - 1
	if start < 1 {
		t.Fatalf("user row missing in %q", rows)
	}
	var got []string
	for _, r := range rows[start-1:] {
		switch {
		case r == bar:
			got = append(got, "BAR")
		case r == "":
			got = append(got, "")
		default:
			got = append(got, strings.Fields(r)[0])
		}
	}
	want := []string{"", "BAR", "❯", "BAR", "", "BAR", "Plan", "∴", "BAR", "", "📖", "💻", "", "BAR", "☤", "It", "BAR", "", "─"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestPanelRowsNeverCarryANewline(t *testing.T) {
	rows := wrapPanelTextKeepWS("python3 - <<'PY'\nprint(sum(range(10)))\nPY", 30, "")
	if !slices.Equal(rows, []string{"python3 - <<'PY'", "print(sum(range(10)))", "PY"}) {
		t.Fatalf("rows %q", rows)
	}
}
