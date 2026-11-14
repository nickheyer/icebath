package tool

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
)

type PlatformOptions struct {
	DefaultMCPServers bool
	Scope             Scope
}

type Strip struct {
	Toolset string
	Tools   []string
	Entries []string
	Hint    string
}

type toolsetItem struct {
	text     string
	isString bool
}

func recentlyShippedToolsets() []string {
	return nil
}

func resolveNode(n *yaml.Node) *yaml.Node {
	for n != nil {
		switch n.Kind {
		case yaml.DocumentNode:
			if len(n.Content) == 0 {
				return nil
			}
			n = n.Content[0]
		case yaml.AliasNode:
			n = n.Alias
		default:
			return n
		}
	}
	return nil
}

func mapGet(n *yaml.Node, key string) *yaml.Node {
	n = resolveNode(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if resolveNode(n.Content[i]).Value == key {
			return resolveNode(n.Content[i+1])
		}
	}
	return nil
}

func mapKeys(n *yaml.Node) []string {
	n = resolveNode(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var out []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, resolveNode(n.Content[i]).Value)
	}
	return out
}

func isNullNode(n *yaml.Node) bool {
	return n == nil || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null")
}

func isStringNode(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.ShortTag() == "!!str"
}

func parseToolsetsNode(n *yaml.Node) ([]toolsetItem, bool) {
	n = resolveNode(n)
	if n == nil {
		return nil, false
	}
	if n.Kind == yaml.SequenceNode {
		out := make([]toolsetItem, 0, len(n.Content))
		for _, it := range n.Content {
			it = resolveNode(it)
			if it == nil || it.Kind != yaml.ScalarNode {
				out = append(out, toolsetItem{})
				continue
			}
			out = append(out, toolsetItem{text: it.Value, isString: it.ShortTag() == "!!str"})
		}
		return out, true
	}
	if !isStringNode(n) || !strings.HasPrefix(strings.TrimSpace(n.Value), "[") {
		return nil, false
	}
	var parsed yaml.Node
	if err := yaml.Unmarshal([]byte(strings.TrimSpace(n.Value)), &parsed); err != nil {
		return nil, false
	}
	seq := resolveNode(&parsed)
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil, false
	}
	out := make([]toolsetItem, 0, len(seq.Content))
	for _, it := range seq.Content {
		it = resolveNode(it)
		if it == nil || it.Kind != yaml.ScalarNode {
			return nil, false
		}
		out = append(out, toolsetItem{text: it.Value, isString: true})
	}
	return out, true
}

func itemTexts(items []toolsetItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.text)
	}
	return out
}

func savedPlatformNode(cfg *config.Config, platform string) (*yaml.Node, error) {
	v, err := cfg.Extra.PlatformToolsets.MarshalYAML()
	if err != nil {
		return nil, err
	}
	n, ok := v.(*yaml.Node)
	if !ok {
		return nil, fmt.Errorf("platform_toolsets encoded as %T", v)
	}
	return mapGet(n, platform), nil
}

func (r *Registry) platformList(cfg *config.Config, platform string) ([]string, bool, error) {
	if list, ok := cfg.Extra.PlatformToolsets.Get(platform); ok {
		return slices.Clone(list), true, nil
	}
	if !slices.Contains(cfg.Extra.PlatformToolsets.Unreadable(), platform) {
		return nil, false, nil
	}
	raw, err := savedPlatformNode(cfg, platform)
	if err != nil {
		return nil, false, err
	}
	if items, ok := parseToolsetsNode(raw); ok {
		return itemTexts(items), true, nil
	}
	r.warnInvalidValue(platform, raw)
	return nil, false, nil
}

func (r *Registry) warnInvalidValue(platform string, raw *yaml.Node) {
	r.warnMu.Lock()
	defer r.warnMu.Unlock()
	if r.warnedSaved[platform] {
		return
	}
	r.warnedSaved[platform] = true
	shown := "null"
	if raw != nil && !isNullNode(raw) {
		shown = raw.Value
		if raw.Kind != yaml.ScalarNode {
			out, err := yaml.Marshal(raw)
			if err == nil {
				shown = strings.TrimSpace(string(out))
			}
		}
	}
	slog.Warn(fmt.Sprintf("platform_toolsets.%s is %q, expected a YAML list of toolset names (e.g. [terminal, file, web]) - falling back to the platform default. Run `icebath tools` to reconfigure.", platform, shown))
}

func (r *Registry) PlatformToolsetsSaved(cfg *config.Config, platform string) (bool, error) {
	_, explicit, err := r.platformList(cfg, platform)
	return explicit, err
}

func (r *Registry) pluginKeys(ctx context.Context) (map[string]bool, error) {
	if r.plugins == nil {
		return nil, &UnfilledError{Name: "plugin toolsets"}
	}
	keys, err := r.plugins.Keys(ctx)
	if err != nil {
		slog.Debug("plugin toolset keys unavailable", "error", err)
		return map[string]bool{}, nil
	}
	return setOf(keys), nil
}

func (r *Registry) PluginToolsetKeys(ctx context.Context) ([]string, error) {
	keys, err := r.pluginKeys(ctx)
	if err != nil {
		return nil, err
	}
	return sortedKeys(keys), nil
}

func (r *Registry) EffectiveConfigurable(ctx context.Context) ([]Configurable, error) {
	if r.plugins == nil {
		return nil, &UnfilledError{Name: "plugin toolsets"}
	}
	out := ConfigurableToolsets()
	seen := map[string]bool{}
	for _, c := range out {
		seen[c.Key] = true
	}
	extra, err := r.plugins.Toolsets(ctx)
	if err != nil {
		slog.Debug("plugin toolsets unavailable", "error", err)
		return out, nil
	}
	for _, c := range extra {
		if !seen[c.Key] {
			seen[c.Key] = true
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *Registry) ChecklistKeys(ctx context.Context, platform string) ([]string, error) {
	effective, err := r.EffectiveConfigurable(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range effective {
		if ToolsetAllowed(c.Key, platform) && !slices.Contains(ConfigOnlyToolsets(), c.Key) {
			out = append(out, c.Key)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (r *Registry) ToolsetLabel(ctx context.Context, key string) (string, error) {
	effective, err := r.EffectiveConfigurable(ctx)
	if err != nil {
		return "", err
	}
	for _, c := range effective {
		if c.Key == key {
			return c.Label, nil
		}
	}
	return key, nil
}

func (r *Registry) ExplicitToolsetKeys(ctx context.Context) ([]string, error) {
	keys, err := r.pluginKeys(ctx)
	if err != nil {
		return nil, err
	}
	for k := range configurableKeys() {
		keys[k] = true
	}
	return sortedKeys(keys), nil
}

func platformEnableEnv() [][2]string {
	return [][2]string{{"telegram", "TELEGRAM_BOT_TOKEN"}, {"discord", "DISCORD_BOT_TOKEN"}, {"slack", "SLACK_BOT_TOKEN"},
		{"whatsapp", "WHATSAPP_ENABLED"}, {"qqbot", "QQ_APP_ID"}}
}

func (r *Registry) EnabledPlatforms(ctx context.Context) ([]string, error) {
	if r.config == nil {
		return nil, &UnfilledError{Name: "config store"}
	}
	out := []string{"cli"}
	for _, pair := range platformEnableEnv() {
		v, ok, err := r.config.Env().Get(ctx, pair[1])
		if err != nil {
			return nil, err
		}
		if ok && v != "" {
			out = append(out, pair[0])
		}
	}
	return out, nil
}

func (r *Registry) PlatformSummary(ctx context.Context, cfg *config.Config, platforms []string, o PlatformOptions) (map[string][]string, error) {
	if platforms == nil {
		enabled, err := r.EnabledPlatforms(ctx)
		if err != nil {
			return nil, err
		}
		platforms = enabled
	}
	out := make(map[string][]string, len(platforms))
	for _, p := range platforms {
		tools, err := r.PlatformTools(ctx, cfg, p, o)
		if err != nil {
			return nil, err
		}
		out[p] = tools
	}
	return out, nil
}

func ParseEnabledFlag(v config.Scalar, def bool) bool {
	switch v.Kind() {
	case config.KindBool:
		b, _ := v.AsBool()
		return b
	case config.KindInt:
		n, _ := v.AsInt()
		return n != 0
	case config.KindString:
		if on, ok := v.Truth(); ok {
			return on
		}
	}
	return def
}

func mcpServerEnabled(v config.Scalar) bool {
	switch v.Kind() {
	case config.KindNull:
		return true
	case config.KindBool:
		b, _ := v.AsBool()
		return b
	case config.KindInt:
		n, _ := v.AsInt()
		return n != 0
	case config.KindFloat:
		f, _ := v.AsFloat()
		return f != 0
	}
	if on, ok := v.Truth(); ok {
		return on
	}
	slog.Warn(fmt.Sprintf("MCP config expected a boolean-ish value, got %q; using default=true", v.String()))
	return true
}

func (r *Registry) EnabledMCPServers(ctx context.Context, cfg *config.Config) ([]string, error) {
	names, err := r.enabledMCPServers(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return sortedKeys(names), nil
}

func (r *Registry) enabledMCPServers(ctx context.Context, cfg *config.Config) (map[string]bool, error) {
	if r.plugins == nil {
		return nil, &UnfilledError{Name: "plugin toolsets"}
	}
	servers := cfg.Extra.MCPServers
	names := map[string]bool{}
	for _, name := range servers.Keys() {
		entry, _ := servers.Get(name)
		if mcpServerEnabled(entry.Enabled) {
			names[name] = true
		}
	}
	configured := setOf(servers.Keys(), servers.Unreadable())
	portable, err := r.plugins.PortableMCPServers(ctx)
	if err != nil {
		slog.Debug("Failed to include portable MCP servers", "error", err)
		return names, nil
	}
	for _, name := range portable {
		if !configured[name] {
			names[name] = true
		}
	}
	return names, nil
}

func defaultOff(platform string, explicit bool) map[string]bool {
	off := setOf(DefaultOffToolsets())
	restrictions := toolsetRestrictions()
	if _, restricted := restrictions[platform]; off[platform] && !restricted {
		delete(off, platform)
	}
	if explicit {
		for ts := range off {
			if slices.Contains(restrictions[ts], platform) {
				delete(off, ts)
			}
		}
	}
	return off
}

func subset(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func (r *Registry) configurableSubsetOf(tools map[string]bool, platform string, at Scope) map[string]bool {
	out := map[string]bool{}
	for _, c := range ConfigurableToolsets() {
		if !ToolsetAllowed(c.Key, platform) {
			continue
		}
		ts := setOf(r.resolveLocked(c.Key, at, false))
		if len(ts) > 0 && subset(ts, tools) {
			out[c.Key] = true
		}
	}
	return out
}

func (r *Registry) enableRecentlyShipped(enabled map[string]bool, cfg *config.Config, platform string, at Scope) {
	var declined map[string]bool
	if offered, ok := cfg.Extra.KnownBuiltinToolsets.Get(platform); ok {
		declined = setOf(offered)
	}
	var composite map[string]bool
	shipped := recentlyShippedToolsets()
	slices.Sort(shipped)
	for _, key := range shipped {
		if enabled[key] || declined[key] || !ToolsetAllowed(key, platform) {
			continue
		}
		ts := setOf(r.resolveLocked(key, at, false))
		if composite == nil {
			composite = setOf(r.resolveLocked(PlatformDefaultToolset(platform), at, true))
		}
		if len(ts) == 0 || !subset(ts, composite) {
			continue
		}
		enabled[key] = true
	}
}

func (r *Registry) explicitToolsets(names []string, known map[string]bool, cfg *config.Config, platform string, explicit bool, at Scope) map[string]bool {
	enabled := map[string]bool{}
	composite := map[string]bool{}
	for _, ts := range names {
		if known[ts] {
			if ToolsetAllowed(ts, platform) {
				enabled[ts] = true
			}
			continue
		}
		if _, static := r.static[ts]; static {
			for _, t := range r.resolveLocked(ts, at, true) {
				composite[t] = true
			}
		}
	}
	if len(composite) > 0 {
		off := defaultOff(platform, explicit)
		for k := range r.configurableSubsetOf(composite, platform, at) {
			if !off[k] {
				enabled[k] = true
			}
		}
	}
	r.enableRecentlyShipped(enabled, cfg, platform, at)
	return enabled
}

func (r *Registry) compositeToolsets(names []string, platform string, explicit, xai bool, at Scope) map[string]bool {
	all := map[string]bool{}
	for _, ts := range names {
		for _, t := range r.resolveLocked(ts, at, true) {
			all[t] = true
		}
	}
	enabled := r.configurableSubsetOf(all, platform, at)
	off := defaultOff(platform, explicit)
	if ToolsetAllowed("x_search", platform) && xai {
		enabled["x_search"] = true
		delete(off, "x_search")
	}
	for k := range off {
		delete(enabled, k)
	}
	return enabled
}

func (r *Registry) recoverNative(enabled map[string]bool, platform string, skip map[string]bool, at Scope) {
	universe := setOf(r.resolveLocked(PlatformDefaultToolset(platform), at, true))
	configurableUniverse := map[string]bool{}
	for _, c := range ConfigurableToolsets() {
		for _, t := range r.resolveLocked(c.Key, at, true) {
			configurableUniverse[t] = true
		}
	}
	claimed := map[string]bool{}
	for ts := range enabled {
		for _, t := range r.resolveLocked(ts, at, true) {
			claimed[t] = true
		}
	}
	for _, t := range r.toolsets {
		if strings.HasPrefix(t.Name, bundlePrefix) {
			skip[t.Name] = true
		}
	}
	for _, ts := range DefaultOffToolsets() {
		if ts != platform {
			skip[ts] = true
		}
	}
	for _, t := range r.toolsets {
		if skip[t.Name] || len(t.Includes) > 0 || t.Posture {
			continue
		}
		tools := setOf(r.resolveLocked(t.Name, at, false))
		if len(tools) == 0 || !subset(tools, universe) || subset(tools, configurableUniverse) {
			continue
		}
		if !subset(tools, claimed) {
			enabled[t.Name] = true
			for k := range tools {
				claimed[k] = true
			}
		}
	}
}

func enabledPluginToolsets(cfg *config.Config, platform string, names []string, pluginKeys map[string]bool) map[string]bool {
	known := map[string]bool{}
	if list, ok := cfg.Extra.KnownPluginToolsets.Get(platform); ok {
		known = setOf(list)
	}
	off := setOf(DefaultOffToolsets())
	out := map[string]bool{}
	for k := range pluginKeys {
		if slices.Contains(names, k) || (!off[k] && !known[k]) {
			out[k] = true
		}
	}
	return out
}

func contextEngineActive(cfg *config.Config) bool {
	engine := cfg.Context.Engine
	if engine == "" {
		engine = "compressor"
	}
	name := strings.ToLower(strings.TrimSpace(engine))
	return name != "" && name != "compressor"
}

func (r *Registry) mergeMCPServers(enabledServers map[string]bool, cfg *config.Config, names []string, passthrough map[string]bool, includeDefault bool) map[string]bool {
	known := setOf(cfg.Extra.MCPServers.Keys(), cfg.Extra.MCPServers.Unreadable())
	for k := range enabledServers {
		known[k] = true
	}
	mapped := map[string]bool{}
	for ts := range passthrough {
		if bare, ok := strings.CutPrefix(ts, "mcp-"); ok && !known[ts] && known[bare] {
			mapped[bare] = true
			continue
		}
		mapped[ts] = true
	}
	result := map[string]bool{}
	for ts := range mapped {
		if !known[ts] {
			result[ts] = true
		}
	}
	if slices.Contains(names, "no_mcp") {
		delete(result, "no_mcp")
		return result
	}
	listedKnown := false
	for ts := range mapped {
		if known[ts] {
			listedKnown = true
			break
		}
	}
	if includeDefault && !listedKnown {
		for k := range enabledServers {
			result[k] = true
		}
		return result
	}
	for ts := range mapped {
		if enabledServers[ts] {
			result[ts] = true
		}
	}
	return result
}

func disabledNames(cfg *config.Config) []string {
	var out []string
	for _, name := range cfg.Agent.DisabledToolsets.Names() {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func (r *Registry) pruneStripped(enabled map[string]bool, disabled []string, at Scope) map[string]bool {
	resolved := map[string]map[string]bool{}
	surviving := map[string]bool{}
	for name := range enabled {
		if slices.Contains(disabled, name) {
			continue
		}
		tools := map[string]bool{}
		if r.validLocked(name, at) {
			tools = setOf(r.resolveLocked(name, at, true))
		}
		resolved[name] = tools
		for t := range tools {
			surviving[t] = true
		}
	}
	r.applySelection(surviving, disabled, true, at)
	out := map[string]bool{}
	for name, tools := range resolved {
		if len(tools) == 0 {
			out[name] = true
			continue
		}
		for t := range tools {
			if surviving[t] {
				out[name] = true
				break
			}
		}
	}
	return out
}

func (r *Registry) disabledEntryTools(name string, at Scope) map[string]bool {
	universe := setOf(r.resolveLocked(name, at, true), legacyToolsets()[name])
	kept := setOf(sortedKeys(universe))
	r.applySelection(kept, []string{name}, true, at)
	out := map[string]bool{}
	for t := range universe {
		if !kept[t] {
			out[t] = true
		}
	}
	return out
}

func (r *Registry) PartiallyStripped(cfg *config.Config, enabled []string, at Scope) []Strip {
	r.mu.RLock()
	defer r.mu.RUnlock()
	disabled := disabledNames(cfg)
	var out []Strip
	for _, name := range sortedUnique(enabled) {
		tools := map[string]bool{}
		if r.validLocked(name, at) {
			tools = setOf(r.resolveLocked(name, at, true))
		}
		by := map[string]map[string]bool{}
		for _, entry := range disabled {
			kept := setOf(sortedKeys(tools))
			r.applySelection(kept, []string{entry}, true, at)
			removed := map[string]bool{}
			for t := range tools {
				if !kept[t] {
					removed[t] = true
				}
			}
			if len(removed) > 0 {
				by[entry] = removed
			}
		}
		if len(by) == 0 {
			continue
		}
		entries := make([]string, 0, len(by))
		stripped := map[string]bool{}
		for entry, removed := range by {
			entries = append(entries, entry)
			for t := range removed {
				stripped[t] = true
			}
		}
		slices.Sort(entries)
		clears := true
		for _, entry := range entries {
			if !subset(r.disabledEntryTools(entry, at), tools) {
				clears = false
				break
			}
		}
		hint := fmt.Sprintf("remove %s from agent.disabled_toolsets to restore", strings.Join(entries, ", "))
		if clears {
			hint = fmt.Sprintf("run `icebath tools enable %s` to restore", name)
		}
		out = append(out, Strip{Toolset: name, Tools: sortedKeys(stripped), Entries: entries, Hint: hint})
	}
	return out
}

func (r *Registry) PlatformTools(ctx context.Context, cfg *config.Config, platform string, o PlatformOptions) ([]string, error) {
	at := o.Scope
	names, explicit, err := r.platformList(cfg, platform)
	if err != nil {
		return nil, err
	}
	if !explicit {
		names = []string{PlatformDefaultToolset(platform)}
	}
	var expanded []string
	for _, name := range names {
		if name == "icebath" {
			expanded = append(expanded, "icebath-cli", "icebath-api-server")
			continue
		}
		expanded = append(expanded, name)
	}
	names = expanded
	configurable := configurableKeys()
	pluginKeys, err := r.pluginKeys(ctx)
	if err != nil {
		return nil, err
	}
	defaults := platformDefaultKeys()
	known := setOf(sortedKeys(configurable), sortedKeys(pluginKeys))
	explicitBranch := slices.ContainsFunc(names, func(ts string) bool { return known[ts] })
	xai := false
	if !explicitBranch && ToolsetAllowed("x_search", platform) {
		if r.credentials == nil {
			return nil, &UnfilledError{Name: "credential store"}
		}
		if xai, err = r.credentials.XAIPresent(ctx); err != nil {
			return nil, err
		}
	}
	enabledServers, err := r.enabledMCPServers(ctx, cfg)
	if err != nil {
		return nil, err
	}
	r.mu.RLock()
	var enabled map[string]bool
	if explicitBranch {
		enabled = r.explicitToolsets(names, known, cfg, platform, explicit, at)
	} else {
		enabled = r.compositeToolsets(names, platform, explicit, xai, at)
	}
	r.recoverNative(enabled, platform, setOf(sortedKeys(configurable), sortedKeys(pluginKeys), sortedKeys(defaults)), at)
	for k := range enabledPluginToolsets(cfg, platform, names, pluginKeys) {
		enabled[k] = true
	}
	if contextEngineActive(cfg) && !(explicit && len(names) == 0) {
		enabled["context_engine"] = true
	}
	passthrough := map[string]bool{}
	for _, ts := range names {
		if !known[ts] && !defaults[ts] {
			passthrough[ts] = true
		}
	}
	for k := range r.mergeMCPServers(enabledServers, cfg, names, passthrough, o.DefaultMCPServers) {
		enabled[k] = true
	}
	if !explicit && slices.Contains(cfg.Toolsets, "kanban") {
		enabled["kanban"] = true
	}
	if disabled := disabledNames(cfg); len(disabled) > 0 {
		enabled = r.pruneStripped(enabled, disabled, at)
	}
	anyValid := false
	if explicit && len(names) > 0 {
		for _, ts := range names {
			if ts != "" && r.validLocked(ts, at) {
				anyValid = true
				break
			}
		}
	}
	r.mu.RUnlock()
	if explicit && len(names) > 0 && !anyValid {
		if err := r.warnAllInvalid(ctx, platform, names, cfg, at); err != nil {
			return nil, err
		}
	}
	return sortedKeys(enabled), nil
}

func (r *Registry) warnAllInvalid(ctx context.Context, platform string, names []string, cfg *config.Config, at Scope) error {
	r.warnMu.Lock()
	warned := r.warnedSaved[platform]
	r.warnMu.Unlock()
	if warned {
		return nil
	}
	var named []string
	for _, ts := range names {
		if ts != "" {
			named = append(named, ts)
		}
	}
	if len(named) == 0 {
		return nil
	}
	for _, ts := range named {
		ok, err := r.knownPlatformBundle(ctx, platform, ts)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	saved := r.SavedToolsets(cfg, at)
	for _, ts := range named {
		ok, err := saved.Known(ctx, ts)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	r.warnMu.Lock()
	defer r.warnMu.Unlock()
	if r.warnedSaved[platform] {
		return nil
	}
	r.warnedSaved[platform] = true
	slog.Warn(fmt.Sprintf("platform '%s' has no valid toolsets configured (unknown name(s): %s) - tools will be unavailable. Run `icebath tools` to reconfigure.", platform, strings.Join(named, ", ")))
	return nil
}

func (r *Registry) SavePlatformTools(ctx context.Context, platform string, keys []string, at Scope) error {
	if r.config == nil {
		return &UnfilledError{Name: "config store"}
	}
	cfg, err := r.config.Load(ctx)
	if err != nil {
		return err
	}
	enabled := map[string]bool{}
	for _, k := range keys {
		if ToolsetAllowed(k, platform) {
			enabled[k] = true
		}
	}
	pluginKeys, err := r.pluginKeys(ctx)
	if err != nil {
		return err
	}
	configurable := configurableKeys()
	drop := setOf(sortedKeys(configurable), sortedKeys(pluginKeys), sortedKeys(platformDefaultKeys()), []string{"no_mcp"})
	existing, _, err := r.platformList(cfg, platform)
	if err != nil {
		return err
	}
	preserved := map[string]bool{}
	for _, e := range existing {
		if !drop[e] {
			preserved[e] = true
		}
	}
	cfg.Extra.PlatformToolsets.Set(platform, sortedKeys(setOf(sortedKeys(enabled), sortedKeys(preserved))))
	if len(pluginKeys) > 0 {
		cfg.Extra.KnownPluginToolsets.Set(platform, sortedKeys(pluginKeys))
	}
	cfg.Extra.KnownBuiltinToolsets.Set(platform, sortedKeys(configurable))
	newly := map[string]bool{}
	for k := range enabled {
		if !preserved[k] {
			newly[k] = true
		}
	}
	if !cfg.Agent.DisabledToolsets.IsZero() && len(newly) > 0 {
		parsed := cfg.Agent.DisabledToolsets.Names()
		r.mu.RLock()
		covered := map[string]bool{}
		for ts := range newly {
			for _, t := range r.resolveLocked(ts, at, true) {
				covered[t] = true
			}
		}
		var remaining []string
		for _, ts := range parsed {
			if newly[ts] {
				continue
			}
			if tools := r.disabledEntryTools(ts, at); len(tools) > 0 && subset(tools, covered) {
				continue
			}
			remaining = append(remaining, ts)
		}
		r.mu.RUnlock()
		if !slices.Equal(remaining, parsed) {
			cfg.Agent.DisabledToolsets = config.ListOf(remaining...)
		}
	}
	return r.config.Save(ctx, cfg)
}
