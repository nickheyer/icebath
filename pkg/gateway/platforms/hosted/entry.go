package hosted

import (
	"context"
	"fmt"
	"maps"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Upstream names a hosted platform's Python values are built from and judged against
const (
	platformConfigBuilder = "gateway.config:PlatformConfig.from_dict"
	gatewayConfigBuilder  = "gateway.config:GatewayConfig.from_dict"
	platformKeyClass      = "gateway.config:Platform"
	adapterBase           = "gateway.platforms.base:BasePlatformAdapter"
	platformEnvModule     = "gateway.platform_env"
	notExplicitSentinel   = "gateway.platforms.send_targets.NOT_EXPLICIT"
	intSentinel           = "gateway.platform_env.INT"
)

// Converts the keywords of a hosted plugin's register_platform into the Go values PlatformEntry fields hold
func EntryKeywords(name string, kwargs map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(kwargs))
	for key, value := range kwargs {
		converted, err := entryValue(name, key, value)
		if err != nil {
			return nil, err
		}
		out[key] = converted
	}
	return out, nil
}

// Go value of one PlatformEntry keyword, hosted callables wrapped in the field's function type
func entryValue(name, key string, value any) (any, error) {
	if n, ok := value.(int64); ok && key == "max_message_length" {
		return int(n), nil
	}
	if r, ok := value.(plugins.Record); ok {
		value = map[string]any(r)
	}
	switch key {
	case "standalone_send":
		if o, ok := value.(*plugins.HostedObject); ok {
			return standaloneSendPolicy(o)
		}
		return value, nil
	case "env_value_rules":
		rules, ok := value.(map[string]any)
		if !ok {
			return value, nil
		}
		out := map[string]gateway.EnvValueRule{}
		for env, raw := range rules {
			pair, _ := raw.([]any)
			if len(pair) != 2 {
				return nil, &gateway.PyError{Kind: "ValueError", Msg: fmt.Sprintf("env_value_rules[%s] must be (accepts, detail)", config.PyRepr(env))}
			}
			out[env] = gateway.EnvValueRule{Check: predicate(pair[0]), Message: config.PyStr(pair[1])}
		}
		return out, nil
	case "env_overrides":
		items, ok := value.([]any)
		if !ok {
			return value, nil
		}
		steps := make([]gateway.EnvStep, len(items))
		for i, item := range items {
			step, err := envStep(item)
			if err != nil {
				return nil, err
			}
			steps[i] = step
		}
		return steps, nil
	}
	f, ok := value.(*plugins.HostedCallable)
	if !ok {
		return value, nil
	}
	switch key {
	case "adapter_factory":
		return gateway.AdapterFactory(func(ctx context.Context, cfg *gateway.PlatformConfig) (any, error) {
			return buildAdapter(ctx, name, f, cfg)
		}), nil
	case "validate_config":
		return func(cfg *gateway.PlatformConfig) (bool, error) { return truthyCall(f.HostContext(), f, cfg) }, nil
	case "is_connected":
		return func(ctx context.Context, cfg *gateway.PlatformConfig) (bool, error) { return truthyCall(ctx, f, cfg) }, nil
	case "config_connected_fn":
		return func(cfg *gateway.PlatformConfig) bool {
			ok, err := truthyCall(f.HostContext(), f, cfg)
			if err != nil {
				panic(err)
			}
			return ok
		}, nil
	case "ensure_deps_fn":
		return func(ctx context.Context) (bool, error) { return truthyCall(ctx, f) }, nil
	case "setup_fn":
		return func(ctx context.Context) error {
			_, _, err := f.CallWith(ctx, nil, nil, plugins.HostedCallOptions{Terminal: true})
			return err
		}, nil
	case "env_enablement_fn":
		return func(ctx context.Context) map[string]any {
			result, err := f.Call(ctx, nil, nil)
			if err != nil {
				panic(err)
			}
			return plainMap(result)
		}, nil
	case "apply_yaml_config_fn":
		return func(ctx context.Context, yamlCfg, block map[string]any) (map[string]any, error) {
			result, dumped, err := f.CallWith(ctx, []any{yamlCfg, block}, nil, plugins.HostedCallOptions{Dump: []string{"copy", "copy"}})
			if err != nil {
				return nil, err
			}
			replaceMap(yamlCfg, dumped[0])
			replaceMap(block, dumped[1])
			return plainMap(result), nil
		}, nil
	case "parse_target_ref_fn":
		return func(ref string) (string, *string, bool) {
			chat, thread, ok := targetPair(mustCall(f, ref))
			if !ok || chat == nil {
				return "", nil, false
			}
			return *chat, thread, true
		}, nil
	case "validate_target_ref_fn":
		return func(ref string) any { return mustCall(f, ref) }, nil
	case "target_ref_syntax_fn":
		return func(ref string) any {
			result := mustCall(f, ref)
			if m, ok := result.(map[string]any); ok && m["__sentinel__"] == notExplicitSentinel {
				return gateway.NotExplicit
			}
			if chat, thread, ok := targetPair(result); ok {
				return &gateway.TargetRefParse{ChatID: chat, ThreadID: thread}
			}
			return nil
		}, nil
	case "send_message_handler":
		return func(ctx context.Context, args map[string]any, chatID, platformName string, pconfig *gateway.PlatformConfig) (map[string]any, error) {
			result, _, err := f.CallWith(ctx, []any{args, chatID, platformName, pconfig.ToDict()}, nil,
				plugins.HostedCallOptions{Build: []string{"", "", "", platformConfigBuilder}})
			if err != nil {
				return nil, err
			}
			return plainMap(result), nil
		}, nil
	case "standalone_sender_fn":
		return func(ctx context.Context, kwargs map[string]any) (map[string]any, error) {
			rest := map[string]any{}
			for k, v := range kwargs {
				if k != "pconfig" && k != "chat_id" && k != "message" {
					rest[k] = senderValue(v)
				}
			}
			var pconfig any
			if pc, ok := kwargs["pconfig"].(*gateway.PlatformConfig); ok && pc != nil {
				pconfig = pc.ToDict()
			}
			result, _, err := f.CallWith(ctx, []any{pconfig, kwargs["chat_id"], kwargs["message"]}, rest,
				plugins.HostedCallOptions{Build: []string{platformConfigBuilder}})
			if err != nil {
				return nil, err
			}
			return plainMap(result), nil
		}, nil
	}
	return value, nil
}

// Wire form of a standalone sender keyword, media refs as the path and voice flag pairs Python passes
func senderValue(v any) any {
	media, ok := v.([]gateway.MediaRef)
	if !ok {
		return v
	}
	out := make([]any, len(media))
	for i, m := range media {
		out[i] = []any{m.Path, m.IsVoice}
	}
	return out
}

// Truthiness of a hosted call whose PlatformConfig arguments cross built as upstream PlatformConfig objects
func truthyCall(ctx context.Context, f *plugins.HostedCallable, args ...any) (bool, error) {
	wire := make([]any, len(args))
	build := make([]string, len(args))
	for i, a := range args {
		wire[i] = a
		if cfg, ok := a.(*gateway.PlatformConfig); ok {
			wire[i], build[i] = cfg.ToDict(), platformConfigBuilder
		}
	}
	result, _, err := f.CallWith(ctx, wire, nil, plugins.HostedCallOptions{Build: build})
	if err != nil {
		return false, err
	}
	return config.PyTruthy(result), nil
}

// Answer of a hosted call that raises as a panic the way Go callers of these hooks recover
func mustCall(f *plugins.HostedCallable, args ...any) any {
	result, err := f.Call(f.HostContext(), args, nil)
	if err != nil {
		panic(err)
	}
	return result
}

// Chat and thread of a (chat_id, thread_id) pair, false for None or any other value
func targetPair(v any) (*string, *string, bool) {
	pair, ok := v.([]any)
	if !ok || len(pair) != 2 {
		return nil, nil, false
	}
	str := func(x any) *string {
		if x == nil {
			return nil
		}
		s := config.PyStr(x)
		return &s
	}
	return str(pair[0]), str(pair[1]), true
}

// Predicate over an env value running a hosted accepts function, a raise counting as rejection
func predicate(v any) func(string) bool {
	f, ok := v.(*plugins.HostedCallable)
	if !ok {
		return func(string) bool {
			panic(&gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not callable", pyrepr.TypeName(v))})
		}
	}
	return func(value string) bool { return config.PyTruthy(mustCall(f, value)) }
}

// Plain mapping of a decoded answer, nil for None or a non mapping
func plainMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case plugins.Record:
		return map[string]any(m)
	}
	return nil
}

// Replaces a Go map's contents with the dict the child dumped after a call that may have mutated it
func replaceMap(dst map[string]any, dumped any) {
	src := plainMap(dumped)
	if dst == nil || src == nil {
		return
	}
	clear(dst)
	maps.Copy(dst, src)
}

// Go policy read from the fields of a hosted StandaloneSendPolicy
func standaloneSendPolicy(o *plugins.HostedObject) (*gateway.StandaloneSendPolicy, error) {
	ctx := context.Background()
	read := func(name string) (any, error) { return o.Get(ctx, name) }
	flag := func(name string) (bool, error) {
		v, err := read(name)
		return config.PyTruthy(v), err
	}
	label, err := read("label")
	if err != nil {
		return nil, err
	}
	policy := &gateway.StandaloneSendPolicy{Label: config.PyStr(label)}
	for name, dst := range map[string]*bool{"media": &policy.Media, "all_sends": &policy.AllSends, "captionable": &policy.Captionable,
		"empty_list_between_chunks": &policy.EmptyListBetweenChunks, "forward_force_document": &policy.ForwardForceDocument,
		"mentions": &policy.Mentions, "text_direct": &policy.TextDirect} {
		if *dst, err = flag(name); err != nil {
			return nil, err
		}
	}
	return policy, nil
}
