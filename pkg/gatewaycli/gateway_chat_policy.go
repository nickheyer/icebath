package gatewaycli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
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
		"yolo", "safe_mode", "ignore_user_config", "skills", "checkpoints", "accept_hooks", "pass_session_id"}
	// Environment equivalents of creation flags, taking effect only by riding this session's create
	envLaunchFlagNames = [][2]string{{"yolo", "ICEBATH_YOLO_MODE"}, {"ignore_rules", "ICEBATH_IGNORE_RULES"},
		{"safe_mode", "ICEBATH_SAFE_MODE"}, {"ignore_user_config", "ICEBATH_IGNORE_USER_CONFIG"},
		{"accept_hooks", "ICEBATH_ACCEPT_HOOKS"}}
	// Refused resume flag as the user typed it and the slash command that changes it after resuming
	resumeFlagHints = map[string][2]string{"model": {"-m/--model", "/model"}, "provider": {"--provider", "/model"},
		"base_url": {"--base-url", "/model"}, "reasoning": {"--reasoning", "/reasoning"},
		"yolo": {"--yolo", "/yolo"}, "toolsets": {"-t/--toolsets", ""},
		"skills": {"-s/--skills", ""}, "verbose": {"-v/--verbose", ""}, "cwd": {"--in", ""}}
)

const resumePolicyMismatch = "Resume retains gateway session policy; creation overrides are unsupported on resume."

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

// Resume refusal naming each flag that differs from the frozen launch and the way forward
func ResumePolicyError(keys []string) *gateway.GatewayClientError {
	var flags, switches []string
	seenFlag, seenSwitch := map[string]bool{}, map[string]bool{}
	for _, key := range keys {
		hint, ok := resumeFlagHints[key]
		if !ok {
			hint = [2]string{"--" + strings.ReplaceAll(key, "_", "-"), ""}
		}
		if !seenFlag[hint[0]] {
			seenFlag[hint[0]] = true
			flags = append(flags, hint[0])
		}
		if hint[1] != "" && !seenSwitch[hint[1]] {
			seenSwitch[hint[1]] = true
			switches = append(switches, hint[1])
		}
	}
	one := len(keys) == 1
	them, differ := "them", "differ"
	if one {
		them, differ = "it", "differs"
	}
	after := fmt.Sprintf(", or start a new session to use %s", them)
	if len(switches) > 0 {
		after = fmt.Sprintf(" and use %s after resuming to switch", strings.Join(switches, " or "))
	}
	return &gateway.GatewayClientError{Msg: fmt.Sprintf("%s %s %s from this session's launch: resume without %s%s.",
		resumePolicyMismatch, strings.Join(flags, ", "), differ, them, after)}
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
		policy.Set("source", agent.CreateSurface(pyrepr.Str(args["source"])))
	}
	return policy
}

// Refuses resume flags that would change the frozen route, the same flags again being fine
func CheckResumePolicy(ctx context.Context, args map[string]any, snapshot any) error {
	requested := RequestedPolicy(args)
	if pyrepr.Truthy(args["in_dir"]) {
		resolved, err := pypath.Resolve(pypath.Expanduser(pyrepr.Str(args["in_dir"])))
		if err != nil {
			return err
		}
		requested.Set("cwd", resolved)
	}
	if requested.Len() == 0 {
		return nil
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
	if model, ok := requested.Get("model"); ok {
		_, hasRequested := frozen["model_requested"]
		if pyjson.Equal(model, frozen["model_requested"]) {
			requested.Set("model", frozen["model"])
		} else if !pyjson.Equal(model, frozen["model"]) && !hasRequested {
			flags := map[string]any{}
			for _, key := range []string{"model", "provider", "base_url", "api_key"} {
				if value, ok := requested.Get(key); ok {
					flags[key] = value
				}
			}
			cfg, err := config.LoadUserEffective(home.ConfigPath(home.Dir(ctx)), os.LookupEnv)
			if err != nil {
				return err
			}
			if cfg == nil {
				cfg = map[string]any{}
			}
			route, err := gateway.ResolveLaunchRoute(ctx, flags, cfg)
			if err != nil {
				return err
			}
			requested.Set("model", route["model"])
		}
	}
	var differs []string
	for _, key := range requested.Keys {
		if key != "api_key" && !pyjson.Equal(frozen[key], requested.Vals[key]) {
			differs = append(differs, key)
		}
	}
	if len(differs) > 0 {
		return ResumePolicyError(differs)
	}
	return nil
}
