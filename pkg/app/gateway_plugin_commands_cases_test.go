package app

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Scripted gateway whose catalog lists the given plugin commands and whose other verbs answer one reply
type pluginGateway struct {
	t            *testing.T
	client       *scriptedClient
	conn         *gatewaycli.GatewayConnection
	mu           sync.Mutex
	plugins      map[string]string
	reply        any
	catalogError error
	extra        map[string]any
	afterCatalog func()
	olderGateway bool
}

func (g *pluginGateway) handle(method string, params map[string]any) (any, bool, error) {
	g.client.record(method, params)
	g.mu.Lock()
	defer g.mu.Unlock()
	if method == "commands.catalog" {
		if g.olderGateway && len(params) > 0 {
			code := int64(-32602)
			return nil, true, &gateway.GatewayRPCError{Msg: "invalid_params", Code: &code}
		}
		if g.catalogError != nil {
			return nil, true, g.catalogError
		}
		names := make([]string, 0, len(g.plugins))
		for name := range g.plugins {
			names = append(names, name)
		}
		sort.Strings(names)
		rows := []any{}
		for _, name := range names {
			rows = append(rows, []any{"/" + name, g.plugins[name]})
		}
		reply := map[string]any{"pairs": append([]any{[]any{"/help", "Show help"}}, rows...),
			"categories": []any{map[string]any{"name": "Session", "pairs": []any{[]any{"/help", "Show help"}}},
				map[string]any{"name": "Plugin commands", "pairs": rows}}}
		for k, v := range g.extra {
			reply[k] = v
		}
		hook := g.afterCatalog
		g.afterCatalog = nil
		if hook != nil {
			g.mu.Unlock()
			hook()
			g.mu.Lock()
		}
		return reply, true, nil
	}
	if refusal, isErr := g.reply.(error); isErr {
		return nil, true, refusal
	}
	return g.reply, true, nil
}

func (g *pluginGateway) set(change func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	change()
}

func (g *pluginGateway) catalogReads() int { return len(g.client.callsOf("commands.catalog")) }

// Commands of every slash.exec the gateway was sent
func (g *pluginGateway) execs() []string {
	out := []string{}
	for _, params := range g.client.callsOf("slash.exec") {
		out = append(out, params["command"].(string))
	}
	return out
}

// Ends the socket the way a gateway hang up does, the connection staying closed until its next call redials
func (g *pluginGateway) hangUp() {
	g.t.Helper()
	g.client.events.PutNowait(&gateway.GatewayClientError{Msg: "Gateway disconnected"})
	waitUntil(g.t, g.conn.Closed)
}

// Moves the connection to another gateway life with no call of the CLI in between
func (g *pluginGateway) redial() {
	g.t.Helper()
	g.hangUp()
	if err := g.conn.Start(context.Background(), 5*time.Second); err != nil {
		g.t.Fatalf("redial: %v", err)
	}
}

// Records every local plugin discovery of this process through its completion log line
type discoveryLog struct {
	slog.Handler
	mu    sync.Mutex
	reads []string
}

func (d *discoveryLog) Enabled(context.Context, slog.Level) bool { return true }

func (d *discoveryLog) Handle(_ context.Context, record slog.Record) error {
	if strings.HasPrefix(record.Message, "Plugin discovery complete") {
		d.mu.Lock()
		d.reads = append(d.reads, record.Message)
		d.mu.Unlock()
	}
	return nil
}

func (d *discoveryLog) WithAttrs([]slog.Attr) slog.Handler { return d }
func (d *discoveryLog) WithGroup(string) slog.Handler      { return d }

func (d *discoveryLog) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.reads...)
}

// Watches for plugin code being discovered in this process, starting from undiscovered managers
func localPluginReads(t *testing.T) *discoveryLog {
	t.Helper()
	plugins.ResetPluginManagersForTests()
	watch := &discoveryLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(watch))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		plugins.ResetPluginManagersForTests()
	})
	return watch
}

// CLI on an open gateway session of a scripted plugin gateway, with no local skills or bundles
func pluginCLI(t *testing.T, pluginRows map[string]string, reply any) (*CLI, *pluginGateway, *printRecorder) {
	t.Helper()
	rec := recordPrints(t)
	if reply == nil {
		reply = map[string]any{"type": "exec", "output": "mcp says hi"}
	}
	gw := &pluginGateway{t: t, client: &scriptedClient{}, plugins: pluginRows, reply: reply}
	gw.client.override = gw.handle
	gw.conn = scriptedConnection(t, gw.client)
	previousBundles := slashSkillBundles
	slashSkillBundles = func(context.Context) skills.SkillCommands { return skills.SkillCommands{} }
	t.Cleanup(func() { slashSkillBundles = previousBundles })
	c := bareCLI()
	c.config = map[string]any{}
	c.pendingInput = &inputQueue{}
	c.sessionID, c.gatewaySessionOpen = "local-s", true
	c.sessionDB = gatewaycli.NewGatewaySessionStore(gw.conn)
	t.Cleanup(func() { _ = c.sessionDB.CloseReader() })
	return c, gw, rec
}

// Replaces prefix expansion with a recorder that answers true
func recordExpansions(t *testing.T) *[]string {
	t.Helper()
	var expanded []string
	previous := slashExpandPrefix
	slashExpandPrefix = func(_ *CLI, cmdOriginal, _ string, _, _ skills.SkillCommands) bool {
		expanded = append(expanded, cmdOriginal)
		return true
	}
	t.Cleanup(func() { slashExpandPrefix = previous })
	return &expanded
}

// Replaces the skill dispatch with a recorder of the commands it was asked to run
func recordSkillRuns(t *testing.T) *[]string {
	t.Helper()
	var ran []string
	previous := slashRunSkill
	slashRunSkill = func(_ *CLI, baseCmd string, _ map[string]any, _ string) { ran = append(ran, baseCmd) }
	t.Cleanup(func() { slashRunSkill = previous })
	return &ran
}

// A bundle named like the plugin command and recorders on bundle, skill and prefix dispatch
func collidingSkillAndBundle(t *testing.T, name string) *[]string {
	t.Helper()
	var ran []string
	prevBundles, prevBundle, prevSkill, prevExpand := slashSkillBundles, slashRunSkillBundle, slashRunSkill, slashExpandPrefix
	slashSkillBundles = func(context.Context) skills.SkillCommands {
		return skills.SkillCommands{"/" + name: {"skills": []any{}}}
	}
	slashRunSkillBundle = func(*CLI, string, map[string]any, string) { ran = append(ran, "_run_skill_bundle_command") }
	slashRunSkill = func(*CLI, string, map[string]any, string) { ran = append(ran, "_run_skill_slash_command") }
	slashExpandPrefix = func(*CLI, string, string, skills.SkillCommands, skills.SkillCommands) bool {
		ran = append(ran, "_expand_slash_prefix")
		return true
	}
	t.Cleanup(func() {
		slashSkillBundles, slashRunSkillBundle, slashRunSkill, slashExpandPrefix = prevBundles, prevBundle, prevSkill, prevExpand
	})
	return &ran
}

func printedText(rec *printRecorder) string { return strings.Join(rec.plain(), "\n") }

// Names of the gateway's plugin commands as completion reads them
func pluginNames(t *testing.T, c *CLI) map[string]string {
	t.Helper()
	names, onGateway := gatewayPluginNames(c)
	if !onGateway {
		t.Fatal("the CLI is not on a gateway session")
	}
	return names
}

func nameSet(names map[string]string) []string {
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func TestPluginCommandRunsInTheGateway(t *testing.T) {
	reads := localPluginReads(t)
	c, gw, rec := pluginCLI(t, map[string]string{"mcp-status": "Ask the MCP server"}, nil)
	if !c.processUnregisteredSlash("/mcp-status Deep Check", "/mcp-status deep check") {
		t.Fatal("dispatch answered false")
	}
	sent := false
	for _, params := range gw.client.callsOf("slash.exec") {
		sent = sent || sameJSON(params, map[string]any{"session_id": "local-s", "command": "mcp-status Deep Check"})
	}
	if !sent {
		t.Fatalf("slash.exec calls %v", gw.client.callsOf("slash.exec"))
	}
	if !strings.Contains(printedText(rec), "mcp says hi") {
		t.Fatalf("output %q", printedText(rec))
	}
	if got := reads.seen(); len(got) != 0 {
		t.Fatalf("local plugin reads %v", got)
	}
}

func TestGatewayRefusalIsPrintedNotRunLocally(t *testing.T) {
	reads := localPluginReads(t)
	c, _, rec := pluginCLI(t, map[string]string{"mcp-status": "Ask the MCP server"}, &gateway.GatewayClientError{Msg: "permission_denied"})
	if !c.processUnregisteredSlash("/mcp-status", "/mcp-status") {
		t.Fatal("dispatch answered false")
	}
	if !strings.Contains(printedText(rec), "permission_denied") || len(reads.seen()) != 0 {
		t.Fatalf("output %q reads %v", printedText(rec), reads.seen())
	}
}

func TestNonPluginCommandFallsThroughWithoutLoadingPlugins(t *testing.T) {
	reads := localPluginReads(t)
	c, gw, _ := pluginCLI(t, map[string]string{"mcp-status": "Ask the MCP server"}, nil)
	expanded := recordExpansions(t)
	if !c.processUnregisteredSlash("/zzz", "/zzz") {
		t.Fatal("dispatch answered false")
	}
	if len(*expanded) == 0 || len(gw.execs()) != 0 || len(reads.seen()) != 0 {
		t.Fatalf("expanded %v execs %v reads %v", *expanded, gw.execs(), reads.seen())
	}
}

func TestKnownPluginCommandsReuseTheSessionsCatalog(t *testing.T) {
	localPluginReads(t)
	c, gw, _ := pluginCLI(t, map[string]string{"mcp-status": "Ask the MCP server"}, nil)
	for range 3 {
		c.processUnregisteredSlash("/mcp-status", "/mcp-status")
	}
	if gw.catalogReads() != 1 || !reflect.DeepEqual(gw.execs(), []string{"mcp-status", "mcp-status", "mcp-status"}) {
		t.Fatalf("catalog reads %d execs %v", gw.catalogReads(), gw.execs())
	}
}

func TestAPluginTheGatewayGainedDispatchesAfterOneMissRefresh(t *testing.T) {
	watch := localPluginReads(t)
	c, gw, _ := pluginCLI(t, map[string]string{}, nil)
	expanded := recordExpansions(t)
	c.processUnregisteredSlash("/zzz", "/zzz")
	reads := gw.catalogReads()
	gw.set(func() { gw.plugins["late-plugin"] = "Enabled after the CLI opened" })
	if !c.processUnregisteredSlash("/late-plugin now", "/late-plugin now") {
		t.Fatal("dispatch answered false")
	}
	if !reflect.DeepEqual(gw.execs(), []string{"late-plugin now"}) || gw.catalogReads() != reads+1 {
		t.Fatalf("execs %v catalog reads %d after %d", gw.execs(), gw.catalogReads(), reads)
	}
	if len(*expanded) != 1 || len(watch.seen()) != 0 {
		t.Fatalf("expanded %v local reads %v", *expanded, watch.seen())
	}
}

func TestAnUnknownCommandRefreshesTheCatalogOnceThenFallsThrough(t *testing.T) {
	c, gw, _ := pluginCLI(t, map[string]string{"mcp-status": "Ask the MCP server"}, nil)
	expanded := recordExpansions(t)
	c.processUnregisteredSlash("/mcp-status", "/mcp-status")
	for submitted := 1; submitted < 4; submitted++ {
		c.processUnregisteredSlash("/zzz", "/zzz")
		if gw.catalogReads() != 1+submitted || len(*expanded) != submitted {
			t.Fatalf("after %d: catalog reads %d expanded %d", submitted, gw.catalogReads(), len(*expanded))
		}
	}
	if !reflect.DeepEqual(gw.execs(), []string{"mcp-status"}) {
		t.Fatalf("execs %v", gw.execs())
	}
}

func TestASessionBoundaryRereadsTheCatalog(t *testing.T) {
	for _, boundary := range []string{"new_session", "reconnect"} {
		t.Run(boundary, func(t *testing.T) {
			c, gw, _ := pluginCLI(t, map[string]string{"old-plugin": "Before"}, nil)
			if got := nameSet(pluginNames(t, c)); !reflect.DeepEqual(got, []string{"old-plugin"}) {
				t.Fatalf("names %v", got)
			}
			gw.set(func() { gw.plugins = map[string]string{"new-plugin": "After a gateway restart"} })
			if got := nameSet(pluginNames(t, c)); !reflect.DeepEqual(got, []string{"old-plugin"}) {
				t.Fatalf("completion left the session's read: %v", got)
			}
			if boundary == "new_session" {
				c.sessionID = "local-s2"
			} else {
				gw.redial()
			}
			if got := nameSet(pluginNames(t, c)); !reflect.DeepEqual(got, []string{"new-plugin"}) {
				t.Fatalf("names after the boundary %v", got)
			}
			c.processUnregisteredSlash("/new-plugin", "/new-plugin")
			if !reflect.DeepEqual(gw.execs(), []string{"new-plugin"}) {
				t.Fatalf("execs %v", gw.execs())
			}
		})
	}
}

func TestAFailedCatalogReadRunsNothingElse(t *testing.T) {
	reads := localPluginReads(t)
	c, gw, rec := pluginCLI(t, map[string]string{}, nil)
	gw.set(func() { gw.catalogError = &gateway.GatewayClientError{Msg: "gateway restarting"} })
	ran := collidingSkillAndBundle(t, "mcp-status")
	if !c.processUnregisteredSlash("/mcp-status", "/mcp-status") {
		t.Fatal("dispatch answered false")
	}
	if !strings.Contains(printedText(rec), "gateway restarting") {
		t.Fatalf("output %q", printedText(rec))
	}
	if len(*ran) != 0 || len(gw.execs()) != 0 || len(reads.seen()) != 0 {
		t.Fatalf("ran %v execs %v reads %v", *ran, gw.execs(), reads.seen())
	}
}

func TestACatalogWhosePluginDiscoveryFailedRunsNothingElse(t *testing.T) {
	reads := localPluginReads(t)
	c, gw, rec := pluginCLI(t, map[string]string{}, nil)
	gw.set(func() {
		gw.extra = map[string]any{"plugin_discovery_error": "boom in register()", "warning": "plugin command discovery unavailable: boom in register()"}
	})
	ran := collidingSkillAndBundle(t, "mcp-status")
	if !c.processUnregisteredSlash("/mcp-status", "/mcp-status") {
		t.Fatal("dispatch answered false")
	}
	if !strings.Contains(printedText(rec), "boom in register()") {
		t.Fatalf("output %q", printedText(rec))
	}
	if len(*ran) != 0 || len(gw.execs()) != 0 || len(reads.seen()) != 0 {
		t.Fatalf("ran %v execs %v reads %v", *ran, gw.execs(), reads.seen())
	}
	printed := len(rec.plain())
	if got := pluginNames(t, c); len(got) != 0 {
		t.Fatalf("names %v", got)
	}
	if len(rec.plain()) != printed {
		t.Fatalf("completion printed %q", rec.plain()[printed:])
	}
	gw.set(func() { gw.extra, gw.plugins = nil, map[string]string{"mcp-status": "Ask the MCP server"} })
	if !c.processUnregisteredSlash("/mcp-status", "/mcp-status") {
		t.Fatal("dispatch answered false")
	}
	if !reflect.DeepEqual(gw.execs(), []string{"mcp-status"}) || len(*ran) != 0 {
		t.Fatalf("execs %v ran %v", gw.execs(), *ran)
	}
}

func TestAnOlderGatewayWithoutTheDiscoveryFieldKeepsTodaysBehaviour(t *testing.T) {
	c, gw, _ := pluginCLI(t, map[string]string{}, nil)
	gw.set(func() { gw.extra = map[string]any{"warning": "skill discovery unavailable: x"} })
	ran := collidingSkillAndBundle(t, "probe")
	if !c.processUnregisteredSlash("/probe", "/probe") {
		t.Fatal("dispatch answered false")
	}
	if !reflect.DeepEqual(*ran, []string{"_run_skill_bundle_command"}) {
		t.Fatalf("ran %v", *ran)
	}
}

func TestAPluginCommandADisplayEntryHidesStillDispatches(t *testing.T) {
	reads := localPluginReads(t)
	c, gw, _ := pluginCLI(t, map[string]string{}, nil)
	gw.set(func() { gw.extra = map[string]any{"plugin_commands": map[string]any{"logs": "Plugin logs"}} })
	ran := collidingSkillAndBundle(t, "logs")
	if !c.processUnregisteredSlash("/logs", "/logs") {
		t.Fatal("dispatch answered false")
	}
	if !reflect.DeepEqual(gw.execs(), []string{"logs"}) || len(*ran) != 0 || len(reads.seen()) != 0 {
		t.Fatalf("execs %v ran %v reads %v", gw.execs(), *ran, reads.seen())
	}
	if got := pluginNames(t, c); !reflect.DeepEqual(got, map[string]string{"logs": "Plugin logs"}) {
		t.Fatalf("names %v", got)
	}
}

func TestAHungUpConnectionRereadsTheCatalogBeforeACachedNameRuns(t *testing.T) {
	reads := localPluginReads(t)
	c, gw, _ := pluginCLI(t, map[string]string{"gone-plugin": "Before the restart"}, nil)
	c.processUnregisteredSlash("/gone-plugin", "/gone-plugin")
	gw.hangUp()
	gw.set(func() { gw.plugins = map[string]string{} })
	ran := collidingSkillAndBundle(t, "gone-plugin")
	if !c.processUnregisteredSlash("/gone-plugin", "/gone-plugin") {
		t.Fatal("dispatch answered false")
	}
	if !reflect.DeepEqual(gw.execs(), []string{"gone-plugin"}) || gw.catalogReads() != 2 {
		t.Fatalf("execs %v catalog reads %d", gw.execs(), gw.catalogReads())
	}
	if !reflect.DeepEqual(*ran, []string{"_run_skill_bundle_command"}) || len(reads.seen()) != 0 {
		t.Fatalf("ran %v reads %v", *ran, reads.seen())
	}
}

func skillKeys(t *testing.T, c *CLI) []string {
	t.Helper()
	found, onGateway := gatewaySessionSkills(c)
	if !onGateway {
		t.Fatal("the CLI is not on a gateway session")
	}
	out := make([]string, 0, len(found))
	for key := range found {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func TestAHungUpConnectionRereadsTheSessionsSkills(t *testing.T) {
	c, gw, _ := pluginCLI(t, map[string]string{}, nil)
	gw.set(func() { gw.extra = map[string]any{"skills": map[string]any{"/old-skill": map[string]any{}}} })
	if got := skillKeys(t, c); !reflect.DeepEqual(got, []string{"/old-skill"}) {
		t.Fatalf("skills %v", got)
	}
	gw.hangUp()
	gw.set(func() { gw.extra = map[string]any{"skills": map[string]any{"/new-skill": map[string]any{}}} })
	if got := skillKeys(t, c); !reflect.DeepEqual(got, []string{"/new-skill"}) {
		t.Fatalf("skills after the hang up %v", got)
	}
	if gw.catalogReads() != 2 {
		t.Fatalf("catalog reads %d", gw.catalogReads())
	}
}

func TestAHungUpConnectionRereadsThePluginToolsets(t *testing.T) {
	c, gw, _ := pluginCLI(t, map[string]string{}, map[string]any{"toolsets": []any{map[string]any{"key": "old-ts"}}})
	rows, portable, onGateway := gatewayPluginToolsets(c)
	if !onGateway || !reflect.DeepEqual(rows, []tools.ConfigurableToolset{{Key: "old-ts", Label: "old-ts"}}) || len(portable) != 0 {
		t.Fatalf("toolsets %v %v %v", rows, portable, onGateway)
	}
	gw.hangUp()
	gw.set(func() { gw.reply = map[string]any{"toolsets": []any{map[string]any{"key": "new-ts"}}} })
	rows, portable, _ = gatewayPluginToolsets(c)
	if !reflect.DeepEqual(rows, []tools.ConfigurableToolset{{Key: "new-ts", Label: "new-ts"}}) || len(portable) != 0 {
		t.Fatalf("toolsets after the hang up %v %v", rows, portable)
	}
	if got := len(gw.client.callsOf("plugins.list")); got != 2 {
		t.Fatalf("plugins.list calls %d", got)
	}
}

func TestAReplyThatIsNoPluginRunIsReported(t *testing.T) {
	c, _, rec := pluginCLI(t, map[string]string{"gone-plugin": "Before the restart"}, map[string]any{"type": "skill", "message": "skill body"})
	if !c.processUnregisteredSlash("/gone-plugin", "/gone-plugin") {
		t.Fatal("dispatch answered false")
	}
	out := printedText(rec)
	if !strings.Contains(out, "/gone-plugin") || !strings.Contains(out, "nothing was run") || strings.Contains(out, "skill body") {
		t.Fatalf("output %q", out)
	}
}

func TestACatalogServedBeforeARedialIsNotCachedAsTheNewLifes(t *testing.T) {
	c, gw, _ := pluginCLI(t, map[string]string{"old-plugin": "Served by the old gateway life"}, nil)
	gw.set(func() {
		gw.afterCatalog = func() {
			gw.redial()
			gw.set(func() { gw.plugins = map[string]string{"new-plugin": "Served by the new gateway life"} })
		}
	})
	if got := nameSet(pluginNames(t, c)); !reflect.DeepEqual(got, []string{"old-plugin"}) {
		t.Fatalf("names %v", got)
	}
	if got := nameSet(pluginNames(t, c)); !reflect.DeepEqual(got, []string{"new-plugin"}) {
		t.Fatalf("names of the new life %v", got)
	}
	if gw.catalogReads() != 2 {
		t.Fatalf("catalog reads %d", gw.catalogReads())
	}
	c.processUnregisteredSlash("/new-plugin", "/new-plugin")
	if !reflect.DeepEqual(gw.execs(), []string{"new-plugin"}) || gw.catalogReads() != 2 {
		t.Fatalf("execs %v catalog reads %d", gw.execs(), gw.catalogReads())
	}
}

func TestACatalogReadAcrossASessionChangeIsNotCachedForTheNewSession(t *testing.T) {
	c, gw, _ := pluginCLI(t, map[string]string{"old-plugin": "Before"}, nil)
	gw.set(func() {
		gw.afterCatalog = func() {
			c.sessionID = "local-s2"
			gw.set(func() { gw.plugins = map[string]string{"new-plugin": "After"} })
		}
	})
	pluginNames(t, c)
	if got := nameSet(pluginNames(t, c)); !reflect.DeepEqual(got, []string{"new-plugin"}) {
		t.Fatalf("names %v", got)
	}
	if gw.catalogReads() != 2 {
		t.Fatalf("catalog reads %d", gw.catalogReads())
	}
}

func TestSlashNewRereadsTheCatalog(t *testing.T) {
	recordPrints(t)
	gw := &pluginGateway{t: t, client: &scriptedClient{}, plugins: map[string]string{"old-plugin": "Before"}, reply: map[string]any{}}
	created := 0
	gw.client.override = func(method string, params map[string]any) (any, bool, error) {
		switch method {
		case "session.create":
			gw.client.record(method, params)
			created++
			return storeSnapshot(fmt.Sprintf("local-new-%d", created)), true, nil
		case "session.resume":
			gw.client.record(method, params)
			return storeSnapshot(params["session_id"].(string)), true, nil
		}
		return gw.handle(method, params)
	}
	gw.conn = scriptedConnection(t, gw.client)
	c := configuredCLI(t, map[string]any{}, CLIOptions{})
	c.sessionDB, c.gatewaySessionOpen, c.sessionID = gatewaycli.NewGatewaySessionStore(gw.conn), true, "local-s"
	t.Cleanup(func() { _ = c.sessionDB.CloseReader() })
	c.agent = nil
	if got := nameSet(pluginNames(t, c)); !reflect.DeepEqual(got, []string{"old-plugin"}) {
		t.Fatalf("names %v", got)
	}
	gw.set(func() { gw.plugins = map[string]string{"new-plugin": "After"} })
	before := c.sessionID
	c.newSession(true, "")
	if c.sessionID == before {
		t.Fatalf("session id stayed %q", before)
	}
	if got := nameSet(pluginNames(t, c)); !reflect.DeepEqual(got, []string{"new-plugin"}) {
		t.Fatalf("names after /new %v", got)
	}
	if gw.catalogReads() != 2 {
		t.Fatalf("catalog reads %d", gw.catalogReads())
	}
}

func TestAFailedRefreshOnAMissRunsNothingElse(t *testing.T) {
	c, gw, rec := pluginCLI(t, map[string]string{"mcp-status": "Ask the MCP server"}, nil)
	c.processUnregisteredSlash("/mcp-status", "/mcp-status")
	gw.set(func() { gw.catalogError = &gateway.GatewayClientError{Msg: "gateway restarting"} })
	ran := recordSkillRuns(t)
	if !c.processUnregisteredSlash("/probe-skill", "/probe-skill") {
		t.Fatal("dispatch answered false")
	}
	if !strings.Contains(printedText(rec), "gateway restarting") || len(*ran) != 0 {
		t.Fatalf("output %q ran %v", printedText(rec), *ran)
	}
}

func TestCompletionDegradesQuietlyWhenTheCatalogFails(t *testing.T) {
	c, gw, rec := pluginCLI(t, map[string]string{}, nil)
	gw.set(func() { gw.catalogError = &gateway.GatewayClientError{Msg: "gateway restarting"} })
	if got := pluginNames(t, c); len(got) != 0 {
		t.Fatalf("names %v", got)
	}
	if out := printedText(rec); out != "" {
		t.Fatalf("output %q", out)
	}
}

func TestCompletionListsTheGatewaysPluginCommands(t *testing.T) {
	reads := localPluginReads(t)
	completer := commands.NewCompleter(commands.CompleterSources{Registry: commands.Default,
		PluginCommands: func() map[string]string { return map[string]string{"mcp-status": "Ask the MCP server"} }})
	found := completer.Complete("/mcp")
	if !slices.ContainsFunc(found, func(c commands.Completion) bool { return strings.Contains(c.Text, "mcp-status") }) {
		t.Fatalf("completions %v", found)
	}
	if len(reads.seen()) != 0 {
		t.Fatalf("local plugin reads %v", reads.seen())
	}
}

// CLI that ran the command once and cached the catalog before a gateway reload dropped it
func unloadedSinceTheCachedRead(t *testing.T, name string, listedSkills ...string) (*CLI, *pluginGateway, *printRecorder) {
	t.Helper()
	c, gw, rec := pluginCLI(t, map[string]string{name: "Before the reload"}, nil)
	listed := map[string]any{}
	for _, skill := range listedSkills {
		listed["/"+skill] = map[string]any{}
	}
	gw.set(func() { gw.extra = map[string]any{"skills": listed} })
	c.processUnregisteredSlash("/"+name, "/"+name)
	gw.set(func() {
		gw.plugins, gw.reply = map[string]string{}, &gateway.GatewayRPCError{Msg: "unsupported_command"}
	})
	return c, gw, rec
}

func TestACommandAReloadUnloadedIsNamedNotTheRawRefusal(t *testing.T) {
	reads := localPluginReads(t)
	c, gw, rec := unloadedSinceTheCachedRead(t, "gone-plugin")
	before := len(rec.plain())
	if !c.processUnregisteredSlash("/gone-plugin", "/gone-plugin") {
		t.Fatal("dispatch answered false")
	}
	out := strings.Join(rec.plain()[before:], "\n")
	if !strings.Contains(out, "no longer has this plugin command") || strings.Contains(out, "unsupported_command") {
		t.Fatalf("output %q", out)
	}
	if !reflect.DeepEqual(gw.execs(), []string{"gone-plugin", "gone-plugin"}) || gw.catalogReads() != 2 {
		t.Fatalf("execs %v catalog reads %d", gw.execs(), gw.catalogReads())
	}
	expanded := recordExpansions(t)
	c.processUnregisteredSlash("/gone-plugin", "/gone-plugin")
	if !reflect.DeepEqual(gw.execs(), []string{"gone-plugin", "gone-plugin"}) || len(*expanded) != 1 || len(reads.seen()) != 0 {
		t.Fatalf("execs %v expanded %v reads %v", gw.execs(), *expanded, reads.seen())
	}
}

func TestASameNamedSkillRunsOnceAReloadUnloadedThePluginCommand(t *testing.T) {
	c, gw, rec := unloadedSinceTheCachedRead(t, "pptwin", "pptwin")
	ran := recordSkillRuns(t)
	before := len(rec.plain())
	for range 2 {
		if !c.processUnregisteredSlash("/pptwin", "/pptwin") {
			t.Fatal("dispatch answered false")
		}
	}
	out := strings.Join(rec.plain()[before:], "\n")
	if !reflect.DeepEqual(*ran, []string{"/pptwin", "/pptwin"}) || strings.Contains(out, "unsupported_command") {
		t.Fatalf("ran %v output %q", *ran, out)
	}
	if !reflect.DeepEqual(gw.execs(), []string{"pptwin", "pptwin"}) {
		t.Fatalf("execs %v", gw.execs())
	}
}

func TestASkillTheSameReloadBroughtRunsOnTheFirstTry(t *testing.T) {
	c, gw, rec := unloadedSinceTheCachedRead(t, "pptwin")
	gw.set(func() { gw.extra = map[string]any{"skills": map[string]any{"/pptwin": map[string]any{}}} })
	ran := recordSkillRuns(t)
	before := len(rec.plain())
	if !c.processUnregisteredSlash("/pptwin", "/pptwin") {
		t.Fatal("dispatch answered false")
	}
	out := strings.Join(rec.plain()[before:], "\n")
	if !reflect.DeepEqual(*ran, []string{"/pptwin"}) || strings.Contains(out, "nothing was run") {
		t.Fatalf("ran %v output %q", *ran, out)
	}
	if !reflect.DeepEqual(gw.execs(), []string{"pptwin", "pptwin"}) || gw.catalogReads() != 2 {
		t.Fatalf("execs %v catalog reads %d", gw.execs(), gw.catalogReads())
	}
}

func TestASkillReplyForAnUnloadedPluginCommandRunsTheSkillHere(t *testing.T) {
	c, gw, rec := unloadedSinceTheCachedRead(t, "pptwin", "pptwin")
	gw.set(func() { gw.reply = map[string]any{"type": "skill", "name": "pptwin", "message": "skill body"} })
	ran := recordSkillRuns(t)
	if !c.processUnregisteredSlash("/pptwin", "/pptwin") {
		t.Fatal("dispatch answered false")
	}
	if !reflect.DeepEqual(*ran, []string{"/pptwin"}) || strings.Contains(printedText(rec), "nothing was run") {
		t.Fatalf("ran %v output %q", *ran, printedText(rec))
	}
}

func TestTheOpenSessionsCatalogIsReadAndAnOlderGatewayIsAskedWithoutIt(t *testing.T) {
	reads := localPluginReads(t)
	c, gw, _ := pluginCLI(t, map[string]string{"mcp-status": "Ask the MCP server"}, nil)
	listed, err := gatewayPluginLookup(c, "mcp-status")
	if err != nil || listed == nil || !*listed {
		t.Fatalf("lookup %v %v", listed, err)
	}
	catalogs := gw.client.callsOf("commands.catalog")
	if !sameJSON(catalogs[len(catalogs)-1], map[string]any{"session_id": "local-s"}) {
		t.Fatalf("catalog params %v", catalogs)
	}
	gw.set(func() { gw.olderGateway = true })
	c.sessionID = "local-t"
	listed, err = gatewayPluginLookup(c, "mcp-status")
	if err != nil || listed == nil || !*listed {
		t.Fatalf("lookup on the older gateway %v %v", listed, err)
	}
	catalogs = gw.client.callsOf("commands.catalog")
	if !sameJSON(catalogs[len(catalogs)-2:], []any{map[string]any{"session_id": "local-t"}, map[string]any{}}) {
		t.Fatalf("catalog params %v", catalogs)
	}
	if len(reads.seen()) != 0 {
		t.Fatalf("local plugin reads %v", reads.seen())
	}
}
