package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
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
func fakeServer(t *testing.T, toolFirst bool) (*httptest.Server, *atomic.Int32) {
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

func testConfig(t *testing.T, base string) (harness.Config, string) {
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
	if m.state != tea.ProgramStateWorking || d.live.Session.Title() != "hello" || !d.ws.Busy() {
		t.Fatalf("state %v title %q busy %v", m.state, d.live.Session.Title(), d.ws.Busy())
	}
	if m.View().ProgressBar == nil {
		t.Fatal("busy app shows an indeterminate bar")
	}
	feed(m, cmd)
	pump(t, m, runDone)
	if m.state != tea.ProgramStateDone || d.live.Session.Len() != 2 {
		t.Fatalf("state %v len %d", m.state, d.live.Session.Len())
	}
	if m.sync(); d.ws.Busy() {
		t.Fatal("chip settles after the run")
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
	if len(m.desks) != 2 || m.wm.ActiveIndex() != 1 {
		t.Fatalf("desks %d active %d", len(m.desks), m.wm.ActiveIndex())
	}
	second := m.desks[1]
	m.sync()
	if m.app.Session != second.live.Session {
		t.Fatal("switching selects the new session")
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
	if len(m.desks) != 1 || len(m.wm.Workspaces()) != 1 || m.app.LiveByID(released) != nil {
		t.Fatal("idle workspace closes and releases its session")
	}
	feed(m, m.closeWorkspace())
	if len(m.desks) != 1 || !toasted(m, "last workspace") {
		t.Fatal("the last workspace stays")
	}
	feed(m, m.openStored(released))
	if len(m.desks) != 2 || m.current().live.Session.ID() != released {
		t.Fatal("stored session reopens as a desk")
	}
	feed(m, m.openStored(released))
	if len(m.desks) != 2 {
		t.Fatal("opening an open session only switches to it")
	}
	feed(m, m.forkStored(first.live.Session.ID()))
	if len(m.desks) != 3 || m.current().live.Session.Parent() != first.live.Session.ID() {
		t.Fatal("fork makes a new desk with the parent set")
	}
	feed(m, m.wm.Switch(0))
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
	if len(m.desks) != 2 {
		t.Fatal("new opens a workspace")
	}
	feed(m, m.command(nil, "close", ""))
	if len(m.desks) != 1 {
		t.Fatal("close drops it")
	}
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
	if len(m.desks) != 3 || m.current().live.Session.ID() != b || !toasted(m, "resumed 2 sessions") {
		t.Fatalf("resuming many opens a workspace for each, the last shown, %d desks", len(m.desks))
	}
	_, cmd = m.Update(cards.OpenSessionMsg{Sessions: []string{a}})
	feed(m, cmd)
	if len(m.desks) != 3 || m.current().live.Session.ID() != a || !toasted(m, "resumed third") {
		t.Fatal("resuming an open one switches to it and names it")
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
	for len(m.desks) > 1 {
		feed(m, m.closeWorkspace())
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
	if left, _ := m.bar(); !strings.Contains(fmt.Sprint(left), "approval") {
		t.Fatalf("bar shows the wait, %v", left)
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

func TestBar(t *testing.T) {
	m, _ := newModel(t, false)
	d := m.desks[0]
	left, right := m.bar()
	if len(left) == 0 || left[0].Text != "m-fast" || len(right) != 0 {
		t.Fatalf("idle bar %v %v", left, right)
	}
	cmd := submit(m, d, "hello")
	left, right = m.bar()
	if !strings.Contains(fmt.Sprint(left), "working") || len(right) != 1 {
		t.Fatalf("busy bar %v %v", left, right)
	}
	feed(m, cmd)
	pump(t, m, runDone)
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
		if it.Keys != "" {
			t.Fatalf("unbound and closed panels carry no keys %+v", it)
		}
	}
	feed(m, m.wm.Dismiss())
	m.keys.Rebind("open.metrics", "alt+i")
	feed(m, m.command(nil, "sessions", ""))
	feed(m, m.wm.Focus(d.session))
	_, cmd = m.Update(key('a', tea.ModAlt))
	feed(m, cmd)
	p = m.wm.Overlay().(*wm.Palette)
	keyed := map[string]string{}
	for _, it := range p.Matches() {
		keyed[it.ID] = it.Keys
	}
	if keyed["metrics"] != "alt+i" || keyed["sessions"] != "open" {
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
	if d.live == old || d.live.Session.Len() != 0 || len(m.desks) != 1 || len(m.wm.Workspaces()) != 1 || d.ws.ID != d.live.Session.ID() || m.app.LiveByID(old.Session.ID()) != nil || m.app.Session != d.live.Session {
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
	if left, _ := m.bar(); !strings.Contains(fmt.Sprint(left), "m-b") || !strings.Contains(m.View().WindowTitle, "m-b") {
		t.Fatal("the bar and title name the model of the workspace on the stage")
	}
	m.Update(key('1', tea.ModAlt))
	if left, _ := m.bar(); !strings.Contains(fmt.Sprint(left), "m-a") || !strings.Contains(m.View().WindowTitle, "m-a") {
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
	feed(m, m.closeWorkspace())
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
	if w := l.Workspaces[0]; w.ID != firstID || w.Mode != "dwindle" || len(w.Tiles) != 2 || w.Tiles[1].Key != "findings:"+firstID || w.Tree == nil {
		t.Fatalf("first workspace %+v", w)
	}
	var view struct {
		Hidden bool `json:"thinking_hidden"`
	}
	json.Unmarshal(l.Workspaces[1].Cards["session:"+secondID], &view)
	if w := l.Workspaces[1]; w.ID != secondID || len(w.Tiles) != 2 || w.Tiles[1].Key != "metrics" || len(w.Floats) != 1 || w.Floats[0].Key != "config" || w.Focus != "config" || !view.Hidden {
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
	if one.live.Session.ID() != firstID || one.live.Session.Len() != 1 || one.ws.Mode() != wm.Dwindle || !again.wm.Has("findings:"+firstID) || one.mission != ms.ID {
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
