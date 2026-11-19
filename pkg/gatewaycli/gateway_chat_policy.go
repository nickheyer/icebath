package gatewaycli

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

var (
	// Creation flags that ride session.create as launch policy
	chatPolicyFields = []string{"model", "provider", "reasoning", "toolsets", "max_turns", "base_url", "ignore_rules", "api_key",
		"yolo", "safe_mode", "ignore_user_config", "skills", "checkpoints", "accept_hooks", "pass_session_id", "run_budget", "verbose"}
	// Environment equivalents of creation flags, taking effect only by riding this session's create
	envLaunchFlagNames = [][2]string{{"yolo", "ICEBATH_YOLO_MODE"}, {"ignore_rules", "ICEBATH_IGNORE_RULES"},
		{"safe_mode", "ICEBATH_SAFE_MODE"}, {"ignore_user_config", "ICEBATH_IGNORE_USER_CONFIG"},
		{"accept_hooks", "ICEBATH_ACCEPT_HOOKS"}}
)

// Creation flags set truthy in this shell's environment, in declaration order
func EnvLaunchFlags() *pyjson.Object {
	flags := pyjson.NewObject()
	for _, pair := range envLaunchFlagNames {
		if value, ok := os.LookupEnv(pair[1]); ok && provider.IsTruthyValue(value) {
			flags.Set(pair[0], true)
		}
	}
	return flags
}

// Allowlisted environment of this shell with the extra names of the profile config, code defaults on a bypass launch
func ShellClientEnv(ctx context.Context) (map[string]string, error) {
	flags := EnvLaunchFlags()
	if _, ok := flags.Get("safe_mode"); ok {
		return state.CollectClientEnv(nil, nil), nil
	}
	if _, ok := flags.Get("ignore_user_config"); ok {
		return state.CollectClientEnv(nil, nil), nil
	}
	cfg, err := config.LoadUserEffective(home.ConfigPath(home.Dir(ctx)), os.LookupEnv)
	if err != nil {
		return nil, err
	}
	return state.CollectClientEnv(nil, cfg), nil
}

// client_env of a new session when the gateway create contract takes it, nothing for an older gateway
func ClientEnvPolicy(ctx context.Context, parameters []any) (map[string]any, error) {
	for _, name := range parameters {
		if name == "client_env" {
			env, err := ShellClientEnv(ctx)
			if err != nil {
				return nil, err
			}
			plain := make(map[string]any, len(env))
			for k, v := range env {
				plain[k] = v
			}
			return map[string]any{"client_env": plain}, nil
		}
	}
	return map[string]any{}, nil
}

// Namespace value with string lists widened so they compare with decoded JSON
func policyValue(v any) any {
	if list, ok := v.([]string); ok {
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = item
		}
		return out
	}
	return v
}

// Creation flags the user passed, by identity so a zero max turns stays a value
func launchFlags(args map[string]any) *pyjson.Object {
	flags := pyjson.NewObject()
	for _, key := range chatPolicyFields {
		value := policyValue(args[key])
		if value == nil || value == false {
			continue
		}
		flags.Set(key, value)
	}
	return flags
}

// Comma separated names stripped, empties dropped
func splitNames(text string) []any {
	out := []any{}
	for _, name := range strings.Split(text, ",") {
		if name = pystr.Strip(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Launch flags as session.create policy, toolsets and skills as lists and the source as its create label
func RequestedPolicy(args map[string]any) *pyjson.Object {
	policy := launchFlags(args)
	if toolsets, ok := policy.Vals["toolsets"].(string); ok {
		policy.Set("toolsets", splitNames(toolsets))
	}
	if skills, ok := policy.Get("skills"); ok {
		raw, isList := skills.([]any)
		if !isList {
			raw = []any{skills}
		}
		names := []any{}
		seen := map[string]bool{}
		for _, item := range raw {
			for _, name := range splitNames(pyrepr.Str(item)) {
				if !seen[name.(string)] {
					seen[name.(string)] = true
					names = append(names, name)
				}
			}
		}
		policy.Set("skills", names)
		if len(names) == 0 {
			policy.Delete("skills")
		}
	}
	if pyrepr.Truthy(args["source"]) {
		policy.Set("source", pyrepr.Str(args["source"]))
	}
	return policy
}

// Launch flags of a resume that differ from the session's frozen launch, the payload of its launch mutation
func ResumeOverrides(ctx context.Context, args map[string]any, snapshot any) (map[string]any, error) {
	requested := RequestedPolicy(args)
	requested.Delete("source")
	if pyrepr.Truthy(args["in_dir"]) {
		resolved, err := pypath.Resolve(pypath.Expanduser(pyrepr.Str(args["in_dir"])))
		if err != nil {
			return nil, err
		}
		requested.Set("cwd", resolved)
	}
	overrides := map[string]any{}
	if requested.Len() == 0 {
		return overrides, nil
	}
	var info map[string]any
	if snap, ok := pyjson.AsObject(snapshot); ok {
		info, _ = plainDict(snap.Vals["info"])
	}
	frozen, _ := plainDict(info["launch_request"])
	if frozen == nil {
		frozen = map[string]any{}
	}
	if cwd, ok := info["cwd"]; ok {
		frozen["cwd"] = cwd
	}
	compared := map[string]any{}
	if model, ok := requested.Get("model"); ok {
		compared["model"] = model
		_, hasRequested := frozen["model_requested"]
		if pyjson.Equal(model, frozen["model_requested"]) {
			compared["model"] = frozen["model"]
		} else if !pyjson.Equal(model, frozen["model"]) && !hasRequested {
			flags := map[string]any{}
			for _, key := range []string{"model", "provider", "base_url", "api_key"} {
				if value, ok := requested.Get(key); ok {
					flags[key] = value
				}
			}
			cfg, err := config.LoadUserEffective(home.ConfigPath(home.Dir(ctx)), os.LookupEnv)
			if err != nil {
				return nil, err
			}
			if cfg == nil {
				cfg = map[string]any{}
			}
			route, err := gateway.ResolveLaunchRoute(ctx, flags, cfg)
			if err != nil {
				return nil, err
			}
			compared["model"] = route["model"]
		}
	}
	for _, key := range requested.Keys {
		value, ok := compared[key]
		if !ok {
			value = requested.Vals[key]
		}
		if key != "api_key" && !pyjson.Equal(frozen[key], value) {
			overrides[key] = requested.Vals[key]
		}
	}
	return overrides, nil
}
