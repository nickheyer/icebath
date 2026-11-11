package dashboard

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Card metadata of a platform in override shape, the has flags standing for keys the override declares
type platformCard struct {
	name, description, docsURL string
	envVars                    []string
	hasEnvVars                 bool
	requiredEnv                []string
	hasRequiredEnv             bool
}

// Card metadata of the core only platforms that have no plugin entry to declare it
var platformOverrides = map[string]platformCard{
	"api_server": {name: "API server",
		description: "Expose Hermes as an OpenAI-compatible HTTP API for tools like Open WebUI.",
		docsURL:     "https://hermes-agent.nousresearch.com/docs/user-guide/messaging/",
		envVars: []string{"API_SERVER_ENABLED", "API_SERVER_KEY", "API_SERVER_PORT", "API_SERVER_HOST",
			"API_SERVER_MODEL_NAME"},
		hasEnvVars: true, requiredEnv: []string{}, hasRequiredEnv: true},
	"webhook": {name: "Webhooks",
		description: "Receive events from GitHub, GitLab, and other webhook sources.",
		docsURL:     "https://hermes-agent.nousresearch.com/docs/user-guide/messaging/webhooks/",
		envVars:     []string{"WEBHOOK_ENABLED", "WEBHOOK_PORT", "WEBHOOK_SECRET"}, hasEnvVars: true,
		requiredEnv: []string{}, hasRequiredEnv: true},
	"msgraph_webhook": {name: "Microsoft Graph Webhook",
		description: "Receive Microsoft Graph change notifications (Teams meetings, Outlook, …).",
		docsURL:     "https://hermes-agent.nousresearch.com/docs/user-guide/messaging/msgraph-webhook",
		requiredEnv: []string{}, hasRequiredEnv: true},
	"relay": {name: "Relay (experimental)",
		description: "Generic relay adapter fronted by the Hermes Relay connector.",
		docsURL:     "", requiredEnv: []string{}, hasRequiredEnv: true},
}

// Display order of the well known platforms, unknown plugins following alphabetically
var platformOrder = []string{
	"telegram", "discord", "slack", "mattermost", "matrix", "whatsapp", "signal", "bluebubbles",
	"email", "sms", "dingtalk", "feishu", "google_chat", "wecom", "wecom_callback",
	"weixin", "qqbot", "yuanbao", "api_server", "webhook",
}

// Messaging catalog of the built in platforms then the plugin platforms, in display order
func messagingPlatformCatalog(ctx context.Context) []*pyjson.Object {
	pluginMap := map[string]*gateway.PlatformEntry{}
	var pluginOrder []string
	func() {
		defer func() {
			if r := recover(); r != nil {
				routersLog().Debug("plugin platform registry unavailable", "panic", fmt.Sprint(r))
			}
		}()
		plugins.DiscoverPlugins(ctx, false)
		for _, entry := range gateway.Registry.PluginEntries(ctx) {
			if _, held := pluginMap[entry.Name]; !held {
				pluginOrder = append(pluginOrder, entry.Name)
			}
			pluginMap[entry.Name] = entry
		}
	}()
	seen := map[string]bool{}
	var ids []string
	for _, m := range gateway.PlatformMemberMap() {
		if m != gateway.PlatformLocal {
			ids = append(ids, string(m))
		}
	}
	ids = append(ids, pluginOrder...)
	entries := []*pyjson.Object{}
	for _, pid := range ids {
		if seen[pid] {
			continue
		}
		seen[pid] = true
		entries = append(entries, buildCatalogEntry(pid, pluginMap[pid]))
	}
	order := map[string]int{}
	for i, pid := range platformOrder {
		order[pid] = i
	}
	rank := func(e *pyjson.Object) int {
		if i, ok := order[e.GetOr("id", "").(string)]; ok {
			return i
		}
		return len(platformOrder)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		ri, rj := rank(entries[i]), rank(entries[j])
		if ri != rj {
			return ri < rj
		}
		return pystr.Lower(entries[i].GetOr("name", "").(string)) < pystr.Lower(entries[j].GetOr("name", "").(string))
	})
	return entries
}

// Env keys a Channels page platform card owns, empty when the catalog cannot be built
func channelManagedEnvKeys(ctx context.Context) (keys map[string]bool) {
	keys = map[string]bool{}
	defer func() {
		if r := recover(); r != nil {
			routersLog().Debug("could not build channel-managed env key set", "panic", fmt.Sprint(r))
			keys = map[string]bool{}
		}
	}()
	for _, entry := range messagingPlatformCatalog(ctx) {
		for _, k := range entry.GetOr("env_vars", []any{}).([]any) {
			keys[k.(string)] = true
		}
	}
	return keys
}

// Gateway and relay knobs that stay on the Keys page although their category is messaging
var messagingKeysPageKeys = map[string]bool{"GATEWAY_ALLOW_ALL_USERS": true, "GATEWAY_PROXY_KEY": true, "GATEWAY_PROXY_URL": true}

// Env prefixes a messaging platform card owns
func platformEnvPrefixes(platformID string) []string { return manage.PlatformEnvPrefixes(platformID) }

// Every visible messaging env var under one of the platform prefixes, sorted
func discoverPlatformEnvVars(platformID string) []string {
	prefixes := platformEnvPrefixes(platformID)
	found := map[string]bool{}
	for _, item := range config.OptionalEnvVars.Items() {
		name := item.Name
		if item.Entry.Category != "messaging" || messagingKeysPageKeys[name] || provider.IsSetupHiddenEnv(name) {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				found[name] = true
				break
			}
		}
	}
	out := make([]string, 0, len(found))
	for n := range found {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Env var list of a platform card, required credentials always kept so the platform stays configurable
func mergePlatformEnvVars(platformID string, override platformCard, pluginEntry *gateway.PlatformEntry) []string {
	discovered := discoverPlatformEnvVars(platformID)
	if override.hasEnvVars {
		var explicit []string
		for _, key := range override.envVars {
			if !provider.IsSetupHiddenEnv(key) {
				explicit = append(explicit, key)
			}
		}
		return dedupeStrings(append(explicit, discovered...))
	}
	if pluginEntry != nil && len(pluginEntry.RequiredEnv) > 0 {
		return dedupeStrings(append(append([]string{}, pluginEntry.RequiredEnv...), discovered...))
	}
	return discovered
}

// Card metadata a plugin declares on its platform entry, undeclared fields left out
func pluginCardFields(entry *gateway.PlatformEntry) platformCard {
	card := platformCard{name: entry.DashboardLabel, description: entry.DashboardDescription, docsURL: entry.DocsURL}
	if len(entry.DashboardEnvVars) > 0 {
		card.envVars, card.hasEnvVars = entry.DashboardEnvVars, true
	}
	if entry.DashboardRequiredEnv != nil {
		card.requiredEnv, card.hasRequiredEnv = entry.DashboardRequiredEnv, true
	}
	return card
}

// Catalog card of one platform from its override or plugin metadata, the rest derived from its id
func buildCatalogEntry(platformID string, pluginEntry *gateway.PlatformEntry) *pyjson.Object {
	override, ok := platformOverrides[platformID]
	if !ok && pluginEntry != nil {
		override = pluginCardFields(pluginEntry)
	}
	requiredEnv := []string{}
	if override.hasRequiredEnv {
		requiredEnv = override.requiredEnv
	} else if pluginEntry != nil {
		requiredEnv = pluginEntry.RequiredEnv
	}
	name := override.name
	if name == "" && pluginEntry != nil {
		name = pluginEntry.Label
	}
	if name == "" {
		name = pystr.Title(strings.ReplaceAll(platformID, "_", " "))
	}
	description := override.description
	if description == "" && pluginEntry != nil {
		description = pluginEntry.InstallHint
	}
	return pyjson.ObjectOf(
		"id", platformID,
		"name", name,
		"description", description,
		"docs_url", override.docsURL,
		"env_vars", stringsAny(mergePlatformEnvVars(platformID, override, pluginEntry)),
		"required_env", stringsAny(requiredEnv),
	)
}

// Persists a platform's enabled flag in the scoped home config
func writePlatformEnabled(ctx context.Context, platformID string, enabled bool) error {
	return config.WritePlatformConfigField(ctx, home.Dir(ctx), platformID, "enabled", enabled, false)
}
