package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Frozen policy of a session executing out of process, bypass sessions always, others under the custom provider opt in
func managedPolicy(ctx context.Context, a *SessionAuthority, ref SessionRef) (*LocalSessionPolicy, error) {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &pyKeyError{ref.SessionID}
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil || policy == nil {
		return nil, err
	}
	if policy.IgnoreUserConfig || policy.KanbanJSON != nil {
		return policy, nil
	}
	cfg, err := policy.config(ctx, nil, true)
	if err != nil {
		return nil, err
	}
	managed := any(nil)
	if section, present := cfg["gateway"]; present {
		gateway, isMap := section.(map[string]any)
		if !isMap {
			return nil, &PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(section) + "' object has no attribute 'get'"}
		}
		managed = gateway["managed_workers"]
	}
	if managed != true {
		return nil, nil
	}
	decoded, err := pyjson.Loads(policy.RequestJSON)
	if err != nil {
		return nil, err
	}
	request, isObj := decoded.(*pyjson.Object)
	if !isObj {
		return nil, &PyError{Kind: "AttributeError", Msg: "'" + jsonValueTypeName(decoded) + "' object has no attribute 'get'"}
	}
	provider, _ := request.Get("provider")
	baseURL, _ := request.Get("base_url")
	if policy.Platform != "cli" || provider != "custom" || !pyTruthy(baseURL) {
		return nil, runtimeRefusal("unsupported_managed_policy")
	}
	return policy, nil
}
