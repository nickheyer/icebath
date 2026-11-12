package app

import (
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Runs fn, a panic in the optional subsystem leaving the default result as the Python probe does
func probeCall(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// Prints the browser.engine lightpanda lines of /browser status, silent unless that engine is set
func (c *CLI) printLightpandaEngineStatus() {
	using := false
	probeCall(func() { using = browser.UsingLightpandaEngine(c.env) })
	if !using {
		return
	}
	used, reason := false, ""
	probeCall(func() { used, reason = browser.LightpandaEngineStatus(c.env) })
	if reason == "" {
		return
	}
	if !used {
		printLines("   " + c.commandsT("browser.engine_not_in_use", map[string]any{"reason": reason}))
		return
	}
	printLines("   " + c.commandsT("browser.engine_lightpanda", map[string]any{"reason": reason}))
	bin, ok := "", false
	probeCall(func() { bin, ok = browser.FindLightpandaBinary(c.env), true })
	if !ok {
		return
	}
	if bin != "" {
		printLines("   " + c.commandsT("browser.binary", map[string]any{"path": bin}))
	} else {
		printLines("   " + c.commandsT("browser.binary_not_found", map[string]any{"hint": browser.LightpandaInstallHint}))
	}
}

// Launches a local debug browser for the default CDP URL and returns its discovered URL, empty after printing why not
func (c *CLI) launchDefaultCDPBrowser(port int) string {
	launchPort := port
	if browser.LocalPortInUse(port, 500*time.Millisecond) {
		launchPort = browser.FindFreeDebugPort(port, 10)
		printLines("   "+c.commandsT("browser.port_occupied", map[string]any{"port": port}),
			"     "+c.commandsT("browser.port_occupied_detail", map[string]any{"launch_port": launchPort}))
	} else {
		printLines("   " + c.commandsT("browser.launching", nil))
	}
	system := pyplatform.System()
	launch := browser.LaunchChromeDebug(c.ctx, launchPort, system)
	if !launch.Launched {
		printLines("   " + c.commandsT("browser.launch_failed", nil))
		if hint := launch.Hint(); hint != "" {
			printLines("     " + hint)
		}
		if chromeCmd := browser.ManualChromeDebugCommand(c.ctx, launchPort, system); chromeCmd != "" {
			printLines("     "+c.commandsT("browser.launch_manually", nil), "     "+chromeCmd)
		} else {
			printLines("     " + c.commandsT("browser.no_executable", nil))
		}
		return ""
	}
	for wait := 0; wait < 10; wait++ {
		if found := browser.DiscoverLocalCDPURL(c.ctx, launchPort, time.Second); found != "" {
			printLines("   " + c.commandsT("browser.launched", map[string]any{"port": launchPort}))
			return found
		}
		time.Sleep(500 * time.Millisecond)
	}
	printLines("   "+c.commandsT("browser.not_responding", map[string]any{"port": launchPort}), "     "+c.commandsT("browser.try_again", nil))
	return ""
}

// Validates a /browser connect URL into the URL and port, printing the error and returning false when invalid
func (c *CLI) normalizeCDPURL(cdpURL string) (string, int, bool) {
	raw := cdpURL
	if !strings.Contains(cdpURL, "://") {
		raw = "http://" + cdpURL
	}
	parsed, err := pyurl.Parse(raw)
	if err != nil {
		panic(err)
	}
	switch parsed.Scheme {
	case "http", "https", "ws", "wss":
	default:
		scheme := parsed.Scheme
		if scheme == "" {
			scheme = c.commandsT("browser.scheme_missing", nil)
		}
		sayBlock("   " + c.commandsT("browser.unsupported_scheme", map[string]any{"scheme": scheme}))
		return "", 0, false
	}
	port, has, perr := parsed.Port()
	if perr != nil {
		sayBlock("   " + c.commandsT("browser.invalid_port", map[string]any{"url": cdpURL}))
		return "", 0, false
	}
	if !has || port == 0 {
		port = 80
		if parsed.Scheme == "https" || parsed.Scheme == "wss" {
			port = 443
		}
	}
	if parsed.Hostname() == "" {
		sayBlock("   " + c.commandsT("browser.missing_host", map[string]any{"url": cdpURL}))
		return "", 0, false
	}
	if strings.HasPrefix(parsed.Path, "/devtools/browser/") {
		return pyurl.Unparse(parsed), port, true
	}
	parsed.Path, parsed.Params, parsed.Query, parsed.Fragment = "", "", "", ""
	return pyurl.Unparse(parsed), port, true
}

// Handles /browser connect, finding or launching a debug browser and pointing the browser tools at it
func (c *CLI) browserConnect(cdpURL string) {
	cdpURL, port, ok := c.normalizeCDPURL(cdpURL)
	if !ok {
		return
	}
	probeCall(func() { browser.CleanupAllBrowsers(c.ctx, c.env) })
	printLines("")
	isDefault := cdpURL == browser.DefaultBrowserCDPURL
	found := ""
	if isDefault {
		found = browser.DiscoverLocalCDPURL(c.ctx, port, time.Second)
	} else if browser.IsBrowserDebugReady(c.ctx, cdpURL, time.Second) {
		found = cdpURL
	}
	switch {
	case found != "":
		printLines("   " + c.commandsT("browser.already_listening", map[string]any{"url": found}))
	case isDefault:
		found = c.launchDefaultCDPBrowser(port)
	default:
		printLines("   " + c.commandsT("browser.port_unreachable", map[string]any{"port": port, "url": cdpURL}))
	}
	if found == "" {
		sayBlock(c.commandsT("browser.not_connected", nil))
		return
	}
	os.Setenv("BROWSER_CDP_URL", found)
	probeCall(func() { browser.EnsureCDPSupervisor(c.env, "default") })
	sayBlock(c.commandsT("browser.connected", nil), "   "+c.commandsT("browser.endpoint", map[string]any{"url": found}))
	if c.pendingInput != nil {
		c.pendingInput.Put("[System note: The user invoked /browser connect and connected your browser tools to " +
			"a Chromium-family dev/debug browser via Chrome DevTools Protocol. " +
			"Your browser tools (browser_exec, browser_cdp) now " +
			"control that CDP browser. The command itself is a signal that using browser tools for " +
			"their current browser-related request is expected; do not wait for separate permission " +
			"just because CDP is connected. This is typically a Icebath-managed isolated debug " +
			"profile, not the user's main everyday browser. It is still user-visible and may contain " +
			"pages, logged-in sessions, or cookies in that debug profile, so avoid destructive actions, " +
			"closing tabs, or navigating away unless the user's task calls for it.]")
	}
}

// Handles /browser disconnect, reverting the browser tools to the default backend
func (c *CLI) browserDisconnect() {
	if pystr.Strip(os.Getenv("BROWSER_CDP_URL")) == "" {
		sayBlock(c.commandsT("browser.already_default", nil))
		return
	}
	os.Unsetenv("BROWSER_CDP_URL")
	probeCall(func() {
		browser.StopCDPSupervisor(c.env, "default")
		browser.CleanupAllBrowsers(c.ctx, c.env)
	})
	sayBlock(c.commandsT("browser.disconnected", nil), "   "+c.commandsT("browser.reverted", nil))
	if c.pendingInput != nil {
		c.pendingInput.Put("[System note: The user has disconnected the browser tools from their live Chromium-family browser. " +
			"Browser tools are back to default mode (headless local browser or cloud provider).]")
	}
}

// Headline and indented detail lines of /browser status for a local browser.engine value
func (c *CLI) localEngineLines(engine string) []string {
	keys := []string{"status_auto"}
	if engine == "lightpanda" {
		keys = []string{"status_lightpanda", "status_lightpanda_note"}
	}
	out := []string{c.commandsT("browser."+keys[0], nil)}
	for _, key := range keys[1:] {
		out = append(out, "   "+c.commandsT("browser."+key, nil))
	}
	return out
}

// Port of the last host colon of a CDP URL, 9222 when it does not parse
func cdpStatusPort(current string) int {
	tail := current[strings.LastIndex(current, ":")+1:]
	if i := strings.Index(tail, "/"); i >= 0 {
		tail = tail[:i]
	}
	if p, err := strconv.Atoi(pystr.Strip(tail)); err == nil {
		return p
	}
	return 9222
}

// Handles /browser status, the CDP endpoint and its reachability or the cloud or local engine
func (c *CLI) browserStatus() {
	current := pystr.Strip(os.Getenv("BROWSER_CDP_URL"))
	printLines("")
	if current != "" {
		printLines(c.commandsT("browser.status_connected", nil), "   "+c.commandsT("browser.endpoint", map[string]any{"url": current}))
		c.printLightpandaEngineStatus()
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cdpStatusPort(current))), time.Second)
		if err == nil {
			_ = conn.Close()
			printLines("   " + c.commandsT("browser.status_reachable", nil))
		} else {
			printLines("   " + c.commandsT("browser.status_unreachable", nil))
		}
	} else {
		var cloud browser.BrowserProvider
		probeCall(func() {
			if p, err := browser.GetCloudProvider(c.env); err == nil {
				cloud = p
			}
		})
		if cloud != nil {
			printLines(c.commandsT("browser.status_cloud", map[string]any{"provider": cloud.DisplayName()}))
			c.printLightpandaEngineStatus()
		} else {
			engine := "auto"
			probeCall(func() { engine = browser.GetBrowserEngine(c.env) })
			printLines(c.localEngineLines(engine)...)
			if engine == "lightpanda" {
				c.printLightpandaEngineStatus()
			}
		}
	}
	sayBlock("   "+c.commandsT("browser.status_connect_hint", nil), "   "+c.commandsT("browser.status_disconnect_hint", nil))
}

// Handles /browser connect, disconnect or status, the bare command reading as status
func (c *CLI) handleBrowserCommand(cmd string) {
	parts := pystr.SplitWhitespace(commandArg(cmd, false), 1)
	word := "status"
	if len(parts) > 0 {
		word = pystr.Lower(parts[0])
	}
	rest := ""
	if len(parts) > 1 {
		rest = pystr.Strip(parts[1])
	}
	if word == "connect" || word == "disconnect" || word == "status" {
		if connection := gatewayConnectionFor(c); connection != nil {
			c.gatewayBrowser(connection, word, rest)
			return
		}
	}
	switch word {
	case "connect":
		if rest == "" {
			rest = browser.DefaultBrowserCDPURL
		}
		c.browserConnect(rest)
	case "disconnect":
		c.browserDisconnect()
	case "status":
		c.browserStatus()
	default:
		lines := []string{c.commandsT("browser.usage", nil), ""}
		for _, sub := range []string{"connect", "disconnect", "status"} {
			lines = append(lines, "   "+c.commandsT("browser.usage_"+sub, nil))
		}
		sayBlock(lines...)
	}
}
