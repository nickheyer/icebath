package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
)

func init() {
	profiles.GatewayChannelHooks.PlatformMembers = func() []string {
		out := []string{}
		for _, p := range PlatformMemberMap() {
			out = append(out, string(p))
		}
		return out
	}
	profiles.GatewayChannelHooks.EnvEnableCredentials = func() [][2]any {
		out := [][2]any{}
		for _, row := range EnvEnableCredentials {
			out = append(out, [2]any{string(row.Platform), append([]string{}, row.Names...)})
		}
		return out
	}
	profiles.GatewayChannelHooks.PlatformEntries = channelPlatformEntries
}

// Every registry entry of the ctx scope as attribute mappings, a raising lookup returned as the error
func channelPlatformEntries(ctx context.Context) (out []map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("%v", r)
		}
	}()
	plugins.DiscoverPlugins(ctx, false)
	for _, e := range Registry.AllEntries(ctx) {
		steps := []map[string]any{}
		for _, s := range e.EnvOverrides {
			steps = append(steps, envStepAttributes(s))
		}
		out = append(out, map[string]any{"name": e.Name, "required_env": e.RequiredEnv, "allowed_users_env": e.AllowedUsersEnv,
			"allow_all_env": e.AllowAllEnv, "cron_deliver_env_var": e.CronDeliverEnvVar,
			"env_enable_credentials": e.EnvEnableCredentials, "env_overrides": steps, "shared_env_prefixes": e.SharedEnvPrefixes})
	}
	return out, nil
}

// Env names a step owns, an EnvCred row or a partial with its keywords, empty for a plain function
func envStepAttributes(step EnvStep) map[string]any {
	specEnvs := func(specs []EnvSpec) []any {
		out := []any{}
		for _, s := range specs {
			if s.AnyOf != nil {
				names := []any{}
				for _, n := range s.AnyOf {
					names = append(names, n)
				}
				out = append(out, names)
			} else {
				out = append(out, s.Env)
			}
		}
		return out
	}
	switch s := step.(type) {
	case *EnvCred:
		creds := []any{}
		for _, group := range s.Creds {
			if len(group) == 1 {
				creds = append(creds, group[0])
				continue
			}
			names := []any{}
			for _, n := range group {
				names = append(names, n)
			}
			creds = append(creds, names)
		}
		var warn []string
		if s.WarnMissingEnv != "" {
			warn = []string{s.WarnMissingEnv, s.WarnMissingMsg}
		}
		return map[string]any{"kind": "cred", "platform": string(s.Platform), "creds": creds, "token": s.Token,
			"fixed": specEnvs(s.Fixed), "optional": specEnvs(s.Optional), "optional_stripped": specEnvs(s.OptionalStripped),
			"warn_missing": warn, "home": s.Home}
	case HomeStep:
		return map[string]any{"kind": "partial", "env_base": s.EnvBase, "platform": string(s.Platform)}
	case ReplyModeStep:
		return map[string]any{"kind": "partial", "env": s.Env, "platform": string(s.Platform)}
	}
	return map[string]any{}
}
