package gatewayrun

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Adapter rendering tool progress as native plan and task cards
type nativeTaskCardAdapter interface {
	SendNativeTaskCardProgress(ctx context.Context, chatID string, tasks []map[string]string, title string, replyTo *string, metadata map[string]any, fallbackText string) (gateway.SendResult, error)
}

// Adapter closing its native task card stream at turn cleanup
type nativeTaskCardStopper interface {
	StopNativeTaskCardProgress(ctx context.Context, chatID string, replyTo *string, metadata map[string]any) (gateway.SendResult, error)
}

// Adapter resolving whether a chat can host a card, nil meaning no verdict
type nativeTaskCardDestination interface {
	NativeTaskCardDestinationSupported(chatID string, replyTo *string, metadata map[string]any) *bool
}

// Adapter whose edit takes the thread routing metadata of an overflow edit
type metadataMessageEditor interface {
	EditMessageWithMetadata(ctx context.Context, chatID, messageID, content string, finalize bool, metadata map[string]any) (gateway.SendResult, error)
}

var taskCardSpaceRe = regexp.MustCompile(`\s+`)

// Task card rail state of one turn
type taskCardState struct {
	adapter               gateway.PlatformAdapter
	tasks                 map[string]map[string]string
	taskOrder             []string
	fallbackMsgID         *string
	nativeFailed          bool
	publicationSuppressed bool
	anonymousSeq          int
}

func newTaskCardState(adapter gateway.PlatformAdapter) *taskCardState {
	return &taskCardState{adapter: adapter, tasks: map[string]map[string]string{}}
}

// Whitespace collapsed text cut to the limit with an ellipsis
func taskCardCompact(value any, limit int) string {
	raw := ""
	if pyrepr.Truthy(value) {
		raw = pyrepr.Str(value)
	}
	text := strings.TrimSpace(taskCardSpaceRe.ReplaceAllString(raw, " "))
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return strings.TrimRight(string([]rune(text)[:limit-3]), " \t\n\r\v\f") + "..."
}

// The last eight tasks in start order
func (s *taskCardState) visibleTasks() []map[string]string {
	order := s.taskOrder
	if len(order) > 8 {
		order = order[len(order)-8:]
	}
	out := make([]map[string]string, 0, len(order))
	for _, id := range order {
		out = append(out, s.tasks[id])
	}
	return out
}

// Plain text card of the visible tasks for the editable fallback
func (s *taskCardState) fallbackText(ctx context.Context) string {
	labels := map[string]string{"in_progress": i18n.T(ctx, "gateway.progress.task_status_running", "", nil),
		"complete": i18n.T(ctx, "gateway.progress.task_status_complete", "", nil),
		"error":    i18n.T(ctx, "gateway.progress.task_status_error", "", nil)}
	var lines []string
	for _, task := range s.visibleTasks() {
		status, ok := labels[task["status"]]
		if !ok {
			status = task["status"]
		}
		lines = append(lines, i18n.T(ctx, "gateway.progress.task_line", "", map[string]any{"title": task["title"], "status": status}))
	}
	return i18n.T(ctx, "gateway.progress.task_card_title", "", nil) + "\n" + strings.Join(lines, "\n")
}

func (s *taskCardState) upsert(callID, title string) map[string]string {
	if _, ok := s.tasks[callID]; !ok {
		s.taskOrder = append(s.taskOrder, callID)
	}
	s.tasks[callID] = map[string]string{"id": callID, "title": taskCardCompact(title, 120), "status": "in_progress"}
	return s.tasks[callID]
}

// Folds a tool lifecycle event into the cards, false for anything else
func (s *taskCardState) applyEvent(raw any) bool {
	event, _ := raw.(map[string]any)
	eventType, _ := event["type"].(string)
	if eventType != "tool.started" && eventType != "tool.completed" {
		return false
	}
	callID := ""
	if pyrepr.Truthy(event["tool_call_id"]) {
		callID = pyrepr.Str(event["tool_call_id"])
	}
	if callID == "" {
		s.anonymousSeq++
		callID = "anonymous_" + pyrepr.Str(s.anonymousSeq)
	}
	toolName := "tool"
	if pyrepr.Truthy(event["tool_name"]) {
		toolName = pyrepr.Str(event["tool_name"])
	}
	if eventType == "tool.started" {
		preview := taskCardCompact(event["preview"], 64)
		title := toolName
		if preview != "" {
			title = toolName + " - " + preview
		}
		s.upsert(callID, title)
		return true
	}
	task, ok := s.tasks[callID]
	if !ok {
		task = s.upsert(callID, toolName)
	}
	if pyrepr.Truthy(event["is_error"]) {
		task["status"] = "error"
	} else {
		task["status"] = "complete"
	}
	return true
}

func sendResultText(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

// Edits the fallback task card message or sends a new one, a declined edit suppressing the rest of the turn
func (t *TurnRunner) taskCardSendOrEditFallback(ctx context.Context, st *taskCardState) error {
	text := st.fallbackText(ctx)
	if st.publicationSuppressed {
		return nil
	}
	if st.fallbackMsgID != nil && *st.fallbackMsgID != "" {
		result, err := t.editTaskCardFallback(ctx, st, *st.fallbackMsgID, text)
		if err != nil {
			return err
		}
		if result.Success {
			return nil
		}
		if gateway.DeclinedSend(&result) {
			runLog().Warn("Task-card fallback edit DECLINED by the connector's egress guard; suppressing progress delivery for the rest of this turn (the destination is not approved)")
			st.publicationSuppressed = true
			return nil
		}
	}
	result, err := t.sendProgressText(ctx, st.adapter, text)
	if err != nil {
		return err
	}
	if result.Success && pyrepr.Truthy(result.MessageID) {
		id := pyrepr.Str(result.MessageID)
		st.fallbackMsgID = &id
	}
	return nil
}

// Edits the fallback card message with the progress metadata when the adapter edit takes it
func (t *TurnRunner) editTaskCardFallback(ctx context.Context, st *taskCardState, messageID, text string) (gateway.SendResult, error) {
	tc := t.ctx
	if editor, ok := st.adapter.(metadataMessageEditor); ok {
		return editor.EditMessageWithMetadata(ctx, tc.source.ChatID, messageID, text, false, tc.progressMetadata)
	}
	return st.adapter.EditMessage(ctx, tc.source.ChatID, messageID, text, false)
}

// Publishes the cards natively, falling back to an editable text card once the native lane fails
func (t *TurnRunner) taskCardPublish(ctx context.Context, st *taskCardState) error {
	tc := t.ctx
	if len(st.tasks) == 0 || st.publicationSuppressed {
		return nil
	}
	if dest, ok := st.adapter.(nativeTaskCardDestination); ok {
		if supported := dest.NativeTaskCardDestinationSupported(tc.source.ChatID, tc.progressReplyTo, tc.progressMetadata); supported != nil && !*supported {
			t.taskCardUncardableDestination(st, "no thread anchor")
			if st.publicationSuppressed {
				return nil
			}
		}
	}
	if !st.nativeFailed {
		cards := st.adapter.(nativeTaskCardAdapter)
		result, err := cards.SendNativeTaskCardProgress(ctx, tc.source.ChatID, st.visibleTasks(),
			i18n.T(ctx, "gateway.progress.task_card_title", "", nil), tc.progressReplyTo, tc.progressMetadata, st.fallbackText(ctx))
		if err != nil {
			return err
		}
		if result.Success {
			return nil
		}
		if gateway.DeclinedSend(&result) {
			st.publicationSuppressed = true
			st.nativeFailed = true
			runLog().Warn("Slack native task-card progress DECLINED by the connector's egress guard — suppressing the text fallback for the rest of this turn (the destination is not approved)")
			return nil
		}
		st.nativeFailed = true
		reason := pyrepr.Str(result.Error)
		if result.Error != nil && cardDestinationRefusals[reason] {
			t.taskCardUncardableDestination(st, reason)
			if st.publicationSuppressed {
				return nil
			}
		} else {
			runLog().Warn("Slack native task-card progress failed; falling back to an editable text update: " + reason)
		}
	}
	return t.taskCardSendOrEditFallback(ctx, st)
}

// A chat that cannot host a card keeps text progress only when the operator asked for it
func (t *TurnRunner) taskCardUncardableDestination(st *taskCardState, reason string) {
	if t.ctx.toolProgressEnabled {
		st.nativeFailed = true
		runLog().Info("Slack native task cards are unsupported for this destination (" + reason + "); tool progress continues as an editable text update")
		return
	}
	st.publicationSuppressed = true
	runLog().Info("Slack native task cards are unsupported for this destination (" + reason + "); tool progress stays off for this turn")
}

// Applies every queued event to the cards, true when any changed them
func (t *TurnRunner) taskCardDrain(st *taskCardState) bool {
	changed := false
	for {
		raw, ok := t.ctx.progressQueue.getNowait()
		if !ok {
			return changed
		}
		changed = st.applyEvent(raw) || changed
	}
}

// Drains progress into native cards until the run ends, publishing what arrived before a cancel
func (t *TurnRunner) sendNativeTaskCardProgress(ctx context.Context, adapter gateway.PlatformAdapter) error {
	tc := t.ctx
	st := newTaskCardState(adapter)
	defer func() {
		if stopper, ok := adapter.(nativeTaskCardStopper); ok {
			if _, err := stopper.StopNativeTaskCardProgress(context.WithoutCancel(ctx), tc.source.ChatID, tc.progressReplyTo, tc.progressMetadata); err != nil {
				runLog().Debug("task-card stop failed during turn cleanup", "error", err)
			}
		}
	}()
	for tc.runStillCurrent() {
		if ctx.Err() != nil {
			break
		}
		raw, ok := tc.progressQueue.getNowait()
		if !ok {
			if sleepCtx(ctx, 0.1) != nil {
				break
			}
			continue
		}
		if !t.agentInterrupted() && st.applyEvent(raw) {
			if err := t.taskCardPublish(ctx, st); err != nil {
				if ctx.Err() != nil {
					break
				}
				return err
			}
		}
	}
	if ctx.Err() != nil {
		if t.taskCardDrain(st) && tc.runStillCurrent() && !t.agentInterrupted() {
			return t.taskCardPublish(context.WithoutCancel(ctx), st)
		}
	}
	return nil
}

// Editable progress bubble state shared by the drain loop and its helpers
type progressEditState struct {
	adapter             gateway.PlatformAdapter
	progressLines       []any
	progressMsgID       any
	canEdit             bool
	lenFn               func(string) int
	textLimit           int
	editAcceptsMetadata bool
}

func (t *TurnRunner) newProgressEditState(adapter gateway.PlatformAdapter) *progressEditState {
	tc := t.ctx
	lenFn := adapter.MessageLenFn()
	rawLimit := 4000
	if base := adapter.Base(); base != nil && base.Traits.MaxMessageLength != 0 {
		rawLimit = base.Traits.MaxMessageLength
	}
	if adapter.Base() != nil {
		if limit, err := adapter.MaxMessageLengthForChat(tc.source.ChatID); err == nil {
			if limit == 0 {
				limit = 4000
			}
			if fn, err := adapter.MessageLenFnForChat(tc.source.ChatID); err == nil {
				rawLimit, lenFn = limit, fn
			} else {
				rawLimit = limit
			}
		}
	}
	margin := 0
	if rawLimit > 128 {
		margin = 64
	}
	_, metadataEdit := adapter.(metadataMessageEditor)
	return &progressEditState{adapter: adapter, canEdit: tc.progressGrouping != "separate", lenFn: lenFn,
		textLimit: max(1, rawLimit-margin), editAcceptsMetadata: len(tc.progressMetadata) > 0 && metadataEdit}
}

// Edits a progress bubble, finalizing where the adapter requires and routing by thread metadata when accepted
func (t *TurnRunner) editProgressMessage(ctx context.Context, st *progressEditState, messageID any, content string) (gateway.SendResult, error) {
	tc := t.ctx
	finalize := st.adapter.Base() != nil && st.adapter.Base().Traits.RequiresEditFinalize
	if st.editAcceptsMetadata {
		return st.adapter.(metadataMessageEditor).EditMessageWithMetadata(ctx, tc.source.ChatID, pyrepr.Str(messageID), content, finalize, tc.progressMetadata)
	}
	return st.adapter.EditMessage(ctx, tc.source.ChatID, pyrepr.Str(messageID), content, finalize)
}

func progressText(lines []any) string {
	parts := make([]string, len(lines))
	for i, line := range lines {
		parts[i] = pyrepr.Str(line)
	}
	return strings.Join(parts, "\n")
}

// Partitions progress lines into platform sized editable bubbles
func splitProgressGroups(st *progressEditState, lines []any) [][]any {
	var groups [][]any
	var current []any
	for _, line := range lines {
		candidate := append(append([]any{}, current...), line)
		if len(current) > 0 && st.lenFn(progressText(candidate)) > st.textLimit {
			groups = append(groups, current)
			candidate = []any{line}
		}
		current = candidate
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// Sends one progress message as a reply in the progress thread and tracks it for cleanup
func (t *TurnRunner) sendProgressText(ctx context.Context, adapter gateway.PlatformAdapter, text string) (gateway.SendResult, error) {
	tc := t.ctx
	replyTo := ""
	if tc.progressReplyTo != nil {
		replyTo = *tc.progressReplyTo
	}
	result, err := adapter.Send(ctx, tc.source.ChatID, text, replyTo, tc.progressMetadata)
	if err != nil {
		return result, err
	}
	t.trackProgressResult(result)
	return result, nil
}

// Starts fresh bubbles before one exceeds the limit, true when it delivered or a transient edit failure keeps the buffer
func (t *TurnRunner) rollProgressOverflowIfNeeded(ctx context.Context, st *progressEditState) (bool, error) {
	if len(st.progressLines) == 0 || !st.canEdit {
		return false, nil
	}
	groups := splitProgressGroups(st, st.progressLines)
	if len(groups) <= 1 {
		return false, nil
	}
	if st.progressMsgID != nil {
		result, err := t.editProgressMessage(ctx, st, st.progressMsgID, progressText(groups[0]))
		if err != nil {
			return false, err
		}
		if !result.Success {
			if result.Retryable {
				runLog().Debug("[" + st.adapter.Name() + "] Transient overflow edit failure — keeping can_edit=True")
				return true, nil
			}
			st.canEdit = false
			return false, nil
		}
		groups = groups[1:]
	}
	for _, group := range groups {
		result, err := t.sendProgressText(ctx, st.adapter, progressText(group))
		if err != nil {
			return false, err
		}
		if result.Success && pyrepr.Truthy(result.MessageID) {
			st.progressMsgID = result.MessageID
		}
	}
	st.progressLines = groups[len(groups)-1]
	return true, nil
}

// Closes the tool bubble after a content bubble landed so the next tool starts below it
func (t *TurnRunner) resetProgressBubble(st *progressEditState) {
	st.progressMsgID, st.progressLines = nil, nil
	t.ctx.lastProgressMsg, t.ctx.repeatCount = nil, 0
}

// Folds a queue item into the bubble buffer and returns the line this tick renders
func progressAbsorb(st *progressEditState, raw any) any {
	if dedup, ok := raw.(progressDedup); ok {
		if len(st.progressLines) == 0 {
			return dedup.msg
		}
		st.progressLines[len(st.progressLines)-1] = appendProgressRepeatCount(dedup.msg, dedup.count+1)
		return st.progressLines[len(st.progressLines)-1]
	}
	st.progressLines = append(st.progressLines, raw)
	return raw
}

func (t *TurnRunner) flushProgressEdit(ctx context.Context, st *progressEditState) {
	if st.canEdit && len(st.progressLines) > 0 && pyrepr.Truthy(st.progressMsgID) {
		_, _ = t.editProgressMessage(ctx, st, st.progressMsgID, progressText(st.progressLines))
	}
}

// Renders every line still queued at cancel into the bubbles with one final edit
func (t *TurnRunner) drainProgressOnCancel(ctx context.Context, st *progressEditState) error {
	tc := t.ctx
	func() {
		for !tc.progressQueue.empty() {
			raw, ok := tc.progressQueue.getNowait()
			if !ok {
				return
			}
			if _, reset := raw.(progressReset); reset {
				if _, err := t.rollProgressOverflowIfNeeded(ctx, st); err != nil {
					return
				}
				t.flushProgressEdit(ctx, st)
				t.resetProgressBubble(st)
				continue
			}
			progressAbsorb(st, raw)
			if _, err := t.rollProgressOverflowIfNeeded(ctx, st); err != nil {
				return
			}
		}
	}()
	if st.canEdit && len(st.progressLines) > 0 && pyrepr.Truthy(st.progressMsgID) {
		if _, err := t.rollProgressOverflowIfNeeded(ctx, st); err != nil {
			return err
		}
	}
	t.flushProgressEdit(ctx, st)
	return nil
}

// Restores the typing indicator on this turn's own message after a bubble update
func (t *TurnRunner) progressRestoreTyping(ctx context.Context, st *progressEditState) error {
	tc := t.ctx
	if err := sleepCtx(ctx, 0.3); err != nil {
		return err
	}
	if tc.runStillCurrent() {
		return st.adapter.SendTyping(ctx, tc.source.ChatID, gateway.TypingMetadataForTurn(st.adapter, tc.source.ChatID, tc.eventMessageID, tc.progressMetadata))
	}
	return nil
}

// Delivers this tick's bubble, false on a transient edit failure that retries next tick
func (t *TurnRunner) progressSendOrEdit(ctx context.Context, st *progressEditState, msg any) (bool, error) {
	if st.canEdit && st.progressMsgID != nil {
		result, err := t.editProgressMessage(ctx, st, st.progressMsgID, progressText(st.progressLines))
		if err != nil {
			return false, err
		}
		if result.Success {
			return true, nil
		}
		if result.Retryable {
			runLog().Debug("[" + st.adapter.Name() + "] Transient edit failure — keeping can_edit=True")
			return false, nil
		}
		errText := strings.ToLower(sendResultText(result.Error))
		if strings.Contains(errText, "flood") || strings.Contains(errText, "retry after") {
			runLog().Info("[" + st.adapter.Name() + "] Progress edit flood control, backing off")
		} else {
			st.canEdit = false
		}
		if _, err := t.sendProgressText(ctx, st.adapter, pyrepr.Str(msg)); err != nil {
			return false, err
		}
		return true, nil
	}
	text := pyrepr.Str(msg)
	if st.canEdit {
		text = progressText(st.progressLines)
	}
	result, err := t.sendProgressText(ctx, st.adapter, text)
	if err != nil {
		return false, err
	}
	if result.Success && pyrepr.Truthy(result.MessageID) {
		st.progressMsgID = result.MessageID
	}
	return true, nil
}

// Drains the progress queue into native cards or throttled editable bubbles until the run ends or is cancelled
func (t *TurnRunner) sendProgressMessages(ctx context.Context) error {
	tc := t.ctx
	var adapter gateway.PlatformAdapter
	if tc.progressQueue != nil {
		var err error
		if adapter, err = t.runner.deliveryAdapterFor(ctx, tc.source); err != nil {
			return err
		}
	}
	if adapter == nil {
		return nil
	}
	if _, cards := adapter.(nativeTaskCardAdapter); tc.nativeSlackTaskCards && cards {
		return t.sendNativeTaskCardProgress(ctx, adapter)
	}
	if !gateway.AdapterOverridesBaseMethod(adapter, "EditMessage") {
		t.drainProgressQueue()
		return nil
	}
	st := t.newProgressEditState(adapter)
	lastEditTS := 0.0
	const editInterval = 1.5
	cancelled := func() error { return t.drainProgressOnCancel(context.WithoutCancel(ctx), st) }
	for {
		if ctx.Err() != nil {
			return cancelled()
		}
		if !tc.runStillCurrent() {
			t.drainProgressQueue()
			return nil
		}
		raw, ok := tc.progressQueue.getNowait()
		if !ok {
			if sleepCtx(ctx, 0.3) != nil {
				return cancelled()
			}
			continue
		}
		if t.agentInterrupted() {
			continue
		}
		if _, reset := raw.(progressReset); reset {
			t.resetProgressBubble(st)
			continue
		}
		msg := progressAbsorb(st, raw)
		err := func() error {
			rolled, err := t.rollProgressOverflowIfNeeded(ctx, st)
			if err != nil {
				return err
			}
			if !rolled {
				remaining := editInterval - (pythread.Monotonic() - lastEditTS)
				if remaining > 0 {
					return sleepCtx(ctx, remaining)
				}
				if !tc.runStillCurrent() {
					return errProgressRunEnded
				}
				sent, err := t.progressSendOrEdit(ctx, st, msg)
				if err != nil || !sent {
					return err
				}
			}
			lastEditTS = pythread.Monotonic()
			return t.progressRestoreTyping(ctx, st)
		}()
		switch {
		case err == errProgressRunEnded:
			return nil
		case err != nil && ctx.Err() != nil:
			return cancelled()
		case err != nil:
			runLog().Error("Progress message error: " + err.Error())
			if sleepCtx(ctx, 1) != nil {
				return cancelled()
			}
		}
	}
}

// Sentinel of a drain loop whose run stopped being current before its send
var errProgressRunEnded = progressRunEnded{}

type progressRunEnded struct{}

func (progressRunEnded) Error() string { return "progress run ended" }
