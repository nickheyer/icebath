package gatewaycli

import (
	"context"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
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
func EnvLaunchFlags() *jsonutil.Object {
	flags := jsonutil.NewObject()
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

// client_env of a new session created from this shell
func ClientEnvPolicy(ctx context.Context) (map[string]any, error) {
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
func launchFlags(args map[string]any) *jsonutil.Object {
	flags := jsonutil.NewObject()
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
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Launch flags as session.create policy, toolsets and skills as lists and the source as its create label
func RequestedPolicy(args map[string]any) *jsonutil.Object {
	policy := launchFlags(args)
	if toolsets, ok := policy.Value("toolsets").(string); ok {
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
			for _, name := range splitNames(anyutil.String(item)) {
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
	if anyutil.IsTrue(args["source"]) {
		policy.Set("source", anyutil.String(args["source"]))
	}
	return policy
}

// Launch flags of a resume that differ from the session's frozen launch, the payload of its launch mutation
func ResumeOverrides(ctx context.Context, args map[string]any, snapshot any) (map[string]any, error) {
	requested := RequestedPolicy(args)
	requested.Delete("source")
	if anyutil.IsTrue(args["in_dir"]) {
		resolved, err := pathutil.Resolve(pathutil.ExpandUser(anyutil.String(args["in_dir"])))
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
	if snap, ok := jsonutil.AsObject(snapshot); ok {
		info, _ = plainDict(snap.Value("info"))
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
		if anyutil.Equal(model, frozen["model_requested"]) {
			compared["model"] = frozen["model"]
		} else if !anyutil.Equal(model, frozen["model"]) && !hasRequested {
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
	for _, key := range requested.Keys() {
		value, ok := compared[key]
		if !ok {
			value = requested.Value(key)
		}
		if key != "api_key" && !anyutil.Equal(frozen[key], value) {
			overrides[key] = requested.Value(key)
		}
	}
	return overrides, nil
}
