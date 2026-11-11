package dashboard

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

var corsAllMethods = []string{"DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "QUERY"}

// Settings of a CORS middleware, the Starlette CORSMiddleware keyword arguments
type CORSOptions struct {
	AllowOrigins         []string
	AllowMethods         []string
	AllowHeaders         []string
	AllowCredentials     bool
	AllowOriginRegex     string
	AllowPrivateNetwork  bool
	ExposeHeaders        []string
	MaxAge               int
	IsAllowedOriginCheck func(origin string) bool
}

type corsMiddleware struct {
	app               ASGIApp
	o                 CORSOptions
	allowMethods      []string
	allowHeaders      []string
	allowAllOrigins   bool
	allowAllHeaders   bool
	preflightExplicit bool
	originRegex       *regexp.Regexp
	simpleHeaders     []string
	preflightHeaders  []string
}

// CORS middleware answering preflights and adding allow headers to simple responses
func CORSMiddleware(o CORSOptions) Middleware {
	return func(app ASGIApp) ASGIApp {
		m := &corsMiddleware{app: app, o: o, allowMethods: o.AllowMethods}
		if m.allowMethods == nil {
			m.allowMethods = []string{"GET"}
		}
		if slices.Contains(m.allowMethods, "*") {
			m.allowMethods = corsAllMethods
		}
		if o.AllowOriginRegex != "" {
			m.originRegex = regexp.MustCompile(`^(?:` + o.AllowOriginRegex + `)$`)
		}
		m.allowAllOrigins = slices.Contains(o.AllowOrigins, "*")
		m.allowAllHeaders = slices.Contains(o.AllowHeaders, "*")
		m.preflightExplicit = !m.allowAllOrigins || o.AllowCredentials
		if m.allowAllOrigins {
			m.simpleHeaders = append(m.simpleHeaders, "Access-Control-Allow-Origin", "*")
		}
		if o.AllowCredentials {
			m.simpleHeaders = append(m.simpleHeaders, "Access-Control-Allow-Credentials", "true")
		}
		if len(o.ExposeHeaders) > 0 {
			m.simpleHeaders = append(m.simpleHeaders, "Access-Control-Expose-Headers", strings.Join(o.ExposeHeaders, ", "))
		}
		maxAge := o.MaxAge
		if maxAge == 0 {
			maxAge = 600
		}
		m.preflightHeaders = []string{"Vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers, Access-Control-Request-Private-Network"}
		if !m.preflightExplicit {
			m.preflightHeaders = append(m.preflightHeaders, "Access-Control-Allow-Origin", "*")
		}
		m.preflightHeaders = append(m.preflightHeaders, "Access-Control-Allow-Methods", strings.Join(m.allowMethods, ", "),
			"Access-Control-Max-Age", strconv.Itoa(maxAge))
		set := map[string]bool{"Accept": true, "Accept-Language": true, "Content-Language": true, "Content-Type": true}
		for _, h := range o.AllowHeaders {
			set[h] = true
		}
		sorted := make([]string, 0, len(set))
		for h := range set {
			sorted = append(sorted, h)
		}
		sort.Strings(sorted)
		if !m.allowAllHeaders {
			m.preflightHeaders = append(m.preflightHeaders, "Access-Control-Allow-Headers", strings.Join(sorted, ", "))
		}
		if o.AllowCredentials {
			m.preflightHeaders = append(m.preflightHeaders, "Access-Control-Allow-Credentials", "true")
		}
		for _, h := range sorted {
			m.allowHeaders = append(m.allowHeaders, strings.ToLower(h))
		}
		return m.serve
	}
}

func (m *corsMiddleware) serve(ctx context.Context, scope *Scope, receive Receive, send Send) error {
	if scope.Type != "http" {
		return m.app(ctx, scope, receive, send)
	}
	headers := headersOfScope(scope)
	_, hasOrigin := headers.Lookup("origin")
	if hasOrigin && scope.Method == "OPTIONS" && headers.Contains("access-control-request-method") {
		return m.preflightResponse(headers).Serve(ctx, scope, receive, send)
	}
	return m.app(ctx, scope, receive, func(ctx context.Context, msg Message) error { return m.send(ctx, msg, send, headers) })
}

func (m *corsMiddleware) isAllowedOrigin(origin string) bool {
	if m.o.IsAllowedOriginCheck != nil {
		return m.o.IsAllowedOriginCheck(origin)
	}
	if m.allowAllOrigins {
		return true
	}
	if m.originRegex != nil && m.originRegex.MatchString(origin) {
		return true
	}
	return slices.Contains(m.o.AllowOrigins, origin)
}

// Dict of name and value pairs where a later name replaces an earlier one in place
func setPair(pairs []string, name, value string) []string {
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i] == name {
			pairs[i+1] = value
			return pairs
		}
	}
	return append(pairs, name, value)
}

// OK or a 400 naming the disallowed parts of a preflight request
func (m *corsMiddleware) preflightResponse(req Headers) *Response {
	origin := req.Get("origin")
	method := req.Get("access-control-request-method")
	reqHeaders, hasReqHeaders := req.Lookup("access-control-request-headers")
	_, privateNetwork := req.Lookup("access-control-request-private-network")
	headers := slices.Clone(m.preflightHeaders)
	var failures []string
	if m.isAllowedOrigin(origin) {
		if m.preflightExplicit {
			headers = setPair(headers, "Access-Control-Allow-Origin", origin)
		}
	} else {
		failures = append(failures, "origin")
	}
	if !slices.Contains(m.allowMethods, method) {
		failures = append(failures, "method")
	}
	if m.allowAllHeaders && hasReqHeaders {
		headers = setPair(headers, "Access-Control-Allow-Headers", reqHeaders)
	} else if hasReqHeaders {
		for _, h := range strings.Split(strings.ToLower(reqHeaders), ",") {
			if !slices.Contains(m.allowHeaders, strings.TrimSpace(h)) {
				failures = append(failures, "headers")
				break
			}
		}
	}
	if privateNetwork {
		if m.o.AllowPrivateNetwork {
			headers = setPair(headers, "Access-Control-Allow-Private-Network", "true")
		} else {
			failures = append(failures, "private-network")
		}
	}
	if len(failures) > 0 {
		return PlainTextResponse("Disallowed CORS "+strings.Join(failures, ", "), 400, headers...)
	}
	return PlainTextResponse("OK", 200, headers...)
}

// Adds the simple CORS headers to the response start of a request carrying an origin
func (m *corsMiddleware) send(ctx context.Context, msg Message, send Send, req Headers) error {
	if msg.Type != "http.response.start" {
		return send(ctx, msg)
	}
	raw := slices.Clone(msg.Headers)
	headers := headersOfRaw(&raw)
	origin, hasOrigin := req.Lookup("origin")
	if hasOrigin {
		headers.Update(m.simpleHeaders...)
	}
	if hasOrigin && m.allowAllOrigins && m.o.AllowCredentials {
		allowExplicitOrigin(headers, origin)
	} else if hasOrigin && !m.allowAllOrigins && m.isAllowedOrigin(origin) {
		allowExplicitOrigin(headers, origin)
	} else {
		headers.Set("Vary", strings.Join(append(headers.GetList("Vary"), "Origin"), ", "))
	}
	msg.Headers = raw
	return send(ctx, msg)
}

func allowExplicitOrigin(h Headers, origin string) {
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Vary", strings.Join(append(h.GetList("Vary"), "Origin"), ", "))
}
