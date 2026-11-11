package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	xterm "github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Characters of a pending admission preview line
const gatewayPreviewChars = 80

// Settled text of a turn whose ending the owner could not confirm
const gatewayUnknownOutcome = "Icebath couldn't confirm how this turn ended (usually because it restarted mid-turn). " +
	"Resume this session and use the /discard <id> command it shows to drop the turn. " +
	"Don't send the same message again until you've checked."

// Control events that block the session FIFO rather than one admission output
var gatewayBlockingControlEvents = map[string]bool{"approval.request": true, "approval.settled": true,
	"clarify.request": true, "clarify.settled": true, "session.info": true}

// Owner connection the chat view talks to, raw RPC answers and the session event queue
type gatewayChatPeer interface {
	RPCTimeout(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error)
	EventQueue() *gateway.EventQueue
}

// One terminal safe line of a pending prompt, escapes removed, non printables folded, capped at the preview width
func gatewayPendingPreview(text any) string {
	s, ok := text.(string)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, r := range tools.StripANSI(s) {
		if pystr.IsPrintable(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	line := strings.Join(pystr.SplitWhitespace(b.String(), -1), " ")
	if pystr.Len(line) > gatewayPreviewChars {
		line = pystr.Slice(line, 0, gatewayPreviewChars-1) + "…"
	}
	return line
}

// Quoted indented prompt of a pending row, or the fallback naming its id when it carries no text
func gatewayQuoted(row map[string]any, fallback string) string {
	if preview := gatewayPendingPreview(row["text"]); preview != "" {
		return `    "` + preview + `"`
	}
	return fmt.Sprintf("    (%s %s)", fallback, pyrepr.Str(requireKey(row, "admission_id")))
}

// Pending admissions as shown on attach, lost turns with their discard command and queued prompts quoted
func gatewayPendingLines(pending []any, interactive bool) []string {
	var lost, queued []map[string]any
	for _, raw := range pending {
		row, _ := raw.(map[string]any)
		switch requireKey(row, "status") {
		case "unknown":
			lost = append(lost, row)
		case "queued":
			queued = append(queued, row)
		}
	}
	var lines []string
	if len(lost) == 1 {
		lines = append(lines, "⏸ This chat is paused. Icebath couldn't confirm how the last turn ended "+
			"(usually because it restarted mid-turn):")
		if gatewayPendingPreview(lost[0]["text"]) != "" {
			lines = append(lines, gatewayQuoted(lost[0], "lost turn"))
		}
		lines = append(lines, "  Icebath won't re-run it by itself, since it may already have done part of the work.")
		if interactive {
			lines = append(lines, "  To drop it and continue, type:", "/discard "+pyrepr.Str(requireKey(lost[0], "admission_id")))
		}
	} else if len(lost) > 0 {
		lines = append(lines, fmt.Sprintf("⏸ This chat is paused. Icebath couldn't confirm how %d turns ended "+
			"(usually because it restarted mid-turn):", len(lost)))
		lines = append(lines, "  Icebath won't re-run them by itself, since they may already have done part of the work.")
		if interactive {
			lines = append(lines, "  To continue, drop each one with the /discard line under it:")
		}
		for _, row := range lost {
			lines = append(lines, gatewayQuoted(row, "lost turn"))
			if interactive {
				lines = append(lines, "/discard "+pyrepr.Str(requireKey(row, "admission_id")))
			}
		}
	}
	if len(queued) > 0 {
		count := fmt.Sprintf("%d messages are", len(queued))
		if len(queued) == 1 {
			count = "1 message is"
		}
		after := "and will run once those turns are dropped"
		switch len(lost) {
		case 0:
			after = "to run"
		case 1:
			after = "and will run once that turn is dropped"
		}
		lines = append(lines, fmt.Sprintf("  %s waiting %s:", count, after))
		for _, row := range queued {
			lines = append(lines, gatewayQuoted(row, "queued message"))
		}
	}
	return lines
}

// Unknown pending row of the resume snapshot that /discard drops, refused when the id names none
func gatewayLostAdmission(snapshot map[string]any, admissionID string) (map[string]any, error) {
	pending, _ := snapshot["pending"].([]any)
	for _, raw := range pending {
		row, _ := raw.(map[string]any)
		if pyrepr.Equal(requireKey(row, "admission_id"), admissionID) && pyrepr.Equal(requireKey(row, "status"), "unknown") {
			return row, nil
		}
	}
	return nil, &gateway.GatewayClientError{Msg: "No unknown (lost) admission with that id; resume to refresh"}
}

// Set or clear signal waited on with the view lock released, the asyncio.Event of the view
type gatewayViewEvent struct {
	ch  chan struct{}
	set bool
}

func newGatewayViewEvent() *gatewayViewEvent { return &gatewayViewEvent{ch: make(chan struct{})} }

// Wakes every waiter and keeps the event set
func (e *gatewayViewEvent) Set() {
	if !e.set {
		e.set = true
		close(e.ch)
	}
}

// Arms the event again
func (e *gatewayViewEvent) Clear() {
	if e.set {
		e.set = false
		e.ch = make(chan struct{})
	}
}

// Admission scoped event held until the submit receipt names the one this invocation owns
type gatewayHeldEvent struct {
	kind      string
	admission any
	payload   map[string]any
}

// Classic terminal view of one gateway session, its event rendering and its fenced controls
type GatewayChatView struct {
	// mu is the event loop, held while view code runs and released at each wait
	mu sync.Mutex

	usageFile       string
	client          gatewayChatPeer
	sessionID       string
	generation      any
	prompts         *pydict.Dict[any, map[string]any]
	pending         []any
	model           string
	emitter         *StreamJSONEmitter
	quiet           bool
	finite          bool
	unattended      bool
	resumeFooter    bool
	launchAPIKey    string
	finiteAdmission any
	finiteEvents    []gatewayHeldEvent
	streams         *pydict.Dict[any, string]
	completions     *pydict.Dict[any, map[string]any]
	cut             *pydict.Dict[any, string]
	mine            *pydict.Dict[any, struct{}]
	gaps            int
	attaching       chan struct{}
	changed         *gatewayViewEvent
	failure         error
	failed          *gatewayViewEvent
	mutations       *PreparedMutations
	composer        *ptPromptSession
	prefill         string
	out             io.Writer
	errOut          io.Writer
}

// Display name of a model id, its last path segment
func gatewayModelName(v any, fallback string) string {
	if !pyrepr.Truthy(v) {
		return fallback
	}
	parts := strings.Split(pyrepr.Str(v), "/")
	return parts[len(parts)-1]
}

// Prompts of a snapshot keyed by prompt id in snapshot order
func gatewaySnapshotPrompts(snapshot map[string]any) *pydict.Dict[any, map[string]any] {
	prompts := pydict.New[any, map[string]any]()
	list, _ := snapshot["prompts"].([]any)
	for _, raw := range list {
		p, _ := raw.(map[string]any)
		prompts.Set(requireKey(p, "prompt_id"), p)
	}
	return prompts
}

// Value under key, the fallback when the key is absent
func gatewayGetOr(m map[string]any, key string, fallback any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return fallback
}

// View attached to the snapshot, stdout owned by the stream json protocol when an emitter is given
func newGatewayChatView(client gatewayChatPeer, snapshot map[string]any, quiet bool, emitter *StreamJSONEmitter, usageFile string) *GatewayChatView {
	info, _ := snapshot["info"].(map[string]any)
	pending, _ := gatewayGetOr(snapshot, "pending", []any{}).([]any)
	return &GatewayChatView{
		usageFile:   usageFile,
		client:      client,
		sessionID:   pyrepr.Str(requireKey(snapshot, "stored_session_id")),
		generation:  gatewayGetOr(snapshot, "execution_generation", int64(0)),
		prompts:     gatewaySnapshotPrompts(snapshot),
		pending:     pending,
		model:       gatewayModelName(info["model"], "Icebath"),
		emitter:     emitter,
		quiet:       quiet || emitter != nil,
		streams:     pydict.New[any, string](),
		completions: pydict.New[any, map[string]any](),
		cut:         pydict.New[any, string](),
		mine:        pydict.New[any, struct{}](),
		attaching:   make(chan struct{}, 1),
		changed:     newGatewayViewEvent(),
		failed:      newGatewayViewEvent(),
		mutations:   NewPreparedMutations(),
		out:         console().Out,
		errOut:      stderrWriter(),
	}
}

// Runs fn with the view lock released, the await of the view coroutine
func (v *GatewayChatView) await(fn func()) {
	v.mu.Unlock()
	defer v.mu.Lock()
	fn()
}

// One request and its raw answer with the view lock released
func (v *GatewayChatView) rpcRaw(ctx context.Context, method string, params map[string]any, timeout time.Duration) (any, error) {
	var result any
	var err error
	v.await(func() { result, err = v.client.RPCTimeout(ctx, method, params, timeout) })
	return result, err
}

// One request answered as a plain map, nil for a null answer
func (v *GatewayChatView) rpc(ctx context.Context, method string, params map[string]any, timeout time.Duration) (map[string]any, error) {
	raw, err := v.rpcRaw(ctx, method, params, timeout)
	if err != nil {
		return nil, err
	}
	return gatewayAnswerMap(raw)
}

// Plain map of a decoded answer, nil for null and the AttributeError a non mapping answer raises
func gatewayAnswerMap(raw any) (map[string]any, error) {
	if raw == nil {
		return nil, nil
	}
	if m, ok := pyjson.Plain(raw).(map[string]any); ok {
		return m, nil
	}
	return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(pyjson.Plain(raw)))
}

// Waits for the event with the view lock released
func (v *GatewayChatView) wait(ctx context.Context, e *gatewayViewEvent) error {
	if e.set {
		return nil
	}
	ch := e.ch
	var err error
	v.await(func() {
		select {
		case <-ch:
		case <-ctx.Done():
			err = ctx.Err()
		}
	})
	return err
}

// Takes the attach lock with the view lock released
func (v *GatewayChatView) lockAttaching(ctx context.Context) error {
	var err error
	v.await(func() {
		select {
		case v.attaching <- struct{}{}:
		case <-ctx.Done():
			err = ctx.Err()
		}
	})
	return err
}

func (v *GatewayChatView) unlockAttaching() { <-v.attaching }

// Writes one printed line to the writer
func (v *GatewayChatView) println(w io.Writer, text string) { _, _ = io.WriteString(w, text+"\n") }

// Translated catalog string of the active language
func (v *GatewayChatView) t(ctx context.Context, key string) string { return i18n.T(ctx, key, "", nil) }

// Admission ids of the lost turns in the pending rows
func (v *GatewayChatView) unknownAdmissions() []any {
	var out []any
	for _, raw := range v.pending {
		row, _ := raw.(map[string]any)
		if pyrepr.Equal(requireKey(row, "status"), "unknown") {
			out = append(out, requireKey(row, "admission_id"))
		}
	}
	return out
}

// Prints the pending admissions on stderr, the discard commands only when interactive
func (v *GatewayChatView) showPending() {
	for _, line := range gatewayPendingLines(v.pending, !v.finite) {
		v.println(v.errOut, line)
	}
}

// Prints a pending approval or clarify prompt with the command that answers it
func (v *GatewayChatView) showPrompt(prompt map[string]any) {
	title := "Approval required"
	if pyrepr.Truthy(prompt["description"]) {
		title = pyrepr.Str(prompt["description"])
	} else if pyrepr.Truthy(prompt["question"]) {
		title = pyrepr.Str(prompt["question"])
	}
	v.println(v.errOut, "\n"+title)
	if pyrepr.Truthy(prompt["command"]) {
		v.println(v.errOut, pyrepr.Str(prompt["command"]))
	}
	command := "/answer"
	if pyrepr.Equal(requireKey(prompt, "kind"), "approval") {
		command = "/approve"
	}
	var choices []string
	list, _ := gatewayGetOr(prompt, "choices", []any{}).([]any)
	for _, c := range list {
		choices = append(choices, pyrepr.Str(c))
	}
	joined := strings.Join(choices, "|")
	if joined == "" {
		joined = "answer"
	}
	v.println(v.errOut, fmt.Sprintf("%s %s <%s>", command, pyrepr.Str(requireKey(prompt, "prompt_id")), joined))
}

// Renders owner events of this session until the stream fails or the context ends
func (v *GatewayChatView) render(ctx context.Context) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for {
		var event any
		var err error
		v.await(func() { event, err = v.client.EventQueue().Get(ctx) })
		if err != nil {
			return
		}
		if failure, ok := event.(error); ok {
			v.fail(failure)
			return
		}
		frame, _ := pyjson.Plain(event).(map[string]any)
		params, _ := gatewayGetOr(frame, "params", map[string]any{}).(map[string]any)
		if !pyrepr.Equal(params["session_id"], v.sessionID) {
			continue
		}
		kind, _ := params["type"].(string)
		payload, _ := gatewayGetOr(params, "payload", map[string]any{}).(map[string]any)
		v.generation = gatewayGetOr(params, "execution_generation", v.generation)
		if kind == "session.replay_gap" {
			if v.reattach(ctx) != nil {
				return
			}
			if v.failure != nil {
				return
			}
			continue
		}
		admission := params["admission_id"]
		if !pyrepr.Truthy(admission) {
			admission = payload["admission_id"]
		}
		if v.finite && gatewayBlockingControlEvents[kind] {
			v.dispatchEvent(ctx, kind, admission, payload)
			v.changed.Set()
			continue
		}
		if v.finite && pyrepr.Truthy(admission) {
			if v.finiteAdmission == nil {
				v.finiteEvents = append(v.finiteEvents, gatewayHeldEvent{kind, admission, payload})
				continue
			}
			if !pyrepr.Equal(admission, v.finiteAdmission) {
				continue
			}
		}
		v.dispatchEvent(ctx, kind, admission, payload)
		v.changed.Set()
	}
}

// Records the failure that ends the view and wakes every waiter
func (v *GatewayChatView) fail(err error) {
	v.failure = err
	v.failed.Set()
	v.changed.Set()
}

// Re-attaches after the owner retired this subscription, shows hidden controls and recovers turns in admission order
func (v *GatewayChatView) reattach(ctx context.Context) error {
	sid := v.sessionID
	if err := v.lockAttaching(ctx); err != nil {
		return err
	}
	defer v.unlockAttaching()
	v.gaps++
	snapshot, err := v.rpc(ctx, "session.resume", map[string]any{"session_id": sid}, 0)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if v.sessionID == sid {
			v.fail(&gateway.GatewayClientError{Msg: "session_replay_gap"})
		}
		return nil
	}
	if v.sessionID != sid {
		return nil
	}
	v.generation = gatewayGetOr(snapshot, "execution_generation", v.generation)
	if pending, ok := snapshot["pending"]; ok {
		v.pending, _ = pending.([]any)
	}
	prompts := gatewaySnapshotPrompts(snapshot)
	if !v.finite {
		for _, id := range prompts.Keys() {
			if !v.prompts.Has(id) {
				p, _ := prompts.Get(id)
				v.showPrompt(p)
			}
		}
	}
	v.prompts = prompts
	order := pydict.New[any, struct{}]()
	for _, k := range v.streams.Keys() {
		order.Set(k, struct{}{})
	}
	for _, k := range v.mine.Keys() {
		order.Set(k, struct{}{})
	}
	for _, admission := range order.Keys() {
		if err := v.recover(ctx, admission, sid); err != nil {
			return err
		}
	}
	v.changed.Set()
	return nil
}

// Settles an admission the replay gap hid, from the snapshot status or its receipt
func (v *GatewayChatView) recover(ctx context.Context, admission any, sid string) error {
	status := replayGapPendingStatus(v.pending, admission)
	if pyrepr.Equal(status, "unknown") && v.mine.Has(admission) {
		v.complete(ctx, admission, map[string]any{"outcome": "unknown", "text": gatewayUnknownOutcome})
		v.changed.Set()
	}
	if !pyrepr.Truthy(admission) || pyrepr.Equal(status, "queued") || pyrepr.Equal(status, "unknown") || v.completions.Has(admission) {
		return nil
	}
	streamed, _ := v.streams.Get(admission)
	v.streams.Delete(admission)
	if !v.cut.Has(admission) {
		v.cut.Set(admission, streamed)
	}
	if pyrepr.Truthy(status) {
		return nil
	}
	for _, held := range v.finiteEvents {
		if held.kind == "message.complete" && pyrepr.Equal(held.admission, admission) {
			return nil
		}
	}
	raw, err := v.rpcRaw(ctx, "prompt.receipt", map[string]any{"session_id": sid, "admission_id": admission, "include_result": true}, 0)
	var receipt map[string]any
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw, receipt = pyjson.ObjectOf("status", "terminal"), map[string]any{"status": "terminal"}
	} else if receipt, err = gatewayAnswerMap(raw); err != nil {
		return err
	} else if receipt == nil {
		return fmt.Errorf("AttributeError: 'NoneType' object has no attribute 'get'")
	}
	var verdict *bool
	no := false
	if v.sessionID == sid && !v.completions.Has(admission) {
		verdict = replayGapSettled(receipt)
	} else {
		verdict = &no
	}
	switch {
	case verdict != nil && *verdict:
		outcome := receipt["outcome"]
		if pyrepr.Equal(outcome, "interrupted") {
			outcome = "cancelled"
		}
		result, _ := receipt["result"].(map[string]any)
		text := ""
		if pyrepr.Truthy(result["final_response"]) {
			text = pyrepr.Str(result["final_response"])
		}
		v.complete(ctx, admission, map[string]any{"outcome": outcome, "_receipt": raw, "text": text})
		v.changed.Set()
	case verdict == nil && v.finite:
		v.fail(&gateway.GatewayClientError{Msg: "session_replay_gap"})
	}
	return nil
}

// Next composer line, empty once the event stream failed first with the pending read cancelled
func (v *GatewayChatView) readLine(ctx context.Context, prompt *ptPromptSession, symbol string) (string, error) {
	def := v.prefill
	v.prefill = ""
	type line struct {
		text string
		err  error
	}
	reader := make(chan line, 1)
	go func() {
		text, err := prompt.prompt(symbol, def)
		reader <- line{text, err}
	}()
	failed := v.failed.ch
	var got line
	done, cancelled := false, false
	v.await(func() {
		select {
		case got = <-reader:
			done = true
			return
		case <-failed:
		case <-ctx.Done():
			cancelled = true
		}
		select {
		case got = <-reader:
			done = true
			return
		default:
		}
		prompt.cancel()
		<-reader
	})
	if done {
		return got.text, got.err
	}
	if cancelled {
		return "", ctx.Err()
	}
	return "", nil
}

// Routes one owner event to its handler
func (v *GatewayChatView) dispatchEvent(ctx context.Context, kind string, admission any, payload map[string]any) {
	switch kind {
	case "message.delta":
		v.delta(admission, payload)
	case "message.complete":
		v.complete(ctx, admission, payload)
	case "tool.start":
		v.toolStart(admission, payload)
	case "tool.complete":
		v.toolComplete(admission, payload)
	case "approval.request", "clarify.request":
		v.request(admission, payload)
	case "approval.settled", "clarify.settled":
		v.settled(admission, payload)
	case "session.info":
		v.sessionInfo(admission, payload)
	case "status.update", "notification.show":
		v.notice(admission, payload)
	}
}

// Pending rows and generation the owner published
func (v *GatewayChatView) sessionInfo(admission any, payload map[string]any) {
	if pending, ok := payload["pending"]; ok {
		v.pending, _ = pending.([]any)
	}
	v.generation = gatewayGetOr(payload, "execution_generation", v.generation)
}

// Prints an owner notice on stderr unless quiet
func (v *GatewayChatView) notice(admission any, payload map[string]any) {
	text := ""
	if pyrepr.Truthy(payload["text"]) {
		text = pyrepr.Str(payload["text"])
	}
	if !v.quiet && pystr.Strip(text) != "" {
		v.println(v.errOut, pystr.Strip(text))
	}
}

// Text of a payload value that is a non empty string or other truthy value
func gatewayText(values ...any) any {
	for _, value := range values {
		if pyrepr.Truthy(value) {
			return value
		}
	}
	return ""
}

// Closes the interim segment before a tool call and shows the tool line
func (v *GatewayChatView) toolStart(admission any, payload map[string]any) {
	if v.cut.Has(admission) {
		v.cut.Set(admission, "")
	}
	streamed, had := v.streams.Get(admission)
	v.streams.Delete(admission)
	if had && streamed != "" && !v.quiet {
		v.println(v.out, "")
	}
	if v.emitter != nil {
		args, _ := pyjson.Plain(payload["args"]).(map[string]any)
		extra := map[string]any{}
		if pyrepr.Truthy(payload["tool_call_id"]) {
			extra["tool_call_id"] = payload["tool_call_id"]
		}
		v.emitter.OnToolProgress("tool.started", gatewayOptionalStr(payload["tool_name"]), "", args, extra)
	} else if !v.quiet {
		name := pyrepr.Str(gatewayText(payload["tool_name"], payload["name"], "tool"))
		args, _ := pyjson.Plain(payload["args"]).(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		zero := 0
		env := bannerToolEnv(context.Background())
		preview := tools.BuildToolPreview(env, name, args, &zero)
		line := tools.GetToolEmoji(env, name, "⚡") + " " + name
		if preview != "" {
			line += ": " + preview
		}
		v.println(v.out, line)
	}
}

// String of an optional payload value, empty for None
func gatewayOptionalStr(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

// Emits the tool result event in stream json mode
func (v *GatewayChatView) toolComplete(admission any, payload map[string]any) {
	if v.emitter == nil {
		return
	}
	args, _ := pyjson.Plain(payload["args"]).(map[string]any)
	extra := map[string]any{"result": payload["result"], "is_error": gatewayGetOr(payload, "is_error", false)}
	if pyrepr.Truthy(payload["tool_call_id"]) {
		extra["tool_call_id"] = payload["tool_call_id"]
	}
	v.emitter.OnToolProgress("tool.completed", gatewayOptionalStr(payload["tool_name"]), "", args, extra)
}

// Streams reply text unless the admission was cut at a replay gap
func (v *GatewayChatView) delta(admission any, payload map[string]any) {
	text, ok := gatewayText(payload["text"], payload["delta"], payload["content"]).(string)
	if !ok || v.cut.Has(admission) {
		return
	}
	if v.emitter != nil {
		v.emitter.OnTextDelta(text)
	} else if !v.quiet {
		streamed, _ := v.streams.Get(admission)
		v.streams.Set(admission, streamed+text)
		_, _ = io.WriteString(v.out, text)
	}
}

// Settles an admission, printing what is owed of its reply and the interrupt cause
func (v *GatewayChatView) complete(ctx context.Context, admission any, payload map[string]any) {
	text := pyrepr.Str(gatewayText(payload["text"], payload["content"]))
	streamed, _ := v.streams.Get(admission)
	v.streams.Delete(admission)
	cut, wasCut := v.cut.Get(admission)
	v.cut.Delete(admission)
	v.mine.Delete(admission)
	if wasCut {
		merged := make(map[string]any, len(payload)+1)
		for k, val := range payload {
			merged[k] = val
		}
		merged["replay_gap"] = true
		payload = merged
		if !v.quiet && text != "" {
			v.println(v.out, replayGapOwedReply(cut, text))
		}
	} else if !v.quiet {
		switch {
		case streamed == "":
			v.println(v.out, text)
		case strings.HasPrefix(text, streamed):
			v.println(v.out, text[len(streamed):])
		case pystr.Strip(text) == pystr.Strip(streamed):
			v.println(v.out, "")
		default:
			v.println(v.out, "\n"+text)
		}
	}
	warning, isStr := payload["warning"].(string)
	if pyrepr.Equal(payload["status"], "interrupted") && isStr && pystr.Strip(warning) != "" {
		v.println(v.errOut, pystr.Strip(warning))
	}
	v.completions.Set(admission, payload)
}

// Records a pending control, shown only to an interactive viewer
func (v *GatewayChatView) request(admission any, payload map[string]any) {
	v.prompts.Set(requireKey(payload, "prompt_id"), payload)
	if !v.finite {
		v.showPrompt(payload)
	}
}

// Forgets a settled control
func (v *GatewayChatView) settled(admission any, payload map[string]any) {
	v.prompts.Delete(requireKey(payload, "prompt_id"))
}

// Submits a prompt, retiring retained controls and recovering it when a re-attach raced the receipt
func (v *GatewayChatView) submit(ctx context.Context, text string) (map[string]any, error) {
	v.mutations.Retire(v.sessionID, nil)
	sid, gaps := v.sessionID, v.gaps
	params := map[string]any{"session_id": v.sessionID, "input_id": uuidHex(), "text": text}
	if v.finite {
		params["finite"] = true
		if v.unattended {
			params["unattended"] = true
		}
	}
	receipt, err := v.rpc(ctx, "prompt.submit", params, 0)
	if err != nil {
		return nil, err
	}
	if receipt["voice_stopped"] == true {
		if !v.quiet {
			v.println(v.out, v.t(ctx, "cli.voice.stop_phrase_typed"))
		}
		return receipt, nil
	}
	admission := receipt["admission_id"]
	if pyrepr.Truthy(admission) && !v.completions.Has(admission) && v.sessionID == sid {
		v.mine.Set(admission, struct{}{})
		if v.gaps != gaps {
			if err := v.lockAttaching(ctx); err != nil {
				return nil, err
			}
			defer v.unlockAttaching()
			if !v.cut.Has(admission) && v.failure == nil {
				if err := v.recover(ctx, admission, sid); err != nil {
					return nil, err
				}
			}
		}
	}
	return receipt, nil
}

// Runs one slash command line, false when it detaches the view
func (v *GatewayChatView) command(ctx context.Context, text string) (bool, error) {
	command, rest, _ := strings.Cut(text, " ")
	switch command {
	case "/quit", "/exit", "/detach":
		return false, nil
	case "/stop":
		_, err := v.rpc(ctx, "session.interrupt", map[string]any{"session_id": v.sessionID, "execution_generation": v.generation}, 0)
		return true, err
	case "/approve", "/answer":
		promptID, answer, _ := strings.Cut(rest, " ")
		prompt, ok := v.prompts.Get(promptID)
		expected := "clarify"
		if command == "/approve" {
			expected = "approval"
		}
		if !ok || prompt == nil || !pyrepr.Equal(requireKey(prompt, "kind"), expected) {
			return false, &gateway.GatewayClientError{Msg: "No matching pending control; resume to refresh"}
		}
		params := map[string]any{"session_id": v.sessionID, "execution_generation": requireKey(prompt, "execution_generation"), "prompt_id": promptID}
		if expected == "approval" {
			params["choice"] = answer
		} else {
			params["answer"] = answer
		}
		_, err := v.rpc(ctx, expected+".respond", params, 0)
		return true, err
	case "/discard":
		snapshot, err := v.rpc(ctx, "session.resume", map[string]any{"session_id": v.sessionID}, 0)
		if err != nil {
			return false, err
		}
		lost, err := gatewayLostAdmission(snapshot, pystr.Strip(rest))
		if err != nil {
			return false, err
		}
		_, err = v.rpc(ctx, "prompt.resolve_unknown", map[string]any{"session_id": v.sessionID,
			"admission_id": requireKey(lost, "admission_id"), "execution_generation": requireKey(lost, "execution_generation")}, 0)
		return true, err
	case "/branch", "/model", "/compress":
		return v.mutationCommand(ctx, command, rest)
	case "/yolo":
		word := pystr.Lower(pystr.Strip(rest))
		if word != "" && word != "on" && word != "off" {
			return false, &gateway.GatewayClientError{Msg: "Usage: /yolo [on|off]"}
		}
		params := map[string]any{"session_id": v.sessionID, "key": "yolo"}
		if word != "" {
			params["value"] = "0"
			if word == "on" {
				params["value"] = "1"
			}
		}
		result, err := v.rpc(ctx, "config.set", params, 0)
		if err != nil {
			return false, err
		}
		state := "off"
		if pyrepr.Equal(result["value"], "1") {
			state = "on"
		}
		v.println(v.out, fmt.Sprintf("YOLO %s for this session", state))
		return true, nil
	case "/help":
		v.println(v.out, gatewayChatHelpText())
		return true, nil
	}
	return gatewayRunCommand(ctx, v, command, rest)
}

// Applies a /branch, /model or /compress mutation and attaches to its result
func (v *GatewayChatView) mutationCommand(ctx context.Context, command, rest string) (bool, error) {
	operation, payload, err := slashMutation(command, pystr.Strip(rest))
	if err != nil {
		return false, err
	}
	original := v.sessionID
	var confirm func(ctx context.Context, refusal map[string]any) (bool, error)
	if operation == "model" && v.composer != nil {
		confirm = v.confirmModelSwitch
	}
	result, err := v.mutations.Apply(ctx, v.rpc, original, operation, payload, confirm)
	if err != nil {
		return false, err
	}
	switch {
	case pyrepr.Equal(result["status"], "cancelled"):
		return true, nil
	case pyrepr.Equal(result["status"], "preview"):
		v.mutations.Acknowledge(original, operation, payload)
		var lines []string
		list, _ := requireKey(result, "lines").([]any)
		for _, l := range list {
			lines = append(lines, pyrepr.Str(l))
		}
		v.println(v.out, strings.Join(lines, "\n"))
		return true, nil
	case pyrepr.Equal(result["status"], "noop"):
		v.mutations.Acknowledge(original, operation, payload)
		summary, _ := result["summary"].(map[string]any)
		if !pyrepr.Truthy(result["summary"]) {
			summary = map[string]any{"note": result["note"]}
		}
		var lines []string
		for _, key := range []string{"headline", "token_line", "note"} {
			if pyrepr.Truthy(summary[key]) {
				lines = append(lines, pyrepr.Str(summary[key]))
			}
		}
		v.println(v.out, strings.Join(lines, "\n"))
		return true, nil
	}
	target := pyrepr.Str(gatewayGetOr(result, "branched_session_id", original))
	snapshot, err := v.rpc(ctx, "session.resume", map[string]any{"session_id": target}, 0)
	if err != nil {
		return false, err
	}
	if err := v.adopt(ctx, snapshot); err != nil {
		return false, err
	}
	v.mutations.Acknowledge(original, operation, payload)
	if operation == "model" && pyrepr.Truthy(result["model"]) {
		v.model = gatewayModelName(result["model"], v.model)
	}
	v.println(v.out, fmt.Sprintf("%s: %s", operation, target))
	return true, nil
}

// Attaches the view to another session snapshot, moving this terminal continue pointer when interactive
func (v *GatewayChatView) adopt(ctx context.Context, snapshot map[string]any) error {
	v.sessionID = pyrepr.Str(requireKey(snapshot, "stored_session_id"))
	v.generation = requireKey(snapshot, "execution_generation")
	v.prompts = gatewaySnapshotPrompts(snapshot)
	v.pending, _ = gatewayGetOr(snapshot, "pending", []any{}).([]any)
	info, _ := snapshot["info"].(map[string]any)
	v.model = gatewayModelName(gatewayText(info["model"], v.model), v.model)
	if !v.finite {
		sid := v.sessionID
		v.await(func() { WriteBreadcrumb(ctx, sid, "") })
	}
	return nil
}

// Asks on the composer whether to switch to a guarded model anyway, only an explicit yes applying
func (v *GatewayChatView) confirmModelSwitch(ctx context.Context, refusal map[string]any) (bool, error) {
	t := func(key string) string { return v.t(ctx, key) }
	choices := [][3]string{{"once", t("cli.model.choice_switch_anyway"), t("cli.model.desc_switch_anyway")},
		{"cancel", t("cli.model.choice_cancel"), t("cli.model.desc_keep_current_model")}}
	v.println(v.out, fmt.Sprintf("\n!!! %s !!!\n%s\n", confirmationTitle(refusal), pyrepr.Str(requireKey(refusal, "confirm_message"))))
	for i, choice := range choices {
		v.println(v.out, fmt.Sprintf("  %d. %s — %s", i+1, choice[1], choice[2]))
	}
	raw, err := v.readLine(ctx, v.composer, t("cli.model.confirm_choice_prompt"))
	if errors.Is(err, errKeyboardInterrupt) || errors.Is(err, io.EOF) {
		raw, err = "", nil
	}
	if err != nil {
		return false, err
	}
	if v.failure == nil {
		choice, err := confirmChoice(raw, choices)
		if err != nil {
			return false, err
		}
		if choice == "once" {
			return true, nil
		}
	}
	v.println(v.out, t("cli.model.switch_cancelled"))
	return false, nil
}

// Says why a one shot cannot go on and exits 3, closing stream json with a failed result
func (v *GatewayChatView) detach(message string) int {
	v.println(v.errOut, message)
	if v.emitter != nil {
		return v.emitter.EmitResult(map[string]any{"failed": true, "error": message}, v.sessionID, 3)
	}
	return 3
}

// Why a finite viewer cannot keep waiting for the admission, empty when it can
func (v *GatewayChatView) finiteBlock(admission any) string {
	var own any
	for _, raw := range v.pending {
		row, _ := raw.(map[string]any)
		if pyrepr.Equal(requireKey(row, "admission_id"), admission) {
			own = requireKey(row, "status")
			break
		}
	}
	if pyrepr.Equal(own, "unknown") {
		v.completions.Set(admission, map[string]any{"outcome": "unknown", "text": gatewayUnknownOutcome})
		return ""
	}
	if pyrepr.Equal(own, "queued") && len(v.unknownAdmissions()) > 0 {
		return "This chat is paused behind a turn whose ending Icebath couldn't confirm. " +
			"Your message is saved and will run once you resume this session and /discard " +
			"that turn; don't send it again."
	}
	if v.prompts.Len() > 0 && (pyrepr.Truthy(own) || v.gaps == 0) {
		return "Input required; detached without cancelling. Resume this session interactively."
	}
	return ""
}

// Structured result the owner committed with the admission settlement, empty when unreadable
func (v *GatewayChatView) settledResult(ctx context.Context, admission any) (*pyjson.Object, error) {
	raw, err := v.rpcRaw(ctx, "prompt.receipt", map[string]any{"session_id": v.sessionID, "admission_id": admission, "include_result": true}, 0)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Debug(fmt.Sprintf("settled result for %s unavailable: %v", pyrepr.Str(admission), err))
		return pyjson.NewObject(), nil
	}
	receipt, ok := raw.(*pyjson.Object)
	if !ok {
		slog.Debug(fmt.Sprintf("settled result for %s unavailable: receipt is not a mapping", pyrepr.Str(admission)))
		return pyjson.NewObject(), nil
	}
	return gatewayResultCopy(receipt.Vals["result"]), nil
}

// Copy of a committed result mapping, empty for a missing or falsy one
func gatewayResultCopy(result any) *pyjson.Object {
	out := pyjson.NewObject()
	if obj, ok := result.(*pyjson.Object); ok {
		for _, key := range obj.Keys {
			out.Set(key, obj.Vals[key])
		}
	} else if m, ok := result.(map[string]any); ok {
		for _, key := range sortedMapKeys(m) {
			out.Set(key, m[key])
		}
	}
	return out
}

// Prints the durable session id on stderr and, for a chat query, the resume block
func (v *GatewayChatView) printExitIDs(ctx context.Context) {
	if !v.unattended {
		v.println(v.errOut, "\nsession_id: "+v.sessionID)
	}
	if v.resumeFooter {
		v.println(v.out, gatewayResumeFooter(ctx, v.sessionID))
	}
}

// Says on stderr that a finite turn was interrupted with no reply and never retried
func (v *GatewayChatView) reportSilentInterrupt(text string, result map[string]any) {
	if text != "" || !pyrepr.Truthy(result["interrupted"]) {
		return
	}
	reason := ""
	if s, ok := result["interrupt_message"].(string); ok && s != "" {
		reason = " (" + s + ")"
	}
	v.println(v.errOut, "Turn interrupted"+reason+"; it was not retried. Resume this session to continue.")
}

// The one shot stderr line for a completed turn with no reply text
func (v *GatewayChatView) reportNoReply(exitCode int) {
	if v.unattended && exitCode == 1 {
		v.println(v.errOut, "icebath -z: no final response was produced; treating the run as failed.")
	}
}

// Writes the usage file ledger of the settled turn
func (v *GatewayChatView) writeUsageFile(result *pyjson.Object, outcome any) {
	report := gatewayResultCopy(result)
	sessionID := report.Vals["session_id"]
	if !pyrepr.Truthy(sessionID) {
		sessionID = v.sessionID
	}
	report.Set("session_id", sessionID)
	var failure any
	if !pyrepr.Equal(outcome, "completed") && !pyrepr.Equal(outcome, "cancelled") {
		failure = gatewayText(report.Vals["error"], outcome, "failed")
	}
	oneshotWriteUsageFile(v.usageFile, report, failure)
}

// Shows pending state, submits the query and either waits for its settlement or runs the interactive composer
func (v *GatewayChatView) Run(ctx context.Context, query string, oneshot bool) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.quiet = v.quiet || oneshot
	v.finite = oneshot
	v.showPending()
	for _, prompt := range v.prompts.Values() {
		v.showPrompt(prompt)
	}
	if oneshot && len(v.unknownAdmissions()) > 0 {
		return v.detach(fmt.Sprintf("Your message was not sent. To continue, run `icebath sessions discard "+
			"%s --yes`, or resume this session in an interactive chat "+
			"and /discard the paused turn there; then send it again.", v.sessionID)), nil
	}
	renderCtx, stopRender := context.WithCancel(ctx)
	rendered := make(chan struct{})
	go func() {
		defer close(rendered)
		v.render(renderCtx)
	}()
	defer func() {
		stopRender()
		v.await(func() { <-rendered })
	}()
	var receipt map[string]any
	if query != "" {
		var err error
		if receipt, err = v.submit(ctx, query); err != nil {
			return 0, err
		}
	}
	if oneshot {
		return v.runFinite(ctx, receipt)
	}
	return v.runInteractive(ctx)
}

// Waits for the one shot admission to settle and reports it under the in process exit contracts
func (v *GatewayChatView) runFinite(ctx context.Context, receipt map[string]any) (int, error) {
	admission, err := gatewayOneshotAdmission(receipt)
	if err != nil {
		return 0, err
	}
	if admission == nil {
		return 0, nil
	}
	v.finiteAdmission = admission
	for _, held := range v.finiteEvents {
		if pyrepr.Equal(held.admission, admission) && !v.completions.Has(admission) {
			v.dispatchEvent(ctx, held.kind, held.admission, held.payload)
		}
	}
	v.finiteEvents = nil
	v.changed.Set()
	for !v.completions.Has(admission) {
		v.changed.Clear()
		if v.failure != nil {
			return 0, v.failure
		}
		if blocked := v.finiteBlock(admission); blocked != "" {
			return v.detach(blocked), nil
		}
		if !v.completions.Has(admission) {
			if err := v.wait(ctx, v.changed); err != nil {
				return 0, err
			}
		}
	}
	terminal, _ := v.completions.Get(admission)
	outcome := terminal["outcome"]
	text := pyrepr.Str(gatewayText(terminal["text"], terminal["content"]))
	var result *pyjson.Object
	if recovered, ok := terminal["_receipt"].(*pyjson.Object); ok && recovered != nil {
		result = gatewayResultCopy(recovered.Vals["result"])
	} else {
		var err error
		if result, err = v.settledResult(ctx, admission); err != nil {
			return 0, err
		}
	}
	result.Set("failed", pyrepr.Truthy(result.Vals["failed"]) || (!pyrepr.Equal(outcome, "completed") && !pyrepr.Equal(outcome, "cancelled")))
	result.Set("interrupted", pyrepr.Truthy(result.Vals["interrupted"]) || pyrepr.Equal(outcome, "cancelled"))
	plain, _ := pyjson.Plain(result).(map[string]any)
	var exitCode int
	if v.unattended {
		exitCode = oneshotExitCode(text, plain)
	} else {
		exitCode = TurnExitCode(plain, TurnExitOptions{})
	}
	if v.usageFile != "" {
		v.writeUsageFile(result, outcome)
	}
	if v.emitter != nil {
		final := make(map[string]any, len(plain)+2)
		for k, val := range plain {
			final[k] = val
		}
		final["final_response"] = text
		final["replay_gap"] = pyrepr.Truthy(terminal["replay_gap"])
		return v.emitter.EmitResult(final, v.sessionID, exitCode), nil
	}
	v.reportSilentInterrupt(text, plain)
	v.println(v.out, text)
	v.reportNoReply(exitCode)
	v.printExitIDs(ctx)
	return exitCode, nil
}

// The interactive composer loop under a patched stdout until the user detaches or the stream fails
func (v *GatewayChatView) runInteractive(ctx context.Context) (int, error) {
	welcome := "Welcome to Icebath Agent! Type your message or /help for commands."
	v.println(v.out, config.GetActiveSkin(ctx).GetBranding("welcome", welcome))
	prompt := newPTPromptSession(tuiStdin(), tuiStdout(), func() string {
		v.mu.Lock()
		defer v.mu.Unlock()
		return fmt.Sprintf(" ☤ %s │ %s ", v.model, v.sessionID)
	})
	symbol := config.GetActivePromptSymbol(ctx, "❯ ")
	if xterm.IsTerminal(tuiStdin().Fd()) {
		v.composer = prompt
	} else {
		v.composer = nil
	}
	proxy := prompt.patchStdout(v.out)
	out, errOut := v.out, v.errOut
	v.out, v.errOut = proxy, proxy
	defer func() {
		proxy.Flush()
		v.out, v.errOut = out, errOut
	}()
	for v.failure == nil {
		line, err := v.readLine(ctx, prompt, symbol)
		if err == nil {
			text := pystr.Strip(line)
			if text == "" {
				continue
			}
			if strings.HasPrefix(text, "/") {
				var keep bool
				if keep, err = v.command(ctx, text); err == nil && !keep {
					return 0, nil
				}
			} else {
				v.println(v.out, fmt.Sprintf("\n%s\n● %s", strings.Repeat("─", 40), text))
				_, err = v.submit(ctx, text)
			}
		}
		var refused *gateway.GatewayClientError
		switch {
		case err == nil:
		case errors.Is(err, errKeyboardInterrupt):
			v.println(v.out, "Use /stop to interrupt execution, /quit to detach.")
		case errors.Is(err, io.EOF):
			return 0, nil
		case errors.As(err, &refused) && ctx.Err() == nil:
			v.println(v.errOut, "Error: "+err.Error())
		default:
			return 0, err
		}
	}
	return 0, v.failure
}

// The one shot admission id, nil when a bare stop phrase ended the voice chat and nothing was admitted
func gatewayOneshotAdmission(receipt map[string]any) (any, error) {
	if receipt == nil {
		return nil, &gateway.GatewayClientError{Msg: "One-shot requires a query"}
	}
	if receipt["voice_stopped"] == true {
		return nil, nil
	}
	return requireKey(receipt, "admission_id"), nil
}

// The classic single query exit block naming how to continue this session, the profile flag when not default
func gatewayResumeFooter(ctx context.Context, sessionID string) string {
	profile := profiles.GetActiveProfileName(ctx)
	flag := ""
	if profile != "default" && profile != "custom" {
		flag = " -p " + profile
	}
	return fmt.Sprintf("\n%s\n  icebath --resume %s%s", i18n.T(ctx, "cli.session.exit_resume_hint", "", nil), sessionID, flag)
}

// Whether a snapshot message row is a micro compaction merge shown by its original prompts instead
func gatewayModelOnlyRow(row map[string]any) bool {
	metadata, ok := row["display_metadata"].(map[string]any)
	return ok && pyrepr.Truthy(metadata[state.ModelOnlyKey])
}
