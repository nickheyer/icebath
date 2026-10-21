package cards

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/agent"
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
	"github.com/nickheyer/icebath/pkg/ui/deck"
	"github.com/nickheyer/icebath/pkg/ui/theme"
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

func (s *sink) all() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]any(nil), s.msgs...)
}

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

func keyp(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	}
	r := []rune(s)
	return tea.KeyPressMsg{Code: r[0], Text: s}
}

func typeText(c deck.Card, text string) deck.Card {
	for _, r := range text {
		c, _ = c.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	return c
}

func newEnv(t *testing.T) (*Env, *sink) {
	t.Helper()
	s := &sink{}
	dir := t.TempDir()
	sh, err := shell.New(shell.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sh.Close)
	reg := &tool.Registry{}
	reg.Add(files.All(files.Options{Root: dir})...)
	pol, _ := rules.New(rules.Set{Allow: []string{"Read"}, Deny: []string{"Bash(rm *)"}})
	sess := session.New()
	fs := facts.New()
	fs.Attach(sess)
	ms := mission.New()
	ms.Attach(sess)
	pump := harness.NewPump(s.send)
	ctl := harness.NewController(s.send, pump)
	ctl.Missions = ms
	meter := harness.NewMeter(s.send)
	opus, _ := profiles.Describe("claude-opus-5")
	meter.SetCatalog(opus, "anthropic")
	env := &Env{
		Theme: theme.Blackwall(), Control: ctl, Meter: meter, Work: harness.NewWorkspace(dir), Facts: fs, Missions: ms,
		Approver: harness.NewApprover(s.send), MCP: harness.NewMCP(s.send, reg), Shell: sh, Tools: reg, Rules: pol,
		Mode: mode.Compose(mode.Default, pol, reg), Store: &session.Memory{},
		Session:  func() *session.Session { return sess },
		Profiles: func() []profiles.Profile { return profiles.Defaults() },
		Primary: func() harness.Endpoint {
			p, _ := profiles.Lookup("anthropic")
			return harness.Endpoint{Profile: p, Model: "claude-opus-5"}
		},
		Models: func(ctx context.Context, p profiles.Profile) ([]profiles.Model, error) {
			return []profiles.Model{{ID: "claude-opus-5"}, {ID: "other"}}, nil
		},
		Knobs:     func() map[string]string { return map[string]string{"max_tokens": "16000"} },
		ConfigDir: dir, ProjectDir: dir,
		Now: func() time.Time { return at },
	}
	return env, s
}

func TestComposerTypesSubmitsAndRemembers(t *testing.T) {
	env, _ := newEnv(t)
	dir := t.TempDir()
	c := NewComposer(env, dir)
	c.Busy = func() bool { return false }
	c.Model = func() string { return "claude-opus-5" }
	c.ModeName = func() string { return "default" }
	if c.Title() != "UPLINK" || !c.Pinned() || c.Capturing() || c.Key() != "uplink" {
		t.Fatal("identity")
	}
	c.Update(deck.FocusMsg{Focused: true})
	if !c.Capturing() || len(c.Hints()) != 4 {
		t.Fatal("focus arms the composer")
	}
	typeText(c, "hello world")
	if c.Value() != "hello world" {
		t.Fatalf("%q", c.Value())
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "draft.txt")); string(data) != "hello world" {
		t.Fatalf("draft must persist: %q", data)
	}
	_, cmd := c.Update(keyp("enter"))
	msg, ok := find[SubmitMsg](run(cmd))
	if !ok || msg.Text != "hello world" || len(msg.Parts) != 1 {
		t.Fatalf("%+v", msg)
	}
	if c.Value() != "" || len(c.History()) != 1 {
		t.Fatal("submit resets and remembers")
	}
	if _, err := os.Stat(filepath.Join(dir, "draft.txt")); err == nil {
		t.Fatal("draft removed after send")
	}
	c.Update(keyp("up"))
	if c.Value() != "hello world" {
		t.Fatalf("history recall %q", c.Value())
	}
	c.Update(keyp("down"))
	if c.Value() != "" {
		t.Fatalf("history forward restores the draft %q", c.Value())
	}
	typeText(c, "/mo")
	c.Update(keyp("tab"))
	if c.comp == nil || len(c.comp.items) < 2 {
		t.Fatalf("slash completion %+v", c.comp)
	}
	c.Update(keyp("enter"))
	if !strings.HasPrefix(c.Value(), "/mod") {
		t.Fatalf("%q", c.Value())
	}
	typeText(c, "x")
	_, cmd = c.Update(keyp("enter"))
	cm, ok := find[CommandMsg](run(cmd))
	if !ok || !strings.HasPrefix(cm.Name, "mod") || cm.Args != "x" {
		t.Fatalf("%+v", cm)
	}
	os.WriteFile(filepath.Join(dir, "pic.png"), []byte("png"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	typeText(c, "/attach "+filepath.Join(dir, "pic.png"))
	_, cmd = c.Update(keyp("enter"))
	run(cmd)
	if len(c.Attachments()) != 1 {
		t.Fatal("attachment")
	}
	typeText(c, "look")
	_, cmd = c.Update(keyp("enter"))
	msg, _ = find[SubmitMsg](run(cmd))
	if len(msg.Parts) != 2 {
		t.Fatalf("image part must precede the text: %+v", msg.Parts)
	}
	if _, ok := msg.Parts[0].(protocol.Image); !ok {
		t.Fatal("image part")
	}
	typeText(c, "/attach nope.txt")
	_, cmd = c.Update(keyp("enter"))
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("bad attachment must toast")
	}
	typeText(c, "/attach")
	_, cmd = c.Update(keyp("enter"))
	run(cmd)
	env.Shell.SetDir(dir)
	typeText(c, "see su")
	c.Update(keyp("tab"))
	if !strings.HasSuffix(c.Value(), "sub/") {
		t.Fatalf("path completion %q", c.Value())
	}
	c.Update(keyp("esc"))
	if c.Capturing() {
		t.Fatal("esc releases")
	}
	c.Update(keyp("i"))
	if !c.Capturing() {
		t.Fatal("i arms again")
	}
	c.Update(tea.PasteMsg{Content: "pasted"})
	if !strings.Contains(c.Value(), "pasted") {
		t.Fatalf("%q", c.Value())
	}
	c.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	if c.Value() != "" {
		t.Fatal("ctrl+u clears")
	}
	c.Busy = func() bool { return true }
	if c.Status() != "steering" || !strings.Contains(ansi.Strip(c.View(40, 5)), "steer") {
		t.Fatalf("%q", c.Status())
	}
	if c.Cursor() == nil {
		t.Fatal("cursor while armed")
	}
	c.Update(deck.FocusMsg{})
	if c.Capturing() || c.Cursor() != nil {
		t.Fatal("blur releases")
	}
	again := NewComposer(env, dir)
	if len(again.History()) != 6 {
		t.Fatalf("history must persist, got %d", len(again.History()))
	}
	again.Update(deck.FocusMsg{Focused: true})
	again.Update(tea.MouseClickMsg{Button: tea.MouseLeft})
	again.Update(keyp("esc"))
	again.Update(tea.MouseClickMsg{Button: tea.MouseLeft})
	if !again.Capturing() {
		t.Fatal("click arms")
	}
	typeText(again, "a")
	again.Update(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
	if _, cmd := again.Update(keyp("enter")); cmd == nil {
		t.Fatal("submit")
	}
	if _, cmd := again.Update(keyp("enter")); cmd != nil {
		t.Fatal("empty submit does nothing")
	}
	again.SetCommands([]Command{{Name: "zzz", Desc: "d"}})
	typeText(again, "/z")
	again.Update(keyp("tab"))
	if !strings.HasPrefix(again.Value(), "/zzz") {
		t.Fatalf("%q", again.Value())
	}
}

func event(typ agent.EventType, round int) agent.Event {
	return agent.Event{Type: typ, Agent: "root", Round: round, Time: at}
}

func model(round, index int, typ protocol.EventType, parts ...protocol.Part) agent.Event {
	ev := event(agent.EventModel, round)
	ev.Model = protocol.Event{Type: typ, Index: index, Response: protocol.Response{Model: "m", Content: parts}}
	return ev
}

func TestTranscriptStreamsToolsAndSearches(t *testing.T) {
	env, _ := newEnv(t)
	tr := NewTranscript(env)
	if tr.Title() != "TRACE" || !tr.Selectable() || len(tr.Claims()) < 10 || len(tr.Hints()) != 6 {
		t.Fatal("identity")
	}
	feed := func(ev agent.Event) { tr.Update(harness.EventMsg{Run: "run-1", Event: ev}) }
	start := event(agent.EventRunStart, 0)
	start.Parts = []protocol.Part{protocol.Text{Text: "scan the host"}, protocol.Image{MediaType: "image/png", Data: []byte("x")}}
	feed(start)
	feed(event(agent.EventTurnStart, 1))
	feed(model(1, 0, protocol.EventThinkingDelta, protocol.Thinking{Text: "hmm"}))
	feed(model(1, 1, protocol.EventTextDelta, protocol.Thinking{Text: "hmm"}, protocol.Text{Text: "I will **read** it"}))
	call := protocol.ToolCall{ID: "c1", Name: "Read", Args: json.RawMessage(`{"file_path":"/tmp/x.go"}`)}
	feed(model(1, 2, protocol.EventToolCallStart, protocol.Thinking{Text: "hmm"}, protocol.Text{Text: "I will **read** it"}, protocol.ToolCall{ID: "c1", Name: "Read"}))
	delta := model(1, 2, protocol.EventToolCallDelta, protocol.Thinking{Text: "hmm"}, protocol.Text{Text: "I will **read** it"}, protocol.ToolCall{ID: "c1", Name: "Read"})
	delta.Model.Partial = `{"file_p`
	feed(delta)
	feed(model(1, 2, protocol.EventToolCallEnd, protocol.Thinking{Text: "hmm"}, protocol.Text{Text: "I will **read** it"}, call))
	end := event(agent.EventTurnEnd, 1)
	end.Response = &protocol.Response{Model: "claude-opus-5"}
	feed(end)
	ts := event(agent.EventToolStart, 1)
	ts.Call = call
	feed(ts)
	view := ansi.Strip(tr.View(60, 20))
	for _, want := range []string{"you", "scan the host", "image", "thinking", "root", "read", "Read", "/tmp/x.go"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view lacks %q:\n%s", want, view)
		}
	}
	if !strings.Contains(tr.Status(), "1 tools") {
		t.Fatalf("%q", tr.Status())
	}
	te := event(agent.EventToolEnd, 1)
	te.Call = call
	te.Result = protocol.ToolResult{CallID: "c1", Name: "Read", Content: []protocol.Part{protocol.Text{Text: "1 package main"}}}
	te.Data = files.ReadData{Path: "/tmp/x.go", Offset: 1, Lines: []string{"package main", "func main() {}"}, Total: 2}
	te.Duration = 1500 * time.Millisecond
	feed(te)
	view = ansi.Strip(tr.View(60, 20))
	if !strings.Contains(view, "ok") || !strings.Contains(view, "1.5s") || !strings.Contains(view, "2 of 2 lines") {
		t.Fatalf("%s", view)
	}
	tr.Update(keyp("."))
	for i := 0; i < 5; i++ {
		tr.Update(keyp("."))
	}
	tr.Update(keyp("o"))
	view = ansi.Strip(tr.View(60, 30))
	if !strings.Contains(view, "func main") {
		t.Fatalf("expanded read must show numbered lines:\n%s", view)
	}
	edit := protocol.ToolCall{ID: "c2", Name: "Edit", Args: json.RawMessage(`{"file_path":"/tmp/x.go","old_string":"a","new_string":"b"}`)}
	ts.Call = edit
	feed(ts)
	te.Call = edit
	te.Result = protocol.ToolResult{CallID: "c2", Name: "Edit"}
	te.Data = files.Change{Path: "/tmp/x.go", Old: "a\nb\n", New: "a\nc\n", Replaced: 1}
	feed(te)
	tr.Update(keyp("e"))
	view = ansi.Strip(tr.View(60, 40))
	if !strings.Contains(view, "+1 -1") || !strings.Contains(view, "- b") || !strings.Contains(view, "+ c") {
		t.Fatalf("diff:\n%s", view)
	}
	pre := &harness.PreImage{}
	env.PreImage = pre
	target := filepath.Join(t.TempDir(), "pre.txt")
	os.WriteFile(target, []byte("line one\nline two\n"), 0o644)
	failing := protocol.ToolCall{ID: "c9", Name: "Edit", Args: json.RawMessage(`{"file_path":"` + target + `","old_string":"zzz","new_string":"y"}`)}
	pre.BeforeTool(context.Background(), failing)
	ts.Call = failing
	feed(ts)
	te.Call = failing
	te.Result = protocol.ToolResult{CallID: "c9", Name: "Edit", Content: []protocol.Part{protocol.Text{Text: "old_string not found"}}, IsError: true}
	te.Data = nil
	feed(te)
	tr.Update(keyp("e"))
	view = ansi.Strip(tr.View(60, 60))
	if !strings.Contains(view, "file before the call") || !strings.Contains(view, "line two") {
		t.Fatalf("failed edits must show the pre-image:\n%s", view)
	}
	missing := protocol.ToolCall{ID: "c10", Name: "Write", Args: json.RawMessage(`{"file_path":"/nope/none.txt","content":"x"}`)}
	pre.BeforeTool(context.Background(), missing)
	ts.Call = missing
	feed(ts)
	te.Call = missing
	te.Result = protocol.ToolResult{CallID: "c10", Name: "Write", Content: []protocol.Part{protocol.Text{Text: "permission denied"}}, IsError: true}
	feed(te)
	tr.Update(keyp("e"))
	if !strings.Contains(ansi.Strip(tr.View(60, 80)), "did not exist") {
		t.Fatal("missing pre-image must be labeled")
	}
	bash := protocol.ToolCall{ID: "c3", Name: "Bash", Args: json.RawMessage(`{"command":"ls -la","description":"list"}`)}
	ts.Call = bash
	feed(ts)
	tr.Update(harness.ProgressMsg{Run: "run-1", Progress: tool.Progress{CallID: "c3", Stream: "stdout", Text: "partial out\n"}})
	view = ansi.Strip(tr.View(60, 40))
	if !strings.Contains(view, "partial out") {
		t.Fatalf("streamed output:\n%s", view)
	}
	te.Call = bash
	te.Result = protocol.ToolResult{CallID: "c3", Name: "Bash", Content: []protocol.Part{protocol.Text{Text: "x"}}, IsError: true}
	te.Data = shell.Run{Command: "ls -la", ExitCode: 2, Stdout: "out", Stderr: "boom"}
	feed(te)
	refused := event(agent.EventToolEnd, 1)
	refused.Call = protocol.ToolCall{ID: "c4", Name: "Bash", Args: json.RawMessage(`{"command":"rm -rf /"}`)}
	refused.Err = errors.New("policy: Bash denied, denied by rule")
	refused.Result = protocol.ToolResult{CallID: "c4", IsError: true}
	feed(refused)
	ap := event(agent.EventApproval, 1)
	ap.Approval = &agent.Approval{Request: policy.Request{Call: bash, Key: "Bash(ls *)"}, Pending: true}
	feed(ap)
	ap.Approval = &agent.Approval{Request: policy.Request{Call: bash, Key: "Bash(ls *)"}, Answer: policy.Answer{Allowed: true, By: "person", Verdict: policy.Always}}
	feed(ap)
	steer := event(agent.EventSteer, 1)
	steer.Parts = []protocol.Part{protocol.Text{Text: "also check ports"}}
	feed(steer)
	feed(event(agent.EventCompactStart, 2))
	ce := event(agent.EventCompactEnd, 2)
	ce.Text = "summary"
	ce.Data = map[string]int{"before": 10, "after": 4}
	feed(ce)
	hk := event(agent.EventHook, 2)
	hk.Text = "Stop \"echo\" ok"
	feed(hk)
	note := event(agent.EventNote, 2)
	note.Text = "a note"
	feed(note)
	cs := event(agent.EventSubagentStart, 2)
	cs.Call = protocol.ToolCall{ID: "c5", Name: "scout-1"}
	cs.Text = "look around"
	cs.Data = subagent.Child{Name: "scout-1"}
	feed(cs)
	cend := event(agent.EventSubagentEnd, 2)
	cend.Call = cs.Call
	cend.Response = &protocol.Response{Content: []protocol.Part{protocol.Text{Text: "found it"}}}
	feed(cend)
	feed(event(agent.EventInterrupt, 2))
	re := event(agent.EventRunEnd, 2)
	re.Err = errors.New("agent: interrupted")
	feed(re)
	tr.Update(keyp("e"))
	view = ansi.Strip(tr.View(70, 80))
	for _, want := range []string{"error", "exit 2", "boom", "refused", "denied by rule", "allowed Bash", "always", "Bash(ls *)", "steer", "also check ports", "compacted 10 messages into 4", "Stop", "a note", "scout-1", "found it", "interrupted", "agent: interrupted"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view lacks %q:\n%s", want, view)
		}
	}
	tr.Update(keyp("/"))
	if !tr.Capturing() {
		t.Fatal("search captures")
	}
	typeText(tr, "boom")
	tr.Update(keyp("enter"))
	tr.View(70, 20)
	if tr.matches != 1 || !strings.Contains(tr.Status(), "1 matches") {
		t.Fatalf("matches %d status %q", tr.matches, tr.Status())
	}
	tr.Update(keyp("n"))
	tr.Update(keyp("N"))
	tr.Update(keyp("/"))
	typeText(tr, "zz")
	tr.Update(keyp("backspace"))
	tr.Update(keyp("esc"))
	if tr.query != "" {
		t.Fatal("esc clears the search")
	}
	for _, k := range []string{"j", "k", "g", "G", "[", "]", ",", "E", "T", "f", "f", "ctrl+d", "ctrl+u"} {
		tr.Update(keyp(k))
	}
	tr.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	tr.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	tr.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 1, Y: 0})
	tr.Update(deck.FocusMsg{})
	tr.Update(transcriptTick{})
	if tr.Blocks() < 15 {
		t.Fatalf("%d blocks", tr.Blocks())
	}
	child := NewChildTranscript(env, "run-1", "scout-1")
	child.Update(harness.EventMsg{Run: "run-1", Event: event(agent.EventRunStart, 0)})
	if child.Blocks() != 0 {
		t.Fatal("child ignores root events")
	}
	ev := event(agent.EventRunStart, 0)
	ev.Agent, ev.Depth = "scout-1", 1
	child.Update(harness.EventMsg{Run: "run-1", Event: ev})
	child.Update(harness.EventMsg{Run: "run-2", Event: ev})
	if child.Blocks() != 1 || child.Title() != "TRACE scout-1" {
		t.Fatal("child follows its agent in its run")
	}
	tr.Update(harness.EventMsg{Run: "run-1", Event: ev})
	if tr.Blocks() != 15+0 && tr.Blocks() < 15 {
		t.Fatal("root ignores child depth events")
	}
}

func TestTranscriptLoadsSessions(t *testing.T) {
	env, _ := newEnv(t)
	sess := session.New()
	sess.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}})
	sess.AddResponse("root", protocol.Response{Model: "m", Content: []protocol.Part{protocol.Thinking{Redacted: true}, protocol.Text{Text: "calling"}, protocol.ToolCall{ID: "c1", Name: "Glob", Args: json.RawMessage(`{"pattern":"*.go"}`)}}})
	sess.Log("root", "approval", map[string]any{"tool": "Glob", "call_id": "c1", "allowed": true, "by": "person", "verdict": "yes", "key": "Glob"})
	sess.AddResults("root", []protocol.ToolResult{{CallID: "c1", Name: "Glob", Content: []protocol.Part{protocol.Text{Text: "a.go"}}}, {CallID: "orphan", Name: "Read", Content: []protocol.Part{protocol.Image{MediaType: "image/png"}}}})
	sess.Log("root", "compact", map[string]any{"before": 4, "after": 2, "summary": "s"})
	sess.Compact("Summary of the earlier conversation:\n\nstuff", 1)
	sess.Log("root", "interrupt", nil)
	sess.Log("root", "run_end", map[string]any{"error": "boom"})
	sess.Log("root", facts.RecordKind, facts.Change{Op: facts.OpAdd, Fact: facts.Fact{ID: "f1", Kind: "finding", Title: "open port"}})
	sess.Log("root", "approval", json.RawMessage(`{bad`))
	tr := NewTranscript(env)
	tr.Load(sess.Entries(), sess.Records())
	tr.Update(keyp("e"))
	view := ansi.Strip(tr.View(60, 60))
	for _, want := range []string{"compaction summary", "allowed Glob", "(yes)", "compacted 4 messages into 2", "interrupted", "boom", "open port"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, session.Ack) {
		t.Fatal("ack must be hidden")
	}
	if tr.turns != 0 {
		t.Fatalf("compaction removed the user turn, turns %d", tr.turns)
	}
	lines := tr.Lines(40)
	if len(lines) == 0 || len(tr.lineBlock) != len(lines) {
		t.Fatal("lines map to blocks")
	}
	if callSummary(protocol.ToolCall{Name: "Read", Args: json.RawMessage(`{"file_path":"f","offset":3}`)}, "") != "f from 3" {
		t.Fatal("read summary")
	}
	if callSummary(protocol.ToolCall{Name: "Grep", Args: json.RawMessage(`{"pattern":"p","path":"d"}`)}, "") != "p in d" {
		t.Fatal("grep summary")
	}
	if callSummary(protocol.ToolCall{Name: "Agent", Args: json.RawMessage(`{"prompt":"do things"}`)}, "") != "do things" {
		t.Fatal("agent summary")
	}
	if callSummary(protocol.ToolCall{Name: "Plan", Args: json.RawMessage(`{"steps":[{},{}]}`)}, "") != "2 steps" {
		t.Fatal("plan summary")
	}
	if callSummary(protocol.ToolCall{Name: "X", Args: json.RawMessage(`{"b":"2","a":"1"}`)}, "") != "1 2" {
		t.Fatal("generic summary")
	}
	if callSummary(protocol.ToolCall{Name: "X"}, "{part") != "{part" || callSummary(protocol.ToolCall{Name: "X", Args: json.RawMessage(`bad`)}, "") != "" {
		t.Fatal("partial summary")
	}
	if callSummary(protocol.ToolCall{Name: "WebFetch", Args: json.RawMessage(`{"url":"u"}`)}, "") != "u" || callSummary(protocol.ToolCall{Name: "Record", Args: json.RawMessage(`{"title":"t"}`)}, "") != "t" {
		t.Fatal("other summaries")
	}
}

func TestApprovalCard(t *testing.T) {
	env, s := newEnv(t)
	a := NewApproval(env)
	if a.Title() != "APPROVAL" || a.Capturing() || a.Status() != "" || len(a.Hints()) != 5 {
		t.Fatal("empty")
	}
	if !strings.Contains(ansi.Strip(a.View(40, 10)), "nothing waiting") {
		t.Fatal("empty view")
	}
	ctx := context.Background()
	req := policy.Request{Call: protocol.ToolCall{ID: "c1", Name: "Bash", Args: json.RawMessage(`{"command":"git status"}`)}, Agent: "root", Reason: "rule 1", Key: "Bash(git *)", Subjects: []policy.Subject{{Kind: "command", Text: "git status"}, {Kind: "command", Text: "$(x)", Unsafe: true, Exact: true}}, Tool: &protocol.Tool{Name: "Bash", Description: "runs commands"}, Hints: tool.Annotations{Destructive: true, OpenWorld: true, ReadOnly: true, Idempotent: true}, Review: &policy.Review{By: "judge", Decision: policy.Ask, Reason: "unsure"}}
	go env.Approver.Ask(ctx, req)
	deadline := time.Now().Add(2 * time.Second)
	for env.Approver.Len() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	go env.Approver.Ask(ctx, policy.Request{Call: protocol.ToolCall{ID: "c2", Name: "Read"}})
	for env.Approver.Len() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	a.Update(harness.ApprovalMsg{})
	if !a.Capturing() || a.Status() != "2 waiting" || !strings.Contains(a.Title(), "Bash") {
		t.Fatalf("%q", a.Status())
	}
	view := ansi.Strip(a.View(100, 30))
	for _, want := range []string{"Bash", "rule 1", "judge", "git status", "unsafe", "exact", "Bash(git *)", "runs commands", "destructive", "queued", "Read"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view lacks %q:\n%s", want, view)
		}
	}
	a.Update(keyp("j"))
	a.Update(keyp("k"))
	a.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	a.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	a.Update(keyp("g"))
	a.View(60, 5)
	_, cmd := a.Update(keyp("a"))
	ans, ok := find[AnswerMsg](run(cmd))
	if !ok || ans.Verdict != policy.Always || ans.ID != a.Pending()[0].ID {
		t.Fatalf("%+v", ans)
	}
	if !env.Approver.Answer(ans.ID, ans.Verdict) {
		t.Fatalf("answer %d refused, pending %v", ans.ID, env.Approver.Pending())
	}
	deadline = time.Now().Add(2 * time.Second)
	for env.Approver.Len() > 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	a.Update(harness.ApprovalDoneMsg{})
	if a.Status() != "1 waiting" {
		t.Fatalf("%q", a.Status())
	}
	for _, k := range []string{"y", "n", "v"} {
		_, cmd := a.Update(keyp(k))
		if _, ok := find[AnswerMsg](run(cmd)); !ok {
			t.Fatalf("%s must answer", k)
		}
	}
	env.Approver.DenyAll()
	for env.Approver.Len() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	_, cmd = a.Update(harness.ApprovalDoneMsg{})
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("an empty queue dismisses the overlay")
	}
	_ = s
	if prettyArgs(json.RawMessage(`bad`)) != "bad" {
		t.Fatal("pretty args")
	}
}

type scriptModel struct{ parts []protocol.Part }

func (s scriptModel) Stream(ctx context.Context, req protocol.Request) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		resp := protocol.Response{Content: s.parts, StopReason: protocol.StopEnd, Usage: protocol.Usage{Input: 3, Output: 4}}
		yield(protocol.Event{Type: protocol.EventDone, Response: resp}, nil)
	}
}

func TestDaemonsCard(t *testing.T) {
	env, s := newEnv(t)
	d := NewDaemons(env)
	if d.Title() != "DAEMONS" || !strings.Contains(ansi.Strip(d.View(40, 5)), "no daemons") || d.Status() != "idle" {
		t.Fatal("empty")
	}
	a := &agent.Agent{Name: "root", Model: scriptModel{parts: []protocol.Part{protocol.Text{Text: "done"}}}, ModelID: "m", Gate: policy.Gate{Policy: policy.AllowAll}, Session: session.New()}
	r, err := env.Control.Start(context.Background(), a, "", []protocol.Part{protocol.Text{Text: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	dc, _ := env.Control.Done(r.ID)
	<-dc
	d.Update(harness.RunDoneMsg{Run: r.ID})
	for _, m := range s.all() {
		if ev, ok := m.(harness.EventMsg); ok {
			d.Update(ev)
		}
	}
	env.Shell.Bash().Run(context.Background(), json.RawMessage(`{"command":"echo hi","run_in_background":true}`))
	d.Update(harness.JobMsg{})
	d.Update(daemonTick{})
	view := ansi.Strip(d.View(80, 10))
	if !strings.Contains(view, "root") || !strings.Contains(view, "done") || !strings.Contains(view, "job-1") {
		t.Fatalf("%s", view)
	}
	d.Update(keyp("g"))
	_, cmd := d.Update(keyp("enter"))
	if open, ok := find[OpenRunMsg](run(cmd)); !ok || open.Run != r.ID {
		t.Fatal("enter opens the run")
	}
	_, cmd = d.Update(keyp("K"))
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("killing a finished run toasts")
	}
	d.Update(keyp("G"))
	d.Update(keyp("k"))
	d.Update(keyp("j"))
	_, cmd = d.Update(keyp("enter"))
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("opening a job toasts")
	}
	wait, _ := env.Shell.Wait("job-1")
	<-wait
	d.Update(harness.JobMsg{})
	_, cmd = d.Update(keyp("K"))
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("killing a finished job toasts")
	}
	d.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 1})
	if d.sel != 0 {
		t.Fatal("click selects")
	}
	if d.Init() != nil {
		t.Fatal("no tick when idle")
	}
	env.Shell.Bash().Run(context.Background(), json.RawMessage(`{"command":"sleep 5","run_in_background":true}`))
	d.Update(harness.JobMsg{})
	if d.Status() != "1 run" || d.Init() == nil {
		t.Fatalf("%q", d.Status())
	}
	d.Update(keyp("G"))
	_, cmd = d.Update(keyp("K"))
	if kill, ok := find[KillJobMsg](run(cmd)); !ok || kill.Job != "job-2" {
		t.Fatal("kill job")
	}
	env.Shell.KillJob(context.Background(), "job-2")
}

func TestMissionFindingsTelemetryWorkspaceJobs(t *testing.T) {
	env, _ := newEnv(t)
	m, _ := env.Missions.Create("map the lan")
	env.Missions.SetPlan(m.ID, []mission.Step{{Text: "scan", Status: mission.StepDone}, {Text: "probe", Status: mission.StepActive}, {Text: "report"}, {Text: "skip", Status: mission.StepSkipped}})
	env.Missions.Assign(m.ID, "root")
	m, _ = env.Missions.Get(m.ID)
	mc := NewMission(env, m)
	env.Facts.Add(facts.Fact{Title: "open ssh", Kind: "finding", Agent: "root", Confidence: 0.9, Tags: []string{"net"}, Evidence: []facts.Evidence{{Kind: "file", Ref: "/tmp/a", Line: 3, Quote: "22"}, {Kind: "url", Ref: "http://x"}}, Body: "port 22 answers"})
	env.Facts.Add(facts.Fact{Title: "weak tls", Kind: "risk", Agent: "other", Parent: "f1", Confidence: 0.3})
	view := ansi.Strip(mc.View(60, 12))
	for _, want := range []string{"map the lan", "active", "50%", "root", "findings 1", "☑ 1. scan", "▶ 2. probe", "☐ 3. report", "⊘ 4. skip"} {
		if !strings.Contains(view, want) {
			t.Fatalf("mission view lacks %q:\n%s", want, view)
		}
	}
	if mc.Status() != "50%" || mc.Key() != "mission:"+m.ID || mc.ID() != m.ID || len(mc.Hints()) != 2 {
		t.Fatalf("%q", mc.Status())
	}
	_, cmd := mc.Update(keyp("d"))
	if st, ok := find[MissionStateMsg](run(cmd)); !ok || st.Status != mission.StatusDone {
		t.Fatal("d finishes")
	}
	done, _ := env.Missions.Finish(m.ID, mission.StatusFailed, "timeout")
	mc.Update(harness.MissionMsg{Mission: done})
	if mc.Status() != "failed" || !strings.Contains(ansi.Strip(mc.View(60, 12)), "timeout") || len(mc.Hints()) != 2 {
		t.Fatal("failed view")
	}
	_, cmd = mc.Update(keyp("D"))
	if rm, ok := find[MissionRemoveMsg](run(cmd)); !ok || rm.ID != m.ID {
		t.Fatal("D forgets an ended mission")
	}
	_, cmd = mc.Update(keyp("r"))
	if _, ok := find[MissionStateMsg](run(cmd)); !ok {
		t.Fatal("r resumes")
	}
	mc.Update(keyp("x"))
	empty := NewMission(env, mission.Mission{ID: "m9", Objective: "o", Status: mission.StatusActive})
	if !strings.Contains(ansi.Strip(empty.View(40, 8)), "no plan yet") || empty.findings() != 2 {
		t.Fatal("empty plan")
	}

	f := NewFindings(env)
	view = ansi.Strip(f.View(70, 20))
	if !strings.Contains(view, "open ssh") || !strings.Contains(view, "weak tls") || f.Status() != "2 open" || len(f.Rows()) != 2 {
		t.Fatalf("%s", view)
	}
	f.Update(keyp("t"))
	if !f.tree || f.depth["f2"] != 1 {
		t.Fatal("tree depth")
	}
	f.Update(keyp("enter"))
	view = ansi.Strip(f.View(70, 20))
	if !strings.Contains(view, "port 22 answers") || !strings.Contains(view, "/tmp/a:3") || !strings.Contains(view, "net") {
		t.Fatalf("%s", view)
	}
	f.Update(keyp("c"))
	if f.kind != "finding" || len(f.Rows()) != 1 {
		t.Fatalf("kind filter %q %d", f.kind, len(f.Rows()))
	}
	for i := 0; i < len(f.env.Facts.Kinds()); i++ {
		f.Update(keyp("c"))
	}
	if f.kind != "" {
		t.Fatalf("kind cycle must return to every kind, got %q", f.kind)
	}
	f.Update(keyp("/"))
	typeText(f, "tls")
	f.Update(keyp("backspace"))
	typeText(f, "s")
	f.Update(keyp("enter"))
	if len(f.Rows()) != 1 || f.Rows()[0].ID != "f2" {
		t.Fatalf("text filter %+v", f.Rows())
	}
	f.Update(keyp("/"))
	f.Update(keyp("esc"))
	f.Update(keyp("s"))
	if f.status != facts.StatusOpen || len(f.Rows()) != 2 {
		t.Fatal("status filter")
	}
	f.Update(keyp("s"))
	f.Update(keyp("s"))
	f.Update(keyp("s"))
	f.Update(keyp("j"))
	_, cmd = f.Update(keyp("x"))
	run(cmd)
	if got, _ := env.Facts.Get("f2"); got.Status != facts.StatusResolved {
		t.Fatal("x resolves")
	}
	f.Update(keyp("x"))
	f.Update(keyp("k"))
	f.Update(keyp("G"))
	f.Update(keyp("g"))
	f.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 2})
	_, cmd = f.Update(harness.FactMsg{Change: facts.Change{Op: facts.OpAdd}})
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("new facts ask for attention")
	}
	f.ExportDir = t.TempDir()
	_, cmd = f.Update(keyp("e"))
	ex, ok := find[ExportMsg](run(cmd))
	if !ok || ex.Err != nil {
		t.Fatalf("%+v", ex)
	}
	if data, _ := os.ReadFile(ex.Path); !strings.Contains(string(data), "open ssh") {
		t.Fatal("export content")
	}
	noFacts := NewFindings(&Env{Theme: theme.Blackwall()})
	if noFacts.Status() != "" || !strings.Contains(ansi.Strip(noFacts.View(40, 5)), "nothing recorded") || noFacts.resolve() != nil || noFacts.export(false) != nil {
		t.Fatal("no store")
	}

	ice := NewTelemetry(env)
	if ice.Title() != "ICE" || ice.Status() != "0" {
		t.Fatalf("%q", ice.Status())
	}
	env.Meter.SetThreshold(150000)
	env.Meter.Observe(provider.Attempt{Status: 429, Err: errors.New("slow"), Wait: time.Second, Duration: time.Millisecond})
	env.Meter.Observe(provider.Attempt{Status: 500, Err: errors.New("down"), Duration: time.Millisecond})
	env.Meter.Observe(provider.Attempt{Status: 200, Duration: time.Millisecond, RateLimit: provider.RateLimit{Requests: 10, RequestsRemaining: 5, Tokens: 100, TokensRemaining: 50, InputTokens: 7, InputRemaining: 1, OutputTokens: 9, OutputRemaining: 2, Reset: time.Minute}})
	for _, a := range env.Meter.Attempts() {
		ice.Update(harness.AttemptMsg{Attempt: a, Telemetry: env.Meter.Snapshot()})
	}
	env.Meter.Failover(0, errors.New("x"))
	ice.Update(harness.FailoverMsg{From: 0, Err: errors.New("x")})
	env.Meter.Served(1)
	ice.Update(harness.ServedMsg{Index: 1})
	env.Meter.Apply("run-1", agent.Event{Type: agent.EventTurnStart, Time: at})
	ice.Update(harness.EventMsg{Event: agent.Event{Type: agent.EventTurnStart}})
	env.Meter.Apply("run-1", agent.Event{Type: agent.EventModel, Time: at.Add(time.Second), Model: protocol.Event{Type: protocol.EventTextDelta, Delta: "12345678"}})
	ice.Update(telemetryTick{})
	env.Meter.Apply("run-1", agent.Event{Type: agent.EventTurnEnd, Time: at.Add(2 * time.Second), Usage: protocol.Usage{Input: 100, Output: 50, CacheRead: 100}})
	ice.Update(harness.EventMsg{Event: agent.Event{Type: agent.EventCompactEnd}})
	ice.Update(telemetryTick{})
	view = ansi.Strip(ice.View(80, 20))
	for _, want := range []string{"claude-opus-5", "anthropic", "fallback 1", "compacts", "tok/s", "cache", "cost", "retries", "failed", "failovers", "limits", "req 5/10", "reset", "retry in", "compacted", "served by fallback"} {
		if !strings.Contains(view, want) {
			t.Fatalf("ice lacks %q:\n%s", want, view)
		}
	}
	if ice.Status() == "0" {
		t.Fatalf("%q", ice.Status())
	}
	ice.View(80, 3)
	plain := NewTelemetry(&Env{Theme: theme.Plain(), Now: func() time.Time { return at }})
	plain.samples = []float64{1, 2}
	plain.View(20, 6)
	none := NewTelemetry(&Env{Theme: theme.Blackwall(), Now: func() time.Time { return at }})
	if !strings.Contains(ansi.Strip(none.View(40, 6)), "no model") {
		t.Fatal("no model")
	}

	ws := NewWorkspace(env)
	if !strings.Contains(ansi.Strip(ws.View(60, 10)), "no files touched") || ws.Status() != "0 files" {
		t.Fatal("empty workspace")
	}
	env.Work.Apply(agent.Event{Type: agent.EventToolEnd, Agent: "root", Time: at, Call: protocol.ToolCall{ID: "w1", Name: "Write"}, Data: files.Change{Path: "/p/a.txt", Old: "", New: "one\n", Created: true}})
	env.Work.Apply(agent.Event{Type: agent.EventToolEnd, Agent: "root", Time: at.Add(time.Second), Call: protocol.ToolCall{ID: "e1", Name: "Edit"}, Data: files.Change{Path: "/p/a.txt", Old: "one\n", New: "two\n", Replaced: 1}})
	env.Work.Apply(agent.Event{Type: agent.EventToolEnd, Agent: "root", Time: at, Data: files.ReadData{Path: "/p/b.txt"}})
	ws.Update(harness.EventMsg{Event: agent.Event{Type: agent.EventToolEnd}})
	ws.Update(keyp("enter"))
	view = ansi.Strip(ws.View(60, 20))
	if !strings.Contains(view, "/p/a.txt") || !strings.Contains(view, "- one") || !strings.Contains(view, "+ two") || ws.Status() != "1 changed" {
		t.Fatalf("%s", view)
	}
	ws.Update(keyp("a"))
	view = ansi.Strip(ws.View(60, 20))
	if !strings.Contains(view, "since the session started") || strings.Contains(view, "- one") {
		t.Fatalf("whole diff:\n%s", view)
	}
	ws.Update(keyp("j"))
	ws.Update(keyp("enter"))
	ws.Update(keyp("enter"))
	if !strings.Contains(ansi.Strip(ws.View(60, 20)), "read only") {
		t.Fatal("read only detail")
	}
	for _, k := range []string{"k", "g", "G", "J", "K"} {
		ws.Update(keyp(k))
	}
	ws.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 2})
	ws.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	ws.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if len(ws.Files()) != 2 {
		t.Fatal("files")
	}

	jobs := NewJobs(env)
	if !strings.Contains(ansi.Strip(jobs.View(40, 5)), "no background jobs") || jobs.Status() != "" {
		t.Fatal("empty jobs")
	}
	env.Shell.Bash().Run(context.Background(), json.RawMessage(`{"command":"echo tail-text; echo more","run_in_background":true}`))
	wait, _ := env.Shell.Wait("job-1")
	<-wait
	_, cmd = jobs.Update(harness.JobMsg{Event: shell.JobEvent{Kind: shell.JobStarted, Job: shell.JobInfo{ID: "job-1"}}})
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("new job asks for attention")
	}
	jobs.Update(harness.JobMsg{Event: shell.JobEvent{Kind: shell.JobFinished}})
	view = ansi.Strip(jobs.View(60, 12))
	if !strings.Contains(view, "job-1") || !strings.Contains(view, "tail-text") || !strings.Contains(view, "done") {
		t.Fatalf("%s", view)
	}
	for _, k := range []string{"j", "k", "g", "G", "J", "f", "f"} {
		jobs.Update(keyp(k))
	}
	_, cmd = jobs.Update(keyp("K"))
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("killing a finished job toasts")
	}
	jobs.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	jobs.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 1})
	jobs.View(60, 2)
	if len(jobs.List()) != 1 {
		t.Fatal("list")
	}
}

func TestSessionsPaletteHelpHistory(t *testing.T) {
	env, _ := newEnv(t)
	ctx := context.Background()
	a := session.New()
	a.SetTitle("alpha")
	a.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "x"}}})
	b := a.Fork()
	b.SetTitle("beta")
	env.Store.Save(ctx, a)
	env.Store.Save(ctx, b)
	s := NewSessions(env)
	if len(s.Rows()) != 2 || s.Status() != "2" {
		t.Fatalf("%d", len(s.Rows()))
	}
	view := ansi.Strip(s.View(80, 10))
	if !strings.Contains(view, "alpha") || !strings.Contains(view, "beta") || !strings.Contains(view, "⑂") {
		t.Fatalf("%s", view)
	}
	s.Update(keyp("/"))
	typeText(s, "beta")
	s.Update(keyp("enter"))
	if len(s.Rows()) != 1 || s.Rows()[0].Title != "beta" {
		t.Fatal("search")
	}
	_, cmd := s.Update(keyp("enter"))
	if r, ok := find[ResumeMsg](run(cmd)); !ok || r.Session != b.ID() {
		t.Fatal("resume")
	}
	_, cmd = s.Update(keyp("f"))
	if _, ok := find[ForkMsg](run(cmd)); !ok {
		t.Fatal("fork")
	}
	s.Update(keyp("d"))
	if !s.Capturing() || !strings.Contains(ansi.Strip(s.View(80, 10)), "delete") {
		t.Fatal("delete confirms")
	}
	s.Update(keyp("n"))
	s.Update(keyp("d"))
	_, cmd = s.Update(keyp("y"))
	if d, ok := find[DeleteMsg](run(cmd)); !ok || d.Session != b.ID() {
		t.Fatal("delete")
	}
	s.Update(keyp("/"))
	typeText(s, "zz")
	s.Update(keyp("backspace"))
	s.Update(keyp("esc"))
	for _, k := range []string{"j", "k", "g", "G", "r"} {
		s.Update(keyp(k))
	}
	s.Update(harness.SavedMsg{})
	s.Update(deck.FocusMsg{Focused: true})
	s.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 2})
	s.View(80, 3)
	empty := NewSessions(&Env{Theme: theme.Blackwall(), Store: &session.Memory{}})
	if !strings.Contains(ansi.Strip(empty.View(40, 5)), "nothing stored") {
		t.Fatal("empty")
	}

	p := NewPalette(env, []Action{{Name: "quit", Desc: "leave", Keys: "ctrl+c"}, {Name: "theme light", Desc: "palette"}, {Name: "mode", Desc: "cycle"}})
	if !p.Capturing() || len(p.Matches()) != 3 || p.Cursor() == nil {
		t.Fatal("palette")
	}
	typeText(p, "thl")
	if m := p.Matches(); len(m) != 1 || m[0].Name != "theme light" {
		t.Fatalf("%+v", m)
	}
	p.Update(keyp("backspace"))
	p.Update(keyp("backspace"))
	p.Update(keyp("backspace"))
	p.Update(keyp("down"))
	p.Update(keyp("up"))
	p.Update(keyp("tab"))
	_, cmd = p.Update(keyp("enter"))
	msgs := run(cmd)
	if pm, ok := find[PaletteMsg](msgs); !ok || pm.Command == "" {
		t.Fatal("palette enter")
	}
	if _, ok := find[deck.Intent](msgs); !ok {
		t.Fatal("palette dismisses")
	}
	typeText(p, "zzzz")
	if !strings.Contains(ansi.Strip(p.View(50, 8)), "no matching") {
		t.Fatal("no matches")
	}
	_, cmd = p.Update(keyp("esc"))
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("esc dismisses")
	}
	p.query = ""
	p.filter()
	_, cmd = p.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 2})
	if _, ok := find[PaletteMsg](run(cmd)); !ok {
		t.Fatal("click runs")
	}
	if fuzzy("", "x") != 0 || fuzzy("b", "a") != -1 || fuzzy("ab", "a b") <= fuzzy("ab", "axxxb") {
		t.Fatal("fuzzy")
	}
	p.View(50, 8)
	p.Status()

	h := NewHelp(env, []HelpSection{{Title: "deck", Keys: [][2]string{{"tab", "next"}, {"q", "quit"}, {"?", "help"}}}})
	view = ansi.Strip(h.View(80, 10))
	if !strings.Contains(view, "deck") || !strings.Contains(view, "quit") {
		t.Fatal("help view")
	}
	h.Update(keyp("j"))
	h.Update(keyp("k"))
	h.View(40, 2)
	_, cmd = h.Update(keyp("esc"))
	if _, ok := find[deck.Intent](run(cmd)); !ok || h.Status() != "" || h.Title() != "HELP" {
		t.Fatal("help dismiss")
	}

	sess := session.New()
	sess.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "first"}}})
	sess.AddResponse("root", protocol.Response{Content: []protocol.Part{protocol.ToolCall{ID: "c", Name: "Read"}}})
	sess.Log("root", "approval", map[string]any{"tool": "Read"})
	sess.AddResults("root", []protocol.ToolResult{{CallID: "c"}})
	sess.AddResponse("root", protocol.Response{Content: []protocol.Part{protocol.Text{Text: "answer"}}})
	sess.Log("root", "compact", map[string]any{"before": 1, "after": 1})
	sess.Log("root", "mission", map[string]any{"id": "m1"})
	fork := sess.Fork()
	moments := Moments(fork)
	kinds := []string{}
	for _, m := range moments {
		kinds = append(kinds, m.Kind)
	}
	if strings.Join(kinds, ",") != "fork,user,assistant,approval,assistant,compact,mission" {
		t.Fatalf("%v", kinds)
	}
	if moments[2].Title != "root: 1 tool calls" || moments[4].Title != "root: answer" {
		t.Fatalf("%+v", moments)
	}
	card := NewMomentCard(env, fork, moments[4], 4)
	view = ansi.Strip(card.View(60, 20))
	if !strings.Contains(view, "first") || !strings.Contains(view, "answer") || !strings.Contains(card.Title(), "5 root: answer") || card.Glyph() != "≋" {
		t.Fatalf("%s", view)
	}
	early := NewMomentCard(env, fork, moments[1], 1)
	if strings.Contains(ansi.Strip(early.View(60, 20)), "answer") {
		t.Fatal("earlier moments must stop before later entries")
	}
	_, cmd = card.Update(keyp("enter"))
	if f, ok := find[ForkAtMsg](run(cmd)); !ok || f.Entries != 4 {
		t.Fatalf("%+v", f)
	}
	card.Update(keyp("c"))
	if !card.Marked() || card.Status() != "compare" || !strings.Contains(ansi.Strip(card.View(60, 20)), "marked") {
		t.Fatal("mark")
	}
	card.Update(keyp("j"))
	card.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	card.View(60, 1)
	if card.Key() != "moment:4" || len(card.Hints()) != 3 || len(card.Claims()) != 9 || card.Moment().Kind != "assistant" {
		t.Fatal("identity")
	}
}

func TestConfigCard(t *testing.T) {
	env, _ := newEnv(t)
	env.Hooks = nil
	c := NewConfig(env)
	if c.Key() != "config" || c.Group.Len() != 8 {
		t.Fatal("tabs")
	}
	for _, msg := range run(c.Init()) {
		c.Update(msg)
	}
	for _, card := range c.Group.Cards() {
		card.View(100, 20)
	}
	profilesTab := c.Group.Cards()[0].(*lister)
	view := ansi.Strip(profilesTab.View(120, 20))
	if !strings.Contains(view, "anthropic") || !strings.Contains(view, "api.anthropic.com") || !strings.Contains(view, "missing") && !strings.Contains(view, "set") {
		t.Fatalf("%s", view)
	}
	profilesTab.Update(keyp("o"))
	profilesTab.View(120, 20)
	_, cmd := profilesTab.Update(keyp("enter"))
	if sp, ok := find[SetProfileMsg](run(cmd)); !ok || sp.Profile == "" {
		t.Fatal("profile select")
	}
	c.Update(SetProfileMsg{Profile: "x"})
	_, cmd = c.models.Update(keyp("r"))
	msgs := run(cmd)
	loaded, ok := find[modelsLoadedMsg](msgs)
	if !ok || len(loaded.models) != 2 {
		t.Fatalf("%+v", loaded)
	}
	c.Update(loaded)
	if len(c.ModelList()) != 2 || !strings.Contains(ansi.Strip(c.models.View(100, 10)), "claude-opus-5") {
		t.Fatal("models listed")
	}
	_, cmd = c.models.Update(keyp("enter"))
	if sm, ok := find[SetModelMsg](run(cmd)); !ok || sm.Model != "claude-opus-5" {
		t.Fatal("model select")
	}
	c.Update(modelsLoadedMsg{err: errors.New("offline")})
	if !strings.Contains(ansi.Strip(c.models.View(100, 10)), "offline") {
		t.Fatal("error note")
	}
	if c.FetchModels() == nil {
		t.Fatal("fetch")
	}
	knobs := c.Group.Cards()[2].(*lister)
	knobs.Init()
	if !strings.Contains(ansi.Strip(knobs.View(60, 10)), "16000") {
		t.Fatal("knob values")
	}
	_, cmd = knobs.Update(keyp("m"))
	if sm, ok := find[SetModeMsg](run(cmd)); !ok || sm.Mode != mode.AcceptEdits {
		t.Fatal("mode cycle")
	}
	knobs.Update(keyp("enter"))
	if !knobs.Capturing() || knobs.Cursor() == nil {
		t.Fatal("mode prompt")
	}
	typeText(knobs, "plan")
	_, cmd = knobs.Update(keyp("enter"))
	if sm, ok := find[SetModeMsg](run(cmd)); !ok || sm.Mode != mode.Plan {
		t.Fatal("mode prompt answer")
	}
	knobs.Update(keyp("enter"))
	typeText(knobs, "weird")
	_, cmd = knobs.Update(keyp("enter"))
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("bad mode toasts")
	}
	knobs.Update(keyp("j"))
	knobs.Update(keyp("enter"))
	typeText(knobs, "4096")
	knobs.Update(keyp("backspace"))
	_, cmd = knobs.Update(keyp("enter"))
	if km, ok := find[KnobMsg](run(cmd)); !ok || km.Key != "max_tokens" || km.Value != "409" {
		t.Fatalf("%+v", km)
	}
	knobs.Update(keyp("enter"))
	knobs.Update(keyp("esc"))
	if knobs.Capturing() {
		t.Fatal("esc leaves the prompt")
	}
	rulesTab := c.Group.Cards()[3].(*lister)
	rulesTab.Init()
	if !strings.Contains(ansi.Strip(rulesTab.View(60, 10)), "Bash(rm *)") {
		t.Fatal("rules listed")
	}
	rulesTab.Update(keyp("a"))
	typeText(rulesTab, "allow Glob")
	_, cmd = rulesTab.Update(keyp("enter"))
	if rm, ok := find[RulesMsg](run(cmd)); !ok || len(rm.Set.Allow) != 2 {
		t.Fatalf("%+v", rm)
	}
	rulesTab.Update(keyp("a"))
	typeText(rulesTab, "nonsense")
	_, cmd = rulesTab.Update(keyp("enter"))
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("bad rule toasts")
	}
	rulesTab.Update(keyp("a"))
	typeText(rulesTab, "maybe Glob")
	_, cmd = rulesTab.Update(keyp("enter"))
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("bad decision toasts")
	}
	rulesTab.Update(keyp("a"))
	typeText(rulesTab, "allow Bash((")
	_, cmd = rulesTab.Update(keyp("enter"))
	if _, ok := find[deck.Intent](run(cmd)); !ok {
		t.Fatal("malformed rule toasts")
	}
	rulesTab.Update(keyp("g"))
	_, cmd = rulesTab.Update(keyp("d"))
	if rm, ok := find[RulesMsg](run(cmd)); !ok || len(rm.Set.Deny) != 0 {
		t.Fatalf("delete rule %+v", rm)
	}
	rulesTab.Update(keyp("G"))
	rulesTab.Update(keyp("k"))
	rulesTab.Update(tea.MouseClickMsg{Button: tea.MouseLeft, Y: 1})
	rulesTab.Update(deck.FocusMsg{Focused: true})
	mcpTab := c.Group.Cards()[5].(*lister)
	env.MCP.Add(context.Background(), harness.ServerConfig{Name: "srv", Command: "x", Disabled: true})
	mcpTab.Init()
	if !strings.Contains(ansi.Strip(mcpTab.View(80, 10)), "srv") {
		t.Fatal("mcp listed")
	}
	mcpTab.Update(keyp("o"))
	mcpTab.View(80, 10)
	_, cmd = mcpTab.Update(keyp("R"))
	if rc, ok := find[ReconnectMsg](run(cmd)); !ok || rc.Server != "srv" {
		t.Fatal("reconnect")
	}
	c.Update(harness.MCPMsg{Server: "srv"})
	toolsTab := c.Group.Cards()[6].(*lister)
	toolsTab.Init()
	if !strings.Contains(ansi.Strip(toolsTab.View(100, 20)), "Read") {
		t.Fatal("tools listed")
	}
	toolsTab.Update(keyp("o"))
	toolsTab.View(100, 30)
	logTab := c.Group.Cards()[7].(*lister)
	logTab.Init()
	logTab.View(60, 5)
	c.Update(keyp("]"))
	if _, i := c.Group.Active(); i != 1 {
		t.Fatal("group keys reach the tabs")
	}
	bare := &lister{env: env, title: "t", glyph: "g", id: "i"}
	if bare.Status() != "0" || !strings.Contains(ansi.Strip(bare.View(20, 3)), "nothing here") || bare.Cursor() != nil || len(bare.Claims()) != 5 {
		t.Fatal("bare lister")
	}
	if credential(profiles.Profile{Auth: profiles.AuthNone}) != "none needed" {
		t.Fatal("credential none")
	}
	t.Setenv("ICEBATH_TEST_KEY", "k")
	if got := credential(profiles.Profile{Auth: profiles.AuthBearer, Key: profiles.Env("ICEBATH_TEST_KEY")}); !strings.HasPrefix(got, "set") {
		t.Fatalf("%q", got)
	}
	if got := credential(profiles.Profile{Auth: profiles.AuthBearer, Key: profiles.File("/nope/none")}); !strings.HasPrefix(got, "error") {
		t.Fatalf("%q", got)
	}
}
