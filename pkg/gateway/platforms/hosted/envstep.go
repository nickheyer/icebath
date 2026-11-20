package hosted

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Env step of a hosted plugin, upstream EnvCred rows and step partials as their Go types and any other callable run in the child
func envStep(item any) (gateway.EnvStep, error) {
	f, ok := item.(*plugins.HostedCallable)
	if !ok {
		return gateway.EnvStepFunc(func(context.Context, *gateway.GatewayConfig) error {
			return &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not callable", pyrepr.TypeName(item))}
		}), nil
	}
	shape := f.Shape
	if shape != nil && shape["kind"] == "dataclass" && shape["module"] == platformEnvModule && shape["qualname"] == "EnvCred" {
		return envCred(plainMap(shape["fields"]))
	}
	if shape != nil && shape["kind"] == "partial" && shape["func"] == platformEnvModule && len(listOf(shape["args"])) == 0 {
		kw := plainMap(shape["keywords"])
		switch shape["qualname"] {
		case "env_home_channel":
			return gateway.HomeStep{Platform: gateway.Platform(config.PyStr(kw["platform"])), EnvBase: config.PyStr(kw["env_base"]),
				Strip: config.PyTruthy(kw["strip"])}, nil
		case "env_reply_mode":
			return gateway.ReplyModeStep{Platform: gateway.Platform(config.PyStr(kw["platform"])), Env: config.PyStr(kw["env"])}, nil
		}
	}
	return hostedStep{f: f}, nil
}

func listOf(v any) []any {
	items, _ := v.([]any)
	return items
}

// Step calling a plugin function with the gateway config built upstream, its changes written back
type hostedStep struct{ f *plugins.HostedCallable }

func (s hostedStep) Apply(ctx context.Context, cfg *gateway.GatewayConfig) error {
	_, dumped, err := s.f.CallWith(ctx, []any{cfg.ToDict()}, nil, plugins.HostedCallOptions{Build: []string{gatewayConfigBuilder}, Dump: []string{"to_dict"}})
	if err != nil {
		return err
	}
	return writeBack(ctx, cfg, dumped[0])
}

// Replaces the Go gateway config with the one the child dumped after the call
func writeBack(ctx context.Context, cfg *gateway.GatewayConfig, dumped any) error {
	fresh, err := gateway.GatewayConfigFromDict(ctx, plainMap(dumped))
	if err != nil {
		return err
	}
	*cfg = *fresh
	return nil
}

// Go EnvCred of an upstream EnvCred's fields, nested callables kept as hosted calls
func envCred(fields map[string]any) (gateway.EnvStep, error) {
	c := &gateway.EnvCred{Platform: gateway.Platform(config.PyStr(fields["platform"]))}
	for _, group := range listOf(fields["creds"]) {
		if names, ok := group.([]any); ok {
			c.Creds = append(c.Creds, stringList(names))
		} else {
			c.Creds = append(c.Creds, []string{config.PyStr(group)})
		}
	}
	if fields["token"] != nil {
		c.Token = config.PyStr(fields["token"])
	}
	var err error
	if c.Fixed, err = envSpecs(listOf(fields["fixed"]), true); err != nil {
		return nil, err
	}
	if c.Optional, err = envSpecs(listOf(fields["optional"]), false); err != nil {
		return nil, err
	}
	if c.OptionalStripped, err = envSpecs(listOf(fields["optional_stripped"]), false); err != nil {
		return nil, err
	}
	if warn := listOf(fields["warn_missing"]); len(warn) == 2 {
		c.WarnMissingEnv, c.WarnMissingMsg = config.PyStr(warn[0]), config.PyStr(warn[1])
	}
	if then, ok := fields["then"].(*plugins.HostedCallable); ok {
		platform := c.Platform
		c.Then = func(ctx context.Context, cfg *gateway.GatewayConfig, pc *gateway.PlatformConfig) error {
			_, dumped, err := then.CallWith(ctx, []any{cfg.ToDict(), nil}, nil, plugins.HostedCallOptions{Build: []string{gatewayConfigBuilder},
				Derive: []*plugins.HostedDerive{nil, {From: 0, Attr: "platforms", Key: string(platform), KeyBuild: platformKeyClass}}, Dump: []string{"to_dict"}})
			if err != nil {
				return err
			}
			if err := writeBack(ctx, cfg, dumped[0]); err != nil {
				return err
			}
			if fresh := cfg.Platforms[platform]; fresh != nil {
				*pc = *fresh
				cfg.Platforms[platform] = pc
			}
			return nil
		}
	}
	if fields["home"] != nil {
		c.Home = config.PyStr(fields["home"])
	}
	c.HomeStrip = config.PyTruthy(fields["home_strip"])
	return c, nil
}

func stringList(items []any) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = config.PyStr(item)
	}
	return out
}

// Specs of (key, env[, default[, fn]]) fixed rows or (key, env[, fn]) optional rows, an env tuple meaning any of
func envSpecs(rows []any, fixed bool) ([]gateway.EnvSpec, error) {
	out := make([]gateway.EnvSpec, 0, len(rows))
	for _, raw := range rows {
		row := listOf(raw)
		if len(row) < 2 {
			return nil, &gateway.PyError{Kind: "ValueError", Msg: fmt.Sprintf("not enough values to unpack (expected at least 2, got %d)", len(row))}
		}
		spec := gateway.EnvSpec{Key: config.PyStr(row[0])}
		if names, ok := row[1].([]any); ok {
			spec.AnyOf = stringList(names)
		} else {
			spec.Env = config.PyStr(row[1])
		}
		rest := row[2:]
		if fixed {
			if len(rest) > 0 {
				spec.Default = config.PyStr(rest[0])
			}
			if len(rest) > 1 {
				spec.Fn = specFn(rest[1])
			}
		} else if len(rest) > 0 {
			if m := plainMap(rest[0]); m != nil && m["__sentinel__"] == intSentinel {
				spec.Int = true
			} else {
				spec.Fn = specFn(rest[0])
			}
		}
		out = append(out, spec)
	}
	return out, nil
}

// Converter of an env value running the plugin's function in the child
func specFn(v any) func(string) any {
	f, ok := v.(*plugins.HostedCallable)
	if !ok {
		return func(string) any {
			panic(&gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not callable", pyrepr.TypeName(v))})
		}
	}
	return func(value string) any { return mustCall(f, value) }
}
