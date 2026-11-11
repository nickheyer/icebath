package tuiserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Nanoseconds of the clock every local process shares, the perf counter of the frames
func nowNS() int64 { return int64(pythread.Monotonic() * 1e9) }

// Transport of the compute host child that wraps every outbound JSON RPC frame into an rpc host frame
type hostTransport struct{ emit func(frame map[string]any) }

// Wraps one frame for the parent, keyed on the event session when it is an event
func (t *hostTransport) Write(obj *pyjson.Object) bool {
	sid := ""
	suppress(func() {
		if method, _ := obj.Get("method"); method == "event" {
			if params, ok := obj.GetOr("params", nil).(*pyjson.Object); ok && params != nil {
				sid = strOr(params.GetOr("session_id", nil))
			}
		}
	})
	t.emit(map[string]any{"type": "rpc", "sid": sid, "message": obj})
	return true
}

// Nothing to close, the pipe belongs to the host
func (t *hostTransport) Close() {}

// Share of the shutdown budget held back for the post drain finalize
const flushReserveSecs = 1.0

// Fallback control.error text of a routed server method that failed without a message
var controlFailures = map[string]string{"session.save": "session save failed", "session.compress": "session compression failed"}

// One submitted turn of the host executor, done once it ran or was cancelled
type hostTurn struct {
	done      chan struct{}
	cancelled bool
}

// Whether the turn finished or was cancelled
func (t *hostTurn) isDone() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Persistent dashboard compute host child owning live agents, line JSON frames over stdin and stdout
type computeHost struct {
	ctx             context.Context
	stdout          io.Writer
	writeMu         sync.Mutex
	slots           chan struct{}
	closed          pythread.Event
	parentPID       int
	bootID          string
	progressMu      sync.Mutex
	progressCounter int
	turnsMu         sync.Mutex
	turns           map[*hostTurn]string
	transport       *hostTransport
	heartbeatSecs   float64
}

// Compute host writing to stdout with a bounded turn pool and, with a positive heartbeat, its heartbeat and parent guard
func newComputeHost(ctx context.Context, stdout io.Writer, maxWorkers int, heartbeatSecs *float64) *computeHost {
	if stdout == nil {
		stdout = os.Stdout
	}
	if maxWorkers <= 0 {
		maxWorkers = defaultHostWorkers()
	}
	h := &computeHost{ctx: ctx, stdout: stdout, slots: make(chan struct{}, maxWorkers), parentPID: os.Getppid(),
		bootID: strings.ReplaceAll(uuid.NewString(), "-", ""), turns: map[*hostTurn]string{}}
	h.transport = &hostTransport{emit: h.emit}
	if heartbeatSecs != nil {
		h.heartbeatSecs = *heartbeatSecs
	} else {
		raw := os.Getenv("ICEBATH_COMPUTE_HOST_HEARTBEAT_SECS")
		if raw == "" {
			raw = "15"
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			panic(err)
		}
		h.heartbeatSecs = value
	}
	if h.heartbeatSecs > 0 {
		go h.heartbeatLoop()
		go h.parentGuardLoop()
	}
	return h
}

// Writes one frame as a compact JSON line, stamped with the host clock
func (h *computeHost) emit(frame map[string]any) {
	if _, present := frame["host_ns"]; !present {
		frame["host_ns"] = nowNS()
	}
	data, err := pyjson.Dumps(objectOf(frame), pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	if err != nil {
		panic(err)
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	_, _ = io.WriteString(h.stdout, data+"\n")
}

// Emits a per session frame keyed by the request it answers
func (h *computeHost) reply(kind, sid string, requestID any, extra map[string]any) {
	frame := map[string]any{"type": kind, "sid": sid, "request_id": requestID}
	for k, v := range extra {
		frame[k] = v
	}
	h.emit(frame)
}

// Stops the pool, kills live foreground commands and flushes the shared metrics rows the hard exit skips
func (h *computeHost) close() {
	h.closed.Set()
	h.cancelQueuedTurns()
	suppress(func() { tools.KillLiveForegroundProcesses(true) })
	observability.FlushBeforeHardExit(h.ctx, observability.ExitFlushTimeout)
}

// Cancels turns still waiting for a pool slot
func (h *computeHost) cancelQueuedTurns() {
	h.turnsMu.Lock()
	defer h.turnsMu.Unlock()
	for turn := range h.turns {
		if !turn.isDone() {
			turn.cancelled = true
		}
	}
}

// Drains in flight turns within the budget less the flush reserve, then finalizes every idle session
func (h *computeHost) shutdown(reason string, wait float64) {
	h.closed.Set()
	budget := max(0.0, wait)
	deadline := pythread.Monotonic() + budget - min(flushReserveSecs, budget/2.0)
	for {
		remaining := deadline - pythread.Monotonic()
		if remaining <= 0 || len(h.liveTurns()) == 0 {
			break
		}
		time.Sleep(time.Duration(min(0.05, remaining) * float64(time.Second)))
	}
	liveSIDs := map[string]bool{}
	h.turnsMu.Lock()
	for turn, sid := range h.turns {
		if sid != "" && !turn.isDone() {
			liveSIDs[sid] = true
		}
	}
	h.turnsMu.Unlock()
	h.flushAllSessions(reason, liveSIDs)
	h.close()
}

// Finalizes every server session except those whose turn is still live
func (h *computeHost) flushAllSessions(reason string, skip map[string]bool) {
	for _, live := range tuigateway.LiveSessions() {
		if skip[live.SID] {
			continue
		}
		suppress(func() { finalizeSession(h.ctx, live.Session, "compute_host_"+reason) })
	}
}

// Routes one frame to its handler, an error frame for an unknown type
func (h *computeHost) handleFrame(frame map[string]any) {
	kind := strOr(frame["type"])
	switch kind {
	case "turn.start":
		h.handleTurnStart(frame)
	case "interrupt":
		h.handleInterrupt(frame)
	case "respond":
		h.handleRespond(frame)
	case "reload_mcp":
		h.handleReloadMCP(frame)
	case "control":
		h.handleControl(frame)
	case "shutdown":
		h.handleShutdown(frame)
	default:
		h.emit(map[string]any{"type": "error", "request_id": frame["request_id"], "message": "unknown frame type: " + kind})
	}
}

// Acknowledges an explicit shutdown and closes cleanly
func (h *computeHost) handleShutdown(frame map[string]any) {
	h.emit(map[string]any{"type": "shutdown.ack", "request_id": frame["request_id"]})
	h.close()
}

// Queues a turn on the pool, tracked until it finishes
func (h *computeHost) handleTurnStart(frame map[string]any) {
	turn := &hostTurn{done: make(chan struct{})}
	copied := make(map[string]any, len(frame))
	for k, v := range frame {
		copied[k] = v
	}
	h.turnsMu.Lock()
	h.turns[turn] = strOr(frame["sid"])
	h.turnsMu.Unlock()
	go func() {
		defer func() {
			close(turn.done)
			h.turnsMu.Lock()
			delete(h.turns, turn)
			h.turnsMu.Unlock()
		}()
		select {
		case h.slots <- struct{}{}:
		case <-h.closed.Waiter():
			return
		}
		defer func() { <-h.slots }()
		h.turnsMu.Lock()
		cancelled := turn.cancelled
		h.turnsMu.Unlock()
		if cancelled {
			return
		}
		h.runRealTurn(copied)
	}()
}

// Runs a body for the frame's session, any failure becoming one reply of the error kind
func (h *computeHost) guarded(frame map[string]any, errorKind string, body func(sid string, requestID any), onError func(sid string), errorExtra map[string]any) {
	sid := strOr(frame["sid"])
	requestID := frame["request_id"]
	defer func() {
		if r := recover(); r != nil {
			if onError != nil {
				onError(sid)
			}
			extra := map[string]any{}
			for k, v := range errorExtra {
				extra[k] = v
			}
			extra["message"] = failureText(r)
			h.reply(errorKind, sid, requestID, extra)
		}
	}()
	body(sid, requestID)
}

// Interrupts the local agent of a session and releases its pending clarify
func (h *computeHost) handleInterrupt(frame map[string]any) {
	h.guarded(frame, "interrupt.ack", func(sid string, requestID any) {
		session := tuigateway.SessionRecord(sid)
		if session == nil {
			h.reply("interrupt.ack", sid, requestID, map[string]any{"applied": false})
			return
		}
		interruptSessionTurn(h.ctx, sid, session, "", false)
		h.reply("interrupt.ack", sid, requestID, map[string]any{"applied": true, "applied_ns": nowNS()})
	}, nil, map[string]any{"applied": false})
}

// Resolves a server to client request this host owns, a batch clarify lock or a cancel of one method
func (h *computeHost) handleRespond(frame map[string]any) {
	h.guarded(frame, "respond.error", func(sid string, requestID any) {
		params, isDict := frame["params"].(map[string]any)
		failure := ""
		if tuigateway.SessionRecord(sid) == nil {
			failure = "session not found"
		} else if !isDict {
			failure = "response params must be an object"
		}
		if failure != "" {
			h.reply("respond.error", sid, requestID, map[string]any{"message": failure})
			return
		}
		var response map[string]any
		if lock, isLock := params["lock"].(map[string]any); isLock {
			response = lookupMethod("clarify.lock")(h.ctx, requestID, lock)
		} else if cancel, isCancel := params["cancel"].(map[string]any); isCancel {
			reason := strOr(cancel["reason"])
			if reason == "" {
				reason = "resolved"
			}
			method, present := cancel["method"]
			if !present {
				panic(errors.New("KeyError: 'method'"))
			}
			methodName := strOr(method)
			withdrawn := tuigateway.Cancel(&sid, reason, &methodName)
			response = map[string]any{"jsonrpc": "2.0", "id": requestID, "result": map[string]any{"cancelled": withdrawn}}
		} else {
			responseFrame := params
			if inner, isFrame := params["frame"].(map[string]any); isFrame {
				responseFrame = inner
			}
			status := "expired"
			if tuigateway.ResolveResponse(objectOf(responseFrame), nil) {
				status = "ok"
			}
			response = map[string]any{"jsonrpc": "2.0", "id": requestID, "result": map[string]any{"status": status}}
		}
		h.reply("respond.ack", sid, requestID, map[string]any{"response": response})
	}, nil, nil)
}

// Runs one isolated turn end to end and answers with turn.end or turn.error
func (h *computeHost) runRealTurn(frame map[string]any) {
	sid := strOr(frame["sid"])
	requestID := strOr(frame["request_id"])
	if requestID == "" {
		requestID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if sid == "" {
		h.reply("turn.error", sid, requestID, map[string]any{"message": "sid required"})
		return
	}
	defer func() {
		if r := recover(); r != nil {
			suppress(func() {
				if session := tuigateway.SessionRecord(sid); session != nil {
					unlock := lockHistory(session)
					session.Set("running", false)
					clearInflightTurn(session)
					unlock()
				}
			})
			h.reply("turn.error", sid, requestID, map[string]any{"reason": "exception", "message": failureText(r)})
		}
	}()
	session := h.ensureServerSession(frame)
	installBorrowedLease(sid, session, frame)
	text, hasText := frame["text"]
	var inflight any
	if hasText {
		inflight = text
	} else {
		text = frame["prompt"]
		if text == nil {
			text = ""
		}
		inflight = frame["prompt"]
	}
	var turnStartedAt float64
	refused := func() bool {
		unlock := lockHistory(session)
		defer unlock()
		if queued := frame["queued_prompt_generation"]; queued != nil {
			want, err := pyIntOf(queued)
			if err != nil {
				panic(err)
			}
			if intOr(session.Get("_queued_prompt_generation")) != int64(want) {
				h.reply("turn.end", sid, requestID, map[string]any{"interrupted": true, "ended_ns": nowNS()})
				return true
			}
		}
		if truthy(session.Get("running")) {
			h.reply("turn.error", sid, requestID, map[string]any{"message": "session busy"})
			return true
		}
		session.Update(map[string]any{"running": true, "_turn_cancel_requested": false, "last_active": nowSeconds()})
		startInflightTurn(session, inflight, "", nil)
		turnStartedAt = nowSeconds()
		return false
	}()
	if refused {
		return
	}
	h.reply("turn.started", sid, requestID, map[string]any{"started_ns": nowNS()})
	suppress(func() {
		ensureSessionDBRow(h.ctx, session)
		db, release := sessionDB(h.ctx, session)
		defer release()
		if db != nil {
			if err := reopenIfFinalized(h.ctx, db, strOr(session.Get("session_key"))); err != nil {
				panic(err)
			}
		}
	})
	suppress(func() { persistBranchSeed(h.ctx, session) })
	displayKind := strOr(frame["display_kind"])
	displayMetadata, _ := frame["display_metadata"].(map[string]any)
	runPromptSubmit(tuigateway.BindTransport(h.ctx, h.transport), requestID, sid, session, text,
		promptSubmitOptions{displayKind: displayKind, displayMetadata: displayMetadata})
	if runThread, ok := session.Get("_run_thread").(interface {
		Alive() bool
		Join(time.Duration)
	}); ok && runThread != nil {
		for runThread.Alive() {
			runThread.Join(time.Second)
			if runThread.Alive() && truthy(frame["turn_id"]) {
				h.emitTurnActivity(sid, session, strOr(frame["turn_id"]), turnStartedAt)
			}
		}
	}
	unlock := lockHistory(session)
	meta := historyMeta(session)
	interrupted := truthy(session.Get("_turn_cancel_requested"))
	unlock()
	info := sessionInfo(h.ctx, sessionAgent(session), session)
	h.progressMu.Lock()
	h.progressCounter++
	h.progressMu.Unlock()
	meta["interrupted"], meta["ended_ns"], meta["session_info"], meta["session_info_emitted"] = interrupted, nowNS(), info, true
	h.reply("turn.end", sid, requestID, meta)
}

// Pushes the agent's own activity clock to the parent, never the host heartbeat
func (h *computeHost) emitTurnActivity(sid string, session *tuigateway.Session, turnID string, startedAt float64) {
	var activityNS any
	func() {
		defer func() {
			if r := recover(); r != nil {
				logger().Debug("compute host activity unavailable sid="+sid, "error", r)
			}
		}()
		a := sessionAgent(session)
		if a == nil {
			panic(errors.New("no agent"))
		}
		summary := a.GetActivitySummary()
		stampedAt, stamped := pyFloat(summary["last_activity_at"])
		elapsed, timed := pyFloat(summary["seconds_since_activity"])
		if summary["last_activity_at"] != nil && stamped == nil && stampedAt >= startedAt && summary["seconds_since_activity"] != nil && timed == nil && elapsed >= 0 {
			activityNS = nowNS() - int64(elapsed*1e9)
		}
	}()
	h.transport.Write(objectOf(map[string]any{"jsonrpc": "2.0", "method": "compute_host.activity", "params": map[string]any{
		"session_id": sid, "turn_id": turnID, "activity_ns": activityNS}}))
}

// Live session of a frame refreshed from it, built under the frame's profile when this host has none
func (h *computeHost) ensureServerSession(frame map[string]any) *tuigateway.Session {
	sid := strOr(frame["sid"])
	session := tuigateway.SessionRecord(sid)
	if session != nil {
		session.Set("transport", h.transport)
		if frame["cols"] != nil {
			cols, _ := pyIntOf(frame["cols"])
			if cols == 0 {
				cols = 80
			}
			session.Set("cols", cols)
		}
		for _, key := range []string{"cwd", "profile_home"} {
			if truthy(frame[key]) {
				session.Set(key, strOr(frame[key]))
			}
		}
		if explicit, present := frame["explicit_cwd"]; present {
			session.Set("explicit_cwd", truthy(explicit))
		}
	} else {
		session = h.buildServerSession(frame, sid)
	}
	if images, isList := frame["attached_images"].([]any); isList {
		session.Set("attached_images", append([]any{}, images...))
	}
	if pending, isDict := frame["pending_model_switch"].(map[string]any); isDict && len(pending) > 0 {
		copied := make(map[string]any, len(pending))
		for k, v := range pending {
			copied[k] = v
		}
		session.Set("pending_model_switch", copied)
	}
	return session
}

// Builds the agent under the frame's profile scope and registers the session, a minimal record when wiring fails
func (h *computeHost) buildServerSession(frame map[string]any, sid string) *tuigateway.Session {
	key := strOr(frame["session_key"])
	if key == "" {
		key = sid
	}
	history, _ := frame["history"].([]any)
	if history == nil {
		history = []any{}
	}
	profileHome := strOr(frame["profile_home"])
	ctx := h.ctx
	var sessionDB *state.DB
	ownsDB := false
	built := func() any {
		defer func() {
			if ownsDB && sessionDB != nil {
				suppress(func() { state.ReleaseOrClose(sessionDB) })
			}
		}()
		scoped := ctx
		if profileHome != "" {
			scoped = profileRuntimeScope(ctx, profileHome, true)
			opened, err := state.Acquire(scoped, profileHome+string(os.PathSeparator)+"state.db")
			if err != nil {
				panic(err)
			}
			sessionDB, ownsDB = opened, true
		}
		launch := truthy(frame["context_cwd_is_launch_artifact"])
		o := makeAgentOptions{sessionID: key, modelOverride: frame["model_override"], platformOverride: strOr(frame["source"]),
			cwdOverride: strOr(frame["cwd"]), contextCwdIsLaunchArtifact: &launch, sessionDB: sessionDB}
		if reasoning, isDict := frame["reasoning_config_override"].(map[string]any); isDict {
			o.reasoningConfigOverride, o.reasoningOverrideSet = reasoning, true
		}
		if tier := frame["service_tier_override"]; tier != nil {
			text := strOr(tier)
			o.serviceTierOverride = &text
		}
		if owner := frame["auth_user_id"]; owner != nil {
			text := strOr(owner)
			o.authUserID = &text
		}
		a, err := makeAgent(scoped, sid, key, o)
		if err != nil {
			panic(err)
		}
		if built, isAgent := a.(*agent.AIAgent); isAgent && transferDBToAgent(scoped, built, sessionDB) {
			ownsDB = false
		}
		return a
	}()
	cols, _ := pyIntOf(frame["cols"])
	if cols == 0 {
		cols = 80
	}
	initialized := func() (done bool) {
		defer func() {
			if recover() != nil {
				done = false
			}
		}()
		initSession(tuigateway.BindTransport(ctx, h.transport), sid, key, built, append([]any{}, history...), cols, strOr(frame["cwd"]),
			sessionDB, strOr(frame["source"]), "", truthy(frame["explicit_cwd"]))
		return true
	}()
	if !initialized {
		cwd := strOr(frame["cwd"])
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		version, _ := pyIntOf(frame["history_version"])
		now := nowSeconds()
		tuigateway.SetSessionRecord(sid, tuigateway.NewSession(map[string]any{
			"agent": built, "session_key": key, "history": append([]any{}, history...), "history_lock": &sync.Mutex{},
			"history_version": int64(version), "inflight_turn": nil, "created_at": now, "last_active": now, "running": false,
			"attached_images": []any{}, "image_counter": 0, "cwd": cwd, "cols": cols, "explicit_cwd": truthy(frame["explicit_cwd"]),
			"slash_worker": nil, "show_reasoning": loadShowReasoning(ctx), "tool_progress_mode": loadToolProgressMode(ctx),
			"edit_snapshots": map[string]any{}, "tool_started_at": map[string]any{}, "model_override": frame["model_override"],
			"source": resolveSessionSource(strOr(frame["source"])), "transport": h.transport}))
	}
	session := tuigateway.SessionRecord(sid)
	session.Set("transport", h.transport)
	session.Set("auth_user_id", frame["auth_user_id"])
	if profileHome != "" {
		session.Set("profile_home", profileHome)
	}
	if override := frame["model_override"]; override != nil {
		session.Set("model_override", override)
	}
	if pending, isDict := frame["pending_model_switch"].(map[string]any); isDict && len(pending) > 0 {
		copied := make(map[string]any, len(pending))
		for k, v := range pending {
			copied[k] = v
		}
		session.Set("pending_model_switch", copied)
	}
	return session
}

// Rebuilds the MCP tool snapshot of a session through the reload.mcp method
func (h *computeHost) handleReloadMCP(frame map[string]any) {
	h.guarded(frame, "control.error", func(sid string, requestID any) {
		resp := handleRequest(h.ctx, map[string]any{"id": requestID, "method": "reload.mcp",
			"params": map[string]any{"session_id": sid, "confirm": true}})
		h.reply("reload_mcp.ack", sid, requestID, map[string]any{"response": resp})
	}, nil, nil)
}

// Runs one classified mutator route against a host owned session
func (h *computeHost) handleControl(frame map[string]any) {
	routeName := strOr(frame["route_name"])
	h.guarded(frame, "control.error", func(sid string, requestID any) {
		route, classified := mutatorRouteTable[routeName]
		session := tuigateway.SessionRecord(sid)
		failure := ""
		switch {
		case !classified:
			failure = "unclassified route: " + routeName
		case session == nil:
			failure = "session not found"
		case route == "idle-gated" && truthy(session.Get("running")):
			failure = "session busy"
		}
		if failure != "" {
			h.reply("control.error", sid, requestID, map[string]any{"message": failure})
			return
		}
		if routeName == "reload.mcp" {
			copied := map[string]any{}
			for k, v := range frame {
				copied[k] = v
			}
			copied["type"] = "reload_mcp"
			h.handleReloadMCP(copied)
			return
		}
		ack := h.controlAck(frame, session)
		if message, failed := ack["error"]; failed {
			extra := map[string]any{"message": message}
			if code := ack["code"]; truthy(code) {
				extra["code"] = code
			}
			h.reply("control.error", sid, requestID, extra)
			return
		}
		ack["route_name"] = routeName
		h.reply("control.ack", sid, requestID, ack)
	}, func(sid string) {
		if routeName != "session.compress" && routeName != "slash.compress" {
			return
		}
		suppress(func() {
			if a := sessionAgent(tuigateway.SessionRecord(sid)); a != nil {
				agent.FinalizeContextEngineCompressionNotification(a, false)
			}
		})
	}, nil)
}

// control.ack payload of one classified route, or an error dict
func (h *computeHost) controlAck(frame map[string]any, session *tuigateway.Session) map[string]any {
	sid := strOr(frame["sid"])
	routeName := strOr(frame["route_name"])
	command := strOr(frame["command"])
	var ack map[string]any
	if routeName == "session.save" || routeName == "session.compress" {
		params := map[string]any{"session_id": sid}
		if routeName == "session.compress" {
			if focus := strings.TrimSpace(strings.TrimPrefix(command, "/compress")); focus != "" {
				params["focus_topic"] = focus
			}
		}
		response := lookupMethod(routeName)(h.ctx, frame["request_id"], params)
		if failure, failed := response["error"].(map[string]any); failed {
			message := strOr(failure["message"])
			if message == "" {
				message = controlFailures[routeName]
			}
			return map[string]any{"error": message, "code": failure["code"]}
		}
		result, _ := response["result"].(map[string]any)
		if result == nil {
			result = map[string]any{}
		}
		ack = map[string]any{"result": result}
		if routeName == "session.save" {
			return ack
		}
		unlock := lockHistory(session)
		for k, v := range historyMeta(session) {
			ack[k] = v
		}
		unlock()
	} else {
		output := ""
		if routeName == "slash.refine" {
			_, focus := splitFirstWord(strings.TrimLeft(command, "/"))
			output, _ = liveSlashCommandOutput(h.ctx, sid, session, "refine", focus)
		} else if command != "" {
			output = mirrorSlashSideEffects(h.ctx, sid, session, command)
		}
		unlock := lockHistory(session)
		messages, err := tuigateway.HistoryToMessages(h.ctx, append([]any{}, historyList(session.Get("history"))...), strOr(session.Get("profile_home")), true)
		if err != nil {
			unlock()
			panic(err)
		}
		ack = map[string]any{"output": output, "messages": messages}
		for k, v := range historyMeta(session) {
			ack[k] = v
		}
		unlock()
	}
	ack["session_info"] = sessionInfo(h.ctx, sessionAgent(session), session)
	return ack
}

// Turns still running or queued
func (h *computeHost) liveTurns() []*hostTurn {
	h.turnsMu.Lock()
	defer h.turnsMu.Unlock()
	var live []*hostTurn
	for turn := range h.turns {
		if !turn.isDone() {
			live = append(live, turn)
		}
	}
	return live
}

// Emits a heartbeat frame each interval with live turns, the progress counter and resident memory
func (h *computeHost) heartbeatLoop() {
	for !h.closed.Wait(h.heartbeatSecs) {
		active := len(h.liveTurns())
		h.progressMu.Lock()
		counter := h.progressCounter
		h.progressMu.Unlock()
		h.emit(map[string]any{"type": "hb", "active_turns": active, "progress_counter": counter, "rss_mb": rssMB(os.Getpid())})
	}
}

// Shuts down and exits once the parent process is gone
func (h *computeHost) parentGuardLoop() {
	for !h.closed.Wait(1.0) {
		ppid := os.Getppid()
		if ppid == 0 || ppid == 1 || (h.parentPID != 0 && ppid != h.parentPID) {
			h.emit(map[string]any{"type": "orphan", "old_ppid": h.parentPID, "ppid": ppid})
			h.shutdown("orphan", 10.0)
			os.Exit(0)
		}
	}
}

// Transcript identity of turn.end and control.ack frames, the caller holding the history lock
func historyMeta(session *tuigateway.Session) map[string]any {
	return map[string]any{"session_key": strOr(session.Get("session_key")), "history_version": intOr(session.Get("history_version")),
		"message_count": len(historyList(session.Get("history")))}
}

// Resident set of a pid in MiB through ps, zero when it cannot be read
func rssMB(pid int) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-o", "rss=", "-p", strconv.Itoa(pid))
	cmd.Stdin, cmd.Stderr = nil, nil
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return 0
	}
	lines := strings.Split(text, "\n")
	kb, err := strconv.Atoi(strings.TrimSpace(lines[len(lines)-1]))
	if err != nil {
		return 0
	}
	return float64(kb) / 1024.0
}

// Turn pool size, ICEBATH_TUI_RPC_POOL_WORKERS at least 2, 8 when unset or not a number
func defaultHostWorkers() int {
	raw := os.Getenv("ICEBATH_TUI_RPC_POOL_WORKERS")
	if raw == "" {
		raw = "8"
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 8
	}
	return max(2, n)
}

// Serves the compute host protocol over stdin and stdout until shutdown, stdin closing or a signal
func runHost(ctx context.Context, stdin io.Reader, stdout io.Writer) {
	_ = os.Setenv("ICEBATH_COMPUTE_HOST_CHILD", "1")
	if stdin == nil {
		stdin = os.Stdin
	}
	host := newComputeHost(ctx, stdout, 0, nil)
	var shuttingDown pythread.Event
	tuigateway.RegisterLiveTransport(host.transport)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(signals)
	cwd, _ := os.Getwd()
	host.emit(map[string]any{"type": "hello", "host_pid": os.Getpid(), "boot_id": host.bootID, "build_sha": hostBuildSHA(),
		"cwd": cwd, "hermes_home": os.Getenv("ICEBATH_HOME")})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		reader := bufio.NewReader(stdin)
		for {
			raw, err := reader.ReadBytes('\n')
			if len(raw) == 0 && err != nil {
				return
			}
			if host.closed.IsSet() {
				return
			}
			parsed, perr := pyjson.LoadsBytes(raw)
			if perr != nil {
				host.emit(map[string]any{"type": "error", "message": fmt.Sprintf("invalid json: %s", perr)})
				continue
			}
			frame, isDict := pyjson.Plain(parsed).(map[string]any)
			if !isDict {
				host.emit(map[string]any{"type": "error", "message": "frame must be an object"})
				continue
			}
			host.handleFrame(frame)
			if frame["type"] == "shutdown" {
				os.Exit(0)
			}
			if host.closed.IsSet() || err != nil {
				return
			}
		}
	}()
	defer func() {
		tuigateway.UnregisterLiveTransport(host.transport)
		host.shutdown("stdin_closed", 2.0)
	}()
	for !host.closed.Wait(0.2) {
		select {
		case <-readerDone:
			return
		case <-signals:
			if !shuttingDown.IsSet() {
				shuttingDown.Set()
				host.shutdown("sigterm", 10.0)
			}
			return
		default:
		}
	}
}

// Runs the compute host child when the binary was re-executed as it, exiting with its status
func DispatchComputeHostEntry(ctx context.Context) {
	if len(os.Args) < 2 || os.Args[1] != ComputeHostEntry {
		return
	}
	runHost(ctx, nil, nil)
	os.Exit(0)
}
