package vsc

import (
	"context"
	"encoding/json"
	"io"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/policy"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/protocol"
)

// Environment variable the scripted profile reads its base URL from
const testBaseEnv = "VSC_TEST_BASE"

// One scripted assistant turn the fake endpoint returns
type turnSpec struct {
	Text     string          `json:"text"`
	ToolID   string          `json:"tool_id"`
	ToolName string          `json:"tool_name"`
	ToolArgs json.RawMessage `json:"tool_args"`
	Stop     string          `json:"stop"`
}

// Fake wire protocol whose endpoint returns a scripted turn as one JSON object
type testProto struct{}

func (testProto) ID() protocol.ID { return "vsc-test" }

func (testProto) Encode(req protocol.Request) (protocol.Call, error) {
	body, err := json.Marshal(req)
	return protocol.Call{Method: http.MethodPost, Path: "/turn", Body: body}, err
}

func (testProto) Decode(r io.Reader) (protocol.Response, error) { return protocol.Response{}, nil }

func (testProto) DecodeError(status int, body []byte) *protocol.Error {
	return protocol.Classify(&protocol.Error{Status: status, Message: string(body)})
}

func (testProto) Stream(r io.Reader) iter.Seq2[protocol.Event, error] {
	return func(yield func(protocol.Event, error) bool) {
		data, err := io.ReadAll(r)
		if err != nil {
			yield(protocol.Event{}, err)
			return
		}
		var spec turnSpec
		if err := json.Unmarshal(data, &spec); err != nil {
			yield(protocol.Event{}, err)
			return
		}
		b := &protocol.Builder{}
		b.Start("resp-1", "scripted")
		if spec.Text != "" {
			b.OpenText("t")
			b.Delta("t", spec.Text)
			b.Close("t")
		}
		if spec.ToolName != "" {
			b.OpenToolCall("c", spec.ToolID, spec.ToolName)
			b.Delta("c", string(spec.ToolArgs))
			b.Close("c")
		}
		stop := protocol.StopEnd
		if spec.Stop == "tool_use" {
			stop = protocol.StopToolUse
		}
		b.Finish(stop, spec.Stop, protocol.Usage{Input: 5, Output: 3})
		for _, ev := range b.Events() {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

var registerOnce sync.Once

// Registers the scripted protocol and profile once for every test in the package
func registerScripted(t *testing.T) {
	t.Helper()
	registerOnce.Do(func() {
		protocol.Register(testProto{})
		profiles.Register(profiles.Profile{
			Name:       "vsctest",
			Protocol:   "vsc-test",
			BaseURL:    "http://127.0.0.1:1",
			BaseURLEnv: testBaseEnv,
			Auth:       profiles.AuthNone,
			Models:     []string{"scripted"},
		})
	})
}

// Fake endpoint that answers each turn from the script, given the role of the last message
func scriptServer(t *testing.T, answer func(lastRole string) turnSpec) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role string `json:"Role"`
			} `json:"Messages"`
		}
		json.Unmarshal(body, &req)
		last := ""
		if n := len(req.Messages); n > 0 {
			last = req.Messages[n-1].Role
		}
		json.NewEncoder(w).Encode(answer(last))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Live app wired to feed every message into the agent and a channel the test inspects
type fixture struct {
	app   *harness.App
	agent *Agent
	msgs  chan any
}

func newFixture(t *testing.T, answer func(lastRole string) turnSpec) *fixture {
	t.Helper()
	registerScripted(t)
	t.Setenv(profiles.ConfigDirEnv, t.TempDir())
	srv := scriptServer(t, answer)
	t.Setenv(testBaseEnv, srv.URL)
	project := t.TempDir()
	cfg, err := harness.Discover(project)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profile, cfg.Model = "vsctest", "scripted"
	cfg.SessionsDir = t.TempDir()
	f := &fixture{msgs: make(chan any, 1024)}
	send := func(msg any) {
		if f.agent != nil {
			f.agent.Observe(msg)
		}
		select {
		case f.msgs <- msg:
		default:
		}
	}
	app, err := harness.Build(context.Background(), cfg, send, harness.BuildOptions{Dir: project})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close(context.Background()) })
	f.app = app
	f.agent = NewAgent(app, Options{Version: "9.9"})
	return f
}

// Client half of the connection with channels the test reads updates and state from
type testClient struct {
	peer    *Peer
	updates chan SessionNotification
	states  chan StateNotification
}

// Connects a client whose handlers are in place before the agent sends its first line
func (f *fixture) connect(t *testing.T) *testClient {
	t.Helper()
	a, b := net.Pipe()
	c := &testClient{peer: newPeer(b), updates: make(chan SessionNotification, 1024), states: make(chan StateNotification, 1024)}
	t.Cleanup(func() { c.peer.Close() })
	c.peer.HandleNotify(MethodUpdate, func(params json.RawMessage) {
		var n SessionNotification
		json.Unmarshal(params, &n)
		push(c.updates, n)
	})
	c.peer.HandleNotify(MethodState, func(params json.RawMessage) {
		var n StateNotification
		json.Unmarshal(params, &n)
		push(c.states, n)
	})
	c.peer.Start()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.agent.Serve(ctx, a)
	return c
}

func push[T any](ch chan T, v T) {
	select {
	case ch <- v:
	default:
	}
}

func (c *testClient) initialize(t *testing.T) InitializeResponse {
	t.Helper()
	var resp InitializeResponse
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.peer.Call(ctx, MethodInitialize, map[string]any{"protocolVersion": 1}, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// Sends a prompt in the background and returns the channel its reply lands on
func (c *testClient) promptAsync(t *testing.T, sessionID, text string) chan PromptResponse {
	t.Helper()
	done := make(chan PromptResponse, 1)
	go func() {
		var resp PromptResponse
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		c.peer.Call(ctx, MethodPrompt, PromptRequest{SessionID: sessionID, Prompt: []ContentBlock{{Type: "text", Text: text}}}, &resp)
		done <- resp
	}()
	return done
}

func waitUpdate(t *testing.T, c *testClient, variant string) SessionNotification {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		select {
		case n := <-c.updates:
			if n.Update.SessionUpdate == variant {
				return n
			}
		case <-deadline:
			t.Fatalf("no %s update arrived", variant)
		}
	}
}

// First update carrying a diff, with its variant and status
func waitDiff(t *testing.T, c *testClient) SessionUpdate {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		select {
		case n := <-c.updates:
			if len(n.Update.Content) > 0 {
				return n.Update
			}
		case <-deadline:
			t.Fatal("no diff arrived")
		}
	}
}

// First tool call update with the status
func waitStatus(t *testing.T, c *testClient, status string) SessionUpdate {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		select {
		case n := <-c.updates:
			if n.Update.Status == status {
				return n.Update
			}
		case <-deadline:
			t.Fatalf("no update with status %s arrived", status)
		}
	}
}

func waitState(t *testing.T, c *testClient, match func(StateNotification) bool) StateNotification {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		select {
		case s := <-c.states:
			if match(s) {
				return s
			}
		case <-deadline:
			t.Fatal("no matching state arrived")
		}
	}
}

func waitReply(t *testing.T, done chan PromptResponse) PromptResponse {
	t.Helper()
	select {
	case resp := <-done:
		return resp
	case <-time.After(8 * time.Second):
		t.Fatal("prompt did not return")
	}
	return PromptResponse{}
}

func waitPending(t *testing.T, f *fixture) *harness.Pending {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		if p := f.app.Approver.Pending(); len(p) > 0 {
			return p[0]
		}
		select {
		case <-deadline:
			t.Fatal("no pending approval")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func textTurn(string) turnSpec { return turnSpec{Text: "hello from the model", Stop: "stop"} }

// Answers one Write of the content to the path, then done
func writeTurn(path, content string) func(string) turnSpec {
	return func(last string) turnSpec {
		if last == "tool" {
			return turnSpec{Text: "done", Stop: "stop"}
		}
		args, _ := json.Marshal(map[string]string{"file_path": path, "content": content})
		return turnSpec{ToolID: "call-1", ToolName: "Write", ToolArgs: args, Stop: "tool_use"}
	}
}

func TestInitialize(t *testing.T) {
	f := newFixture(t, textTurn)
	c := f.connect(t)
	resp := c.initialize(t)
	if resp.ProtocolVersion != 1 || resp.SessionID != f.app.Current().Session.ID() || resp.Pid != os.Getpid() || resp.Version != "9.9" {
		t.Fatalf("initialize %+v", resp)
	}
	if len(resp.Modes.AvailableModes) != 4 || resp.Modes.CurrentModeID != string(policy.ModeDefault) {
		t.Fatalf("modes %+v", resp.Modes)
	}
}

func TestPromptRuns(t *testing.T) {
	f := newFixture(t, textTurn)
	c := f.connect(t)
	c.initialize(t)
	id := f.app.Current().Session.ID()
	done := c.promptAsync(t, id, "do the thing")
	if resp := waitReply(t, done); resp.StopReason != StopEndTurn {
		t.Fatalf("stop reason %q", resp.StopReason)
	}
	waitState(t, c, func(s StateNotification) bool { return s.SessionID == id && !s.Running })
	if entries := f.app.Current().Session.Entries(); len(entries) < 2 {
		t.Fatalf("session holds %d entries", len(entries))
	}
	if f.app.Current().Session.Title() != "do the thing" {
		t.Fatalf("title %q", f.app.Current().Session.Title())
	}
}

func TestProposedDiffThenAllowed(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.txt")
	f := newFixture(t, writeTurn(target, "written"))
	c := f.connect(t)
	c.initialize(t)
	id := f.app.Current().Session.ID()
	done := c.promptAsync(t, id, "write it")
	up := waitDiff(t, c)
	if up.SessionUpdate != UpdateToolCall || up.Status != StatusPending || up.ToolCallID != "call-1" {
		t.Fatalf("proposal %+v", up)
	}
	if d := up.Content[0]; d.Path != target || d.NewText != "written" || d.OldText != nil {
		t.Fatalf("proposal diff %+v", d)
	}
	waitState(t, c, func(s StateNotification) bool { return s.SessionID == id && s.Waiting })
	f.app.Approver.Answer(waitPending(t, f).ID, policy.Yes)
	up = waitStatus(t, c, StatusCompleted)
	if up.SessionUpdate != UpdateToolCallMod || up.ToolCallID != "call-1" || len(up.Content) != 1 || up.Content[0].NewText != "written" || up.Content[0].OldText != nil {
		t.Fatalf("completion %+v", up)
	}
	waitReply(t, done)
	if data, err := os.ReadFile(target); err != nil || string(data) != "written" {
		t.Fatalf("file %q err %v", data, err)
	}
}

func TestProposedDiffThenDenied(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.txt")
	f := newFixture(t, writeTurn(target, "written"))
	c := f.connect(t)
	c.initialize(t)
	done := c.promptAsync(t, f.app.Current().Session.ID(), "write it")
	waitDiff(t, c)
	f.app.Approver.Answer(waitPending(t, f).ID, policy.No)
	up := waitStatus(t, c, StatusFailed)
	if up.SessionUpdate != UpdateToolCallMod || up.ToolCallID != "call-1" || len(up.Content) != 0 {
		t.Fatalf("failure %+v", up)
	}
	waitReply(t, done)
	if _, err := os.Stat(target); err == nil {
		t.Fatal("denied write reached the disk")
	}
}

func TestAcceptEditsDiff(t *testing.T) {
	target := filepath.Join(t.TempDir(), "edit.txt")
	os.WriteFile(target, []byte("before"), 0o644)
	f := newFixture(t, writeTurn(target, "after"))
	f.app.Mode.Set(policy.ModeAcceptEdits)
	c := f.connect(t)
	c.initialize(t)
	done := c.promptAsync(t, f.app.Current().Session.ID(), "edit it")
	up := waitDiff(t, c)
	if up.SessionUpdate != UpdateToolCallMod || up.Status != StatusCompleted {
		t.Fatalf("a change made without asking should arrive completed and only then, got %+v", up)
	}
	if d := up.Content[0]; d.Path != target || d.NewText != "after" || d.OldText == nil || *d.OldText != "before" {
		t.Fatalf("diff content %+v", d)
	}
	waitReply(t, done)
}

func TestProposalOnConnect(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.txt")
	f := newFixture(t, writeTurn(target, "written"))
	live := f.app.Current()
	if _, err := f.app.RunOn(context.Background(), live, "", []protocol.Part{protocol.Text{Text: "write it"}}); err != nil {
		t.Fatal(err)
	}
	pending := waitPending(t, f)
	c := f.connect(t)
	c.initialize(t)
	up := waitDiff(t, c)
	if up.Status != StatusPending || up.ToolCallID != pending.Request.Call.ID {
		t.Fatalf("proposal after connect %+v", up)
	}
	f.app.Approver.Answer(pending.ID, policy.No)
	waitStatus(t, c, StatusFailed)
}

func TestSetModeAndTuiMode(t *testing.T) {
	f := newFixture(t, textTurn)
	c := f.connect(t)
	c.initialize(t)
	id := f.app.Current().Session.ID()
	var resp struct{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.peer.Call(ctx, MethodSetMode, SetModeRequest{SessionID: id, ModeID: string(policy.ModeAcceptEdits)}, &resp); err != nil {
		t.Fatal(err)
	}
	if f.app.Mode.Mode() != policy.ModeAcceptEdits {
		t.Fatalf("mode is %q", f.app.Mode.Mode())
	}
	up := waitUpdate(t, c, UpdateModeChanged)
	if up.Update.CurrentModeID != string(policy.ModeAcceptEdits) {
		t.Fatalf("mode update %q", up.Update.CurrentModeID)
	}
	f.app.Mode.Set(policy.ModePlan)
	up = waitUpdate(t, c, UpdateModeChanged)
	if up.Update.CurrentModeID != string(policy.ModePlan) {
		t.Fatalf("tui mode update %q", up.Update.CurrentModeID)
	}
}

func TestCancelDuringPrompt(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, func(last string) turnSpec {
		<-release
		return turnSpec{Text: "late", Stop: "stop"}
	})
	t.Cleanup(func() { close(release) })
	c := f.connect(t)
	c.initialize(t)
	id := f.app.Current().Session.ID()
	done := c.promptAsync(t, id, "go")
	waitState(t, c, func(s StateNotification) bool { return s.Running })
	if err := c.peer.Notify(MethodCancel, CancelNotification{SessionID: id}); err != nil {
		t.Fatal(err)
	}
	if resp := waitReply(t, done); resp.StopReason != StopCancelled {
		t.Fatalf("stop reason %q", resp.StopReason)
	}
}

func TestSessionsList(t *testing.T) {
	f := newFixture(t, textTurn)
	c := f.connect(t)
	c.initialize(t)
	live := f.app.Current()
	live.Session.SetTitle("stored one")
	live.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}})
	f.app.Saver.Flush(live.Session)
	var resp SessionsListResponse
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.peer.Call(ctx, MethodSessionsList, struct{}{}, &resp); err != nil {
		t.Fatal(err)
	}
	for _, s := range resp.Sessions {
		if s.SessionID == live.Session.ID() && s.Title == "stored one" {
			return
		}
	}
	t.Fatalf("session not listed %+v", resp.Sessions)
}

func TestLoadSessionOpens(t *testing.T) {
	f := newFixture(t, textTurn)
	stored, err := f.app.Create()
	if err != nil {
		t.Fatal(err)
	}
	stored.Session.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "earlier"}}})
	f.app.Saver.Flush(stored.Session)
	if err := f.app.Release(stored); err != nil {
		t.Fatal(err)
	}
	id := stored.Session.ID()
	if f.app.LiveByID(id) != nil {
		t.Fatal("released session still open")
	}
	c := f.connect(t)
	c.initialize(t)
	var resp LoadSessionResponse
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.peer.Call(ctx, MethodLoadSession, LoadSessionRequest{SessionID: id}, &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Modes.AvailableModes) != 4 {
		t.Fatalf("modes %+v", resp.Modes)
	}
	if f.app.LiveByID(id) == nil {
		t.Fatal("load did not open the session")
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-f.msgs:
			if opened, ok := msg.(harness.OpenedMsg); ok && opened.Live.Session.ID() == id {
				return
			}
		case <-deadline:
			t.Fatal("load did not announce the session to the TUI")
		}
	}
}
