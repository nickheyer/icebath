package mcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Completed or abandoned sessions linger this long, and in flight flows are capped
const (
	oauthSessionTTL = 900 * time.Second
	oauthMaxPending = 12
)

// Worker of an RPC session flow, swapped by tests
var oauthSessionWorker = RunOAuthWorker

type oauthSession struct {
	sessionID  string
	serverName string
	homeDir    string
	flow       *DashboardOAuthFlow
	createdAt  time.Time

	mu    sync.Mutex
	httpd *http.Server
}

var (
	oauthSessionsMu sync.Mutex
	oauthSessions   = map[string]*oauthSession{}
)

// Stops the session's backend listener once
func (rec *oauthSession) shutdownListener() {
	rec.mu.Lock()
	srv := rec.httpd
	rec.httpd = nil
	rec.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// Registers a callback relay flow so RPC and card starts share ownership checks
func RegisterOAuthFlow(flow *DashboardOAuthFlow, httpd *http.Server) *oauthSession {
	rec := &oauthSession{sessionID: flow.FlowID, serverName: flow.ServerName, homeDir: flow.HomeDir, flow: flow,
		httpd: httpd, createdAt: time.Now()}
	oauthSessionsMu.Lock()
	oauthSessions[flow.FlowID] = rec
	oauthSessionsMu.Unlock()
	return rec
}

// Releases a finished flow's listener and keeps its relay visible outcome
func FinishOAuthFlow(sessionID string) {
	oauthSessionsMu.Lock()
	rec := oauthSessions[sessionID]
	oauthSessionsMu.Unlock()
	if rec != nil {
		rec.shutdownListener()
	}
}

// Begins an RPC MCP OAuth flow and returns its session id and authorization URL once published
func StartOAuthFlow(ctx context.Context, env *tools.Env, homeDir, serverName string, cfg map[string]any, reconnectLive bool,
	urlTimeout time.Duration, clientRedirectURI *string) (*pyjson.Object, error) {
	if clientRedirectURI != nil {
		uri, err := ValidateClientRedirectURI(*clientRedirectURI)
		if err != nil {
			return nil, err
		}
		clientRedirectURI = &uri
	}
	cutoff := time.Now().Add(-oauthSessionTTL)
	var expired []*oauthSession
	oauthSessionsMu.Lock()
	for sid, rec := range oauthSessions {
		if rec.createdAt.Before(cutoff) {
			expired = append(expired, rec)
			delete(oauthSessions, sid)
		}
	}
	oauthSessionsMu.Unlock()
	for _, rec := range expired {
		rec.shutdownListener()
	}
	oauthSessionsMu.Lock()
	active := 0
	busy := false
	for _, rec := range oauthSessions {
		if rec.flow.WorkerDone() {
			continue
		}
		active++
		busy = busy || (rec.serverName == serverName && rec.homeDir == homeDir)
	}
	oauthSessionsMu.Unlock()
	if active >= oauthMaxPending {
		return nil, errors.New("Too many MCP OAuth flows are already in progress")
	}
	if busy {
		return nil, fmt.Errorf("MCP OAuth for '%s' is already in progress", serverName)
	}
	sessionID := tokenURLSafe(24)
	flow := NewDashboardOAuthFlow(sessionID, serverName, "", homeDir, "", reconnectLive)
	httpd, err := ChooseCallbackReceiver(flow, cfg, clientRedirectURI)
	if err != nil {
		return nil, err
	}
	rec := RegisterOAuthFlow(flow, httpd)
	go oauthSessionWorker(context.WithoutCancel(ctx), env, homeDir, serverName, maps.Clone(cfg), reconnectLive,
		OAuthWorkerOptions{Flow: flow, OnDone: rec.shutdownListener})
	authURL, err := func() (string, error) {
		deadline := time.Now().Add(urlTimeout)
		for time.Now().Before(deadline) {
			snap := flow.Snapshot()
			if u, _ := snap["authorization_url"].(string); u != "" {
				return u, nil
			}
			if snap["status"] == "error" {
				msg, _ := snap["error"].(string)
				return "", errors.New(orText(msg, "MCP OAuth flow failed before authorization"))
			}
			time.Sleep(100 * time.Millisecond)
		}
		return "", &TimeoutError{}
	}()
	if err != nil {
		var te *TimeoutError
		if errors.As(err, &te) {
			err = errors.New("Timed out waiting for MCP authorization URL")
		}
		flow.MarkError(ExceptionMessage(err), false)
		rec.shutdownListener()
		return nil, err
	}
	out := pyjson.NewObject()
	out.Set("session_id", sessionID)
	out.Set("auth_url", authURL)
	out.Set("flow", "pkce")
	return out, nil
}

// The session when it belongs to the caller's resolved profile, else the refusal text
func oauthLookup(ctx context.Context, sessionID, serverName, homeDir string) (*oauthSession, string) {
	oauthSessionsMu.Lock()
	rec := oauthSessions[sessionID]
	oauthSessionsMu.Unlock()
	if rec == nil {
		return nil, "OAuth session not found or expired"
	}
	if rec.serverName != serverName {
		return nil, "server name mismatch for session"
	}
	if homeDir == "" {
		homeDir = home.Dir(ctx)
	}
	if home.Key(rec.homeDir) != home.Key(homeDir) {
		return nil, "profile mismatch for session"
	}
	return rec, ""
}

// Polls a session for pending, approved or error with its link and tools
func PollOAuthFlow(ctx context.Context, sessionID, serverName string) *pyjson.Object {
	out := pyjson.NewObject()
	rec, msg := oauthLookup(ctx, sessionID, serverName, "")
	if rec == nil {
		out.Set("status", "error")
		out.Set("error_message", msg)
		return out
	}
	snap := rec.flow.Snapshot()
	status, _ := snap["status"].(string)
	if status != "approved" && status != "error" {
		status = "pending"
	}
	out.Set("session_id", sessionID)
	out.Set("status", status)
	out.Set("error_message", snap["error"])
	out.Set("auth_url", snap["authorization_url"])
	if status == "approved" {
		list := make([]any, len(rec.flow.Tools))
		for i, t := range rec.flow.Tools {
			list[i] = t
		}
		out.Set("tools", list)
	}
	return out
}

// Cancels only the owning profile's flow and releases its callback waiter
func CancelOAuthFlow(ctx context.Context, sessionID, serverName, homeDir string) *pyjson.Object {
	out := pyjson.NewObject()
	rec, msg := oauthLookup(ctx, sessionID, serverName, homeDir)
	if rec == nil {
		out.Set("ok", false)
		out.Set("error_message", msg)
		return out
	}
	rec.flow.MarkError("OAuth cancelled by user", true)
	rec.shutdownListener()
	out.Set("ok", true)
	out.Set("status", rec.flow.Snapshot()["status"])
	return out
}

// Relays a client captured OAuth redirect into the session's flow, which still checks state and replays
func DeliverOAuthCallbackFlow(ctx context.Context, sessionID, serverName, code, state, errText, iss string) *pyjson.Object {
	out := pyjson.NewObject()
	rec, msg := oauthLookup(ctx, sessionID, serverName, "")
	if rec == nil {
		out.Set("ok", false)
		out.Set("error_message", msg)
		return out
	}
	if err := rec.flow.DeliverCallback(code, state, errText, iss); err != nil {
		out.Set("ok", false)
		out.Set("error_message", err.Error())
		return out
	}
	out.Set("ok", true)
	out.Set("session_id", sessionID)
	return out
}
