package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/internal/app"
	"github.com/nickheyer/icebath/internal/cli"
	"github.com/nickheyer/icebath/internal/client"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/plugins"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agent/config"
	"github.com/nickheyer/icebath/internal/plugins/defaults/sqlite"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/server"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsserver "github.com/nickheyer/icebath/internal/sessions/server"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1/agentv1connect"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1/sessionsv1connect"
)

type step func(request *provider.Request, fn provider.StreamFunc) (*provider.Response, error)

type model struct {
	mu       sync.Mutex
	steps    []step
	requests []*provider.Request
	routes   []provider.Route
}

func (m *model) Profiles() []provider.Profile {
	return []provider.Profile{
		{Name: "keyless", BaseURL: "https://keyless.test", Mode: "scripted", Auth: provider.AuthAPIKey, DefaultModel: "never"},
		{Name: "scripted", DisplayName: "Scripted", BaseURL: "https://scripted.test", Mode: "scripted", Auth: provider.AuthAPIKey, Aliases: []string{"fake"}, DefaultModel: "model-default"},
		{Name: "other", BaseURL: "https://other.test", Mode: "scripted", Auth: provider.AuthNone},
		{Name: "wireless", BaseURL: "https://wireless.test", Mode: "unregistered", Auth: provider.AuthNone, DefaultModel: "m"},
	}
}

func (m *model) Credential(name string) (credentials.Credential, bool) {
	return credentials.Credential{APIKey: "key", Source: "test"}, name == "scripted"
}

func (m *model) Models(_ context.Context, route provider.Route) ([]string, bool) {
	return []string{"model-default", "model-two"}, route.Provider == "scripted"
}

func (m *model) Factories() map[provider.APIMode]provider.Factory {
	return map[provider.APIMode]provider.Factory{"scripted": func(route provider.Route) (provider.Client, error) {
		return scripted{m, route}, nil
	}}
}

type scripted struct {
	model *model
	route provider.Route
}

func (s scripted) Route() provider.Route { return s.route }

func (s scripted) Stream(_ context.Context, request *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
	s.model.mu.Lock()
	s.model.requests, s.model.routes = append(s.model.requests, request), append(s.model.routes, s.route)
	if len(s.model.steps) == 0 {
		s.model.mu.Unlock()
		return nil, errors.New("the script has no reply left")
	}
	next := s.model.steps[0]
	s.model.steps = s.model.steps[1:]
	s.model.mu.Unlock()
	return next(request, fn)
}

func says(text string, calls ...*sessionsv1.ToolCall) step {
	return func(_ *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
		fn(provider.StreamEvent{Kind: provider.EventReasoningDelta, Text: "hm"})
		fn(provider.StreamEvent{Kind: provider.EventTextDelta, Text: text})
		return &provider.Response{Message: &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: text, Reasoning: "hm", ToolCalls: calls}, FinishReason: "stop"}, nil
	}
}

func fails(err error) step {
	return func(*provider.Request, provider.StreamFunc) (*provider.Response, error) { return nil, err }
}

type probe struct{}

func (probe) Tools() []tools.Def {
	return []tools.Def{{
		Name:   "probe",
		Schema: tools.Schema("Probe", []string{"text"}, "text", tools.Property("string", "Text")),
		Handler: tools.Bind(func(ctx context.Context, call *tools.Call, args struct {
			Text string `json:"text"`
			Risk string `json:"risk"`
		}) (string, error) {
			if args.Risk != "" {
				if err := call.Env.Approve(ctx, "probe "+args.Text, args.Risk); err != nil {
					return "", err
				}
			}
			return call.Env.Cwd + " probed " + args.Text + " in " + call.SessionID + " for " + call.ToolCallID, nil
		}),
	}}
}

type harness struct {
	t        *testing.T
	config   *config.Config
	model    *model
	slash    *slash.Slash
	agents   agentv1connect.AgentServiceClient
	sessions sessionsv1connect.SessionServiceClient
	id       string
	events   chan *agentv1.WatchResponse
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg := config.New(t.TempDir())
	srv, sl, s := server.New(cfg), slash.New(), sessions.New(cfg)
	registries := plugins.New(cfg, srv, app.New(), cli.New(), provider.New(), transport.New(), credentials.New(), models.New(), tools.New(cfg), s, sl)
	sessionsserver.Register(srv, cfg, s)
	sqlite.Register(registries)
	script := new(model)
	registries.Provider.Register(script)
	registries.Credentials.Register(script)
	registries.Models.Register(script)
	registries.Transport.Register(script)
	registries.Tools.Register(probe{})
	Register(registries)
	t.Cleanup(func() {
		if err := errors.Join(srv.Close(), s.Close()); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	conn := client.InProcess(srv.Services())
	h := &harness{t: t, config: cfg, model: script, slash: sl, agents: agentv1connect.NewAgentServiceClient(conn), sessions: sessionsv1connect.NewSessionServiceClient(conn), events: make(chan *agentv1.WatchResponse, 4096)}
	created, err := h.sessions.Create(t.Context(), &sessionsv1.CreateRequest{Directory: "/work/project"})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	h.id = created.GetSession().GetId()
	stream, err := h.agents.Watch(t.Context(), &agentv1.WatchRequest{SessionId: h.id})
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	go func() {
		defer stream.Close()
		for {
			event, err := stream.Receive()
			if err != nil {
				return
			}
			h.events <- event
		}
	}()
	return h
}

func (h *harness) script(steps ...step) {
	h.model.mu.Lock()
	defer h.model.mu.Unlock()
	h.model.steps = append(h.model.steps, steps...)
}

func (h *harness) send(text string) error {
	_, err := h.agents.Send(h.t.Context(), &agentv1.SendRequest{SessionId: h.id, Text: text})
	return err
}

func (h *harness) next(what string, match func(*agentv1.WatchResponse) bool) *agentv1.WatchResponse {
	h.t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case event := <-h.events:
			if match(event) {
				return event
			}
		case <-timeout:
			h.t.Fatalf("no %s event arrived", what)
		}
	}
}

func (h *harness) notice() string {
	h.t.Helper()
	return h.next("notice", func(event *agentv1.WatchResponse) bool { return event.GetNotice() != "" }).GetNotice()
}

func (h *harness) idle() {
	h.t.Helper()
	h.next("idle", func(event *agentv1.WatchResponse) bool {
		_, ok := event.GetEvent().(*agentv1.WatchResponse_Busy)
		return ok && !event.GetBusy()
	})
}

func (h *harness) command(text string) string {
	h.t.Helper()
	if err := h.send(text); err != nil {
		h.t.Fatalf("Send(%q) error = %v", text, err)
	}
	return h.notice()
}

func (h *harness) messages() []*sessionsv1.Message {
	h.t.Helper()
	response, err := h.sessions.Get(h.t.Context(), &sessionsv1.GetRequest{Id: h.id})
	if err != nil {
		h.t.Fatalf("Get() error = %v", err)
	}
	return response.GetMessages()
}

func texts(messages []*sessionsv1.Message) string {
	var out []string
	for _, message := range messages {
		out = append(out, strings.TrimPrefix(message.GetRole().String(), "ROLE_")+":"+message.GetText())
	}
	return strings.Join(out, " | ")
}

func TestTurnRunsToolsUntilTheModelIsDone(t *testing.T) {
	h := newHarness(t)
	h.script(
		says("looking", &sessionsv1.ToolCall{Id: "call-1", Name: "probe", Arguments: `{"text":"a"}`}, &sessionsv1.ToolCall{Id: "call-2", Name: "missing", Arguments: `{}`}),
		says("done"),
	)
	if err := h.send("  find it\nplease  "); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	delta := h.next("delta", func(event *agentv1.WatchResponse) bool { return event.GetDelta() != nil }).GetDelta()
	if delta.GetReasoning() != "hm" {
		t.Fatalf("first delta = %v, want the reasoning", delta)
	}
	h.idle()

	got := h.messages()
	want := "USER:find it\nplease | ASSISTANT:looking | TOOL:/work/project probed a in " + h.id + " for call-1 | TOOL:{\"error\":\"unknown tool missing\"} | ASSISTANT:done"
	if texts(got) != want {
		t.Fatalf("messages = %s\nwant       %s", texts(got), want)
	}
	if got[2].GetToolCallId() != "call-1" || got[2].GetFailed() || got[3].GetToolCallId() != "call-2" || !got[3].GetFailed() {
		t.Fatalf("tool results = %v, %v", got[2], got[3])
	}
	session, _ := h.sessions.Get(t.Context(), &sessionsv1.GetRequest{Id: h.id})
	if session.GetSession().GetTitle() != "find it" {
		t.Fatalf("title = %q, want the first line of the first message", session.GetSession().GetTitle())
	}

	h.model.mu.Lock()
	defer h.model.mu.Unlock()
	if len(h.model.requests) != 2 {
		t.Fatalf("the model was called %d times, want 2", len(h.model.requests))
	}
	first, second := h.model.requests[0], h.model.requests[1]
	if route := h.model.routes[0]; route.Provider != "scripted" || route.APIKey != "key" || route.BaseURL != "https://scripted.test" || first.Model != "model-default" {
		t.Fatalf("route = %+v with model %q, want the first provider with a credential and its default model", route, first.Model)
	}
	if system := first.Messages[0]; system.GetRole() != sessionsv1.Role_ROLE_SYSTEM || !strings.Contains(system.GetText(), "Working directory: /work/project") {
		t.Fatalf("first message = %v, want the system prompt", system)
	}
	if len(first.Messages) != 2 || len(second.Messages) != 5 || len(first.Tools) != 1 || first.Tools[0]["type"] != "function" {
		t.Fatalf("requests carried %d and %d messages and %d tools", len(first.Messages), len(second.Messages), len(first.Tools))
	}
}

func TestTurnReportsWhatStopsIt(t *testing.T) {
	h := newHarness(t)
	h.script(fails(errors.New("wire broke")))
	if got, want := h.command("go"), "Error: wire broke"; got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	h.idle()

	h.script(func(*provider.Request, provider.StreamFunc) (*provider.Response, error) {
		return &provider.Response{Message: &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: "cut"}, FinishReason: "length"}, nil
	})
	if got, want := h.command("go"), "Error: reply cut off: length"; got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	h.idle()

	if err := h.config.Set(agentconfig.MaxTurns, 1); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	h.script(says("again", &sessionsv1.ToolCall{Id: "c", Name: "probe", Arguments: `{"text":"a"}`}))
	if got, want := h.command("go"), "Error: stopped after 1 turns"; got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	h.idle()
	if err := h.config.Set(agentconfig.MaxTurns, nil); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	for provider, want := range map[string]string{
		"nowhere":  `Error: unknown provider "nowhere"`,
		"keyless":  "Error: provider keyless: no credential",
		"other":    "Error: provider other has no model set: pick one with /model",
		"wireless": `Error: provider wireless: no adapter registered for api mode "unregistered"`,
	} {
		if err := h.config.Set(agentconfig.Provider, provider); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
		if got := h.command("go"); got != want {
			t.Errorf("notice with provider %s = %q, want %q", provider, got, want)
		}
		h.idle()
	}
	if err := h.send("   "); err == nil || !strings.Contains(err.Error(), "nothing to send") {
		t.Fatalf("Send() of blank text error = %v", err)
	}
	if _, err := h.agents.Send(t.Context(), &agentv1.SendRequest{SessionId: "missing", Text: "hi"}); err == nil || !strings.Contains(err.Error(), "session not found") {
		t.Fatalf("Send() to a missing session error = %v", err)
	}
}

func TestTurnRetriesWhatCanBeRetried(t *testing.T) {
	h := newHarness(t)
	h.script(
		fails(&provider.APIError{Provider: "scripted", Model: "m", StatusCode: 503, Message: "busy", Kind: provider.ErrServer}),
		says("recovered"),
	)
	if err := h.send("go"); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	h.idle()
	if got, want := texts(h.messages()), "USER:go | ASSISTANT:recovered"; got != want {
		t.Fatalf("messages = %s, want %s", got, want)
	}

	if err := h.config.Set(agentconfig.APIMaxRetries, 0); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	h.script(fails(&provider.APIError{Provider: "scripted", Model: "m", StatusCode: 503, Message: "busy", Kind: provider.ErrServer}), says("unused"))
	if got, want := h.command("go"), "Error: scripted/m: busy (503 server)"; got != want {
		t.Fatalf("notice with retries off = %q, want %q", got, want)
	}
	h.idle()

	if err := h.config.Set(agentconfig.APIMaxRetries, 3); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	h.model.mu.Lock()
	h.model.steps = nil
	h.model.mu.Unlock()
	h.script(fails(&provider.APIError{Provider: "scripted", Model: "m", StatusCode: 401, Message: "bad key", Kind: provider.ErrAuth}), says("unused"))
	if got, want := h.command("go"), "Error: scripted/m: bad key (401 auth)"; got != want {
		t.Fatalf("notice for a failure that cannot be retried = %q, want %q", got, want)
	}
	h.idle()
}

func TestApprovals(t *testing.T) {
	h := newHarness(t)
	risky := func(id string) *sessionsv1.ToolCall {
		return &sessionsv1.ToolCall{Id: id, Name: "probe", Arguments: `{"text":"x","risk":"is risky"}`}
	}
	waiting := func() *agentv1.Approval {
		t.Helper()
		return h.next("approval", func(event *agentv1.WatchResponse) bool { return event.GetApproval() != nil }).GetApproval()
	}
	last := func() *sessionsv1.Message {
		t.Helper()
		messages := h.messages()
		return messages[len(messages)-2]
	}

	if err := h.send("/approve"); err == nil || !strings.Contains(err.Error(), "nothing waits for approval") {
		t.Fatalf("/approve with nothing waiting error = %v", err)
	}
	h.script(says("try", risky("c1")), says("ok"))
	h.send("go")
	if approval := waiting(); approval.GetCommand() != "probe x" || approval.GetReason() != "is risky" {
		t.Fatalf("approval = %v", approval)
	}
	if err := h.send("/approve nonsense"); err == nil || !strings.Contains(err.Error(), "usage: /approve [session|always]") {
		t.Fatalf("/approve nonsense error = %v", err)
	}
	if err := h.send("/deny not today"); err != nil {
		t.Fatalf("/deny error = %v", err)
	}
	h.idle()
	if got, want := last().GetText(), `{"error":"denied by the user not today"}`; got != want || !last().GetFailed() {
		t.Fatalf("denied tool result = %q, want %q", got, want)
	}

	h.script(says("try", risky("c2")), says("ok"), says("try", risky("c3")), says("ok"))
	h.send("go")
	waiting()
	if err := h.send("/approve session"); err != nil {
		t.Fatalf("/approve session error = %v", err)
	}
	h.idle()
	if last().GetFailed() {
		t.Fatalf("approved tool result = %q", last().GetText())
	}
	h.send("go")
	h.idle()
	if last().GetFailed() || last().GetToolCallId() != "c3" {
		t.Fatalf("a reason approved for the session asked again: %v", last())
	}

	other := newHarness(t)
	other.script(says("try", risky("c4")), says("ok"), says("try", risky("c5")), says("ok"), says("try", risky("c6")), says("ok"))
	other.send("go")
	other.next("approval", func(event *agentv1.WatchResponse) bool { return event.GetApproval() != nil })
	if err := other.send("/approve always"); err != nil {
		t.Fatalf("/approve always error = %v", err)
	}
	other.idle()
	if allowed := other.config.GetStringSlice(agentconfig.ApprovalsAllowlist); !slices.Equal(allowed, []string{"is risky"}) {
		t.Fatalf("allowlist = %v, want the approved reason stored", allowed)
	}
	if err := other.config.Set(agentconfig.ApprovalsAllowlist, nil); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if got, want := other.command("/yolo"), "yolo on: risky commands run without asking"; got != want {
		t.Fatalf("/yolo = %q, want %q", got, want)
	}
	other.send("go")
	other.idle()
	if messages := other.messages(); messages[len(messages)-2].GetFailed() {
		t.Fatalf("yolo still asked: %v", messages[len(messages)-2])
	}
	if got, want := other.command("/yolo"), "yolo off: risky commands ask first"; got != want {
		t.Fatalf("/yolo = %q, want %q", got, want)
	}
	if err := other.config.Set(agentconfig.ApprovalsMode, agentconfig.ApprovalsOff); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	other.send("go")
	other.idle()
	if messages := other.messages(); messages[len(messages)-2].GetFailed() {
		t.Fatalf("approvals off still asked: %v", messages[len(messages)-2])
	}
}

func TestSlashCommands(t *testing.T) {
	h := newHarness(t)
	help := h.command("/help")
	for _, name := range []string{"/help", "/title", "/retry", "/undo", "/approve", "/deny", "/yolo", "/model"} {
		if !strings.Contains(help, "`"+name) {
			t.Errorf("/help does not list %s:\n%s", name, help)
		}
	}
	if got := h.command("/help YOLO mode"); strings.Count(got, "\n") != 1 || !strings.Contains(got, "/yolo") {
		t.Fatalf("/help with a filter = %q, want only the yolo line", got)
	}
	if got, want := h.command("/help zzz"), "no command matches zzz"; got != want {
		t.Fatalf("/help zzz = %q, want %q", got, want)
	}
	if got, want := h.command("/title"), "title: "; got != want {
		t.Fatalf("/title = %q, want %q", got, want)
	}
	if got, want := h.command("/title my work"), "title set to my work"; got != want {
		t.Fatalf("/title my work = %q, want %q", got, want)
	}
	if got, want := h.command("/title"), "title: my work"; got != want {
		t.Fatalf("/title = %q, want %q", got, want)
	}

	h.script(says("one"), says("two"), says("two again"))
	h.send("first")
	h.idle()
	h.send("second")
	h.idle()
	if err := h.send("/retry"); err != nil {
		t.Fatalf("/retry error = %v", err)
	}
	rewind := h.next("rewind", func(event *agentv1.WatchResponse) bool { return event.GetRewind() != nil }).GetRewind()
	h.idle()
	if rewind.GetMessages() != 2 {
		t.Fatalf("rewind kept %d messages, want 2", rewind.GetMessages())
	}
	if got, want := texts(h.messages()), "USER:first | ASSISTANT:one | USER:second | ASSISTANT:two again"; got != want {
		t.Fatalf("messages after /retry = %s, want %s", got, want)
	}
	if err := h.send("/undo x"); err == nil || !strings.Contains(err.Error(), "usage: /undo [N]") {
		t.Fatalf("/undo x error = %v", err)
	}
	if got, want := h.command("/undo 2"), "took back: first"; got != want {
		t.Fatalf("/undo 2 = %q, want %q", got, want)
	}
	if got := h.messages(); len(got) != 0 {
		t.Fatalf("messages after /undo 2 = %s, want none", texts(got))
	}
	if err := h.send("/undo"); err == nil || !strings.Contains(err.Error(), "nothing to take back") {
		t.Fatalf("/undo with nothing left error = %v", err)
	}

	h.script(says("unknown commands reach the model"))
	h.send("/notacommand at all")
	h.idle()
	if got, want := texts(h.messages()), "USER:/notacommand at all | ASSISTANT:unknown commands reach the model"; got != want {
		t.Fatalf("messages = %s, want %s", got, want)
	}
}

func TestModelCommand(t *testing.T) {
	h := newHarness(t)
	if got, want := h.command("/model"), "model: `model-default` on scripted\n\nModels:\n\n- `model-default`\n- `model-two`\n"; got != want {
		t.Fatalf("/model = %q, want %q", got, want)
	}
	if got, want := h.command("/model model-two --reasoning high"), "model: `model-two` on scripted, reasoning high"; got != want {
		t.Fatalf("/model model-two = %q, want %q", got, want)
	}
	h.script(says("hi"))
	h.send("go")
	h.idle()
	h.model.mu.Lock()
	request := h.model.requests[len(h.model.requests)-1]
	h.model.mu.Unlock()
	if request.Model != "model-two" || request.Reasoning.Effort != "high" {
		t.Fatalf("the turn used model %q with effort %q, want the session's choice", request.Model, request.Reasoning.Effort)
	}
	if h.config.GetString(agentconfig.Model) != "" {
		t.Fatalf("a session switch stored model %q", h.config.GetString(agentconfig.Model))
	}
	if got, want := h.command("/model --provider other"), "no model set on other, reasoning high"; got != want {
		t.Fatalf("/model --provider other = %q, want %q", got, want)
	}
	if got, want := h.command("/model reset"), "model: `model-default` on scripted"; got != want {
		t.Fatalf("/model reset = %q, want %q", got, want)
	}
	if got, want := h.command("/model model-two --provider fake --global"), "model: `model-two` on scripted"; got != want {
		t.Fatalf("/model --global = %q, want %q", got, want)
	}
	if provider, model := h.config.GetString(agentconfig.Provider), h.config.GetString(agentconfig.Model); provider != "scripted" || model != "model-two" {
		t.Fatalf("stored provider %q and model %q, want the alias resolved and both stored", provider, model)
	}
	if got, want := h.command("/model reset --global"), "model: `model-default` on scripted"; got != want {
		t.Fatalf("/model reset --global = %q, want %q", got, want)
	}
	if provider, model := h.config.GetString(agentconfig.Provider), h.config.GetString(agentconfig.Model); provider != "" || model != "" {
		t.Fatalf("stored provider %q and model %q after a global reset, want none", provider, model)
	}
	for _, text := range []string{"/model a b", "/model --provider", "/model --unknown", "/model reset now"} {
		if err := h.send(text); err == nil || !strings.Contains(err.Error(), "usage: /model") {
			t.Errorf("%s error = %v, want the usage", text, err)
		}
	}
	if err := h.send("/model --provider nowhere"); err == nil || !strings.Contains(err.Error(), `unknown provider "nowhere"`) {
		t.Fatalf("/model --provider nowhere error = %v", err)
	}
}

func TestStopAndQueue(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	h.script(func(_ *provider.Request, fn provider.StreamFunc) (*provider.Response, error) {
		fn(provider.StreamEvent{Kind: provider.EventTextDelta, Text: "partial"})
		<-release
		return &provider.Response{Message: &sessionsv1.Message{Role: sessionsv1.Role_ROLE_ASSISTANT, Text: "first done"}, FinishReason: "stop"}, nil
	}, says("second done"))
	h.send("first")
	h.next("delta", func(event *agentv1.WatchResponse) bool { return event.GetDelta().GetText() == "partial" })
	if got, want := h.command("second"), "queued for the next turn"; got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	if err := h.send("/undo"); err == nil || !strings.Contains(err.Error(), "a turn is running: stop it first") {
		t.Fatalf("/undo during a turn error = %v", err)
	}

	late, err := h.agents.Watch(t.Context(), &agentv1.WatchRequest{SessionId: h.id})
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	var replay []string
	for range 3 {
		event, err := late.Receive()
		if err != nil {
			t.Fatalf("Receive() error = %v", err)
		}
		switch {
		case event.GetMessage() != nil:
			replay = append(replay, "message:"+event.GetMessage().GetText())
		case event.GetDelta() != nil:
			replay = append(replay, "delta:"+event.GetDelta().GetText())
		default:
			replay = append(replay, "busy")
		}
	}
	late.Close()
	if got, want := strings.Join(replay, " "), "message:first busy delta:partial"; got != want {
		t.Fatalf("a late watcher was sent %q, want %q", got, want)
	}

	close(release)
	h.idle()
	if got, want := texts(h.messages()), "USER:first | ASSISTANT:first done | USER:second | ASSISTANT:second done"; got != want {
		t.Fatalf("messages = %s, want %s", got, want)
	}

	blocked := make(chan struct{})
	h.script(func(*provider.Request, provider.StreamFunc) (*provider.Response, error) {
		close(blocked)
		time.Sleep(50 * time.Millisecond)
		return nil, context.Canceled
	})
	h.send("third")
	<-blocked
	if _, err := h.agents.Stop(t.Context(), &agentv1.StopRequest{SessionId: h.id}); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if got, want := h.notice(), "stopped"; got != want {
		t.Fatalf("notice = %q, want %q", got, want)
	}
	h.idle()
}
