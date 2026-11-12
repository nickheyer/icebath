package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Display category the catalog files plugin commands under on older gateways
const gatewayPluginCategory = "Plugin commands"

// Line printed when the gateway no longer has a plugin command
const gatewayGoneLine = "  /%s: the gateway no longer has this plugin command; nothing was run."

// How often a running command's clarify requests are looked for
const gatewayChoicePoll = 100 * time.Millisecond

// Line printed when Ctrl+C stopped waiting for a plugin command
const gatewayStoppedLine = "  /%s: stopped waiting. The command keeps running in the gateway and its reply will not be shown."

// Outcome of a plugin command run on the gateway
type pluginRunOutcome int

const (
	pluginNotRun pluginRunOutcome = iota
	pluginAnswered
	pluginGone
)

// Raised when the gateway's command list could not be read, already printed, so nothing runs for the input
type CatalogUnavailable struct{ Name string }

func (e *CatalogUnavailable) Error() string { return e.Name }

// Connection, epoch and session a catalog read was served for
type gatewayBoundary struct {
	connection *gatewaycli.GatewayConnection
	epoch      int
	sessionID  string
}

// Plugin commands of one catalog read and the boundary it was served for
type gatewayPluginCatalog struct {
	served gatewayBoundary
	found  map[string]string
}

// Skill commands and notice of one catalog read and the boundary it was served for
type gatewaySkillCatalog struct {
	served  gatewayBoundary
	skills  map[string]map[string]any
	warning string
}

// Plugin toolsets of one gateway life
type gatewayToolsetCatalog struct {
	connection *gatewaycli.GatewayConnection
	epoch      int
	toolsets   []tools.ConfigurableToolset
	portable   map[string]bool
}

func gatewayBoundaryOf(c *CLI, connection *gatewaycli.GatewayConnection) gatewayBoundary {
	return gatewayBoundary{connection: connection, epoch: connection.Epoch(), sessionID: c.sessionID}
}

// Reads the catalog's plugin commands and caches them and the skills for the boundary that served them
func readGatewayCatalog(c *CLI, connection *gatewaycli.GatewayConnection) (map[string]string, error) {
	session := c.sessionID
	raw, epoch, err := gatewayCatalog(c, connection, session)
	if err != nil {
		return nil, err
	}
	catalog, _ := pyjson.Plain(raw).(map[string]any)
	if pyrepr.Truthy(catalog["plugin_discovery_error"]) {
		return nil, &gateway.GatewayClientError{Msg: "plugin command discovery failed: " + pyrepr.Str(catalog["plugin_discovery_error"])}
	}
	found := map[string]string{}
	if listed, isDict := catalog["plugin_commands"].(map[string]any); isDict {
		for name, desc := range listed {
			found[strings.TrimLeft(name, "/")] = strOrEmpty(desc)
		}
	} else {
		categories, _ := catalog["categories"].([]any)
		for _, rawCategory := range categories {
			category, _ := rawCategory.(map[string]any)
			if category == nil || category["name"] != gatewayPluginCategory {
				continue
			}
			pairs, _ := category["pairs"].([]any)
			for _, rawRow := range pairs {
				if row, isList := rawRow.([]any); isList && len(row) == 2 {
					if name, isStr := row[0].(string); isStr {
						found[strings.TrimLeft(name, "/")] = strOrEmpty(row[1])
					}
				}
			}
		}
	}
	descriptions := map[string]string{}
	pairs, _ := catalog["pairs"].([]any)
	for _, rawRow := range pairs {
		if row, isList := rawRow.([]any); isList && len(row) == 2 {
			if name, isStr := row[0].(string); isStr {
				descriptions[name] = strOrEmpty(row[1])
			}
		}
	}
	skillRows := map[string]map[string]any{}
	if listed, isDict := catalog["skills"].(map[string]any); isDict {
		for key := range listed {
			description := descriptions[key]
			if description == "" {
				description = "Skill"
			}
			skillRows[key] = map[string]any{"name": strings.TrimLeft(key, "/"), "description": description}
		}
	}
	served := gatewayBoundary{connection: connection, epoch: epoch, sessionID: session}
	if gatewayBoundaryOf(c, connection) == served {
		c.uiMu.Lock()
		c.gatewayPluginCommands = &gatewayPluginCatalog{served: served, found: found}
		c.gatewaySkills = &gatewaySkillCatalog{served: served, skills: skillRows, warning: strOrEmpty(catalog["warning"])}
		c.uiMu.Unlock()
	}
	return found, nil
}

// Command catalog of the open session with its epoch, asked again without the session on an older gateway
func gatewayCatalog(c *CLI, connection *gatewaycli.GatewayConnection, session string) (any, int, error) {
	if session != "" && c.gatewaySessionOpen {
		raw, epoch, err := connection.CallTagged(c.ctx, "commands.catalog", map[string]any{"session_id": session}, gatewaycli.CallOptions{})
		var refused *gateway.GatewayRPCError
		if err == nil || !errors.As(err, &refused) || refused.Error() != "invalid_params" {
			return raw, epoch, err
		}
	}
	return connection.CallTagged(c.ctx, "commands.catalog", map[string]any{}, gatewaycli.CallOptions{})
}

// Drops the CLI's copy of the gateway's catalog so the next read asks the gateway again
func invalidateGatewayCatalog(c *CLI) {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	c.gatewayPluginCommands, c.gatewaySkills = nil, nil
}

// Cached plugin commands for the current boundary, nil after a hang up or a boundary move
func cachedGatewayCommands(c *CLI, connection *gatewaycli.GatewayConnection) map[string]string {
	if connection.Closed() {
		return nil
	}
	c.uiMu.Lock()
	cached := c.gatewayPluginCommands
	c.uiMu.Unlock()
	if cached == nil || cached.served != gatewayBoundaryOf(c, connection) {
		return nil
	}
	return cached.found
}

// Skill commands of the session from the gateway's catalog, false off the gateway
func gatewaySessionSkills(c *CLI) (map[string]map[string]any, bool) {
	found, _, ok := gatewaySkillsAndNotice(c)
	return found, ok
}

// Skill commands and the shadowing notice from the gateway's catalog, a failed read listing none
func gatewaySkillsAndNotice(c *CLI) (map[string]map[string]any, string, bool) {
	connection := gatewayConnectionFor(c)
	if connection == nil {
		return nil, "", false
	}
	c.uiMu.Lock()
	cached := c.gatewaySkills
	c.uiMu.Unlock()
	if connection.Closed() || cached == nil || cached.served != gatewayBoundaryOf(c, connection) {
		if _, err := readGatewayCatalog(c, connection); err != nil {
			if !isGatewayClientError(err) {
				panic(err)
			}
			slog.Debug(fmt.Sprintf("commands.catalog unavailable: %s", err.Error()))
			return map[string]map[string]any{}, "", true
		}
		c.uiMu.Lock()
		cached = c.gatewaySkills
		c.uiMu.Unlock()
		if cached == nil {
			return map[string]map[string]any{}, "", true
		}
	}
	return cached.skills, cached.warning, true
}

// Help notes of skills a built in command shadows, the gateway's notice on its session, else the local scan
func gatewaySkillNotes(c *CLI, local func() []string) []string {
	_, notice, ok := gatewaySkillsAndNotice(c)
	if !ok {
		return local()
	}
	if notice == "" {
		return []string{}
	}
	return []string{notice}
}

// Runs a skill command on the gateway session and queues the message it returns, false off the gateway
func runGatewaySkill(c *CLI, baseCmd, rest string) bool {
	connection := gatewayConnectionFor(c)
	if connection == nil {
		return false
	}
	command := strings.TrimLeft(baseCmd, "/")
	if rest != "" {
		command += " " + rest
	}
	raw, err := connection.Call(c.ctx, "slash.exec", map[string]any{"session_id": c.sessionID, "command": command})
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		cprint(fmt.Sprintf("  %s: %s", baseCmd, err.Error()))
		return true
	}
	reply, _ := pyjson.Plain(raw).(map[string]any)
	if reply["type"] == "skill" && pyrepr.Truthy(reply["message"]) {
		name := command
		if pyrepr.Truthy(reply["name"]) {
			name = pyrepr.Str(reply["name"])
		}
		c.queueLoadedSkills(pyrepr.Str(reply["message"]), i18n.T(c.ctx, "cli.skills.loading_skill", "", map[string]any{"name": name}), nil)
	}
	if pyrepr.Truthy(reply["notice"]) {
		cprint("  " + pyrepr.Str(reply["notice"]))
	} else if pyrepr.Truthy(reply["output"]) {
		cprint("  " + pyrepr.Str(reply["output"]))
	}
	return true
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
		slog.Debug(fmt.Sprintf("plugins.list unavailable: %s", err.Error()))
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

// Plugin command names and descriptions for completion, false off the gateway, a failed read listing none
func gatewayPluginNames(c *CLI) (map[string]string, bool) {
	connection := gatewayConnectionFor(c)
	if connection == nil {
		return nil, false
	}
	found := cachedGatewayCommands(c, connection)
	if found == nil {
		read, err := readGatewayCatalog(c, connection)
		if err != nil {
			if !isGatewayClientError(err) {
				panic(err)
			}
			return map[string]string{}, true
		}
		found = read
	}
	return found, true
}

// Whether name is one of the gateway's plugin commands, rereading once on a miss, nil off the gateway
func gatewayPluginLookup(c *CLI, name string) (*bool, error) {
	connection := gatewayConnectionFor(c)
	if connection == nil {
		return nil, nil
	}
	yes := true
	if found := cachedGatewayCommands(c, connection); found != nil {
		if _, ok := found[name]; ok {
			return &yes, nil
		}
	}
	found, err := readGatewayCatalog(c, connection)
	if err != nil {
		if !isGatewayClientError(err) {
			return nil, err
		}
		cprint(fmt.Sprintf("  /%s: the gateway's command list is unavailable (%s); nothing was run.", name, err.Error()))
		return nil, &CatalogUnavailable{Name: name}
	}
	_, listed := found[name]
	return &listed, nil
}

// Runs slash.exec unbounded on its own socket, stopped on Ctrl+C, the interrupt returned after the stopped line
func callUnbounded(c *CLI, connection *gatewaycli.GatewayConnection, name, command string) (any, bool, error) {
	wake := &pythread.Event{}
	c.uiMu.Lock()
	c.gatewayCommandWait = wake
	c.uiMu.Unlock()
	answering := &gatewayAnswering{}
	memory.SpawnContextThread(c.ctx, "icebath-command-choices", func(ctx context.Context) {
		answerGatewayChoices(c, connection, wake, answering)
	}).Start()
	defer func() {
		c.uiMu.Lock()
		c.gatewayCommandWait = nil
		c.uiMu.Unlock()
		if answering.active() {
			c.withdrawGatewayModal("clarify")
		}
	}()
	raw, err := connection.CallUntil(c.turnCtx, "slash.exec", wake, map[string]any{"session_id": c.sessionID, "command": command})
	var stopped *gatewaycli.CallStopped
	interrupted := errors.Is(err, tools.ErrKeyboardInterrupt)
	if errors.As(err, &stopped) || interrupted {
		cprint(fmt.Sprintf(gatewayStoppedLine, name))
		if interrupted {
			return nil, false, err
		}
		return nil, true, nil
	}
	return raw, false, err
}

// Clarify request id the panel is answering while it is open
type gatewayAnswering struct {
	mu sync.Mutex
	id any
	on bool
}

func (g *gatewayAnswering) set(id any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.id, g.on = id, true
}

func (g *gatewayAnswering) clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.id, g.on = nil, false
}

func (g *gatewayAnswering) active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.on
}

func (g *gatewayAnswering) is(id any) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.on && pyjson.Equal(g.id, id)
}

// Answers the clarify requests a running command sends with the CLI's panel, other frames staying queued
func answerGatewayChoices(c *CLI, connection *gatewaycli.GatewayConnection, wake *pythread.Event, answering *gatewayAnswering) {
	events, state := connection.Events, connection.State(c.sessionID)
	sessionID := c.sessionID
	listener := gatewaycli.SessionListener(func(ctx context.Context, payload map[string]any) {
		if answering.is(payload["id"]) {
			c.withdrawGatewayModal("clarify")
		}
	})
	state.Lock()
	state.RequestListeners = append(state.RequestListeners, &listener)
	state.Unlock()
	defer func() {
		state.Lock()
		defer state.Unlock()
		state.RequestListeners = gatewaycli.WithoutListener(state.RequestListeners, &listener)
	}()
	for !wake.Wait(gatewayChoicePoll.Seconds()) {
		asks := events.TakeMatching(func(item any) bool {
			obj, isDict := pyjson.AsObject(item)
			if !isDict || obj.Vals["method"] != "clarify" {
				return false
			}
			if _, hasID := obj.Get("id"); !hasID {
				return false
			}
			params := frameParams(item)
			return params["session_id"] == sessionID
		})
		for _, frame := range asks {
			obj, _ := pyjson.AsObject(frame)
			answering.set(obj.Vals["id"])
			reply := func() map[string]any {
				defer answering.clear()
				return clarifyReply(c.clarifyCallback, frameParams(frame))
			}()
			connection.Reply(obj.Vals["id"], reply)
		}
	}
}

// Runs a plugin command on the gateway, gone when a fresh catalog no longer lists it, not run off the gateway
func runGatewayPluginCommand(c *CLI, name, arg string) pluginRunOutcome {
	connection := gatewayConnectionFor(c)
	if connection == nil {
		return pluginNotRun
	}
	command := name
	if arg != "" {
		command += " " + arg
	}
	raw, stopped, err := callUnbounded(c, connection, name, command)
	reply := map[string]any{}
	if err != nil {
		var refused *gateway.GatewayRPCError
		switch {
		case errors.As(err, &refused) && refused.Error() == "unsupported_command":
			reply = map[string]any{"type": "unsupported"}
		case isGatewayClientError(err):
			cprint(fmt.Sprintf("  /%s: %s", name, err.Error()))
			return pluginAnswered
		default:
			panic(err)
		}
	} else if stopped {
		return pluginAnswered
	} else if dict, isDict := pyjson.Plain(raw).(map[string]any); isDict {
		reply = dict
	}
	kind := any("exec")
	if v, has := reply["type"]; has {
		kind = v
	}
	if kind != "exec" {
		if gatewayCommandDropped(c, connection, name) {
			return pluginGone
		}
		cprint(fmt.Sprintf(gatewayGoneLine, name))
		return pluginAnswered
	}
	if output := strOrEmpty(reply["output"]); output != "" {
		cprint(output)
	}
	return pluginAnswered
}

// Whether a fresh catalog read with the cached copy dropped no longer lists name, false when the read failed
func gatewayCommandDropped(c *CLI, connection *gatewaycli.GatewayConnection, name string) bool {
	c.uiMu.Lock()
	c.gatewayPluginCommands = nil
	c.uiMu.Unlock()
	found, err := readGatewayCatalog(c, connection)
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		return false
	}
	_, listed := found[name]
	return !listed
}

// Prints that name is no longer a plugin command unless a command, skill or bundle name starts with it
func reportGatewayCommandGone(name string, known ...map[string]any) bool {
	prefix := "/" + name
	for key := range commands.Commands(commands.Default) {
		if strings.HasPrefix(key, prefix) {
			return false
		}
	}
	for _, group := range known {
		for key := range group {
			if strings.HasPrefix(key, prefix) {
				return false
			}
		}
	}
	cprint(fmt.Sprintf(gatewayGoneLine, name))
	return true
}

// Tells the gateway about a command this process runs so pre_command fires there, false off the gateway
func observeGatewayCommand(c *CLI, name, arg string, runs bool) bool {
	connection := gatewayConnectionFor(c)
	if connection == nil {
		return false
	}
	if !runs {
		return true
	}
	epoch := connection.Epoch()
	c.uiMu.Lock()
	unsupported := c.observeUnsupported
	c.uiMu.Unlock()
	if unsupported != nil && unsupported.connection == connection && unsupported.epoch == epoch {
		return true
	}
	_, err := connection.Call(c.ctx, "command.observe", map[string]any{"session_id": c.sessionID, "name": name, "arg": arg})
	var refused *gateway.GatewayRPCError
	switch {
	case err == nil:
	case errors.As(err, &refused):
		if !(refused.Code != nil && *refused.Code == -32601) && refused.Error() != "unknown_method" {
			slog.Debug(fmt.Sprintf("command.observe /%s refused: %s", name, err.Error()))
			return true
		}
		c.uiMu.Lock()
		c.observeUnsupported = &gatewayBoundary{connection: connection, epoch: epoch}
		shown := c.observeNoticeShown
		c.observeNoticeShown = true
		c.uiMu.Unlock()
		if !shown {
			cprint("  This gateway predates this CLI: plugin pre_command hooks don't see the commands the CLI " +
				"runs itself until it restarts.")
		}
	case isGatewayClientError(err):
		slog.Debug(fmt.Sprintf("command.observe /%s not delivered: %s", name, err.Error()))
	default:
		panic(err)
	}
	return true
}

// Plugins the gateway loaded by name for the plugins listing, false off the gateway
func gatewayLoadedPlugins(c *CLI) (map[string]map[string]any, bool) {
	connection := gatewayConnectionFor(c)
	if connection == nil {
		return nil, false
	}
	raw, err := connection.Call(c.ctx, "plugins.list", map[string]any{})
	if err != nil {
		var refused *gateway.GatewayRPCError
		switch {
		case errors.As(err, &refused):
			if (refused.Code != nil && *refused.Code == -32601) || refused.Error() == "unknown_method" {
				cprint("  This gateway doesn't report its loaded plugins (it predates this CLI); restart it for load details.")
			} else {
				cprint(fmt.Sprintf("  The gateway's plugin list is unavailable (%s).", err.Error()))
			}
		case isGatewayClientError(err):
			cprint(fmt.Sprintf("  The gateway's plugin list is unavailable (%s).", err.Error()))
		default:
			panic(err)
		}
		return map[string]map[string]any{}, true
	}
	reply, _ := pyjson.Plain(raw).(map[string]any)
	rows, _ := reply["plugins"].([]any)
	out := map[string]map[string]any{}
	for _, row := range anyRows(rows) {
		if name, isStr := row["name"].(string); isStr {
			out[name] = row
		}
	}
	return out, true
}
