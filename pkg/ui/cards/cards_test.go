package cards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/agent/hooks"
	"github.com/nickheyer/icebath/pkg/agent/subagent"
	"github.com/nickheyer/icebath/pkg/facts"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/mission"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/policy/mode"
	"github.com/nickheyer/icebath/pkg/policy/rules"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/tool/files"
	"github.com/nickheyer/icebath/pkg/tool/shell"
	"github.com/nickheyer/icebath/pkg/ui/art"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

var at = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

type sink struct {
	mu   sync.Mutex
	msgs []any
}

func (s *sink) send(m any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = append(s.msgs, m)
}

// Runs a command and collects every message it produces, batches flattened
func run(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, run(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

func find[T any](msgs []tea.Msg) (T, bool) {
	for _, m := range msgs {
		if v, ok := m.(T); ok {
			return v, true
		}
	}
	var zero T
	return zero, false
}

// Key press a terminal reports for the chord
func keyp(chord string) tea.KeyPressMsg { return keys.Press(chord) }

// Delivers the chord the way the manager does, as the action the keymap binds it to in the card's contexts, else as typed text
func (te *testEnv) press(c wm.Card, chord string) (wm.Card, tea.Cmd) {
	msg := keyp(chord)
	var ctxs []string
	if k, ok := c.(wm.Contexter); ok {
		ctxs = k.Contexts()
	}
	raw := false
	if r, ok := c.(wm.Raw); ok {
		raw = r.Raw()
	}
	for _, id := range te.env.Keys.Lookup(ctxs, msg) {
		a, _ := te.env.Keys.Action(id)
		if slices.Contains(ctxs, a.Context) {
			return c.Update(wm.ActionMsg{ID: id, Key: msg})
		}
		if !raw {
			return c, nil
		}
	}
	return c.Update(msg)
}

// Messages produced by pressing the chord on the card
func (te *testEnv) pressed(c wm.Card, chord string) []tea.Msg {
	_, cmd := te.press(c, chord)
	return run(cmd)
}

// Delivers the chord to a bare composer as the session card would
func (te *testEnv) compose(c *Composer, chord string) tea.Cmd {
	msg := keyp(chord)
	ctxs := []string{keys.Session}
	if c.Completing() {
		ctxs = []string{keys.Complete, keys.Session}
	}
	for _, id := range te.env.Keys.Lookup(ctxs, msg) {
		if a, _ := te.env.Keys.Action(id); slices.Contains(ctxs, a.Context) {
			return c.Action(id)
		}
	}
	return c.Update(msg)
}

func typeText(c wm.Card, text string) wm.Card {
	for _, r := range text {
		c, _ = c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return c
}

func plain(s string) string { return ansi.Strip(s) }

// Model that streams one text answer, holding the stream open while hold is unclosed
type stubModel struct {
	text string
	hold chan struct{}
}

func (m stubModel) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		if m.hold != nil {
			select {
			case <-m.hold:
			case <-ctx.Done():
				yield(protocol.Event{}, ctx.Err())
				return
			}
		}
		resp := protocol.Response{Model: "m-test", Content: []protocol.Part{protocol.Text{Text: m.text}}, StopReason: protocol.StopEnd, Usage: protocol.Usage{Input: 3, Output: 2}}
		if !yield(protocol.Event{Type: protocol.EventTextDelta, Delta: m.text, Response: resp}, nil) {
			return
		}
		yield(protocol.Event{Type: protocol.EventDone, Response: resp}, nil)
	}
}

type testEnv struct {
	env      *Env
	sink     *sink
	live     *harness.Live
	dir      string
	sh       *shell.Shell
	app      *harness.App
	settings map[string]string
}

// Builds an environment over a shell, file tools, a rules policy and one open session with its stores attached
func newEnv(t *testing.T) *testEnv {
	t.Helper()
	s := &sink{}
	dir := t.TempDir()
	sh, err := shell.New(shell.Options{Dir: dir, OnJob: func(ev shell.JobEvent) { s.send(harness.JobMsg{Event: ev}) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sh.Close)
	t.Setenv(profiles.ConfigDirEnv, filepath.Join(dir, "user"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	reg := &tool.Registry{}
	reg.Add(files.All(files.Options{Root: dir})...)
	pol, _ := rules.New()
	sess := session.New()
	fs := facts.New()
	fs.Attach(sess)
	ms := mission.New()
	ms.Attach(sess)
	live := &harness.Live{Session: sess, Facts: fs, Missions: ms}
	pump := harness.NewPump(s.send)
	ctl := harness.NewController(s.send, pump)
	ctl.Missions = ms
	meter := harness.NewMeter(s.send)
	opus, _ := profiles.Describe("claude-opus-5")
	meter.SetCatalog(opus, "anthropic")
	journal := harness.NewJournal()
	mcpm := harness.NewMCP(journal.Tap(s.send), reg)
	anthropic, _ := profiles.Lookup("anthropic")
	app := &harness.App{Dir: dir, Rules: pol, Tools: reg, MCP: mcpm, Journal: journal, Primary: harness.Endpoint{Profile: anthropic, Model: "claude-opus-5", Catalog: opus}}
	te := &testEnv{sink: s, live: live, dir: dir, sh: sh, app: app, settings: map[string]string{"max_tokens": "16000", "theme": "blackwall", "motion": "1.00", "cascade": "false"}}
	te.env = &Env{
		Theme: theme.Blackwall(), Keys: keys.Default(), Control: ctl, Meter: meter, Work: harness.NewWorkspace(dir),
		Approver: harness.NewApprover(s.send), MCP: mcpm, Shell: sh, Tools: reg,
		Mode: mode.Compose(mode.Default, pol, reg), Store: &session.Memory{}, Files: files.Options{Dir: dir, Root: dir},
		Journal: journal, Editor: app, Ctx: context.Background(),
		Lives:   func() []*harness.Live { return []*harness.Live{live} },
		Primary: func() harness.Endpoint { return app.Primary },
		Models: func(ctx context.Context, p profiles.Profile) ([]profiles.Model, error) {
			return []profiles.Model{{ID: "claude-opus-5"}, {ID: "other"}}, nil
		},
		Settings:   func() map[string]string { return maps.Clone(te.settings) },
		ProjectDir: dir,
		Now:        func() time.Time { return at },
	}
	return te
}

// Starts a root run on the live session and waits for it unless hold keeps it open
func (te *testEnv) startRun(t *testing.T, hold chan struct{}, text string) *harness.Run {
	t.Helper()
	ag := &agent.Agent{Name: "root", Model: stubModel{text: text, hold: hold}, ModelID: "m-test", Session: te.live.Session}
	r, err := te.env.Control.Start(context.Background(), ag, "", []protocol.Part{protocol.Text{Text: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if hold == nil {
		te.waitRun(t, r)
	}
	return r
}

func (te *testEnv) waitRun(t *testing.T, r *harness.Run) {
	t.Helper()
	done, _ := te.env.Control.Done(r.ID)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run never finished")
	}
}

func TestComposerSendsCommandsAndKeepsHistory(t *testing.T) {
	te := newEnv(t)
	dir := filepath.Join(te.dir, "cfg")
	c := NewComposer(te.env.Theme, te.env.Keys, dir, func() string { return te.dir })
	c.Bind("s1")
	c.SetCommands([]Command{{Name: "model", Desc: "switch the model", Arg: "id"}, {Name: "mode", Desc: "mode"}})
	if c.Cursor() != nil {
		t.Fatal("unfocused composer must hide the cursor")
	}
	run(c.SetFocus(true))
	if c.Cursor() == nil {
		t.Fatal("focused composer shows a cursor")
	}
	for _, r := range "hello" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if c.Height() != 2 {
		t.Fatalf("one line of text keeps the two row prompt, height %d", c.Height())
	}
	te.compose(c, "shift+enter")
	if c.Value() != "hello" {
		t.Fatalf("shift+enter is no newline %q", c.Value())
	}
	c.Update(tea.KeyPressMsg{Code: '\\', Text: "\\"})
	if run(te.compose(c, "enter")) != nil {
		t.Fatal("enter after a backslash sends nothing")
	}
	for _, r := range "two" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if c.Height() != 2 || c.ta.LineCount() != 2 || c.Value() != "hello\ntwo" {
		t.Fatalf("backslash newline height %d lines %d %q", c.Height(), c.ta.LineCount(), c.Value())
	}
	msgs := run(te.compose(c, "enter"))
	sub, ok := find[SubmitMsg](msgs)
	if !ok || sub.Session != "s1" || sub.Text != "hello\ntwo" || len(sub.Parts) != 1 {
		t.Fatalf("submit %+v", sub)
	}
	if c.Value() != "" || len(c.History()) != 1 {
		t.Fatalf("value %q history %v", c.Value(), c.History())
	}
	for _, r := range "/mode plan" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	cmd, ok := find[CommandMsg](run(te.compose(c, "enter")))
	if !ok || cmd.Session != "s1" || cmd.Name != "mode" || cmd.Args != "plan" {
		t.Fatalf("command %+v", cmd)
	}
	te.compose(c, "up")
	if c.Value() != "/mode plan" {
		t.Fatalf("recall newest %q", c.Value())
	}
	te.compose(c, "up")
	if c.Value() != "hello\ntwo" {
		t.Fatalf("recall older %q", c.Value())
	}
	if !strings.Contains(c.Status(), "history 1/2") {
		t.Fatalf("status %q", c.Status())
	}
	te.compose(c, "down")
	te.compose(c, "down")
	te.compose(c, "down")
	if c.Value() != "" {
		t.Fatalf("draft back %q", c.Value())
	}
	for _, r := range "/mo" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if c.comp == nil || len(c.comp.items) != 2 || !c.Completing() {
		t.Fatalf("slash completion opens as the command is typed %+v", c.comp)
	}
	if c.Height() != 4 {
		t.Fatalf("popup height %d", c.Height())
	}
	if cur := c.Cursor(); cur == nil || cur.Y != 2 {
		t.Fatalf("cursor below the popup %+v", cur)
	}
	if !strings.Contains(plain(c.View(40, 4)), "/model") {
		t.Fatal("popup lists the commands")
	}
	te.compose(c, "tab")
	if c.comp.items[c.comp.idx] != "/mode" {
		t.Fatalf("tab moves to the second command, at %q", c.comp.items[c.comp.idx])
	}
	ran, ok := find[CommandMsg](run(te.compose(c, "enter")))
	if !ok || ran.Name != "mode" || c.Value() != "" {
		t.Fatalf("enter runs a command without an argument at once, %+v %q", ran, c.Value())
	}
	for _, r := range "/mo" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	te.compose(c, "esc")
	if c.Completing() || c.Value() != "/mo" {
		t.Fatalf("esc closes the popup and keeps the text %q", c.Value())
	}
	c.ta.Reset()
	os.MkdirAll(filepath.Join(te.dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(te.dir, "sub", "file.txt"), []byte("x"), 0o644)
	for _, r := range "see su" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	te.compose(c, "tab")
	if c.Value() != "see sub/file.txt " {
		t.Fatalf("path completion %q", c.Value())
	}
	c.ta.Reset()
	c.comp = nil
	for _, r := range "draft kept" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	te.compose(c, "ctrl+a")
	te.compose(c, "ctrl+k")
	if c.Value() != "" {
		t.Fatalf("line start then delete to the end through the keymap %q", c.Value())
	}
	for _, r := range "draft kept" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	te.compose(c, "ctrl+w")
	if c.Value() != "draft " {
		t.Fatalf("delete word back %q", c.Value())
	}
	if run(c.Action("composer.word.left")) != nil || c.ta.Column() != 0 {
		t.Fatalf("an action fired without a key presses its first chord, column %d", c.ta.Column())
	}
	c.Update(tea.KeyPressMsg{Code: tea.KeyF5})
	if c.Value() != "draft " {
		t.Fatal("an unbound key without text types nothing")
	}
	c.ta.MoveToEnd()
	for _, r := range "kept" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	again := NewComposer(te.env.Theme, te.env.Keys, dir, func() string { return te.dir })
	if again.Value() != "" || len(again.History()) != 3 {
		t.Fatalf("history comes back before a session is bound, draft %q history %v", again.Value(), again.History())
	}
	again.Bind("s1")
	if again.Value() != "draft kept" || again.Empty() {
		t.Fatalf("binding the session brings its draft back %q", again.Value())
	}
}

func TestComposerPlaceholderAndSymbolButton(t *testing.T) {
	te := newEnv(t)
	c := NewComposer(te.env.Theme, te.env.Keys, "", func() string { return te.dir })
	run(c.SetFocus(true))
	view := plain(c.View(140, 2))
	rows := strings.Split(view, "\n")
	if !strings.HasSuffix(strings.TrimRight(rows[0], " "), te.env.Theme.Glyphs.Send) || strings.Contains(view, "send") {
		t.Fatalf("the send button is a symbol floating on the first row\n%s", view)
	}
	if err := te.env.Keys.Unbind("palette"); err != nil {
		t.Fatal(err)
	}

	if c.button.w != 5 {
		t.Fatalf("the button takes five columns %d", c.button.w)
	}
	if c.Height() != 2 || len(rows) != 2 {
		t.Fatalf("the empty prompt stands two rows tall %d", c.Height())
	}
	c.SetBusy(true)
	if !strings.Contains(plain(c.View(60, 2)), te.env.Theme.Glyphs.Steer) {
		t.Fatal("the button turns into the steering hook during a run")
	}
	c.SetBusy(false)
	if _, ok := c.Click(0, 0); ok {
		t.Fatal("a click off the button sends nothing")
	}
	for _, r := range "via button" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	c.View(60, 2)
	cmd, ok := c.Click(c.button.x+2, c.button.y)
	if sub, found := find[SubmitMsg](run(cmd)); !ok || !found || sub.Text != "via button" {
		t.Fatalf("clicking the button sends %v %+v", ok, sub)
	}
	if err := te.env.Keys.Rebind("composer.left", "ctrl+b"); err != nil {
		t.Fatal(err)
	}
	if err := te.env.Keys.Rebind("composer.send", "ctrl+s"); err != nil {
		t.Fatal(err)
	}
	c.Rebind()
	for _, r := range "ab" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	te.compose(c, "ctrl+b")
	c.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if c.Value() != "axb" {
		t.Fatalf("a rebound key moves the cursor %q", c.Value())
	}
	if sub, ok := find[SubmitMsg](run(te.compose(c, "ctrl+s"))); !ok || sub.Text != "axb" {
		t.Fatalf("the rebound send key sends %+v", sub)
	}
	plainEnv := newEnv(t)
	plainEnv.env.Theme = theme.Plain()
	pc := NewComposer(plainEnv.env.Theme, plainEnv.env.Keys, "", func() string { return te.dir })
	if !strings.Contains(plain(pc.View(60, 2)), "[ > ]") {
		t.Fatalf("plain button %q", plain(pc.View(60, 2)))
	}
}

func TestComposerAttachesImages(t *testing.T) {
	te := newEnv(t)
	c := NewComposer(te.env.Theme, te.env.Keys, "", func() string { return te.dir })
	c.Bind("s1")
	run(c.SetFocus(true))
	png := filepath.Join(te.dir, "shot.png")
	os.WriteFile(png, []byte("\x89PNG"), 0o644)
	if _, ok := find[wm.Intent](run(c.Attach("nope.txt"))); !ok {
		t.Fatal("bad attach notifies")
	}
	for _, r := range "/attach shot.png" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	c.comp = nil
	run(te.compose(c, "enter"))
	if got := c.Attachments(); len(got) != 1 || got[0] != png {
		t.Fatalf("attachments %v", got)
	}
	if !strings.Contains(c.Status(), "1 attached") || c.Empty() {
		t.Fatalf("status %q", c.Status())
	}
	for _, r := range "look" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	sub, ok := find[SubmitMsg](run(te.compose(c, "enter")))
	if !ok || len(sub.Parts) != 2 {
		t.Fatalf("submit with image %+v", sub)
	}
	if _, ok := sub.Parts[0].(protocol.Image); !ok {
		t.Fatal("image part first")
	}
	if len(c.Attachments()) != 0 {
		t.Fatal("attachments cleared after sending")
	}
	run(c.Attach("shot.png"))
	msgs := run(te.compose(c, "ctrl+x"))
	if _, ok := find[wm.Intent](msgs); !ok || len(c.Attachments()) != 0 {
		t.Fatal("ctrl+x clears attachments and notifies")
	}
}

func toolEnd(name, id string, data any) agent.Event {
	return agent.Event{Type: agent.EventToolEnd, Time: at, Call: protocol.ToolCall{ID: id, Name: name}, Result: protocol.ToolResult{CallID: id, Name: name, Content: []protocol.Part{protocol.Text{Text: "ok"}}}, Data: data, Duration: 1200 * time.Millisecond}
}

func TestTranscriptAppliesEventsAndSearches(t *testing.T) {
	te := newEnv(t)
	tr := NewTranscript(te.env)
	tr.Apply(agent.Event{Type: agent.EventRunStart, Time: at, Parts: []protocol.Part{protocol.Text{Text: "check disk"}}})
	tr.Apply(agent.Event{Type: agent.EventTurnStart, Time: at, Round: 1})
	resp := protocol.Response{Model: "m-test", Content: []protocol.Part{protocol.Text{Text: "looking"}, protocol.Thinking{Text: "hmm"}}}
	tr.Apply(agent.Event{Type: agent.EventModel, Time: at, Round: 1, Model: protocol.Event{Type: protocol.EventTextDelta, Index: 0, Delta: "looking", Response: resp}})
	tr.Apply(agent.Event{Type: agent.EventModel, Time: at, Round: 1, Model: protocol.Event{Type: protocol.EventThinkingDelta, Index: 1, Delta: "hmm", Response: resp}})
	if !tr.Busy() {
		t.Fatal("streaming turn is busy")
	}
	tr.Apply(agent.Event{Type: agent.EventTurnEnd, Time: at, Response: &resp})
	call := protocol.ToolCall{ID: "c1", Name: files.ReadName, Args: json.RawMessage(`{"file_path":"a.go"}`)}
	tr.Apply(agent.Event{Type: agent.EventToolStart, Time: at, Round: 1, Call: call})
	_, cmd := tr.Update(harness.ProgressMsg{Progress: tool.Progress{CallID: "c1", Text: "partial out"}})
	_ = cmd
	if !strings.Contains(plain(strings.Join(tr.Lines(80), "\n")), "partial out") {
		t.Fatal("progress text shows while the tool runs")
	}
	tr.Apply(toolEnd(files.ReadName, "c1", files.ReadData{Path: "a.go", Offset: 1, Lines: []string{"package a", "func f() {}"}, Total: 2}))
	tr.Apply(agent.Event{Type: agent.EventToolStart, Time: at, Round: 1, Call: protocol.ToolCall{ID: "c2", Name: files.EditName, Args: json.RawMessage(`{"file_path":"b.go"}`)}})
	tr.Apply(toolEnd(files.EditName, "c2", files.Change{Path: "b.go", Old: "a\nb\n", New: "a\nc\n"}))
	tr.Apply(agent.Event{Type: agent.EventApproval, Time: at, Approval: &agent.Approval{Request: policy.Request{Call: protocol.ToolCall{ID: "c3", Name: "Bash"}}, Pending: true}})
	tr.Apply(agent.Event{Type: agent.EventApproval, Time: at, Approval: &agent.Approval{Request: policy.Request{Call: protocol.ToolCall{ID: "c3", Name: "Bash"}}, Answer: policy.Answer{Allowed: true, By: "person", Verdict: policy.Yes}}})
	tr.Apply(agent.Event{Type: agent.EventSteer, Time: at, Parts: []protocol.Part{protocol.Text{Text: "also check memory"}}})
	tr.Apply(agent.Event{Type: agent.EventCompactStart, Time: at})
	tr.Apply(agent.Event{Type: agent.EventCompactEnd, Time: at, Text: "summary here", Data: map[string]int{"before": 10, "after": 2}})
	tr.Apply(agent.Event{Type: agent.EventHook, Time: at, Text: "PreToolUse ran", Data: hooks.Execution{Stdout: "hook said hi\n"}})
	tr.Apply(agent.Event{Type: agent.EventNote, Time: at, Text: "a note"})
	tr.Apply(agent.Event{Type: agent.EventSubagentStart, Time: at, Call: protocol.ToolCall{Name: "scout"}, Text: "scan ports", Data: subagent.Child{Name: "scout"}})
	child := protocol.Response{Content: []protocol.Part{protocol.Text{Text: "22 open"}}}
	tr.Apply(agent.Event{Type: agent.EventSubagentEnd, Time: at, Call: protocol.ToolCall{Name: "scout"}, Response: &child})
	tr.Apply(agent.Event{Type: agent.EventInterrupt, Time: at})
	tr.Apply(agent.Event{Type: agent.EventRunEnd, Time: at, Err: errors.New("boom")})
	if tr.Blocks() != 13 {
		t.Fatalf("blocks %d", tr.Blocks())
	}
	first := strings.Join(tr.Lines(80), "\n")
	if first != strings.Join(tr.Lines(80), "\n") {
		t.Fatal("lines are deterministic")
	}
	text := plain(first)
	for _, want := range []string{"check disk", "looking", "Read", "a.go", "2 of 2 lines", "Edit", "+1 -1", "allowed Bash", "also check memory", "compacted 10 messages into 2", "PreToolUse ran", "hook said hi", "a note", "scout", "22 open", "interrupted", "boom", "ok 1.2s"} {
		if !strings.Contains(text, want) {
			t.Fatalf("lines lack %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "package a") {
		t.Fatal("collapsed tool hides its body")
	}
	tr.ExpandAll(true)
	text = plain(strings.Join(tr.Lines(80), "\n"))
	if !strings.Contains(text, "package a") || !strings.Contains(text, "+ c") {
		t.Fatalf("expanded bodies\n%s", text)
	}
	tr.ExpandAll(false)
	if strings.Contains(plain(strings.Join(tr.Lines(80), "\n")), "package a") {
		t.Fatal("folded again")
	}
	if boiling := plain(strings.Join(tr.Lines(80), "\n")); strings.Contains(boiling, "hmm") || !strings.Contains(boiling, "thinking") {
		t.Fatalf("a thought still boils the instant it lands\n%s", boiling)
	}
	te.env.Now = func() time.Time { return at.Add(time.Second) }
	if !tr.Thinking() || !strings.Contains(plain(strings.Join(tr.Lines(80), "\n")), "hmm") {
		t.Fatal("thinking shows by default once settled")
	}
	tr.ShowThinking(false)
	if tr.Thinking() || strings.Contains(plain(strings.Join(tr.Lines(80), "\n")), "hmm") {
		t.Fatal("hiding folds a finished thought")
	}
	tr.ShowThinking(true)
	view := strings.Split(plain(tr.View(80, 10)), "\n")
	if len(view) != 10 || !strings.HasSuffix(view[0], "⇞") || !strings.HasSuffix(view[9], "⇟") {
		t.Fatalf("the scrollbar runs down the right edge\n%s", strings.Join(view, "\n"))
	}
	tall := strings.Split(plain(tr.View(80, 20)), "\n")
	if !strings.HasSuffix(tall[0], " ⇞") || !strings.HasSuffix(tall[19], " ⇟") || strings.Contains(strings.Join(tall, ""), "pgup") {
		t.Fatalf("the caps are the page key symbols, nothing spelled out\n%s", strings.Join(tall, "\n"))
	}
	for _, row := range tall[1:19] {
		if r := []rune(row); !strings.ContainsRune("┊┃╂╋◈", r[len(r)-1]) {
			t.Fatalf("the track is a thin line with ticks and the thumb %q", row)
		}
	}
	fits := gutter{h: 8, total: 5, visible: 8, marks: []int{0, 3}, up: "pgup", down: "pgdown"}
	if col := plain(strings.Join(fits.column(te.env.Theme), "")); col != "        " {
		t.Fatalf("content that fits draws no scrollbar %q", col)
	}
	if hit, _ := fits.hit(0); hit != hitNone {
		t.Fatal("caps of a hidden scrollbar page nothing")
	}
	runs := gutter{h: 8, total: 60, offset: 60 - 8, visible: 8, marks: []int{0, 55}, hits: []int{30}, up: "pgup", down: "pgdown"}
	if col := plain(strings.Join(runs.column(te.env.Theme), "")); col != "⇞╂┊┊◈┊╋⇟" {
		t.Fatalf("a scrolled view shows the thumb at the end, a tick on it and the match %q", col)
	}
	te.env.Keys.Rebind("session.scroll.up", "ctrl+b")
	if top := plain(strings.Split(tr.View(80, 20), "\n")[0]); !strings.HasSuffix(top, " ctrl+b ⌃") {
		t.Fatalf("a chord without a symbol sits beside an arrow cap %q", top)
	}
	te.env.Keys.Reset("session.scroll.up")
	tr.View(80, 10)
	tr.Top()
	if tr.Following() {
		t.Fatal("scrolling to the top stops following")
	}
	tr.Follow()
	if !tr.Following() {
		t.Fatal("end follows again")
	}
	tr.ScrollBy(-1)
	tr.ScrollBy(1)
	tr.Search()
	if !tr.Searching() {
		t.Fatal("Search opens the bar")
	}
	tr.SearchType("scout")
	if len(tr.found) != 1 || tr.current != 0 {
		t.Fatalf("matches as it is typed %d", len(tr.found))
	}
	if !strings.Contains(tr.View(80, 10), "scout") {
		t.Fatal("the match is shown")
	}
	tr.SearchAction("search.accept")
	tr.View(80, 10)
	if tr.Searching() || len(tr.found) != 1 {
		t.Fatalf("matches %d", len(tr.found))
	}
	if !strings.Contains(tr.Status(), "1 turn ∷ ") || !strings.HasSuffix(tr.Status(), "1 match") {
		t.Fatalf("status %q", tr.Status())
	}
	if g := tr.gutter(); len(g.hits) != 1 || len(g.marks) != 1 {
		t.Fatalf("the scrollbar marks the turn and the match %+v", g)
	}
	if !strings.Contains(plain(tr.SearchView(40, "find")), "1/1") {
		t.Fatalf("bar counts the matches %q", plain(tr.SearchView(40, "find")))
	}
	tr.Search()
	if !tr.Searching() || tr.search.Query() != "scout" {
		t.Fatal("Search reopens the bar with the last query")
	}
	tr.SearchAction("search.delete.word")
	tr.SearchType("o")
	tr.SearchAction("search.next")
	tr.SearchAction("search.prev")
	if len(tr.found) < 2 {
		t.Fatalf("a shorter query matches more %d", len(tr.found))
	}
	tr.SearchAction("search.close")
	if tr.Searching() || tr.search.Query() != "" || len(tr.found) != 0 || tr.SearchShown() {
		t.Fatal("close clears the search")
	}
	tr.Top()
	tr.Follow()
	if !tr.Following() {
		t.Fatal("Follow jumps to the end")
	}
	tr.MoveCursor(1)
	tr.Toggle()
	if tr.cursor < 0 {
		t.Fatal("cursor placed")
	}
	tr.JumpTurn(-1)
	tr.Page(-1)
	tr.Page(1)
	tr.Follow()
	tr.ToggleFollow()
	if tr.Following() {
		t.Fatal("toggling follow holds still")
	}
	tr.ToggleFollow()
	if !tr.Following() {
		t.Fatal("toggling again follows")
	}
	if !strings.HasPrefix(tr.Status(), "1 turn") {
		t.Fatalf("status %q", tr.Status())
	}
	tr.View(80, 10)
	tr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 79, Y: 0})
	if tr.Following() {
		t.Fatal("clicking the top cap pages up")
	}
	tr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 79, Y: 9})
	te.env.Theme = theme.Light()
	tr.View(80, 10)
	if tr.themed != "light" {
		t.Fatal("a new theme repaints the cached blocks")
	}
}

func TestTranscriptFiltersRunsAndLoadsSessions(t *testing.T) {
	te := newEnv(t)
	child := NewChildTranscript(te.env, "run-1", "scout")
	if child.Key() != "trace:run-1/scout" || child.Title() != "TRACE scout" {
		t.Fatalf("child identity %q %q", child.Key(), child.Title())
	}
	child.Update(harness.EventMsg{Run: "run-2", Event: agent.Event{Type: agent.EventNote, Agent: "scout", Text: "wrong run"}})
	child.Update(harness.EventMsg{Run: "run-1", Event: agent.Event{Type: agent.EventNote, Agent: "root", Text: "wrong agent"}})
	child.Update(harness.EventMsg{Run: "run-1", Event: agent.Event{Type: agent.EventNote, Agent: "scout", Text: "mine"}})
	if child.Blocks() != 1 {
		t.Fatalf("child blocks %d", child.Blocks())
	}
	root := NewTranscript(te.env)
	root.Update(harness.EventMsg{Run: "run-1", Event: agent.Event{Type: agent.EventNote, Depth: 1, Text: "nested"}})
	root.Update(harness.EventMsg{Run: "run-1", Event: agent.Event{Type: agent.EventNote, Text: "top"}})
	if root.Blocks() != 1 {
		t.Fatalf("root blocks %d", root.Blocks())
	}
	sess := session.New()
	sess.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "stored prompt"}}})
	sess.AddResponse("root", protocol.Response{Model: "m", Content: []protocol.Part{protocol.Text{Text: "stored answer"}, protocol.ToolCall{ID: "t1", Name: "Bash", Args: json.RawMessage(`{"command":"ls"}`)}}})
	sess.AddResults("root", []protocol.ToolResult{{CallID: "t1", Name: "Bash", Content: []protocol.Part{protocol.Text{Text: "a.go"}}}})
	sess.Log("root", "approval", map[string]any{"tool": "Bash", "call_id": "t1", "allowed": true, "by": "person", "verdict": "yes"})
	sess.Log("root", "compact", map[string]any{"before": 4, "after": 1, "summary": "short"})
	sess.Log("root", "interrupt", nil)
	sess.Log("root", "run_end", map[string]any{"error": "ended badly"})
	sess.Log("root", facts.RecordKind, facts.Change{Op: facts.OpAdd, Fact: facts.Fact{ID: "f1", Kind: "finding", Title: "a fact"}})
	sess.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "Summary of the earlier conversation: things"}}})
	tr := NewTranscript(te.env)
	tr.Load(sess.Entries(), sess.Records())
	text := plain(strings.Join(tr.Lines(100), "\n"))
	for _, want := range []string{"stored prompt", "stored answer", "Bash", "ls", "allowed Bash", "compacted 4 messages into 1", "interrupted", "ended badly", "recorded finding f1", "compaction summary"} {
		if !strings.Contains(text, want) {
			t.Fatalf("loaded lines lack %q in\n%s", want, text)
		}
	}
	if tr.Blocks() != 9 || tr.turns != 1 {
		t.Fatalf("blocks %d turns %d", tr.Blocks(), tr.turns)
	}
	tr.View(100, 8)
	tr.Top()
	tr.View(100, 8)
	tr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 0})
	if tr.cursor != 0 {
		t.Fatalf("click selects the block under it, got %d", tr.cursor)
	}
}

func TestSessionRoutesKeysAndOwnsRuns(t *testing.T) {
	te := newEnv(t)
	te.live.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "earlier"}}})
	te.live.Endpoint = te.app.Primary
	s := NewSession(te.env, te.live, "")
	if s.Title() != "NEW" || s.Key() != "session:"+te.live.Session.ID() || !s.Closable() || !s.Selectable() {
		t.Fatalf("identity %q %q", s.Title(), s.Key())
	}
	if s.Live() != te.live || s.Session() != te.live.Session || s.Transcript.Blocks() != 1 {
		t.Fatal("session loads the live transcript")
	}
	if !strings.HasSuffix(s.Status(), "claude-opus-5") {
		t.Fatalf("the status trails with the model %q", s.Status())
	}
	locked := true
	s.Locked = func() bool { return locked }
	if s.Closable() {
		t.Fatal("a session the app holds does not close")
	}
	locked = false
	if !s.Closable() {
		t.Fatal("an idle session closes")
	}
	closed, ok := find[SessionClosedMsg](run(second(s.Update(wm.ClosedMsg{}))))
	if !ok || closed.Session != te.live.Session.ID() {
		t.Fatalf("a closed card asks the app to store its session %+v", closed)
	}
	te.live.Session.SetTitle("disk usage on the box, and more words here than fit in a title")
	if !strings.HasPrefix(s.Title(), "disk usage") || len([]rune(s.Title())) > 40 {
		t.Fatalf("title %q", s.Title())
	}
	te.live.Session.Rename("probe")
	te.live.Session.SetTitle("derived again")
	if s.Title() != "probe" {
		t.Fatalf("a chosen label holds against derivation %q", s.Title())
	}
	te.live.Session.Rename("")
	te.live.Session.SetTitle("disk usage")
	run2, _ := s.Update(wm.FocusMsg{Focused: true})
	_ = run2
	if !s.Composer.focused || s.Cursor() == nil {
		t.Fatal("focus reaches the composer")
	}
	typeText(s, "hello there")
	sub, ok := find[SubmitMsg](te.pressed(s, "enter"))
	if !ok || sub.Text != "hello there" || sub.Session != te.live.Session.ID() {
		t.Fatalf("submit %+v", sub)
	}
	te.press(s, "ctrl+f")
	if s.Transcript.Searching() {
		t.Fatal("ctrl+f stays with the terminal")
	}
	te.press(s, "alt+/")
	if !s.Transcript.Searching() || !slices.Equal(s.Contexts(), []string{keys.Search}) {
		t.Fatal("alt+/ opens the search bar")
	}
	typeText(s, "earl")
	if s.Transcript.search.Query() != "earl" || s.Composer.Value() != "" {
		t.Fatalf("search typing goes to the transcript, query %q composer %q", s.Transcript.search.Query(), s.Composer.Value())
	}
	s.Update(tea.PasteMsg{Content: "y"})
	te.press(s, "backspace")
	view := plain(s.View(60, 12))
	if !strings.Contains(strings.Split(view, "\n")[s.split], "⚲ earl") || !strings.Contains(view, "1/1") {
		t.Fatalf("search bar takes the place of the rule\n%s", view)
	}
	if cur := s.Cursor(); cur == nil || cur.Y != s.split || cur.X != s.Transcript.SearchCursor() {
		t.Fatalf("search cursor in the bar %+v", cur)
	}
	if h := s.Hints(); len(h) != 3 || h[0].Action != "search.accept" {
		t.Fatalf("search hints %+v", h)
	}
	te.press(s, "enter")
	if s.Transcript.Searching() || len(s.Transcript.found) != 1 {
		t.Fatalf("search applied, matches %d", len(s.Transcript.found))
	}
	te.press(s, "alt+/")
	te.press(s, "esc")
	if s.Transcript.SearchShown() {
		t.Fatal("esc clears the search")
	}
	for i := 0; i < 30; i++ {
		s.Transcript.Apply(agent.Event{Type: agent.EventNote, Time: at, Text: "filler line"})
	}
	s.View(60, 12)
	te.press(s, "pgup")
	if s.Transcript.Following() {
		t.Fatal("pgup scrolls the transcript away from the end")
	}
	te.press(s, "pgdown")
	te.press(s, "ctrl+home")
	if s.Transcript.vp.YOffset() != 0 {
		t.Fatal("ctrl+home reaches the top")
	}
	te.press(s, "alt+g")
	if !s.Transcript.Following() {
		t.Fatal("alt+g follows again")
	}
	te.press(s, "alt+e")
	te.press(s, "alt+E")
	te.press(s, "alt+t")
	if s.Transcript.Thinking() {
		t.Fatal("alt+t hides thinking")
	}
	if cmd, ok := find[CommandMsg](te.pressed(s, "shift+tab")); !ok || cmd.Name != "mode" || cmd.Session != te.live.Session.ID() {
		t.Fatalf("shift+tab cycles the mode %+v", cmd)
	}
	view = plain(s.View(60, 12))
	lines := strings.Split(view, "\n")
	if len(lines) != 12 || !strings.Contains(lines[s.split], "───") || !strings.Contains(lines[s.split+1], "❯") || strings.TrimSpace(lines[len(lines)-1]) != "" {
		t.Fatalf("rule between transcript and a two row prompt whose spare row stays blank\n%s", view)
	}
	if !strings.HasSuffix(lines[0], "⇞") || !strings.HasSuffix(lines[s.split-1], "⇟") || !strings.Contains(lines[s.split+1], "➜") {
		t.Fatalf("scrollbar beside the transcript and the button on the prompt\n%s", view)
	}
	if h := s.Hints(); len(h) != 2 || h[0].Action != "session.search" || h[1].Action != "mode.cycle" {
		t.Fatalf("the legend names search and mode only %+v", h)
	}
	if cur := s.Cursor(); cur == nil || cur.Y != s.split+1 {
		t.Fatalf("cursor below the rule %+v", cur)
	}
	s.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 3, Y: s.split})
	if s.Transcript.Searching() || s.Transcript.SearchShown() {
		t.Fatal("clicking the rule above the prompt leaves the search alone")
	}
	typeText(s, "/mo")
	s.Composer.SetCommands([]Command{{Name: "model"}, {Name: "mode"}})
	typeText(s, "d")
	if !s.Composer.Completing() || s.Contexts()[0] != keys.Complete {
		t.Fatalf("an open popup puts the completion keys first %v", s.Contexts())
	}
	if h := s.Hints(); h[0].Action != "complete.accept" {
		t.Fatalf("completion hints %+v", h)
	}
	te.press(s, "esc")
	s.Composer.ta.Reset()
	s.Update(keys.ChangedMsg{})
	run(func() tea.Cmd { _, c := s.Update(wm.FocusMsg{}); return c }())
	if s.Cursor() != nil {
		t.Fatal("blurred session hides the cursor")
	}
	r := te.startRun(t, nil, "answer")
	before := s.Transcript.Blocks()
	s.Update(harness.EventMsg{Run: "nope", Event: agent.Event{Type: agent.EventNote, Text: "foreign"}})
	if s.Transcript.Blocks() != before {
		t.Fatal("events of unknown runs are dropped")
	}
	s.Update(harness.EventMsg{Run: r.ID, Event: agent.Event{Type: agent.EventNote, Text: "owned"}})
	if s.Transcript.Blocks() != before+1 {
		t.Fatal("events of the owned run reach the transcript")
	}
	s.Update(harness.ProgressMsg{Run: "nope", Progress: tool.Progress{CallID: "x"}})
	if s.Status() != "1 turn ∷ claude-opus-5" {
		t.Fatalf("status %q", s.Status())
	}
	s.Transcript.Top()
	s.Composer.recall(-1)
	if s.Status() != "1 turn ∷ paused ∷ history 1/1 ∷ claude-opus-5" {
		t.Fatalf("status joins the transcript and the prompt %q", s.Status())
	}
	s.SetTheme(theme.Light())
}

func TestAgentCardFollowsAChildRun(t *testing.T) {
	te := newEnv(t)
	hold := make(chan struct{})
	r := te.startRun(t, hold, "found it")
	a := NewAgent(te.env, r)
	if a.Key() != "agent:"+r.ID || a.Title() != "root" || a.Run() != r || !a.Selectable() {
		t.Fatalf("identity %q %q", a.Key(), a.Title())
	}
	head := plain(a.View(60, 1))
	if strings.Count(head, "\n") != 0 || a.Status() != "running" {
		t.Fatalf("running header %q status %q", head, a.Status())
	}
	tick, ok := find[tickMsg](run(a.Init()))
	if !ok || tick.owner != &a.timer {
		t.Fatal("running card ticks")
	}
	close(hold)
	te.waitRun(t, r)
	a.Update(tick)
	head = plain(a.View(60, 2))
	if !strings.Contains(head, "done") || a.Status() != "done" {
		t.Fatalf("finished header %q", head)
	}
	if run(a.Init()) != nil {
		t.Fatal("finished card stops ticking")
	}
	child := &harness.Run{ID: r.ID + "/scout", Name: "scout", Parent: r.ID, Session: session.New(), Description: "scan ports", Depth: 1, Started: at}
	child.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "scan the ports"}}})
	c := NewAgent(te.env, child)
	if c.Key() != "agent:"+child.ID || c.Title() != "scout" {
		t.Fatalf("child identity %q", c.Key())
	}
	view := plain(c.View(60, 6))
	if !strings.Contains(view, "scan ports") || !strings.Contains(view, "done") || !strings.Contains(view, "scan the ports") {
		t.Fatalf("child view\n%s", view)
	}
	c.Update(harness.EventMsg{Run: r.ID, Event: agent.Event{Type: agent.EventNote, Agent: "scout", Text: "child note"}})
	c.Update(harness.EventMsg{Run: r.ID, Event: agent.Event{Type: agent.EventNote, Agent: "root", Text: "parent note"}})
	if !strings.Contains(plain(c.View(60, 8)), "child note") || strings.Contains(plain(c.View(60, 8)), "parent note") {
		t.Fatal("child card shows only its agent")
	}
	te.press(c, "/")
	if !c.tr.Searching() || !slices.Equal(c.Contexts(), []string{keys.Search}) {
		t.Fatal("slash opens the search bar")
	}
	typeText(c, "child")
	c.Update(tea.PasteMsg{Content: " note"})
	view = plain(c.View(60, 8))
	if !strings.Contains(strings.Split(view, "\n")[7], "⚲ child note") || c.Cursor() == nil {
		t.Fatalf("the search bar sits under the transcript\n%s", view)
	}
	if h := c.Hints(); h[0].Action != "search.accept" {
		t.Fatalf("search hints %+v", h)
	}
	te.press(c, "enter")
	if c.Cursor() != nil || c.Hints()[0].Action != "transcript.down" {
		t.Fatal("accepting leaves the bar")
	}
	c.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 3, Y: 7})
	if !c.tr.Searching() {
		t.Fatal("clicking the bar reopens it")
	}
	te.press(c, "esc")
	for _, chord := range []string{"j", "k", "pgdown", "pgup", "g", "G", "n", "N", "[", "]", ".", ",", "enter", "e", "E", "T", "f"} {
		te.press(c, chord)
	}
	if !slices.Equal(c.Contexts(), []string{keys.Transcript, keys.Panel}) {
		t.Fatalf("contexts %v", c.Contexts())
	}
	c.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 0})
	c.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 1})
	if c.tr.cursor != 0 {
		t.Fatalf("click below the header reaches the transcript, cursor %d", c.tr.cursor)
	}
	c.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 1, Y: 2})
}

// Asks the approver for a run and reports the pending entry once it is queued
func ask(t *testing.T, te *testEnv, runID, toolName string) *harness.Pending {
	t.Helper()
	ctx, cancel := context.WithCancel(harness.WithRun(context.Background(), runID))
	t.Cleanup(cancel)
	req := policy.Request{Call: protocol.ToolCall{ID: "c-" + toolName, Name: toolName, Args: json.RawMessage(`{"command":"rm -rf /tmp/x"}`)}, Agent: "root", Reason: "deny rule", Key: "Bash(rm *)", Subjects: []policy.Subject{{Kind: "command", Text: "rm -rf /tmp/x", Exact: true}, {Kind: "command", Text: "$(evil)", Unsafe: true}}, Tool: &protocol.Tool{Name: toolName, Description: "Runs a command"}, Hints: tool.Annotations{Destructive: true, OpenWorld: true}}
	go te.env.Approver.Ask(ctx, req)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range te.env.Approver.Pending() {
			if p.Run == runID && p.Request.Call.Name == toolName {
				return p
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("approval never queued")
	return nil
}

func TestApprovalDialogAnswersItsSession(t *testing.T) {
	te := newEnv(t)
	a := NewApproval(te.env, "s1", func(run string) bool { return run == "run-1" })
	if !a.Floating() || !a.Closable() || a.Title() != "APPROVAL" || a.Key() != "approval:s1" {
		t.Fatalf("identity %q %q", a.Title(), a.Key())
	}
	if w, h := a.MinSize(); w != approvalWidth || h != approvalMinRows || a.Hints() != nil {
		t.Fatalf("empty size %d %d hints %v", w, h, a.Hints())
	}
	if !strings.Contains(plain(a.View(60, 10)), "no approvals") {
		t.Fatal("empty dialog says so")
	}
	ask(t, te, "run-9", "Bash")
	p := ask(t, te, "run-1", "Bash")
	a.Update(harness.ApprovalMsg{Pending: p})
	if len(a.Pending()) != 1 || a.Pending()[0].ID != p.ID || a.Closable() {
		t.Fatalf("filtered pending %d", len(a.Pending()))
	}
	if a.Title() != "APPROVAL" || a.Status() != "" {
		t.Fatalf("title %q status %q", a.Title(), a.Status())
	}
	if _, h := a.MinSize(); h != len(a.body(approvalWidth))+2 {
		t.Fatalf("the dialog opens at the height of its request, %d", h)
	}
	view := plain(a.View(70, 24))
	for _, want := range []string{"⌁ Bash", "by      root", "reason  deny rule", "command  rm -rf /tmp/x", "! $(evil)  unsafe", "▹ rm -rf /tmp/x  exact", "rule    Bash(rm *)", "destructive  open world", "allow", "deny", "always", "never", p.Asked.Local().Format("15:04:05")} {
		if !strings.Contains(view, want) {
			t.Fatalf("view lacks %q\n%s", want, view)
		}
	}
	for _, noise := range []string{"Runs a command", "hints", "subjects", "approval 1 of", "{", "}"} {
		if strings.Contains(view, noise) {
			t.Fatalf("view carries %q\n%s", noise, view)
		}
	}
	if a.Hints() != nil {
		t.Fatalf("a request that fits offers no scroll hint %v", a.Hints())
	}
	short := strings.Split(plain(a.View(70, 8)), "\n")
	if hs := a.Hints(); len(hs) != 1 || hs[0].Action != "approval.down" || hs[0].Pair != "approval.up" {
		t.Fatalf("a request that runs past the dialog offers scrolling %v", hs)
	}
	if !strings.HasSuffix(short[0], "⇞") || !strings.HasSuffix(short[5], "⇟") {
		t.Fatalf("an overflowing request gets a scrollbar\n%s", strings.Join(short, "\n"))
	}
	a.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 69, Y: 5})
	if a.scroll == 0 {
		t.Fatal("clicking the bottom cap pages down")
	}
	a.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 69, Y: 0})
	if a.scroll != 0 {
		t.Fatal("clicking the top cap pages up")
	}
	root := *p
	root.Request.Agent = root.Run
	root.Request.Reason = "default"
	desc := plain(strings.Join(Describe(&root, 70, te.env.Theme), "\n"))
	if strings.Contains(desc, "\nby") || strings.Contains(desc, "because") {
		t.Fatalf("the root agent and the default reason say nothing\n%s", desc)
	}
	nested := *p
	nested.Request.Call.Args = json.RawMessage(`{"file_path":"a.go","old_string":"one\ntwo","edits":[{"n":1}]}`)
	desc = plain(strings.Join(Describe(&nested, 40, te.env.Theme), "\n"))
	for _, want := range []string{"file_path   a.go", "old_string  one", "            two", "edits       [{\"n\":1}]"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("arguments lack %q\n%s", want, desc)
		}
	}
	bare := *p
	bare.Request.Call.Args = json.RawMessage(`"just text"`)
	if desc = plain(strings.Join(Describe(&bare, 40, te.env.Theme), "\n")); !strings.Contains(desc, `"just text"`) {
		t.Fatalf("arguments that are not an object show as they are\n%s", desc)
	}
	te.press(a, "j")
	te.press(a, "pgdown")
	if a.scroll == 0 {
		t.Fatal("scroll keys move")
	}
	te.press(a, "k")
	te.press(a, "pgup")
	te.press(a, "g")
	if a.scroll != 0 {
		t.Fatal("g returns to the top")
	}
	for _, tc := range []struct {
		key  string
		want policy.Verdict
	}{{"y", policy.Yes}, {"n", policy.No}, {"Y", policy.Always}, {"a", policy.Always}, {"N", policy.Never}} {
		ans, ok := find[AnswerMsg](te.pressed(a, tc.key))
		if !ok || ans.ID != p.ID || ans.Verdict != tc.want {
			t.Fatalf("%s gave %+v", tc.key, ans)
		}
	}
	a.View(70, 24)
	if len(a.buttons) != 4 {
		t.Fatalf("buttons %d", len(a.buttons))
	}
	te.env.Keys.Rebind("approval.allow", "ctrl+y")
	if foot := plain(a.footer(70)); !strings.HasPrefix(foot, " ctrl+y  allow") || !strings.Contains(foot, " n  deny") {
		t.Fatalf("buttons carry the keys of the keymap %q", foot)
	}
	te.env.Keys.Unbind("approval.never")
	if foot := plain(a.footer(70)); !strings.HasSuffix(foot, " never ") || strings.Contains(foot, " v ") {
		t.Fatalf("an unbound verdict shows its word alone %q", foot)
	}
	te.env.Keys = keys.Default()
	ans, ok := find[AnswerMsg](run(func() tea.Cmd {
		_, c := a.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: a.buttons[1].x, Y: a.h - 1})
		return c
	}()))
	if !ok || ans.Verdict != policy.No {
		t.Fatalf("click on deny gave %+v", ans)
	}
	te.env.Approver.Answer(p.ID, policy.Yes)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(te.env.Approver.Pending()) > 1 {
		time.Sleep(time.Millisecond)
	}
	msgs := run(func() tea.Cmd { _, c := a.Update(harness.ApprovalDoneMsg{ID: p.ID}); return c }())
	if _, ok := find[wm.Intent](msgs); !ok || len(a.Pending()) != 0 {
		t.Fatal("an empty queue closes the dialog")
	}
}

func TestAgentsRosterListsRuns(t *testing.T) {
	te := newEnv(t)
	d := NewAgents(te.env)
	if d.Title() != "AGENTS" || d.Key() != "agents" || d.Status() != "idle" {
		t.Fatalf("identity %q %q %q", d.Title(), d.Key(), d.Status())
	}
	if !strings.Contains(plain(d.View(60, 5)), "no agents") {
		t.Fatal("empty agents panel")
	}
	te.live.Session.SetTitle("probe the box")
	hold := make(chan struct{})
	r := te.startRun(t, hold, "done")
	d.Update(harness.EventMsg{Run: r.ID, Event: agent.Event{Type: agent.EventRunStart}})
	if len(d.Rows()) != 1 || d.Status() != "1 running" {
		t.Fatalf("rows %d status %q", len(d.Rows()), d.Status())
	}
	tick, ok := find[tickMsg](run(d.Init()))
	if !ok || tick.owner != &d.timer {
		t.Fatal("the agents panel ticks while a run goes")
	}
	view := plain(d.View(90, 6))
	if !strings.Contains(view, "root") || !strings.Contains(view, "probe the box") || !strings.Contains(view, "running") {
		t.Fatalf("view\n%s", view)
	}
	if got := d.sessionOf(r); got != "probe the box" {
		t.Fatalf("sessionOf %q", got)
	}
	open, ok := find[OpenRunMsg](te.pressed(d, "enter"))
	if !ok || open.Run != r.ID {
		t.Fatalf("open %+v", open)
	}
	kill, ok := find[KillRunMsg](te.pressed(d, "x"))
	if !ok || kill.Run != r.ID {
		t.Fatalf("kill %+v", kill)
	}
	close(hold)
	te.waitRun(t, r)
	d.Update(harness.RunDoneMsg{Run: r.ID})
	d.Update(harness.EventMsg{Run: r.ID, Event: agent.Event{Type: agent.EventTurnEnd, Agent: "root", Usage: protocol.Usage{Input: 3, Output: 2}}})
	if d.Status() != "idle" || d.tokens[r.ID+"/root"] != 5 {
		t.Fatalf("status %q tokens %v", d.Status(), d.tokens)
	}
	if _, ok := find[wm.Intent](te.pressed(d, "x")); !ok {
		t.Fatal("killing a finished run notifies")
	}
	d.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 1})
	if _, ok := find[OpenRunMsg](run(second(d.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: render.TableHead})))); !ok {
		t.Fatal("clicking the selected row opens its transcript")
	}
	te.press(d, "j")
	te.press(d, "k")
	te.press(d, "G")
	te.press(d, "g")
	te.press(d, "pgdown")
	te.press(d, "pgup")
	if d.sel != 0 {
		t.Fatal("selection keys")
	}
	if h := d.Hints(); len(h) != 3 || h[2].Label != "move" || h[1].Action != "agents.stop" {
		t.Fatalf("hints %+v", h)
	}
	if _, cmd := d.Update(tick); cmd != nil || d.timer.armed {
		t.Fatal("the agents panel stops ticking once every run ended")
	}
}

func TestSessionsListsStoredOnes(t *testing.T) {
	te := newEnv(t)
	store := te.env.Store
	ctx := context.Background()
	one := session.New()
	one.SetTitle("alpha run")
	one.SetModel("m-a")
	one.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "a"}}})
	two := session.New()
	two.SetTitle("beta run")
	store.Save(ctx, one)
	store.Save(ctx, two)
	store.Save(ctx, te.live.Session)
	s := NewSessions(te.env)
	if s.Title() != "SESSIONS" || s.Key() != "sessions" || len(s.Rows()) != 3 || s.Status() != "" {
		t.Fatalf("rows %d status %q", len(s.Rows()), s.Status())
	}
	view := plain(s.View(90, 10))
	if !strings.Contains(view, "alpha run") || !strings.Contains(view, "beta run") || !strings.Contains(view, "◉") || !strings.Contains(view, "m-a") {
		t.Fatalf("view\n%s", view)
	}
	if strings.Contains(view, "⌕") || strings.Contains(view, "3 sessions") || !strings.HasPrefix(strings.TrimSpace(strings.Split(view, "\n")[0]), "UPDATED") {
		t.Fatalf("the table starts at the top with no bar while none is open\n%s", view)
	}
	if !slices.Equal(s.Contexts(), []string{keys.Sessions, keys.Filter, keys.List, keys.Panel}) || s.Cursor() != nil {
		t.Fatalf("contexts %v", s.Contexts())
	}
	if h := s.Hints(); h[0].Action != "sessions.resume" {
		t.Fatalf("resume leads the legend %+v", h)
	}
	te.press(s, "/")
	bottom := func() string { return plain(strings.Split(s.View(90, 10), "\n")[9]) }
	if !slices.Equal(s.Contexts(), []string{keys.Search, keys.Filter}) || s.Cursor() == nil || s.Cursor().Y != 9 || !strings.Contains(bottom(), "⚲ filter") || !strings.Contains(bottom(), "3 sessions") {
		t.Fatalf("slash opens the filter bar along the bottom with the count %q", bottom())
	}
	te.press(s, "/")
	if s.bar.Shown() || strings.Contains(plain(s.View(90, 10)), "⌕") {
		t.Fatal("slash again before typing collapses the bar")
	}
	te.press(s, "/")
	typeText(s, "beta")
	if len(s.Rows()) != 1 || s.Rows()[0].ID != two.ID() || !slices.Equal(s.Contexts(), []string{keys.Search}) {
		t.Fatalf("filters as it is typed %v", s.Rows())
	}
	te.press(s, "/")
	if s.bar.Query() != "beta/" {
		t.Fatalf("once typing began slash is text %q", s.bar.Query())
	}
	te.press(s, "backspace")
	if b := bottom(); !strings.Contains(b, "⚲ beta") || !strings.Contains(b, "1 of 3") {
		t.Fatalf("bar with the query and the count %q", b)
	}
	if h := s.Hints(); h[0].Action != "search.accept" {
		t.Fatalf("search hints %+v", h)
	}
	te.press(s, "enter")
	if len(s.Rows()) != 1 || s.bar.Active() || !strings.Contains(bottom(), "⚲ beta") {
		t.Fatal("enter keeps the filter showing and leaves the bar")
	}
	open, ok := find[OpenSessionMsg](te.pressed(s, "enter"))
	if !ok || !slices.Equal(open.Sessions, []string{two.ID()}) {
		t.Fatalf("resume %+v", open)
	}
	fork, ok := find[ForkMsg](te.pressed(s, "f"))
	if !ok || !slices.Equal(fork.Sessions, []string{two.ID()}) {
		t.Fatalf("fork %+v", fork)
	}
	te.press(s, "d")
	if !strings.Contains(bottom(), "delete "+two.ID()+"? y yes ∷ n no") || !slices.Equal(s.Contexts(), []string{keys.Confirm}) {
		t.Fatal("delete asks along the bottom first")
	}
	if h := s.Hints(); len(h) != 2 || h[0].Label != "delete" || h[1].Label != "keep" {
		t.Fatalf("confirm hints %+v", h)
	}
	te.press(s, "n")
	te.press(s, "d")
	del, ok := find[DeleteMsg](te.pressed(s, "y"))
	if !ok || !slices.Equal(del.Sessions, []string{two.ID()}) {
		t.Fatalf("delete %+v", del)
	}
	if sel := s.Selected(); len(sel) != 1 || sel[0].ID != two.ID() {
		t.Fatal("selected")
	}
	te.press(s, "/")
	if len(s.Rows()) != 3 || s.bar.Shown() {
		t.Fatal("slash on a kept filter collapses the bar and shows every row")
	}
	te.press(s, "/")
	typeText(s, "zz")
	te.press(s, "esc")
	if len(s.Rows()) != 3 || s.bar.Shown() {
		t.Fatal("esc clears the filter")
	}
	saved := one.Info()
	saved.Title = "alpha renamed"
	s.Update(harness.SavedMsg{Info: saved})
	if s.Rows()[0].ID != one.ID() || s.Rows()[0].Title != "alpha renamed" || len(s.Rows()) != 3 {
		t.Fatalf("saved first %v", s.Rows())
	}
	te.press(s, "g")
	te.press(s, "shift+down")
	te.press(s, "J")
	if len(s.Selected()) != 3 || s.Status() != "3 selected" {
		t.Fatalf("shift with a move extends the selection, %d %q", len(s.Selected()), s.Status())
	}
	view = plain(s.View(90, 10))
	if strings.Count(view, "▮") != 2 || strings.Count(view, "►") != 1 {
		t.Fatalf("selected rows are marked\n%s", view)
	}
	every := []string{s.Rows()[0].ID, s.Rows()[1].ID, s.Rows()[2].ID}
	if open, _ := find[OpenSessionMsg](te.pressed(s, "enter")); !slices.Equal(open.Sessions, every) {
		t.Fatalf("enter resumes every selected session %v", open.Sessions)
	}
	if fork, _ := find[ForkMsg](te.pressed(s, "f")); !slices.Equal(fork.Sessions, every) {
		t.Fatalf("f forks every selected session %v", fork.Sessions)
	}
	te.press(s, "d")
	if !strings.Contains(plain(s.View(90, 10)), "delete 3 sessions?") {
		t.Fatal("deleting many asks for all of them")
	}
	te.press(s, "esc")
	if len(s.confirm) != 0 || len(s.Selected()) != 3 {
		t.Fatal("esc keeps them")
	}
	te.press(s, "shift+up")
	if len(s.Selected()) != 2 {
		t.Fatalf("shift with a move back shrinks the selection %d", len(s.Selected()))
	}
	te.press(s, "k")
	if len(s.Selected()) != 1 || s.sel != 0 {
		t.Fatal("a plain move collapses the selection")
	}
	s.View(90, 10)
	row := func(i int, mod tea.KeyMod) tea.MouseClickMsg {
		return tea.MouseClickMsg{Button: tea.MouseLeft, X: 3, Y: s.top + render.TableHead + i, Mod: mod}
	}
	s.Update(row(2, 0))
	if s.sel != 2 || len(s.Selected()) != 1 {
		t.Fatalf("click selects, got %d", s.sel)
	}
	s.Update(row(0, tea.ModShift))
	if len(s.Selected()) != 3 {
		t.Fatal("shift click selects the range")
	}
	s.Update(row(1, 0))
	if len(s.Selected()) != 1 || s.sel != 1 {
		t.Fatal("a plain click collapses the range")
	}
	te.press(s, "/")
	typeText(s, "run")
	te.press(s, "enter")
	s.View(90, 10)
	s.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 3, Y: 9})
	if !s.bar.Active() {
		t.Fatal("clicking the bar takes typing again")
	}
	te.press(s, "down")
	te.press(s, "up")
	if !s.bar.Active() || s.sel != 0 {
		t.Fatalf("arrows move the rows while the bar takes typing, at %d", s.sel)
	}
	s.Update(tea.PasteMsg{Content: "!"})
	te.press(s, "backspace")
	te.press(s, "ctrl+u")
	if s.bar.Query() != "" || len(s.Rows()) != 3 {
		t.Fatal("bar edits refilter")
	}
	te.press(s, "esc")
	te.press(s, "pgdown")
	te.press(s, "pgup")
	s.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	s.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if h := s.Hints(); len(h) != 6 || h[0].Action != "sessions.resume" || h[3].Action != "filter.toggle" || h[4].Label != "move" || h[5].Action != "sessions.select.down" {
		t.Fatalf("hints %+v", h)
	}
	s.Update(wm.FocusMsg{Focused: true})
	if _, ok := find[wm.Intent](te.pressed(s, "r")); ok || len(s.Rows()) != 3 {
		t.Fatal("focus reads the store again and r is bound to nothing")
	}
	te.press(s, "d")
	s.Update(wm.FocusMsg{})
	if len(s.confirm) != 0 {
		t.Fatal("losing focus drops the question")
	}
}

func TestFindingsFilterResolveAndExport(t *testing.T) {
	te := newEnv(t)
	fs := te.live.Facts
	parent, _ := fs.Add(facts.Fact{Kind: "finding", Title: "open port 22", Tags: []string{"net"}, Confidence: 0.9, Agent: "root", Body: "ssh listens", Evidence: []facts.Evidence{{Kind: "file", Ref: "a.go", Line: 3}, {Kind: "url", Ref: "https://x"}}})
	fs.Add(facts.Fact{Kind: "risk", Title: "weak cipher", Tags: []string{"crypto"}, Confidence: 0.5, Parent: parent.ID})
	f := NewFindings(te.env, fs, te.live.Session.ID())
	if f.Key() != "findings:"+te.live.Session.ID() || f.Title() != "FINDINGS" || !f.Selectable() {
		t.Fatalf("identity %q", f.Key())
	}
	if len(f.Rows()) != 2 || f.Status() != "2 open" {
		t.Fatalf("rows %d status %q", len(f.Rows()), f.Status())
	}
	view := plain(f.View(80, 12))
	if !strings.Contains(view, "open port 22") || !strings.Contains(view, "weak cipher") || strings.Contains(view, "⌕") {
		t.Fatalf("view\n%s", view)
	}
	te.press(f, "c")
	if f.kind != "finding" || len(f.Rows()) != 1 {
		t.Fatalf("kind filter %q rows %d", f.kind, len(f.Rows()))
	}
	te.press(f, "c")
	te.press(f, "c")
	if f.kind != "" {
		t.Fatalf("kind cycles back, got %q", f.kind)
	}
	te.press(f, "T")
	if f.tag != "crypto" || len(f.Rows()) != 1 || f.Rows()[0].Title != "weak cipher" {
		t.Fatalf("tag filter %q", f.tag)
	}
	te.press(f, "T")
	te.press(f, "T")
	te.press(f, "t")
	if !f.tree || f.depth[f.Rows()[1].ID] != 1 {
		t.Fatalf("tree depth %v", f.depth)
	}
	te.press(f, "/")
	if !slices.Equal(f.Contexts(), []string{keys.Search, keys.Filter}) || f.Cursor() == nil {
		t.Fatal("slash opens the filter bar")
	}
	typeText(f, "port")
	if h := f.Hints(); h[0].Action != "search.accept" {
		t.Fatalf("search hints %+v", h)
	}
	te.press(f, "down")
	te.press(f, "up")
	f.Update(tea.PasteMsg{Content: "x"})
	te.press(f, "backspace")
	te.press(f, "enter")
	if len(f.Rows()) != 1 || f.Cursor() != nil {
		t.Fatalf("search rows %d", len(f.Rows()))
	}
	if foot := plain(strings.Split(f.View(80, 12), "\n")[11]); !strings.Contains(foot, "⚲ port") || !strings.Contains(foot, "1 fact") || f.Status() != "2 open ∷ tree" {
		t.Fatalf("filter bar along the bottom %q, status %q", foot, f.Status())
	}
	te.press(f, "/")
	if f.bar.Shown() || len(f.Rows()) != 2 {
		t.Fatal("slash collapses the bar and the filter")
	}
	te.press(f, "t")
	te.press(f, "enter")
	view = plain(f.View(80, 20))
	if !strings.Contains(view, "ssh listens") || !strings.Contains(view, "net") || !strings.Contains(view, "a.go") {
		t.Fatalf("expanded view\n%s", view)
	}
	msgs := te.pressed(f, "x")
	if _, ok := find[wm.Intent](msgs); !ok || f.Rows()[0].Status != facts.StatusResolved || f.Status() != "1 open" {
		t.Fatalf("resolve status %q", f.Status())
	}
	te.press(f, "s")
	if f.status != facts.StatusOpen || len(f.Rows()) != 1 {
		t.Fatalf("status filter %q rows %d", f.status, len(f.Rows()))
	}
	te.press(f, "s")
	te.press(f, "s")
	te.press(f, "s")
	exp, ok := find[ExportMsg](te.pressed(f, "e"))
	if !ok || exp.Err != nil || !strings.HasSuffix(exp.Path, ".md") || filepath.Dir(exp.Path) != te.dir {
		t.Fatalf("markdown export %+v", exp)
	}
	data, _ := os.ReadFile(exp.Path)
	if !strings.Contains(string(data), "open port 22") {
		t.Fatal("markdown content")
	}
	exp, ok = find[ExportMsg](te.pressed(f, "E"))
	if !ok || exp.Err != nil || !strings.HasSuffix(exp.Path, ".json") {
		t.Fatalf("json export %+v", exp)
	}
	data, _ = os.ReadFile(exp.Path)
	if !json.Valid(data) {
		t.Fatal("json content")
	}
	before := len(f.Rows())
	fs.Add(facts.Fact{Title: "late one"})
	f.Update(harness.FactMsg{Session: "other", Change: facts.Change{Op: facts.OpAdd}})
	if len(f.Rows()) != before {
		t.Fatal("other session changes are ignored")
	}
	msgs = run(func() tea.Cmd {
		_, c := f.Update(harness.FactMsg{Session: te.live.Session.ID(), Change: facts.Change{Op: facts.OpAdd}})
		return c
	}())
	if _, ok := find[wm.Intent](msgs); !ok || len(f.Rows()) != before+1 {
		t.Fatal("own session changes refresh and ask for attention")
	}
	te.press(f, "G")
	te.press(f, "g")
	te.press(f, "j")
	te.press(f, "k")
	f.View(80, 12)
	f.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: f.top + render.TableHead + 1})
	if f.sel != 1 {
		t.Fatalf("click selects, got %d", f.sel)
	}
	expanded := f.expanded
	f.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: f.top + render.TableHead + 1})
	if f.expanded == expanded {
		t.Fatal("clicking the selected row toggles its detail")
	}
	te.press(f, "/")
	typeText(f, "o")
	te.press(f, "enter")
	f.View(80, 12)
	f.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 11})
	if !f.bar.Active() || f.Cursor().Y != 11 {
		t.Fatal("clicking the bar takes typing again")
	}
	f.Update(wm.FocusMsg{})
	if f.bar.Active() {
		t.Fatal("losing focus leaves the bar")
	}
	te.press(f, "esc")
	if h := f.Hints(); len(h) != 10 || h[0].Action != "findings.detail" || h[8].Action != "filter.toggle" {
		t.Fatalf("hints %+v", h)
	}
}

func TestMissionCardTracksItsMission(t *testing.T) {
	te := newEnv(t)
	ms := te.live.Missions
	m, err := ms.Create("map the subnet")
	if err != nil {
		t.Fatal(err)
	}
	id := te.live.Session.ID()
	c := NewMission(te.env, ms, te.live.Facts, id, m)
	if c.Key() != "mission:"+id+":"+m.ID || c.ID() != m.ID || c.Title() != "MISSION "+m.ID || c.Status() != "0%" {
		t.Fatalf("identity %q status %q", c.Key(), c.Status())
	}
	planned, _ := ms.SetPlan(m.ID, []mission.Step{{Text: "ping sweep", Status: mission.StepDone}, {Text: "port scan", Status: mission.StepActive}, {Text: "report"}})
	c.Update(harness.MissionMsg{Session: "other", Mission: planned})
	if len(c.Mission().Plan) != 0 {
		t.Fatal("other session updates are ignored")
	}
	c.Update(harness.MissionMsg{Session: id, Mission: planned})
	if len(c.Mission().Plan) != 3 || c.Status() != "33%" {
		t.Fatalf("plan %d status %q", len(c.Mission().Plan), c.Status())
	}
	if h := c.Hints(); len(h) != 2 || h[0].Action != "mission.done" || !slices.Equal(c.Contexts(), []string{keys.Mission, keys.Panel}) {
		t.Fatalf("active hints %+v", h)
	}
	view := plain(c.View(60, 12))
	for _, want := range []string{"map the subnet", "active", "33%", "ping sweep", "port scan", "report", "findings 0"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view lacks %q\n%s", want, view)
		}
	}
	st, ok := find[MissionStateMsg](te.pressed(c, "d"))
	if !ok || st.Session != id || st.ID != m.ID || st.Status != mission.StatusDone {
		t.Fatalf("done %+v", st)
	}
	st, ok = find[MissionStateMsg](te.pressed(c, "x"))
	if !ok || st.Status != mission.StatusFailed {
		t.Fatalf("failed %+v", st)
	}
	if _, ok := find[MissionRemoveMsg](te.pressed(c, "D")); ok {
		t.Fatal("an active mission is not removable")
	}
	ended, _ := ms.Finish(m.ID, mission.StatusDone, "")
	c.Update(harness.MissionMsg{Session: id, Mission: ended})
	if c.Status() != mission.StatusDone || !strings.Contains(plain(c.View(60, 12)), "done") {
		t.Fatalf("ended status %q", c.Status())
	}
	if h := c.Hints(); len(h) != 2 || h[0].Action != "mission.resume" || h[1].Action != "mission.forget" {
		t.Fatalf("ended hints %+v", h)
	}
	st, ok = find[MissionStateMsg](te.pressed(c, "r"))
	if !ok || st.Status != mission.StatusActive {
		t.Fatalf("resume %+v", st)
	}
	rm, ok := find[MissionRemoveMsg](te.pressed(c, "D"))
	if !ok || rm.Session != id || rm.ID != m.ID {
		t.Fatalf("remove %+v", rm)
	}
}

func TestTelemetryShowsAttemptsAndFailovers(t *testing.T) {
	te := newEnv(t)
	c := NewTelemetry(te.env)
	if c.Title() != "METRICS" || c.Key() != "metrics" || !slices.Equal(c.Contexts(), []string{keys.Panel}) {
		t.Fatalf("identity %q %q", c.Title(), c.Key())
	}
	if c.Snapshot().Model != "claude-opus-5" {
		t.Fatalf("snapshot %+v", c.Snapshot())
	}
	snap := te.env.Meter.Snapshot()
	snap.Usage = protocol.Usage{Input: 1200, Output: 300}
	snap.Cost = 0.42
	snap.Context = 5000
	snap.Requests = 2
	snap.Retries = 1
	snap.LastLatency = 800 * time.Millisecond
	snap.RateLimit = provider.RateLimit{Requests: 50, RequestsRemaining: 49}
	c.Update(harness.AttemptMsg{Attempt: provider.Attempt{Status: 200, Duration: 800 * time.Millisecond}, Telemetry: snap})
	c.Update(harness.AttemptMsg{Attempt: provider.Attempt{Status: 429, Err: errors.New("slow down"), Wait: 2 * time.Second}, Telemetry: snap})
	c.Update(harness.AttemptMsg{Attempt: provider.Attempt{Status: 500, Err: errors.New("broken")}, Telemetry: snap})
	c.Update(harness.ServedMsg{Index: 1})
	msgs := run(func() tea.Cmd { _, cmd := c.Update(harness.FailoverMsg{From: 0, Err: errors.New("dead")}); return cmd }())
	if _, ok := find[wm.Intent](msgs); !ok {
		t.Fatal("failover notifies")
	}
	view := plain(c.View(100, 20))
	for _, want := range []string{"claude-opus-5", "via anthropic", "$0.42", "1.2k", "requests 2", "1 retries", "200 in", "retry in 2.0s after 429", "broken", "served by fallback model 1", "failover past model 0", "req 49/50", "latency 800ms"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view lacks %q\n%s", want, view)
		}
	}
	if c.Status() != "$0.42" {
		t.Fatalf("status %q", c.Status())
	}
	small := plain(c.View(30, 3))
	if strings.Count(small, "\n") > 2 {
		t.Fatalf("small view keeps to its height\n%s", small)
	}
	tick, ok := find[tickMsg](run(func() tea.Cmd {
		_, cmd := c.Update(harness.EventMsg{Event: agent.Event{Type: agent.EventTurnStart}})
		return cmd
	}()))
	if !ok || tick.owner != &c.timer {
		t.Fatal("turn start begins sampling")
	}
	c.Update(tick)
	if len(c.samples) != 1 || c.timer.armed {
		t.Fatalf("samples %d armed %v", len(c.samples), c.timer.armed)
	}
	c.Update(harness.EventMsg{Event: agent.Event{Type: agent.EventCompactEnd}})
	if !strings.Contains(plain(c.View(100, 20)), "compacted") {
		t.Fatal("compaction noted")
	}
	c.Reload()
	if len(c.samples) != 0 {
		t.Fatal("reload drops samples")
	}
}

func TestFilesShowsTouchesAndDiffs(t *testing.T) {
	te := newEnv(t)
	c := NewFiles(te.env)
	if c.Title() != "FILES" || c.Key() != "files" || !c.Selectable() {
		t.Fatalf("identity %q %q", c.Title(), c.Key())
	}
	if !strings.Contains(plain(c.View(60, 5)), "no files touched") || c.Status() != "0 files" {
		t.Fatalf("empty status %q", c.Status())
	}
	read := agent.Event{Type: agent.EventToolEnd, Time: at, Agent: "root", Call: protocol.ToolCall{ID: "r1", Name: files.ReadName}, Data: files.ReadData{Path: "a.go", Lines: []string{"x"}, Total: 1}}
	edit := agent.Event{Type: agent.EventToolEnd, Time: at.Add(time.Second), Agent: "root", Call: protocol.ToolCall{ID: "e1", Name: files.EditName}, Data: files.Change{Path: "b.go", Old: "one\ntwo\n", New: "one\nthree\n"}}
	te.env.Work.Apply(read)
	te.env.Work.Apply(edit)
	te.env.Work.Apply(agent.Event{Type: agent.EventToolEnd, Data: files.GlobData{Total: 3}})
	c.Update(harness.EventMsg{Event: read})
	c.Update(harness.EventMsg{Event: edit})
	if len(c.Files()) != 2 || c.Status() != "1 changed" {
		t.Fatalf("files %d status %q", len(c.Files()), c.Status())
	}
	view := plain(c.View(80, 12))
	for _, want := range []string{"cwd", "searches 1", "a.go", "b.go", "+1", "-1", "root"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view lacks %q\n%s", want, view)
		}
	}
	if h := c.Hints(); len(h) != 2 || h[0].Action != "files.diff" {
		t.Fatalf("hints before a diff %+v", h)
	}
	te.press(c, "enter")
	view = plain(c.View(80, 14))
	if !strings.Contains(view, "last change") || !strings.Contains(view, "three") {
		t.Fatalf("diff view\n%s", view)
	}
	if h := c.Hints(); len(h) != 4 || h[2].Pair != "files.scroll.up" {
		t.Fatalf("hints with a diff %+v", h)
	}
	te.press(c, "a")
	if !strings.Contains(plain(c.View(80, 14)), "since start") {
		t.Fatal("whole diff label")
	}
	dark := c.View(80, 14)
	te.env.Theme = theme.Light()
	if c.View(80, 14) == dark || c.shown.theme != "light" {
		t.Fatal("a new theme repaints the cached diff")
	}
	te.env.Theme = theme.Blackwall()
	long := agent.Event{Type: agent.EventToolEnd, Time: at.Add(2 * time.Second), Agent: "root", Call: protocol.ToolCall{ID: "e2", Name: files.EditName}, Data: files.Change{Path: "c.go", Old: "", New: strings.Repeat("added line\n", 40)}}
	te.env.Work.Apply(long)
	c.Update(harness.EventMsg{Event: long})
	te.press(c, "g")
	lines := strings.Split(plain(c.View(80, 20)), "\n")
	if c.Files()[0].Path != "c.go" || !strings.HasSuffix(lines[c.diffTop], "⇞") || !strings.HasSuffix(lines[len(lines)-1], "⇟") {
		t.Fatalf("a long diff gets a scrollbar\n%s", strings.Join(lines, "\n"))
	}
	te.press(c, "pgdown")
	if c.scroll == 0 {
		t.Fatal("pgdown pages the diff")
	}
	te.press(c, "pgup")
	if c.scroll != 0 {
		t.Fatal("pgup pages back")
	}
	c.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 79, Y: len(lines) - 1})
	if c.scroll == 0 {
		t.Fatal("clicking the bottom cap pages the diff")
	}
	c.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	c.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 79, Y: c.diffTop + c.diffRows/2})
	jumped := c.scroll
	c.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 79, Y: c.diffTop})
	if jumped == 0 || c.scroll >= jumped {
		t.Fatalf("a click on the track jumps and the top cap pages back, %d then %d", jumped, c.scroll)
	}
	c.scroll = 0
	c.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	c.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if c.scroll != 0 {
		t.Fatal("wheel scroll")
	}
	te.press(c, "j")
	te.press(c, "j")
	if c.sel != 2 || !strings.Contains(plain(c.View(80, 14)), "read only") {
		t.Fatalf("read only file sel %d", c.sel)
	}
	te.press(c, "k")
	te.press(c, "G")
	te.press(c, "g")
	c.View(80, 14)
	c.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 1 + render.TableHead + 1})
	if c.sel != 1 || !c.expanded {
		t.Fatalf("click selects, got %d", c.sel)
	}
	c.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 1 + render.TableHead + 1})
	if c.expanded {
		t.Fatal("clicking the selected row folds its diff")
	}
	if !slices.Equal(c.Contexts(), []string{keys.Files, keys.List, keys.Panel}) {
		t.Fatalf("contexts %v", c.Contexts())
	}
}

func TestJobsTailsBackgroundJobs(t *testing.T) {
	te := newEnv(t)
	j := NewJobs(te.env)
	if j.Title() != "JOBS" || j.Key() != "jobs" || !j.Selectable() || j.Status() != "" {
		t.Fatalf("identity %q %q", j.Title(), j.Key())
	}
	if !strings.Contains(plain(j.View(60, 5)), "no jobs") {
		t.Fatal("empty view")
	}
	res, err := te.sh.Bash().Run(context.Background(), json.RawMessage(`{"command":"echo hello; sleep 0.2; echo bye","run_in_background":true,"description":"greets"}`))
	if err != nil || res.IsError {
		t.Fatalf("job start %v %+v", err, res)
	}
	jobs := te.sh.Jobs()
	if len(jobs) != 1 {
		t.Fatalf("jobs %d", len(jobs))
	}
	msgs := run(func() tea.Cmd {
		_, c := j.Update(harness.JobMsg{Event: shell.JobEvent{Kind: shell.JobStarted, Job: jobs[0]}})
		return c
	}())
	if _, ok := find[wm.Intent](msgs); !ok || len(j.List()) != 1 || j.Status() != "1 running" {
		t.Fatalf("started job list %d status %q", len(j.List()), j.Status())
	}
	kill, ok := find[KillJobMsg](te.pressed(j, "x"))
	if !ok || kill.Job != jobs[0].ID {
		t.Fatalf("kill %+v", kill)
	}
	done, _ := te.sh.Wait(jobs[0].ID)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("job never finished")
	}
	j.Update(harness.JobMsg{Event: shell.JobEvent{Kind: shell.JobFinished, Job: jobs[0]}})
	view := plain(j.View(80, 12))
	for _, want := range []string{"greets", "done", "hello", "bye"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view lacks %q\n%s", want, view)
		}
	}
	if j.Status() != "" {
		t.Fatalf("status %q", j.Status())
	}
	if _, ok := find[wm.Intent](te.pressed(j, "x")); !ok {
		t.Fatal("killing a finished job notifies")
	}
	lines := strings.Split(plain(j.View(80, 12)), "\n")
	if strings.HasSuffix(lines[j.listH+1], "⇞") || strings.HasSuffix(lines[11], "⇟") {
		t.Fatalf("a tail that fits gets no scrollbar\n%s", strings.Join(lines, "\n"))
	}
	j.tails[jobs[0].ID].buf = append(j.tails[jobs[0].ID].buf, []byte(strings.Repeat("more\n", 20))...)
	lines = strings.Split(plain(j.View(80, 12)), "\n")
	if !strings.HasSuffix(lines[j.listH+1], "⇞") || !strings.HasSuffix(lines[11], "⇟") {
		t.Fatalf("a tail that scrolls gets a scrollbar\n%s", strings.Join(lines, "\n"))
	}
	te.press(j, "pgup")
	if j.follow {
		t.Fatal("pgup holds the tail still")
	}
	te.press(j, "f")
	if !j.follow {
		t.Fatal("f follows again")
	}
	te.press(j, "f")
	te.press(j, "pgdown")
	if !j.follow {
		t.Fatal("paging to the end follows")
	}
	j.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 79, Y: j.listH + 1})
	if j.follow {
		t.Fatal("clicking the top cap pages the tail up")
	}
	j.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 79, Y: 11})
	j.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 79, Y: j.listH + 4})
	te.press(j, "j")
	te.press(j, "k")
	te.press(j, "G")
	te.press(j, "g")
	if j.sel != 0 {
		t.Fatal("list keys move the job selection")
	}
	j.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: render.TableHead})
	j.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if h := j.Hints(); len(h) != 4 || h[0].Action != "jobs.stop" || h[2].Label != "tail" {
		t.Fatalf("hints %+v", h)
	}
	short := plain(j.View(40, 2))
	if strings.Count(short, "\n") > 1 {
		t.Fatalf("short view keeps to its height\n%s", short)
	}
}

func TestComposerSuggestsArgumentsAndCardsShowHints(t *testing.T) {
	te := newEnv(t)
	c := NewComposer(te.env.Theme, te.env.Keys, "", func() string { return te.dir })
	c.Bind("s1")
	c.SetCommands([]Command{{Name: "mode", Desc: "mode", Arg: "name", Suggest: func(string) []string { return []string{"default", "plan", "bypassPermissions"} }, Action: "mode.cycle"}, {Name: "compact", Desc: "now", Action: "session.compact"}})
	c.SetFocus(true)
	for _, r := range "/" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	popup := strings.Split(plain(c.View(60, 4)), "\n")
	if !strings.HasSuffix(popup[0], "shift+tab ") || strings.Contains(popup[1], "+") {
		t.Fatalf("each command shows the key of its action when it has one\n%s", strings.Join(popup, "\n"))
	}
	te.env.Keys.Rebind("session.compact", "alt+c")
	if !strings.HasSuffix(strings.Split(plain(c.View(60, 4)), "\n")[1], "alt+c ") {
		t.Fatal("the popup follows the keymap")
	}
	te.env.Keys.Reset("session.compact")
	c.ta.Reset()
	c.comp = nil
	for _, r := range "/mode" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if got := c.Completions(); len(got) != 1 || got[0] != "/mode" {
		t.Fatalf("command list while typing %v", got)
	}
	te.compose(c, "enter")
	if c.Value() != "/mode " {
		t.Fatalf("enter fills a command that takes an argument %q", c.Value())
	}
	if got := c.Completions(); len(got) != 3 {
		t.Fatalf("argument values offered at once %v", got)
	}
	for _, r := range "pl" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if got := c.Completions(); len(got) != 1 || got[0] != "plan" {
		t.Fatalf("typed prefix filters the values %v", got)
	}
	if c.Height() != 3 {
		t.Fatalf("popup adds rows %d", c.Height())
	}
	ran, ok := find[CommandMsg](run(te.compose(c, "enter")))
	if !ok || ran.Name != "mode" || ran.Args != "plan" || ran.Session != "s1" || c.Value() != "" {
		t.Fatalf("picking a value sends the command %+v %q", ran, c.Value())
	}
	for _, r := range "/x" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if c.Completions() != nil {
		t.Fatal("no match closes the popup")
	}
	c.ta.Reset()
	for _, r := range "/c" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	te.compose(c, "esc")
	if c.Completions() != nil {
		t.Fatal("esc closes the popup")
	}
	c.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	if got := c.Completions(); len(got) != 1 || got[0] != "/compact" {
		t.Fatalf("typing on reopens the popup %v", got)
	}
	s := NewSession(te.env, te.live, "")
	if hs := s.Hints(); len(hs) != 2 || hs[0].Action != "session.search" {
		t.Fatalf("session hints %v", hs)
	}
	s.Transcript.Search()
	if hs := s.Hints(); len(hs) != 3 || hs[2].Action != "search.close" {
		t.Fatalf("search hints %v", hs)
	}
	if wide := plain(s.Transcript.View(60, 8)); !strings.Contains(wide, "idle") || !strings.Contains(wide, art.Wordmark[0]) || !strings.Contains(wide, te.env.Theme.Glyphs.Idle) {
		t.Fatalf("an empty session shows the wordmark over the idle line\n%s", wide)
	}
	if small := plain(s.Transcript.View(30, 3)); strings.Contains(small, art.Wordmark[0]) || !strings.Contains(small, "idle") {
		t.Fatalf("a small empty session keeps the idle line alone\n%s", small)
	}
	if !strings.Contains(plain(s.Transcript.View(60, 8)), "idle") {
		t.Fatal("an empty transcript names the model as waiting")
	}
	for _, h := range []wm.Hinter{NewAgents(te.env), NewSessions(te.env), NewFindings(te.env, te.live.Facts, "s1"), NewFiles(te.env), NewJobs(te.env), NewConfig(te.env)} {
		if len(h.Hints()) == 0 {
			t.Fatalf("%T has no hints", h)
		}
	}
}

func TestComposerBreaksLinesWithABackslash(t *testing.T) {
	te := newEnv(t)
	c := NewComposer(te.env.Theme, te.env.Keys, "", func() string { return te.dir })
	c.SetFocus(true)
	for _, r := range `one\` {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if msgs := run(te.compose(c, "enter")); len(msgs) != 0 || c.Value() != "one\n" || c.Height() != 2 {
		t.Fatalf("a backslash before enter becomes a newline %q %v", c.Value(), msgs)
	}
	for _, r := range "two" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if sent, ok := find[SubmitMsg](run(te.compose(c, "enter"))); !ok || sent.Text != "one\ntwo" {
		t.Fatalf("enter without a backslash sends %+v", sent)
	}
	for _, r := range `ab\cd` {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	te.compose(c, "left")
	te.compose(c, "left")
	te.compose(c, "enter")
	c.Update(tea.KeyPressMsg{Code: 'X', Text: "X"})
	if c.Value() != "ab\nXcd" {
		t.Fatalf("a break mid line leaves the cursor at the start of the new line %q", c.Value())
	}
	te.compose(c, "shift+enter")
	te.compose(c, "alt+enter")
	if c.Value() != "ab\nXcd" {
		t.Fatalf("no other chord breaks a line %q", c.Value())
	}
}

func TestComposerKeepsTheFirstLineInViewAcrossBreaks(t *testing.T) {
	te := newEnv(t)
	c := NewComposer(te.env.Theme, te.env.Keys, "", func() string { return te.dir })
	c.SetFocus(true)
	c.View(60, 2)
	for i, word := range []string{"first", "second", "third"} {
		for _, r := range word + `\` {
			c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		te.compose(c, "enter")
		rows := strings.Split(plain(c.View(60, c.Height())), "\n")
		if c.Height() != max(i+2, 2) || len(rows) != c.Height() || !strings.Contains(rows[0], "first") {
			t.Fatalf("after %d breaks the first line stays on top in %d rows\n%s", i+1, c.Height(), strings.Join(rows, "\n"))
		}
		if cur := c.Cursor(); cur == nil || cur.Y != i+1 {
			t.Fatalf("the cursor sits on the new line %+v", cur)
		}
	}
}

// Command half of a card update
func second(_ wm.Card, cmd tea.Cmd) tea.Cmd { return cmd }

func TestKeyedHintsDropUnboundKeys(t *testing.T) {
	te := newEnv(t)
	if te.env.keyed("confirm.yes", "yes") != "y yes" {
		t.Fatal("a bound action leads with its key")
	}
	te.env.Keys.Unbind("confirm.no")
	if te.env.keyed("confirm.no", "no") != "no" {
		t.Fatal("an unbound action reads as the text alone")
	}
	te.env.Store.Save(context.Background(), session.New())
	s := NewSessions(te.env)
	te.press(s, "d")
	if foot := plain(strings.Split(s.View(80, 5), "\n")[4]); !strings.HasSuffix(strings.TrimSpace(foot), "y yes ∷ no") {
		t.Fatalf("the question drops the missing key %q", foot)
	}
}

func TestComposerMeasuresAWrappedDraftAtTheCardWidth(t *testing.T) {
	te := newEnv(t)
	dir := t.TempDir()
	os.MkdirAll(filepath.Dir(DraftPath(dir, te.live.Session.ID())), 0o755)
	os.WriteFile(DraftPath(dir, te.live.Session.ID()), []byte(strings.Repeat("word ", 30)), 0o600)
	s := NewSession(te.env, te.live, dir)
	view := strings.Split(plain(s.View(60, 14)), "\n")
	h := s.Composer.Height()
	if h < 3 || strings.TrimSpace(view[len(view)-1]) == "" || !strings.Contains(view[len(view)-h], "word") {
		t.Fatalf("a long draft fills the rows it wraps to on the first paint, %d rows\n%s", h, strings.Join(view, "\n"))
	}
}

func TestComposerKeepsADraftPerSession(t *testing.T) {
	te := newEnv(t)
	dir := filepath.Join(te.dir, "cfg")
	c := NewComposer(te.env.Theme, te.env.Keys, dir, func() string { return te.dir })
	c.Bind("s1")
	run(c.SetFocus(true))
	for _, r := range "for one" {
		c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if data, err := os.ReadFile(DraftPath(dir, "s1")); err != nil || string(data) != "for one" {
		t.Fatalf("the draft lands in the file of its session %q %v", data, err)
	}
	c.Bind("s2")
	if _, err := os.Stat(DraftPath(dir, "s1")); !os.IsNotExist(err) {
		t.Fatal("typed text moves to the session bound next and leaves the old one")
	}
	if data, err := os.ReadFile(DraftPath(dir, "s2")); err != nil || string(data) != "for one" || c.SessionID != "s2" {
		t.Fatalf("the moved draft %q %v", data, err)
	}
	c.Bind("s2")
	c.ta.Reset()
	c.persist()
	if _, err := os.Stat(DraftPath(dir, "s2")); !os.IsNotExist(err) {
		t.Fatal("clearing the prompt removes the draft file")
	}
	os.WriteFile(DraftPath(dir, "s3"), []byte("third"), 0o600)
	c.Bind("s3")
	if c.Value() != "third" || c.SessionID != "s3" {
		t.Fatalf("binding an empty prompt reads the draft of the session %q", c.Value())
	}
	other := NewComposer(te.env.Theme, te.env.Keys, dir, func() string { return te.dir })
	other.Bind("s1")
	if other.Value() != "" {
		t.Fatalf("another session starts clean %q", other.Value())
	}
	bare := NewComposer(te.env.Theme, te.env.Keys, "", func() string { return te.dir })
	bare.Bind("s3")
	if bare.Value() != "" || bare.SessionID != "s3" {
		t.Fatal("a composer without a directory binds without reading anything")
	}
}

func TestSessionKeepsItsViewStateBetweenRuns(t *testing.T) {
	te := newEnv(t)
	s := NewSession(te.env, te.live, "")
	if !s.Transcript.Thinking() {
		t.Fatal("thinking starts shown")
	}
	s.Transcript.ShowThinking(false)
	raw, err := json.Marshal(s.State())
	if err != nil || string(raw) != `{"thinking_hidden":true}` {
		t.Fatalf("state %s %v", raw, err)
	}
	again := NewSession(te.env, te.live, "")
	if err := again.SetState(raw); err != nil || again.Transcript.Thinking() {
		t.Fatalf("the saved state shows thinking again %v", err)
	}
	if again.SetState(json.RawMessage(`{`)) == nil {
		t.Fatal("a broken state is an error")
	}
	if _, ok := wm.Card(again).(wm.Stateful); !ok {
		t.Fatal("the session card is a stateful card")
	}
}

func TestTranscriptGlidesAfterItsOffset(t *testing.T) {
	te := newEnv(t)
	tr := NewTranscript(te.env)
	for i := 0; i < 40; i++ {
		tr.Apply(agent.Event{Type: agent.EventNote, Time: at, Text: fmt.Sprintf("line %02d", i)})
	}
	tr.View(60, 8)
	if tr.paintedOffset() != tr.vp.YOffset() || tr.vp.YOffset() == 0 {
		t.Fatal("before any frame the painted offset is the viewport's, at the end while following")
	}
	if tr.Animate(1.0 / 60) {
		t.Fatal("the first frame settles the glide where the viewport is")
	}
	tr.Top()
	if tr.paintedOffset() == 0 {
		t.Fatal("the painted offset trails the viewport")
	}
	if !tr.Animate(1.0 / 60) {
		t.Fatal("the glide moves")
	}
	if tr.paintedOffset() > 3*8 {
		t.Fatalf("a long jump trails by three pages at most, %d", tr.paintedOffset())
	}
	first := plain(strings.Split(tr.View(60, 8), "\n")[0])
	if strings.Contains(first, "line 00") {
		t.Fatalf("the view paints the gliding offset %q", first)
	}
	for i := 0; i < 400 && tr.Animate(1.0/60); i++ {
	}
	if tr.paintedOffset() != 0 || !strings.Contains(plain(strings.Split(tr.View(60, 8), "\n")[0]), "line 00") {
		t.Fatal("the glide lands on the viewport offset")
	}
	tr.Follow()
	if tr.Animate(0) != true || tr.glide || tr.paintedOffset() != tr.vp.YOffset() {
		t.Fatal("a zero frame snaps the painted offset")
	}
	if tr.Animate(0) {
		t.Fatal("a second zero frame changes nothing")
	}
	s := NewSession(te.env, te.live, t.TempDir())
	if s.Animate(1.0/60) || !s.Transcript.glide {
		t.Fatal("the session hands frames to its transcript")
	}
}

func TestThoughtsMaterializeAsTheyStream(t *testing.T) {
	te := newEnv(t)
	tr := NewTranscript(te.env)
	tr.Apply(agent.Event{Type: agent.EventRunStart, Time: at, Parts: []protocol.Part{protocol.Text{Text: "go"}}})
	tr.Apply(agent.Event{Type: agent.EventTurnStart, Time: at, Round: 1})
	resp := protocol.Response{Content: []protocol.Part{protocol.Thinking{Text: "weigh the options"}}}
	tr.Apply(agent.Event{Type: agent.EventModel, Time: at, Round: 1, Model: protocol.Event{Type: protocol.EventThinkingDelta, Index: 0, Delta: "weigh the options", Response: resp}})
	text := func() string { return plain(strings.Join(tr.Lines(80), "\n")) }
	if fresh := text(); strings.Contains(fresh, "options") || !strings.ContainsAny(fresh, string(te.env.Theme.Glyphs.Churn)) {
		t.Fatalf("a fresh thought boils\n%s", fresh)
	}
	if !tr.settling() || tr.ensureTick() == nil {
		t.Fatal("a boiling thought keeps the ticks coming")
	}
	te.env.Now = func() time.Time { return at.Add(settleTime / 2) }
	if half := text(); !strings.Contains(half, "weigh") || strings.Contains(half, "options") {
		t.Fatalf("a thought settles left to right\n%s", half)
	}
	te.env.Now = func() time.Time { return at.Add(settleTime) }
	if full := text(); !strings.Contains(full, "weigh the options") || tr.settling() {
		t.Fatalf("a settled thought reads whole\n%s", full)
	}
	tr.ShowThinking(false)
	if streaming := text(); !strings.Contains(streaming, "weigh the options") {
		t.Fatalf("a streaming thought shows whole even while hidden\n%s", streaming)
	}
	tr.Apply(agent.Event{Type: agent.EventModel, Time: at.Add(time.Second), Round: 1, Model: protocol.Event{Type: protocol.EventThinkingEnd, Index: 0, Response: resp}})
	if done := text(); strings.Contains(done, "weigh") || !strings.Contains(done, "thinking "+te.env.Theme.Glyphs.Sep+" 1.0s") {
		t.Fatalf("a finished thought folds when hidden and shows how long it took\n%s", done)
	}
	tr.ShowThinking(true)
	if shown := text(); !strings.Contains(shown, "weigh the options") {
		t.Fatalf("showing brings a finished thought back\n%s", shown)
	}
	late := &block{kind: bThinking, text: "stale", grown: at.Add(time.Hour), settled: 0}
	if late.resolved(at) != 5 || (&block{kind: bThinking, text: "old"}).resolved(at) != 3 {
		t.Fatal("thoughts timed from the future or never timed read settled")
	}
}

// Cache keys of every block, empty for one not rendered
func blockKeys(tr *Transcript) []string {
	var out []string
	for _, b := range tr.blocks {
		out = append(out, b.cacheKey)
	}
	return out
}

func TestTranscriptKeepsBlocksAcrossHeightChangesAndMemoizesItsView(t *testing.T) {
	tr := benchTranscript(3)
	tr.View(80, 20)
	keys := blockKeys(tr)
	tr.View(80, 25)
	if !slices.Equal(blockKeys(tr), keys) {
		t.Fatal("a height change keeps every rendered block")
	}
	tr.View(81, 25)
	for i, k := range blockKeys(tr) {
		if k == keys[i] {
			t.Fatal("a width change renders every block again")
		}
	}
	first := tr.View(81, 25)
	gen := tr.gen
	if tr.View(81, 25) != first || tr.gen != gen {
		t.Fatal("an unchanged transcript hands back the same view without painting again")
	}
	tr.ScrollBy(-3)
	if tr.View(81, 25) == first {
		t.Fatal("scrolling changes the view")
	}
	tr.Follow()
	if tr.View(81, 25) != first {
		t.Fatal("back at the end the view is the same again")
	}
}

// Pending tick of a ticker, the message its last start scheduled
func pendingTick(k *ticker) tickMsg { return tickMsg{owner: k, gen: k.gen} }

func TestTicksDriveOnlyTheirOwnCard(t *testing.T) {
	te := newEnv(t)
	streaming := func() *Transcript {
		tr := NewTranscript(te.env)
		if _, cmd := tr.Update(harness.EventMsg{Event: agent.Event{Type: agent.EventTurnStart, Time: at}}); cmd == nil {
			t.Fatal("a streaming transcript ticks")
		}
		return tr
	}
	a, b := streaming(), streaming()
	pending := []tickMsg{pendingTick(&a.timer), pendingTick(&b.timer)}
	for round := 0; round < 6; round++ {
		var next []tickMsg
		for _, tick := range pending {
			for _, tr := range []*Transcript{a, b} {
				if _, cmd := tr.Update(tick); cmd != nil {
					next = append(next, pendingTick(&tr.timer))
				}
			}
		}
		if len(next) != 2 {
			t.Fatalf("round %d leaves %d ticks pending where each streaming transcript keeps one", round, len(next))
		}
		pending = next
	}
	if a.tick != 6 || b.tick != 6 {
		t.Fatalf("spinners advanced %d and %d times in six rounds", a.tick, b.tick)
	}
	lost := pendingTick(&a.timer)
	if a.Init() == nil {
		t.Fatal("a reopened streaming transcript starts a fresh loop")
	}
	if _, cmd := a.Update(lost); cmd != nil || a.tick != 6 {
		t.Fatal("the tick of the abandoned loop is ignored")
	}
	if _, cmd := a.Update(pendingTick(&a.timer)); cmd == nil || a.tick != 7 {
		t.Fatal("the fresh loop goes on")
	}
	hold := make(chan struct{})
	r := te.startRun(t, hold, "done")
	x, y := NewAgent(te.env, r), NewAgent(te.env, r)
	if x.Init() == nil || y.Init() == nil {
		t.Fatal("running agent cards tick")
	}
	if _, cmd := y.Update(pendingTick(&x.timer)); cmd != nil || y.tick != 0 {
		t.Fatal("an agent card ignores the clock of another")
	}
	if _, cmd := x.Update(pendingTick(&x.timer)); cmd == nil || x.tick != 1 {
		t.Fatal("an agent card follows its own clock")
	}
	close(hold)
	te.waitRun(t, r)
}

func TestStreamingMarkdownKeepsSettledParagraphs(t *testing.T) {
	env := &Env{Theme: theme.Blackwall(), Keys: keys.Default(), Now: time.Now}
	tr := NewTranscript(env)
	tr.Apply(agent.Event{Type: agent.EventRunStart, Time: at, Parts: []protocol.Part{protocol.Text{Text: "go"}}})
	tr.Apply(agent.Event{Type: agent.EventTurnStart, Time: at})
	stream := func(text string) {
		resp := protocol.Response{Model: "m", Content: []protocol.Part{protocol.Text{Text: text}}}
		tr.Apply(agent.Event{Type: agent.EventModel, Time: at, Model: protocol.Event{Type: protocol.EventTextDelta, Response: resp}})
		tr.View(80, 30)
	}
	stream("First paragraph here.\n\n- one\n- two\n\nSecond paragraph that is still")
	b := tr.blocks[len(tr.blocks)-1]
	if b.headKey == "" || !strings.Contains(b.head, "two") || strings.Contains(b.head, "Second") {
		t.Fatalf("the settled paragraphs ahead of the open one are rendered on their own %q", plain(b.head))
	}
	head := b.headKey
	stream("First paragraph here.\n\n- one\n- two\n\nSecond paragraph that is still growing")
	if b.headKey != head || !strings.Contains(plain(strings.Join(tr.lines, "\n")), "still growing") {
		t.Fatal("more of the open paragraph leaves the settled render alone and shows")
	}
	stream("First paragraph here.\n\n- one\n- two\n\nSecond paragraph that is still growing\n\nThird opens")
	if b.headKey == head || !strings.Contains(b.head, "growing") {
		t.Fatal("a finished paragraph joins the settled render")
	}
	final := "First paragraph here.\n\n- one\n- two\n\nSecond paragraph that is still growing\n\nThird opens and closes."
	stream(final)
	resp := protocol.Response{Model: "m", Content: []protocol.Part{protocol.Text{Text: final}}}
	tr.Apply(agent.Event{Type: agent.EventTurnEnd, Time: at, Response: &resp})
	tr.View(80, 30)
	whole := NewTranscript(env)
	whole.Apply(agent.Event{Type: agent.EventRunStart, Time: at, Parts: []protocol.Part{protocol.Text{Text: "go"}}})
	whole.Apply(agent.Event{Type: agent.EventTurnStart, Time: at})
	whole.Apply(agent.Event{Type: agent.EventModel, Time: at, Model: protocol.Event{Type: protocol.EventTextDelta, Response: resp}})
	whole.Apply(agent.Event{Type: agent.EventTurnEnd, Time: at, Response: &resp})
	if tr.View(80, 30) != whole.View(80, 30) {
		t.Fatal("a finished block renders as one piece, the same as one that never streamed")
	}
}

func TestSettledCutFindsTheLastProseParagraph(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"", ""},
		{"one paragraph only", ""},
		{"a\n\nb", "b"},
		{"a\n\nb\n\nc", "c"},
		{"a\n\n- item", ""},
		{"a\n\n* item", ""},
		{"a\n\n1. item", ""},
		{"a\n\n> quote", ""},
		{"a\n\n# heading", ""},
		{"a\n\n  indented", ""},
		{"a\n\n*emphasis* leads", "*emphasis* leads"},
		{"a\n\n```\ncode\n\nmore\n```\n\nafter", "after"},
		{"a\n\n```\ncode\n\nmore", ""},
		{"a\n\nb\n\n~~~\nx\n\ny", "b\n\n~~~\nx\n\ny"},
	}
	for _, c := range cases {
		got := settledCut(c.text)
		want := 0
		if c.want != "" {
			want = strings.LastIndex(c.text, c.want)
		}
		if got != want {
			t.Fatalf("%q cuts at %d, want %d", c.text, got, want)
		}
	}
}
