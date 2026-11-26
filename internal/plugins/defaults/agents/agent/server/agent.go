package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/internal/sessions"
	sessionsconfig "github.com/nickheyer/icebath/internal/sessions/config"
	"github.com/nickheyer/icebath/internal/slash"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/transport"
	agentv1 "github.com/nickheyer/icebath/pkg/gen/icebath/plugins/agent/v1"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

type Agent struct {
	ctx    context.Context
	cancel context.CancelFunc

	config      *config.Config
	providers   *provider.Registry
	transports  *transport.Registry
	credentials *credentials.Registry
	models      *models.Registry
	tools       *tools.Registry
	sessions    *sessions.Registry
	slash       *slash.Registry

	mu      sync.Mutex
	runs    map[string]*run
	working sync.WaitGroup
}

type run struct {
	id string

	mu        sync.Mutex
	watchers  map[chan *agentv1.WatchResponse]struct{}
	queue     []string
	busy      bool
	stop      context.CancelFunc
	text      strings.Builder
	reasoning strings.Builder
	approval  *agentv1.Approval
	answer    chan error
	allowed   map[string]bool
	yolo      bool
	provider  string
	model     string
	effort    string
	// Notices of the session, kept so a watcher that comes later, the first one of a new session included, still sees them
	notices []notice
}

// Notice and the count of messages the session held when it was given, which places it among them on a replay
type notice struct {
	after int
	text  string
}

// Notices a session keeps at most, the oldest dropped first
const keptNotices = 200

func (a *Agent) Close() error {
	a.mu.Lock()
	a.cancel()
	a.mu.Unlock()
	a.working.Wait()
	return nil
}

func (a *Agent) Send(ctx context.Context, id, text string) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	if text = strings.TrimSpace(text); text == "" {
		return connect.NewError(connect.CodeInvalidArgument, "nothing to send")
	}
	if command, argument, ok := a.slash.Command(text); ok {
		note, err := command.Run(ctx, &slash.Call{SessionID: id, Argument: argument})
		if err != nil {
			note = "Error: " + err.Error()
		}
		if note != "" {
			r.mu.Lock()
			a.notify(r, note)
			r.mu.Unlock()
		}
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queue = append(r.queue, text)
	if r.busy {
		a.notify(r, "queued for the next turn")
		return nil
	}
	return a.start(r)
}

func (a *Agent) Stop(id string) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		r.stop()
	}
	return nil
}

func (a *Agent) Watch(ctx context.Context, id string, send func(*agentv1.WatchResponse) error) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	store, err := a.store()
	if err != nil {
		return err
	}
	events := make(chan *agentv1.WatchResponse, 1024)
	state := a.state(r)
	r.mu.Lock()
	messages, err := store.Messages(id)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	pending := []*agentv1.WatchResponse{state}
	noted := 0
	for i, message := range messages {
		for ; noted < len(r.notices) && r.notices[noted].after <= i; noted++ {
			pending = append(pending, noticeEvent(r.notices[noted].text))
		}
		pending = append(pending, messageEvent(message))
	}
	for ; noted < len(r.notices); noted++ {
		pending = append(pending, noticeEvent(r.notices[noted].text))
	}
	if r.busy {
		pending = append(pending, busyEvent(true))
	}
	if r.text.Len()+r.reasoning.Len() > 0 {
		pending = append(pending, deltaEvent(r.text.String(), r.reasoning.String()))
	}
	if r.approval != nil {
		pending = append(pending, approvalEvent(r.approval))
	}
	r.watchers[events] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.watchers, events)
		r.mu.Unlock()
	}()
	for _, event := range pending {
		if err := send(event); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.ctx.Done():
			return connect.NewError(connect.CodeUnavailable, "shutting down")
		case event, ok := <-events:
			if !ok {
				return connect.NewError(connect.CodeAborted, "fell behind the session")
			}
			if err := send(event); err != nil {
				return err
			}
		}
	}
}

func (a *Agent) store() (sessions.Store, error) {
	return a.sessions.Store(a.config.GetString(sessionsconfig.Store))
}

func (a *Agent) run(id string) (*run, error) {
	store, err := a.store()
	if err != nil {
		return nil, err
	}
	if _, err := store.Get(id); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.runs[id]
	if r == nil {
		r = &run{id: id, watchers: map[chan *agentv1.WatchResponse]struct{}{}, allowed: map[string]bool{}}
		a.runs[id] = r
	}
	return r, nil
}

func (a *Agent) start(r *run) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil {
		return connect.NewError(connect.CodeUnavailable, "shutting down")
	}
	r.busy = true
	r.publish(busyEvent(true))
	a.working.Add(1)
	go a.work(r)
	return nil
}

func (a *Agent) work(r *run) {
	defer a.working.Done()
	for {
		r.mu.Lock()
		if len(r.queue) == 0 || a.ctx.Err() != nil {
			r.busy = false
			r.publish(busyEvent(false))
			r.mu.Unlock()
			return
		}
		text := r.queue[0]
		r.queue = r.queue[1:]
		ctx, stop := context.WithCancel(a.ctx)
		r.stop = stop
		r.mu.Unlock()
		err := a.turn(ctx, r, text)
		r.mu.Lock()
		r.text.Reset()
		r.reasoning.Reset()
		switch {
		case ctx.Err() != nil:
			a.notify(r, "stopped")
		case err != nil:
			a.notify(r, "Error: "+err.Error())
		}
		r.mu.Unlock()
		stop()
	}
}

func (a *Agent) turn(ctx context.Context, r *run, text string) error {
	store, err := a.store()
	if err != nil {
		return err
	}
	session, err := store.Get(r.id)
	if err != nil {
		return err
	}
	if session.GetTitle() == "" {
		line, _, _ := strings.Cut(text, "\n")
		title := []rune(line)
		if err := store.Rename(r.id, string(title[:min(len(title), 60)])); err != nil {
			return err
		}
	}
	if err := a.append(store, r, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_USER, Text: text}); err != nil {
		return err
	}
	env := &tools.Env{Cwd: session.GetDirectory(), Config: a.config, Approve: func(ctx context.Context, action, reason string) error {
		return a.ask(ctx, r, action, reason)
	}}
	for turns := 0; ; turns++ {
		if limit := a.config.GetInt(agentconfig.MaxTurns); limit > 0 && turns == limit {
			return fmt.Errorf("stopped after %d turns", limit)
		}
		messages, err := store.Messages(r.id)
		if err != nil {
			return err
		}
		profile, route, err := a.route(r)
		if err != nil {
			return err
		}
		if route.Model == "" {
			return fmt.Errorf("provider %s has no model set: pick one with /model", route.Provider)
		}
		client, err := a.transports.Client(route)
		if err != nil {
			return err
		}
		response, err := a.complete(ctx, r, client, &provider.Request{
			Model:     route.Model,
			Messages:  append([]*sessionsv1.Message{{Role: sessionsv1.Role_ROLE_SYSTEM, Text: a.prompt(session)}}, messages...),
			Tools:     a.tools.Definitions(env),
			MaxTokens: profile.DefaultMaxTokens,
			Reasoning: provider.Reasoning{Effort: a.effort(r)},
			SessionID: r.id,
		})
		if err != nil {
			return err
		}
		reply := response.Message
		switch response.FinishReason {
		case "length", "content_filter":
			reply.ToolCalls = nil
			if reply.GetText() != "" || reply.GetReasoning() != "" {
				if err := a.append(store, r, reply); err != nil {
					return err
				}
			}
			return fmt.Errorf("reply cut off: %s", response.FinishReason)
		}
		if err := a.append(store, r, reply); err != nil {
			return err
		}
		if len(reply.GetToolCalls()) == 0 {
			return nil
		}
		for _, call := range reply.GetToolCalls() {
			result, failed := "interrupted", true
			if ctx.Err() == nil {
				result = a.tools.Dispatch(ctx, &tools.Call{Name: call.GetName(), RawArgs: call.GetArguments(), ToolCallID: call.GetId(), SessionID: r.id, Env: env})
				failed = tools.Failed(result)
			}
			if err := a.append(store, r, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_TOOL, ToolCallId: call.GetId(), Text: result, Failed: failed}); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func (a *Agent) complete(ctx context.Context, r *run, client provider.Client, request *provider.Request) (*provider.Response, error) {
	for attempt := 0; ; attempt++ {
		streamed := false
		response, err := client.Stream(ctx, request, func(event provider.StreamEvent) error {
			streamed = true
			switch event.Kind {
			case provider.EventTextDelta:
				r.delta(event.Text, "")
			case provider.EventReasoningDelta:
				r.delta("", event.Text)
			}
			return nil
		})
		if err == nil {
			return response, nil
		}
		failure, ok := errors.AsType[*provider.APIError](err)
		if !ok || !failure.Retryable() || streamed || attempt >= a.config.GetInt(agentconfig.APIMaxRetries) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(max(failure.RetryAfter, time.Second<<attempt)):
		}
	}
}

func (a *Agent) route(r *run) (provider.Profile, provider.Route, error) {
	r.mu.Lock()
	name, model := r.provider, r.model
	r.mu.Unlock()
	profile, err := a.profile(cmp.Or(name, a.config.GetString(agentconfig.Provider)))
	if err != nil {
		return profile, provider.Route{}, err
	}
	route, err := a.credentials.Route(profile, cmp.Or(model, a.config.GetString(agentconfig.Model)))
	return profile, route, err
}

func (a *Agent) profile(name string) (provider.Profile, error) {
	if name != "" {
		profile, ok := a.providers.Profile(name)
		if !ok {
			return profile, fmt.Errorf("unknown provider %q", name)
		}
		return profile, nil
	}
	wires := a.transports.Factories()
	for _, profile := range a.providers.Profiles() {
		if _, wired := wires[profile.Mode]; !wired || profile.Hidden || profile.BaseURL == "" || providerconfig.IsRemoved(a.config, profile.Name) {
			continue
		}
		// A local server needs no credential, so having one proves nothing: it is used only when chosen with /model --provider
		if _, ok := a.credentials.Credential(profile.Name); ok && !profile.Local {
			return profile, nil
		}
	}
	return provider.Profile{}, errors.New("no provider has a credential: the providers screen lists them and where to get one, or choose a local server with /model --provider lmstudio")
}

func (a *Agent) state(r *run) *agentv1.WatchResponse {
	_, route, _ := a.route(r)
	effort := a.effort(r)
	r.mu.Lock()
	defer r.mu.Unlock()
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_State{State: &agentv1.State{
		Provider:        route.Provider,
		Model:           route.Model,
		ReasoningEffort: effort,
		Yolo:            r.yolo,
	}}}
}

func (a *Agent) changed(r *run) {
	state := a.state(r)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.publish(state)
}

func (a *Agent) effort(r *run) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cmp.Or(r.effort, a.config.GetString(agentconfig.ReasoningEffort))
}

func (a *Agent) prompt(session *sessionsv1.Session) string {
	prompt := fmt.Sprintf(`You are icebath, an agent working for its user through tools on their machine.

Working directory: %s
Platform: %s/%s
Date: %s

Do the work with your tools instead of describing it, and keep going until the request is done. Read a file before you change it. Report what you did and what failed as it is.`,
		session.GetDirectory(), runtime.GOOS, runtime.GOARCH, time.Now().Format(time.DateOnly))
	if rules, err := os.ReadFile(filepath.Join(session.GetDirectory(), "AGENTS.md")); err == nil {
		prompt += "\n\nProject instructions from AGENTS.md:\n\n" + string(rules[:min(len(rules), a.config.GetInt(agentconfig.ContextFileMaxChars))])
	}
	return prompt
}

func (a *Agent) append(store sessions.Store, r *run, message *sessionsv1.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := store.Append(r.id, message); err != nil {
		return err
	}
	r.text.Reset()
	r.reasoning.Reset()
	r.publish(messageEvent(message))
	return nil
}

func (a *Agent) ask(ctx context.Context, r *run, action, reason string) error {
	r.mu.Lock()
	if r.yolo || r.allowed[reason] || a.config.GetString(agentconfig.ApprovalsMode) == agentconfig.ApprovalsOff || slices.Contains(a.config.GetStringSlice(agentconfig.ApprovalsAllowlist), reason) {
		r.mu.Unlock()
		return nil
	}
	answer := make(chan error, 1)
	r.approval, r.answer = &agentv1.Approval{Command: action, Reason: reason}, answer
	r.publish(approvalEvent(r.approval))
	r.mu.Unlock()
	var err error
	select {
	case err = <-answer:
	case <-time.After(time.Duration(a.config.GetInt(agentconfig.ApprovalsTimeout)) * time.Second):
		err = errors.New("denied: nobody answered in time")
	case <-ctx.Done():
		err = ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() == nil {
		select {
		case err = <-answer:
		default:
		}
	}
	r.approval, r.answer = nil, nil
	return err
}

func (r *run) delta(text, reasoning string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.text.WriteString(text)
	r.reasoning.WriteString(reasoning)
	r.publish(deltaEvent(text, reasoning))
}

func (r *run) publish(event *agentv1.WatchResponse) {
	for events := range r.watchers {
		select {
		case events <- event:
		default:
			delete(r.watchers, events)
			close(events)
		}
	}
}

// Keeps the notice for later watchers and shows it to those watching now, r.mu held
func (a *Agent) notify(r *run, text string) {
	after := 0
	if store, err := a.store(); err == nil {
		if messages, err := store.Messages(r.id); err == nil {
			after = len(messages)
		}
	}
	r.notices = append(r.notices, notice{after: after, text: text})
	if len(r.notices) > keptNotices {
		r.notices = r.notices[len(r.notices)-keptNotices:]
	}
	r.publish(noticeEvent(text))
}

// Drops the notices given after the first keep messages, for a session cut back to them, r.mu held
func (r *run) forget(keep int) {
	r.notices = slices.DeleteFunc(r.notices, func(n notice) bool { return n.after > keep })
}

func messageEvent(message *sessionsv1.Message) *agentv1.WatchResponse {
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_Message{Message: message}}
}

func deltaEvent(text, reasoning string) *agentv1.WatchResponse {
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_Delta{Delta: &agentv1.Delta{Text: text, Reasoning: reasoning}}}
}

func approvalEvent(approval *agentv1.Approval) *agentv1.WatchResponse {
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_Approval{Approval: approval}}
}

func noticeEvent(text string) *agentv1.WatchResponse {
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_Notice{Notice: text}}
}

func busyEvent(busy bool) *agentv1.WatchResponse {
	return &agentv1.WatchResponse{Event: &agentv1.WatchResponse_Busy{Busy: busy}}
}
