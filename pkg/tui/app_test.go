package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Environment lookup over a fixed map
func envMap(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

// App on a temp home with a sized window and no gateway started
func testApp(t *testing.T) *app {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	a := newApp(t.Context(), Host{})
	a.gw.bootstrap = func(context.Context, bool, bool) (Grant, error) {
		return Grant{}, errors.New("gateway unavailable: no owner in this test")
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
			case sessionMsg:
				switch inner.m.(type) {
				case asyncDoneMsg, dieMsg:
					_, more := a.Update(inner)
					next = append(next, more)
				}
			}
		}
		cmd = tea.Batch(next...)
	}
}

// Text of the whole window as the terminal shows it
func frame(a *app) string { return ansi.Strip(a.View().Content) }

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
	drain(a, a.dispatchSubmission("/detail detailed"))
	if a.store.ui.DetailsMode != "expanded" || !a.store.ui.DetailsModeCommandOverride {
		t.Fatalf("details mode %q", a.store.ui.DetailsMode)
	}
	drain(a, a.dispatchSubmission("/mouse wheel"))
	if a.store.ui.MouseTracking != "wheel" {
		t.Fatalf("mouse tracking %q", a.store.ui.MouseTracking)
	}
	got := screen(a)
	for _, want := range []string{"/detail detailed", "mouse tracking wheel"} {
		if !strings.Contains(got, want) {
			t.Fatalf("screen lacks %q:\n%s", want, got)
		}
	}
	drain(a, a.dispatchSubmission("/help"))
	help, isList := a.wm.Overlay().(*cards.List)
	if !isList {
		t.Fatalf("/help opened %T", a.wm.Overlay())
	}
	if view := ansi.Strip(help.View(160, 400)); !strings.Contains(view, "/timestamps") {
		t.Fatalf("help lacks /timestamps:\n%s", view)
	}
}

func TestApprovalDialogAnswersTheServerRequest(t *testing.T) {
	t.Setenv("ICEBATH_TUI_REDUCED_MOTION", "1")
	a := testApp(t)
	g := attachScripted(t, a, nil)
	req := &serverRequest{ID: "req-1", Method: tuigateway.ApprovalRequest.Name(), Params: map[string]any{"command": "rm -rf build", "description": "delete a folder"}, client: a.gw}
	a.Update(serverRequestMsg{Request: req})
	d, isDialog := a.wm.Sheet(a.card).(*dialogCard)
	if !isDialog || d.kind != "approval" {
		t.Fatalf("sheet of the session card %T", a.wm.Sheet(a.card))
	}
	shown := frame(a)
	for _, want := range []string{"rm -rf build", "delete a folder", "Allow once", "Deny", "Always allow"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("screen lacks %q:\n%s", want, shown)
		}
	}
	a.Update(keys.Press("y"))
	if a.store.overlay.Approval == nil || a.card.composer.Value() != "y" {
		t.Fatalf("a key typed as the approval appeared did not go to the prompt: approval %v draft %q", a.store.overlay.Approval, a.card.composer.Value())
	}
	time.Sleep(wm.SheetArmDelay + 50*time.Millisecond)
	a.Update(keys.Press("n"))
	if a.store.overlay.Approval != nil || a.store.turn.Outcome != tr(txt.SessionApprovalDenied) {
		t.Fatalf("approval %v outcome %q", a.store.overlay.Approval, a.store.turn.Outcome)
	}
	if a.wm.Sheet(a.card) != nil || strings.Contains(frame(a), "rm -rf build") {
		t.Fatalf("the answered approval stayed on screen:\n%s", frame(a))
	}
	eventually(t, "the verdict on the wire", func() bool {
		for _, sent := range g.s.sentFrames() {
			if result, answered := sent["result"].(map[string]any); answered && sent["id"] == "req-1" {
				return result["choice"] == "deny"
			}
		}
		return false
	})
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
	card.Update(keys.Press("enter"))
	if g := card.active.state.(*gridState); g.cols != 4 || g.rows != 2 || g.col != 1 || !g.zoomed {
		t.Fatalf("grid %+v", g)
	}
	drain(a, a.dispatchSubmission("/grid-test 99"))
	if !strings.Contains(screen(a), "usage: /grid-test") {
		t.Fatal("refused size printed no usage")
	}
}

func TestGroupDigitsSeparatesThousands(t *testing.T) {
	if groupDigits(1234567) != "1,234,567" || groupDigits(999) != "999" {
		t.Fatalf("grouping %s %s", groupDigits(1234567), groupDigits(999))
	}
}

func TestDropLastExchangeKeepsNotesAndEarlierTurns(t *testing.T) {
	items := []msg{{Role: "user", Text: "one"}, {Role: "assistant", Text: "first"}, {Role: "user", Text: "two"}, {Kind: "trail", Role: "system", Tools: []toolRow{{Call: "Read"}}},
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
	t.Setenv("ICEBATH_TUI_REDUCED_MOTION", "1")
	a := testApp(t)
	g := attachScripted(t, a, map[string]func(map[string]any) map[string]any{
		"connection.respond": fixed(map[string]any{"status": "ok", "settled": false}),
	})
	a.store.ui.Sid = "s1"
	opened := tuigateway.ConnectionRequestPayload{OpID: "op-1", Seq: 1, Targets: []tuigateway.ConnectionOperationTarget{
		{Name: "github", Kind: "mcp", State: "connected", Action: "authorize"},
		{Name: "linear", Kind: "mcp", State: "pending", Action: "install", Description: ptr("Issue tracker"),
			RequiredEnv: []tuigateway.ConnectionTargetEnvField{{Name: "LINEAR_API_KEY", Secret: true, Required: true}}},
	}}
	a.Update(gatewayEventMsg{Type: tuigateway.ConnectionRequestEvent.Name(), SessionID: "s1", Payload: tuigateway.ConnectionRequestEvent.Wire(opened)})
	d, isDialog := a.wm.Sheet(a.card).(*dialogCard)
	if !isDialog || d.kind != "connection" {
		t.Fatalf("sheet of the session card %T", a.wm.Sheet(a.card))
	}
	shown := frame(a)
	for _, want := range []string{"● github · connected", "○ linear · pending", "Issue tracker", "Install linear", "Skip", "LINEAR_API_KEY"} {
		if !strings.Contains(shown, want) {
			t.Fatalf("screen lacks %q:\n%s", want, shown)
		}
	}
	target := a.activeConnectionTarget()
	if skipped := connectionAnswer(target, "skipped", nil).Targets[0]; skipped.Name != "linear" || skipped.Status != "skipped" || skipped.Env != nil {
		t.Fatalf("skip %+v", skipped)
	}
	time.Sleep(wm.SheetArmDelay + 50*time.Millisecond)
	for _, key := range []string{"l", "i", "n", "_", "k", "e", "y"} {
		a.Update(keys.Press(key))
	}
	_, answered := a.Update(keys.Press("enter"))
	settle(a, answered)
	sent := g.sent("connection.respond")
	if len(sent) != 1 || sent[0]["op_id"] != "op-1" {
		t.Fatalf("answers on the wire %v", sent)
	}
	owner, _ := sent[0]["owner"].(map[string]any)
	result, _ := sent[0]["result"].(map[string]any)
	rows, _ := result["targets"].([]any)
	if owner["session_id"] != "s1" || len(rows) != 1 {
		t.Fatalf("answer %v", sent[0])
	}
	row, _ := rows[0].(map[string]any)
	env, _ := row["env"].(map[string]any)
	if row["name"] != "linear" || row["status"] != "approved" || env["LINEAR_API_KEY"] != "lin_key" {
		t.Fatalf("answer %v", sent[0])
	}
	moved := tuigateway.ConnectionUpdatePayload{Owner: tuigateway.SessionOwner{Type: "session", SessionID: "s1"}, ConnectionOperationSnapshot: tuigateway.ConnectionOperationSnapshot{OpID: "op-1", Seq: 2,
		Targets: []tuigateway.ConnectionOperationTarget{{Name: "github", Kind: "mcp", Action: "authorize", State: "connected"},
			{Name: "linear", Kind: "mcp", Action: "install", State: "initiated", ConnectURL: ptr("https://linear.app/oauth")}}}}
	a.Update(gatewayEventMsg{Type: tuigateway.ConnectionUpdateEvent.Name(), SessionID: "s1", Payload: tuigateway.ConnectionUpdateEvent.Wire(moved)})
	next, _ := a.wm.Sheet(a.card).(*dialogCard)
	if next == nil || next == d || !strings.Contains(frame(a), "https://linear.app/oauth") {
		t.Fatalf("card did not follow the update: %v\n%s", next, frame(a))
	}
	if done := connectionAnswer(nil, "", nil); val(done.SettledBy) != "continue" {
		t.Fatalf("continue %+v", done)
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
