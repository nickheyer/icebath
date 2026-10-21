package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

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
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/deck"
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
		harness.FactMsg{Change: facts.Change{Op: facts.OpAdd, Fact: facts.Fact{ID: "f1"}}},
		harness.MissionMsg{Mission: mission.Mission{ID: "m1"}},
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

// Runs a command and collects the messages it produces
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
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

func newModel(t *testing.T, toolFirst bool) (*Model, *httptest.Server) {
	t.Helper()
	srv, _ := fakeServer(t, toolFirst)
	cfg, dir := testConfig(t, srv.URL)
	m, err := New(context.Background(), Options{Config: cfg, Dir: dir, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
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
			m.Update(msg)
			if until(msg) {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("message never arrived")
}

func TestModelRunsPromptsAndRoutesMessages(t *testing.T) {
	m, _ := newModel(t, false)
	if m.deck.Len() < 6 || m.app == nil || m.state != tea.ProgramStateIdle {
		t.Fatal("deck seeded")
	}
	drain(m.Init())
	v := m.View()
	if !v.AltScreen || v.ProgramStatus == nil || v.ProgramStatus.State != tea.ProgramStateIdle {
		t.Fatal("view status")
	}
	_, cmd := m.Update(StartPrompt("hello"))
	drain(cmd)
	if m.state != tea.ProgramStateWorking || m.app.Session.Title() != "hello" {
		t.Fatalf("state %v title %q", m.state, m.app.Session.Title())
	}
	if m.View().ProgressBar == nil {
		t.Fatal("busy app shows an indeterminate bar")
	}
	pump(t, m, func(msg tea.Msg) bool { _, ok := msg.(harness.RunDoneMsg); return ok })
	if m.state != tea.ProgramStateDone || m.app.Session.Len() != 2 {
		t.Fatalf("state %v len %d", m.state, m.app.Session.Len())
	}
	if len(m.deck.Toasts()) == 0 {
		t.Fatal("finished run toasts")
	}
	_, cmd = m.Update(cards.SubmitMsg{Parts: []protocol.Part{protocol.Text{Text: "second"}}, Text: "second"})
	drain(cmd)
	_, cmd = m.Update(cards.SubmitMsg{Parts: []protocol.Part{protocol.Text{Text: "steer"}}, Text: "steer"})
	drain(cmd)
	pump(t, m, func(msg tea.Msg) bool { _, ok := msg.(harness.RunDoneMsg); return ok })
	if m.app.Session.Len() < 4 {
		t.Fatalf("len %d", m.app.Session.Len())
	}
	_, cmd = m.Update(cards.CommandMsg{Name: "mode"})
	drain(cmd)
	if m.app.Mode.Mode() != mode.AcceptEdits {
		t.Fatal("mode cycles")
	}
	m.Update(cards.CommandMsg{Name: "mode", Args: "plan"})
	if m.app.Mode.Mode() != mode.Plan {
		t.Fatal("mode set")
	}
	m.Update(cards.CommandMsg{Name: "mode", Args: "weird"})
	m.Update(cards.SetModeMsg{Mode: mode.Default})
	if m.app.Mode.Mode() != mode.Default {
		t.Fatal("set mode message")
	}
	m.Update(cards.CommandMsg{Name: "model", Args: "m-other"})
	if m.app.Primary.Model != "m-other" {
		t.Fatal("model command")
	}
	m.Update(cards.SetModelMsg{Model: "m-fast"})
	if m.app.Primary.Model != "m-fast" {
		t.Fatal("model message")
	}
	m.Update(cards.CommandMsg{Name: "theme", Args: "light"})
	if m.theme.Name != "light" || m.deck.Theme().Name != "light" {
		t.Fatal("theme command")
	}
	m.Update(cards.CommandMsg{Name: "theme", Args: "nope"})
	m.Update(cards.KnobMsg{Key: "theme", Value: "plain"})
	if m.theme.Name != "plain" {
		t.Fatal("theme knob")
	}
	m.Update(cards.KnobMsg{Key: "motion", Value: "0"})
	if m.deck.Motion() != 0 {
		t.Fatal("motion knob")
	}
	m.Update(cards.KnobMsg{Key: "compact_threshold", Value: "500"})
	if m.app.Meter.Snapshot().Threshold != 500 {
		t.Fatal("threshold knob")
	}
	m.Update(cards.KnobMsg{Key: "mode", Value: "plan"})
	m.Update(cards.KnobMsg{Key: "max_tokens", Value: "bad"})
	m.Update(cards.KnobMsg{Key: "max_tokens", Value: "2048"})
	if m.knobs()["max_tokens"] != "2048" || m.knobs()["temperature"] != "model default" {
		t.Fatalf("%v", m.knobs())
	}
	for _, name := range []string{"sessions", "findings", "config", "jobs", "workspace", "daemons", "ice", "help", "palette", "timeline", "export", "nope", "cancel", "interrupt", "compare", "mission"} {
		m.Update(cards.CommandMsg{Name: name})
	}
	if m.deck.Mode() != deck.Timeline {
		t.Fatal("timeline command")
	}
	if _, err := os.Stat(filepath.Join(m.app.Root, "transcript-"+m.app.Session.ID()+".md")); err != nil {
		t.Fatal("export writes the transcript")
	}
	m.Update(cards.PaletteMsg{Command: "theme contrast"})
	if m.theme.Name != "contrast" {
		t.Fatal("palette command")
	}
	m.Update(cards.ExportMsg{Path: "p"})
	m.Update(cards.ExportMsg{Err: errors.New("x")})
	m.Update(cards.AnswerMsg{ID: 99})
	m.Update(cards.KillRunMsg{Run: "nope"})
	m.Update(cards.KillJobMsg{Job: "nope"})
	m.Update(cards.OpenRunMsg{Run: "nope"})
	m.Update(cards.OpenRunMsg{Run: "run-1"})
	m.Update(cards.ReconnectMsg{Server: "nope"})
	m.Update(cards.MissionStateMsg{ID: "nope", Status: mission.StatusDone})
	m.Update(cards.ResumeMsg{Session: "nope"})
	m.Update(cards.ForkMsg{Session: "nope"})
	m.Update(cards.DeleteMsg{Session: m.app.Session.ID()})
	m.Update(cards.SetProfileMsg{Profile: "nope"})
	m.Update(harness.SavedMsg{Err: errors.New("disk")})
	m.Update(harness.MCPMsg{Server: "s", Err: errors.New("x")})
	m.Update(harness.MCPMsg{Server: "s", Tools: []string{"a"}})
	m.Update(harness.MCPMsg{Server: "s"})
	m.Update(harness.HookOutputMsg{Text: "hook"})
	m.Update(harness.FactMsg{Change: facts.Change{Op: facts.OpAdd, Fact: facts.Fact{Title: "t"}}})
	m.Update(harness.ProgressMsg{})
	m.Update(errMsg{errors.New("e")})
	m.Update(errMsg{})
	m.Update(compactedMsg{})
	first := m.app.Session.ID()
	m.Update(cards.CommandMsg{Name: "fork"})
	if m.app.Session.ID() == first || m.app.Session.Parent() != first {
		t.Fatal("fork command")
	}
	m.Update(cards.ResumeMsg{Session: first})
	if m.app.Session.ID() != first {
		t.Fatal("resume")
	}
	m.Update(cards.ForkMsg{Session: first})
	if m.app.Session.Parent() != first {
		t.Fatal("fork message")
	}
	forked := m.app.Session.ID()
	m.Update(cards.ResumeMsg{Session: first})
	m.Update(cards.DeleteMsg{Session: forked})
	if ok, _ := m.app.Store.Exists(context.Background(), forked); ok {
		t.Fatal("delete")
	}
	m.Update(cards.CommandMsg{Name: "clear"})
	if m.app.Session.Len() != 0 {
		t.Fatal("clear")
	}
	m.Update(cards.RulesMsg{Set: rules.Set{Allow: []string{"Glob"}}})
	if _, err := os.Stat(filepath.Join(m.app.Root, harness.ProjectDir, harness.RulesFile)); err != nil {
		t.Fatal("rules saved")
	}
	m.Update(cards.MissionMsg{Objective: "scan"})
	pump(t, m, func(msg tea.Msg) bool { _, ok := msg.(harness.RunDoneMsg); return ok })
	if len(m.missions) != 1 || m.mission == "" {
		t.Fatal("mission card")
	}
	for id := range m.missions {
		m.Update(cards.MissionStateMsg{ID: id, Status: mission.StatusDone})
		m.Update(cards.MissionStateMsg{ID: id, Status: mission.StatusActive})
	}
	m.View()
	m.Update(cards.CommandMsg{Name: "history"})
	if !m.inHistory || m.history == nil || m.history.Mode() != deck.Timeline {
		t.Fatal("history deck")
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.View()
	m.Update(cards.CommandMsg{Name: "compare"})
	for _, c := range m.history.Cards() {
		if mc, ok := c.(*cards.MomentCard); ok {
			mc.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
		}
	}
	m.Update(cards.CommandMsg{Name: "compare"})
	if m.history.Mode() != deck.Split {
		t.Fatal("compare splits the history deck")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.inHistory {
		t.Fatal("esc leaves history")
	}
	m.Update(cards.CommandMsg{Name: "history"})
	n := m.app.Session.Len()
	m.Update(cards.ForkAtMsg{Entries: 1})
	if m.inHistory || m.app.Session.Len() != 1 || n < 1 {
		t.Fatal("fork at")
	}
	m.Update(tea.KeyPressMsg{Code: 'k', Mod: tea.ModCtrl})
	if m.deck.Overlay() == nil {
		t.Fatal("palette key")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m.Update(tea.KeyPressMsg{Code: tea.KeyF1})
	if m.deck.Overlay() == nil {
		t.Fatal("help key")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	m.Update(cards.CommandMsg{Name: "compact"})
	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if _, ok := drain(cmd)[0].(tea.QuitMsg); !ok || !m.quitting {
		t.Fatal("ctrl+c quits when idle")
	}
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if _, err := os.Stat(m.layoutPath()); err != nil {
		t.Fatal("layout saved on quit")
	}
}

func TestModelApprovalsChildrenAndCancel(t *testing.T) {
	m, _ := newModel(t, true)
	drain(m.Init())
	m.Update(StartPrompt("run it"))
	pump(t, m, func(msg tea.Msg) bool { _, ok := msg.(harness.ApprovalMsg); return ok })
	if m.deck.Overlay() != m.approval || m.state != tea.ProgramStateBlocked || m.View().ProgramStatus.Kind != tea.ProgramStatusKindPermission {
		t.Fatal("approval overlay")
	}
	pending := m.app.Approver.Pending()
	if len(pending) != 1 {
		t.Fatalf("%d pending", len(pending))
	}
	m.Update(cards.AnswerMsg{ID: pending[0].ID, Verdict: policy.Yes})
	pump(t, m, func(msg tea.Msg) bool { _, ok := msg.(harness.RunDoneMsg); return ok })
	if m.state != tea.ProgramStateDone {
		t.Fatalf("%v", m.state)
	}
	if len(m.app.Workspace.Files()) != 0 || m.app.Shell.Dir() == "" {
		t.Fatal("workspace")
	}
	m.Update(StartPrompt("again"))
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	drain(cmd)
	if m.quitting {
		t.Fatal("ctrl+c during a run cancels instead of quitting")
	}
	pump(t, m, func(msg tea.Msg) bool { _, ok := msg.(harness.RunDoneMsg); return ok })
	if m.state != tea.ProgramStateError && m.state != tea.ProgramStateDone {
		t.Fatalf("%v", m.state)
	}
	m.Update(harness.EventMsg{Run: "r", Event: agent.Event{Type: agent.EventSubagentStart, Call: protocol.ToolCall{Name: "kid"}}})
	drain(m.deck.Focus(1))
	m.Update(harness.RunDoneMsg{Run: "r", Err: errors.New("boom")})
	if m.state != tea.ProgramStateError || m.deck.UrgencyOf(m.deck.IndexOf(m.transcript)) != deck.Alarm {
		t.Fatal("failed run alarms the transcript")
	}
	m.Update(harness.RunDoneMsg{Run: "r", Err: agent.ErrInterrupted})
	m.Update(harness.ApprovalDoneMsg{})
	child := &agent.Agent{Name: "kid", Session: session.New()}
	m.app.Control = harness.NewController(m.relay.Send, m.app.Pump)
	m.env.Control = m.app.Control
	_ = child
	if _, ok := m.Filter(m, tea.QuitMsg{}).(tea.QuitMsg); !ok || !m.quitting {
		t.Fatal("quit message shuts down")
	}
}

func TestThemeResolutionAndLayout(t *testing.T) {
	t.Setenv("TERM", "dumb")
	th, err := resolveTheme(harness.Config{}, "")
	if err != nil || th.Name != "plain" {
		t.Fatalf("%v %s", err, th.Name)
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	th, _ = resolveTheme(harness.Config{}, "")
	if th.Name != "blackwall" {
		t.Fatal(th.Name)
	}
	if _, err := resolveTheme(harness.Config{UI: harness.UIConfig{Theme: "nope"}}, ""); err == nil {
		t.Fatal("bad theme")
	}
	r := &relay{}
	r.Send("queued")
	if len(r.queue) != 1 {
		t.Fatal("queue before attach")
	}
	r.Close()
	r.Send("dropped")
	if len(r.queue) != 0 {
		t.Fatal("closed relay drops")
	}
	if StartMission("x").(cards.MissionMsg).Objective != "x" {
		t.Fatal("start mission")
	}
}

// Program options for an event loop with no terminal behind it
func headlessProgram(ctx context.Context, m tea.Model, extra ...tea.ProgramOption) *tea.Program {
	opts := append([]tea.ProgramOption{tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignals(), tea.WithoutSignalHandler(), tea.WithWindowSize(120, 40)}, extra...)
	return tea.NewProgram(m, opts...)
}

// Model that sends through the relay from inside its own Update and quits once the message comes back
type relayModel struct {
	r    *relay
	seen []string
}

func (m *relayModel) Init() tea.Cmd { return func() tea.Msg { return "kick" } }

func (m *relayModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	s, ok := msg.(string)
	if !ok {
		return m, nil
	}
	m.seen = append(m.seen, s)
	switch s {
	case "kick":
		m.r.Send("first")
		m.r.Send("second")
	case "second":
		return m, tea.Quit
	}
	return m, nil
}

func (m *relayModel) View() tea.View { return tea.NewView("") }

func TestRelayNeverBlocksTheEventLoop(t *testing.T) {
	r := &relay{}
	r.Send("early")
	m := &relayModel{r: r}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := headlessProgram(ctx, m)
	r.Attach(p)
	if _, err := p.Run(); err != nil {
		t.Fatalf("a send from inside Update stalled the program: %v", err)
	}
	if got := strings.Join(m.seen, " "); got != "early kick first second" {
		t.Fatalf("order %q", got)
	}
}

func TestClearCommandOverALiveProgram(t *testing.T) {
	m, _ := newModel(t, false)
	first := m.app.Session.ID()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := headlessProgram(ctx, m, tea.WithFilter(m.Filter))
	m.Attach(p)
	idle := func() bool {
		m.relay.mu.Lock()
		defer m.relay.mu.Unlock()
		return len(m.relay.queue) == 0 && !m.relay.pumping
	}
	settled := func() {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) && !m.app.Control.Busy() {
			time.Sleep(2 * time.Millisecond)
		}
		for time.Now().Before(deadline) && (m.app.Control.Busy() || !idle()) {
			time.Sleep(2 * time.Millisecond)
		}
	}
	go func() {
		p.Send(StartPrompt("hello"))
		settled()
		p.Send(cards.CommandMsg{Name: "clear"})
		p.Send(cards.CommandMsg{Name: "fork"})
		p.Send(cards.ResumeMsg{Session: first})
		p.Send(cards.CommandMsg{Name: "mission", Args: "scan the repo"})
		settled()
		p.Send(cards.CommandMsg{Name: "quit"})
	}()
	if _, err := p.Run(); err != nil {
		t.Fatalf("the event loop stalled: %v", err)
	}
	if !m.quitting || m.app.Session.ID() != first || m.app.Session.Len() < 4 {
		t.Fatalf("quitting %v session %s wanted %s with %d entries", m.quitting, m.app.Session.ID(), first, m.app.Session.Len())
	}
	if m.app.Missions.Len() != 1 || len(m.missions) != 1 {
		t.Fatalf("missions %d cards %d", m.app.Missions.Len(), len(m.missions))
	}
}

func TestCompactionBlocksSessionChanges(t *testing.T) {
	m, _ := newModel(t, false)
	drain(m.Init())
	m.compacting = true
	first := m.app.Session.ID()
	for _, msg := range []tea.Msg{cards.SubmitMsg{Parts: []protocol.Part{protocol.Text{Text: "hi"}}, Text: "hi"}, cards.CommandMsg{Name: "clear"}, cards.CommandMsg{Name: "fork"}, cards.CommandMsg{Name: "compact"}, cards.CommandMsg{Name: "mission", Args: "x"}, cards.ResumeMsg{Session: first}, cards.ForkMsg{Session: first}, cards.ForkAtMsg{Entries: 0}} {
		m.Update(msg)
		if m.app.Session.ID() != first || m.app.Control.Busy() || m.app.Missions.Len() != 0 {
			t.Fatalf("%T changed the session while compacting", msg)
		}
	}
	m.Update(compactedMsg{Err: errors.New("summary failed")})
	if m.compacting {
		t.Fatal("a failed compaction releases the guard")
	}
	m.Update(cards.CommandMsg{Name: "compact"})
	if !m.compacting {
		t.Fatal("compact arms the guard")
	}
	pump(t, m, func(msg tea.Msg) bool { _, ok := msg.(compactedMsg); return ok })
	if m.compacting {
		t.Fatal("a finished compaction releases the guard")
	}
	m.Update(cards.CommandMsg{Name: "clear"})
	if m.app.Session.ID() == first {
		t.Fatal("clear works again")
	}
}

// Runs a command with every batch member in parallel and returns the messages that arrive within the wait
func collect(cmd tea.Cmd, wait time.Duration) []tea.Msg {
	out := make(chan tea.Msg, 64)
	var wg sync.WaitGroup
	var walk func(c tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := c()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, cc := range batch {
					walk(cc)
				}
				return
			}
			if msg != nil {
				out <- msg
			}
		}()
	}
	walk(cmd)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	var msgs []tea.Msg
	timer := time.After(wait)
	for {
		select {
		case m := <-out:
			msgs = append(msgs, m)
		case <-done:
			for {
				select {
				case m := <-out:
					msgs = append(msgs, m)
				default:
					return msgs
				}
			}
		case <-timer:
			return msgs
		}
	}
}

func TestProfileSwitchOpensOffTheEventLoop(t *testing.T) {
	m, _ := newModel(t, false)
	if len(collect(m.switchProfile(" ", ""), time.Second)) == 0 {
		t.Fatal("an empty name toasts")
	}
	_, cmd := m.Update(cards.SetProfileMsg{Profile: "fake", Model: "m-other"})
	var opened profileMsg
	found := false
	for _, msg := range collect(cmd, 5*time.Second) {
		if p, ok := msg.(profileMsg); ok {
			opened, found = p, true
		}
	}
	if !found || opened.Err != nil {
		t.Fatalf("found %v err %v", found, opened.Err)
	}
	if m.app.Primary.Model != "m-fast" {
		t.Fatal("nothing changes before the endpoint is adopted")
	}
	m.Update(opened)
	if m.app.Primary.Model != "m-other" || m.app.Session.Model() != "m-other" || m.app.Config.Profile != "fake" || m.app.Config.Model != "m-other" {
		t.Fatalf("%+v", m.app.Primary)
	}
	_, cmd = m.Update(cards.CommandMsg{Name: "profile", Args: "nope"})
	for _, msg := range collect(cmd, 5*time.Second) {
		if p, ok := msg.(profileMsg); ok {
			if p.Err == nil {
				t.Fatal("unknown profile opens")
			}
			m.Update(p)
		}
	}
	if m.app.Primary.Model != "m-other" {
		t.Fatal("a failed open leaves the endpoint alone")
	}
}

func TestKillJobAndReconnectRunOffTheEventLoop(t *testing.T) {
	m, _ := newModel(t, false)
	res, err := m.app.Shell.Bash().Run(context.Background(), json.RawMessage(`{"command":"sleep 30","run_in_background":true}`))
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	jobs := m.app.Shell.Jobs()
	if len(jobs) != 1 || !jobs[0].Running {
		t.Fatalf("%+v", jobs)
	}
	id := jobs[0].ID
	_, cmd := m.Update(cards.KillJobMsg{Job: id})
	killed := false
	for _, msg := range collect(cmd, 10*time.Second) {
		if k, ok := msg.(jobKilledMsg); ok && k.Job == id {
			killed = true
			m.Update(k)
		}
	}
	if !killed {
		t.Fatal("the kill reports back")
	}
	if jb, ok := m.app.Shell.Job(id); !ok || jb.Running {
		t.Fatalf("%+v", jb)
	}
	for _, msg := range []tea.Msg{cards.KillJobMsg{Job: "nope"}, cards.ReconnectMsg{Server: "nope"}} {
		_, cmd := m.Update(msg)
		failed := false
		for _, got := range collect(cmd, 5*time.Second) {
			if _, ok := got.(errMsg); ok {
				failed = true
			}
		}
		if !failed {
			t.Fatalf("%T reports its error", msg)
		}
	}
}

func TestApprovalLeavesHistory(t *testing.T) {
	m, _ := newModel(t, false)
	drain(m.Init())
	m.Update(StartPrompt("hello"))
	pump(t, m, func(msg tea.Msg) bool { _, ok := msg.(harness.RunDoneMsg); return ok })
	m.Update(cards.CommandMsg{Name: "history"})
	if !m.inHistory {
		t.Fatal("history opens")
	}
	go m.app.Approver.Ask(context.Background(), policy.Request{Call: protocol.ToolCall{Name: "Bash"}})
	pump(t, m, func(msg tea.Msg) bool { _, ok := msg.(harness.ApprovalMsg); return ok })
	if m.inHistory || m.history != nil || m.deck.Overlay() != m.approval {
		t.Fatal("an approval leaves history and shows the overlay")
	}
	pending := m.app.Approver.Pending()
	if len(pending) != 1 || !m.app.Approver.Answer(pending[0].ID, policy.No) {
		t.Fatal("approval answered")
	}
}

func TestSlashCommandsCoverPaletteActions(t *testing.T) {
	m, _ := newModel(t, false)
	seen := map[string]bool{}
	for _, c := range m.slashCommands() {
		if seen[c.Name] || strings.Contains(c.Name, " ") {
			t.Fatalf("duplicate or spaced %q", c.Name)
		}
		seen[c.Name] = true
	}
	for _, want := range []string{"attach", "model", "jobs", "workspace", "daemons", "ice", "timeline", "compare", "palette", "clear", "quit"} {
		if !seen[want] {
			t.Fatalf("missing /%s", want)
		}
	}
}
