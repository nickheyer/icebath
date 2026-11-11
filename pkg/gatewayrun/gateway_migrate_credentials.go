package gatewayrun

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Command that converges every per profile gateway onto the host gateway
const MigrateCommand = gateway.HostMigrateCommand

// Profile name and its gateway config read the way the multiplexer reads it
type ProfileGatewayConfig struct {
	Name string
	Cfg  *gateway.GatewayConfig
}

// Config shaped stand in for an adapter carrying the credential attributes the fingerprint probes
type credentialProbe map[string]any

func (p credentialProbe) AdapterAttr(name string) any { return p[name] }

// Profile gateway config under the profile's own secret scope, plugin discovery suppressed for a parked profile
func profileGatewayConfigFor(ctx context.Context, h string) (*gateway.GatewayConfig, error) {
	scoped := gateway.ProfileRuntimeScope(ctx, h, true)
	if profiles.ProfileIsParked(h) {
		scoped = plugins.SuppressPluginDiscovery(scoped)
	}
	return gateway.LoadGatewayConfig(scoped)
}

// Runs fn with multiplexing marked active so secrets resolve the way the multiplexer resolves them
func withMultiplexReadMode(fn func()) {
	previous := provider.MultiplexProcess()
	provider.SetMultiplexActive(true)
	defer provider.SetMultiplexActive(previous)
	fn()
}

// Token or api key plus the id style credentials adapters expose from extra
func newCredentialProbe(pc *gateway.PlatformConfig) credentialProbe {
	token := pc.Token
	if !credentialTruthy(token) {
		token = pc.APIKey
	}
	extra := pc.Extra
	return credentialProbe{"token": token, "_app_id": extra["app_id"], "_client_id": extra["client_id"],
		"_bot_id": extra["bot_id"], "_project_secret": extra["project_secret"]}
}

func credentialTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	}
	return true
}

// Platform and fingerprint of every enabled platform with a discoverable credential, in platform name order
func credentialClaims(cfg *gateway.GatewayConfig) [][2]string {
	names := make([]string, 0, len(cfg.Platforms))
	for p := range cfg.Platforms {
		names = append(names, string(p))
	}
	sort.Strings(names)
	claims := [][2]string{}
	for _, name := range names {
		pc := cfg.Platforms[gateway.Platform(name)]
		if pc == nil || !pc.Enabled {
			continue
		}
		if fp := adapterCredentialFingerprint(newCredentialProbe(pc)); fp != "" {
			claims = append(claims, [2]string{name, fp})
		}
	}
	return claims
}

// Env key names that make the platform connect as a bot, the platform token wording when none is registered
func credentialKeyNames(ctx context.Context, platform string) string {
	owners := manage.CredentialEnvKeys(ctx)
	names := []string{}
	for _, key := range owners.Keys() {
		if pid, _ := owners.Get(key); pid == platform {
			names = append(names, key)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Sprintf("the %s token", platform)
	}
	return strings.Join(names, "/")
}

// One finding per platform credential two profiles both hold, naming profiles and key names only
func DuplicateCredentialLines(ctx context.Context, configs []ProfileGatewayConfig) []string {
	owners := map[[2]string]string{}
	lines := []string{}
	for _, entry := range configs {
		for _, claim := range credentialClaims(entry.Cfg) {
			owner, seen := owners[claim]
			if !seen {
				owners[claim] = entry.Name
				owner = entry.Name
			}
			if owner == entry.Name {
				continue
			}
			platform := claim[0]
			key := credentialKeyNames(ctx, platform)
			lines = append(lines, fmt.Sprintf("Profiles '%s' and '%s' both hold the same %s credential (%s): "+
				"one platform token can serve only one gateway, so the bot answers from whichever profile "+
				"claims it first and the other's adapter is parked. Give '%s' its own bot token, or remove "+
				"%s from the profile that should not own it (or keep it in %s and route %s's "+
				"chats with profile_routes — gateway.profile_routes in %s's config.yaml), then run "+
				"%s.", owner, entry.Name, platform, key, entry.Name, key, owner, entry.Name, owner, MigrateCommand))
		}
	}
	return lines
}

// Duplicate credential findings read straight from the local profile homes, unloadable configs skipped
func DuplicateCredentialFindings(ctx context.Context) ([]string, error) {
	configs := []ProfileGatewayConfig{}
	var err error
	withMultiplexReadMode(func() {
		var homes []profiles.ServedProfile
		if homes, err = migrateProfileHomes(ctx); err != nil {
			return
		}
		for _, p := range homes {
			if cfg, loadErr := profileGatewayConfigFor(ctx, p.Home); loadErr == nil && cfg != nil {
				configs = append(configs, ProfileGatewayConfig{p.Name, cfg})
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return DuplicateCredentialLines(ctx, configs), nil
}

// Blocker for every bot credential two profiles hold, since the multiplexer parks the duplicate adapter
func checkDuplicateCredentials(ctx context.Context, plan *MigrationPlan, configs map[string]*gateway.GatewayConfig) {
	entries := []ProfileGatewayConfig{}
	for _, p := range plan.Profiles {
		if cfg, ok := configs[p.Name]; ok {
			entries = append(entries, ProfileGatewayConfig{p.Name, cfg})
		}
	}
	plan.Blockers = append(plan.Blockers, DuplicateCredentialLines(ctx, entries)...)
}
