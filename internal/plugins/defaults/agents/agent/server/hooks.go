package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	agentconfig "github.com/nickheyer/icebath/internal/plugins/defaults/agents/agent/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/sessions"
	"github.com/nickheyer/icebath/internal/tools/redact"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Text a user message is stored as once a pre_llm_call hook refused it
const withheldPrompt = "[Message withheld: a pre_llm_call policy hook blocked it.]"

// Why a turn ends when a pre_llm_call hook refused its message
const blockedExit = "blocked_by_plugin_pre_llm_call"

// What the hook points of a turn know of it, carried on the turn's context
type turning struct {
	hooks.Turn
	// What the person sent, as the hooks read it
	message string
	// Model calls of the turn so far, and the id of the last
	calls   int
	request string
	// The turn's replies are held back from the screen until the transform_llm_output hooks rewrote them
	held bool
	// Why the turn ended where it ended on its own terms, such as at the step limit; empty for an answer, a failure or a stop
	exit string
	// Reply the turn ended with as the person was shown it, empty when it ended with none
	final string
	// Why the model stopped its last reply
	finish string
	// Model the turn ran on last
	model string
	// A model request is in flight, so a stop now stops it
	calling bool
}

type turningKey struct{}

// Hooks' view of the turn the context runs, nil outside one
func turningOf(ctx context.Context) *turning {
	th, _ := ctx.Value(turningKey{}).(*turning)
	return th
}

// Starts the hooks' view of a turn: its task and turn ids on the context, so whatever fires inside the turn names them, and whether its
// replies are held back
func (a *Agent) beginTurn(ctx context.Context, r *run, settings *config.Config, session *sessionsv1.Session, surface, message string) (context.Context, *turning) {
	task := uuid.NewString()
	id := fmt.Sprintf("%s:%s:%s", cmp.Or(r.sessionID(), "session"), task, strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	th := &turning{Turn: hooks.Turn{Session: r.sessionID(), Task: task, ID: id, Platform: surface, Cwd: session.GetDirectory()}, message: message,
		held: a.hooks.Holds(settings)}
	return th.bind(ctx), th
}

// Context naming the turn and its model call as they stand
func (th *turning) bind(ctx context.Context) context.Context {
	turn := th.Turn
	turn.Request = th.request
	return context.WithValue(hooks.WithTurn(ctx, turn), turningKey{}, th)
}

// Runs the pre_llm_call hooks on the person's message: why the first that refused it did, and the context the others give the turn
func (a *Agent) preLLM(ctx context.Context, r *run, settings *config.Config, th *turning, history []*sessionsv1.Message, model string) (string, string) {
	if !a.hooks.Has(settings, hooks.PreLLMCall) {
		return "", ""
	}
	sent := hooks.Messages(sent(history))
	sent = append(sent, map[string]any{"role": "user", "content": th.message})
	var parent *string
	if c := r.subagent(); c != nil {
		parent = hooks.Optional(c.owner)
	}
	return a.hooks.PreLLMCall(ctx, settings, hooks.Prompt{Session: th.Session, Task: th.Task, Turn: th.ID, Message: th.message, History: sent,
		First: len(history) == 0, Model: model, Platform: th.Platform, Parent: parent})
}

// Puts the reply a turn ends with as the person is shown it: what the transform_llm_output hooks make of it, the model's own words kept
// for the model; the withheld notice in place of a held reply whose hooks all failed
func (a *Agent) transform(ctx context.Context, settings *config.Config, message *sessionsv1.Message) {
	th := turningOf(ctx)
	text := message.GetText()
	if text == "" || ctx.Err() != nil {
		return
	}
	shown, changed := a.hooks.TransformReply(ctx, settings, hooks.Reply{Text: text, Session: th.Session, Model: th.model, Platform: th.Platform,
		Turn: th.ID}, th.held)
	if changed {
		if message.Sent == "" {
			message.Sent = text
		}
		message.Text = shown
	}
	th.final = message.GetText()
}

// Appends the reply the turn ends with, as the person is shown it
func (a *Agent) answer(ctx context.Context, store sessions.Store, r *run, settings *config.Config, message *sessionsv1.Message) error {
	a.transform(ctx, settings, message)
	return a.append(store, r, message)
}

// Hooks of a turn's end: post_llm_call when it ended with a reply and was not stopped, then on_session_end however it ended
func (a *Agent) endTurn(ctx context.Context, r *run, settings *config.Config, th *turning, err error) {
	interrupted := ctx.Err() != nil
	ctx = context.WithoutCancel(ctx)
	failed := err != nil && !interrupted
	reason := th.exit
	switch {
	case interrupted && th.calling:
		reason = "interrupted_during_api_call"
	case interrupted:
		reason = "interrupted_by_user"
	case failed:
		reason = failedExit(err, th.calls)
	case reason == "":
		reason = fmt.Sprintf("text_response(finish_reason=%s)", cmp.Or(th.finish, "stop"))
	}
	if th.final != "" && !interrupted && a.hooks.Has(settings, hooks.PostLLMCall) {
		var history []map[string]any
		if store, serr := a.store(); serr == nil {
			if messages, merr := store.Messages(ctx, r.sessionID(), sessions.Model); merr == nil {
				history = hooks.Messages(sent(messages))
			}
		}
		a.hooks.Fire(ctx, settings, hooks.Answered{Session: th.Session, Task: th.Task, Turn: th.ID, Message: th.message, Response: th.final, History: history,
			Model: th.model, Platform: th.Platform})
	}
	a.hooks.Fire(ctx, settings, hooks.TurnEnded{Session: th.Session, Task: th.Task, Turn: th.ID, Completed: th.final != "" && !failed && !interrupted &&
		th.exit != "budget_exhausted", Failed: failed, Interrupted: interrupted, Reason: reason, Model: th.model, Platform: th.Platform})
}

// Why a turn that failed ended, in the words on_session_end tells it: a model request that got no answer however often it was sent, else a
// failure of the turn's own work after the turn's last model call, its first 80 characters
func failedExit(err error, calls int) string {
	if _, ok := errors.AsType[*provider.APIError](err); ok {
		return "all_retries_exhausted_no_response"
	}
	return "local_processing_error(" + strs.Head(fmt.Sprintf("Error during local message processing after API call #%d: %s", calls, err), 80) + ")"
}

// Ids and route of one model request of the turn, as the request hooks name it
func (th *turning) requestOf(route provider.Route, model string, started time.Time) hooks.Request {
	return hooks.Request{Task: th.Task, Turn: th.ID, ID: th.request, Session: th.Session, Platform: th.Platform, Model: model, Provider: route.Provider,
		BaseURL: route.BaseURL, Mode: string(route.Mode), Count: th.calls, Started: epoch(started)}
}

// Where a reply streams, as every stream event names it
func (th *turning) stream(route provider.Route, model string) hooks.Stream {
	return hooks.Stream{Turn: th.ID, Iteration: th.calls, Session: th.Session, Model: model, Provider: route.Provider, Surface: cmp.Or(th.Platform, "cli")}
}

// Instant in seconds since the epoch, as the hooks take times
func epoch(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

// Fires pre_api_request for one attempt of a model request
func (a *Agent) preRequest(ctx context.Context, settings *config.Config, th *turning, route provider.Route, request *provider.Request, retries int,
	started time.Time) {
	if !a.hooks.Has(settings, hooks.PreAPIRequest) {
		return
	}
	var conversation []*sessionsv1.Message
	for _, m := range request.Messages {
		if m.GetRole() != sessionsv1.Role_ROLE_SYSTEM {
			conversation = append(conversation, m)
		}
	}
	chars := hooks.Chars(request.Messages)
	var limit *int
	if request.MaxTokens > 0 {
		limit = &request.MaxTokens
	}
	a.hooks.Fire(ctx, settings, hooks.APIRequest{Request: th.requestOf(route, request.Model, started), Message: th.message,
		History: hooks.Messages(conversation), Retries: retries, Messages: hooks.Messages(request.Messages), System: hooks.System(request.Messages),
		MessageCount: len(request.Messages), ToolCount: len(request.Tools), Tokens: chars / 4, Chars: chars, MaxTokens: limit, Trace: []any{},
		Body: hooks.RequestView(settings, request)})
}

// Fires post_api_request for a model request that was answered
func (a *Agent) postRequest(ctx context.Context, settings *config.Config, th *turning, route provider.Route, request *provider.Request,
	response *provider.Response, started time.Time, first *time.Time) {
	if !a.hooks.Has(settings, hooks.PostAPIRequest) {
		return
	}
	ended := time.Now()
	var firstAt *float64
	if first != nil {
		at := epoch(*first)
		firstAt = &at
	}
	var window *int
	if n := a.window(ctx, settings, route); n > 0 {
		window = &n
	}
	message := response.Message
	assistant := hooks.Messages([]*sessionsv1.Message{message})[0]
	assistant["finish_reason"] = hooks.Optional(response.FinishReason)
	a.hooks.Fire(ctx, settings, hooks.APIResponse{Request: th.requestOf(route, request.Model, started), Duration: ended.Sub(started).Seconds(),
		Finished: epoch(ended), FirstAt: firstAt, Finish: hooks.Optional(response.FinishReason), Messages: len(request.Messages), Answering: request.Model,
		Body: hooks.ResponseView(settings, request.Model, response), Usage: hooks.UsageOf(response.Usage), Window: window, Assistant: assistant,
		Chars: len([]rune(message.GetText())), ToolCalls: len(message.GetToolCalls())})
}

// Fires api_request_error for one attempt of a model request that failed
func (a *Agent) requestFailed(ctx context.Context, settings *config.Config, th *turning, route provider.Route, request *provider.Request, err error,
	retries int, started time.Time) {
	if !a.hooks.Has(settings, hooks.APIRequestError) {
		return
	}
	ended := time.Now()
	kind := strings.TrimLeft(fmt.Sprintf("%T", err), "*")
	if i := strings.LastIndex(kind, "."); i >= 0 {
		kind = kind[i+1:]
	}
	event := hooks.APIError{Request: th.requestOf(route, request.Model, started), Duration: ended.Sub(started).Seconds(), Finished: epoch(ended),
		Retries: retries, MaxRetries: settings.GetInt(agentconfig.APIMaxRetries), Reason: hooks.Optional(string(provider.ErrUnknown)),
		Error: hooks.Failure{Type: kind, Message: err.Error()}, Body: hooks.RequestView(settings, request)}
	if failure, ok := errors.AsType[*provider.APIError](err); ok {
		if failure.StatusCode != 0 {
			status := failure.StatusCode
			event.Status = &status
		}
		event.Retryable = failure.Retryable()
		event.Reason = hooks.Optional(cmp.Or(failure.Reason, string(failure.Kind)))
		event.Error.Type = "APIError"
	}
	a.hooks.Fire(ctx, settings, event)
}

// What hands each failure of the request to the transform_api_error_classification hooks as the wire classifies it, their verdict laid
// over the wire's; nil when none is set
func (a *Agent) classifier(ctx context.Context, settings *config.Config, route provider.Route, request *provider.Request) func(*provider.APIError) {
	if !a.hooks.Has(settings, hooks.TransformAPIErrorClassification) {
		return nil
	}
	return func(failure *provider.APIError) {
		var status *int
		statusText := "None"
		if failure.StatusCode != 0 {
			code := failure.StatusCode
			status, statusText = &code, strconv.Itoa(code)
		}
		body := failure.Body
		if body == nil {
			body = map[string]any{}
		}
		text := ""
		if failure.Err != nil {
			text = failure.Err.Error()
		}
		class := a.hooks.Classify(ctx, settings, hooks.Classify{Provider: failure.Provider, Model: failure.Model, Status: status, Type: failure.Type,
			Code: failure.Code, Message: failure.Message, Body: body, Error: text, Tokens: hooks.Chars(request.Messages) / 4,
			Window: a.window(ctx, settings, route), Count: len(request.Messages)}, func(reason string) bool {
			_, known := provider.Reasons[reason]
			return known
		})
		if class == nil {
			return
		}
		failure.Reason, failure.Kind = class.Reason, provider.Reasons[class.Reason]
		failure.Retry, failure.Compress, failure.Rotate, failure.Fallback = class.Retryable, class.Compress, class.Rotate, class.Fallback
		if class.Message != "" {
			failure.Message = class.Message
		}
		failure.Context = class.Context
		slog.Info(fmt.Sprintf("API error classified by plugin hook: %s (provider=%s, status=%s)", class.Reason, failure.Provider, statusText))
	}
}

// Tells the stream observers what the turn says before its tool calls, which a held turn keeps for the model alone
func (a *Agent) interim(ctx context.Context, settings *config.Config, th *turning, route provider.Route, reply *sessionsv1.Message) {
	text := strings.TrimSpace(thinkBlocks.ReplaceAllString(reply.GetText(), ""))
	if text == "" {
		return
	}
	a.hooks.Stream(ctx, settings, hooks.Interim{Stream: th.stream(route, route.Model), Text: text, Streamed: !th.held})
	if th.held {
		reply.Display = "withheld"
	}
}

// Text the user is asked, with its secrets masked, as the human input hooks read it
func masked(text string) string {
	return redact.Text(text, false, redact.Ends)
}

// Triggers of a compression as post_compaction tells them, where they are named otherwise
var triggers = map[string]string{"preflight": "turn_start_threshold"}

// Fires post_compaction for a compaction of the session's context that was stored: the session it is in, the one it came from when the
// conversation went on in a new one, what kind it was and what asked for it, the tokens before and after and the messages folded away
func (a *Agent) postCompaction(ctx context.Context, settings *config.Config, session, previous, kind, trigger string, before, after, removed int) {
	old := ""
	if previous != session {
		old = previous
	}
	reason := trigger
	if named, ok := triggers[trigger]; ok {
		reason = named
	}
	a.hooks.Fire(context.WithoutCancel(ctx), settings, hooks.Compacted{Session: session, Old: old, Kind: kind, Reason: reason, Before: &before, After: &after,
		Removed: &removed})
}

// Settings the stored session runs with: its profile's with its own over them
func (a *Agent) stored(ctx context.Context, id string) (*sessionsv1.Session, *config.Config, error) {
	store, err := a.store()
	if err != nil {
		return nil, nil, err
	}
	session, err := store.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return session, a.config.In(session.GetProfile()).With(sessions.OverridesOf(session).Layer()), nil
}

// Runs the session's on_session_finalize hooks once, as it ends for good: what they have to tell the person. A session left for a new
// one names both
func (a *Agent) endSession(ctx context.Context, id, reason, replaced string) []string {
	a.mu.Lock()
	done := a.ended[id]
	a.ended[id] = true
	a.mu.Unlock()
	if done {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	session, settings, err := a.stored(ctx, id)
	if err != nil {
		return nil
	}
	event := hooks.Finalized{Session: id, Platform: platformOf(session), Reason: reason}
	if replaced != "" {
		event.Old, event.New = hooks.Optional(id), hooks.Optional(replaced)
	}
	return a.hooks.Finalize(ctx, settings, event)
}

// Ends the sessions as their hooks know it, such as those of an app that exits: what the hooks have to tell the person
func (a *Agent) Finalize(ctx context.Context, ids []string, reason string) []string {
	var out []string
	for _, id := range ids {
		out = append(out, a.endSession(ctx, id, cmp.Or(reason, "shutdown"), "")...)
	}
	return out
}

// Logs what the hooks of a session that ended with nobody watching had to tell the person
func unheard(id string, messages []string) {
	for _, message := range messages {
		slog.Info("Session end message: "+message, "session", id)
	}
}

// Hooks of a window that left its session for a new one: the session left ends as its hooks know it, what they say told in the new one,
// then on_session_reset
func (a *Agent) reset(ctx context.Context, left, id string) error {
	r, err := a.run(id)
	if err != nil {
		return err
	}
	messages := a.endSession(ctx, left, "new_session", id)
	r.mu.Lock()
	for _, message := range messages {
		a.notify(r, message)
	}
	r.mu.Unlock()
	session, settings, err := a.stored(ctx, id)
	if err != nil {
		return err
	}
	a.hooks.Fire(context.WithoutCancel(ctx), settings, hooks.Reset{Session: hooks.Optional(id), Reason: "new_session", Platform: platformOf(session),
		Old: hooks.Optional(left), New: hooks.Optional(id)})
	return nil
}

// Most pre_command hooks of one session that may wait their turn
const commandQueue = 32

// Slash commands that end or answer what the session's turn waits on, whose pre_command hooks run only while no turn does
var escapes = map[string]bool{"stop": true, "approve": true, "deny": true}

// pre_command hooks of one session waiting their turn, each run once those before it ran
type observing struct {
	pending int
	last    chan struct{}
}

// Queues the pre_command hooks of a slash command typed in the session, run after those queued before it: what is done once they ran,
// nil when nothing was queued
func (a *Agent) precommand(ctx context.Context, id, name, alias, args string) <-chan struct{} {
	_, settings, err := a.stored(ctx, id)
	if err != nil || !a.hooks.Has(settings, hooks.PreCommand) {
		return nil
	}
	if escapes[name] {
		a.mu.Lock()
		r := a.runs[id]
		a.mu.Unlock()
		if r != nil {
			r.mu.Lock()
			busy := r.busy
			r.mu.Unlock()
			if busy {
				return nil
			}
		}
	}
	a.mu.Lock()
	q := a.commands[id]
	if q == nil {
		q = &observing{}
		a.commands[id] = q
	}
	if q.pending >= commandQueue {
		a.mu.Unlock()
		slog.Warn(fmt.Sprintf("pre_command for /%s dropped: %d deliveries already queued for this session", name, q.pending))
		return nil
	}
	q.pending++
	previous, done := q.last, make(chan struct{})
	q.last = done
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			if q.pending--; q.pending == 0 && a.commands[id] == q {
				delete(a.commands, id)
			}
			a.mu.Unlock()
			close(done)
		}()
		if previous != nil {
			<-previous
		}
		key, platform := id, "cli"
		for _, result := range a.hooks.Fire(context.WithoutCancel(ctx), settings, hooks.Command{Surface: "cli", Name: name, Alias: alias, Args: args,
			SessionKey: &key, Platform: &platform}) {
			if result.Action != "" {
				slog.Debug(fmt.Sprintf("pre_command is observer-only in v1: ignoring directive %s for /%s (surface=cli).", result.Action, name))
			}
		}
	}()
	return done
}
