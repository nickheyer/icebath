package plugins

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

type hostSDKOp func(ctx context.Context, h *PluginHost, args []any, kwargs map[string]any) (any, error)

// SDK calls this package answers itself for the host profile
var hostSDKOps = map[string]hostSDKOp{
	"redact_sensitive_text": sdkRedactSensitiveText,
	"get_env_value":         sdkGetEnvValue,
	"build_subprocess_env":  sdkBuildSubprocessEnv,
	"plugin_db_prepare":     sdkPluginDBPrepare,
}

// SDK calls the top level wiring answers through Services.PluginSDK
var hostSDKServiceOps = map[string]bool{
	"provider_media.save_url": true, "read_credential_pool": true, "open_credentialed_url": true,
	"chat_catalog_ids": true, "format_secret_source_suffix": true, "get_version_info": true,
	"cron.recover_interrupted": true, "cron.claim_fire": true, "cron.fire_claimed": true, "moved_command_hint": true,
}

// Runs one plugin SDK call the child sent and encodes its result for the child
func (h *PluginHost) serveSDK(ctx context.Context, params map[string]any) (any, error) {
	op := wireStr(params["op"])
	resolve := func(ref map[string]any) (any, error) { return h.resolveRef(ref, "") }
	args, err := wireDecode(listOrEmpty(params["args"]), resolve)
	if err != nil {
		return nil, err
	}
	kwargs, err := wireDecode(mapOrEmpty(params["kwargs"]), resolve)
	if err != nil {
		return nil, err
	}
	var result any
	if run, ok := hostSDKOps[op]; ok {
		result, err = run(ctx, h, args.([]any), kwargs.(map[string]any))
	} else if hostSDKServiceOps[op] {
		s := installedServices()
		if s == nil || s.PluginSDK == nil {
			return nil, &PluginHostUnsupported{Msg: fmt.Sprintf("plugin SDK call %s is not served by this Icebath process", config.PyRepr(op))}
		}
		result, err = s.PluginSDK(ctx, h.home, op, args.([]any), kwargs.(map[string]any))
	} else {
		return nil, &HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("unknown plugin SDK call %s", config.PyRepr(op))}
	}
	if err != nil {
		return nil, err
	}
	return h.encodeForHost(result), nil
}

// Masks secrets in a plugin string with the redactor options agent.redact accepts
func sdkRedactSensitiveText(_ context.Context, h *PluginHost, args []any, kwargs map[string]any) (any, error) {
	a, _, err := bindPy("redact_sensitive_text", []pyParam{req("text"), kwo("force", false), kwo("code_file", false),
		kwo("file_read", false), kwo("secret_file", false), kwo("redact_url_credentials", false)}, args, kwargs)
	if err != nil {
		return nil, err
	}
	text, ok := a["text"].(string)
	if !ok {
		return nil, &HostBuiltinError{Type: "TypeError", Message: fmt.Sprintf("expected string or bytes-like object, got '%s'", pyTypeName(a["text"]))}
	}
	return tools.RedactSensitiveTextWith(&tools.Env{Home: h.home}, text, tools.RedactOptions{Force: config.PyTruthy(a["force"]),
		CodeFile: config.PyTruthy(a["code_file"]), FileRead: config.PyTruthy(a["file_read"]), SecretFile: config.PyTruthy(a["secret_file"]),
		URLCredentials: config.PyTruthy(a["redact_url_credentials"])}), nil
}

// Value of a key from the host profile env file or process env, None when unset
func sdkGetEnvValue(ctx context.Context, h *PluginHost, args []any, kwargs map[string]any) (any, error) {
	a, _, err := bindPy("get_env_value", []pyParam{req("key")}, args, kwargs)
	if err != nil {
		return nil, err
	}
	value, ok, err := config.GetEnvValue(ctx, h.home, argStr(a["key"]))
	if err != nil || !ok {
		return nil, err
	}
	return value, nil
}

// Environment of a helper child the plugin starts, built from the child env it sent
func sdkBuildSubprocessEnv(ctx context.Context, h *PluginHost, args []any, kwargs map[string]any) (any, error) {
	a, _, err := bindPy("build_subprocess_env", []pyParam{opt("base", nil), kwo("inherit_profile_home", true),
		kwo("scrub_secrets", true), kwo("extra", nil), kwo("strip_launch_profile", false)}, args, kwargs)
	if err != nil {
		return nil, err
	}
	strs := func(v any) map[string]string {
		if v == nil {
			return nil
		}
		out := map[string]string{}
		for k, item := range argMap(v) {
			out[k] = config.PyStr(item)
		}
		return out
	}
	env, err := tools.BuildSubprocessEnv(ctx, &tools.Env{Home: h.home}, tools.SubprocessEnvOptions{Base: strs(a["base"]),
		InheritProfileHome: config.PyTruthy(a["inherit_profile_home"]), ScrubSecrets: config.PyTruthy(a["scrub_secrets"]),
		Extra: strs(a["extra"]), StripLaunchProfile: config.PyTruthy(a["strip_launch_profile"])})
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out, nil
}
