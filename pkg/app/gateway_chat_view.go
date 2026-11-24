package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
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
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	line := strings.Join(strings.Fields(b.String()), " ")
	if utf8.RuneCountInString(line) > gatewayPreviewChars {
		line = textutil.First(line, gatewayPreviewChars-1) + "…"
	}
	return line
}

// Quoted indented prompt of a pending row, or the fallback naming its id when it carries no text
func gatewayQuoted(row map[string]any, fallback string) string {
	if preview := gatewayPendingPreview(row["text"]); preview != "" {
		return `    "` + preview + `"`
	}
	return fmt.Sprintf("    (%s %s)", fallback, anyutil.String(requireKey(row, "admission_id")))
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
			lines = append(lines, "  To drop it and continue, type:", "/discard "+anyutil.String(requireKey(lost[0], "admission_id")))
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
				lines = append(lines, "/discard "+anyutil.String(requireKey(row, "admission_id")))
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

// Terminal view of one gateway session, its event rendering and its fenced controls
type GatewayChatView struct {
	// mu is the event loop, held while view code runs and released at each wait
	mu sync.Mutex

	usageFile       string
	attachments     []any
	client          gatewayChatPeer
	sessionID       string
	generation      any
	prompts         *orderedutil.Map[any, map[string]any]
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
	streams         *orderedutil.Map[any, string]
	completions     *orderedutil.Map[any, map[string]any]
	cut             *orderedutil.Map[any, string]
	mine            *orderedutil.Map[any, struct{}]
	gaps            int
	attaching       chan struct{}
	changed         *gatewayViewEvent
	failure         error
	failed          *gatewayViewEvent
	mutations       *PreparedMutations
	out             io.Writer
	errOut          io.Writer
}

// Display name of a model id, its last path segment
func gatewayModelName(v any, fallback string) string {
	if !anyutil.IsTrue(v) {
		return fallback
	}
	parts := strings.Split(anyutil.String(v), "/")
	return parts[len(parts)-1]
}

// Prompts of a snapshot keyed by prompt id in snapshot order
func gatewaySnapshotPrompts(snapshot map[string]any) *orderedutil.Map[any, map[string]any] {
	prompts := orderedutil.NewMap[any, map[string]any]()
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
		sessionID:   anyutil.String(requireKey(snapshot, "stored_session_id")),
		generation:  gatewayGetOr(snapshot, "execution_generation", int64(0)),
		prompts:     gatewaySnapshotPrompts(snapshot),
		pending:     pending,
		model:       gatewayModelName(info["model"], "Icebath"),
		emitter:     emitter,
		quiet:       quiet || emitter != nil,
		streams:     orderedutil.NewMap[any, string](),
		completions: orderedutil.NewMap[any, map[string]any](),
		cut:         orderedutil.NewMap[any, string](),
		mine:        orderedutil.NewMap[any, struct{}](),
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
	if m, ok := jsonutil.Plain(raw).(map[string]any); ok {
		return m, nil
	}
	return nil, fmt.Errorf("the gateway answer is %s, expected a mapping", errchain.Kind(jsonutil.Plain(raw)))
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
		if anyutil.Equal(requireKey(row, "status"), "unknown") {
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
	if anyutil.IsTrue(prompt["description"]) {
		title = anyutil.String(prompt["description"])
	} else if anyutil.IsTrue(prompt["question"]) {
		title = anyutil.String(prompt["question"])
	}
	v.println(v.errOut, "\n"+title)
	if anyutil.IsTrue(prompt["command"]) {
		v.println(v.errOut, anyutil.String(prompt["command"]))
	}
	command := "/answer"
	if anyutil.Equal(requireKey(prompt, "kind"), "approval") {
		command = "/approve"
	}
	var choices []string
	list, _ := gatewayGetOr(prompt, "choices", []any{}).([]any)
	for _, c := range list {
		choices = append(choices, anyutil.String(c))
	}
	joined := strings.Join(choices, "|")
	if joined == "" {
		joined = "answer"
	}
	v.println(v.errOut, fmt.Sprintf("%s %s <%s>", command, anyutil.String(requireKey(prompt, "prompt_id")), joined))
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
		frame, _ := jsonutil.Plain(event).(map[string]any)
		params, _ := gatewayGetOr(frame, "params", map[string]any{}).(map[string]any)
		if !anyutil.Equal(params["session_id"], v.sessionID) {
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
		if !anyutil.IsTrue(admission) {
			admission = payload["admission_id"]
		}
		if v.finite && gatewayBlockingControlEvents[kind] {
			v.dispatchEvent(ctx, kind, admission, payload)
			v.changed.Set()
			continue
		}
		if v.finite && anyutil.IsTrue(admission) {
			if v.finiteAdmission == nil {
				v.finiteEvents = append(v.finiteEvents, gatewayHeldEvent{kind, admission, payload})
				continue
			}
			if !anyutil.Equal(admission, v.finiteAdmission) {
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
	order := orderedutil.NewMap[any, struct{}]()
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
	if anyutil.Equal(status, "unknown") && v.mine.Has(admission) {
		v.complete(ctx, admission, map[string]any{"outcome": "unknown", "text": gatewayUnknownOutcome})
		v.changed.Set()
	}
	if !anyutil.IsTrue(admission) || anyutil.Equal(status, "queued") || anyutil.Equal(status, "unknown") || v.completions.Has(admission) {
		return nil
	}
	streamed, _ := v.streams.Get(admission)
	v.streams.Delete(admission)
	if !v.cut.Has(admission) {
		v.cut.Set(admission, streamed)
	}
	if anyutil.IsTrue(status) {
		return nil
	}
	for _, held := range v.finiteEvents {
		if held.kind == "message.complete" && anyutil.Equal(held.admission, admission) {
			return nil
		}
	}
	raw, err := v.rpcRaw(ctx, "prompt.receipt", map[string]any{"session_id": sid, "admission_id": admission, "include_result": true}, 0)
	var receipt map[string]any
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw, receipt = jsonutil.ObjectOf("status", "terminal"), map[string]any{"status": "terminal"}
	} else if receipt, err = gatewayAnswerMap(raw); err != nil {
		return err
	} else if receipt == nil {
		return errors.New("the gateway returned no receipt for the admission")
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
		if anyutil.Equal(outcome, "interrupted") {
			outcome = "cancelled"
		}
		result, _ := receipt["result"].(map[string]any)
		text := ""
		if anyutil.IsTrue(result["final_response"]) {
			text = anyutil.String(result["final_response"])
		}
		v.complete(ctx, admission, map[string]any{"outcome": outcome, "_receipt": raw, "text": text})
		v.changed.Set()
	case verdict == nil && v.finite:
		v.fail(&gateway.GatewayClientError{Msg: "session_replay_gap"})
	}
	return nil
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
	if anyutil.IsTrue(payload["text"]) {
		text = anyutil.String(payload["text"])
	}
	if !v.quiet && strings.TrimSpace(text) != "" {
		v.println(v.errOut, strings.TrimSpace(text))
	}
}

// Text of a payload value that is a non empty string or other truthy value
func gatewayText(values ...any) any {
	for _, value := range values {
		if anyutil.IsTrue(value) {
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
		args, _ := jsonutil.Plain(payload["args"]).(map[string]any)
		extra := map[string]any{}
		if anyutil.IsTrue(payload["tool_call_id"]) {
			extra["tool_call_id"] = payload["tool_call_id"]
		}
		v.emitter.OnToolProgress("tool.started", gatewayOptionalStr(payload["tool_name"]), "", args, extra)
	} else if !v.quiet {
		name := anyutil.String(gatewayText(payload["tool_name"], payload["name"], "tool"))
		args, _ := jsonutil.Plain(payload["args"]).(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		zero := 0
		env := bannerToolEnv(context.Background())
		preview := tools.BuildToolPreview(env, name, args, &zero)
		line := tools.GetToolEmoji(context.Background(), env, name, "⚡") + " " + name
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
	return anyutil.String(v)
}

// Emits the tool result event in stream json mode
func (v *GatewayChatView) toolComplete(admission any, payload map[string]any) {
	if v.emitter == nil {
		return
	}
	args, _ := jsonutil.Plain(payload["args"]).(map[string]any)
	extra := map[string]any{"result": payload["result"], "is_error": gatewayGetOr(payload, "is_error", false)}
	if anyutil.IsTrue(payload["tool_call_id"]) {
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
	text := anyutil.String(gatewayText(payload["text"], payload["content"]))
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
		case strings.TrimSpace(text) == strings.TrimSpace(streamed):
			v.println(v.out, "")
		default:
			v.println(v.out, "\n"+text)
		}
	}
	warning, isStr := payload["warning"].(string)
	if anyutil.Equal(payload["status"], "interrupted") && isStr && strings.TrimSpace(warning) != "" {
		v.println(v.errOut, strings.TrimSpace(warning))
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
	if len(v.attachments) > 0 {
		params["attachments"], v.attachments = v.attachments, nil
	}
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
	if anyutil.IsTrue(admission) && !v.completions.Has(admission) && v.sessionID == sid {
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
		if anyutil.Equal(requireKey(row, "admission_id"), admission) {
			own = requireKey(row, "status")
			break
		}
	}
	if anyutil.Equal(own, "unknown") {
		v.completions.Set(admission, map[string]any{"outcome": "unknown", "text": gatewayUnknownOutcome})
		return ""
	}
	if anyutil.Equal(own, "queued") && len(v.unknownAdmissions()) > 0 {
		return "This chat is paused behind a turn whose ending Icebath couldn't confirm. " +
			"Your message is saved and will run once you resume this session and /discard " +
			"that turn; don't send it again."
	}
	if v.prompts.Len() > 0 && (anyutil.IsTrue(own) || v.gaps == 0) {
		return "Input required; detached without cancelling. Resume this session interactively."
	}
	return ""
}

// Structured result the owner committed with the admission settlement, empty when unreadable
func (v *GatewayChatView) settledResult(ctx context.Context, admission any) (*jsonutil.Object, error) {
	raw, err := v.rpcRaw(ctx, "prompt.receipt", map[string]any{"session_id": v.sessionID, "admission_id": admission, "include_result": true}, 0)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pkgLog().Debug(fmt.Sprintf("settled result for %s unavailable: %v", anyutil.String(admission), err))
		return jsonutil.NewObject(), nil
	}
	receipt, ok := raw.(*jsonutil.Object)
	if !ok {
		pkgLog().Debug(fmt.Sprintf("settled result for %s unavailable: receipt is not a mapping", anyutil.String(admission)))
		return jsonutil.NewObject(), nil
	}
	return gatewayResultCopy(receipt.Value("result")), nil
}

// Copy of a committed result mapping, empty for a missing or falsy one
func gatewayResultCopy(result any) *jsonutil.Object {
	out := jsonutil.NewObject()
	if obj, ok := result.(*jsonutil.Object); ok {
		for _, key := range obj.Keys() {
			out.Set(key, obj.Value(key))
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
	if text != "" || !anyutil.IsTrue(result["interrupted"]) {
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
func (v *GatewayChatView) writeUsageFile(result *jsonutil.Object, outcome any) {
	report := gatewayResultCopy(result)
	sessionID := report.Value("session_id")
	if !anyutil.IsTrue(sessionID) {
		sessionID = v.sessionID
	}
	report.Set("session_id", sessionID)
	var failure any
	if !anyutil.Equal(outcome, "completed") && !anyutil.Equal(outcome, "cancelled") {
		failure = gatewayText(report.Value("error"), outcome, "failed")
	}
	oneshotWriteUsageFile(v.usageFile, report, failure)
}

// Shows pending state, submits the query and waits for its settlement
func (v *GatewayChatView) Run(ctx context.Context, query string) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.quiet = true
	v.finite = true
	v.showPending()
	for _, prompt := range v.prompts.Values() {
		v.showPrompt(prompt)
	}
	if len(v.unknownAdmissions()) > 0 {
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
	if query != "" || len(v.attachments) > 0 {
		var err error
		if receipt, err = v.submit(ctx, query); err != nil {
			return 0, err
		}
	}
	return v.runFinite(ctx, receipt)
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
		if anyutil.Equal(held.admission, admission) && !v.completions.Has(admission) {
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
	text := anyutil.String(gatewayText(terminal["text"], terminal["content"]))
	var result *jsonutil.Object
	if recovered, ok := terminal["_receipt"].(*jsonutil.Object); ok && recovered != nil {
		result = gatewayResultCopy(recovered.Value("result"))
	} else {
		var err error
		if result, err = v.settledResult(ctx, admission); err != nil {
			return 0, err
		}
	}
	result.Set("failed", anyutil.IsTrue(result.Value("failed")) || (!anyutil.Equal(outcome, "completed") && !anyutil.Equal(outcome, "cancelled")))
	result.Set("interrupted", anyutil.IsTrue(result.Value("interrupted")) || anyutil.Equal(outcome, "cancelled"))
	plain, _ := jsonutil.Plain(result).(map[string]any)
	var exitCode int
	if v.unattended {
		exitCode = oneshotExitCode(text, plain)
	} else {
		exitCode = tools.TurnExitCode(plain, tools.TurnExitOptions{})
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
		final["replay_gap"] = anyutil.IsTrue(terminal["replay_gap"])
		return v.emitter.EmitResult(final, v.sessionID, exitCode), nil
	}
	v.reportSilentInterrupt(text, plain)
	v.println(v.out, text)
	v.reportNoReply(exitCode)
	v.printExitIDs(ctx)
	return exitCode, nil
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

// The single query exit block naming how to continue this session, the profile flag when not default
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
	return ok && anyutil.IsTrue(metadata[state.ModelOnlyKey])
}
