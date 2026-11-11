package pyacp

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/logging"
)

// Handler of one routed method receiving the raw params
type RouteFunc func(ctx context.Context, params any) (any, error)

// One request or notification route of a message router
type Route struct {
	Method        string
	Func          RouteFunc
	Kind          string
	Optional      bool
	DefaultResult any
	AdaptResult   func(any) any
	WarnUnstable  bool
}

// Result of the route, method not found when the handler is absent or unstable and disabled
func (r *Route) Handle(ctx context.Context, params any) (any, error) {
	if r.Func == nil {
		if r.Optional {
			return r.DefaultResult, nil
		}
		return nil, MethodNotFound(r.Method)
	}
	if r.WarnUnstable {
		logging.GetLogger("py.warnings").Warning("UserWarning: The method " + r.Method + " is part of the unstable protocol, please enable `use_unstable_protocol` flag to use it.")
		return nil, MethodNotFound(r.Method)
	}
	result, err := r.Func(ctx, params)
	if err != nil {
		return nil, err
	}
	if r.AdaptResult != nil && r.Kind == "request" {
		return r.AdaptResult(result), nil
	}
	return result, nil
}

// Handler of extension methods whose name lost its leading underscore
type ExtensionHandler func(ctx context.Context, name string, payload *pyjson.Object) (any, error)

// Method name router for requests, notifications and underscore extensions
type MessageRouter struct {
	requests               map[string]*Route
	notifications          map[string]*Route
	requestExtensions      ExtensionHandler
	notificationExtensions ExtensionHandler
	useUnstableProtocol    bool
}

// Empty router, unstable routes refused unless enabled
func NewMessageRouter(useUnstableProtocol bool) *MessageRouter {
	return &MessageRouter{requests: map[string]*Route{}, notifications: map[string]*Route{}, useUnstableProtocol: useUnstableProtocol}
}

// Registers a route under its method and kind
func (m *MessageRouter) AddRoute(r *Route) {
	if r.Kind == "request" {
		m.requests[r.Method] = r
	} else {
		m.notifications[r.Method] = r
	}
}

// Registers a request route, unstable routes warning when the protocol flag is off
func (m *MessageRouter) RouteRequest(method string, fn RouteFunc, adapt func(any) any, unstable bool) *Route {
	r := &Route{Method: method, Func: fn, Kind: "request", AdaptResult: adapt, WarnUnstable: unstable && !m.useUnstableProtocol}
	m.AddRoute(r)
	return r
}

// Registers a notification route
func (m *MessageRouter) RouteNotification(method string, fn RouteFunc, unstable bool) *Route {
	r := &Route{Method: method, Func: fn, Kind: "notification", WarnUnstable: unstable && !m.useUnstableProtocol}
	m.AddRoute(r)
	return r
}

// Sets the handler of underscore prefixed requests
func (m *MessageRouter) HandleExtensionRequest(h ExtensionHandler) { m.requestExtensions = h }

// Sets the handler of underscore prefixed notifications
func (m *MessageRouter) HandleExtensionNotification(h ExtensionHandler) {
	m.notificationExtensions = h
}

// Dispatches one incoming message to its route or extension handler
func (m *MessageRouter) Call(ctx context.Context, method string, params any, isNotification bool) (any, error) {
	ext, routes := m.requestExtensions, m.requests
	if isNotification {
		ext, routes = m.notificationExtensions, m.notifications
	}
	if strings.HasPrefix(method, "_") {
		if ext == nil {
			return nil, MethodNotFound(method)
		}
		payload, ok := params.(*pyjson.Object)
		if !ok {
			payload = pyjson.NewObject()
		}
		return ext(ctx, method[1:], payload)
	}
	r, ok := routes[method]
	if !ok {
		return nil, MethodNotFound(method)
	}
	return r.Handle(ctx, params)
}

// Empty object for nil, serialized params for a model, the payload otherwise
func NormalizeResult(payload any) any {
	if IsNil(payload) {
		return pyjson.NewObject()
	}
	if IsModel(payload) {
		return Dump(payload, SerializeParams)
	}
	return payload
}

// Wrapper validating params into the request model before calling the agent method
func modelRoute[T any, R any](fn func(context.Context, *T) (R, error)) RouteFunc {
	return func(ctx context.Context, params any) (any, error) {
		req, err := Validate[T](params)
		if err != nil {
			return nil, err
		}
		return fn(ctx, req)
	}
}

// Agent method answering initialize
type Initializer interface {
	Initialize(ctx context.Context, req *InitializeRequest) (*InitializeResponse, error)
}

// Agent method answering authenticate
type Authenticator interface {
	Authenticate(ctx context.Context, req *AuthenticateRequest) (*AuthenticateResponse, error)
}

// Agent method answering session/new
type SessionCreator interface {
	NewSession(ctx context.Context, req *NewSessionRequest) (*NewSessionResponse, error)
}

// Agent method answering session/load
type SessionLoader interface {
	LoadSession(ctx context.Context, req *LoadSessionRequest) (*LoadSessionResponse, error)
}

// Agent method answering session/list
type SessionLister interface {
	ListSessions(ctx context.Context, req *ListSessionsRequest) (*ListSessionsResponse, error)
}

// Agent method answering session/close
type SessionCloser interface {
	CloseSession(ctx context.Context, req *CloseSessionRequest) (*CloseSessionResponse, error)
}

// Agent method answering session/set_mode
type SessionModeSetter interface {
	SetSessionMode(ctx context.Context, req *SetSessionModeRequest) (*SetSessionModeResponse, error)
}

// Agent method answering session/prompt
type Prompter interface {
	Prompt(ctx context.Context, req *PromptRequest) (*PromptResponse, error)
}

// Agent method answering session/set_model
type SessionModelSetter interface {
	SetSessionModel(ctx context.Context, req *SetSessionModelRequest) (*SetSessionModelResponse, error)
}

// Shared fields of the boolean and select set config option requests
type SetConfigOptionParams struct {
	Meta      *pyjson.Object
	ConfigID  string
	SessionID string
	Value     any
}

// Agent method answering session/set_config_option
type ConfigOptionSetter interface {
	SetConfigOption(ctx context.Context, req *SetConfigOptionParams) (*SetSessionConfigOptionResponse, error)
}

// Agent method answering session/fork
type SessionForker interface {
	ForkSession(ctx context.Context, req *ForkSessionRequest) (*ForkSessionResponse, error)
}

// Agent method answering session/resume
type SessionResumer interface {
	ResumeSession(ctx context.Context, req *ResumeSessionRequest) (*ResumeSessionResponse, error)
}

// Agent method receiving session/cancel
type Canceller interface {
	Cancel(ctx context.Context, req *CancelNotification) error
}

// Agent method answering underscore extension requests
type ExtMethoder interface {
	ExtMethod(ctx context.Context, name string, params *pyjson.Object) (any, error)
}

// Agent method receiving underscore extension notifications
type ExtNotifier interface {
	ExtNotification(ctx context.Context, name string, params *pyjson.Object) error
}

// Agent hook receiving the connection it is served on
type Connector interface {
	OnConnect(conn *AgentSideConnection)
}

// Validates set config option params as the boolean request when typed boolean, else the select request
func validateSetConfigOption(params any) (*SetConfigOptionParams, error) {
	if obj, ok := params.(*pyjson.Object); ok {
		if t, _ := obj.Get("type"); t == "boolean" {
			req, err := Validate[SetSessionConfigOptionBooleanRequest](params)
			if err != nil {
				return nil, err
			}
			return &SetConfigOptionParams{Meta: req.Meta, ConfigID: req.ConfigID, SessionID: req.SessionID, Value: req.Value}, nil
		}
	}
	req, err := Validate[SetSessionConfigOptionSelectRequest](params)
	if err != nil {
		return nil, err
	}
	return &SetConfigOptionParams{Meta: req.Meta, ConfigID: req.ConfigID, SessionID: req.SessionID, Value: req.Value}, nil
}

// Router of every agent method, routes absent from the agent answering method not found
func BuildAgentRouter(agent any, useUnstableProtocol bool) *MessageRouter {
	r := NewMessageRouter(useUnstableProtocol)
	var fn RouteFunc
	fn = nil
	if a, ok := agent.(Initializer); ok {
		fn = modelRoute(a.Initialize)
	}
	r.RouteRequest(AgentMethods["initialize"], fn, nil, false)
	fn = nil
	if a, ok := agent.(SessionCreator); ok {
		fn = modelRoute(a.NewSession)
	}
	r.RouteRequest(AgentMethods["session_new"], fn, nil, false)
	fn = nil
	if a, ok := agent.(SessionLoader); ok {
		fn = modelRoute(a.LoadSession)
	}
	r.RouteRequest(AgentMethods["session_load"], fn, NormalizeResult, false)
	fn = nil
	if a, ok := agent.(SessionLister); ok {
		fn = modelRoute(a.ListSessions)
	}
	r.RouteRequest(AgentMethods["session_list"], fn, nil, false)
	fn = nil
	if a, ok := agent.(SessionCloser); ok {
		fn = modelRoute(a.CloseSession)
	}
	r.RouteRequest(AgentMethods["session_close"], fn, NormalizeResult, true)
	fn = nil
	if a, ok := agent.(SessionModeSetter); ok {
		fn = modelRoute(a.SetSessionMode)
	}
	r.RouteRequest(AgentMethods["session_set_mode"], fn, NormalizeResult, false)
	fn = nil
	if a, ok := agent.(Prompter); ok {
		fn = modelRoute(a.Prompt)
	}
	r.RouteRequest(AgentMethods["session_prompt"], fn, nil, false)
	fn = nil
	if a, ok := agent.(SessionModelSetter); ok {
		fn = modelRoute(a.SetSessionModel)
	}
	r.RouteRequest(AgentMethods["session_set_model"], fn, NormalizeResult, true)
	fn = nil
	if a, ok := agent.(ConfigOptionSetter); ok {
		fn = func(ctx context.Context, params any) (any, error) {
			req, err := validateSetConfigOption(params)
			if err != nil {
				return nil, err
			}
			return a.SetConfigOption(ctx, req)
		}
	}
	r.AddRoute(&Route{Method: AgentMethods["session_set_config_option"], Func: fn, Kind: "request", AdaptResult: NormalizeResult})
	fn = nil
	if a, ok := agent.(Authenticator); ok {
		fn = modelRoute(a.Authenticate)
	}
	r.RouteRequest(AgentMethods["authenticate"], fn, NormalizeResult, false)
	fn = nil
	if a, ok := agent.(SessionForker); ok {
		fn = modelRoute(a.ForkSession)
	}
	r.RouteRequest(AgentMethods["session_fork"], fn, nil, true)
	fn = nil
	if a, ok := agent.(SessionResumer); ok {
		fn = modelRoute(a.ResumeSession)
	}
	r.RouteRequest(AgentMethods["session_resume"], fn, nil, true)
	fn = nil
	if a, ok := agent.(Canceller); ok {
		fn = func(ctx context.Context, params any) (any, error) {
			req, err := Validate[CancelNotification](params)
			if err != nil {
				return nil, err
			}
			return nil, a.Cancel(ctx, req)
		}
	}
	r.RouteNotification(AgentMethods["session_cancel"], fn, false)
	r.HandleExtensionRequest(func(ctx context.Context, name string, payload *pyjson.Object) (any, error) {
		ext, ok := agent.(ExtMethoder)
		if !ok {
			return nil, MethodNotFound("_" + name)
		}
		return ext.ExtMethod(ctx, name, payload)
	})
	r.HandleExtensionNotification(func(ctx context.Context, name string, payload *pyjson.Object) (any, error) {
		ext, ok := agent.(ExtNotifier)
		if !ok {
			return nil, nil
		}
		return nil, ext.ExtNotification(ctx, name, payload)
	})
	return r
}
