package camofox

import (
	"context"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Hosts a credential may reach over plain HTTP
var loopbackHosts = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// Whether a credential may go to the Camofox URL, HTTPS or loopback HTTP with no userinfo, query or fragment
func secretTransportOK(raw string) (bool, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return false, &kindError{"ValueError", err.Error()}
	}
	user := u.User.Username()
	pass, _ := u.User.Password()
	if user != "" || pass != "" || u.RawQuery != "" || u.Fragment != "" {
		return false, nil
	}
	return u.Scheme == "https" || (u.Scheme == "http" && loopbackHosts[strings.ToLower(u.Hostname())]), nil
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
		return nil, &kindError{"RuntimeError", noSessionError}
	}
	if secret {
		ok, err := secretTransportOK(s.camofoxURL())
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &kindError{"RuntimeError", "vault fills need CAMOFOX_URL on loopback or HTTPS"}
		}
	}
	reply, err := s.request("POST", tabPath(ss, "evaluate"), commandTimeoutDefault, nil,
		jsonutil.ObjectOf("expression", expression, "userId", ss.userID), true)
	if err != nil {
		return nil, err
	}
	resp, err := decodeJSON(reply.Content)
	if err != nil {
		return nil, err
	}
	obj, isObj := resp.(*jsonutil.Object)
	var result any
	var hasResult bool
	var failure any
	if isObj && obj != nil {
		result, hasResult = obj.Get("result")
		failure, _ = obj.Get("error")
	}
	if !isObj || obj == nil || !hasResult || anyutil.IsTrue(failure) {
		text := ""
		if isObj && obj != nil {
			text = anyutil.String(failure)
		}
		if text == "" {
			text = "evaluate failed"
		}
		return nil, &kindError{"RuntimeError", text}
	}
	return result, nil
}
