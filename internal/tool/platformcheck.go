package tool

import (
	"context"
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
)

const noToolsTail = "the agent will have no tools on this platform. Run `icebath tools` to reconfigure."

type SavedToolsets struct {
	r       *Registry
	at      Scope
	known   map[string]bool
	bundles map[string]bool
	plugins map[string]bool
}

func (r *Registry) SavedToolsets(cfg *config.Config, at Scope) *SavedToolsets {
	return r.savedToolsets(at, append(cfg.Extra.MCPServers.Keys(), cfg.Extra.MCPServers.Unreadable()...))
}

func (r *Registry) savedToolsets(at Scope, servers []string) *SavedToolsets {
	known := map[string]bool{"no_mcp": true}
	for _, name := range servers {
		known[name] = true
		known["mcp-"+name] = true
	}
	return &SavedToolsets{r: r, at: at, known: known}
}

func (s *SavedToolsets) Known(ctx context.Context, name string) (bool, error) {
	if s.r.Valid(name, s.at) || s.known[name] {
		return true, nil
	}
	if s.bundles == nil {
		bundles, err := s.r.platformBundles(ctx)
		if err != nil {
			return false, err
		}
		s.bundles = bundles
	}
	if s.bundles[name] {
		return true, nil
	}
	if s.plugins == nil {
		plugins, err := s.r.pluginNames(ctx)
		if err != nil {
			return false, err
		}
		s.plugins = plugins
	}
	return s.plugins[name], nil
}

func (r *Registry) platformBundles(ctx context.Context) (map[string]bool, error) {
	if r.config == nil {
		return nil, &UnfilledError{Name: "config store"}
	}
	out := map[string]bool{}
	for _, set := range config.BundledPlatformEnv() {
		out[bundlePrefix+set.Dir] = true
	}
	sets, _, err := r.config.Env().InstalledPlatforms(ctx, false)
	if err != nil {
		return nil, err
	}
	for _, set := range sets {
		out[bundlePrefix+set.Dir] = true
	}
	return out, nil
}

func (r *Registry) pluginNames(ctx context.Context) (map[string]bool, error) {
	if r.plugins == nil {
		return nil, &UnfilledError{Name: "plugin toolsets"}
	}
	keys, err := r.plugins.Keys(ctx)
	if err != nil {
		return nil, err
	}
	portable, err := r.plugins.PortableMCPServers(ctx)
	if err != nil {
		return nil, err
	}
	out := setOf(keys, portable)
	for _, name := range portable {
		out["mcp-"+name] = true
	}
	return out, nil
}

func (r *Registry) knownPlatformBundle(ctx context.Context, platform, name string) (bool, error) {
	if name != PlatformDefaultToolset(platform) {
		return false, nil
	}
	if slices.Contains(knownPlatforms(), platform) || slices.Contains(leftCorePlatforms(), platform) {
		return true, nil
	}
	for _, set := range config.BundledPlatformEnv() {
		if set.Dir == platform {
			return true, nil
		}
	}
	return r.platforms != nil && r.platforms(platform), nil
}

func nodeKind(n *yaml.Node) string {
	switch {
	case n.Kind == yaml.MappingNode:
		return "mapping"
	case n.Kind == yaml.SequenceNode:
		return "list"
	case n.ShortTag() == "!!int":
		return "integer"
	case n.ShortTag() == "!!float":
		return "float"
	case n.ShortTag() == "!!bool":
		return "boolean"
	}
	return "scalar"
}

func (r *Registry) ToolsetDiagnostics(ctx context.Context, raw *yaml.Node, at Scope) ([]string, error) {
	saved := mapGet(raw, "platform_toolsets")
	if saved == nil || saved.Kind != yaml.MappingNode || len(saved.Content) == 0 {
		return nil, nil
	}
	resolver := r.savedToolsets(at, mapKeys(mapGet(raw, "mcp_servers")))
	var warnings []string
	valid := 0
	for i := 0; i+1 < len(saved.Content); i += 2 {
		platform := resolveNode(saved.Content[i]).Value
		value := resolveNode(saved.Content[i+1])
		def := PlatformDefaultToolset(platform)
		known, err := resolver.Known(ctx, def)
		if err != nil {
			return nil, err
		}
		defaultValid := (known && ToolsetAllowed(def, platform)) || (r.platforms != nil && r.platforms(platform))
		platformValid := 0
		items, ok := parseToolsetsNode(value)
		if !ok {
			if defaultValid {
				valid++
				platformValid++
			}
			fallback := fmt.Sprintf("falling back to unknown default '%s'", def)
			if defaultValid {
				fallback = fmt.Sprintf("falling back to '%s'", def)
			}
			var detail string
			switch {
			case isNullNode(value):
				detail = "a null toolset value"
			case isStringNode(value):
				detail = fmt.Sprintf("invalid toolset value '%s'", value.Value)
			default:
				detail = fmt.Sprintf("invalid %s toolset value", nodeKind(value))
			}
			warnings = append(warnings, fmt.Sprintf("platform '%s' has %s — %s. Run `icebath tools` to configure explicitly.", platform, detail, fallback))
			if platformValid == 0 {
				warnings = append(warnings, fmt.Sprintf("platform '%s' has no valid toolsets configured — %s", platform, noToolsTail))
			}
			continue
		}
		for _, it := range items {
			if !it.isString || it.text == "" {
				continue
			}
			ok, err := resolver.Known(ctx, it.text)
			if err != nil {
				return nil, err
			}
			if !ok {
				if ok, err = r.knownPlatformBundle(ctx, platform, it.text); err != nil {
					return nil, err
				}
			}
			switch {
			case !ok:
				hint := ""
				if defaultValid {
					hint = fmt.Sprintf(" — did you mean '%s'?", def)
				}
				warnings = append(warnings, fmt.Sprintf("platform '%s' references unknown toolset '%s'%s", platform, it.text, hint))
			case ToolsetAllowed(it.text, platform):
				valid++
				platformValid++
			default:
				warnings = append(warnings, fmt.Sprintf("platform '%s' references toolset '%s' which is not available on this platform", platform, it.text))
			}
		}
		if platformValid == 0 {
			reason := "has no valid toolsets configured"
			if len(items) == 0 {
				reason = "is configured with an empty toolset list"
			}
			warnings = append(warnings, fmt.Sprintf("platform '%s' %s — %s", platform, reason, noToolsTail))
		}
	}
	if valid == 0 {
		warnings = append(warnings, "platform_toolsets resolves to zero valid toolsets — the agent will have no tools. Run `icebath tools` to reconfigure.")
	}
	return warnings, nil
}
