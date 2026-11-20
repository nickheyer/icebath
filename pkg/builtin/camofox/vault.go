package camofox

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Hosts a credential may reach over plain HTTP
var loopbackHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// Whether a credential may go to the Camofox URL, HTTPS or loopback HTTP with no userinfo, query or fragment
func secretTransportOK(raw string) (bool, error) {
	parts, err := pyurl.Split(raw)
	if err != nil {
		return false, &pyError{"ValueError", err.Error()}
	}
	user, _, pass, _ := netlocUserinfo(parts.Netloc)
	if user != "" || pass != "" || parts.Query != "" || parts.Fragment != "" {
		return false, nil
	}
	return parts.Scheme == "https" || (parts.Scheme == "http" && loopbackHosts[parts.Hostname()]), nil
}

// browser_vault page backend evaluating on the tab browser_navigate opened, bound to the plugin's profile
type pageBackend struct {
	provider.ProviderDefaults
	homeCtx context.Context
}

// Page backend whose default methods dispatch to itself
func newPageBackend(homeCtx context.Context) *pageBackend {
	b := &pageBackend{homeCtx: homeCtx}
	b.ProviderDefaults.Self = b
	return b
}

// Registry name camofox
func (*pageBackend) Name() string { return "camofox" }

// Label Camofox
func (*pageBackend) DisplayName() string { return "Camofox" }

// Scope of the profile the backend was registered for
func (b *pageBackend) scope() *scope { return newScope(b.homeCtx, nil) }

// Whether the task already holds a Camofox tab, never opening one
func (b *pageBackend) HasPage(taskID string) bool {
	s := b.scope()
	if s.camofoxURL() == "" {
		return false
	}
	ss := s.trackedSession(taskID)
	return ss != nil && ss.hasTab()
}

// Evaluates on the task tab over REST, a secret script sent only to loopback or HTTPS without proxy or redirect
func (b *pageBackend) Evaluate(taskID, expression string, secret bool) (any, error) {
	s := b.scope()
	ss := s.trackedSession(taskID)
	if ss == nil || !ss.hasTab() {
		return nil, &pyError{"RuntimeError", noSessionError}
	}
	if secret {
		ok, err := secretTransportOK(s.camofoxURL())
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &pyError{"RuntimeError", "vault fills need CAMOFOX_URL on loopback or HTTPS"}
		}
	}
	reply, err := s.request("POST", tabPath(ss, "evaluate"), commandTimeoutDefault, nil,
		pyjson.ObjectOf("expression", expression, "userId", ss.userID), true)
	if err != nil {
		return nil, err
	}
	resp, err := decodeJSON(reply.Body)
	if err != nil {
		return nil, err
	}
	obj, isObj := resp.(*pyjson.Object)
	var result any
	var hasResult bool
	var failure any
	if isObj && obj != nil {
		result, hasResult = obj.Get("result")
		failure, _ = obj.Get("error")
	}
	if !isObj || obj == nil || !hasResult || pyrepr.Truthy(failure) {
		text := ""
		if isObj && obj != nil {
			text = pyrepr.Str(failure)
		}
		if text == "" {
			text = "evaluate failed"
		}
		return nil, &pyError{"RuntimeError", text}
	}
	return result, nil
}
