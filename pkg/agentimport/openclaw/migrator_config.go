package openclaw

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Path of the target config.yaml
func (m *Migrator) configPath() string { return pathutil.Join(m.targetRoot, "config.yaml") }

// Writes a JSON archive file under the archive directory, creating it first
func (m *Migrator) archiveJSON(name string, value any) string {
	mkdirAll(m.archiveDir)
	dest := pathutil.Join(m.archiveDir, name)
	writeJSONFile(dest, value)
	return dest
}

// Copies a source directory into the archive, merging into an existing copy
func copyTreeInto(src, dst string) {
	if err := fsutil.CopyTree(src, dst, fsutil.CopyTreeOptions{DirsExistOK: true}); err != nil {
		raise(err)
	}
}

// Imports MCP server definitions into mcp_servers
func (m *Migrator) migrateMCPServers(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	mcpRaw := or(dget(or(dget(cfg, "mcp", nil), jsonutil.NewObject()), "servers", nil), jsonutil.NewObject())
	if !anyutil.IsTrue(mcpRaw) {
		m.record("mcp-servers", "", "", StatusSkipped, "No MCP servers found in OpenClaw config")
		return
	}
	path := m.configPath()
	icebathCfg := loadYAML(path)
	existingMCP := or(dget(icebathCfg, "mcp_servers", nil), newYAMLMap())
	added := 0
	for _, e := range ditems(mcpRaw) {
		name, srv := e.Key, e.Val
		if !isDict(srv) {
			continue
		}
		if contains(existingMCP, name) && !m.overwrite {
			m.record("mcp-servers", "mcp.servers."+name, "mcp_servers."+name, StatusConflict, "MCP server already exists in Icebath config")
			continue
		}
		icebathSrv := newYAMLMap()
		if anyutil.IsTrue(dget(srv, "command", nil)) {
			dset(icebathSrv, "command", toYAML(dindex(srv, "command")))
			for _, k := range []string{"args", "env", "cwd"} {
				if anyutil.IsTrue(dget(srv, k, nil)) {
					dset(icebathSrv, k, toYAML(dindex(srv, k)))
				}
			}
		}
		if anyutil.IsTrue(dget(srv, "url", nil)) {
			dset(icebathSrv, "url", toYAML(dindex(srv, "url")))
			for _, k := range []string{"headers", "auth"} {
				if anyutil.IsTrue(dget(srv, k, nil)) {
					dset(icebathSrv, k, toYAML(dindex(srv, k)))
				}
			}
		}
		if isFalse(dget(srv, "enabled", nil)) {
			dset(icebathSrv, "enabled", false)
		}
		if anyutil.IsTrue(dget(srv, "timeout", nil)) {
			dset(icebathSrv, "timeout", toYAML(dindex(srv, "timeout")))
		}
		if anyutil.IsTrue(dget(srv, "connectTimeout", nil)) {
			dset(icebathSrv, "connect_timeout", toYAML(dindex(srv, "connectTimeout")))
		}
		toolsCfg := or(dget(srv, "tools", nil), jsonutil.NewObject())
		if anyutil.IsTrue(dget(toolsCfg, "include", nil)) || anyutil.IsTrue(dget(toolsCfg, "exclude", nil)) {
			tools := newYAMLMap()
			dset(icebathSrv, "tools", tools)
			for _, k := range []string{"include", "exclude"} {
				if anyutil.IsTrue(dget(toolsCfg, k, nil)) {
					dset(tools, k, toYAML(dindex(toolsCfg, k)))
				}
			}
		}
		if sampling := dget(srv, "sampling", nil); anyutil.IsTrue(sampling) && isDict(sampling) {
			out := newYAMLMap()
			fields := []kv{
				{"enabled", dget(sampling, "enabled", nil)},
				{"model", dget(sampling, "model", nil)},
				{"max_tokens_cap", or(dget(sampling, "maxTokensCap", nil), dget(sampling, "max_tokens_cap", nil))},
				{"timeout", dget(sampling, "timeout", nil)},
				{"max_rpm", or(dget(sampling, "maxRpm", nil), dget(sampling, "max_rpm", nil))},
			}
			for _, f := range fields {
				if f.Val != nil {
					dset(out, f.Key, toYAML(f.Val))
				}
			}
			dset(icebathSrv, "sampling", out)
		}
		dset(existingMCP, name, icebathSrv)
		added++
		m.record("mcp-servers", "mcp.servers."+name, "config.yaml mcp_servers."+name, StatusMigrated, "", "servers_added", int64(added))
	}
	if added > 0 && m.execute {
		m.maybeBackup(path)
		dset(icebathCfg, "mcp_servers", existingMCP)
		dumpYAML(path, icebathCfg)
	}
}

// Archives the plugins config and extensions directory and imports plugin API keys when secrets are enabled
func (m *Migrator) migratePluginsConfig(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	plugins := or(dget(cfg, "plugins", nil), jsonutil.NewObject())
	if !anyutil.IsTrue(plugins) {
		m.record("plugins-config", "", "", StatusSkipped, "No plugins configuration found")
		return
	}
	if m.archiveDir != "" && m.execute {
		dest := m.archiveJSON("plugins-config.json", plugins)
		m.record("plugins-config", "openclaw.json plugins.*", dest, StatusArchived, "Plugins config archived for manual review")
	} else {
		status := StatusArchived
		if m.execute {
			status = StatusMigrated
		}
		m.record("plugins-config", "openclaw.json plugins.*", "archive/plugins-config.json", status, "Would archive plugins config")
	}
	if extDir := pathutil.Join(m.sourceRoot, "extensions"); isDir(extDir) && m.archiveDir != "" {
		destExt := pathutil.Join(m.archiveDir, "extensions")
		if m.execute {
			copyTreeInto(extDir, destExt)
		}
		m.record("plugins-config", extDir, destExt, StatusArchived, "Extensions directory archived")
	}
	for _, e := range ditems(or(dget(plugins, "entries", nil), jsonutil.NewObject())) {
		if !isDict(e.Val) {
			continue
		}
		if apiKey := dget(e.Val, "apiKey", nil); anyutil.IsTrue(apiKey) && m.migrateSecrets {
			envKey := "PLUGIN_" + strings.ReplaceAll(upper(e.Key), "-", "_") + "_API_KEY"
			m.setEnvVar(envKey, apiKey, "plugins.entries."+e.Key+".apiKey")
		}
	}
}

// Archives the cron config and the cron job store
func (m *Migrator) migrateCronJobs(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	cron := or(dget(cfg, "cron", nil), jsonutil.NewObject())
	cronStore := pathutil.Join(m.sourceRoot, "cron")
	found := false
	if anyutil.IsTrue(cron) {
		found = true
		if m.archiveDir != "" && m.execute {
			dest := m.archiveJSON("cron-config.json", cron)
			m.record("cron-jobs", "openclaw.json cron.*", dest, StatusArchived, "Cron config archived. Use 'icebath cron' to recreate jobs manually.")
		} else {
			m.record("cron-jobs", "openclaw.json cron.*", "archive/cron-config.json", StatusArchived, "Would archive cron config")
		}
	}
	if isDir(cronStore) && m.archiveDir != "" {
		found = true
		destCron := pathutil.Join(m.archiveDir, "cron-store")
		if m.execute {
			copyTreeInto(cronStore, destCron)
		}
		m.record("cron-jobs", cronStore, destCron, StatusArchived, "Cron job store archived")
	}
	if !found {
		m.record("cron-jobs", "", "", StatusSkipped, "No cron configuration found")
	}
}

// Archives the hooks config and the workspace hooks directory
func (m *Migrator) migrateHooksConfig(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	hooks := or(dget(cfg, "hooks", nil), jsonutil.NewObject())
	if !anyutil.IsTrue(hooks) {
		m.record("hooks-config", "", "", StatusSkipped, "No hooks configuration found")
		return
	}
	if m.archiveDir != "" && m.execute {
		dest := m.archiveJSON("hooks-config.json", hooks)
		m.record("hooks-config", "openclaw.json hooks.*", dest, StatusArchived, "Hooks config archived for manual review")
	} else {
		m.record("hooks-config", "openclaw.json hooks.*", "archive/hooks-config.json", StatusArchived, "Would archive hooks config")
	}
	for _, ws := range []string{"workspace", "workspace.default"} {
		hooksDir := pathutil.Join(m.sourceRoot, ws, "hooks")
		if isDir(hooksDir) && m.archiveDir != "" {
			destHooks := pathutil.Join(m.archiveDir, "workspace-hooks")
			if m.execute {
				copyTreeInto(hooksDir, destHooks)
			}
			m.record("hooks-config", hooksDir, destHooks, StatusArchived, "Workspace hooks directory archived")
			break
		}
	}
}

// Python thinking in a set of literals, TypeError for an unhashable value
func inSet(v any, members ...string) bool {
	requireHashable(v)
	s, ok := v.(string)
	return ok && containsString(members, s)
}

// Maps agent defaults onto agent, compression, human_delay, timezone and terminal and archives the agent list and bindings
func (m *Migrator) migrateAgentConfig(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	agents := or(dget(cfg, "agents", nil), jsonutil.NewObject())
	defaults := or(dget(agents, "defaults", nil), jsonutil.NewObject())
	agentList := or(dget(agents, "list", nil), []any{})
	if !anyutil.IsTrue(defaults) && !anyutil.IsTrue(agentList) {
		m.record("agent-config", "", "", StatusSkipped, "No agent configuration found")
		return
	}
	path := m.configPath()
	icebathCfg := loadYAML(path)
	changes := false
	agentCfg := or(dget(icebathCfg, "agent", nil), newYAMLMap())
	dget(defaults, "contextTokens", nil)
	if anyutil.IsTrue(dget(defaults, "timeoutSeconds", nil)) {
		dset(agentCfg, "max_turns", floorDiv10Min200(dindex(defaults, "timeoutSeconds")))
		changes = true
	}
	if anyutil.IsTrue(dget(defaults, "verboseDefault", nil)) {
		dset(agentCfg, "verbose", toYAML(dindex(defaults, "verboseDefault")))
		changes = true
	}
	if anyutil.IsTrue(dget(defaults, "thinkingDefault", nil)) {
		thinking := dindex(defaults, "thinkingDefault")
		switch {
		case inSet(thinking, "always", "high", "xhigh"):
			dset(agentCfg, "reasoning_effort", "high")
		case inSet(thinking, "auto", "medium", "adaptive"):
			dset(agentCfg, "reasoning_effort", "medium")
		case inSet(thinking, "off", "low", "none", "minimal"):
			dset(agentCfg, "reasoning_effort", "low")
		}
		changes = true
	}
	if compaction := or(dget(defaults, "compaction", nil), jsonutil.NewObject()); anyutil.IsTrue(compaction) {
		compression := or(dget(icebathCfg, "compression", nil), newYAMLMap())
		dset(compression, "enabled", !eqStr(dget(compaction, "mode", nil), "off"))
		dget(compaction, "timeout", nil)
		if anyutil.IsTrue(dget(compaction, "model", nil)) {
			aux := dsetdefaultDict(icebathCfg, "auxiliary")
			auxComp := dsetdefaultDict(aux, "compression")
			dset(auxComp, "model", toYAML(dindex(compaction, "model")))
		}
		dset(icebathCfg, "compression", compression)
		changes = true
	}
	if humanDelay := or(dget(defaults, "humanDelay", nil), jsonutil.NewObject()); anyutil.IsTrue(humanDelay) {
		hd := or(dget(icebathCfg, "human_delay", nil), newYAMLMap())
		var natural any
		if anyutil.IsTrue(dget(humanDelay, "enabled", nil)) {
			natural = "natural"
		}
		hdMode := or(dget(humanDelay, "mode", nil), natural)
		if anyutil.IsTrue(hdMode) && !eqStr(hdMode, "off") {
			dset(hd, "mode", toYAML(hdMode))
		}
		if anyutil.IsTrue(dget(humanDelay, "minMs", nil)) {
			dset(hd, "min_ms", toYAML(dindex(humanDelay, "minMs")))
		}
		if anyutil.IsTrue(dget(humanDelay, "maxMs", nil)) {
			dset(hd, "max_ms", toYAML(dindex(humanDelay, "maxMs")))
		}
		dset(icebathCfg, "human_delay", hd)
		changes = true
	}
	if anyutil.IsTrue(dget(defaults, "userTimezone", nil)) {
		dset(icebathCfg, "timezone", toYAML(dindex(defaults, "userTimezone")))
		changes = true
	}
	if execCfg := or(dget(or(dget(cfg, "tools", nil), jsonutil.NewObject()), "exec", nil), jsonutil.NewObject()); anyutil.IsTrue(execCfg) {
		terminalCfg := or(dget(icebathCfg, "terminal", nil), newYAMLMap())
		if timeout := orElse(dget(execCfg, "timeoutSec", nil), lazyGet(execCfg, "timeout")); anyutil.IsTrue(timeout) {
			dset(terminalCfg, "timeout", toYAML(orElse(dget(execCfg, "timeoutSec", nil), lazyGet(execCfg, "timeout"))))
			changes = true
		}
		dset(icebathCfg, "terminal", terminalCfg)
	}
	if sandbox := or(dget(defaults, "sandbox", nil), jsonutil.NewObject()); anyutil.IsTrue(sandbox) && eqStr(dget(sandbox, "backend", nil), "docker") {
		terminalCfg := or(dget(icebathCfg, "terminal", nil), newYAMLMap())
		dset(terminalCfg, "backend", "docker")
		if anyutil.IsTrue(dget(dget(sandbox, "docker", jsonutil.NewObject()), "image", nil)) {
			dset(terminalCfg, "docker_image", toYAML(dindex(dindex(sandbox, "docker"), "image")))
		}
		dset(icebathCfg, "terminal", terminalCfg)
		changes = true
	}
	if changes {
		dset(icebathCfg, "agent", agentCfg)
		if m.execute {
			m.maybeBackup(path)
			dumpYAML(path, icebathCfg)
		}
		m.record("agent-config", "openclaw.json agents.defaults", "config.yaml agent/compression/terminal", StatusMigrated, "Agent defaults mapped to Icebath config")
	}
	if anyutil.IsTrue(agentList) {
		if m.archiveDir != "" && m.execute {
			m.archiveJSON("agents-list.json", agentList)
		}
		m.record("agent-config", "openclaw.json agents.list", "archive/agents-list.json", StatusArchived,
			fmt.Sprintf("Multi-agent setup (%d agents) archived for manual recreation", pyLen(agentList)))
	}
	if bindings := or(dget(cfg, "bindings", nil), []any{}); anyutil.IsTrue(bindings) {
		if m.archiveDir != "" && m.execute {
			m.archiveJSON("bindings.json", bindings)
		}
		m.record("agent-config", "openclaw.json bindings", "archive/bindings.json", StatusArchived,
			fmt.Sprintf("Agent routing bindings (%d rules) archived", pyLen(bindings)))
	}
}

// Archives the gateway config and imports the gateway auth token when secrets are enabled
func (m *Migrator) migrateGatewayConfig(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	gateway := or(dget(cfg, "gateway", nil), jsonutil.NewObject())
	if !anyutil.IsTrue(gateway) {
		m.record("gateway-config", "", "", StatusSkipped, "No gateway configuration found")
		return
	}
	if m.archiveDir != "" && m.execute {
		m.archiveJSON("gateway-config.json", gateway)
	}
	m.record("gateway-config", "openclaw.json gateway.*", "archive/gateway-config.json", StatusArchived, "Gateway config archived. Use 'icebath gateway' to configure.")
	auth := or(dget(gateway, "auth", nil), jsonutil.NewObject())
	if anyutil.IsTrue(dget(auth, "token", nil)) && m.migrateSecrets {
		m.setEnvVar("ICEBATH_GATEWAY_TOKEN", dindex(auth, "token"), "gateway.auth.token")
	}
}

// Records that reset timers are not imported and archives the advanced session settings
func (m *Migrator) migrateSessionConfig(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	session := or(dget(cfg, "session", nil), jsonutil.NewObject())
	if !anyutil.IsTrue(session) {
		m.record("session-config", "", "", StatusSkipped, "No session configuration found")
		return
	}
	if anyutil.IsTrue(dget(session, "reset", nil)) || anyutil.IsTrue(dget(session, "resetTriggers", nil)) || anyutil.IsTrue(dget(session, "reset_triggers", nil)) {
		m.record("session-config", "session reset timers", "", StatusSkipped, "Icebath conversations do not reset on idle or daily timers")
	}
	complexSession := jsonutil.NewObject()
	for _, e := range ditems(session) {
		if containsString([]string{"identityLinks", "threadBindings", "maintenance", "scope", "sendPolicy"}, e.Key) && anyutil.IsTrue(e.Val) {
			complexSession.Set(e.Key, e.Val)
		}
	}
	if complexSession.Len() > 0 && m.archiveDir != "" {
		if m.execute {
			m.archiveJSON("session-config.json", complexSession)
		}
		m.record("session-config", "openclaw.json session (advanced)", "archive/session-config.json", StatusArchived,
			"Advanced session settings archived (identity links, thread bindings, etc.)")
	}
}

// API modes of the OpenClaw provider api types
var apiModeMap = map[string]string{
	"openai": "chat_completions", "openai-completions": "chat_completions", "openai-responses": "chat_completions",
	"anthropic": "anthropic_messages", "anthropic-messages": "anthropic_messages", "google-generative-ai": "chat_completions",
	"cohere": "chat_completions",
}

// Providers whose keys are imported but which need no providers entry
var wellKnownProviders = []string{"openrouter", "openai", "anthropic", "deepseek", "google", "groq"}

// Adds custom providers as providers entries, imports provider keys when secrets are enabled and archives the model catalog
func (m *Migrator) migrateFullProviders(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	models := or(dget(cfg, "models", nil), jsonutil.NewObject())
	providers := or(dget(models, "providers", nil), jsonutil.NewObject())
	if !anyutil.IsTrue(providers) {
		m.record("full-providers", "", "", StatusSkipped, "No model providers found")
		return
	}
	path := m.configPath()
	icebathCfg := loadYAML(path)
	userProviders := or(dget(icebathCfg, "providers", nil), newYAMLMap())
	added := 0
	for _, e := range ditems(providers) {
		provName, provCfg := e.Key, e.Val
		if !isDict(provCfg) {
			continue
		}
		if apiKey := or(dget(provCfg, "apiKey", nil), dget(provCfg, "api_key", nil)); anyutil.IsTrue(apiKey) && m.migrateSecrets {
			envKey := strings.ReplaceAll(upper(provName), "-", "_") + "_API_KEY"
			m.setEnvVar(envKey, apiKey, "models.providers."+provName+".apiKey")
		}
		if containsString(wellKnownProviders, lower(provName)) || !anyutil.IsTrue(dget(provCfg, "baseUrl", nil)) {
			continue
		}
		entryKey := strings.ReplaceAll(lower(provName), " ", "-")
		if anyutil.IsTrue(dget(userProviders, entryKey, nil)) && !m.overwrite {
			m.record("full-providers", "models.providers."+provName, "config.yaml providers", StatusConflict, fmt.Sprintf("Provider '%s' already exists", provName))
			continue
		}
		apiType := or(or(or(dget(provCfg, "apiType", nil), dget(provCfg, "api", nil)), dget(provCfg, "type", nil)), "openai")
		requireHashable(apiType)
		apiMode := "chat_completions"
		if s, ok := apiType.(string); ok {
			if mode, known := apiModeMap[s]; known {
				apiMode = mode
			}
		}
		entry := newYAMLMap()
		dset(entry, "name", provName)
		dset(entry, "base_url", toYAML(dindex(provCfg, "baseUrl")))
		dset(entry, "api_key", "")
		dset(entry, "api_mode", apiMode)
		if !isDict(userProviders) {
			raisef("AttributeError", "providers must be a mapping, got %s", errchain.Kind(userProviders))
		}
		dset(userProviders, entryKey, entry)
		added++
		m.record("full-providers", "models.providers."+provName, "config.yaml providers."+entryKey, StatusMigrated, "")
	}
	if added > 0 && m.execute {
		m.maybeBackup(path)
		dset(icebathCfg, "providers", userProviders)
		dumpYAML(path, icebathCfg)
	}
	agentDefaults := or(dget(or(dget(cfg, "agents", nil), jsonutil.NewObject()), "defaults", nil), jsonutil.NewObject())
	if aliases := or(dget(agentDefaults, "models", nil), jsonutil.NewObject()); anyutil.IsTrue(aliases) {
		if m.archiveDir != "" && m.execute {
			m.archiveJSON("model-aliases.json", aliases)
		}
		m.record("full-providers", "agents.defaults.models", "archive/model-aliases.json", StatusArchived,
			fmt.Sprintf("Model aliases/catalog (%d entries) archived", pyLen(aliases)))
	}
}

// Env mapping of one extended channel
type channelEnv struct {
	name       string
	token      string
	tokenField string
	allowFrom  string
	extras     []kv
}

// Extended channel token, allowlist and extra field env names in import order
var channelEnvMap = []channelEnv{
	{name: "matrix", token: "MATRIX_ACCESS_TOKEN", tokenField: "accessToken", allowFrom: "MATRIX_ALLOWED_USERS",
		extras: []kv{{"homeserverUrl", "MATRIX_HOMESERVER_URL"}, {"userId", "MATRIX_USER_ID"}}},
	{name: "mattermost", token: "MATTERMOST_BOT_TOKEN", allowFrom: "MATTERMOST_ALLOWED_USERS",
		extras: []kv{{"url", "MATTERMOST_URL"}, {"teamId", "MATTERMOST_TEAM_ID"}}},
	{name: "irc", extras: []kv{{"server", "IRC_SERVER"}, {"nick", "IRC_NICK"}, {"channels", "IRC_CHANNELS"}}},
	{name: "googlechat", extras: []kv{{"serviceAccountKeyPath", "GOOGLE_CHAT_SA_KEY_PATH"}}},
	{name: "imessage"},
	{name: "bluebubbles", extras: []kv{{"server", "BLUEBUBBLES_SERVER"}, {"password", "BLUEBUBBLES_PASSWORD"}}},
	{name: "msteams", token: "MSTEAMS_BOT_TOKEN", allowFrom: "MSTEAMS_ALLOWED_USERS"},
	{name: "nostr", extras: []kv{{"nsec", "NOSTR_NSEC"}, {"relays", "NOSTR_RELAYS"}}},
	{name: "twitch", token: "TWITCH_BOT_TOKEN", extras: []kv{{"channels", "TWITCH_CHANNELS"}}},
}

// Str of a value, a list joined with commas of the str of each element
func joinedStr(v any) string {
	if list, ok := v.([]any); ok {
		parts := make([]string, len(list))
		for i, x := range list {
			parts[i] = pyStr(x)
		}
		return strings.Join(parts, ",")
	}
	return pyStr(v)
}

// Imports extended channel settings into .env, Discord options into config.yaml, and archives the complex channel settings
func (m *Migrator) migrateDeepChannels(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	channels := or(dget(cfg, "channels", nil), jsonutil.NewObject())
	if !anyutil.IsTrue(channels) {
		m.record("deep-channels", "", "", StatusSkipped, "No channel configuration found")
		return
	}
	for _, ch := range channelEnvMap {
		chCfg := or(dget(channels, ch.name, nil), jsonutil.NewObject())
		if !anyutil.IsTrue(chCfg) {
			continue
		}
		tokenField := ch.tokenField
		if tokenField == "" {
			tokenField = "botToken"
		}
		botToken := channelField(chCfg, tokenField)
		if ch.token != "" && anyutil.IsTrue(botToken) && m.migrateSecrets {
			m.setEnvVar(ch.token, pyStr(botToken), "channels."+ch.name+"."+tokenField)
		}
		if allowVal := channelField(chCfg, "allowFrom"); ch.allowFrom != "" && anyutil.IsTrue(allowVal) {
			m.setEnvVar(ch.allowFrom, joinedStr(allowVal), "channels."+ch.name+".allowFrom")
		}
		for _, extra := range ch.extras {
			val := channelField(chCfg, extra.Key)
			if !anyutil.IsTrue(val) {
				continue
			}
			low := lower(extra.Key)
			isSecret := strings.Contains(low, "password") || strings.Contains(low, "token") || strings.Contains(low, "nsec")
			if isSecret && !m.migrateSecrets {
				continue
			}
			m.setEnvVar(extra.Val.(string), joinedStr(val), "channels."+ch.name+"."+extra.Key)
		}
	}
	if discord := or(dget(channels, "discord", nil), jsonutil.NewObject()); anyutil.IsTrue(discord) {
		path := m.configPath()
		icebathCfg := loadYAML(path)
		discordIcebath := or(dget(icebathCfg, "discord", nil), newYAMLMap())
		changed := false
		if contains(discord, "requireMention") {
			dset(discordIcebath, "require_mention", toYAML(dindex(discord, "requireMention")))
			changed = true
		}
		if dget(discord, "autoThread", nil) != nil {
			dset(discordIcebath, "auto_thread", toYAML(dindex(discord, "autoThread")))
			changed = true
		}
		if changed && m.execute {
			dset(icebathCfg, "discord", discordIcebath)
			dumpYAML(path, icebathCfg)
		}
	}
	complexArchive := jsonutil.NewObject()
	for _, e := range ditems(channels) {
		if !isDict(e.Val) {
			continue
		}
		complexKeys := jsonutil.NewObject()
		for _, inner := range ditems(e.Val) {
			if containsString([]string{"botToken", "appToken", "allowFrom", "enabled"}, inner.Key) || !anyutil.IsTrue(inner.Val) ||
				containsString([]string{"requireMention", "autoThread"}, inner.Key) {
				continue
			}
			complexKeys.Set(inner.Key, inner.Val)
		}
		if complexKeys.Len() > 0 {
			complexArchive.Set(e.Key, complexKeys)
		}
	}
	if complexArchive.Len() > 0 && m.archiveDir != "" {
		if m.execute {
			m.archiveJSON("channels-deep-config.json", complexArchive)
		}
		m.record("deep-channels", "openclaw.json channels (advanced settings)", "archive/channels-deep-config.json", StatusArchived,
			fmt.Sprintf("Deep channel config for %d channels archived", complexArchive.Len()))
	}
}

// Maps cdpUrl and headless into the browser section and archives the other browser settings
func (m *Migrator) migrateBrowserConfig(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	browser := or(dget(cfg, "browser", nil), jsonutil.NewObject())
	if !anyutil.IsTrue(browser) {
		m.record("browser-config", "", "", StatusSkipped, "No browser configuration found")
		return
	}
	path := m.configPath()
	icebathCfg := loadYAML(path)
	browserIcebath := or(dget(icebathCfg, "browser", nil), newYAMLMap())
	changed := false
	if anyutil.IsTrue(dget(browser, "cdpUrl", nil)) {
		dset(browserIcebath, "cdp_url", toYAML(dindex(browser, "cdpUrl")))
		changed = true
	}
	if dget(browser, "headless", nil) != nil {
		dset(browserIcebath, "headless", toYAML(dindex(browser, "headless")))
		changed = true
	}
	if changed {
		dset(icebathCfg, "browser", browserIcebath)
		if m.execute {
			m.maybeBackup(path)
			dumpYAML(path, icebathCfg)
		}
		m.record("browser-config", "openclaw.json browser.*", "config.yaml browser", StatusMigrated, "")
	}
	advanced := jsonutil.NewObject()
	for _, e := range ditems(browser) {
		if e.Key != "cdpUrl" && e.Key != "headless" && anyutil.IsTrue(e.Val) {
			advanced.Set(e.Key, e.Val)
		}
	}
	if advanced.Len() > 0 && m.archiveDir != "" {
		if m.execute {
			m.archiveJSON("browser-config.json", advanced)
		}
		m.record("browser-config", "openclaw.json browser (advanced)", "archive/browser-config.json", StatusArchived, "")
	}
}

// Maps the exec timeout onto terminal.timeout, imports the Brave key when secrets are enabled and archives the tools config
func (m *Migrator) migrateToolsConfig(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	tools := or(dget(cfg, "tools", nil), jsonutil.NewObject())
	if !anyutil.IsTrue(tools) {
		m.record("tools-config", "", "", StatusSkipped, "No tools configuration found")
		return
	}
	path := m.configPath()
	icebathCfg := loadYAML(path)
	changed := false
	execCfg := or(dget(tools, "exec", nil), jsonutil.NewObject())
	if timeoutVal := orElse(dget(execCfg, "timeoutSec", nil), lazyGet(execCfg, "timeout")); anyutil.IsTrue(timeoutVal) {
		terminalCfg := or(dget(icebathCfg, "terminal", nil), newYAMLMap())
		dset(terminalCfg, "timeout", toYAML(timeoutVal))
		dset(icebathCfg, "terminal", terminalCfg)
		changed = true
	}
	webCfg := orElse(dget(tools, "web", nil), func() any { return or(dget(tools, "webSearch", nil), jsonutil.NewObject()) })
	searchCfg := webCfg
	if anyutil.IsTrue(dget(webCfg, "search", nil)) {
		searchCfg = dindex(webCfg, "search")
	}
	braveCfg := or(dget(searchCfg, "brave", nil), jsonutil.NewObject())
	braveKey := orElse(dget(braveCfg, "apiKey", nil), func() any {
		return orElse(dget(searchCfg, "braveApiKey", nil), lazyGet(webCfg, "braveApiKey"))
	})
	if s, ok := braveKey.(string); ok && s != "" && m.migrateSecrets {
		m.setEnvVar("BRAVE_API_KEY", s, "tools.web.search.brave.apiKey")
	}
	if changed && m.execute {
		m.maybeBackup(path)
		dumpYAML(path, icebathCfg)
		m.record("tools-config", "openclaw.json tools.*", "config.yaml terminal", StatusMigrated, "")
	}
	if m.archiveDir != "" {
		if m.execute {
			m.archiveJSON("tools-config.json", tools)
		}
		m.record("tools-config", "openclaw.json tools (full)", "archive/tools-config.json", StatusArchived, "Full tools config archived for reference")
	}
}

// Icebath approval modes of the OpenClaw ones
var approvalModeMap = map[string]string{"auto": "off", "always": "manual", "smart": "smart", "manual": "manual"}

// Maps the approval mode into approvals.mode and archives the approval rules
func (m *Migrator) migrateApprovalsConfig(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	approvals := or(dget(cfg, "approvals", nil), jsonutil.NewObject())
	if !anyutil.IsTrue(approvals) {
		m.record("approvals-config", "", "", StatusSkipped, "No approvals configuration found")
		return
	}
	path := m.configPath()
	icebathCfg := loadYAML(path)
	execApprovals := or(dget(approvals, "exec", nil), jsonutil.NewObject())
	var execMode any
	if isDict(execApprovals) {
		execMode = dget(execApprovals, "mode", nil)
	}
	mode := orElse(execMode, func() any { return orElse(dget(approvals, "mode", nil), lazyGet(approvals, "defaultMode")) })
	if anyutil.IsTrue(mode) {
		requireHashable(mode)
		icebathMode := "manual"
		if s, ok := mode.(string); ok {
			if mapped, known := approvalModeMap[s]; known {
				icebathMode = mapped
			}
		}
		dset(dsetdefaultDict(icebathCfg, "approvals"), "mode", icebathMode)
		if m.execute {
			m.maybeBackup(path)
			dumpYAML(path, icebathCfg)
		}
		m.record("approvals-config", "openclaw.json approvals.mode", "config.yaml approvals.mode", StatusMigrated,
			fmt.Sprintf("Mapped '%s' -> '%s'", pyStr(mode), icebathMode))
	}
	if pyLen(approvals) > 1 && m.archiveDir != "" {
		if m.execute {
			m.archiveJSON("approvals-config.json", approvals)
		}
		m.record("approvals-config", "openclaw.json approvals (rules)", "archive/approvals-config.json", StatusArchived, "")
	}
}

// Archives one top level config section, recording it skipped when absent
func (m *Migrator) archiveSection(kind string, value any, emptyReason, file, source, reason string) {
	if !anyutil.IsTrue(value) {
		m.record(kind, "", "", StatusSkipped, emptyReason)
		return
	}
	if m.archiveDir != "" && m.execute {
		m.archiveJSON(file, value)
	}
	m.record(kind, source, "archive/"+file, StatusArchived, reason)
}

// Archives the memory backend settings
func (m *Migrator) migrateMemoryBackend(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	m.archiveSection("memory-backend", or(dget(cfg, "memory", nil), jsonutil.NewObject()), "No memory backend configuration found",
		"memory-backend-config.json", "openclaw.json memory.*", "Memory backend config (QMD, vector search, citations) archived for manual review")
}

// Archives the skills registry settings
func (m *Migrator) migrateSkillsConfig(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	skills := or(dget(cfg, "skills", nil), jsonutil.NewObject())
	entries := or(dget(skills, "entries", nil), jsonutil.NewObject())
	if !anyutil.IsTrue(entries) && !anyutil.IsTrue(skills) {
		m.record("skills-config", "", "", StatusSkipped, "No skills registry configuration found")
		return
	}
	if m.archiveDir != "" && m.execute {
		m.archiveJSON("skills-registry-config.json", skills)
	}
	m.record("skills-config", "openclaw.json skills.*", "archive/skills-registry-config.json", StatusArchived,
		fmt.Sprintf("Skills registry config (%d entries) archived", pyLen(entries)))
}

// Archives the UI theme and identity settings
func (m *Migrator) migrateUIIdentity(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	m.archiveSection("ui-identity", or(dget(cfg, "ui", nil), jsonutil.NewObject()), "No UI/identity configuration found",
		"ui-identity-config.json", "openclaw.json ui.*", "UI theme and identity settings archived")
}

// Archives the logging and diagnostics settings together
func (m *Migrator) migrateLoggingConfig(cfg *jsonutil.Object) {
	cfg = m.cfgOr(cfg)
	combined := jsonutil.NewObject()
	if v := or(dget(cfg, "logging", nil), jsonutil.NewObject()); anyutil.IsTrue(v) {
		combined.Set("logging", v)
	}
	if v := or(dget(cfg, "diagnostics", nil), jsonutil.NewObject()); anyutil.IsTrue(v) {
		combined.Set("diagnostics", v)
	}
	if combined.Len() == 0 {
		m.record("logging-config", "", "", StatusSkipped, "No logging/diagnostics configuration found")
		return
	}
	if m.archiveDir != "" && m.execute {
		m.archiveJSON("logging-diagnostics-config.json", combined)
	}
	m.record("logging-config", "openclaw.json logging/diagnostics", "archive/logging-diagnostics-config.json", StatusArchived, "")
}

// Sets one .env variable during apply, an existing key a conflict unless overwriting
func (m *Migrator) setEnvVar(key string, value any, sourceLabel string) {
	envPath := pathutil.Join(m.targetRoot, ".env")
	if m.execute {
		envData := parseEnvFile(envPath)
		if _, has := envData.get(key); has && !m.overwrite {
			m.record("env-var", sourceLabel, ".env "+key, StatusConflict, "Env var "+key+" already set")
			return
		}
		envData.set(key, pyStr(value))
		saveEnvFile(envPath, envData)
	}
	m.record("env-var", sourceLabel, ".env "+key, StatusMigrated, "")
}

// Writes MIGRATION_NOTES.md listing what needs manual attention after an apply
func (m *Migrator) generateMigrationNotes() {
	if m.outputDir == "" {
		return
	}
	notes := []string{
		"# OpenClaw -> Icebath Migration Notes",
		"",
		"This document lists items that require manual attention after migration.",
		"",
		"## PM2 / External Processes",
		"",
		"Your PM2 processes (Discord bots, Telegram bots, etc.) are NOT affected",
		"by this migration. They run independently and will continue working.",
		"No action needed for PM2-managed processes.",
		"",
	}
	var archived, conflicts []ItemResult
	hasCronConfig, hasCronStore, hasSkills, hasWhatsApp := false, false, false, false
	for _, i := range m.items {
		switch i.Status {
		case StatusArchived:
			archived = append(archived, i)
		case StatusConflict:
			conflicts = append(conflicts, i)
		}
		dest := i.DestinationText()
		if i.Kind == "cron-jobs" && i.Status == StatusArchived && dest != "" {
			hasCronConfig = hasCronConfig || strings.HasSuffix(dest, "cron-config.json")
			hasCronStore = hasCronStore || strings.HasSuffix(dest, "cron-store")
		}
		hasSkills = hasSkills || (i.Kind == "skills" && i.Status == StatusMigrated)
		hasWhatsApp = hasWhatsApp || (i.Kind == "whatsapp-settings" && i.Status == StatusMigrated)
	}
	if len(archived) > 0 {
		notes = append(notes,
			"## Archived Items (Manual Review Needed)",
			"",
			"These OpenClaw configurations were archived because they don't have a",
			"direct 1:1 mapping in Icebath. Review each file and recreate manually:",
			"",
		)
		for _, i := range archived {
			notes = append(notes, "- **"+i.Kind+"**: `"+derefOr(i.Destination, "None")+"` -- "+i.Reason)
		}
		notes = append(notes, "")
	}
	if len(conflicts) > 0 {
		notes = append(notes,
			"## Conflicts (Existing Icebath Config Not Overwritten)",
			"",
			"These items already existed in your Icebath config. Re-run with",
			"`--overwrite` to force, or merge manually:",
			"",
		)
		for _, i := range conflicts {
			notes = append(notes, "- **"+i.Kind+"**: "+i.Reason)
		}
		notes = append(notes, "")
	}
	notes = append(notes,
		"## IMPORTANT: Archive the OpenClaw Directory",
		"",
		"After migration, your OpenClaw directory still exists on disk with workspace",
		"state files (todo.json, sessions, logs). If the Icebath agent discovers these",
		"directories, it may read/write to them instead of the Icebath state, causing",
		"confusion (e.g., cron jobs reading a different todo list than interactive sessions).",
		"",
		"**Strongly recommended:** Run `icebath claw cleanup` to rename the OpenClaw",
		"directory to `.openclaw.pre-migration`. This prevents the agent from finding it.",
		"The directory is renamed, not deleted — you can undo this at any time.",
		"",
		"If you skip this step and notice the agent getting confused about workspaces",
		"or todo lists, run `icebath claw cleanup` to fix it.",
		"",
		"## Icebath-Specific Setup",
		"",
		"After migration, you may want to:",
		"- Run `icebath claw cleanup` to archive the OpenClaw directory (prevents state confusion)",
		"- Start `icebath` and type `/setup` to configure any remaining settings",
		"- Run `icebath mcp list` to verify MCP servers were imported correctly",
	)
	if hasCronConfig {
		notes = append(notes, "- Run `icebath cron` to recreate scheduled tasks (see archive/cron-config.json)")
	} else if hasCronStore {
		notes = append(notes, "- Run `icebath cron` to recreate scheduled tasks (see archived cron-store)")
	}
	if hasSkills {
		notes = append(notes,
			"",
			"## Imported Skills",
			"",
			"Imported skills require a new session to take effect. After migration,",
			"restart your agent or start a new chat session, then run `/skills`",
			"to verify they loaded correctly.",
			"",
		)
	}
	if hasWhatsApp {
		notes = append(notes,
			"",
			"## WhatsApp Requires Re-Pairing",
			"",
			"WhatsApp uses QR-code pairing, not token-based auth. Your allowlist",
			"was migrated, but you must re-pair the device by running:",
			"",
			"    icebath whatsapp",
			"",
		)
	}
	notes = append(notes,
		"- Run `icebath gateway install` if you need the gateway service",
		"- Review `~/.icebath/config.yaml` for any adjustments",
		"",
	)
	if m.execute {
		mkdirAll(m.outputDir)
		writeText(pathutil.Join(m.outputDir, "MIGRATION_NOTES.md"), strings.Join(notes, "\n")+"\n")
	}
}
