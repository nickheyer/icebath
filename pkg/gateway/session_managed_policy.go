package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Frozen policy of a session executing out of process, bypass sessions always, others under the custom provider opt in
func managedPolicy(ctx context.Context, a *SessionAuthority, ref SessionRef) (*LocalSessionPolicy, error) {
	live, ok := a.live(ref.SessionID)
	if !ok {
		return nil, &keyError{ref.SessionID}
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
			return nil, &errorutil.Error{Kind: "AttributeError", Msg: "the managed policy section is " + errchain.Kind(section) + ", expected a mapping"}
		}
		managed = gateway["managed_workers"]
	}
	if managed != true {
		return nil, nil
	}
	decoded, err := jsonutil.Decode(policy.RequestJSON)
	if err != nil {
		return nil, err
	}
	request, isObj := decoded.(*jsonutil.Object)
	if !isObj {
		return nil, &errorutil.Error{Kind: "AttributeError", Msg: "the managed policy is " + errchain.Kind(decoded) + ", expected a mapping"}
	}
	provider, _ := request.Get("provider")
	baseURL, _ := request.Get("base_url")
	if policy.Platform != "cli" || provider != "custom" || !anyutil.IsTrue(baseURL) {
		return nil, runtimeRefusal("unsupported_managed_policy")
	}
	return policy, nil
}
