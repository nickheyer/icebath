package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/initprompt"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const (
	memoryReviewUsage = "Unknown /memory subcommand. Use: pending, approve <id>, reject <id>, approval <on|off>."
	skillsReviewUsage = "Unknown /skills subcommand here. Use: pending, approve <id>, reject <id>, diff <id>, approval <on|off>. " +
		"(Search/install/browse are terminal-side.)"
)

// Built in slash commands command.dispatch answers itself when no earlier stage claimed the name
var slashBuiltins map[string]dispatchStage

func init() {
	slashBuiltins = map[string]dispatchStage{
		"queue": cmdQueue, "q": cmdQueue, "learn": promptBuiltin(memory.BuildLearnPrompt), "plan": promptBuiltin(provider.BuildPlanPrompt),
		"init": cmdInit, "moa": cmdMoa, "focus": cmdFocus, "retry": cmdRetry, "steer": cmdSteer, "goal": cmdGoal,
		"loop": cmdLoop, "undo": cmdUndo, "snapshot": cmdSnapshot, "snap": cmdSnapshot,
		"compress": cmdCompress, "compact": cmdCompress, "memory": cmdMemory, "skills": cmdSkills}
}

// Busy refusal of a running session, nil when idle
func busyError(rid any, session *tuigateway.Session, cmd string) map[string]any {
	if truthy(session.Get("running")) {
		return rpcErr(rid, 4009, busyMessage(cmd, truthy(session.Get("_manual_compress_active"))), nil)
	}
	return nil
}

// Session key the goal and loop managers act on, else the 4001 refusal
func sessionKeyOrErr(rid any, session *tuigateway.Session) (string, map[string]any) {
	if session == nil {
		return "", rpcErr(rid, 4001, "no active session", nil)
	}
	sidKey := strOr(session.Get("session_key"))
	if sidKey == "" {
		return "", rpcErr(rid, 4001, "no session key", nil)
	}
	return sidKey, nil
}

// Durable history and the indices of its user originated turns, called under the history lock
func userTurnIndices(session *tuigateway.Session) ([]map[string]any, []int) {
	history := historyWithoutEphemeralScaffolding(historyList(session.Get("history")))
	var indices []int
	for i, m := range history {
		if state.UserOriginatedTurnView(m) != nil {
			indices = append(indices, i)
		}
	}
	return history, indices
}

// Busy recheck under the history lock, then the history and user turn indices or the refusal
func rewindPrelude(rid any, session *tuigateway.Session, cmd, emptyMsg string) ([]map[string]any, []int, map[string]any) {
	if busy := busyError(rid, session, cmd); busy != nil {
		return nil, nil, busy
	}
	history, userIndices := userTurnIndices(session)
	if len(userIndices) == 0 {
		return nil, nil, rpcErr(rid, 4018, emptyMsg, nil)
	}
	return history, userIndices, nil
}

// Whether a rewind failure is the ValueError family the caller answers with its own code
func isRewindValueError(err error) bool {
	var unavailable *state.RewindTargetUnavailableError
	return isValueError(err) || errors.Is(err, state.ErrNotUserOriginatedTurn) || errors.As(err, &unavailable)
}

// Rewound history with its live view and count, a ValueError answered with its code and anything else with 5008
func rewindOrErr(ctx context.Context, rid any, session *tuigateway.Session, keep int, valueCode int, valuePrefix, failPrefix string, requireRetryable bool) ([]map[string]any, map[string]any, int, map[string]any) {
	installed, liveView, count, err := rewindActiveSessionHistory(ctx, session, keep, requireRetryable)
	if err == nil {
		return installed, liveView, count, nil
	}
	if isRewindValueError(err) {
		return nil, nil, 0, rpcErr(rid, valueCode, valuePrefix+err.Error(), nil)
	}
	return nil, nil, 0, rpcErr(rid, 5008, failPrefix+errorMessage(err), nil)
}

// Sends the argument as the next turn
func cmdQueue(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	if arg == "" {
		return rpcErr(rid, 4004, "usage: /queue <prompt>", nil)
	}
	return ok(rid, map[string]any{"type": "send", "message": arg})
}

// Built in that submits a prompt built from the argument as a normal turn
func promptBuiltin(build func(string) string) dispatchStage {
	return func(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
		return ok(rid, map[string]any{"type": "send", "message": build(arg)})
	}
}

// AGENTS.md prompt built against the session's active directory, submitted as a normal turn
func cmdInit(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	var skey *string
	if session != nil {
		if raw, has := session.Lookup("session_key"); has && raw != nil {
			key := strOr(raw)
			skey = &key
		}
	}
	env := tools.ToolEnvFrom(ctx)
	cwd := ""
	if skey != nil && *skey != "" {
		catchPanic(func() { cwd, _ = tools.GetSessionCwd(env, *skey) })
	}
	if info, err := os.Stat(cwd); cwd == "" || err != nil || !info.IsDir() {
		cwd = ""
		if session != nil {
			cwd = sessionCwd(ctx, session)
		}
	}
	return ok(rid, map[string]any{"type": "send", "message": initprompt.BuildInitPromptForCwd(env, cwd, arg, skey)})
}

// One prompt through the default MoA preset with the prior model restored after the turn
func cmdMoa(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	var res map[string]any
	failure := catchPanic(func() {
		if arg == "" {
			res = rpcErr(rid, 4004, provider.MoAUsage(), nil)
			return
		}
		if session == nil {
			res = rpcErr(rid, 4001, "no active session", nil)
			return
		}
		moaCfg := loadCfg(ctx)["moa"]
		if !truthy(moaCfg) {
			moaCfg = map[string]any{}
		}
		preset := provider.NormalizeMoAConfig(moaCfg)["default_preset"]
		a := sessionAgent(session)
		restore := map[string]any{"override": session.Get("model_override"), "model": nil, "provider": nil}
		if a != nil {
			restore["model"], restore["provider"] = a.Model(), a.Provider()
		}
		session.Set("moa_one_shot_restore", restore)
		if a != nil {
			persist := false
			_, err := applyModelSwitch(ctx, strOr(params["session_id"]), session, pyrepr.Str(preset)+" --provider moa",
				modelSwitchOptions{confirmExpensiveModel: false, pinSessionOverride: true, persistOverride: &persist, countSwitch: false})
			if err != nil {
				session.Pop("moa_one_shot_restore")
				panic(err)
			}
		} else {
			session.Set("model_override", map[string]any{"provider": "moa", "model": preset, "base_url": "moa://local",
				"api_key": "moa-virtual-provider", "api_mode": "chat_completions"})
		}
		notice := fmt.Sprintf("MoA one-shot queued with preset %s; previous model will be restored after this turn.", pyrepr.Str(preset))
		res = ok(rid, map[string]any{"type": "send", "notice": notice, "message": arg})
	})
	if failure != nil {
		return rpcErr(rid, 5030, "moa unavailable: "+errorMessage(failure), nil)
	}
	return res
}

// Focus view toggle through the config.set branch the TUI uses, or its status line
func cmdFocus(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	display, isDict := loadCfg(ctx)["display"].(map[string]any)
	if !isDict {
		display = map[string]any{}
	}
	cur := truthy(display["focus_view"])
	action, target := tuigateway.ResolveFocusArg(arg, cur)
	if action == "usage" {
		return rpcErr(rid, 4004, "usage: /focus [on|off|status]", nil)
	}
	if action == "status" {
		saved := display["focus_saved_tool_progress"]
		if !truthy(saved) {
			saved = loadToolProgressMode(ctx)
		}
		return execOut(rid, tuigateway.FormatFocusStatus(cur, saved))
	}
	value := "off"
	if target != nil && *target {
		value = "on"
	}
	res := lookupMethod("config.set")(ctx, rid, map[string]any{"key": "focus", "value": value, "session_id": strOr(params["session_id"])})
	if _, failed := res["error"]; failed {
		return res
	}
	result, _ := res["result"].(map[string]any)
	toolProgress := result["tool_progress"]
	if !truthy(toolProgress) {
		toolProgress = "all"
	}
	return execOut(rid, tuigateway.FormatFocusToggleMessage(target != nil && *target, toolProgress))
}

// Rewinds the last user turn and resubmits its text, refusing media it cannot reconstruct
func cmdRetry(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	if session == nil {
		return rpcErr(rid, 4001, "no active session to retry", nil)
	}
	if busy := busyError(rid, session, "retry"); busy != nil {
		return busy
	}
	content, refusal := func() (string, map[string]any) {
		unlock := lockHistory(session)
		defer unlock()
		if busy := busyError(rid, session, "retry"); busy != nil {
			return "", busy
		}
		if truthy(session.Get("attached_images")) {
			return "", rpcErr(rid, 4018, "retry cannot safely reconstruct or combine attached media", nil)
		}
		history, userIndices, errResp := rewindPrelude(rid, session, "retry", "no previous user message to retry")
		if errResp != nil {
			return "", errResp
		}
		_, liveView, err := state.HistoryBeforeUserOriginatedTurn(history, userIndices[len(userIndices)-1])
		panicIf(err)
		if _, err := state.RetryableUserText(liveView["content"]); err != nil {
			return "", rpcErr(rid, 4018, err.Error(), nil)
		}
		_, rewoundView, _, errResp := rewindOrErr(ctx, rid, session, len(userIndices)-1, 4018, "", "retry: failed to persist history: ", true)
		if errResp != nil {
			return "", errResp
		}
		text, err := state.RetryableUserText(rewoundView["content"])
		panicIf(err)
		return text, nil
	}()
	if refusal != nil {
		return refusal
	}
	tuiModelFriction(ctx, "retry", session, 1)
	return ok(rid, map[string]any{"type": "send", "message": content})
}

// Queues a steer on a running agent, or sends the text as the next turn when idle or refused
func cmdSteer(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	if arg == "" {
		return rpcErr(rid, 4004, "usage: /steer <prompt>", nil)
	}
	shown := pystr.Slice(arg, 0, 80)
	if len([]rune(arg)) > 80 {
		shown += "..."
	}
	if !(session != nil && truthy(session.Get("running"))) {
		return ok(rid, map[string]any{"type": "send", "message": arg, "notice": "No agent running; sent as next turn: " + shown})
	}
	if a := sessionAgent(session); a != nil {
		queued := false
		catchPanic(func() { queued = a.Steer(arg) })
		if queued {
			return execOut(rid, "⏩ Steer queued — arrives after the next tool call: "+shown)
		}
	}
	return ok(rid, map[string]any{"type": "send", "message": arg})
}

// Goal max turns of the config, twenty when unset, falsy or not an integer
func goalMaxTurns(cfg map[string]any) int64 {
	goalsCfg, _ := cfg["goals"].(map[string]any)
	raw, has := goalsCfg["max_turns"]
	if !has {
		raw = int64(20)
	}
	if !truthy(raw) {
		return 20
	}
	n, err := pyint.Int64(raw)
	if err != nil || n == 0 {
		return 20
	}
	return n
}

// Runs the goal command on the session's goal manager in its profile scope
func cmdGoal(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	scoped := profileRuntimeScope(ctx, strOr(session.Get("profile_home")), true)
	sidKey, errResp := sessionKeyOrErr(rid, session)
	if errResp != nil {
		return errResp
	}
	mgr := goals.NewGoalManager(scoped, sidKey, goalMaxTurns(loadCfg(scoped)))
	result, err := goals.DispatchGoalCommand(mgr, arg, goals.GoalCommandOptions{AuthorizeGate: func() string { return "" },
		LastUserMessage: goals.LastUserMessageFromDB(scoped, sidKey)})
	panicIf(err)
	if result.Error {
		return rpcErr(rid, 4004, result.Output, nil)
	}
	if result.Prompt == nil || *result.Prompt == "" {
		return execOut(rid, result.Output)
	}
	payload := map[string]any{"type": "send", "notice": result.Output, "message": *result.Prompt}
	if !result.Kickoff {
		payload["notice"] = result.Output + "\nContinuing now — taking the next step."
		payload["display"] = "/goal resume"
	}
	return ok(rid, payload)
}

// Runs the loop command on the session's loop manager, noting an active goal that defers its wakeups
func cmdLoop(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	sidKey, errResp := sessionKeyOrErr(rid, session)
	if errResp != nil {
		return errResp
	}
	result, err := agent.DispatchLoopCommand(ctx, agent.NewLoopManager(ctx, sidKey), arg, nil)
	panicIf(err)
	output := result.Output
	if result.Created {
		blocks := false
		catchPanic(func() { blocks = agent.GoalBlocksLoopTick(ctx, sidKey) })
		if blocks {
			output += "\nNote: an active /goal is driving this session — loop " +
				"wakeups defer until the goal finishes, pauses, or parks."
		}
	}
	return execOut(rid, output)
}

// Backs up N user turns, soft deleting them on disk, and prefills the composer with the rewound text
func cmdUndo(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	if session == nil {
		return rpcErr(rid, 4001, "no active session to undo", nil)
	}
	if busy := busyError(rid, session, "undo"); busy != nil {
		return busy
	}
	sessionKey := strOr(session.Get("session_key"))
	if sessionKey == "" {
		return rpcErr(rid, 4001, "no session key for undo", nil)
	}
	argStr := pystr.Strip(arg)
	n := int64(1)
	if argStr != "" {
		parsed, valid := pyint.Parse(pystr.SplitWhitespace(argStr, -1)[0])
		if !valid {
			return rpcErr(rid, 4004, fmt.Sprintf("undo: invalid count %s — use /undo or /undo N", pyrepr.Repr(argStr)), nil)
		}
		switch {
		case parsed.IsInt64():
			n = max(parsed.Int64(), 1)
		case parsed.Sign() > 0:
			n = math.MaxInt64
		}
	}
	var active []map[string]any
	var targetText string
	var turnsUndone, rewoundCount int
	if refusal := func() map[string]any {
		unlock := lockHistory(session)
		defer unlock()
		_, userIndices, errResp := rewindPrelude(rid, session, "undo", "no user messages to undo")
		if errResp != nil {
			return errResp
		}
		turnsUndone = int(min(n, int64(len(userIndices))))
		installed, liveView, count, errResp := rewindOrErr(ctx, rid, session, len(userIndices)-turnsUndone, 4004, "undo: ", "undo: ", false)
		if errResp != nil {
			return errResp
		}
		active, rewoundCount = installed, count
		targetText = state.FlattenMessageText(liveView["content"], "\n")
		return nil
	}(); refusal != nil {
		return refusal
	}
	if a := sessionAgent(session); a != nil {
		scoped := sessionProfileRuntimeScope(ctx, session, true)
		for _, step := range []func(){
			func() { a.NotifyMemoryRewound(scoped, sessionKey) },
			func() { _ = agent.InvalidateSystemPrompt(scoped, a) },
			func() { a.SetLastFlushedDBIdx(len(active)) },
		} {
			catchPanic(step)
		}
	}
	tuiModelFriction(ctx, "undo", session, turnsUndone)
	turnWord := "turns"
	if turnsUndone == 1 {
		turnWord = "turn"
	}
	notice := fmt.Sprintf("↶ Undid %d %s (%d message(s)). Edit and resubmit, or send a new message.", turnsUndone, turnWord, rewoundCount)
	return ok(rid, map[string]any{"type": "prefill", "message": targetText, "notice": notice})
}

// Whether a snapshot argument asks to restore
func isSnapshotRestore(arg string) bool {
	if arg == "" {
		return false
	}
	words := pystr.SplitWhitespace(arg, 1)
	return len(words) > 0 && (pystr.Lower(words[0]) == "restore" || pystr.Lower(words[0]) == "rewind")
}

// Blocks snapshot restore in the TUI, every other snapshot form falling through to the refusal
func cmdSnapshot(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	if !isSnapshotRestore(arg) {
		return nil
	}
	return execOut(rid, "/snapshot restore is blocked in the TUI because it changes config/state on disk "+
		"while the live agent has cached settings. Run it in the classic CLI, then restart the TUI.")
}

// Compresses the live session, relayed to the compute host when it owns the session
func cmdCompress(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	if session == nil {
		return rpcErr(rid, 4001, "no active session to compress", nil)
	}
	if busy := busyError(rid, session, "compress"); busy != nil {
		return busy
	}
	sid := strOr(params["session_id"])
	if sessionUsesComputeHost(ctx, session, nil) {
		command := "/" + name
		if arg != "" {
			command += " " + arg
		}
		status, text := computeHostSlash(ctx, sid, session, "compress", command)
		switch status {
		case "failed":
			return rpcErr(rid, 5019, text, nil)
		case "rejected":
			return rpcErr(rid, 4009, text, nil)
		case "pending":
			return ok(rid, map[string]any{"type": "exec", "status": "pending", "output": text})
		}
		return execOut(rid, text)
	}
	a := sessionAgent(session)
	var output string
	var err error
	if failure := catchPanic(func() { output, err = compressLiveWithFeedback(ctx, sid, session, a, arg, true) }); failure != nil {
		if fe, isErr := failure.(error); isErr {
			err = fe
		} else {
			err = errors.New(errorMessage(failure))
		}
	}
	if err == nil {
		return execOut(rid, output)
	}
	var busy *CompressionBusy
	if errors.As(err, &busy) {
		return rpcErr(rid, 4009, busy.Error(), nil)
	}
	agent.FinalizeContextEngineCompressionNotification(a, false)
	return rpcErr(rid, 5009, "compress failed: "+errorMessage(err), nil)
}

// Memory store of the session profile, the live agent store else a freshly loaded one, nil when both fail
func pendingSubcommandStore(ctx context.Context, session *tuigateway.Session) *memory.Store {
	if session != nil {
		var store *memory.Store
		catchPanic(func() {
			if a := sessionAgent(session); a != nil {
				store = a.MemoryStore()
			}
		})
		if store != nil {
			return store
		}
	}
	var store *memory.Store
	catchPanic(func() {
		loaded, err := memory.LoadOnDiskStore(ctx, tools.ToolEnvFrom(ctx))
		if err == nil {
			store = loaded
		}
	})
	return store
}

// Runs a write approval review subcommand, the usage line when the shared handler does not claim it
func runPendingReview(ctx context.Context, rid any, subsystem, arg string, session *tuigateway.Session, unknown string) map[string]any {
	opts := memory.PendingOptions{SetMode: func(enabled bool) error { return writeConfigKey(ctx, subsystem+".write_approval", enabled) }}
	if subsystem == tools.WriteApprovalMemory {
		opts.MemoryStore = pendingSubcommandStore(ctx, session)
	}
	out, handled, err := memory.HandlePendingSubcommand(ctx, tools.ToolEnvFrom(ctx), subsystem, pystr.SplitWhitespace(arg, -1), opts)
	panicIf(err)
	if !handled {
		out = unknown
	}
	return execOut(rid, out)
}

// Reviews staged memory writes, approves or rejects them and toggles the gate
func cmdMemory(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	return runPendingReview(ctx, rid, "memory", arg, session, memoryReviewUsage)
}

// Skill write approval review subcommands, every hub subcommand falling through to the refusal
func cmdSkills(ctx context.Context, rid any, params map[string]any, session *tuigateway.Session, name, arg string) map[string]any {
	sub := pystr.SplitWhitespace(arg, -1)
	var allowed []string
	if c, found := ResolveCommand("skills"); found {
		allowed = c.DesktopSubcommands
	}
	if len(sub) == 0 || !slices.Contains(allowed, pystr.Lower(sub[0])) {
		return nil
	}
	env := tools.ToolEnvFrom(ctx)
	first := pystr.Lower(sub[0])
	if first != "approval" && first != "mode" && !tools.WriteApprovalEnabled(env, tools.WriteApprovalSkills) &&
		tools.PendingCount(env, tools.WriteApprovalSkills) == 0 {
		return execOut(rid, "Skill write approval is off (skills.write_approval). "+
			"Enable it with /skills approval on, then review staged writes "+
			"with /skills pending.")
	}
	return runPendingReview(ctx, rid, "skills", arg, session, skillsReviewUsage)
}
