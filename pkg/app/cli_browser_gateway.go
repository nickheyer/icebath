package app

import (
	"net/url"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Whether this CLI attaches to an explicitly named gateway WebSocket that is not loopback
func gatewayIsRemote() bool {
	raw := strings.TrimSpace(os.Getenv("ICEBATH_TUI_GATEWAY_URL"))
	if raw == "" {
		return false
	}
	parsed, err := url.Parse(raw)
	host := ""
	if err == nil {
		host = strings.ToLower(parsed.Hostname())
	}
	return host != "127.0.0.1" && host != "localhost" && host != "::1"
}

// Runs browser connect, disconnect or status on the shared gateway where the browser tools run
func (c *CLI) gatewayBrowser(connection *gatewaycli.GatewayConnection, word, rest string) {
	if (word == "connect" || word == "disconnect") && gatewayIsRemote() {
		sayBlock(c.commandsT("browser.remote_gateway", nil))
		return
	}
	params := map[string]any{"action": word, "session_id": c.sessionID}
	if word == "connect" {
		target := rest
		if target == "" {
			target = browser.DefaultBrowserCDPURL
		}
		normalized, _, ok := c.normalizeCDPURL(target)
		if !ok {
			return
		}
		params["url"] = normalized
		infoPrint("   " + c.commandsT("browser.checking", map[string]any{"url": normalized}))
	}
	raw, err := connection.Call(c.ctx, "browser.manage", params)
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		sayBlock("   ⚠ " + err.Error())
		return
	}
	result, _ := pyjson.Plain(raw).(map[string]any)
	messages, _ := result["messages"].([]any)
	for _, message := range messages {
		infoPrint("   " + pyrepr.Str(message))
	}
	endpoint := "   " + c.commandsT("browser.endpoint", map[string]any{"url": strOrEmpty(result["url"])})
	switch {
	case word == "disconnect":
		sayBlock(c.commandsT("browser.disconnected", nil), "   "+c.commandsT("browser.reverted", nil))
	case word == "connect" && !pyrepr.Truthy(result["connected"]):
		sayBlock(c.commandsT("browser.not_connected", nil))
	case word == "connect":
		sayBlock(c.commandsT("browser.connected", nil), endpoint)
	case pyrepr.Truthy(result["connected"]):
		sayBlock(c.commandsT("browser.status_connected", nil), endpoint)
	default:
		sayBlock(c.commandsT("browser.status_gateway_default", nil), "   "+c.commandsT("browser.status_connect_hint", nil),
			"   "+c.commandsT("browser.status_disconnect_hint", nil))
	}
}
