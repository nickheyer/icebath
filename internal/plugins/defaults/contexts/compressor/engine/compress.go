package engine

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/internal/contexts"
	contextsconfig "github.com/nickheyer/icebath/internal/contexts/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Clears what one attempt reports and opens its record
func (c *compressor) begin(request *contexts.Request) {
	c.dropped, c.fallbackUsed, c.feasibilitySkip = 0, false, false
	c.lastError, c.auxFailedModel, c.auxFailure = "", "", ""
	c.aborted, c.refusedGrow, c.overloadDegraded, c.madeProgress = false, false, false, false
	c.terminal = nil
	c.rearmBefore = c.pruneRearm
	c.record = contexts.Record{Attempt: strings.ReplaceAll(uuid.NewString(), "-", ""), Window: c.window, Threshold: c.threshold, Method: "none"}
	if request.Force {
		c.clearCooldown()
		c.backoffUntil = time.Time{}
	}
}

func (c *compressor) Compress(ctx context.Context, env *contexts.Env, request *contexts.Request) (*contexts.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.env, c.notify = env, env.Notify
	c.begin(request)
	c.aggressive = request.Aggressive
	defer func() { c.aggressive = false }()
	if err := c.feasibility(ctx, env); err != nil {
		return nil, err
	}
	messages := request.Messages
	unchanged := func(class string) (*contexts.Result, error) {
		c.record.Failure = class
		return c.result(messages), nil
	}
	n := len(messages)
	least := c.headSize(messages) + 3 + 1
	if c.aggressive {
		least = c.headSize(messages) + 1
	}
	if n <= least {
		c.structuralNoOp(fmt.Sprintf("only %d messages (need > %d)", n, least), n)
		return unchanged("insufficient_messages")
	}
	shown := request.Tokens
	if shown == 0 {
		shown = cmp.Or(c.lastPrompt, contexts.Estimate(messages))
	}
	sparedStart, sparedEnd := c.sparedRound(messages)
	canonical := dropBlankEchoes(messages)
	working, pruned := c.pruneResults(messages, c.s.protectLast, c.tailBudget, pruneMinChars)
	if pruned > 0 {
		slog.Info(fmt.Sprintf("Pre-compression: pruned %d old tool result(s)", pruned))
	}
	working = dropBlankEchoes(working)
	start, end := c.middle(working)
	c.record.Messages = len(working)
	if start >= end {
		c.regions(working, start, end, nil)
		c.structuralNoOp(fmt.Sprintf("compress_start (%d) >= compress_end (%d) - transcript fits within tail budget", start, end), len(working))
		return unchanged("no_compressible_window")
	}
	fresh := request.Force || request.Focus != "" || request.Bypass
	prepared := c.takePrepared(ctx, working, start, fresh, strings.TrimSpace(request.Memory) != "")
	if prepared != nil {
		end = prepared.end
	}
	turns := slices.Clone(working[start:end])
	if c.s.tailMode == contextsconfig.TailLean {
		working, _ = demoteStaleTail(working, end, c.session)
	}
	scan := c.scanHandoffs(working, start, end, turns)
	turns = scan.turns
	c.regions(working, start, end, turns)
	if len(turns) == 0 {
		c.structuralNoOp(fmt.Sprintf("window %d-%d holds only already-summarized handoffs", start, end), len(working))
		return unchanged("empty_post_handoff_window")
	}
	slog.Info(fmt.Sprintf("Context compression triggered (%d tokens >= %d threshold)", shown, c.threshold))
	slog.Info(fmt.Sprintf("Model context limit: %d tokens (%.0f%% = %d)", c.window, c.percent*100, c.threshold))
	slog.Info(fmt.Sprintf("Summarizing turns %d-%d (%d turns), protecting %d head + %d tail messages", start+1, end, len(turns), start, len(working)-scan.tailStart))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	skip := !(request.Force || prepared != nil) && (c.benched() || c.tooSmall(contexts.Estimate(turns), start, end))
	summary := ""
	switch {
	case prepared != nil:
		c.adopt(prepared)
		summary = prepared.summary
	case request.Deterministic:
		c.lastError = "The summary model stalled on every provider."
		c.record.Failure = "stall_deterministic_fallback"
	case !skip:
		focus := request.Focus
		if focus == "" {
			focus = deriveFocus(working)
		}
		pin := pinned{}
		if request.Entry {
			if pin.entry = entryOf(env); pin.entry == "" {
				slog.Info("Skipping compression stall fallback: the compression task has no fallback_chain entry")
				c.record.Failure = "no_fallback_entry"
				c.previous, c.hasUser = scan.previous, scan.hasUser
				return c.result(messages), nil
			}
		}
		got, err := c.summarize(ctx, env, turns, focus, request.Memory, request.Bypass, pin)
		if err != nil {
			if ctx.Err() != nil {
				c.previous, c.hasUser = scan.previous, scan.hasUser
			}
			return nil, err
		}
		summary = got
		if summary == "" && c.abort(end-start, scan) {
			return c.result(messages), nil
		}
	}
	if summary == "" {
		summary = c.fallback(turns, end-start, skip)
	}
	compressed := c.assemble(working, start, end, scan, summary)
	result := c.result(c.finish(compressed, canonical, len(working), sparedEnd > sparedStart))
	result.Pruned = pruned
	return result, nil
}

// First entry of the summary task's own fallback chain, empty when it has none
func entryOf(env *contexts.Env) string {
	if env.Aux == nil {
		return ""
	}
	for _, entry := range env.Aux.Chain(Task) {
		if entry = strings.TrimSpace(entry); entry != "" {
			return entry
		}
	}
	return ""
}

// Records the rough sizes of the head, the middle and the tail
func (c *compressor) regions(messages []*sessionsv1.Message, start, end int, middle []*sessionsv1.Message) {
	c.record.Head = contexts.Estimate(span(messages, 0, start))
	c.record.Middle = contexts.Estimate(middle)
	c.record.Tail = contexts.Estimate(span(messages, end, len(messages)))
}

// Result of the attempt with what it came to
func (c *compressor) result(messages []*sessionsv1.Message) *contexts.Result {
	result := &contexts.Result{Messages: messages, Aborted: c.aborted, Fallback: c.fallbackUsed, Dropped: c.dropped, Skipped: c.feasibilitySkip,
		Failure: c.lastError, AuxModel: c.auxFailedModel, AuxFailure: c.auxFailure, Record: c.record}
	if left := time.Until(c.cooldownUntil); left > 0 {
		result.Cooldown = left.Seconds()
	}
	result.Record.Method = "llm_summary"
	switch {
	case !c.madeProgress:
		result.Record.Method = "none"
	case c.fallbackUsed:
		result.Record.Method = "deterministic_fallback"
	case c.auxFailure != "":
		result.Record.Method = "aux_fallback_main"
	}
	return result
}

// Whether the attempt aborts with the messages kept, after a failure that must not lose context or as the settings ask
func (c *compressor) abort(skipped int, scan handoffScan) bool {
	terminal := c.terminalFailure()
	if terminal == nil && !c.s.abortOnFailure {
		return false
	}
	c.dropped, c.fallbackUsed, c.aborted = 0, false, true
	class, message := "summary_generation_aborted", "Summary generation failed — aborting compression (compression.abort_on_summary_failure=true). %d "+
		"message(s) preserved unchanged. Conversation is frozen until the next /compress or /new."
	if terminal != nil {
		class, message = terminal.class, terminal.message
	}
	c.record.Failure = class
	c.previous = scan.previous
	slog.Warn(fmt.Sprintf(message, skipped))
	return true
}

// Summary built without the summariser for the window, becoming the base of the next one
func (c *compressor) fallback(turns []*sessionsv1.Message, dropped int, skipped bool) string {
	if skipped {
		slog.Info("Pre-LLM summary skip — inserting deterministic fallback context summary")
	} else {
		slog.Warn("Summary generation failed — inserting deterministic fallback context summary")
	}
	c.dropped, c.fallbackUsed = dropped, true
	switch {
	case c.overloadDegraded:
		c.record.Failure = "summary_overload_degraded"
	case c.record.Failure != "":
	case skipped:
		c.record.Failure = "feasibility_skip"
	default:
		c.record.Failure = "summary_generation_failed"
	}
	reason := ""
	if !skipped {
		reason = c.lastError
	}
	summary := c.staticSummary(turns, reason)
	c.previous = stripPrefix(summary)
	return summary
}

// What the scan of earlier summaries found: the turns to summarise with summaries unwrapped, where summaries sit, where the tail starts,
// and the state before it so a cancelled attempt can put it back
type handoffScan struct {
	turns     []*sessionsv1.Message
	summaries map[int]bool
	tailStart int
	previous  string
	hasUser   *bool
}

// Rehydrates the summary to build on and whether the person took part from summaries in the transcript, and unwraps those in the window
func (c *compressor) scanHandoffs(messages []*sessionsv1.Message, start, end int, turns []*sessionsv1.Message) handoffScan {
	scan := handoffScan{turns: turns, summaries: map[int]bool{}, tailStart: end, previous: c.previous, hasUser: c.hasUser}
	var hits []int
	var bodies []string
	for i, m := range messages {
		if contexts.IsSummary(m) {
			hits = append(hits, i)
			if body := stripPrefix(contexts.Text(m)); body != "" {
				bodies = append(bodies, body)
			}
		}
	}
	realUser := hasRealUser(messages)
	if len(hits) == 0 {
		c.previous = ""
		c.hasUser = &realUser
		return scan
	}
	last := hits[len(hits)-1]
	if c.previous == "" {
		c.previous = strings.Join(bodies, "\n\n")
	}
	switch {
	case realUser:
		c.hasUser = &realUser
	case c.hasUser == nil:
		said := !strings.Contains(stripPrefix(contexts.Text(messages[last])), noUserTask)
		c.hasUser = &said
	}
	for _, i := range hits {
		scan.summaries[i] = true
	}
	var window []*sessionsv1.Message
	for i := start; i <= last && i < len(messages); i++ {
		m := messages[i]
		if scan.summaries[i] {
			if unwrapped := contexts.Unwrap(m); unwrapped != nil {
				window = append(window, unwrapped)
			}
			continue
		}
		if i < last {
			window = append(window, m)
		}
	}
	scan.turns = append(window, span(messages, last+1, end)...)
	if last >= end {
		scan.tailStart = last + 1
	}
	return scan
}

// Opening messages kept, the summaries among them unwrapped
func head(messages []*sessionsv1.Message, start int) []*sessionsv1.Message {
	var out []*sessionsv1.Message
	for _, m := range messages[:start] {
		if unwrapped := contexts.Unwrap(m); unwrapped != nil {
			out = append(out, unwrapped)
		}
	}
	return out
}

// Messages kept after the window, the summaries already folded into the new one left out
func tail(messages []*sessionsv1.Message, end, tailStart int, summaries map[int]bool) []*sessionsv1.Message {
	var out []*sessionsv1.Message
	for i := max(end, tailStart); i < len(messages); i++ {
		if summaries[i] && i >= tailStart {
			continue
		}
		if unwrapped := contexts.Unwrap(messages[i]); unwrapped != nil {
			out = append(out, unwrapped)
		}
	}
	return out
}

// Role a chat template renders a message with, false for tool results and replies that call tools
func visibleRole(m *sessionsv1.Message) (sessionsv1.Role, bool) {
	if isTool(m) || calls(m) {
		return 0, false
	}
	return m.GetRole(), true
}

func lastVisible(messages []*sessionsv1.Message) (sessionsv1.Role, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		if role, ok := visibleRole(messages[i]); ok {
			return role, true
		}
	}
	return 0, false
}

// Role the summary takes, whether it merges into the first tail message instead, whether it must lead as the person's, and that message
func placement(compressed, tailRows []*sessionsv1.Message, start int) (sessionsv1.Role, bool, bool, int) {
	lastHead, known := sessionsv1.Role_ROLE_USER, true
	if len(compressed) > 0 {
		lastHead, known = lastVisible(compressed)
	}
	first, firstRole := -1, sessionsv1.Role(0)
	for i, m := range tailRows {
		if role, ok := visibleRole(m); ok {
			first, firstRole = i, role
			break
		}
	}
	said := slices.ContainsFunc(append(slices.Clone(compressed), tailRows...), func(m *sessionsv1.Message) bool {
		return isUser(m) && strings.TrimSpace(contexts.Text(m)) != ""
	})
	forceUser := start == 0 || known && lastHead == sessionsv1.Role_ROLE_SYSTEM || !said
	role := sessionsv1.Role_ROLE_ASSISTANT
	if !known || lastHead == sessionsv1.Role_ROLE_ASSISTANT || lastHead == sessionsv1.Role_ROLE_TOOL || forceUser {
		role = sessionsv1.Role_ROLE_USER
	}
	merge := false
	if first >= 0 && role == firstRole {
		flipped := sessionsv1.Role_ROLE_USER
		if role == sessionsv1.Role_ROLE_USER {
			flipped = sessionsv1.Role_ROLE_ASSISTANT
		}
		if known && flipped != lastHead && !forceUser {
			role = flipped
		} else {
			merge = len(tailRows) > 0
		}
	}
	return role, merge, forceUser, first
}

// Tail message with the summary merged in: leading it as the person's message, or after its own text as prior context
func (c *compressor) mergeInto(m *sessionsv1.Message, summary string, role sessionsv1.Role, forceUser bool) *sessionsv1.Message {
	old := contexts.Text(m)
	var text string
	if forceUser && role == sessionsv1.Role_ROLE_USER {
		text = summary + "\n\n" + contexts.EndMarker + "\n\n" + old
	} else {
		text = contexts.PriorHeader + "\n" + old + "\n\n" + contexts.Delimiter + "\n\n" + summary + "\n\n" + contexts.EndMarker
	}
	out := contexts.Rewritten(m, text)
	out.Summary = true
	return out
}

// Head, summary and tail: the summary a message of its own in the role that keeps the template's turns alternating, else merged into the
// first tail message
func (c *compressor) assemble(messages []*sessionsv1.Message, start, end int, scan handoffScan, summary string) []*sessionsv1.Message {
	compressed := head(messages, start)
	tailRows := tail(messages, end, scan.tailStart, scan.summaries)
	summary, compressed, tailRows = withSkillSafety(summary, compressed, tailRows)
	role, merge, forceUser, first := placement(compressed, tailRows, start)
	if !merge {
		compressed = append(compressed, &sessionsv1.Message{Role: role, Text: summary + "\n\n" + contexts.EndMarker, Summary: true, ModelOnly: true})
	}
	target := 0
	if forceUser && first >= 0 {
		target = first
	}
	for i, m := range tailRows {
		if merge && i == target {
			m = c.mergeInto(m, summary, role, forceUser)
		}
		compressed = append(compressed, m)
	}
	return compressed
}

// Tool results whose call is gone dropped, and calls whose result is gone stripped from their reply, except the newest reply's
func sanitizePairs(messages []*sessionsv1.Message) []*sessionsv1.Message {
	calledIDs, answeredIDs := map[string]bool{}, map[string]bool{}
	for _, m := range messages {
		for _, call := range m.GetToolCalls() {
			calledIDs[call.GetId()] = true
		}
		if isTool(m) {
			answeredIDs[m.GetToolCallId()] = true
		}
	}
	var kept []*sessionsv1.Message
	orphans := 0
	for _, m := range messages {
		if isTool(m) && !calledIDs[m.GetToolCallId()] {
			orphans++
			continue
		}
		kept = append(kept, m)
	}
	if orphans > 0 {
		slog.Info(fmt.Sprintf("Compression sanitizer: removed %d orphaned tool result(s)", orphans))
	}
	trailing := -1
	for i := len(kept) - 1; i >= 0; i-- {
		if !isTool(kept[i]) {
			if isAssistant(kept[i]) {
				trailing = i
			}
			break
		}
	}
	stripped := 0
	for i, m := range kept {
		if !calls(m) || i == trailing {
			continue
		}
		var answered []*sessionsv1.ToolCall
		for _, call := range m.GetToolCalls() {
			if answeredIDs[call.GetId()] {
				answered = append(answered, call)
			}
		}
		if len(answered) == len(m.GetToolCalls()) {
			continue
		}
		stripped += len(m.GetToolCalls()) - len(answered)
		out := contexts.Rewritten(m, contexts.Text(m))
		out.ToolCalls = answered
		if len(answered) == 0 && strings.TrimSpace(contexts.Text(out)) == "" {
			out = contexts.Rewritten(out, "(tool call removed)")
			out.ToolCalls = nil
		}
		kept[i] = out
	}
	if stripped > 0 {
		slog.Info(fmt.Sprintf("Compression sanitizer: stripped %d orphaned tool_call(s) from assistant messages", stripped))
	}
	return kept
}

// Whether a summary message carries the restated request after its end marker
func mergedReplay(m *sessionsv1.Message) bool {
	if !contexts.IsSummary(m) {
		return false
	}
	text := contexts.Text(m)
	i := strings.LastIndex(text, contexts.EndMarker)
	if i < 0 {
		return false
	}
	rest := strings.TrimLeftFunc(text[i+len(contexts.EndMarker):], unicode.IsSpace)
	return strings.HasPrefix(rest, inflightHeader) && strings.TrimSpace(strings.TrimPrefix(rest, inflightHeader)) != ""
}

// Message of the person still waiting on its answer, nil when the newest exchange ended on a reply that said something
func inflight(messages []*sessionsv1.Message) *sessionsv1.Message {
	last := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if contexts.RealUser(messages[i]) || mergedReplay(messages[i]) {
			last = i
			break
		}
	}
	if last < 0 {
		return nil
	}
	for i := len(messages) - 1; i > last; i-- {
		m := messages[i]
		if !isAssistant(m) || len(m.GetToolCalls()) > 0 {
			break
		}
		if strings.TrimSpace(contexts.Text(m)) != "" {
			return nil
		}
	}
	return messages[last]
}

// Restates an unfinished request of the person after the summary, so the handoff points at it again
func restate(compressed []*sessionsv1.Message, pending *sessionsv1.Message) []*sessionsv1.Message {
	if pending == nil || len(compressed) == 0 {
		return compressed
	}
	carrier := -1
	for i := len(compressed) - 1; i >= 0; i-- {
		if contexts.IsSummary(compressed[i]) {
			carrier = i
			break
		}
	}
	if carrier < 0 {
		return compressed
	}
	for _, m := range compressed[carrier+1:] {
		if contexts.RealUser(m) {
			return compressed
		}
	}
	_, after, found := strings.Cut(contexts.Text(compressed[carrier]), contexts.EndMarker)
	if !found || strings.TrimSpace(after) != "" {
		return compressed
	}
	task := restated(contexts.Text(pending))
	if task == "" {
		return compressed
	}
	slog.Info("Re-appending the in-flight user task after the compaction handoff so it stays actionable (#100818)")
	compressed = slices.Clone(compressed)
	lastRole, _ := lastVisible(compressed)
	var replay *sessionsv1.Message
	if mergedReplay(pending) {
		replay = &sessionsv1.Message{Role: sessionsv1.Role_ROLE_USER, Text: inflightHeader + "\n" + task}
	} else {
		replay = contexts.Copy(pending)
		replay.Text, replay.Sent, replay.Carriers, replay.Id, replay.Time = inflightHeader+"\n"+task, "", nil, 0, nil
	}
	replay.ModelOnly = true
	if uid := replay.GetUid(); uid != "" {
		// Other copies of the request before the summary go, so the model reads it once, after the handoff
		summary := compressed[carrier]
		kept := slices.DeleteFunc(slices.Clone(compressed), func(m *sessionsv1.Message) bool {
			return m != summary && isUser(m) && m.GetUid() == uid
		})
		if len(kept) != len(compressed) {
			if !isUser(kept[0]) {
				if _, visible := visibleRole(summary); visible {
					kept = slices.DeleteFunc(kept, func(m *sessionsv1.Message) bool { return m == summary })
					kept = slices.Insert(kept, 0, summary)
				} else {
					kept = compressed
				}
			}
			if len(kept) != len(compressed) {
				at := slices.Index(kept, summary)
				prior, _ := lastVisible(kept[:at])
				if role, visible := visibleRole(summary); visible && role == sessionsv1.Role_ROLE_ASSISTANT && prior != sessionsv1.Role_ROLE_USER {
					flipped := contexts.Copy(summary)
					flipped.Role = sessionsv1.Role_ROLE_USER
					kept[at] = flipped
				}
				lastRole, _ = lastVisible(kept)
				compressed = kept
				carrier = slices.IndexFunc(compressed, contexts.IsSummary)
				for i := len(compressed) - 1; i >= 0; i-- {
					if contexts.IsSummary(compressed[i]) {
						carrier = i
						break
					}
				}
			}
		}
		replay.Uid = ""
	}
	if lastRole == sessionsv1.Role_ROLE_USER {
		merged := contexts.Rewritten(compressed[carrier], contexts.Text(compressed[carrier])+"\n\n"+inflightHeader+"\n"+task)
		merged.Absorbed = append(merged.Absorbed, pending.GetUid())
		compressed[carrier] = merged
		return compressed
	}
	return append(compressed, replay)
}

// Pictures taken out of messages older than the newest that carry them, the round still waiting on its answer spared
func stripHistoricalMedia(messages []*sessionsv1.Message, sparedStart, sparedEnd int) []*sessionsv1.Message {
	newest := func(role sessionsv1.Role) int {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].GetRole() == role && len(messages[i].GetMedia()) > 0 {
				return i
			}
		}
		return -1
	}
	anchor, toolAnchor := newest(sessionsv1.Role_ROLE_USER), newest(sessionsv1.Role_ROLE_TOOL)
	if anchor <= 0 && toolAnchor < 0 {
		return messages
	}
	stale := func(i int, m *sessionsv1.Message) bool {
		if i >= sparedStart && i < sparedEnd {
			return false
		}
		return 0 < anchor && i < anchor || anchor == 0 && i == 0 && toolAnchor > 0 || isTool(m) && i != toolAnchor
	}
	out := slices.Clone(messages)
	for i, m := range messages {
		if len(m.GetMedia()) == 0 || !stale(i, m) {
			continue
		}
		if isTool(m) {
			out[i] = stripToolImages(m)
			continue
		}
		fresh := contexts.Rewritten(m, strings.TrimSpace(contexts.Text(m)+"\n"+attachedPlaceholder))
		fresh.Media = nil
		out[i] = fresh
	}
	return out
}

// Compacted messages made sound: tool pairs repaired, the unfinished request restated, old pictures dropped; the count, savings and
// record moved on
func (c *compressor) finish(compressed, original []*sessionsv1.Message, n int, spareImages bool) []*sessionsv1.Message {
	compressed = sanitizePairs(compressed)
	sparedStart, sparedEnd := 0, 0
	if spareImages {
		sparedStart, sparedEnd = pendingRound(compressed)
	}
	compressed = restate(compressed, inflight(original))
	c.compressions++
	compressed = stripHistoricalMedia(compressed, sparedStart, sparedEnd)
	before := contexts.Estimate(original)
	after := contexts.Estimate(compressed)
	c.savings = 0
	if before > 0 {
		c.savings = float64(before-after) / float64(before) * 100
	}
	slog.Info(fmt.Sprintf("Compressed: %d -> %d messages (~%d tokens saved, %.0f%%)", n, len(compressed), before-after, c.savings))
	slog.Info(fmt.Sprintf("Compression #%d complete", c.compressions))
	c.record.Before, c.record.After, c.record.Left = before, after, len(compressed)
	c.madeProgress = true
	c.micro.resetCursor()
	c.pruneRearm, c.reclaimed = 0, ""
	return compressed
}
