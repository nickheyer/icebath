package tui

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Environment lookup over a fixed map
func envMap(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

// App on a temp home with a sized window and no gateway started
func testApp(t *testing.T) *app {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := newApp(t.Context())
	a.gw.bootstrap = func(context.Context, bool, bool) (localGrant, error) {
		return localGrant{}, errors.New("gateway unavailable: no owner in this test")
	}
	a.Update(tea.WindowSizeMsg{Width: 110, Height: 36})
	return a
}

// Runs a command and every command it returns that answers at once, feeding the messages back into the app
func drain(a *app, cmd tea.Cmd) {
	for depth := 0; cmd != nil && depth < 6; depth++ {
		m := cmd()
		batch, isBatch := m.(tea.BatchMsg)
		if !isBatch {
			if m != nil {
				_, cmd = a.Update(m)
				continue
			}
			return
		}
		var next []tea.Cmd
		for _, c := range batch {
			if c == nil {
				continue
			}
			switch inner := c().(type) {
			case nil:
			case tea.BatchMsg:
				next = append(next, inner...)
			case wm.Intent, asyncDoneMsg, dieMsg:
				_, more := a.Update(inner)
				next = append(next, more)
			}
		}
		cmd = tea.Batch(next...)
	}
}

// Every row of the transcript as plain text, the rows scrolled out of view included
func screen(a *app) string {
	a.card.View(104, 30)
	return ansi.Strip(strings.Join(a.card.lines, "\n"))
}

func TestStreamedTurnRendersInTheTranscript(t *testing.T) {
	a := testApp(t)
	a.Update(gatewayEventMsg{Type: "message.start", Payload: map[string]any{}})
	a.appendMessage(msg{Role: "user", Text: "what is in the folder"})
	a.Update(gatewayEventMsg{Type: "tool.start", Payload: map[string]any{"tool_id": "t1", "name": "read_file", "context": "notes.md"}})
	a.Update(gatewayEventMsg{Type: "tool.complete", Payload: map[string]any{"tool_id": "t1", "name": "read_file", "summary": "12 lines"}})
	a.Update(gatewayEventMsg{Type: "message.complete", Payload: map[string]any{"text": "The folder holds **notes.md** and nothing else."}})
	got := screen(a)
	for _, want := range []string{"what is in the folder", "Read File", "The folder holds notes.md and nothing else."} {
		if !strings.Contains(got, want) {
			t.Fatalf("screen lacks %q:\n%s", want, got)
		}
	}
	if a.store.ui.Busy {
		t.Fatal("turn still busy after message.complete")
	}
}

func TestLocalSlashCommandsChangeTheView(t *testing.T) {
	a := testApp(t)
	drain(a, a.dispatchSubmission("/details expanded"))
	if a.store.ui.DetailsMode != "expanded" || !a.store.ui.DetailsModeCommandOverride {
		t.Fatalf("details mode %q", a.store.ui.DetailsMode)
	}
	drain(a, a.dispatchSubmission("/mouse wheel"))
	if a.store.ui.MouseTracking != "wheel" {
		t.Fatalf("mouse tracking %q", a.store.ui.MouseTracking)
	}
	drain(a, a.dispatchSubmission("/help"))
	got := screen(a)
	for _, want := range []string{"/details expanded", "mouse tracking wheel", "/timestamps", "ctrl+o"} {
		if !strings.Contains(got, want) {
			t.Fatalf("screen lacks %q:\n%s", want, got)
		}
	}
}

func TestDestructiveCommandAsksThroughADialog(t *testing.T) {
	a := testApp(t)
	drain(a, a.dispatchSubmission("/new"))
	d, isDialog := a.wm.Overlay().(*dialogCard)
	if !isDialog || d.kind != "confirm" || !a.dialogs.waiting() {
		t.Fatalf("overlay %T, waiting %v", a.wm.Overlay(), a.dialogs.waiting())
	}
	a.Update(keys.Press("esc"))
	if a.store.overlay.Confirm != nil || a.dialogs.waiting() {
		t.Fatal("escape left the confirm dialog open")
	}
}

func TestApprovalDialogAnswersTheServerRequest(t *testing.T) {
	a := testApp(t)
	a.store.overlay.Approval = &approvalReq{RequestID: "req-1", Command: "rm -rf build", Description: "delete a folder", AllowPermanent: true}
	a.Update(struct{}{})
	d, isDialog := a.wm.Overlay().(*dialogCard)
	if !isDialog || d.kind != "approval" {
		t.Fatalf("overlay %T", a.wm.Overlay())
	}
	view := ansi.Strip(d.View(80, 12))
	for _, want := range []string{"rm -rf build", "Allow once", "Deny", "Always allow"} {
		if !strings.Contains(view, want) {
			t.Fatalf("dialog lacks %q:\n%s", want, view)
		}
	}
	drain(a, d.pick("deny"))
	if a.store.overlay.Approval != nil || a.store.turn.Outcome != tr("session.approval.denied") {
		t.Fatalf("approval %v outcome %q", a.store.overlay.Approval, a.store.turn.Outcome)
	}
}

func TestPasteCollapsesIntoATokenThatExpandsOnSubmit(t *testing.T) {
	a := testApp(t)
	long := strings.Repeat("line of pasted text\n", 40)
	a.insertPaste(strings.TrimRight(long, "\n"))
	if len(a.tokens) != 1 || a.tokens[0].Kind != "paste" || !strings.Contains(a.card.composer.Value(), a.tokens[0].Label) {
		t.Fatalf("tokens %+v draft %q", a.tokens, a.card.composer.Value())
	}
	text, _ := prepareSubmission(a.card.composer.Value(), a.tokens)
	if !strings.Contains(text, "line of pasted text") || strings.Contains(text, a.tokens[0].Label) {
		t.Fatalf("expanded %q", text[:min(len(text), 80)])
	}
}

func TestBuiltInWidgetsOpenFromTheirCommands(t *testing.T) {
	a := testApp(t)
	drain(a, a.dispatchSubmission("/ticker abc"))
	if len(a.widgets.ambient) != 1 || a.widgets.ambient[0].widget.ID != "ticker" {
		t.Fatalf("ambient widgets %+v", a.widgets.ambient)
	}
	if view := ansi.Strip(a.card.View(104, 30)); !strings.Contains(view, "/ticker") || !strings.Contains(view, "ABC") {
		t.Fatalf("ticker box missing:\n%s", view)
	}
	drain(a, a.dispatchSubmission("/ticker"))
	if len(a.widgets.ambient) != 0 {
		t.Fatal("bare command left the ambient widget open")
	}
	drain(a, a.dispatchSubmission("/grid-test 4x2"))
	card, isWidget := a.wm.Overlay().(*widgetCard)
	if !isWidget {
		t.Fatalf("overlay %T", a.wm.Overlay())
	}
	card.Update(keys.Press("right"))
	card.Update(keys.Press("space"))
	if g := card.active.state.(*gridState); g.cols != 4 || g.rows != 2 || g.x != 1 || !g.marked[[2]int{1, 0}] {
		t.Fatalf("grid %+v", g)
	}
	drain(a, a.dispatchSubmission("/grid-test 99"))
	if !strings.Contains(screen(a), "usage: /grid-test") {
		t.Fatal("refused size printed no usage")
	}
}

func TestEveryCatalogKeyTheAppNamesExists(t *testing.T) {
	keyRe := regexp.MustCompile(`\btr\("([A-Za-z0-9_.]+)"`)
	names := []string{"prompt.approval.once", "prompt.approval.session", "prompt.approval.always", "prompt.approval.deny",
		"slashCmd.wake.reason.disabled", "slashCmd.wake.reason.disabledForSurface", "slashCmd.wake.reason.notOwner", "slashCmd.wake.reason.owned",
		"slashCmd.wake.reason.unavailable", "slashCmd.ops.rollback.keptUserEdits", "slashCmd.ops.rollback.keptOversize", "slashCmd.ops.rollback.failedDeletes"}
	for _, file := range []string{"app.go", "slash.go", "slash_commands.go", "slash_session.go", "slash_control.go", "dialogs.go", "pickers.go", "composerio.go", "sessioncard.go", "transcriptview.go", "widgets.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range keyRe.FindAllStringSubmatch(string(src), -1) {
			if !strings.HasSuffix(m[1], ".") {
				names = append(names, m[1])
			}
		}
	}
	for _, name := range names {
		if !hasKey(name) {
			t.Errorf("catalog has no key %q", name)
		}
	}
}

func TestPlanUsageLinesShowEachWindow(t *testing.T) {
	now := float64(1_700_000_000_000)
	plan := map[string]any{"kind": "usage_window", "plan": "Pro", "gauges": []any{
		map[string]any{"title": "5 hour", "left_fraction": 0.75, "resets_at_ms": now + 90*60*1000},
		map[string]any{"label": "week", "left_fraction": 0.999, "resets_at_ms": now + 3*24*3600*1000},
		map[string]any{"label": "broken"},
	}}
	got := planUsageLines(plan, now)
	want := []string{"Pro", "5 hour  [|||       ]  25% used  ·  resets in 1h 30m", "week    [|         ]  <1% used  ·  resets in 3d 0h"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if groupDigits(1234567) != "1,234,567" || groupDigits(999) != "999" {
		t.Fatalf("grouping %s %s", groupDigits(1234567), groupDigits(999))
	}
}

func TestDropLastExchangeKeepsNotesAndEarlierTurns(t *testing.T) {
	items := []msg{{Role: "user", Text: "one"}, {Role: "assistant", Text: "first"}, {Role: "user", Text: "two"}, {Kind: "trail", Role: "system", Tools: []string{"Read ✓"}},
		{Role: "assistant", Text: "second"}, {Kind: "slash", Role: "system", Text: "/undo"}}
	got := dropLastExchange(items)
	if len(got) != 3 || got[1].Text != "first" || got[2].Text != "/undo" {
		t.Fatalf("rows %+v", got)
	}
	if rest := dropLastExchange([]msg{{Role: "system", Text: "note"}}); len(rest) != 1 {
		t.Fatalf("rows %+v", rest)
	}
}

func TestConnectionCardWalksTheTargets(t *testing.T) {
	a := testApp(t)
	a.store.applyConnectionRequest(map[string]any{"op_id": "op-1", "seq": float64(1), "deadline_at": float64(0), "targets": []any{
		map[string]any{"name": "github", "state": "connected", "action": "connect"},
		map[string]any{"name": "linear", "state": "pending", "action": "install", "description": "Issue tracker",
			"required_env": []any{map[string]any{"name": "LINEAR_API_KEY", "secret": true, "required": true, "default": ""}}},
	}})
	a.Update(struct{}{})
	d, isDialog := a.wm.Overlay().(*dialogCard)
	if !isDialog || d.kind != "connection" {
		t.Fatalf("overlay %T", a.wm.Overlay())
	}
	view := ansi.Strip(d.View(80, 14))
	for _, want := range []string{"● github · connected", "○ linear · pending", "Issue tracker", "install linear", "skip linear", "LINEAR_API_KEY"} {
		if !strings.Contains(view, want) {
			t.Fatalf("card lacks %q:\n%s", want, view)
		}
	}
	target := a.activeConnectionTarget()
	approved := connectionAnswer(target, "approved", []string{"lin_key"})
	row := approved["targets"].([]any)[0].(map[string]any)
	if row["name"] != "linear" || row["status"] != "approved" || row["env"].(map[string]any)["LINEAR_API_KEY"] != "lin_key" {
		t.Fatalf("answer %v", approved)
	}
	if skipped := connectionAnswer(target, "skipped", nil)["targets"].([]any)[0].(map[string]any); skipped["status"] != "skipped" || skipped["env"] != nil {
		t.Fatalf("skip %v", skipped)
	}
	a.store.applyConnectionUpdate(map[string]any{"op_id": "op-1", "seq": float64(2), "targets": []any{
		map[string]any{"name": "github", "state": "connected"}, map[string]any{"name": "linear", "state": "initiated", "connect_url": "https://linear.app/oauth"}}})
	a.Update(struct{}{})
	next, _ := a.wm.Overlay().(*dialogCard)
	if next == nil || next == d || !strings.Contains(ansi.Strip(next.View(80, 14)), "https://linear.app/oauth") {
		t.Fatalf("card did not follow the update: %v", next)
	}
	if done := connectionAnswer(nil, "", nil); done["settled_by"] != "continue" {
		t.Fatalf("continue %v", done)
	}
}

func TestPromptStaysInViewWhileItsReplyScrolls(t *testing.T) {
	a := testApp(t)
	a.appendMessage(msg{Role: "user", Text: "explain the scheduler"})
	a.appendMessage(msg{Role: "assistant", Text: strings.Repeat("A paragraph of the answer.\n\n", 60)})
	a.card.View(104, 20)
	view := ansi.Strip(a.card.View(104, 20))
	if first := strings.SplitN(view, "\n", 2)[0]; !strings.Contains(first, "explain the scheduler") {
		t.Fatalf("first row %q", first)
	}
	a.card.view.scrollTop()
	if got := a.card.view.stickyPrompt(a.card.view.top()); got != "" {
		t.Fatalf("prompt pinned at the top of the transcript: %q", got)
	}
}
