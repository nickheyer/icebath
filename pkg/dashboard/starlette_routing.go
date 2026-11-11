package dashboard

import (
	"context"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Outcome of matching a scope against a route
type Match int

const (
	MatchNone Match = iota
	MatchPartial
	MatchFull
)

// Scope keys a matching route sets on the scope before handling it
type childScope struct {
	endpoint    any
	pathParams  map[string]any
	route       BaseRoute
	rootPath    *string
	appRootPath *string
}

// Applies a route match to the scope the way scope.update(child_scope) does
func (c *childScope) apply(s *Scope) {
	s.Endpoint = c.endpoint
	s.PathParams = c.pathParams
	if c.route != nil {
		s.Route = c.route
	}
	if c.rootPath != nil {
		s.RootPath = *c.rootPath
	}
	if c.appRootPath != nil {
		s.Set("app_root_path", *c.appRootPath)
	}
}

// Route of a router, an http route, websocket route or mount
type BaseRoute interface {
	Matches(scope *Scope) (Match, *childScope)
	Handle(ctx context.Context, scope *Scope, receive Receive, send Send) error
	RoutePath() string
}

// Path parameter convertor of a route template
type convertor struct {
	regex   string
	convert func(string) any
}

var convertorTypes = map[string]convertor{
	"str":  {"[^/]+", func(v string) any { return v }},
	"path": {".*", func(v string) any { return v }},
	"int": {"[0-9]+", func(v string) any {
		n, ok := new(big.Int).SetString(v, 10)
		if ok && n.IsInt64() {
			return int(n.Int64())
		}
		return n
	}},
	"float": {`[0-9]+(\.[0-9]+)?`, func(v string) any { f, _ := strconv.ParseFloat(v, 64); return f }},
	"uuid": {"[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}",
		func(v string) any { u, _ := uuid.Parse(strings.ReplaceAll(v, "-", "")); return u }},
}

var paramRE = regexp.MustCompile(`{([a-zA-Z_][a-zA-Z0-9_]*)(:[a-zA-Z_][a-zA-Z0-9_]*)?}`)

// Anchored regex, display format and convertors of a route path template
func compilePath(path string) (*regexp.Regexp, string, map[string]convertor) {
	pathRegex, pathFormat := "^", ""
	convertors := map[string]convertor{}
	dup := map[string]bool{}
	idx := 0
	for _, m := range paramRE.FindAllStringSubmatchIndex(path, -1) {
		name := path[m[2]:m[3]]
		ctype := "str"
		if m[4] >= 0 {
			ctype = path[m[4]+1 : m[5]]
		}
		conv, ok := convertorTypes[ctype]
		if !ok {
			panic(fmt.Sprintf("Unknown path convertor '%s'", ctype))
		}
		pathRegex += regexp.QuoteMeta(path[idx:m[0]]) + "(?P<" + name + ">" + conv.regex + ")"
		pathFormat += path[idx:m[0]] + "{" + name + "}"
		if _, seen := convertors[name]; seen {
			dup[name] = true
		}
		convertors[name] = conv
		idx = m[1]
	}
	if len(dup) > 0 {
		names := make([]string, 0, len(dup))
		for n := range dup {
			names = append(names, n)
		}
		sort.Strings(names)
		ending := ""
		if len(names) > 1 {
			ending = "s"
		}
		panic(fmt.Sprintf("Duplicated param name%s %s at path %s", ending, strings.Join(names, ", "), path))
	}
	pathRegex += regexp.QuoteMeta(path[idx:]) + "$"
	pathFormat += path[idx:]
	return regexp.MustCompile(pathRegex), pathFormat, convertors
}

// Scope path below the root path, the route matching input
func getRoutePath(s *Scope) string {
	if s.RootPath == "" || !strings.HasPrefix(s.Path, s.RootPath) {
		return s.Path
	}
	if s.Path == s.RootPath {
		return ""
	}
	if s.Path[len(s.RootPath)] == '/' {
		return s.Path[len(s.RootPath):]
	}
	return s.Path
}

// Converted parameters of a regex match merged over the scope path params
func matchParams(re *regexp.Regexp, convs map[string]convertor, s *Scope, routePath string) (map[string]any, bool) {
	m := re.FindStringSubmatch(routePath)
	if m == nil {
		return nil, false
	}
	params := map[string]any{}
	for k, v := range s.PathParams {
		params[k] = v
	}
	for i, name := range re.SubexpNames() {
		if conv, ok := convs[name]; ok && name != "" {
			params[name] = conv.convert(m[i])
		}
	}
	return params, true
}

// Mounted ASGI app under a path prefix, the static assets mount
type Mount struct {
	Path       string
	App        ASGIApp
	Name       string
	pathRegex  *regexp.Regexp
	convertors map[string]convertor
}

// Mount of app below path with its trailing slash dropped
func NewMount(path string, app ASGIApp, name string) *Mount {
	if path != "" && !strings.HasPrefix(path, "/") {
		panic("Routed paths must start with '/'")
	}
	m := &Mount{Path: strings.TrimRight(path, "/"), App: app, Name: name}
	m.pathRegex, _, m.convertors = compilePath(m.Path + "/{path:path}")
	return m
}

func (m *Mount) RoutePath() string { return m.Path }

func (m *Mount) Matches(s *Scope) (Match, *childScope) {
	if s.Type != "http" && s.Type != "websocket" {
		return MatchNone, nil
	}
	rootPath := s.RootPath
	routePath := getRoutePath(s)
	params, ok := matchParams(m.pathRegex, m.convertors, s, routePath)
	if !ok {
		return MatchNone, nil
	}
	remaining := "/" + params["path"].(string)
	delete(params, "path")
	matched := routePath[:len(routePath)-len(remaining)]
	appRoot, has := s.Extra["app_root_path"].(string)
	if !has {
		appRoot = rootPath
	}
	newRoot := rootPath + matched
	return MatchFull, &childScope{endpoint: m.App, pathParams: params, appRootPath: &appRoot, rootPath: &newRoot}
}

func (m *Mount) Handle(ctx context.Context, s *Scope, receive Receive, send Send) error {
	return m.App(ctx, s, receive, send)
}

// Router of routes in order with trailing slash redirects, the lifespan and the not found fallback
type Router struct {
	Routes        []BaseRoute
	RedirectSlash bool
	lifespanEnter func(ctx context.Context) error
	lifespanExit  func(ctx context.Context) error
}

// Dispatches a scope to the first full match, else the first partial match, else a redirect or not found
func (r *Router) app(ctx context.Context, s *Scope, receive Receive, send Send) error {
	if s.Router == nil {
		s.Router = r
	}
	if s.Type == "lifespan" {
		return r.lifespan(ctx, s, receive, send)
	}
	var partial BaseRoute
	var partialScope *childScope
	for _, route := range r.Routes {
		match, child := route.Matches(s)
		if match == MatchFull {
			s.Route = route
			child.apply(s)
			return route.Handle(ctx, s, receive, send)
		}
		if match == MatchPartial && partial == nil {
			partial, partialScope = route, child
		}
	}
	if partial != nil {
		s.Route = partial
		partialScope.apply(s)
		return partial.Handle(ctx, s, receive, send)
	}
	routePath := getRoutePath(s)
	if s.Type == "http" && r.RedirectSlash && routePath != "/" {
		redirect := s.Clone()
		if strings.HasSuffix(routePath, "/") {
			redirect.Path = strings.TrimRight(redirect.Path, "/")
		} else {
			redirect.Path += "/"
		}
		for _, route := range r.Routes {
			if match, _ := route.Matches(redirect); match != MatchNone {
				return RedirectResponse(URLOfScope(redirect).String(), 0).Serve(ctx, s, receive, send)
			}
		}
	}
	return r.notFound(ctx, s, receive, send)
}

// Closes an unmatched websocket, raises 404 inside an application, else answers plain text
func (r *Router) notFound(ctx context.Context, s *Scope, receive Receive, send Send) error {
	if s.Type == "websocket" {
		return websocketClose(1000, "")(ctx, s, receive, send)
	}
	if s.App != nil {
		return NewHTTPException(404, nil)
	}
	return PlainTextResponse("Not Found", 404).Serve(ctx, s, receive, send)
}

// Runs the lifespan context on startup and leaves it on shutdown, reporting failures to the server
func (r *Router) lifespan(ctx context.Context, s *Scope, receive Receive, send Send) error {
	if _, err := receive(ctx); err != nil {
		return err
	}
	if r.lifespanEnter != nil {
		if err := r.lifespanEnter(ctx); err != nil {
			if serr := send(ctx, Message{Type: "lifespan.startup.failed", Reason: pyTraceback(err)}); serr != nil {
				return serr
			}
			return err
		}
	}
	if err := send(ctx, Message{Type: "lifespan.startup.complete"}); err != nil {
		return err
	}
	if _, err := receive(ctx); err != nil {
		return err
	}
	if r.lifespanExit != nil {
		if err := r.lifespanExit(ctx); err != nil {
			if serr := send(ctx, Message{Type: "lifespan.shutdown.failed", Reason: pyTraceback(err)}); serr != nil {
				return serr
			}
			return err
		}
	}
	return send(ctx, Message{Type: "lifespan.shutdown.complete"})
}

// Text a lifespan failure event carries, the exception type and message
func pyTraceback(err error) string {
	return pyrepr.ExceptionName(err) + ": " + pyrepr.ExceptionMessage(err)
}
