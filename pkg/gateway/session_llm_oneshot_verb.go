package gateway

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Main runtime of a live agent for the auxiliary client, assigned from pkg/tuiserver in cmd/icebath/wiring.go
var MainRuntimeFromAgent func(agent any) map[string]any

// One shot model call of pkg/agent, assigned in cmd/icebath/wiring.go
var RunOneshot func(ctx context.Context, instructions, userInput, template string, variables map[string]any, task string,
	maxTokens int, temperature float64, mainRuntime map[string]any) (string, error)

// Runner resolving a policy's primary model route without walking its fallback chain
type policyRuntimeResolver interface {
	ResolvePolicyAgentRuntime(ctx context.Context, policy *LocalSessionPolicy, fallback bool) (string, map[string]any, error)
}

// One shot verb of a connection keyed by method name
func oneshotHandlers(c *AuthorityConnection) map[string]controlHandler {
	return map[string]controlHandler{"llm.oneshot": func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
		return oneshot(ctx, c, ref, params)
	}}
}

// Main runtime of the session's resident agent, else its frozen launch route resolved in the worker
func sessionRuntime(ctx context.Context, a *SessionAuthority, ref SessionRef) (func(ctx context.Context) (map[string]any, error), error) {
	agent, err := a.agent(ref)
	if err != nil {
		return nil, err
	}
	if !nilAgent(agent) {
		runtime := MainRuntimeFromAgent(agent)
		return func(context.Context) (map[string]any, error) { return runtime, nil }, nil
	}
	live, err := liveSession(a, ref.SessionID)
	if err != nil {
		return nil, err
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return func(context.Context) (map[string]any, error) { return nil, nil }, nil
	}
	return func(ctx context.Context) (map[string]any, error) {
		resolver, ok := a.Runner.(policyRuntimeResolver)
		if !ok {
			return nil, &PyError{Kind: "AttributeError", Msg: "runner cannot resolve a policy agent runtime"}
		}
		model, kwargs, err := resolver.ResolvePolicyAgentRuntime(ctx, policy, false)
		if err != nil {
			return nil, err
		}
		runtime := map[string]any{}
		for _, key := range []string{"provider", "base_url", "api_key", "api_mode"} {
			if pyrepr.Truthy(kwargs[key]) {
				runtime[key] = kwargs[key]
			}
		}
		if model != "" {
			runtime["model"] = model
			return runtime, nil
		}
		if len(runtime) == 0 {
			return nil, nil
		}
		return runtime, nil
	}, nil
}

// Text of a validated string param, empty for a missing or null one
func oneshotParamText(params *pyjson.Object, key string) string {
	value, _ := params.GetOr(key, "").(string)
	return value
}

// One shot generation on the owner's pool, answered later as a deferred reply
func oneshot(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if err := validateOneshot(params); err != nil {
		return nil, err
	}
	a := c.authority
	runtime := func(context.Context) (map[string]any, error) { return nil, nil }
	if ref.SessionID != "" {
		if err := authorizeBusy(ctx, a, c.actor, ref, params, "session:read"); err != nil {
			return nil, err
		}
		resolved, err := sessionRuntime(ctx, a, ref)
		if err != nil {
			return nil, err
		}
		runtime = resolved
	} else {
		if err := requireServing(a); err != nil {
			return nil, err
		}
		if !c.actor.Capabilities["session:read"] {
			return nil, runtimeRefusal("permission_denied")
		}
		if profile := params.GetOr("profile", nil); pyrepr.Truthy(profile) {
			name, isText := profile.(string)
			if !isText || !profiles.ProfileMatchesHome(ctx, name, a.ProfileID) {
				return nil, runtimeRefusal("profile_mismatch")
			}
		}
	}
	return NewDeferredReply(func(ctx context.Context) (any, error) {
		scoped := ownerScope(ctx, a, false)
		variables := map[string]any{}
		if raw, ok := pyjson.Plain(params.GetOr("variables", nil)).(map[string]any); ok {
			variables = raw
		}
		task := pystr.Strip(oneshotParamText(params, "task"))
		if task == "" {
			task = "title_generation"
		}
		maxTokens := int64(1024)
		if raw := params.GetOr("max_tokens", nil); pyrepr.Truthy(raw) {
			n, err := pyint.Int64(raw)
			if err != nil {
				return nil, err
			}
			maxTokens = n
		}
		temperature := 0.3
		if raw := params.GetOr("temperature", nil); raw != nil {
			f, err := pyint.Float(raw)
			if err != nil {
				return nil, err
			}
			temperature = f
		}
		main, err := runtime(scoped)
		if err != nil {
			return nil, err
		}
		reply, err := RunOneshot(scoped, oneshotParamText(params, "instructions"), oneshotParamText(params, "input"),
			pystr.Strip(oneshotParamText(params, "template")), variables, task, int(maxTokens), temperature, main)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("text", reply), nil
	}), nil
}
