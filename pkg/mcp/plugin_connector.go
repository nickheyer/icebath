package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Tool environment of a plugin facing call, the bound session env else the active home with its config
func pluginEnv(ctx context.Context) *tools.Env {
	if env := tools.ToolEnvFrom(ctx); env != nil {
		return env
	}
	h := home.Dir(ctx)
	cfg, _ := config.LoadReadonly(ctx, h, nil)
	cwd, _ := os.Getwd()
	return &tools.Env{Home: h, Config: cfg, Secrets: config.NewSecrets(h), Cwd: cwd, Log: slog.Default()}
}

// MCP configuration, registration and naming the plugin activation path reaches through the wiring
type pluginConnector struct{}

// Plugin facing MCP connector service cmd/icebath installs as plugins.Services.MCP
func PluginConnector() plugins.MCPConnector { return pluginConnector{} }

// Raw server configs of the active home config, the suspicious ones already dropped
func (pluginConnector) LoadConfig(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	for name, r := range LoadServerConfigs(ctx, pluginEnv(ctx)) {
		out[name] = r.Config
	}
	return out, nil
}

func (pluginConnector) FilterSuspicious(servers map[string]any) map[string]any {
	return filterSuspiciousServers(servers)
}

// Connects the servers and registers their tools, every MCP tool name of the scope returned
func (pluginConnector) RegisterServers(ctx context.Context, servers map[string]any) ([]string, error) {
	env := pluginEnv(ctx)
	rendered := map[string]*Rendered{}
	for _, name := range orderedKeys(servers) {
		cfg, ok := servers[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("MCP server %q configuration is not a mapping", name)
		}
		r, err := renderServerConfig(ctx, env, cfg)
		if err != nil {
			return nil, err
		}
		rendered[name] = r
	}
	return Connect(ctx, env, rendered)
}

// The server's callable names once its registration finished, waking a parked server once
func (pluginConnector) RegisteredToolNames(ctx context.Context, server string) ([]string, error) {
	return connRegisteredToolNames(ctx, server, 30*time.Second), nil
}

// Resource, prompt and event wrapper kinds every server gets, sorted
func (pluginConnector) UtilityKinds() []string {
	kinds := make([]string, 0, len(utilityHandlerFactories))
	for kind := range utilityHandlerFactories {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// Per server status rows of the active home, read from cached runtime state only
func (pluginConnector) Status(ctx context.Context) ([]map[string]any, error) {
	rows := Status(ctx, pluginEnv(ctx), nil, true)
	raw, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out, nil
}

func (pluginConnector) SanitizeNameComponent(value string) string {
	return SanitizeNameComponent(value)
}

func (pluginConnector) ToolNamePrefix() string { return ToolNamePrefix }

// Calls one tool of a server through the registered handler, the same trust gates and breaker as the agent
func CallToolForPlugin(ctx context.Context, server, tool string, timeout float64, args map[string]any) (string, error) {
	env := pluginEnv(ctx)
	handler := makeToolHandler(server, tool, timeout, false)
	copied := map[string]any{}
	for k, v := range args {
		copied[k] = v
	}
	raw, err := json.Marshal(copied)
	if err != nil {
		return "", err
	}
	return handler(ctx, &tools.Call{Name: PrefixedToolName(server, tool), Args: copied, RawArgs: string(raw), Env: env}), nil
}

// Platform declaration of a portable server declaration, the app name labelling the parse
func portableDeclaration(decl plugins.AgentPluginServerDeclaration) (*tools.PlatformDeclaration, error) {
	var app, requires any
	if decl.App != nil {
		app = decl.App
	}
	if decl.Requires != nil {
		requires = decl.Requires
	}
	return tools.ParsePlatformDeclaration(decl.Name, app, requires, "plugin.json extension server "+pyrepr.Repr(decl.Name))
}

// Install time availability verdict of a portable server declaration
func PortableServerAvailability(ctx context.Context, decl plugins.AgentPluginServerDeclaration) (plugins.PortableAvailability, error) {
	parsed, err := portableDeclaration(decl)
	if err != nil {
		return plugins.PortableAvailability{}, err
	}
	a := tools.PlatformAvailabilityOf(parsed, "")
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	return plugins.PortableAvailability{State: a.State, Version: deref(a.Version), Path: deref(a.Path)}, nil
}

// Why a declared server cannot be offered, offerable true with an empty reason when its app is available
func ServerAvailabilityReason(ctx context.Context, declaration plugins.AgentPluginServerDeclaration, appTitle string) (string, bool, error) {
	parsed, err := portableDeclaration(declaration)
	if err != nil {
		return "", false, err
	}
	a := tools.PlatformAvailabilityOf(parsed, "")
	if a.Offerable() {
		return "", true, nil
	}
	decl := (toolsPlatform{}).declarationOf(parsed)
	available := (toolsPlatform{}).Availability(decl)
	problem := availabilityProblem(available)
	if problem == "" {
		return fmt.Sprintf("its app is unavailable (%s).", available.State), false, nil
	}
	return describeUnavailable(decl, available, problem, appTitle), false, nil
}
