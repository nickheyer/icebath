package dashboard

import (
	"context"
	"regexp"
	"slices"
	"strings"
)

// Starlette Route whose endpoint is an ASGI app instance, served as is for its methods
type ASGIRoute struct {
	Path      string
	Methods   []string
	App       ASGIApp
	Endpoint  any
	pathRegex *regexp.Regexp
	convs     map[string]convertor
}

// Route of an ASGI app endpoint for the methods, HEAD added beside GET, every method when none are given
func NewASGIRoute(path string, endpoint any, app ASGIApp, methods []string) *ASGIRoute {
	r := &ASGIRoute{Path: path, App: app, Endpoint: endpoint}
	if methods != nil {
		for _, m := range methods {
			if u := strings.ToUpper(m); !slices.Contains(r.Methods, u) {
				r.Methods = append(r.Methods, u)
			}
		}
		if slices.Contains(r.Methods, "GET") && !slices.Contains(r.Methods, "HEAD") {
			r.Methods = append(r.Methods, "HEAD")
		}
	}
	r.pathRegex, _, r.convs = compilePath(path)
	return r
}

func (r *ASGIRoute) RoutePath() string { return r.Path }

func (r *ASGIRoute) Matches(s *Scope) (Match, *childScope) {
	if s.Type != "http" {
		return MatchNone, nil
	}
	params, ok := matchParams(r.pathRegex, r.convs, s, getRoutePath(s))
	if !ok {
		return MatchNone, nil
	}
	child := &childScope{endpoint: r.Endpoint, pathParams: params}
	if r.Methods != nil && !slices.Contains(r.Methods, s.Method) {
		return MatchPartial, child
	}
	return MatchFull, child
}

// Raises 405 with the allowed methods for another method inside an application, else runs the app
func (r *ASGIRoute) Handle(ctx context.Context, s *Scope, receive Receive, send Send) error {
	if r.Methods != nil && !slices.Contains(r.Methods, s.Method) {
		allow := strings.Join(r.Methods, ", ")
		if s.App != nil {
			return NewHTTPException(405, nil, "Allow", allow)
		}
		return PlainTextResponse("Method Not Allowed", 405, "Allow", allow).Serve(ctx, s, receive, send)
	}
	return r.App(ctx, s, receive, send)
}
