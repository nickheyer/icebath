package acputil

import (
	"context"
	"encoding/json"
	"strings"
)

// Handler of one routed method given the raw params of its frame
type routeFunc func(ctx context.Context, params json.RawMessage) (any, error)

type route struct {
	handle   routeFunc
	unstable bool
}

// Method of the agent table: its wire name, whether it belongs to the unstable protocol and how to bind it to an agent, nil when the agent lacks it
type agentMethod struct {
	method   string
	unstable bool
	bind     func(agent any) routeFunc
}

// Requests an agent can serve
var agentRequests = []agentMethod{
	{MethodInitialize, false, bindRequest(Initializer.Initialize, false)},
	{MethodAuthenticate, false, bindRequest(Authenticator.Authenticate, true)},
	{MethodSessionNew, false, bindRequest(SessionCreator.NewSession, false)},
	{MethodSessionLoad, false, bindRequest(SessionLoader.LoadSession, true)},
	{MethodSessionList, false, bindRequest(SessionLister.ListSessions, false)},
	{MethodSessionClose, true, bindRequest(SessionCloser.CloseSession, true)},
	{MethodSessionSetMode, false, bindRequest(SessionModeSetter.SetSessionMode, true)},
	{MethodSessionPrompt, false, bindRequest(Prompter.Prompt, false)},
	{MethodSessionSetModel, true, bindRequest(SessionModelSetter.SetSessionModel, true)},
	{MethodSessionSetConfigOption, false, bindSetConfigOption},
	{MethodSessionFork, true, bindRequest(SessionForker.ForkSession, false)},
	{MethodSessionResume, true, bindRequest(SessionResumer.ResumeSession, false)},
}

// Notifications an agent can receive
var agentNotifications = []agentMethod{
	{MethodSessionCancel, false, bindCancel},
}

// Route of a request method of interface I: params decoded into Q, a nil result answered with an empty object when emptyWhenNil, else with null
func bindRequest[I, Q, P any](fn func(I, context.Context, *Q) (*P, error), emptyWhenNil bool) func(agent any) routeFunc {
	return func(agent any) routeFunc {
		a, ok := agent.(I)
		if !ok {
			return nil
		}
		return func(ctx context.Context, params json.RawMessage) (any, error) {
			req, err := decodeParams[Q](params)
			if err != nil {
				return nil, err
			}
			resp, err := fn(a, ctx, req)
			if err != nil {
				return nil, err
			}
			if resp == nil {
				if emptyWhenNil {
					return struct{}{}, nil
				}
				return nil, nil
			}
			return resp, nil
		}
	}
}

func bindSetConfigOption(agent any) routeFunc {
	a, ok := agent.(ConfigOptionSetter)
	if !ok {
		return nil
	}
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		req, err := decodeSetConfigOption(params)
		if err != nil {
			return nil, err
		}
		resp, err := a.SetConfigOption(ctx, req)
		if err != nil {
			return nil, err
		}
		if resp == nil {
			return struct{}{}, nil
		}
		return resp, nil
	}
}

func bindCancel(agent any) routeFunc {
	a, ok := agent.(Canceller)
	if !ok {
		return nil
	}
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		req, err := decodeParams[CancelNotification](params)
		if err != nil {
			return nil, err
		}
		return nil, a.Cancel(ctx, req)
	}
}

// Params of a frame decoded into a new Q, absent or null params read as an empty object; a failure is an invalid params error whose details give the reason
func decodeParams[Q any](params json.RawMessage) (*Q, error) {
	if len(params) == 0 || isNull(params) {
		params = json.RawMessage("{}")
	}
	req := new(Q)
	if err := json.Unmarshal(params, req); err != nil {
		return nil, InvalidParams(details(err))
	}
	return req, nil
}

// Method table of one agent
type agentRouter struct {
	agent         any
	requests      map[string]route
	notifications map[string]route
	unstable      bool
}

func newAgentRouter(agent any, unstable bool) *agentRouter {
	r := &agentRouter{agent: agent, requests: map[string]route{}, notifications: map[string]route{}, unstable: unstable}
	for _, m := range agentRequests {
		if h := m.bind(agent); h != nil {
			r.requests[m.method] = route{handle: h, unstable: m.unstable}
		}
	}
	for _, m := range agentNotifications {
		if h := m.bind(agent); h != nil {
			r.notifications[m.method] = route{handle: h, unstable: m.unstable}
		}
	}
	return r
}

// Dispatches one incoming frame: extension methods by the name after their underscore, the rest through the table; a method the agent lacks, or an unstable one the connection does not serve, is method not found
func (r *agentRouter) handle(ctx context.Context, method string, params json.RawMessage, notification bool) (any, error) {
	if name, ok := strings.CutPrefix(method, "_"); ok {
		return r.extension(ctx, name, params, notification)
	}
	routes := r.requests
	if notification {
		routes = r.notifications
	}
	rt, ok := routes[method]
	if !ok {
		return nil, MethodNotFound(method)
	}
	if rt.unstable && !r.unstable {
		pkgLog().Warning("acp method " + method + " belongs to the unstable protocol, which this connection does not serve")
		return nil, MethodNotFound(method)
	}
	return rt.handle(ctx, params)
}

// Hands an extension method to the agent, params that are not an object passed as an empty one; an unanswered extension request is method not found, an unhandled extension notification is dropped
func (r *agentRouter) extension(ctx context.Context, name string, params json.RawMessage, notification bool) (any, error) {
	if !isObject(params) {
		params = json.RawMessage("{}")
	}
	if notification {
		if h, ok := r.agent.(ExtensionNotificationHandler); ok {
			return nil, h.ExtNotification(ctx, name, params)
		}
		return nil, nil
	}
	if h, ok := r.agent.(ExtensionMethodHandler); ok {
		return h.ExtMethod(ctx, name, params)
	}
	return nil, MethodNotFound("_" + name)
}
