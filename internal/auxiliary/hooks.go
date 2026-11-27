package auxiliary

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/pkg/strs"
)

// One attempt of a call as the auxiliary hooks know it, nil when none watches the calls
type watched struct {
	base    hooks.Auxiliary
	started time.Time
}

// One call of a task as its hooks name it: its request id, and the attempts made of it so far on every route it tried
type auxCall struct {
	id       string
	attempts int
}

type auxCallKey struct{}

// Context of one call of a task, which every attempt of it on every route names
func withAuxCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, auxCallKey{}, &auxCall{id: "aux-" + strings.ReplaceAll(uuid.NewString(), "-", "")})
}

// Fires pre_auxiliary_call for one attempt of a call on the route: the task, the turn it runs under, the call's id and attempts so far, the
// route and the request
func (c *Client) preCall(ctx context.Context, task Task, route provider.Route, call *provider.Request) *watched {
	record, _ := ctx.Value(auxCallKey{}).(*auxCall)
	if record == nil {
		record = &auxCall{id: "aux-" + strings.ReplaceAll(uuid.NewString(), "-", "")}
	}
	retries := record.attempts
	record.attempts++
	h := c.Tasks.hooking()
	if !h.Has(c.Config, hooks.PreAuxiliaryCall) && !h.Has(c.Config, hooks.PostAuxiliaryCall) {
		return nil
	}
	turn := hooks.TurnOf(ctx)
	started := time.Now()
	w := &watched{started: started, base: hooks.Auxiliary{Session: turn.Session, Task: turn.Task, Turn: turn.ID, Platform: turn.Platform, Job: task.Name,
		ID: record.id, Retries: retries, Count: retries + 1, Model: call.Model, Provider: route.Provider, BaseURL: route.BaseURL, Mode: string(route.Mode),
		Started: float64(started.UnixNano()) / 1e9, Messages: len(call.Messages)}}
	if h.Has(c.Config, hooks.PreAuxiliaryCall) {
		chars := hooks.Chars(call.Messages)
		var limit *int
		if call.MaxTokens > 0 {
			limit = &call.MaxTokens
		}
		h.Fire(ctx, c.Config, hooks.AuxRequest{Auxiliary: w.base, Sent: hooks.Messages(call.Messages), System: hooks.System(call.Messages), Tokens: chars / 4,
			Chars: chars, MaxTokens: limit, Body: hooks.RequestView(c.Config, call)})
	}
	return w
}

// Fires post_auxiliary_call for an attempt pre_auxiliary_call heard of: how long it took, and the reply or what failed
func (c *Client) postCall(ctx context.Context, w *watched, call *provider.Request, response *provider.Response, err error) {
	h := c.Tasks.hooking()
	if w == nil || !h.Has(c.Config, hooks.PostAuxiliaryCall) {
		return
	}
	ended := time.Now()
	event := hooks.AuxResponse{Auxiliary: w.base, Finished: float64(ended.UnixNano()) / 1e9, Duration: max(ended.Sub(w.started).Seconds(), 0)}
	if err != nil {
		kind := failureType(err)
		event.Error, event.ErrorType = hooks.Optional(strs.Head(kind+": "+err.Error(), 2000)), &kind
	} else if response != nil {
		message := response.Message
		event.Finish, event.Answering = hooks.Optional(response.FinishReason), hooks.Optional(call.Model)
		event.Usage, event.Body = hooks.UsageOf(response.Usage), hooks.ResponseView(c.Config, call.Model, response)
		event.Chars, event.ToolCalls = len([]rune(message.GetText())), len(message.GetToolCalls())
	}
	h.Fire(context.WithoutCancel(ctx), c.Config, event)
}

// Kind of a failed attempt as the auxiliary hooks name it
func failureType(err error) string {
	var late *timeout
	var failure *provider.APIError
	switch {
	case errors.As(err, &late), errors.Is(err, context.DeadlineExceeded):
		return "TimeoutError"
	case errors.Is(err, context.Canceled):
		return "CancelledError"
	case errors.As(err, &failure) && failure.Kind == provider.ErrNetwork:
		return "ConnectionError"
	case errors.As(err, &failure):
		return "APIError"
	}
	return "Exception"
}
