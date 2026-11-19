package app

import (
	"fmt"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Plugin toolsets of one gateway life
type gatewayToolsetCatalog struct {
	connection *gatewaycli.GatewayConnection
	epoch      int
	toolsets   []tools.ConfigurableToolset
	portable   map[string]bool
}

// Plugin toolsets and portable MCP servers the gateway's plugins provide, false off the gateway
func gatewayPluginToolsets(c *CLI) ([]tools.ConfigurableToolset, map[string]bool, bool) {
	connection := gatewayConnectionFor(c)
	if connection == nil {
		return nil, nil, false
	}
	epoch := connection.Epoch()
	c.uiMu.Lock()
	cached := c.gatewayPluginToolsetCache
	c.uiMu.Unlock()
	if cached != nil && cached.connection == connection && cached.epoch == epoch && !connection.Closed() {
		return cached.toolsets, cached.portable, true
	}
	raw, err := connection.Call(c.ctx, "plugins.list", map[string]any{})
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		pkgLog().Debug(fmt.Sprintf("plugins.list unavailable: %s", err.Error()))
		return []tools.ConfigurableToolset{}, map[string]bool{}, true
	}
	reply, _ := pyjson.Plain(raw).(map[string]any)
	var rows []tools.ConfigurableToolset
	listed, _ := reply["toolsets"].([]any)
	for _, rawRow := range listed {
		row, _ := rawRow.(map[string]any)
		key, isStr := row["key"].(string)
		if !isStr {
			continue
		}
		label := pyrepr.Str(row["key"])
		if pyrepr.Truthy(row["label"]) {
			label = pyrepr.Str(row["label"])
		}
		rows = append(rows, tools.ConfigurableToolset{Key: key, Label: label, Description: strOrEmpty(row["description"])})
	}
	portable := map[string]bool{}
	servers, _ := reply["portable_mcp_servers"].([]any)
	for _, name := range servers {
		if s, isStr := name.(string); isStr {
			portable[s] = true
		}
	}
	c.uiMu.Lock()
	c.gatewayPluginToolsetCache = &gatewayToolsetCatalog{connection: connection, epoch: epoch, toolsets: rows, portable: portable}
	c.uiMu.Unlock()
	return rows, portable, true
}
