package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Context key of the non launch profile home a service verb's ticket is bound to
type connectionProfileHomeKey struct{}

func init() {
	gateway.ServiceVerbs = serviceVerb
}

// Service table verb of a method name, nil when this package registers none
func serviceVerb(method string) gateway.ServiceVerb {
	if lookupMethod(method) == nil {
		return nil
	}
	return answerServiceVerb
}

// Runs a service table verb with the ticket profile home bound, refusing a ticket whose home stopped resolving
func answerServiceVerb(ctx context.Context, actor gateway.Principal, transport tuihost.Transport, req map[string]any) *pyjson.Object {
	ticketHome, err := foreignProfileHome(actor.ProfileID)
	if err != nil {
		return frameObject(profileUnavailable(req))
	}
	scope := ctx
	if ticketHome != "" {
		bound, target, ok := ticketProfileRequest(req, ticketHome)
		if !ok {
			return frameObject(bound)
		}
		req = bound
		scope = profileRuntimeScope(ctx, target, true)
	} else if resp, target, ok, named := explicitProfileRequest(req); named {
		if !ok {
			return frameObject(resp)
		}
		req = resp
		if target != "" {
			scope = profileRuntimeScope(ctx, target, true)
		}
	}
	if resp := Dispatch(withConnectionProfileHome(scope, ticketHome), req, transport); resp != nil {
		return frameObject(resp)
	}
	return nil
}

// Context bound to the non launch profile home of a service verb's ticket
func withConnectionProfileHome(ctx context.Context, h string) context.Context {
	return context.WithValue(ctx, connectionProfileHomeKey{}, h)
}

// Non launch profile home the current service verb's ticket is bound to, empty for none
func connectionProfileHome(ctx context.Context) string {
	h, _ := ctx.Value(connectionProfileHomeKey{}).(string)
	return h
}

// Non launch home a ticket names, empty for the launch profile, an error when that home stopped being a directory
func foreignProfileHome(profileID any) (string, error) {
	h := strOr(profileID)
	if !filepath.IsAbs(h) {
		return "", nil
	}
	if !isDir(h) {
		return "", &os.PathError{Op: "stat", Path: h, Err: os.ErrNotExist}
	}
	if resolveLax(h) == resolveLax(launchHome()) {
		return "", nil
	}
	return h, nil
}

// Profile unavailable error frame of a request
func profileUnavailable(req map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": req["id"], "error": map[string]any{"code": 4064, "message": "profile unavailable"}}
}

// Profile unavailable error frame of a request refused before any handler ran, its reason the bounded code a client keeps to tell this refusal from an outage inside a handler
func profileRefusedUnrouted(req map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": req["id"], "error": map[string]any{"code": 4064, "message": "profile unavailable",
		"data": map[string]any{"reason": "profile_unavailable"}}}
}

// Home an explicit profile param names, empty for launch, ok false with the refusal when unresolved
func explicitProfileRequest(req map[string]any) (resp map[string]any, home string, ok bool, named bool) {
	params, isDict := req["params"].(map[string]any)
	if !isDict {
		return nil, "", false, false
	}
	explicit := strings.TrimSpace(strOr(params["profile"]))
	if explicit == "" {
		return nil, "", false, false
	}
	resolved, err := profileHome(explicit)
	if err != nil {
		return profileRefusedUnrouted(req), "", false, true
	}
	return req, resolved, true, true
}

// Request bound to a non launch ticket profile and the home to scope, ok false with the refusal
func ticketProfileRequest(req map[string]any, ticketHome string) (map[string]any, string, bool) {
	params, isDict := req["params"].(map[string]any)
	if !isDict {
		params = map[string]any{}
	}
	refused := profileRefusedUnrouted(req)
	if resp, target, ok, named := explicitProfileRequest(req); named {
		return resp, target, ok
	}
	method, _ := req["method"].(string)
	typed := tuigateway.Lookup(tuigateway.KindMethod, method)
	if typed == nil || !typed.TakesParam("profile") {
		return req, ticketHome, true
	}
	name, _ := gateway.ProfileNameForHome(ticketHome)
	resolved := ""
	if name != "" {
		if dir, err := profileHome(name); err == nil {
			resolved = dir
		}
	}
	if resolved == "" || resolveLax(resolved) != resolveLax(ticketHome) {
		return refused, "", false
	}
	bound := make(map[string]any, len(req))
	for key, value := range req {
		bound[key] = value
	}
	scoped := make(map[string]any, len(params)+1)
	for key, value := range params {
		scoped[key] = value
	}
	scoped["profile"] = name
	bound["params"] = scoped
	return bound, ticketHome, true
}

