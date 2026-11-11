package dashboard

import (
	"context"
	"mime"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Request parameter of a path operation read from the path, query, headers or cookies
type Param struct {
	Name     string
	Alias    string
	In       string
	Type     FieldType
	Required bool
	Default  any
}

// Required path parameter
func PathParam(name string, t FieldType) Param {
	return Param{Name: name, In: "path", Type: t, Required: true}
}

// Query parameter with a default
func QueryParam(name string, t FieldType, def any) Param {
	return Param{Name: name, In: "query", Type: t, Default: def}
}

// Required query parameter
func RequiredQuery(name string, t FieldType) Param {
	return Param{Name: name, In: "query", Type: t, Required: true}
}

// Header parameter with a default, its underscores read as dashes
func HeaderParam(name string, t FieldType, def any) Param {
	return Param{Name: name, In: "header", Type: t, Default: def}
}

// Cookie parameter with a default
func CookieParam(name string, t FieldType, def any) Param {
	return Param{Name: name, In: "cookie", Type: t, Default: def}
}

// Key a parameter is read from
func (p Param) validationAlias() string {
	if p.Alias != "" {
		return p.Alias
	}
	return p.Name
}

// JSON body parameter, the whole body for a lone model parameter or one embedded key otherwise
type BodyParam struct {
	Name     string
	Type     FieldType
	Required bool
	Default  any
	Embed    bool
}

// Required body parameter of a type
func Body(name string, t FieldType) BodyParam { return BodyParam{Name: name, Type: t, Required: true} }

// Route dependency run before the endpoint, its HTTP error ending the request
type Dependency struct {
	Name string
	Call func(ctx context.Context, c *Call) error
}

// Values a path operation receives, its connection and validated parameters
type Call struct {
	Request   *Request
	WebSocket *WebSocket
	Values    map[string]any
	received  map[string]any
}

// Connection of the call, the request or the websocket
func (c *Call) Conn() *HTTPConnection {
	if c.Request != nil {
		return &c.Request.HTTPConnection
	}
	return &c.WebSocket.HTTPConnection
}

// Validated string parameter
func (c *Call) Str(name string) string { s, _ := c.Values[name].(string); return s }

// Validated int parameter
func (c *Call) Int(name string) int64 { n, _ := c.Values[name].(int64); return n }

// Validated float parameter
func (c *Call) Float(name string) float64 { f, _ := c.Values[name].(float64); return f }

// Validated bool parameter
func (c *Call) Bool(name string) bool { b, _ := c.Values[name].(bool); return b }

// Validated model or dict parameter, nil when None
func (c *Call) Object(name string) *pyjson.Object { o, _ := c.Values[name].(*pyjson.Object); return o }

// Validated parameter as it came out of validation
func (c *Call) Value(name string) any { return c.Values[name] }

// Fields of a body model the request gave explicitly by alias, the pydantic model_fields_set
func (c *Call) FieldsSet(name string, m *Model) map[string]bool {
	out := map[string]bool{}
	obj, ok := c.received[name].(*pyjson.Object)
	if !ok {
		return out
	}
	for _, f := range m.Fields {
		if _, has := obj.Get(f.validationAlias()); has {
			out[f.Name] = true
		}
	}
	return out
}

// Path operation, the parameters it validates and the function it runs
type Endpoint struct {
	Name   string
	Doc    string
	Tags   []string
	Params []Param
	Body   []BodyParam
	Deps   []*Dependency
	Handle func(ctx context.Context, c *Call) (any, error)
}

// Websocket operation, the parameters it validates and the session it runs
type WSEndpoint struct {
	Name   string
	Params []Param
	Deps   []*Dependency
	Handle func(ctx context.Context, c *Call) error
}

// Runs the dependencies then validates every parameter, collecting all errors
func solveDependencies(ctx context.Context, c *Call, deps []*Dependency, params []Param, body []BodyParam, received any) ([]ErrorDetails, error) {
	for _, d := range deps {
		if err := d.Call(ctx, c); err != nil {
			return nil, err
		}
	}
	conn := c.Conn()
	var errs []ErrorDetails
	for _, in := range []string{"path", "query", "header", "cookie"} {
		var group []Param
		for _, p := range params {
			if p.In == in {
				group = append(group, p)
			}
		}
		errs = append(errs, requestParamsToArgs(c.Values, group, in, conn)...)
	}
	if len(body) > 0 {
		errs = append(errs, requestBodyToArgs(c, body, received)...)
	}
	return errs, nil
}

// Raw value of a parameter from its source, nil when absent or an empty sequence
func paramRawValue(p Param, in string, conn *HTTPConnection) any {
	alias := p.validationAlias()
	switch in {
	case "path":
		v, ok := conn.PathParams()[alias]
		if !ok {
			return nil
		}
		return v
	case "query":
		qp := conn.QueryParams()
		if p.Type.isSequence() {
			vals := qp.GetList(alias)
			if len(vals) == 0 {
				return nil
			}
			return vals
		}
		v, ok := qp.Lookup(alias)
		if !ok {
			return nil
		}
		return v
	case "header":
		key := alias
		if p.Alias == "" {
			key = strings.ReplaceAll(alias, "_", "-")
		}
		h := conn.Headers()
		if p.Type.isSequence() {
			vals := h.GetList(key)
			if len(vals) == 0 {
				return nil
			}
			return vals
		}
		v, ok := h.Lookup(key)
		if !ok {
			return nil
		}
		return v
	}
	v, ok := conn.Cookies()[alias]
	if !ok {
		return nil
	}
	return v
}

// Validates one parameter group into values, a missing required one reported with a None input
func requestParamsToArgs(values map[string]any, params []Param, in string, conn *HTTPConnection) []ErrorDetails {
	var errs []ErrorDetails
	for _, p := range params {
		loc := []any{in, p.validationAlias()}
		raw := paramRawValue(p, in, conn)
		if raw == nil {
			if p.Required {
				errs = append(errs, ErrorDetails{Type: "missing", Loc: loc, Msg: "Field required", Input: nil})
				continue
			}
			values[p.Name] = pyjson.Clone(p.Default)
			continue
		}
		v, e := p.Type.Validate(raw)
		if len(e) > 0 {
			errs = append(errs, prefixLoc(loc, e)...)
			continue
		}
		values[p.Name] = v
	}
	return errs
}

// Validates the body into values, whole for a lone unembedded parameter and by key otherwise
func requestBodyToArgs(c *Call, body []BodyParam, received any) []ErrorDetails {
	values := c.Values
	if c.received == nil {
		c.received = map[string]any{}
	}
	if len(body) == 1 && !body[0].Embed {
		p := body[0]
		c.received[p.Name] = received
		v, e := validateBodyValue(p, received, []any{"body"})
		if len(e) > 0 {
			return e
		}
		values[p.Name] = v
		return nil
	}
	var errs []ErrorDetails
	for _, p := range body {
		loc := []any{"body", p.Name}
		var raw any
		if received != nil {
			obj, ok := received.(*pyjson.Object)
			if !ok {
				errs = append(errs, ErrorDetails{Type: "missing", Loc: loc, Msg: "Field required", Input: nil})
				continue
			}
			raw = obj.Vals[p.Name]
		}
		c.received[p.Name] = raw
		v, e := validateBodyValue(p, raw, loc)
		if len(e) > 0 {
			errs = append(errs, e...)
			continue
		}
		values[p.Name] = v
	}
	return errs
}

// Validated body value, the default for None when optional and a missing error when required
func validateBodyValue(p BodyParam, raw any, loc []any) (any, []ErrorDetails) {
	if raw == nil {
		if p.Required {
			return nil, []ErrorDetails{{Type: "missing", Loc: loc, Msg: "Field required", Input: nil}}
		}
		return pyjson.Clone(p.Default), nil
	}
	v, e := p.Type.Validate(raw)
	if len(e) > 0 {
		return nil, prefixLoc(loc, e)
	}
	return v, nil
}

// Whether the body of a path operation is embedded under each parameter name
func embedBodyFields(body []BodyParam) bool {
	if len(body) > 1 {
		return true
	}
	return len(body) == 1 && body[0].Embed
}

// Decoded JSON body for a JSON content type, the raw bytes for a missing or other content type
func readJSONBody(ctx context.Context, req *Request) (any, error) {
	raw, err := req.Body(ctx)
	if err != nil {
		if _, ok := err.(ClientDisconnect); ok {
			return nil, NewHTTPException(400, "There was an error parsing the body")
		}
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	ct, _ := req.Headers().Lookup("content-type")
	isJSON := false
	if ct != "" {
		mt, _, perr := mime.ParseMediaType(ct)
		if perr != nil {
			mt = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
		}
		main, sub, _ := strings.Cut(mt, "/")
		isJSON = main == "application" && (sub == "json" || strings.HasSuffix(sub, "+json"))
	}
	if !isJSON {
		return string(raw), nil
	}
	v, err := req.JSON(ctx)
	if err != nil {
		if de, ok := err.(*pyjson.DecodeError); ok {
			return nil, &RequestValidationError{Errors: []ErrorDetails{{Type: "json_invalid", Loc: []any{"body", int64(de.Pos)},
				Msg: "JSON decode error", Input: pyjson.NewObject(), Ctx: jsonObj("error", de.Msg)}}, Body: string(raw)}
		}
		return nil, NewHTTPException(400, "There was an error parsing the body")
	}
	return v, nil
}

// HTTP path operation route, the FastAPI APIRoute
type APIRoute struct {
	Path       string
	Methods    []string
	Endpoint   *Endpoint
	Name       string
	pathRegex  *regexp.Regexp
	pathFormat string
	convertors map[string]convertor
	app        ASGIApp
}

// Route of an endpoint for the methods, GET when none are given
func NewAPIRoute(path string, methods []string, ep *Endpoint) *APIRoute {
	if len(methods) == 0 {
		methods = []string{"GET"}
	}
	upper := make([]string, 0, len(methods))
	for _, m := range methods {
		if u := strings.ToUpper(m); !slices.Contains(upper, u) {
			upper = append(upper, u)
		}
	}
	r := &APIRoute{Path: path, Methods: upper, Endpoint: ep, Name: ep.Name}
	r.pathRegex, r.pathFormat, r.convertors = compilePath(path)
	r.app = r.requestResponse()
	return r
}

func (r *APIRoute) RoutePath() string { return r.Path }

func (r *APIRoute) Matches(s *Scope) (Match, *childScope) {
	if s.Type != "http" {
		return MatchNone, nil
	}
	params, ok := matchParams(r.pathRegex, r.convertors, s, getRoutePath(s))
	if !ok {
		return MatchNone, nil
	}
	child := &childScope{endpoint: r.Endpoint, pathParams: params, route: r}
	if !slices.Contains(r.Methods, s.Method) {
		return MatchPartial, child
	}
	return MatchFull, child
}

// Raises 405 with the allowed methods for another method, else runs the operation
func (r *APIRoute) Handle(ctx context.Context, s *Scope, receive Receive, send Send) error {
	if !slices.Contains(r.Methods, s.Method) {
		allow := strings.Join(r.Methods, ", ")
		if s.App != nil {
			return NewHTTPException(405, nil, "Allow", allow)
		}
		return PlainTextResponse("Method Not Allowed", 405, "Allow", allow).Serve(ctx, s, receive, send)
	}
	return r.app(ctx, s, receive, send)
}

// ASGI app of the operation, the body read, dependencies solved and the result rendered as JSON
func (r *APIRoute) requestResponse() ASGIApp {
	ep := r.Endpoint
	return func(ctx context.Context, s *Scope, receive Receive, send Send) error {
		req := NewRequest(s, receive, send)
		inner := func(ctx context.Context, s *Scope, receive Receive, send Send) error {
			var received any
			if len(ep.Body) > 0 {
				var err error
				var closeBody func()
				if received, closeBody, err = readRequestBody(ctx, req, ep); err != nil {
					return err
				}
				defer closeBody()
			}
			c := &Call{Request: req, Values: map[string]any{}}
			errs, err := solveDependencies(ctx, c, ep.Deps, ep.Params, ep.Body, received)
			if err != nil {
				return err
			}
			if len(errs) > 0 {
				return &RequestValidationError{Errors: errs, Body: received}
			}
			result, err := ep.Handle(ctx, c)
			if err != nil {
				return err
			}
			resp, ok := result.(ASGIResponse)
			if !ok {
				jr, err := JSONResponse(jsonableEncoder(result), 200)
				if err != nil {
					return err
				}
				resp = jr
			}
			return resp.Serve(ctx, s, receive, send)
		}
		return wrapAppHandlingExceptions(inner, req, nil)(ctx, s, receive, send)
	}
}

// Websocket path operation route, the FastAPI APIWebSocketRoute
type APIWebSocketRoute struct {
	Path       string
	Endpoint   *WSEndpoint
	Name       string
	pathRegex  *regexp.Regexp
	convertors map[string]convertor
}

// Websocket route of an endpoint
func NewAPIWebSocketRoute(path string, ep *WSEndpoint) *APIWebSocketRoute {
	r := &APIWebSocketRoute{Path: path, Endpoint: ep, Name: ep.Name}
	r.pathRegex, _, r.convertors = compilePath(path)
	return r
}

func (r *APIWebSocketRoute) RoutePath() string { return r.Path }

func (r *APIWebSocketRoute) Matches(s *Scope) (Match, *childScope) {
	if s.Type != "websocket" {
		return MatchNone, nil
	}
	params, ok := matchParams(r.pathRegex, r.convertors, s, getRoutePath(s))
	if !ok {
		return MatchNone, nil
	}
	return MatchFull, &childScope{endpoint: r.Endpoint, pathParams: params, route: r}
}

// Solves the dependencies and parameters, raising the websocket validation error, then runs the session
func (r *APIWebSocketRoute) Handle(ctx context.Context, s *Scope, receive Receive, send Send) error {
	ws := NewWebSocket(s, receive, send)
	inner := func(ctx context.Context, s *Scope, receive Receive, send Send) error {
		c := &Call{WebSocket: ws, Values: map[string]any{}}
		errs, err := solveDependencies(ctx, c, r.Endpoint.Deps, r.Endpoint.Params, nil, nil)
		if err != nil {
			return err
		}
		if len(errs) > 0 {
			return &WebSocketRequestValidationError{Errors: errs}
		}
		return r.Endpoint.Handle(ctx, c)
	}
	return wrapAppHandlingExceptions(inner, nil, ws)(ctx, s, receive, send)
}

// Group of routes under a prefix sharing dependencies, the FastAPI APIRouter
type APIRouter struct {
	Prefix       string
	Dependencies []*Dependency
	Routes       []BaseRoute
}

// Router with a prefix and router level dependencies
func NewAPIRouter(prefix string, deps ...*Dependency) *APIRouter {
	if prefix != "" && (!strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/")) {
		panic("A path prefix must start with '/' and must not end with '/'")
	}
	return &APIRouter{Prefix: prefix, Dependencies: deps}
}

// Adds an http operation, the router dependencies running before the operation ones
func (r *APIRouter) AddAPIRoute(path string, methods []string, ep *Endpoint) {
	withDeps := *ep
	withDeps.Deps = append(slices.Clone(r.Dependencies), ep.Deps...)
	r.Routes = append(r.Routes, NewAPIRoute(r.Prefix+path, methods, &withDeps))
}

// Adds a websocket operation, the router dependencies running before the operation ones
func (r *APIRouter) AddAPIWebSocketRoute(path string, ep *WSEndpoint) {
	withDeps := *ep
	withDeps.Deps = append(slices.Clone(r.Dependencies), ep.Deps...)
	r.Routes = append(r.Routes, NewAPIWebSocketRoute(r.Prefix+path, &withDeps))
}

// GET operation
func (r *APIRouter) Get(path string, ep *Endpoint) { r.AddAPIRoute(path, []string{"GET"}, ep) }

// POST operation
func (r *APIRouter) Post(path string, ep *Endpoint) { r.AddAPIRoute(path, []string{"POST"}, ep) }

// PUT operation
func (r *APIRouter) Put(path string, ep *Endpoint) { r.AddAPIRoute(path, []string{"PUT"}, ep) }

// PATCH operation
func (r *APIRouter) Patch(path string, ep *Endpoint) { r.AddAPIRoute(path, []string{"PATCH"}, ep) }

// DELETE operation
func (r *APIRouter) Delete(path string, ep *Endpoint) { r.AddAPIRoute(path, []string{"DELETE"}, ep) }

// Websocket operation
func (r *APIRouter) WebSocket(path string, ep *WSEndpoint) { r.AddAPIWebSocketRoute(path, ep) }

// Copies another router's routes under a prefix, the include dependencies running before the route ones
func (r *APIRouter) IncludeRouter(other *APIRouter, prefix string, deps ...*Dependency) {
	if prefix != "" && (!strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/")) {
		panic("A path prefix must start with '/' and must not end with '/'")
	}
	for _, route := range other.Routes {
		switch rt := route.(type) {
		case *APIRoute:
			ep := *rt.Endpoint
			ep.Deps = append(slices.Clone(deps), rt.Endpoint.Deps...)
			r.AddAPIRoute(prefix+rt.Path, rt.Methods, &ep)
		case *APIWebSocketRoute:
			ep := *rt.Endpoint
			ep.Deps = append(slices.Clone(deps), rt.Endpoint.Deps...)
			r.AddAPIWebSocketRoute(prefix+rt.Path, &ep)
		default:
			r.Routes = append(r.Routes, route)
		}
	}
}
