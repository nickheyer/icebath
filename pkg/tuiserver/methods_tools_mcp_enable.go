package tuiserver

import (
	"context"
	"fmt"
	"maps"

	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var toolsMCPEnableRegistry = &handlerRegistry{}

func init() {
	mcpRPC(toolsMCPEnableRegistry, "enable", []requiredParam{{"name", strippedPresent}, {"enabled", func(v any) bool { _, isBool := v.(bool); return isBool }}}, mcpServersEnable)
	toolsMCPEnableRegistry.install()
}

// Sets the enabled flag of a config server, the next session or MCP reload loading or dropping it
func mcpServersEnable(ctx context.Context, rid any, params map[string]any, _ *tuihost.Session) (map[string]any, error) {
	name, servers, errResp := mcpConfigServerOrError(ctx, rid, params)
	if errResp != nil {
		return errResp, nil
	}
	serverConfig := maps.Clone(servers[name])
	serverConfig["enabled"] = params["enabled"].(bool)
	saved, err := mcp.SaveServer(ctx, mcp.HeadlessUI(), name, serverConfig)
	if err != nil {
		return nil, err
	}
	if !saved {
		return rpcErr(rid, 4001, fmt.Sprintf("server '%s' rejected: suspicious command/args configuration", name), nil), nil
	}
	return ok(rid, map[string]any{"ok": true, "name": name, "server": summarizeMCPServer(ctx, name, serverConfig, nil)}), nil
}
