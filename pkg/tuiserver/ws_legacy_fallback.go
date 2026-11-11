package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Context key of the non launch profile home a legacy fallback request's ticket is bound to
type connectionProfileHomeKey struct{}

// Namespaces the session authority owns, legacy handlers refused except the store wide reads
var sessionNamespaces = []string{"session.", "prompt.", "message."}

// Store wide session reads and foreign imports a legacy handler may still serve
var sessionFallbackAllowed = map[string]bool{
	"session.most_recent": true, "session.active_list": true, "session.events.stats": true,
	"session.foreign.list": true, "session.foreign.preview": true, "session.foreign.import": true,
}

// Sidecar verbs acting on process global state, never reachable through the legacy fallback
var processGlobal = map[string]bool{"process.stop": true, "delegation.pause": true, "reload.mcp": true, "reload.env": true, "agents.list": true}

// Whether a connection's grant covers the interactive purpose and the method is no authority owned or process global verb
func legacyFallbackAllowed(capabilities []string, method string) bool {
	if processGlobal[method] {
		return false
	}
	for _, prefix := range sessionNamespaces {
		if strings.HasPrefix(method, prefix) && !sessionFallbackAllowed[method] {
			return false
		}
	}
	held := map[string]bool{}
	for _, capability := range capabilities {
		held[capability] = true
	}
	for capability := range gateway.PurposeCapabilities("interactive") {
		if !held[capability] {
			return false
		}
	}
	return true
}

// Context bound to the non launch profile home of a legacy fallback request's ticket
func withConnectionProfileHome(ctx context.Context, h string) context.Context {
	return context.WithValue(ctx, connectionProfileHomeKey{}, h)
}

// Non launch profile home the current legacy fallback request's ticket is bound to, empty for none
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
		return profileUnavailable(req), "", false, true
	}
	return req, resolved, true, true
}

// Request bound to a non launch ticket profile and the home to scope, ok false with the refusal
func ticketProfileRequest(req map[string]any, ticketHome string) (map[string]any, string, bool) {
	params, isDict := req["params"].(map[string]any)
	if !isDict {
		params = map[string]any{}
	}
	refused := profileUnavailable(req)
	if resp, target, ok, named := explicitProfileRequest(req); named {
		return resp, target, ok
	}
	method, _ := req["method"].(string)
	contract := tuigateway.Methods[method]
	if contract == nil || !slices.Contains(contract.Params.ModelFields(), "profile") {
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

// Store wide session read of a non launch ticket home, bound to that profile or answered from its live sessions
func ownProfileRead(ctx context.Context, req map[string]any, ticketHome string) map[string]any {
	params, isDict := req["params"].(map[string]any)
	if !isDict {
		params = map[string]any{}
	}
	name := strOr(params["profile"])
	if name == "" {
		name, _ = gateway.ProfileNameForHome(ticketHome)
	}
	resolved := ""
	if name != "" {
		if dir, err := profileHome(name); err == nil {
			resolved = dir
		}
	}
	if resolved == "" || resolveLax(resolved) != resolveLax(ticketHome) {
		return profileUnavailable(req)
	}
	if req["method"] != "session.active_list" {
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
		return bound
	}
	current := strOr(params["current_session_id"])
	rows := []any{}
	for _, live := range tuigateway.LiveSessions() {
		owner := strOr(live.Session.Get("profile_home"))
		if owner == "" {
			owner = launchHome()
		}
		if !truthy(live.Session.Get("_finalized")) && resolveLax(owner) == resolveLax(ticketHome) {
			rows = append(rows, sessionLiveItem(ctx, live.SID, live.Session, current))
		}
	}
	return map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"sessions": rows}}
}
