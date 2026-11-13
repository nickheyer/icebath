package app

import (
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Commands whose state lives in the gateway session, answered there by these routes
var gatewayCLIRoutes = map[string]func(c *CLI, connection *gatewaycli.GatewayConnection, cmd string) (bool, error){
	"diff": gatewayDiff, "reload": gatewayReload, "handoff": gatewayHandoff,
}

// Named refusals, nothing runs so the gateway is not told
var gatewayCLIRefused = map[string]bool{"handoff": true}

// Session diff of the gateway tools' checkpoint baseline, false for the working tree modes
func gatewayDiff(c *CLI, connection *gatewaycli.GatewayConnection, cmd string) (bool, error) {
	_, rest, _ := strings.Cut(cmd, " ")
	args := pystr.SplitWhitespace(pystr.Lower(pystr.Strip(rest)), -1)
	has := func(word string) bool {
		for _, a := range args {
			if a == word {
				return true
			}
		}
		return false
	}
	if !has("session") {
		return false, nil
	}
	raw, err := connection.Call(c.ctx, "rollback.session_diff", map[string]any{"session_id": c.sessionID})
	if err != nil {
		return false, err
	}
	result, _ := pyjson.Plain(raw).(map[string]any)
	if !pyrepr.Truthy(result["enabled"]) {
		printLines(catalogLines(c.commandsT("diff.not_enabled", nil))...)
	} else {
		c.printSessionDiff(result, has("--stat") || has("stat"))
	}
	return true, nil
}

// Rereads .env here and where the session's turns and tools run
func gatewayReload(c *CLI, connection *gatewaycli.GatewayConnection, cmd string) (bool, error) {
	if _, err := config.ReloadEnv(home.Dir(c.ctx)); err != nil {
		panic(err)
	}
	raw, err := connection.Call(c.ctx, "reload.env", map[string]any{"session_id": c.sessionID})
	if err != nil {
		return false, err
	}
	result, _ := pyjson.Plain(raw).(map[string]any)
	updated := result["updated"]
	if updated == nil {
		updated = 0
	}
	printLines("  " + c.t("cli.reload.env_reloaded", map[string]any{"count": updated}))
	return true, nil
}

// Refuses handoff, no gateway verb carrying one yet
func gatewayHandoff(c *CLI, connection *gatewaycli.GatewayConnection, cmd string) (bool, error) {
	cprint("  " + gatewayRefusal("handoff"))
	return true, nil
}

// Runs a command whose state the gateway session owns, true when the gateway answered it
func runGatewayCLICommand(c *CLI, canonical, cmd string) bool {
	route := gatewayCLIRoutes[canonical]
	if route == nil {
		return false
	}
	connection := gatewayConnectionFor(c)
	if connection == nil {
		return false
	}
	answered, err := route(c, connection, pystr.Strip(cmd))
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		cprint(fmt.Sprintf("  /%s: %s", canonical, err.Error()))
		return true
	}
	return answered
}

// Busy input modes the busy setting mirrors locally
var gatewayBusyModes = map[string]bool{"queue": true, "steer": true, "interrupt": true}

// Mirrors an accepted busy mode onto what Enter does while a turn runs
func mirrorGatewayBusy(c *CLI, arg string) {
	if mode := pystr.Lower(arg); gatewayBusyModes[mode] {
		c.busyInputMode = mode
	}
}

// Polls the subagent roster right away after an off turn review spawned its reviewer
func watchGatewayReviewer(c *CLI, arg string) {
	if view := gatewayProcessView(); view != nil {
		view.watchSubagents(gatewayToolsRetainSeconds)
	}
}

// Reports the owner lays out itself, printed as is
var gatewayVerbatimSettings = map[string]bool{"toolsets": true}

// Per session setting commands the gateway applies, with what is mirrored locally after it accepted
var gatewaySettings = map[string]func(c *CLI, arg string){
	"personality": nil, "busy": mirrorGatewayBusy, "approvals": nil, "footer": nil, "fast": nil,
	"refine": nil, "review": watchGatewayReviewer, "cron": nil, "toolsets": nil,
}

// Whether a form waits for its work, true for a cron write and false for cron reads or unclosed quotes
func gatewaySettingUnbounded(canonical, arg string) bool {
	if canonical != "cron" {
		return false
	}
	tokens, err := pyshlex.SplitPOSIX(arg, false)
	if err != nil {
		return false
	}
	return gateway.CronRead(tokens) == ""
}

// Gateway reply of a setting command, a cron write waiting with no deadline and true when Ctrl+C stopped it
func callGatewaySetting(c *CLI, connection *gatewaycli.GatewayConnection, canonical, arg string) (any, bool, error) {
	command := canonical
	if arg != "" {
		command += " " + arg
	}
	if !gatewaySettingUnbounded(canonical, arg) {
		raw, err := connection.Call(c.ctx, "slash.exec", map[string]any{"session_id": c.sessionID, "command": command})
		return raw, false, err
	}
	return callUnbounded(c, connection, canonical, command)
}

// Runs a per session setting command on the gateway and prints its reply, true when the gateway answered it
func runGatewaySetting(c *CLI, canonical, cmd string) bool {
	mirror, known := gatewaySettings[canonical]
	if !known {
		return false
	}
	connection := gatewayConnectionFor(c)
	if connection == nil {
		return false
	}
	parts := pystr.SplitWhitespace(pystr.Strip(cmd), 1)
	arg := ""
	if len(parts) > 1 {
		arg = pystr.Strip(parts[1])
	}
	raw, stopped, err := callGatewaySetting(c, connection, canonical, arg)
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		cprint(fmt.Sprintf("  /%s: %s", canonical, err.Error()))
		return true
	}
	if stopped {
		return true
	}
	reply, _ := pyjson.Plain(raw).(map[string]any)
	for _, line := range pystr.Splitlines(strOrEmpty(reply["output"]), false) {
		if line != "" && !gatewayVerbatimSettings[canonical] {
			cprint("  " + line)
		} else {
			cprint(line)
		}
	}
	if mirror != nil {
		mirror(c, arg)
	}
	return true
}

// Checkpoint rows of a rollback.list answer as the checkpoint list formatter reads them
func gatewayCheckpointRows(result map[string]any) []tools.CheckpointEntry {
	listed, _ := result["checkpoints"].([]any)
	rows := make([]tools.CheckpointEntry, 0, len(listed))
	for _, row := range anyRows(listed) {
		hash := pyrepr.Str(mapValueOr(row, "hash", ""))
		short := ""
		if pyrepr.Truthy(row["hash"]) {
			short = pystr.Slice(hash, 0, 7)
		}
		rows = append(rows, tools.CheckpointEntry{Hash: hash, ShortHash: short, Timestamp: pyrepr.Str(mapValueOr(row, "timestamp", "")),
			Reason: pyrepr.Str(mapValueOr(row, "message", ""))})
	}
	return rows
}

// Lists, diffs or restores the gateway tools' checkpoints, rendering main's text over the owner's answers
func gatewayRollback(c *CLI, connection *gatewaycli.GatewayConnection, command string) {
	words := pystr.SplitWhitespace(command, -1)
	if len(words) > 0 {
		words = words[1:]
	}
	var args []string
	for _, a := range words {
		if l := pystr.Lower(a); l != "--all" && l != "--force" {
			args = append(args, a)
		}
	}
	restoreAll := len(args) != len(words)
	wd, werr := os.Getwd()
	if werr != nil {
		panic(werr)
	}
	cwd, ok := os.LookupEnv("TERMINAL_CWD")
	if !ok {
		cwd = wd
	}
	sid := c.sessionID
	err := func() error {
		if len(args) == 0 {
			raw, err := connection.Call(c.ctx, "rollback.list", map[string]any{"session_id": sid})
			if err != nil {
				return err
			}
			listed, _ := pyjson.Plain(raw).(map[string]any)
			if !pyrepr.Truthy(listed["enabled"]) {
				var lines []string
				for _, line := range pystr.Splitlines(c.commandsT("rollback.not_enabled", nil), false) {
					if line != "" {
						line = "  " + line
					}
					lines = append(lines, line)
				}
				infoPrint(strings.Join(lines, "\n"))
				return nil
			}
			infoPrint(tools.FormatCheckpointList(gatewayCheckpointRows(listed), cwd))
			return nil
		}
		if pystr.Lower(args[0]) == "diff" {
			if len(args) < 2 {
				infoPrint("  " + c.commandsT("rollback.usage_diff", nil))
				return nil
			}
			raw, err := connection.Call(c.ctx, "rollback.diff", map[string]any{"session_id": sid, "hash": args[1]})
			if err != nil {
				return err
			}
			result, _ := pyjson.Plain(raw).(map[string]any)
			c.gatewayRollbackDiff(result)
			return nil
		}
		params := map[string]any{"session_id": sid, "hash": args[0]}
		filePath := ""
		if len(args) > 1 {
			filePath = args[1]
			params["file_path"] = filePath
		} else if restoreAll {
			params["all"] = true
		}
		raw, err := connection.Call(c.ctx, "rollback.restore", params)
		if err != nil {
			return err
		}
		result, _ := pyjson.Plain(raw).(map[string]any)
		return c.gatewayRollbackRestored(connection, result, filePath)
	}()
	if err != nil {
		if !isGatewayClientError(err) {
			panic(err)
		}
		infoPrint("  ❌ " + err.Error())
	}
}

// Prints a rollback diff answer, stat then the body cut at eighty lines
func (c *CLI) gatewayRollbackDiff(result map[string]any) {
	stat, diff := strOrEmpty(mapValueOr(result, "stat", "")), strOrEmpty(mapValueOr(result, "diff", ""))
	if stat == "" && diff == "" {
		infoPrint("  " + c.commandsT("rollback.no_changes_since", nil))
		return
	}
	if stat != "" {
		infoPrint("\n" + stat)
	}
	if diff == "" {
		return
	}
	lines := pystr.Splitlines(diff, false)
	if len(lines) > 80 {
		infoPrint(strings.Join(lines[:80], "\n"))
		infoPrint("\n  " + c.commandsT("rollback.more_lines", map[string]any{"count": len(lines) - 80}))
		return
	}
	infoPrint("\n" + diff)
}

// Prints a restore answer and rereads the transcript when the owner rewound the newest user turn
func (c *CLI) gatewayRollbackRestored(connection *gatewaycli.GatewayConnection, result map[string]any, filePath string) error {
	if !pyrepr.Truthy(result["success"]) {
		infoPrint("  ❌ " + pyrepr.Str(result["error"]))
		return nil
	}
	restored := map[string]any{"hash": mapValueOr(result, "restored_to", ""), "reason": mapValueOr(result, "reason", "")}
	if filePath != "" {
		restored["file"] = filePath
		infoPrint("  " + c.commandsT("rollback.restored_file", restored))
	} else {
		infoPrint("  " + c.commandsT("rollback.restored_all", restored))
	}
	c.rollbackKept(result)
	infoPrint("  " + c.commandsT("rollback.snapshot_saved", nil))
	if pyrepr.Truthy(result["history_removed"]) {
		snapshot, err := connection.Call(c.ctx, "session.resume", map[string]any{"session_id": c.sessionID})
		if err != nil {
			return err
		}
		connection.State(c.sessionID).AdoptSnapshot(snapshot)
		snap, _ := pyjson.Plain(snapshot).(map[string]any)
		rows, _ := snap["messages"].([]any)
		var history []map[string]any
		for _, row := range anyRows(rows) {
			if pyrepr.Truthy(row["archived"]) {
				continue
			}
			kept := copyMap(row)
			delete(kept, "row_id")
			history = append(history, kept)
		}
		c.conversationHistory = history
		infoPrint("  " + c.commandsT("rollback.chat_turn_undone", nil))
	}
	return nil
}
