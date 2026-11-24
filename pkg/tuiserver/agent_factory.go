package tuiserver

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Toolset validation and plugin discovery an explicit toolset pin resolves through
var (
	validateToolset = tools.Default.ValidateToolset
	discoverPlugins = plugins.DiscoverPlugins
	// Coding posture selection, configured platform toolsets and disabled toolsets every session build reads
	codingSelection      = tools.CodingSelection
	platformTools        = tools.Default.PlatformTools
	loadDisabledToolsets = loadDisabledToolsetsConfig
	// Effective config of the active home with defaults merged, the load_config read
	loadConfig = func(ctx context.Context) (config.Tree, error) { return config.LoadContext(ctx, home.Dir(ctx), nil) }
)

// Platform every session this server builds runs on
const sessionPlatform = "tui"

// Config env of the active home the approval and toolset gates read
func configEnv(ctx context.Context) *tools.Env {
	tree, err := loadConfig(ctx)
	if err != nil {
		panic(err)
	}
	return &tools.Env{Home: home.Dir(ctx), Config: tree, TerminalScope: tools.GetTerminalScope(ctx)}
}

// Selection plus the project toolset the session carries unless selected or disabled
func withSessionToolsets(ctx context.Context, selection []string) []string {
	foldIn := map[string]bool{"project": true}
	for _, name := range selection {
		delete(foldIn, name)
	}
	for _, name := range loadDisabledToolsets(ctx) {
		delete(foldIn, name)
	}
	extra := make([]string, 0, len(foldIn))
	for name := range foldIn {
		extra = append(extra, name)
	}
	sort.Strings(extra)
	return append(append([]string{}, selection...), extra...)
}

// Writes a TUI notice line to stderr
func tuiNotice(text string) {
	fmt.Fprintln(os.Stderr, text)
}

// Resolves an ICEBATH_TUI_TOOLSETS pin, nil with all for every toolset, ok false when nothing was valid
func resolveExplicitToolsets(ctx context.Context, explicit []string, validate func(string) bool) (resolved []string, all bool, ok bool) {
	builtIn := []string{}
	unresolved := []string{}
	for _, name := range explicit {
		if validate(name) {
			builtIn = append(builtIn, name)
		}
	}
	for _, name := range explicit {
		if !slices.Contains(builtIn, name) {
			unresolved = append(unresolved, name)
		}
	}
	if len(unresolved) > 0 {
		pluginValid := func() (valid []string) {
			defer func() {
				if recover() != nil {
					valid = nil
				}
			}()
			discoverPlugins(ctx, false)
			for _, name := range unresolved {
				if validate(name) {
					valid = append(valid, name)
				}
			}
			return valid
		}()
		builtIn = append(builtIn, pluginValid...)
		unresolved = slices.DeleteFunc(unresolved, func(name string) bool { return slices.Contains(pluginValid, name) })
	}
	if slices.ContainsFunc(builtIn, func(name string) bool { return name == "all" || name == "*" }) {
		var ignored []string
		for _, name := range explicit {
			if name != "all" && name != "*" {
				ignored = append(ignored, name)
			}
		}
		if len(ignored) > 0 {
			tuiNotice("[tui] ICEBATH_TUI_TOOLSETS=all enables every toolset; ignoring additional entries: " + strings.Join(ignored, ", "))
		}
		return nil, true, true
	}
	if len(unresolved) == 0 {
		return builtIn, false, true
	}
	mcpNames, mcpDisabled := map[string]bool{}, map[string]bool{}
	func() {
		defer func() {
			if recover() != nil {
				mcpNames, mcpDisabled = map[string]bool{}, map[string]bool{}
			}
		}()
		raw, err := config.ReadRawConfig(ctx, home.Dir(ctx))
		if err != nil {
			panic(err)
		}
		servers, _ := raw["mcp_servers"].(map[string]any)
		for name, serverCfg := range servers {
			if cfg, isDict := serverCfg.(map[string]any); isDict {
				if tools.MCPServerEnabled(cfg) {
					mcpNames[name] = true
				} else {
					mcpDisabled[name] = true
				}
			}
		}
	}()
	var mcpValid, disabled, unknown []string
	for _, name := range unresolved {
		switch {
		case mcpNames[name]:
			mcpValid = append(mcpValid, name)
		case mcpDisabled[name]:
			disabled = append(disabled, name)
		default:
			if line, refused := tools.ToolsetTierRefusal(name); refused {
				tuiNotice("[tui] ignoring ICEBATH_TUI_TOOLSETS entry " + name + ", " + line)
				continue
			}
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		tuiNotice("[tui] ignoring unknown ICEBATH_TUI_TOOLSETS entries: " + strings.Join(unknown, ", "))
	}
	if len(disabled) > 0 {
		tuiNotice("[tui] ignoring disabled MCP servers in ICEBATH_TUI_TOOLSETS " +
			"(set enabled: true in config.yaml to use): " + strings.Join(disabled, ", "))
	}
	resolved = append(builtIn, mcpValid...)
	if len(resolved) == 0 {
		return nil, false, false
	}
	return resolved, false, true
}

// Agent toolsets of a session, nil for all, an explicit pin, the coding posture or the configured CLI set
func loadEnabledToolsets(ctx context.Context, platform string) []string {
	if platform == "" {
		platform = sessionPlatform
	}
	var explicit []string
	for _, item := range strings.Split(os.Getenv("ICEBATH_TUI_TOOLSETS"), ",") {
		if item = strings.TrimSpace(item); item != "" {
			explicit = append(explicit, item)
		}
	}
	fallbackNotice := ""
	if len(explicit) == 0 {
		var selection []string
		suppress(func() {
			selection = codingSelection(ctx, configEnv(ctx), platform, "")
		})
		if selection != nil {
			out := withSessionToolsets(ctx, selection)
			sort.Strings(out)
			return out
		}
	}
	scope := tools.ScopeKey(home.Dir(ctx))
	if len(explicit) > 0 {
		resolved, all, ok := resolveExplicitToolsets(ctx, explicit, func(name string) bool {
			return validateToolset(ctx, scope, name)
		})
		if ok {
			if all {
				return nil
			}
			return resolved
		}
		fallbackNotice = "[tui] no valid ICEBATH_TUI_TOOLSETS entries; using configured CLI toolsets"
	}
	var result []string
	loaded := func() (done bool) {
		defer func() {
			if recover() != nil {
				done = false
			}
		}()
		cfg, err := loadConfig(ctx)
		if err != nil {
			panic(err)
		}
		enabled := platformTools(ctx, scope, cfg, "cli", tools.PlatformToolsOptions{IncludeDefaultMCPServers: true})
		if fallbackNotice != "" {
			tuiNotice(fallbackNotice)
		}
		if len(enabled) > 0 {
			names := make([]string, 0, len(enabled))
			for name := range enabled {
				names = append(names, name)
			}
			result = withSessionToolsets(ctx, names)
			sort.Strings(result)
			return true
		}
		if tools.PlatformToolsetsExplicitlySaved(cfg, "cli") {
			result = []string{}
			return true
		}
		result = nil
		return true
	}()
	if !loaded {
		if fallbackNotice != "" {
			tuiNotice("[tui] no valid ICEBATH_TUI_TOOLSETS entries and configured CLI toolsets could not be loaded; enabling all toolsets")
		}
		return nil
	}
	return result
}

// Enabled and configured disabled toolsets a TUI session of the platform is built with
func GatewayToolsets(ctx context.Context, platform string) (enabled, disabled []string) {
	return loadEnabledToolsets(ctx, platform), loadDisabledToolsetsConfig(ctx)
}

// Configured agent disabled toolsets, nil when none
func loadDisabledToolsetsConfig(ctx context.Context) []string {
	disabled := settings.AgentDisabledToolsets.Strings(ctx)
	if len(disabled) == 0 {
		return nil
	}
	return disabled
}

// Tool progress modes a session may run

// Launch scoped model seed, empty when unset
func envModelSeed() string {
	if seed := os.Getenv("ICEBATH_MODEL"); seed != "" {
		return strings.TrimSpace(seed)
	}
	return strings.TrimSpace(os.Getenv("ICEBATH_INFERENCE_MODEL"))
}

// Model of a new session, the launch seed, the configured default, else the cached silent default
func resolveModel(ctx context.Context) (model string) {
	if seed := envModelSeed(); seed != "" {
		return seed
	}
	switch m := loadCfg(ctx)["model"].(type) {
	case map[string]any:
		return strings.TrimSpace(strOr(m["default"]))
	case string:
		if m != "" {
			return strings.TrimSpace(m)
		}
	}
	defer func() {
		if recover() != nil {
			model = provider.PreferredSilentDefaultModel
		}
	}()
	return provider.GetPreferredSilentDefaultModel(ctx, "openrouter")
}
