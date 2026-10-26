package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/agent"
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
	"github.com/nickheyer/icebath/pkg/tool/shell"
	"github.com/nickheyer/icebath/pkg/ui/anim"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Chat completions server that answers with text, or with a tool call on the first request when asked
func fakeServer(t testing.TB, toolFirst bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		answered := false
		for _, m := range req.Messages {
			if m.Role == "tool" {
				answered = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if toolFirst && !answered {
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"{\\\"command\\\":\\\"echo hi\\\"}\"}}]},\"finish_reason\":null}]}\n\n", req.Model)
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n", req.Model)
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"all done\"},\"finish_reason\":null}]}\n\n", req.Model)
		fmt.Fprintf(w, "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n", req.Model)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func testConfig(t testing.TB, base string) (harness.Config, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, filepath.Join(dir, "cfg"))
	t.Setenv(harness.ProfileEnv, "")
	t.Setenv(harness.ModelEnv, "")
	list := fmt.Sprintf(`[{"name":"fake","vendor":"openai","protocol":"openai-completions","base_url":%q,"models":["m-fast"]}]`, base)
	path := filepath.Join(dir, "profiles.json")
	os.WriteFile(path, []byte(list), 0o644)
	return harness.Config{Profile: "fake", ProfileFiles: []string{path}, Permissions: rules.Set{Allow: []string{"Read"}}, Web: harness.WebConfig{Disabled: true}}, dir
}

func TestHeadlessRunsPromptAndPrintsEvents(t *testing.T) {
	srv, hits := fakeServer(t, false)
	cfg, dir := testConfig(t, srv.URL)
	var out, errw bytes.Buffer
	code := Headless(context.Background(), HeadlessOptions{Config: cfg, Dir: dir, Prompt: "say hi", Out: &out, Err: &errw})
	if code != 0 || hits.Load() != 1 {
		t.Fatalf("code %d hits %d err %s", code, hits.Load(), errw.String())
	}
	if !strings.Contains(out.String(), "all done") || !strings.Contains(errw.String(), "session ") {
		t.Fatalf("out %q err %q", out.String(), errw.String())
	}
	out.Reset()
	errw.Reset()
	code = Headless(context.Background(), HeadlessOptions{Config: cfg, Dir: dir, Prompt: "again", Print: true, Out: &out, Err: &errw})
	if code != 0 {
		t.Fatalf("code %d %s", code, errw.String())
	}
	var kinds []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var l Line
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("%v: %s", err, line)
		}
		kinds = append(kinds, l.Type+":"+l.Event+":"+l.Kind)
	}
	joined := strings.Join(kinds, " ")
	for _, want := range []string{"event:run_start:", "event:turn_start:", "attempt::", "event:model:text_delta", "event:turn_end:", "event:run_end:", "saved::", "run_done::"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("print lacks %q in %s", want, joined)
		}
	}
	out.Reset()
	errw.Reset()
	code = Headless(context.Background(), HeadlessOptions{Config: cfg, Dir: dir, In: strings.NewReader(" from stdin "), Out: &out, Err: &errw})
	if code != 0 || !strings.Contains(out.String(), "all done") {
		t.Fatalf("stdin prompt %d %q", code, errw.String())
	}
	if code := Headless(context.Background(), HeadlessOptions{Config: cfg, Dir: dir, Out: &out, Err: &errw}); code != 2 {
		t.Fatal("empty prompt must fail")
	}
	bad := cfg
	bad.Profile = "nope"
	if code := Headless(context.Background(), HeadlessOptions{Config: bad, Dir: dir, Prompt: "x", Out: &out, Err: &errw}); code != 2 {
		t.Fatal("bad profile must fail")
	}
	out.Reset()
	errw.Reset()
	code = Headless(context.Background(), HeadlessOptions{Config: cfg, Dir: dir, Mission: "map things", Print: true, Out: &out, Err: &errw})
	if code != 0 || !strings.Contains(out.String(), `"type":"mission"`) {
		t.Fatalf("mission %d %s", code, out.String())
	}
}

func TestHeadlessAnswersApprovals(t *testing.T) {
	srv, _ := fakeServer(t, true)
	cfg, dir := testConfig(t, srv.URL)
	var out, errw bytes.Buffer
	code := Headless(context.Background(), HeadlessOptions{Config: cfg, Dir: dir, Prompt: "run it", Out: &out, Err: &errw})
	if code != 0 || !strings.Contains(errw.String(), "refused") {
		t.Fatalf("denied approval must refuse the tool: %d %s", code, errw.String())
	}
	out.Reset()
	errw.Reset()
	code = Headless(context.Background(), HeadlessOptions{Config: cfg, Dir: dir, Prompt: "run it", Yes: true, Print: true, Out: &out, Err: &errw})
	if code != 0 || !strings.Contains(out.String(), `"verdict":"yes"`) || !strings.Contains(out.String(), `"event":"tool_end"`) {
		t.Fatalf("yes must approve: %d %s", code, out.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := Headless(ctx, HeadlessOptions{Config: cfg, Dir: dir, Prompt: "x", Out: &out, Err: &errw}); code != 130 && code != 1 && code != 2 {
		t.Fatalf("cancelled context %d", code)
	}
}

func TestEncodeCoversEveryMessage(t *testing.T) {
	now := time.Now()
	msgs := []any{
		harness.EventMsg{Run: "r", Event: agent.Event{Type: agent.EventToolEnd, Time: now, Call: protocol.ToolCall{ID: "c", Name: "Read"}, Result: protocol.ToolResult{CallID: "c", Content: []protocol.Part{protocol.Text{Text: "x"}}}, Err: errors.New("e"), Usage: protocol.Usage{Input: 1}, Duration: time.Second, Parts: []protocol.Part{protocol.Text{Text: "p"}}, Data: map[string]int{"a": 1}}},
		harness.EventMsg{Event: agent.Event{Type: agent.EventModel, Model: protocol.Event{Type: protocol.EventTextDelta, Delta: "d"}}},
		harness.ProgressMsg{Run: "r", Progress: tool.Progress{CallID: "c", Tool: "Bash", Stream: "stdout", Text: "t"}},
		harness.ApprovalMsg{Pending: &harness.Pending{ID: 1, Run: "r", Request: policy.Request{Call: protocol.ToolCall{Name: "Bash"}, Reason: "why", Key: "k"}}},
		harness.ApprovalDoneMsg{ID: 1, Verdict: policy.Yes, Err: errors.New("e")},
		harness.RunDoneMsg{Run: "r", Response: protocol.Response{Content: []protocol.Part{protocol.Text{Text: "t"}}}, Err: errors.New("e")},
		harness.AttemptMsg{Attempt: provider.Attempt{Status: 500, Err: errors.New("e"), Started: now}},
		harness.FailoverMsg{From: 0, Err: errors.New("e")},
		harness.SavedMsg{Session: "s", Path: "p", Err: errors.New("e")},
		harness.FactMsg{Session: "s", Change: facts.Change{Op: facts.OpAdd, Fact: facts.Fact{ID: "f1"}}},
		harness.MissionMsg{Session: "s", Mission: mission.Mission{ID: "m1"}},
		harness.JobMsg{Event: shell.JobEvent{Kind: shell.JobOutput, Data: []byte("x")}},
		harness.MCPMsg{Server: "s", Err: errors.New("e")},
		harness.HookOutputMsg{Run: "r", Text: "t"},
	}
	for _, m := range msgs {
		l, ok := encode(m)
		if !ok || l.Type == "" {
			t.Fatalf("%T not encoded", m)
		}
		if _, err := json.Marshal(l); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := encode("other"); ok {
		t.Fatal("unknown messages are skipped")
	}
	tw := &textWriter{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	tw.write(harness.EventMsg{Event: agent.Event{Type: agent.EventModel, Model: protocol.Event{Type: protocol.EventTextDelta, Delta: "d"}}})
	tw.write(harness.EventMsg{Event: agent.Event{Type: agent.EventToolStart, Call: protocol.ToolCall{Name: "Read", Args: json.RawMessage(`{}`)}}})
	tw.write(harness.EventMsg{Event: agent.Event{Type: agent.EventToolEnd, Call: protocol.ToolCall{Name: "Read"}, Result: protocol.ToolResult{IsError: true, Content: []protocol.Part{protocol.Text{Text: "bad"}}}}})
	tw.write(harness.EventMsg{Event: agent.Event{Type: agent.EventSubagentStart, Call: protocol.ToolCall{Name: "kid"}}})
	tw.write(harness.EventMsg{Event: agent.Event{Type: agent.EventNote, Text: "n"}})
	tw.write(harness.EventMsg{Event: agent.Event{Type: agent.EventTurnEnd}})
	tw.write(harness.EventMsg{Event: agent.Event{Type: agent.EventTurnEnd, Depth: 1}})
	tw.write(harness.ApprovalMsg{Pending: &harness.Pending{Request: policy.Request{Call: protocol.ToolCall{Name: "Bash"}}}})
	tw.write(harness.FactMsg{Change: facts.Change{Op: facts.OpAdd, Fact: facts.Fact{ID: "f1", Title: "t"}}})
	if got := tw.err.(*bytes.Buffer).String(); !strings.Contains(got, "Read") || !strings.Contains(got, "bad") || !strings.Contains(got, "kid") || !strings.Contains(got, "approval") || !strings.Contains(got, "f1") {
		t.Fatalf("%q", got)
	}
}

// Runs a command and collects the messages it produces, dropping commands that sleep longer than a beat
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var msg tea.Msg
	select {
	case msg = <-done:
	case <-time.After(120 * time.Millisecond):
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, drain(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// Drains a command and hands every message it produced back to the model, frames excepted
func feed(m *Model, cmd tea.Cmd) []tea.Msg {
	var seen []tea.Msg
	queue := drain(cmd)
	for depth := 0; len(queue) > 0 && depth < 6; depth++ {
		var next []tea.Msg
		for _, msg := range queue {
			seen = append(seen, msg)
			switch msg.(type) {
			case anim.FrameMsg, tea.QuitMsg:
				continue
			}
			_, c := m.Update(msg)
			next = append(next, drain(c)...)
		}
		queue = next
	}
	return seen
}

// Delivers a message the way the runtime does, rendering after every update before the next message runs
func live(m *Model, msg tea.Msg) {
	_, cmd := m.Update(msg)
	m.View()
	for _, next := range drain(cmd) {
		switch next.(type) {
		case anim.FrameMsg, tea.QuitMsg:
			continue
		}
		live(m, next)
	}
}

func has[T any](msgs []tea.Msg) bool {
	for _, m := range msgs {
		if _, ok := m.(T); ok {
			return true
		}
	}
	return false
}

func toasted(m *Model, text string) bool {
	for _, t := range m.wm.Toasts() {
		if strings.Contains(t, text) {
			return true
		}
	}
	return false
}

func newModel(t *testing.T, toolFirst bool) (*Model, *httptest.Server) {
	t.Helper()
	srv, _ := fakeServer(t, toolFirst)
	cfg, dir := testConfig(t, srv.URL)
	m, err := New(context.Background(), Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(m.Init())
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m, srv
}

// Pumps queued relay messages into the model until the predicate holds or time runs out
func pump(t *testing.T, m *Model, until func(tea.Msg) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.relay.mu.Lock()
		queued := m.relay.queue
		m.relay.queue = nil
		m.relay.mu.Unlock()
		for _, msg := range queued {
			_, cmd := m.Update(msg)
			feed(m, cmd)
			if until(msg) {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("message never arrived")
}

func runDone(msg tea.Msg) bool { _, ok := msg.(harness.RunDoneMsg); return ok }

func key(code rune, mod tea.KeyMod) tea.KeyPressMsg {
	k := tea.KeyPressMsg{Code: code, Mod: mod}
	if mod == 0 {
		k.Text = string(code)
	}
	return k
}

// Every dial on the bar, joined by spaces
func dialText(m *Model) string {
	var parts []string
	for _, s := range m.status().Dials {
		parts = append(parts, s.Text)
	}
	return strings.Join(parts, " ")
}

// Lane the bar shows for a workspace
func laneOf(m *Model, ws *wm.Workspace) wm.Lane { return m.status().Lanes[ws] }

func typeInto(m *Model, text string) {
	for _, r := range text {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

func submit(m *Model, d *desk, text string) tea.Cmd {
	_, cmd := m.Update(cards.SubmitMsg{Session: d.live.Session.ID(), Parts: []protocol.Part{protocol.Text{Text: text}}, Text: text})
	return cmd
}

func TestModelRunsPromptAndRoutes(t *testing.T) {
	m, _ := newModel(t, false)
	if len(m.desks) != 1 || m.desks[0].live != m.app.Current() || m.current() != m.desks[0] || m.state != tea.ProgramStateIdle {
		t.Fatal("one desk bound to the current session")
	}
	v := m.View()
	if !v.AltScreen || v.ProgramStatus == nil || v.ProgramStatus.State != tea.ProgramStateIdle {
		t.Fatal("view status")
	}
	d := m.desks[0]
	_, cmd := m.Update(StartPrompt("hello"))
	if l := laneOf(m, m.workspaceOf(d)); m.state != tea.ProgramStateWorking || d.live.Session.Title() != "hello" || l.Signal != wm.Working || l.Name != "hello" {
		t.Fatalf("state %v title %q lane %+v", m.state, d.live.Session.Title(), l)
	}
	if m.View().ProgressBar == nil {
		t.Fatal("busy app shows an indeterminate bar")
	}
	feed(m, cmd)
	pump(t, m, runDone)
	if m.state != tea.ProgramStateDone || d.live.Session.Len() != 2 {
		t.Fatalf("state %v len %d", m.state, d.live.Session.Len())
	}
	if m.sync(); laneOf(m, m.workspaceOf(d)).Signal != wm.Idle {
		t.Fatal("chip settles after the run, seen since its workspace is on the stage")
	}
	feed(m, submit(m, d, "second"))
	wasBusy := m.app.Busy(d.live)
	feed(m, submit(m, d, "steer"))
	if wasBusy && !toasted(m, "queued") {
		t.Fatal("input during a run steers it")
	}
	pump(t, m, runDone)
	deadline := time.Now().Add(5 * time.Second)
	for m.app.Busy(d.live) && time.Now().Before(deadline) {
		pump(t, m, func(tea.Msg) bool { return true })
	}
	if d.live.Session.Len() < 4 {
		t.Fatalf("len %d", d.live.Session.Len())
	}
}

func TestWorkspaces(t *testing.T) {
	m, _ := newModel(t, false)
	first := m.desks[0]
	feed(m, m.newWorkspace())
	if len(m.desks) != 2 || len(m.wm.Workspaces()) != 2 || m.wm.ActiveIndex() != 1 {
		t.Fatalf("desks %d active %d", len(m.desks), m.wm.ActiveIndex())
	}
	second := m.desks[1]
	m.sync()
	if m.app.Session != second.live.Session || m.current() != second {
		t.Fatal("a new workspace opens with a fresh session selected")
	}
	cmd := submit(m, second, "work")
	if !m.app.Busy(second.live) || m.app.Busy(first.live) {
		t.Fatal("prompt runs in its own workspace")
	}
	if feed(m, m.closeWorkspace()); len(m.desks) != 2 || !toasted(m, "agent still working") {
		t.Fatal("busy workspace stays open")
	}
	feed(m, cmd)
	pump(t, m, runDone)
	m.Update(key('1', tea.ModAlt))
	if m.wm.ActiveIndex() != 0 || m.app.Session != first.live.Session {
		t.Fatal("alt+1 switches back and selects the first session")
	}
	m.Update(key('2', tea.ModAlt))
	if m.wm.ActiveIndex() != 1 {
		t.Fatal("alt+2 switches forward")
	}
	released := second.live.Session.ID()
	feed(m, m.closeWorkspace())
	if len(m.desks) != 1 || len(m.wm.Workspaces()) != 1 || m.app.LiveByID(released) != nil || !toasted(m, "stored work") {
		t.Fatal("idle workspace closes and stores its session")
	}
	feed(m, m.closeWorkspace())
	if len(m.desks) != 1 || !toasted(m, "last workspace") {
		t.Fatal("the last workspace stays")
	}
	feed(m, m.openStored(released))
	if len(m.desks) != 2 || len(m.wm.Workspaces()) != 1 || m.current().live.Session.ID() != released || m.wm.Focused() != m.current().session {
		t.Fatal("a stored session reopens as a card in the workspace on the stage with the focus")
	}
	resumed := m.current()
	feed(m, m.wm.Focus(first.session))
	feed(m, m.openStored(released))
	if len(m.desks) != 2 || m.wm.Focused() != resumed.session {
		t.Fatal("opening an open session only focuses it")
	}
	feed(m, m.wm.Focus(first.session))
	feed(m, m.forkStored(first.live.Session.ID()))
	if len(m.desks) != 3 || len(m.wm.Workspaces()) != 1 || m.current().live.Session.Parent() != first.live.Session.ID() {
		t.Fatal("fork makes a new card beside the source with the parent set")
	}
	feed(m, m.wm.Focus(first.session))
	feed(m, submit(m, first, "turn"))
	pump(t, m, runDone)
	feed(m, m.pickTurn())
	p, ok := m.wm.Overlay().(*wm.Palette)
	if !ok || len(p.Matches()) < 2 {
		t.Fatal("turn picker lists turns and the end")
	}
	n := len(m.desks)
	feed(m, p.Matches()[0].Run())
	feed(m, m.wm.Dismiss())
	if len(m.desks) != n+1 || m.current().live.Session.Len() != 0 || m.current().live.Session.Parent() != first.live.Session.ID() {
		t.Fatalf("fork before the first turn keeps nothing, len %d", m.current().live.Session.Len())
	}
}

func TestCommandsAndPickers(t *testing.T) {
	m, _ := newModel(t, false)
	d := m.desks[0]
	feed(m, submit(m, d, "hello"))
	pump(t, m, runDone)
	feed(m, m.command(nil, "mode", ""))
	if m.app.Mode.Mode() != mode.AcceptEdits {
		t.Fatal("mode cycles")
	}
	feed(m, m.command(nil, "mode", "plan"))
	if m.app.Mode.Mode() != mode.Plan {
		t.Fatal("mode set")
	}
	feed(m, m.command(nil, "mode", "weird"))
	if m.app.Mode.Mode() != mode.Plan {
		t.Fatal("bad mode keeps the old one")
	}
	feed(m, m.command(nil, "model", "m-other"))
	if m.app.Primary.Model != "m-other" {
		t.Fatal("model command")
	}
	feed(m, m.command(nil, "theme", "light"))
	if m.theme.Name != "light" || m.wm.Theme().Name != "light" {
		t.Fatal("theme command")
	}
	feed(m, m.command(nil, "theme", "nope"))
	if !toasted(m, "themes:") {
		t.Fatal("bad theme toasts")
	}
	feed(m, m.command(nil, "rename", "named"))
	if d.live.Session.Title() != "named" {
		t.Fatal("rename")
	}
	feed(m, m.command(nil, "export", ""))
	if _, err := os.Stat(m.exportPath(d)); err != nil {
		t.Fatal("export writes the transcript")
	}
	feed(m, m.command(nil, "compact", ""))
	if !d.compacting {
		t.Fatal("compact starts")
	}
	pump(t, m, func(msg tea.Msg) bool { _, ok := msg.(compactedMsg); return ok })
	if d.compacting {
		t.Fatal("compaction ends")
	}
	feed(m, m.command(nil, "cancel", ""))
	feed(m, m.command(nil, "interrupt", ""))
	if !toasted(m, "nothing running") {
		t.Fatal("cancel and interrupt with nothing running toast")
	}
	feed(m, m.command(nil, "nope", ""))
	if !toasted(m, "unknown command") {
		t.Fatal("unknown command toasts")
	}
	feed(m, m.command(nil, "attach", "/nowhere/at/all.png"))
	if len(m.wm.Toasts()) == 0 {
		t.Fatal("missing attachment toasts")
	}
	feed(m, m.command(nil, "new", ""))
	if len(m.desks) != 2 || len(m.wm.Workspaces()) != 1 || m.current() == d {
		t.Fatal("new opens a session beside this one and focuses it")
	}
	feed(m, m.command(nil, "close", ""))
	if len(m.desks) != 1 || m.current() != d {
		t.Fatal("close drops it")
	}
	feed(m, m.command(nil, "workspace", ""))
	if len(m.desks) != 2 || len(m.wm.Workspaces()) != 2 || m.wm.ActiveIndex() != 1 {
		t.Fatal("workspace opens a new one holding a fresh session")
	}
	feed(m, m.closeWorkspace())
	for name, k := range map[string]string{"metrics": "metrics", "files": "files", "jobs": "jobs", "sessions": "sessions", "config": "config", "agents": "agents", "findings": "findings:" + d.live.Session.ID()} {
		feed(m, m.command(nil, name, ""))
		if !m.wm.Has(k) {
			t.Fatalf("%s opens %s", name, k)
		}
		n := len(m.wm.Active().Cards())
		feed(m, m.command(nil, name, ""))
		if len(m.wm.Active().Cards()) != n {
			t.Fatalf("%s opens once", name)
		}
	}
	for name, open := range map[string]func() tea.Cmd{"mode": m.pickMode, "theme": m.pickTheme, "motion": m.pickMotion, "profile": m.pickProfile, "session": m.pickSession, "turn": m.pickTurn} {
		feed(m, open())
		if m.wm.Overlay() == nil {
			t.Fatalf("%s picker opens", name)
		}
		feed(m, m.wm.Dismiss())
	}
	feed(m, m.promptRename())
	if _, ok := m.wm.Overlay().(*wm.Palette); !ok {
		t.Fatal("rename prompt opens")
	}
	m.Update(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	typeInto(m, "zed")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	feed(m, cmd)
	if d.live.Session.Title() != "zed" || m.wm.Overlay() != nil {
		t.Fatalf("prompt submits through the overlay, title %q", d.live.Session.Title())
	}
	os.Remove(m.exportPath(d))
	feed(m, m.promptExport())
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	feed(m, cmd)
	if _, err := os.Stat(m.exportPath(d)); err != nil {
		t.Fatal("export prompt writes the default path")
	}
	feed(m, m.promptMission())
	typeInto(m, "scan")
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	feed(m, cmd)
	if d.mission == "" {
		t.Fatal("mission prompt starts a mission")
	}
	pump(t, m, runDone)
	m.Update(modelsMsg{err: errors.New("listing broke")})
	if !toasted(m, "listing broke") {
		t.Fatal("model listing error toasts")
	}
	_, cmd = m.Update(modelsMsg{list: []profiles.Model{{ID: "m-fast", Context: 1000}, {ID: "m-other"}}})
	feed(m, cmd)
	p, ok := m.wm.Overlay().(*wm.Palette)
	if !ok || len(p.Matches()) != 2 {
		t.Fatal("model picker opens")
	}
	feed(m, m.wm.Dismiss())
	_, cmd = m.Update(key(tea.KeySpace, tea.ModAlt))
	feed(m, cmd)
	p, ok = m.wm.Overlay().(*wm.Palette)
	if !ok {
		t.Fatal("alt+space opens the palette")
	}
	typeInto(m, "theme")
	if ms := p.Matches(); len(ms) == 0 || ms[0].Title != "Theme"+m.theme.Glyphs.Ellipsis {
		t.Fatalf("palette finds the app commands, %v", ms)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.wm.Overlay() != nil {
		t.Fatal("esc closes the palette")
	}
}

func TestCardMessages(t *testing.T) {
	m, _ := newModel(t, false)
	d := m.desks[0]
	feed(m, submit(m, d, "hello"))
	pump(t, m, runDone)
	m.Update(cards.AnswerMsg{ID: 99})
	if !toasted(m, "approval gone") {
		t.Fatal("stale answer toasts")
	}
	m.Update(cards.KillRunMsg{Run: "nope"})
	if !toasted(m, "no agent nope") {
		t.Fatal("bad run kill toasts")
	}
	_, cmd := m.Update(cards.KillJobMsg{Job: "nope"})
	if !has[errMsg](drain(cmd)) {
		t.Fatal("bad job kill yields an error")
	}
	feed(m, m.command(nil, "metrics", ""))
	feed(m, m.wm.Focus(m.metrics))
	_, cmd = m.Update(cards.OpenRunMsg{Run: m.app.Control.Runs()[0].ID})
	feed(m, cmd)
	if m.wm.Focused() != d.session {
		t.Fatal("opening the root run focuses the session card")
	}
	m.Update(cards.OpenSessionMsg{Sessions: []string{"nope"}})
	if len(m.wm.Toasts()) == 0 {
		t.Fatal("bad session open toasts")
	}
	m.Update(cards.DeleteMsg{Sessions: []string{d.live.Session.ID()}})
	if !toasted(m, d.live.Session.ID()+" is open") {
		t.Fatal("open sessions cannot be deleted")
	}
	stored := func(text string) string {
		feed(m, m.newWorkspace())
		other := m.current()
		feed(m, submit(m, other, text))
		pump(t, m, runDone)
		id := other.live.Session.ID()
		feed(m, m.closeWorkspace())
		return id
	}
	gone := stored("second")
	m.Update(cards.DeleteMsg{Sessions: []string{gone}})
	if ok, _ := m.app.Store.Exists(context.Background(), gone); ok || !toasted(m, "deleted "+gone) {
		t.Fatal("closed session deletes")
	}
	a, b := stored("third"), stored("fourth")
	_, cmd = m.Update(cards.OpenSessionMsg{Sessions: []string{a, b}})
	feed(m, cmd)
	if len(m.desks) != 3 || len(m.wm.Workspaces()) != 1 || m.current().live.Session.ID() != b || !toasted(m, "resumed 2 sessions") {
		t.Fatalf("resuming many opens a card for each in this workspace, the last focused, %d desks", len(m.desks))
	}
	_, cmd = m.Update(cards.OpenSessionMsg{Sessions: []string{a}})
	feed(m, cmd)
	if len(m.desks) != 3 || m.current().live.Session.ID() != a || !toasted(m, "resumed third") {
		t.Fatal("resuming an open one focuses it and names it")
	}
	_, cmd = m.Update(cards.ForkMsg{Sessions: []string{a, b}})
	feed(m, cmd)
	if len(m.desks) != 5 || m.current().live.Session.Parent() != b || !toasted(m, "forked 2 sessions") {
		t.Fatalf("forking many makes a copy of each, %d desks", len(m.desks))
	}
	_, cmd = m.Update(cards.ForkMsg{Sessions: []string{a, "nope"}})
	feed(m, cmd)
	if len(m.desks) != 6 || !toasted(m, "forked third into ") {
		t.Fatal("one good fork names its source among the failures")
	}
	for _, other := range slices.Clone(m.desks) {
		if other != d {
			feed(m, m.closeSession(other))
		}
	}
	if len(m.desks) != 1 || m.desks[0] != d {
		t.Fatalf("closing every other session card leaves the first, %d desks", len(m.desks))
	}
	_, cmd = m.Update(cards.DeleteMsg{Sessions: []string{a, b}})
	feed(m, cmd)
	if ok, _ := m.app.Store.Exists(context.Background(), a); ok || !toasted(m, "deleted 2 sessions") {
		t.Fatal("deleting many deletes each")
	}
	m.Update(cards.SetModelMsg{Model: "m-two"})
	if m.app.Primary.Model != "m-two" {
		t.Fatal("set model message")
	}
	m.Update(cards.SetModeMsg{Mode: mode.Plan})
	if m.app.Mode.Mode() != mode.Plan {
		t.Fatal("set mode message")
	}
	_, cmd = m.Update(cards.SetProfileMsg{Profile: "nope"})
	msgs := feed(m, cmd)
	if !has[profileMsg](msgs) || len(m.wm.Toasts()) == 0 {
		t.Fatal("bad profile reports its error")
	}
	_, cmd = m.Update(cards.ReconnectMsg{Server: "nope"})
	if !has[errMsg](drain(cmd)) {
		t.Fatal("bad reconnect yields an error")
	}
	m.Update(cards.ExportMsg{Path: "p"})
	if !toasted(m, "wrote p") {
		t.Fatal("export result toasts")
	}
	m.Update(cards.ExportMsg{Err: errors.New("disk full")})
	if !toasted(m, "disk full") {
		t.Fatal("export error toasts")
	}
	m.Update(cards.SettingMsg{Key: "theme", Value: "plain"})
	if m.theme.Name != "plain" {
		t.Fatal("theme setting")
	}
	m.Update(cards.SettingMsg{Key: "motion", Value: "0"})
	if m.wm.Motion() != 0 {
		t.Fatal("motion setting")
	}
	m.Update(cards.SettingMsg{Key: "cascade", Value: "true"})
	if !m.wm.Cascade() || m.settings()["cascade"] != "true" {
		t.Fatal("cascade setting")
	}
	m.Update(cards.SettingMsg{Key: "cascade", Value: "maybe"})
	if !toasted(m, "cascade must be") || !m.wm.Cascade() {
		t.Fatal("bad cascade setting toasts and keeps the old value")
	}
	feed(m, m.wm.Run("cascade.toggle"))
	if m.wm.Cascade() || !toasted(m, "cascade = false") {
		t.Fatal("the palette toggles the comet off")
	}
	feed(m, m.wm.Run("cascade.toggle"))
	if !m.wm.Cascade() {
		t.Fatal("the palette toggles the comet back on")
	}
	m.Update(cards.SettingMsg{Key: "compact_threshold", Value: "500"})
	if m.app.Meter.Snapshot().Threshold != 500 {
		t.Fatal("threshold setting")
	}
	m.Update(cards.SettingMsg{Key: "mode", Value: "acceptEdits"})
	if m.app.Mode.Mode() != mode.AcceptEdits {
		t.Fatal("mode setting")
	}
	m.Update(cards.SettingMsg{Key: "max_tokens", Value: "bad"})
	if !toasted(m, "max_tokens must be") {
		t.Fatal("bad setting toasts")
	}
	m.Update(cards.SettingMsg{Key: "max_tokens", Value: "2048"})
	if m.settings()["max_tokens"] != "2048" || m.settings()["temperature"] != "" || m.settings()["max_rounds"] != "" {
		t.Fatalf("unset settings read empty %v", m.settings())
	}
	m.Update(cards.SettingMsg{Key: "max_tokens", Value: ""})
	if m.app.Config.MaxTokens != 0 || m.settings()["max_tokens"] != "" || !toasted(m, "max_tokens unset") {
		t.Fatalf("an empty value unsets the setting %v", m.settings())
	}
	m.Update(cards.SettingMsg{Key: "max_rounds", Value: "9", Quiet: true})
	if m.app.Config.MaxRounds != 9 || toasted(m, "max_rounds = 9") {
		t.Fatal("a quiet setting applies without a toast")
	}
	feed(m, m.startMission(d, "scan the net"))
	pump(t, m, runDone)
	if len(d.missions) != 1 || d.mission == "" {
		t.Fatal("mission card")
	}
	id := d.mission
	if !m.wm.Has("mission:" + d.live.Session.ID() + ":" + id) {
		t.Fatal("mission card opens")
	}
	m.Update(cards.MissionStateMsg{Session: d.live.Session.ID(), ID: id, Status: mission.StatusDone})
	if ms, _ := d.live.Missions.Get(id); ms.Status != mission.StatusDone {
		t.Fatal("mission finishes")
	}
	m.Update(cards.MissionStateMsg{Session: d.live.Session.ID(), ID: id, Status: mission.StatusActive})
	if ms, _ := d.live.Missions.Get(id); ms.Status != mission.StatusActive || d.mission != id {
		t.Fatal("mission resumes")
	}
	m.Update(cards.MissionStateMsg{Session: d.live.Session.ID(), ID: "nope", Status: mission.StatusDone})
	m.Update(cards.MissionRemoveMsg{Session: d.live.Session.ID(), ID: "nope"})
	if !toasted(m, "no mission nope") {
		t.Fatal("bad mission remove toasts")
	}
	m.Update(cards.MissionRemoveMsg{Session: d.live.Session.ID(), ID: id})
	pump(t, m, func(msg tea.Msg) bool { mm, ok := msg.(harness.MissionMsg); return ok && mm.Op == mission.OpRemove })
	if len(d.missions) != 0 || m.wm.Has("mission:"+d.live.Session.ID()+":"+id) || !toasted(m, "forgot mission") {
		t.Fatal("removed mission closes its card")
	}
}

func TestApprovalFlow(t *testing.T) {
	m, _ := newModel(t, true)
	d := m.desks[0]
	_, cmd := m.Update(StartPrompt("run it"))
	feed(m, cmd)
	pump(t, m, func(msg tea.Msg) bool { _, ok := msg.(harness.ApprovalMsg); return ok })
	if !m.wm.Has("approval:"+d.live.Session.ID()) || m.state != tea.ProgramStateBlocked || len(d.approval.Pending()) != 1 {
		t.Fatal("approval dialog opens in the workspace")
	}
	if l := laneOf(m, m.wm.Active()); l.Signal != wm.Blocked || l.Detail != "Bash" || m.status().Notice.Text != "" {
		t.Fatalf("the lane shows the wait while the dialog holds the focus, %+v %+v", l, m.status().Notice)
	}
	feed(m, m.wm.Focus(d.session))
	if n := m.status().Notice; n.Text != "approve Bash echo hi ∷ 1 run it" || n.Signal != wm.Blocked || n.Action != "attention.next" || n.More != 0 {
		t.Fatalf("an approval off the focus waits in the field, %+v", n)
	}
	feed(m, m.wm.Run("attention.next"))
	if m.wm.Focused() != wm.Card(d.approval) {
		t.Fatal("the field serves the approval by focusing its dialog")
	}
	feed(m, m.answer(policy.Yes)())
	pump(t, m, runDone)
	if m.state != tea.ProgramStateDone || len(d.approval.Pending()) != 0 || m.wm.Has("approval:"+d.live.Session.ID()) {
		t.Fatalf("answered approval lets the run finish, state %v", m.state)
	}
	if m.answer(policy.No)() != nil {
		t.Fatal("answering with nothing pending does nothing")
	}
}

func TestSubagentCardsOpenAndRetire(t *testing.T) {
	m, _ := newModel(t, false)
	if m.openAgent("run-9/kid") != nil {
		t.Fatal("unknown child run opens nothing")
	}
	if m.retireAgent("run-9/kid") != nil {
		t.Fatal("unknown child run retires nothing")
	}
	feed(m, m.openRun("run-9"))
	if !toasted(m, "no agent run-9") {
		t.Fatal("unknown run toasts")
	}
}

func TestThemeAndLayoutPersistence(t *testing.T) {
	srv, _ := fakeServer(t, false)
	cfg, dir := testConfig(t, srv.URL)
	m, err := New(context.Background(), Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(m.Init())
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	feed(m, m.setSetting("theme", "light", false))
	if m.theme.Name != "light" || m.env.Theme.Name != "light" || m.wm.Theme().Name != "light" {
		t.Fatal("theme reaches the manager and the cards")
	}
	feed(m, m.command(nil, "metrics", ""))
	m.saveLayout()
	l, _, ok := loadLayout(layoutPath(m.app.Config))
	if !ok || l.Theme != "light" || len(l.Workspaces) != 1 || len(l.Workspaces[0].Tiles) != 2 || l.Workspaces[0].Tiles[1].Key != "metrics" {
		t.Fatalf("layout %+v", l)
	}
	again, err := New(context.Background(), Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(again.Init())
	again.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if again.theme.Name != "light" || !again.wm.Has("metrics") {
		t.Fatal("saved layout reopens the metrics card with the theme")
	}
	again.shutdown()
	m.shutdown()
}

func TestSettingsSurviveARestart(t *testing.T) {
	srv, _ := fakeServer(t, false)
	cfg, dir := testConfig(t, srv.URL)
	project := filepath.Join(dir, harness.ProjectDir, harness.ConfigFile)
	os.MkdirAll(filepath.Dir(project), 0o755)
	os.WriteFile(project, []byte(`{"ui":{"theme":"contrast"}}`), 0o644)
	m, err := New(context.Background(), Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(m.Init())
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, k := range [][2]string{{"cascade", "true"}, {"motion", "0.5"}, {"max_tokens", "2048"}, {"max_rounds", "7"}, {"temperature", "0.3"}, {"compact_threshold", "500"}} {
		m.Update(cards.SettingMsg{Key: k[0], Value: k[1]})
	}
	feed(m, m.command(nil, "theme", "light"))
	m.Update(cards.SetModeMsg{Mode: mode.Plan})
	_, cmd := m.Update(cards.SetProfileMsg{Profile: "fake", Model: "m-fast"})
	feed(m, cmd)
	feed(m, m.command(nil, "model", "m-other"))
	got, err := harness.Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.UI.Cascade == nil || !*got.UI.Cascade || *got.UI.Motion != 0.5 || got.MaxTokens != 2048 || got.MaxRounds != 7 || *got.Temperature != 0.3 || got.Compaction.Threshold != 500 {
		t.Fatalf("settings are saved %+v", got)
	}
	if got.UI.Theme != "light" || got.Mode != "plan" || got.Profile != "fake" || got.Model != "m-other" {
		t.Fatalf("theme, mode, profile and model are saved %+v", got)
	}
	if user, _ := harness.ReadConfigFile(filepath.Join(dir, "cfg", harness.ConfigFile)); user.UI.Theme != "" {
		t.Fatal("the theme lands in the project file that already set it")
	}
	again, err := New(context.Background(), Options{Config: harness.Merge(cfg, got), Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(again.Init())
	if !again.wm.Cascade() || again.wm.Motion() != 0.5 || again.theme.Name != "light" || again.app.Mode.Mode() != mode.Plan || again.app.Primary.Model != "m-other" {
		t.Fatal("a restart comes back with every setting")
	}
	os.WriteFile(filepath.Join(dir, "cfg", harness.ConfigFile), []byte(`{bad`), 0o644)
	m.Update(cards.SettingMsg{Key: "cascade", Value: "false"})
	if m.wm.Cascade() || !toasted(m, harness.ConfigFile) {
		t.Fatal("a setting that cannot be saved applies and toasts")
	}
	feed(m, m.command(nil, "model", "m-third"))
	if m.app.Primary.Model != "m-third" || !toasted(m, harness.ConfigFile) {
		t.Fatal("a model that cannot be saved applies and toasts")
	}
	again.shutdown()
	m.shutdown()
}

func TestConfigSettingsApplyTheMomentTheyChange(t *testing.T) {
	m, _ := newModel(t, false)
	m.wm.SetMotion(0)
	feed(m, m.command(nil, "config", ""))
	feed(m, m.config.Select(2))
	feed(m, m.wm.Focus(m.config))
	shows := func(row, value string) bool {
		return regexp.MustCompile(`\b` + row + `\s+` + regexp.QuoteMeta(value) + `[\s▏]`).MatchString(ansi.Strip(m.View().Content))
	}
	press := func(code rune, times int) {
		for range times {
			live(m, tea.KeyPressMsg{Code: code})
		}
	}
	if !shows("max_rounds", "-") || !shows("temperature", "-") || !shows("compact_threshold", "-") {
		t.Fatal("unset numbers show a muted dash")
	}
	press(tea.KeyDown, 7)
	press(tea.KeyLeft, 1)
	if !m.wm.Cascade() || toasted(m, "cascade = true") {
		t.Fatal("a choice applies quietly the moment it moves")
	}
	press(tea.KeyUp, 6)
	for _, r := range "4096" {
		live(m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if !shows("max_tokens", "4096") {
		t.Fatal("the value being typed shows in its box")
	}
	press(tea.KeyEnter, 1)
	if m.app.Config.MaxTokens != 4096 || !shows("max_tokens", "4096") {
		t.Fatal("an applied value shows at once")
	}
	press(tea.KeyDown, 1)
	press(tea.KeyRight, 1)
	if m.app.Config.MaxRounds != 50 || !shows("max_rounds", "50") {
		t.Fatalf("the slider steps an unset number from its start %d", m.app.Config.MaxRounds)
	}
	press(tea.KeyRight, 2)
	if m.app.Config.MaxRounds != 52 {
		t.Fatalf("the slider steps on %d", m.app.Config.MaxRounds)
	}
	press(tea.KeyBackspace, 1)
	if m.app.Config.MaxRounds != 0 || !shows("max_rounds", "-") {
		t.Fatal("backspace unsets the number")
	}
	live(m, cards.CommandMsg{Session: m.desks[0].live.Session.ID(), Name: "mode"})
	if m.app.Mode.Mode() != mode.AcceptEdits || !shows("mode", "default") {
		t.Fatal("a mode cycled from the session applies with every choice still in view")
	}
	live(m, key(tea.KeySpace, tea.ModAlt))
	for _, r := range "cascade" {
		live(m, tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	press(tea.KeyEnter, 1)
	if m.wm.Cascade() {
		t.Fatal("a setting changed from the palette applies")
	}
}

func TestCtrlC(t *testing.T) {
	m, _ := newModel(t, false)
	d := m.desks[0]
	submit(m, d, "hello")
	if !m.app.Busy(d.live) {
		t.Fatal("run goes")
	}
	_, cmd := m.Update(key('c', tea.ModCtrl))
	if m.quitting || !toasted(m, "cancelled") || has[tea.QuitMsg](drain(cmd)) {
		t.Fatal("first ctrl+c cancels")
	}
	_, cmd = m.Update(key('c', tea.ModCtrl))
	if !m.quitting || !has[tea.QuitMsg](drain(cmd)) {
		t.Fatal("second ctrl+c inside the window quits")
	}
	idle, _ := newModel(t, false)
	_, cmd = idle.Update(key('c', tea.ModCtrl))
	if !idle.quitting || !has[tea.QuitMsg](drain(cmd)) {
		t.Fatal("ctrl+c while idle quits")
	}
	if _, ok := idle.Filter(idle, tea.QuitMsg{}).(tea.QuitMsg); !ok {
		t.Fatal("filter passes quit through")
	}
}

func TestBarDials(t *testing.T) {
	m, _ := newModel(t, false)
	d := m.desks[0]
	dials := m.status().Dials
	if len(dials) != 3 || dials[0] != (wm.Segment{Text: "ctx 0", Style: wm.Dim, Action: "open.metrics"}) || dials[1] != (wm.Segment{Text: m.theme.Glyphs.Models + " m-fast", Style: wm.Cool, Action: "model.switch"}) || dials[2] != (wm.Segment{Text: "default", Style: wm.Dim, Action: "mode.set"}) {
		t.Fatalf("idle dials, the context counted in tokens while the window is unknown %+v", dials)
	}
	cmd := submit(m, d, "hello")
	if l := laneOf(m, m.wm.Active()); l.Signal != wm.Working || !strings.HasSuffix(l.Detail, "s") {
		t.Fatalf("a working lane counts the time %+v", l)
	}
	feed(m, cmd)
	pump(t, m, runDone)
	d.live.Endpoint.Catalog.Context = d.live.Session.Context() * 4
	if dials := m.status().Dials; dials[0] != (wm.Segment{Text: "ctx ▰▰▱▱▱▱ 25%", Style: wm.Dim, Action: "open.metrics"}) {
		t.Fatalf("the gauge fills with the context %+v", dials[0])
	}
	d.live.Endpoint.Catalog.Context = d.live.Session.Context() * 10 / 8
	if dials := m.status().Dials; dials[0].Style != wm.Warm {
		t.Fatalf("a filling context warms the gauge %+v", dials[0])
	}
	d.live.Endpoint.Catalog.Context = d.live.Session.Context()
	if dials := m.status().Dials; dials[0] != (wm.Segment{Text: "ctx ▰▰▰▰▰▰ 100%", Style: wm.Bad, Action: "open.metrics"}) {
		t.Fatalf("a full context alarms %+v", dials[0])
	}
	if _, err := m.app.Shell.Bash().Run(context.Background(), json.RawMessage(`{"command":"sleep 30","run_in_background":true}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dialText(m), m.theme.Glyphs.Jobs+" 1") || m.status().Dials[0].Action != "open.jobs" {
		t.Fatalf("running jobs get a dial %q", dialText(m))
	}
	for _, j := range m.app.Shell.Jobs() {
		m.app.Shell.KillJob(context.Background(), j.ID)
	}
	if strings.Contains(dialText(m), m.theme.Glyphs.Jobs) {
		t.Fatal("the jobs dial leaves with the jobs")
	}
	for md, st := range map[mode.Mode]wm.SegmentStyle{mode.Default: wm.Dim, mode.AcceptEdits: wm.Warm, mode.Plan: wm.Cool, mode.Bypass: wm.Bad} {
		m.app.Mode.Set(md)
		if last := m.status().Dials[len(m.status().Dials)-1]; last.Text != md.Label() || last.Style != st {
			t.Fatalf("mode dial %+v for %s", last, md)
		}
	}
	if failed(nil) || failed(context.Canceled) || failed(fmt.Errorf("wrapped %w", agent.ErrInterrupted)) || !failed(errors.New("rate limited")) {
		t.Fatal("only errors the person did not cause count as failures")
	}
}

func TestPanelPickerAndModeCycle(t *testing.T) {
	m, _ := newModel(t, false)
	d := m.current()
	feed(m, m.wm.Focus(d.session))
	_, cmd := m.Update(key('a', tea.ModAlt))
	feed(m, cmd)
	p, ok := m.wm.Overlay().(*wm.Palette)
	if !ok || len(p.Matches()) != len(panels) {
		t.Fatalf("alt+a lists every panel, %v", m.wm.Overlay())
	}
	for _, it := range p.Matches() {
		if it.Keys != "" && it.ID != "new" {
			t.Fatalf("unbound and closed panels carry no keys %+v", it)
		}
	}
	feed(m, m.wm.Dismiss())
	m.keys.Rebind("open.metrics", "alt+o")
	feed(m, m.command(nil, "sessions", ""))
	feed(m, m.wm.Focus(d.session))
	_, cmd = m.Update(key('a', tea.ModAlt))
	feed(m, cmd)
	p = m.wm.Overlay().(*wm.Palette)
	keyed := map[string]string{}
	for _, it := range p.Matches() {
		keyed[it.ID] = it.Keys
	}
	if keyed["metrics"] != "alt+o" || keyed["sessions"] != "open" || keyed["new"] != "alt+n" {
		t.Fatalf("the window menu shows the keymap key and what is open %v", keyed)
	}
	feed(m, m.wm.Close(m.sessions))
	typeInto(m, "metr")
	ms := p.Matches()
	if len(ms) == 0 || ms[0].Title != "Metrics" {
		t.Fatalf("the picker filters %v", ms)
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	feed(m, cmd)
	if !m.wm.Has("metrics") || m.wm.Overlay() != nil {
		t.Fatal("picking opens the panel")
	}
	feed(m, m.wm.Focus(d.session))
	before := m.app.Mode.Mode()
	_, cmd = m.Update(key(tea.KeyTab, tea.ModShift))
	feed(m, cmd)
	if m.app.Mode.Mode() != before.Next() {
		t.Fatalf("shift+tab cycles the mode, %v", m.app.Mode.Mode())
	}
	feed(m, m.wm.Focus(m.metrics))
	_, cmd = m.Update(key(tea.KeyTab, tea.ModShift))
	feed(m, cmd)
	if m.app.Mode.Mode() != before.Next() {
		t.Fatal("shift+tab away from the session leaves the mode alone")
	}
	if _, ok := m.settings()["newline"]; ok {
		t.Fatal("there is no newline setting")
	}
	feed(m, m.newWorkspace())
	_, cmd = m.Update(key('W', tea.ModAlt))
	feed(m, cmd)
	if len(m.desks) != 1 {
		t.Fatalf("alt+W closes the workspace on legacy terminals, %d left", len(m.desks))
	}
}

func TestKeybindFileDrivesTheApp(t *testing.T) {
	m, _ := newModel(t, false)
	path := m.keys.Path()
	dir, _ := profiles.ConfigDir()
	if path != filepath.Join(dir, keys.FileName) || m.env.Keys != m.keys || m.wm.Keys() != m.keys {
		t.Fatalf("one keymap read from the config dir %q", path)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"open": ["alt+a"]`) {
		t.Fatalf("defaults written on first start %v", err)
	}
	for _, a := range m.keys.Actions() {
		if a.Context != keys.Global {
			continue
		}
		found := false
		for _, c := range m.wm.Commands() {
			if c.ID == a.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("global action %s has no command behind it", a.ID)
		}
	}
	os.WriteFile(path, []byte(`{"global": {"open": "alt+o", "workspace.new": "alt+a"}}`), 0o644)
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	_, cmd := m.Update(keysTickMsg{})
	msgs := drain(cmd)
	if !has[keys.ChangedMsg](msgs) || m.keys.First("open") != "alt+o" || !toasted(m, "keybinds reloaded") {
		t.Fatalf("an outside edit reloads the keybinds, open on %q", m.keys.First("open"))
	}
	if _, cmd := m.Update(keysTickMsg{}); has[keys.ChangedMsg](drain(cmd)) {
		t.Fatal("an unchanged file is left alone")
	}
	_, cmd = m.Update(key('o', tea.ModAlt))
	feed(m, cmd)
	if _, ok := m.wm.Overlay().(*wm.Palette); !ok {
		t.Fatal("the reloaded chord opens the panel picker")
	}
	feed(m, m.wm.Dismiss())
	_, cmd = m.Update(key('a', tea.ModAlt))
	feed(m, cmd)
	if len(m.desks) != 2 {
		t.Fatal("a traded chord fires its new action")
	}
	os.WriteFile(path, []byte(`{"global": {"nope": "x"}}`), 0o644)
	later := future.Add(time.Minute)
	os.Chtimes(path, later, later)
	feed(m, func() tea.Msg { return keysTickMsg{} })
	if !toasted(m, "unknown action") {
		t.Fatal("a bad edit says what it rejected")
	}
	feed(m, m.editKeys())
	tab, _ := m.config.Active()
	if m.wm.Focused() != m.config || tab.Title() != "keys" {
		t.Fatal("keys.edit opens the configuration on its keys tab")
	}
	feed(m, m.command(nil, "keys", ""))
	if m.wm.Focused() != m.config {
		t.Fatal("the keys slash command opens the editor")
	}
	_, cmd = m.Update(keys.ChangedMsg{})
	feed(m, cmd)
	os.WriteFile(path, []byte(`not json`), 0o644)
	again, err := New(context.Background(), m.opts)
	if err != nil {
		t.Fatal(err)
	}
	drain(again.Init())
	if again.keysErr == nil || !toasted(again, "keybinds: ") || again.keys.First("palette") != "alt+space" {
		t.Fatal("a broken file is reported at start and the defaults hold")
	}
	again.shutdown()
}

func TestThemePickedInThePaletteRepaintsEverything(t *testing.T) {
	m, _ := newModel(t, false)
	m.wm.SetMotion(0)
	d := m.desks[0]
	feed(m, submit(m, d, "paint me"))
	pump(t, m, runDone)
	feed(m, m.command(nil, "files", ""))
	feed(m, m.wm.Focus(d.session))
	before := m.View().Content
	fg := func(c color.Color) string {
		r, g, b, _ := c.RGBA()
		return fmt.Sprintf("38;2;%d;%d;%d", r>>8, g>>8, b>>8)
	}
	dark := theme.Blackwall()
	if trace := d.session.Transcript.View(100, 12); !strings.Contains(trace, fg(dark.Ice)) {
		t.Fatal("the transcript paints in the dark palette before the switch")
	}
	_, cmd := m.Update(key(tea.KeySpace, tea.ModAlt))
	feed(m, cmd)
	typeInto(m, "theme")
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	feed(m, cmd)
	p, ok := m.wm.Overlay().(*wm.Palette)
	if !ok || len(p.Matches()) != 4 {
		t.Fatalf("the theme picker opens %v", m.wm.Overlay())
	}
	typeInto(m, "light")
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	feed(m, cmd)
	if m.theme.Name != "light" || m.wm.Theme().Name != "light" || m.env.Theme.Name != "light" || m.wm.Overlay() != nil {
		t.Fatalf("picking light switches the theme, %q %q", m.theme.Name, m.wm.Theme().Name)
	}
	v := m.View()
	if v.Content == before || v.BackgroundColor != theme.Light().Bg {
		t.Fatal("the screen repaints in the new palette")
	}
	r, g, b, _ := theme.Light().Surface.RGBA()
	if !strings.Contains(v.Content, fmt.Sprintf("48;2;%d;%d;%d", r>>8, g>>8, b>>8)) {
		t.Fatal("card surfaces take the light surface color")
	}
	trace := d.session.Transcript.View(100, 12)
	for _, stale := range []color.Color{dark.Text, dark.Ice, dark.TextDim, dark.Accent} {
		if strings.Contains(trace, fg(stale)) {
			t.Fatalf("the transcript kept a dark color %s from its cache", fg(stale))
		}
	}
	if !strings.Contains(trace, fg(theme.Light().Ice)) {
		t.Fatal("the transcript paints in the light palette")
	}
}

func TestClearStartsAFreshSessionInPlace(t *testing.T) {
	m, _ := newModel(t, false)
	d := m.desks[0]
	before := d.live
	cmd := submit(m, d, "hello")
	if feed(m, m.command(nil, "clear", "")); d.live != before || !toasted(m, "agent still working") {
		t.Fatal("a busy session does not clear")
	}
	feed(m, cmd)
	pump(t, m, runDone)
	feed(m, m.command(nil, "findings", ""))
	old := d.live
	feed(m, m.command(nil, "clear", ""))
	if d.live == old || d.live.Session.Len() != 0 || len(m.desks) != 1 || len(m.wm.Workspaces()) != 1 || m.app.LiveByID(old.Session.ID()) != nil || m.app.Session != d.live.Session {
		t.Fatal("clear swaps in a fresh session and releases the old one")
	}
	if ok, _ := m.app.Store.Exists(context.Background(), old.Session.ID()); !ok || !toasted(m, old.Session.ID()+" stored") {
		t.Fatal("the old session stays stored")
	}
	if d.session.Session() != d.live.Session || d.session.Composer.SessionID != d.live.Session.ID() || d.session.Transcript.Blocks() != 0 || d.findings != nil || m.wm.Has("findings:"+old.Session.ID()) {
		t.Fatal("the session card follows the new session and the old findings close")
	}
	feed(m, submit(m, d, "again"))
	pump(t, m, runDone)
	if d.live.Session.Len() != 2 || d.session.Transcript.Blocks() == 0 {
		t.Fatal("the fresh session runs in the same workspace")
	}
}

// Last assistant entry of a session
func lastAnswer(s *session.Session) session.Entry {
	entries := s.Entries()
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Role == protocol.RoleAssistant {
			return entries[i]
		}
	}
	return session.Entry{}
}

func TestWorkspacesRunOnTheirOwnEndpoints(t *testing.T) {
	m, srv := newModel(t, false)
	first := m.desks[0]
	feed(m, m.command(nil, "model", "m-a"))
	feed(m, m.newWorkspace())
	second := m.desks[1]
	feed(m, m.command(nil, "model", "m-b"))
	if first.live.Endpoint.Model != "m-a" || second.live.Endpoint.Model != "m-b" || m.app.Primary.Model != "m-b" {
		t.Fatalf("each workspace keeps its model, %s and %s", first.live.Endpoint.Model, second.live.Endpoint.Model)
	}
	if !strings.Contains(dialText(m), "m-b") || !strings.Contains(m.View().WindowTitle, "m-b") {
		t.Fatal("the bar and title name the model of the workspace on the stage")
	}
	m.Update(key('1', tea.ModAlt))
	if !strings.Contains(dialText(m), "m-a") || !strings.Contains(m.View().WindowTitle, "m-a") {
		t.Fatal("switching workspaces switches the model shown")
	}
	one := submit(m, first, "one")
	m.Update(key('2', tea.ModAlt))
	two := submit(m, second, "two")
	feed(m, one)
	feed(m, two)
	done := 0
	pump(t, m, func(msg tea.Msg) bool {
		if runDone(msg) {
			done++
		}
		return done == 2
	})
	if lastAnswer(first.live.Session).Model != "m-a" || lastAnswer(second.live.Session).Model != "m-b" {
		t.Fatalf("runs go to the model of their workspace, %q and %q", lastAnswer(first.live.Session).Model, lastAnswer(second.live.Session).Model)
	}
	if err := m.app.SaveProfile("", profiles.Profile{Name: "other", Vendor: "openai", Protocol: "openai-completions", BaseURL: srv.URL, Models: []string{"m-x"}}); err != nil {
		t.Fatal(err)
	}
	m.Update(key('1', tea.ModAlt))
	busy := submit(m, first, "long")
	m.Update(key('2', tea.ModAlt))
	feed(m, m.switchProfile("other", ""))
	if second.live.Endpoint.Profile.Name != "other" || second.live.Endpoint.Model != "m-x" || first.live.Endpoint.Profile.Name != "fake" || m.app.Primary.Profile.Name != "other" {
		t.Fatalf("a workspace switches profile while another runs, %+v", second.live.Endpoint.Profile.Name)
	}
	feed(m, busy)
	pump(t, m, runDone)
	stored := second.live.Session.ID()
	feed(m, m.closeWorkspace())
	feed(m, m.switchProfile("fake", ""))
	if m.app.Primary.Profile.Name != "fake" {
		t.Fatal("the default moves back with the workspace on the stage")
	}
	feed(m, m.openStored(stored))
	if d := m.current(); d.live.Session.ID() != stored || d.live.Endpoint.Profile.Name != "other" || d.live.Endpoint.Model != "m-x" || !toasted(m, "on m-x") {
		t.Fatalf("a resumed session comes back on its own endpoint, %+v", d.live.Endpoint.Profile.Name)
	}
	feed(m, m.closeSession(m.current()))
	if err := m.app.RemoveProfile("other"); err != nil {
		t.Fatal(err)
	}
	feed(m, m.openStored(stored))
	if d := m.current(); d.live.Endpoint.Profile.Name != "fake" || d.live.Session.Labels()["profile"] != "fake" {
		t.Fatal("a session whose profile is gone resumes on the default and is relabeled")
	}
}

func TestWorkspacesSurviveARestart(t *testing.T) {
	srv, _ := fakeServer(t, false)
	cfg, dir := testConfig(t, srv.URL)
	ctx := context.Background()
	m, err := New(ctx, Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(m.Init())
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	first := m.desks[0]
	firstID := first.live.Session.ID()
	first.live.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "one"}}})
	ms, err := first.live.Missions.Create("map the ports")
	if err != nil {
		t.Fatal(err)
	}
	first.mission = ms.ID
	feed(m, m.command(first, "findings", ""))
	feed(m, m.wm.SetLayout(wm.Dwindle))
	feed(m, m.newWorkspace())
	second := m.current()
	secondID := second.live.Session.ID()
	feed(m, m.command(second, "metrics", ""))
	feed(m, m.command(second, "config", ""))
	feed(m, m.wm.ToggleFloat())
	feed(m, m.config.ShowKeys())
	_, keysTab := m.config.Active()
	second.session.Transcript.ShowThinking(false)
	run := second.session.Composer
	drain(run.SetFocus(true))
	for _, r := range "half typed" {
		run.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.Update(keysTickMsg{})
	l, _, ok := loadLayout(layoutPath(m.app.Config))
	if !ok || len(l.Workspaces) != 2 || l.Active != 1 || l.Missions[firstID] != ms.ID {
		t.Fatalf("the layout is written as it changes %+v", l)
	}
	if w := l.Workspaces[0]; w.ID != "w1" || w.Mode != "dwindle" || len(w.Tiles) != 2 || w.Tiles[0].Key != "session:"+firstID || w.Tiles[1].Key != "findings:"+firstID || w.Tree == nil {
		t.Fatalf("first workspace %+v", w)
	}
	var view struct {
		Hidden bool `json:"thinking_hidden"`
	}
	json.Unmarshal(l.Workspaces[1].Cards["session:"+secondID], &view)
	if w := l.Workspaces[1]; w.ID != "w2" || len(w.Tiles) != 2 || w.Tiles[0].Key != "session:"+secondID || w.Tiles[1].Key != "metrics" || len(w.Floats) != 1 || w.Floats[0].Key != "config" || w.Focus != "config" || !view.Hidden {
		t.Fatalf("second workspace %+v", w)
	}
	m.shutdown()
	if _, err := m.app.Store.Load(ctx, secondID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("a session nothing was sent on is not written, %v", err)
	}
	again, err := New(ctx, Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(again.Init())
	again.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if len(again.desks) != 2 || again.wm.ActiveIndex() != 1 || len(again.app.Lives()) != 2 || again.app.Current() != again.desks[1].live {
		t.Fatalf("both workspaces come back with the second shown, %d desks active %d lives %d", len(again.desks), again.wm.ActiveIndex(), len(again.app.Lives()))
	}
	one, two := again.desks[0], again.desks[1]
	if one.live.Session.ID() != firstID || one.live.Session.Len() != 1 || again.workspaceOf(one).Mode() != wm.Dwindle || !again.wm.Has("findings:"+firstID) || one.mission != ms.ID {
		t.Fatal("the first workspace comes back with its transcript, layout, findings card and mission")
	}
	if two.live.Session.ID() != secondID || two.live.Session.Len() != 0 || !again.wm.Has("metrics") || !again.wm.Has("config") {
		t.Fatal("the second workspace comes back empty under its id with its panels")
	}
	back := again.wm.Layout().Workspaces[1]
	if len(back.Floats) != 1 || back.Floats[0].Key != "config" || back.Focus != "config" {
		t.Fatalf("the config card floats with the focus %+v", back)
	}
	if _, tab := again.config.Active(); tab != keysTab {
		t.Fatalf("the config card shows the keys tab again, %d", tab)
	}
	if two.session.Transcript.Thinking() || two.session.Composer.Value() != "half typed" || two.session.Composer.SessionID != secondID {
		t.Fatalf("thinking and the draft come back %q", two.session.Composer.Value())
	}
	again.shutdown()
	if _, err := again.app.Store.Load(ctx, secondID); !errors.Is(err, session.ErrNotFound) {
		t.Fatal("a revived session nothing was sent on is still not written")
	}
	third, err := New(ctx, Options{Config: cfg, Dir: dir, Resume: firstID, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(third.Init())
	if len(third.desks) != 2 || third.wm.ActiveIndex() != 0 || third.current().live.Session.ID() != firstID {
		t.Fatalf("resuming a session shows its workspace among the restored ones, %d desks active %d", len(third.desks), third.wm.ActiveIndex())
	}
	third.shutdown()
	os.Remove(layoutPath(cfg))
	fourth, err := New(ctx, Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(fourth.Init())
	if len(fourth.desks) != 1 || fourth.current().live.Session.Len() != 0 {
		t.Fatal("without a layout a fresh session opens alone")
	}
	fourth.shutdown()
}

func TestSessionsShareAWorkspace(t *testing.T) {
	m, _ := newModel(t, true)
	m.wm.SetMotion(0)
	approval := func(msg tea.Msg) bool { _, ok := msg.(harness.ApprovalMsg); return ok }
	first := m.desks[0]
	_, cmd := m.Update(key('n', tea.ModAlt))
	feed(m, cmd)
	if len(m.desks) != 2 || len(m.wm.Workspaces()) != 1 {
		t.Fatalf("alt+n opens a second session in the same workspace, %d desks %d workspaces", len(m.desks), len(m.wm.Workspaces()))
	}
	second := m.desks[1]
	if m.wm.WorkspaceOf(second.session) != m.wm.Active() || m.wm.Focused() != second.session || m.current() != second || m.app.Session != second.live.Session {
		t.Fatal("the new session card takes the focus and the selection")
	}
	feed(m, m.command(nil, "model", "m-b"))
	if second.live.Endpoint.Model != "m-b" || first.live.Endpoint.Model != "m-fast" {
		t.Fatal("the model switch lands on the focused session")
	}
	if !strings.Contains(dialText(m), "m-b") || !strings.HasSuffix(second.session.Status(), "m-b") {
		t.Fatalf("the bar and the card status name its model %q %q", dialText(m), second.session.Status())
	}
	feed(m, m.wm.Focus(first.session))
	m.sync()
	if m.current() != first || m.app.Session != first.live.Session {
		t.Fatal("focusing the other card moves the selection")
	}
	if !strings.Contains(dialText(m), "m-fast") {
		t.Fatal("the bar follows the focused session")
	}
	feed(m, m.command(nil, "metrics", ""))
	if m.wm.Focused() != m.metrics || m.current() != first {
		t.Fatal("a focused panel leaves the last focused session current")
	}
	feed(m, m.command(nil, "rename", "mine"))
	if first.live.Session.Title() != "mine" || second.live.Session.Title() != "" {
		t.Fatal("session commands from a panel act on that session")
	}
	cmd = submit(m, second, "go")
	m.sync()
	if l := laneOf(m, m.wm.Active()); l.Signal != wm.Working || l.Name != "go" {
		t.Fatalf("the lane shows a busy session anywhere in it under the name of the last focused %+v", l)
	}
	feed(m, m.wm.Focus(second.session))
	if feed(m, m.wm.CloseFocused()); !m.wm.Has(second.session.Key()) || len(m.desks) != 2 || second.session.Closable() {
		t.Fatal("a busy session card refuses to close")
	}
	if feed(m, m.closeSession(second)); len(m.desks) != 2 || !toasted(m, "agent still working") {
		t.Fatal("the close command refuses a busy session too")
	}
	feed(m, cmd)
	pump(t, m, approval)
	if m.current() != second || m.wm.WorkspaceOf(second.approval) != m.wm.Active() {
		t.Fatal("the approval dialog opens beside its session and counts as it")
	}
	feed(m, m.answer(policy.Yes)())
	pump(t, m, runDone)
	m.sync()
	if laneOf(m, m.wm.Active()).Signal != wm.Idle {
		t.Fatal("the chip settles")
	}
	id := second.live.Session.ID()
	feed(m, m.wm.Focus(second.session))
	feed(m, m.wm.CloseFocused())
	if len(m.desks) != 1 || m.app.LiveByID(id) != nil || m.wm.Has("session:"+id) || !toasted(m, "stored go") {
		t.Fatal("closing the card stores and releases the session")
	}
	if ok, _ := m.app.Store.Exists(context.Background(), id); !ok {
		t.Fatal("the closed session is on disk")
	}
	if m.current() != first || m.app.Session != first.live.Session {
		t.Fatal("the remaining session is current")
	}
	feed(m, m.wm.Close(m.metrics))
	feed(m, m.wm.Close(first.session))
	if len(m.desks) != 0 || m.current() != nil || len(m.wm.Workspaces()) != 1 || toasted(m, "stored mine") {
		t.Fatal("the last session card closes too without a toast for a session that holds nothing, leaving the workspace")
	}
	if dials := m.status().Dials; len(dials) != 2 || !strings.HasSuffix(dials[0].Text, " "+m.app.Primary.Model) || laneOf(m, m.wm.Active()) != (wm.Lane{}) {
		t.Fatalf("the bar falls back to the default endpoint %+v", dials)
	}
	if text := ansi.Strip(m.View().Content); !strings.Contains(text, "alt+n new session") || !strings.Contains(text, "alt+a open panel") {
		t.Fatal("an empty workspace invites a session or a panel")
	}
	m.Update(cards.SetModelMsg{Model: "m-z"})
	if m.app.Primary.Model != "m-z" || !toasted(m, "model m-z") {
		t.Fatal("with no session the model pick sets the default")
	}
	_, cmd = m.Update(StartPrompt("hello"))
	feed(m, cmd)
	if len(m.desks) != 1 || m.desks[0].live.Endpoint.Model != "m-z" || !m.app.Busy(m.desks[0].live) || m.wm.Focused() != m.desks[0].session {
		t.Fatal("a prompt with no session opens one on the default")
	}
	pump(t, m, approval)
	feed(m, m.answer(policy.Yes)())
	pump(t, m, runDone)
	feed(m, m.wm.Focus(m.desks[0].session))
	feed(m, m.command(nil, "fork", ""))
	if len(m.desks) != 2 || len(m.wm.Workspaces()) != 1 || m.current().live.Session.Parent() != m.desks[0].live.Session.ID() {
		t.Fatal("fork opens the copy beside its source")
	}
}

func TestSharedWorkspaceSurvivesARestart(t *testing.T) {
	srv, _ := fakeServer(t, false)
	cfg, dir := testConfig(t, srv.URL)
	ctx := context.Background()
	m, err := New(ctx, Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(m.Init())
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	first := m.desks[0]
	feed(m, m.newSession())
	second := m.desks[1]
	firstID, secondID := first.live.Session.ID(), second.live.Session.ID()
	first.live.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "one"}}})
	second.live.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "two"}}})
	feed(m, m.command(second, "findings", ""))
	feed(m, m.wm.Focus(second.session))
	m.Update(keysTickMsg{})
	l, _, ok := loadLayout(layoutPath(m.app.Config))
	if !ok || len(l.Workspaces) != 1 || len(l.Workspaces[0].Tiles) != 3 || l.Workspaces[0].Focus != "session:"+secondID {
		t.Fatalf("one workspace holds both sessions and the findings %+v", l)
	}
	m.shutdown()
	again, err := New(ctx, Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(again.Init())
	again.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if len(again.desks) != 2 || len(again.wm.Workspaces()) != 1 || again.current().live.Session.ID() != secondID || again.app.Current().Session.ID() != secondID {
		t.Fatalf("both sessions come back in one workspace with the focused one selected, %d desks %d workspaces", len(again.desks), len(again.wm.Workspaces()))
	}
	if !again.wm.Has("session:"+firstID) || !again.wm.Has("findings:"+secondID) || again.desks[0].live.Session.Len() != 1 || again.desks[1].live.Session.Len() != 1 {
		t.Fatal("every card comes back with its transcript")
	}
	again.shutdown()
	third, err := New(ctx, Options{Config: cfg, Dir: dir, Resume: firstID, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(third.Init())
	if len(third.desks) != 2 || len(third.wm.Workspaces()) != 1 || third.current().live.Session.ID() != firstID || third.wm.Focused() != third.current().session {
		t.Fatal("resuming a session already in the layout focuses its card")
	}
	third.shutdown()
	legacy := fmt.Sprintf(`{"active":1,"workspaces":[{"id":%q,"tiles":[{"key":"session:%s"},{"key":"metrics"}]},{"id":%q,"tiles":[{"key":"session:%s"}]}]}`, firstID, firstID, secondID, secondID)
	os.WriteFile(layoutPath(cfg), []byte(legacy), 0o644)
	fourth, err := New(ctx, Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(fourth.Init())
	if len(fourth.desks) != 2 || len(fourth.wm.Workspaces()) != 2 || fourth.wm.ActiveIndex() != 1 || fourth.current().live.Session.ID() != secondID || !fourth.wm.Has("metrics") {
		t.Fatal("a layout from before sessions shared workspaces comes back by its session keys")
	}
	fourth.shutdown()
	os.WriteFile(layoutPath(cfg), []byte(`{"workspaces":[{"id":"w1","tiles":[{"key":"metrics"}]}]}`), 0o644)
	fifth, err := New(ctx, Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(fifth.Init())
	if len(fifth.desks) != 0 || len(fifth.wm.Workspaces()) != 1 || fifth.current() != nil || len(fifth.app.Lives()) != 0 || !fifth.wm.Has("metrics") {
		t.Fatalf("a workspace saved without sessions comes back without one, %d desks %d lives", len(fifth.desks), len(fifth.app.Lives()))
	}
	fifth.shutdown()
}

func TestBackgroundWorkWaitsInTheField(t *testing.T) {
	m, _ := newModel(t, true)
	m.wm.SetMotion(0)
	first := m.desks[0]
	cmd := submit(m, first, "run it")
	feed(m, m.newWorkspace())
	ws1, ws2 := m.wm.Workspaces()[0], m.wm.Workspaces()[1]
	feed(m, cmd)
	pump(t, m, func(msg tea.Msg) bool { _, ok := msg.(harness.ApprovalMsg); return ok })
	if m.wm.Active() != ws2 || m.wm.WorkspaceOf(first.approval) != ws1 || ws1.Focused() != wm.Card(first.approval) {
		t.Fatal("an approval in a hidden workspace leaves the stage alone and waits as the focus of its own")
	}
	if l := laneOf(m, ws1); l != (wm.Lane{Name: "run it", Signal: wm.Blocked, Detail: "Bash"}) {
		t.Fatalf("blocked lane %+v", l)
	}
	if n := m.status().Notice; n != (wm.Notice{Text: "approve Bash echo hi ∷ 1 run it", Signal: wm.Blocked, Action: "attention.next"}) {
		t.Fatalf("the field raises the approval %+v", n)
	}
	m.app.Approver.Answer(first.approval.Pending()[0].ID, policy.Yes)
	pump(t, m, runDone)
	if l := laneOf(m, ws1); m.wm.Active() != ws2 || l.Signal != wm.Fresh || l.Detail != "" {
		t.Fatalf("a run ending out of view leaves a fresh lane %+v", l)
	}
	n := m.status().Notice
	if n.Signal != wm.Fresh || !strings.HasPrefix(n.Text, "done in ") || !strings.HasSuffix(n.Text, " with 1 tool ∷ 1 run it") {
		t.Fatalf("the field raises the finished run %+v", n)
	}
	feed(m, m.wm.Run("attention.next"))
	if m.wm.Active() != ws1 || m.wm.Focused() != wm.Card(first.session) {
		t.Fatal("serving the notice shows the session")
	}
	if laneOf(m, ws1).Signal != wm.Idle || m.status().Notice.Text != "" || m.wm.Run("attention.next") != nil {
		t.Fatal("visiting the workspace marks the run seen and empties the queue")
	}
}

func TestFailedRunWaitsInTheField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad request"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	cfg, dir := testConfig(t, srv.URL)
	m, err := New(context.Background(), Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(m.Init())
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.wm.SetMotion(0)
	first := m.desks[0]
	cmd := submit(m, first, "doomed")
	feed(m, m.newWorkspace())
	feed(m, cmd)
	pump(t, m, runDone)
	ws1 := m.wm.Workspaces()[0]
	if l := laneOf(m, ws1); l.Signal != wm.Failed || l.Name != "doomed" {
		t.Fatalf("a run failing out of view leaves a failed lane %+v", l)
	}
	if n := m.status().Notice; n.Signal != wm.Failed || !strings.HasPrefix(n.Text, "failed ") || !strings.HasSuffix(n.Text, "∷ 1 doomed") {
		t.Fatalf("the field raises the failure %+v", n)
	}
	feed(m, m.wm.Run("attention.next"))
	if m.wm.Active() != ws1 || laneOf(m, ws1).Signal != wm.Idle {
		t.Fatal("serving the failure shows its session and marks it seen")
	}
}

func TestSubagentsOpenAsideAndCountOnTheBar(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		json.Unmarshal(body, &req)
		tooled := slices.ContainsFunc(req.Messages, func(m struct {
			Role string `json:"role"`
		}) bool {
			return m.Role == "tool"
		})
		w.Header().Set("Content-Type", "text/event-stream")
		if !tooled && strings.Contains(string(body), "spawn") {
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Agent\",\"arguments\":\"{\\\"prompt\\\":\\\"greet\\\",\\\"description\\\":\\\"greeting\\\"}\"}}]},\"finish_reason\":null}]}\n\n", req.Model)
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n", req.Model)
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		if !tooled {
			<-release
		}
		fmt.Fprintf(w, "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":null}]}\n\n", req.Model)
		fmt.Fprintf(w, "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n", req.Model)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	cfg, dir := testConfig(t, srv.URL)
	cfg.Permissions.Allow = append(cfg.Permissions.Allow, "Agent")
	m, err := New(context.Background(), Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	drain(m.Init())
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.wm.SetMotion(0)
	first := m.desks[0]
	cmd := submit(m, first, "spawn")
	feed(m, m.newWorkspace())
	ws1, ws2 := m.wm.Workspaces()[0], m.wm.Workspaces()[1]
	feed(m, cmd)
	pump(t, m, func(msg tea.Msg) bool {
		e, ok := msg.(harness.EventMsg)
		return ok && e.Event.Type == agent.EventSubagentStart
	})
	if len(first.agents) != 1 {
		close(release)
		t.Fatalf("one agent card, got %d", len(first.agents))
	}
	for _, card := range first.agents {
		if m.wm.Active() != ws2 || m.wm.WorkspaceOf(card) != ws1 || ws1.Focused() != wm.Card(card) {
			close(release)
			t.Fatal("the card of a sub-agent of a hidden session opens aside")
		}
	}
	if d := m.status().Dials[0]; d != (wm.Segment{Text: m.theme.Glyphs.Agent + " 1", Style: wm.Cool, Action: "open.agents"}) {
		close(release)
		t.Fatalf("a running sub-agent gets a dial %+v", d)
	}
	close(release)
	pump(t, m, runDone)
	if strings.Contains(dialText(m), m.theme.Glyphs.Agent) {
		t.Fatal("the agents dial leaves when the sub-agents end")
	}
}

func TestSystemCardTunesTheSessionAndGhostStaysOffDisk(t *testing.T) {
	m, _ := newModel(t, false)
	m.wm.SetMotion(0)
	d := m.desks[0]
	feed(m, submit(m, d, "hello"))
	pump(t, m, runDone)
	m.app.Saver.Wait()
	id := d.live.Session.ID()
	if ok, _ := m.app.Store.Exists(context.Background(), id); !ok {
		t.Fatal("the session is stored before ghost")
	}
	feed(m, m.command(nil, "system", ""))
	if m.wm.Focused() != wm.Card(d.system) || m.wm.WorkspaceOf(d.system) != m.wm.Active() || m.deskOfCard(d.system) != d {
		t.Fatal("/system opens the editor of the session beside it")
	}
	if c, _ := m.wm.Find("system:" + id); c != wm.Card(d.system) || m.cardFor("system:"+id) != wm.Card(d.system) {
		t.Fatal("the editor is found by its key")
	}
	os.WriteFile(cards.DraftPath(m.configDir(), id), []byte("unsent"), 0o600)
	feed(m, func() tea.Msg { return cards.TuneMsg{Session: id, Tuning: harness.Tuning{Ghost: true, Bare: true}} })
	if ok, _ := m.app.Store.Exists(context.Background(), id); ok {
		t.Fatal("turning ghost removes the stored copy")
	}
	if _, err := os.Stat(cards.DraftPath(m.configDir(), id)); !os.IsNotExist(err) {
		t.Fatal("turning ghost removes the draft")
	}
	if !toasted(m, "ghost on, hello kept off disk") || !strings.Contains(d.session.Status(), "ghost "+m.theme.Glyphs.Sep+" bare") {
		t.Fatalf("the change is named and the card shows it %q", d.session.Status())
	}
	feed(m, submit(m, d, "again"))
	pump(t, m, runDone)
	m.app.Saver.Wait()
	if ok, _ := m.app.Store.Exists(context.Background(), id); ok {
		t.Fatal("a ghost session stays off disk after its turns")
	}
	feed(m, func() tea.Msg { return cards.TuneMsg{Session: id, Tuning: harness.Tuning{}} })
	m.app.Saver.Wait()
	if ok, _ := m.app.Store.Exists(context.Background(), id); !ok || !toasted(m, "ghost off, hello saved to disk") {
		t.Fatal("leaving ghost writes the session again")
	}
	feed(m, func() tea.Msg { return cards.TuneMsg{Session: id, Tuning: harness.Tuning{Ghost: true}} })
	feed(m, m.closeSession(d))
	if m.app.LiveByID(id) != nil {
		t.Fatal("closing a ghost session discards it")
	}
	if ok, _ := m.app.Store.Exists(context.Background(), id); ok {
		t.Fatal("a discarded ghost session leaves nothing behind")
	}
	if feed(m, func() tea.Msg { return cards.TuneMsg{Session: id} }); !toasted(m, "session closed") {
		t.Fatal("tuning a closed session says so")
	}
}
