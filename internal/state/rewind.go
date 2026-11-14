package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	withheldPromptKey         = "withheld_prompt"
	withheldPromptPlaceholder = "[Message withheld: a pre_llm_call policy hook blocked it.]"
	rewindHistoryChanged      = "session history changed before the rewind could be persisted"
)

var ErrNoRewindHooks = errors.New("store was opened without the transcript rewind hooks")

type RewindUnavailableError struct{ msg string }

func (e *RewindUnavailableError) Error() string { return e.msg }

type RewindConflictError struct{ msg string }

func (e *RewindConflictError) Error() string { return e.msg }

type RewindOptions struct {
	WarmHistory      []*Message
	RequireRetryable bool
	RequireComposite bool
	AdoptRowIDs      bool
}

type RewindPins struct {
	PreserveCompactionHandoff bool
	ExpectedActiveIDs         []int64
	ExpectedTargetContent     *Content
}

type RewindPlan struct {
	TargetRowID           int64
	ExpectedActiveIDs     []int64
	ExpectedTargetContent Content
	LiveText              sql.Null[string]
	scaffold              *Message
	prefix                []*Message
	durablePrefix         []*Message
	warm                  bool
	liveView              *Message
	userTurns             int
	userOrdinal           int
}

type RewindOutcome struct {
	Prefix       []*Message
	LiveView     *Message
	LiveText     string
	RewoundCount int
	TurnsUndone  int
}

type rewound struct {
	target        *msgRow
	ids           []int64
	newHead       sql.Null[int64]
	replacementID sql.Null[int64]
}

func carrierDurableMetadata(raw json.RawMessage) json.RawMessage {
	meta, ok := objectMembers(raw)
	if !ok {
		return nil
	}
	var kept []configMember
	for _, key := range summaryCarrierDurableMetadataKeys {
		if v, has := memberValue(meta, key); has {
			kept = append(kept, configMember{key, v})
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return rawObject(kept)
}

func splitUserMessage(m *Message) (*Message, *Message) {
	if m == nil || m.Role != "user" {
		return nil, nil
	}
	split := splitUserOriginatedTurn(userTurn{content: m.Content, displayKind: m.DisplayKind.V, summary: m.CompressedSummary})
	var handoff *Message
	if split.handoff {
		handoff = &Message{Role: "user", Content: split.handoffContent, CompressedSummary: true,
			DisplayKind: sql.Null[string]{V: "hidden", Valid: true}, Timestamp: m.Timestamp}
	}
	if !split.live {
		return handoff, nil
	}
	live := *m
	live.Content = split.liveContent
	live.CompressedSummary, live.Persisted = false, false
	if split.handoff {
		live.RowID, live.Snapshot = 0, ""
	}
	live.DisplayKind = sql.Null[string]{}
	live.DisplayMetadata = carrierDurableMetadata(m.DisplayMetadata)
	live.APIContent = sql.Null[string]{}
	return handoff, &live
}

func userTurnIndices(messages []*Message) []int {
	var out []int
	for i, m := range messages {
		if _, live := splitUserMessage(m); live != nil {
			out = append(out, i)
		}
	}
	return out
}

func typedPromptContent(c Content, message *Message) Content {
	meta, ok := objectMembers(message.DisplayMetadata)
	if !ok {
		return c
	}
	typed, ok := memberText(meta, withheldPromptKey)
	if !ok {
		return c
	}
	if text, ok := c.Text(); ok {
		return TextContent(strings.ReplaceAll(text, withheldPromptPlaceholder, typed))
	}
	parts, ok := contentParts(c)
	if !ok {
		return c
	}
	for i, p := range parts {
		if !p.isObj {
			continue
		}
		if text, ok := p.partText(); ok {
			parts[i] = p.withText(strings.ReplaceAll(text, withheldPromptPlaceholder, typed))
		}
	}
	return partsContent(parts)
}

func historyBeforeUserTurn(messages []*Message, index int) ([]*Message, *Message, error) {
	if index < 0 || index >= len(messages) {
		return nil, nil, &RewindUnavailableError{"user turn index is outside the transcript"}
	}
	handoff, live := splitUserMessage(messages[index])
	if live == nil {
		return nil, nil, &RewindUnavailableError{"selected row is not a user-originated turn"}
	}
	live.Content = typedPromptContent(live.Content, messages[index])
	prefix := make([]*Message, 0, index+1)
	for _, m := range messages[:index] {
		c := *m
		prefix = append(prefix, &c)
	}
	if handoff != nil {
		prefix = append(prefix, handoff)
	}
	return prefix, live, nil
}

func sameContentValue(a, b Content) bool {
	if a.kind == contentStructured && b.kind == contentStructured {
		return jsonCompareKey(a.raw) == jsonCompareKey(b.raw)
	}
	return a.equal(b)
}

func (h TranscriptHooks) comparisonContent(m *Message) Content {
	return loadedViewContent(m.Role, h.DurableContent(m.Content))
}

func (s *Store) PlanUserTurnRewind(ctx context.Context, sessionID string, userOrdinal int, o RewindOptions) (*RewindPlan, error) {
	hooks := s.tables.state.options.Transcript
	if (o.WarmHistory != nil && (hooks.EphemeralScaffolding == nil || hooks.DurableContent == nil)) ||
		(o.RequireRetryable && hooks.RetryableUserText == nil) {
		return nil, ErrNoRewindHooks
	}
	expected, err := s.GetActiveMessageIDs(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if expected == nil {
		expected = []int64{}
	}
	stored, err := s.GetMessagesAsConversation(ctx, sessionID, ConversationQuery{IncludeRowIDs: true})
	if err != nil {
		return nil, err
	}
	durable, err := s.GetMessagesAsConversation(ctx, sessionID, ConversationQuery{IncludeRowIDs: true, RepairAlternation: true})
	if err != nil {
		return nil, err
	}
	durableUser := userTurnIndices(durable)
	if userOrdinal < 0 {
		userOrdinal = max(len(durableUser)+userOrdinal, 0)
	}
	if userOrdinal >= len(durableUser) {
		return nil, &RewindUnavailableError{"target user message is no longer in session history"}
	}
	targetIndex := durableUser[userOrdinal]
	target := durable[targetIndex]
	durablePrefix, liveView, err := historyBeforeUserTurn(durable, targetIndex)
	if err != nil {
		return nil, err
	}
	scaffold, _ := splitUserMessage(target)
	if o.RequireComposite && scaffold == nil {
		return nil, &RewindUnavailableError{"target user message is not a compaction carrier"}
	}
	plan := &RewindPlan{ExpectedActiveIDs: expected, scaffold: scaffold, prefix: durablePrefix, durablePrefix: durablePrefix,
		liveView: liveView, userTurns: len(durableUser), userOrdinal: userOrdinal}
	if o.WarmHistory != nil {
		var warm []*Message
		for _, m := range o.WarmHistory {
			if m != nil && !hooks.EphemeralScaffolding(m) {
				warm = append(warm, m)
			}
		}
		warmUser := userTurnIndices(warm)
		if len(warmUser) != len(durableUser) {
			return nil, &RewindConflictError{rewindHistoryChanged}
		}
		prefix, warmLive, err := historyBeforeUserTurn(warm, warmUser[userOrdinal])
		if err != nil {
			return nil, err
		}
		if !sameContentValue(hooks.comparisonContent(liveView), hooks.comparisonContent(warmLive)) {
			return nil, &RewindConflictError{rewindHistoryChanged}
		}
		plan.prefix, plan.warm = prefix, true
	}
	if o.RequireRetryable {
		text, err := hooks.RetryableUserText(liveView.Content)
		if err != nil {
			return nil, err
		}
		plan.LiveText = sql.Null[string]{V: text, Valid: true}
	}
	if target.RowID <= 0 {
		return nil, &RewindConflictError{"rewind target has no durable row identity"}
	}
	plan.TargetRowID = target.RowID
	var storedView *Message
	for _, m := range stored {
		if m.RowID == target.RowID {
			_, storedView = splitUserMessage(m)
			break
		}
	}
	if storedView == nil {
		return nil, &RewindConflictError{rewindHistoryChanged}
	}
	plan.ExpectedTargetContent = storedView.Content
	return plan, nil
}

func (s *Store) RewindUserTurn(ctx context.Context, sessionID string, userOrdinal int, o RewindOptions) (*RewindOutcome, error) {
	hooks := s.tables.state.options.Transcript
	if (!o.RequireRetryable && hooks.FlattenText == nil) || (o.AdoptRowIDs && o.WarmHistory != nil && hooks.DurableContent == nil) {
		return nil, ErrNoRewindHooks
	}
	plan, err := s.PlanUserTurnRewind(ctx, sessionID, userOrdinal, o)
	if err != nil {
		return nil, err
	}
	var result *rewound
	var replacementUID sql.NullString
	err = s.Write(ctx, WritePatience, func(c Conn) error {
		var err error
		result, err = s.rewindIn(ctx, c, sessionID, plan.TargetRowID, RewindPins{PreserveCompactionHandoff: plan.scaffold != nil,
			ExpectedActiveIDs: plan.ExpectedActiveIDs, ExpectedTargetContent: &plan.ExpectedTargetContent})
		if err != nil || !result.replacementID.Valid {
			return err
		}
		return c.QueryRowContext(ctx, "SELECT message_uid FROM messages WHERE id = ?", result.replacementID.V).Scan(&replacementUID)
	})
	if err != nil {
		return nil, err
	}
	prefix := plan.prefix
	if plan.scaffold != nil {
		if !result.replacementID.Valid || len(plan.durablePrefix) == 0 || len(prefix) == 0 {
			return nil, &RewindConflictError{"rewind did not retain its compaction handoff"}
		}
		head := plan.durablePrefix[len(plan.durablePrefix)-1]
		head.RowID, head.Persisted = result.replacementID.V, true
		if replacementUID.String != "" {
			head.UID = replacementUID.String
		}
		prefix[len(prefix)-1] = head
	}
	if o.AdoptRowIDs && plan.warm && len(prefix) == len(plan.durablePrefix) {
		same := true
		for i, warm := range prefix {
			d := plan.durablePrefix[i]
			if warm.Role != d.Role || (warm.DisplayKind.V != "") != (d.DisplayKind.V != "") ||
				!sameContentValue(hooks.comparisonContent(warm), hooks.comparisonContent(d)) {
				same = false
				break
			}
		}
		for i := 0; same && i < len(prefix); i++ {
			d := plan.durablePrefix[i]
			if d.RowID > 0 {
				prefix[i].RowID = d.RowID
			}
			if d.UID != "" {
				prefix[i].UID = d.UID
			}
		}
	}
	liveText := plan.LiveText.V
	if !plan.LiveText.Valid {
		liveText = hooks.FlattenText(plan.liveView.Content)
	}
	return &RewindOutcome{Prefix: prefix, LiveView: plan.liveView, LiveText: liveText, RewoundCount: len(result.ids),
		TurnsUndone: plan.userTurns - plan.userOrdinal}, nil
}

func (s *Store) RewindIn(ctx context.Context, c Conn, sessionID string, targetMessageID int64, pins RewindPins) error {
	_, err := s.rewindIn(ctx, c, sessionID, targetMessageID, pins)
	return err
}

func splitRewindTarget(target *msgRow, expected *Content, preserve bool) (*Message, error) {
	if !cellTruthy(target.active) {
		return nil, &RewindUnavailableError{"rewind target is not active"}
	}
	split := splitUserOriginatedTurn(userTurn{content: decodeContent(target.content), displayKind: textCell(target.displayKind),
		summary: cellTruthy(target.compressedSummary)})
	if !split.live {
		return nil, &RewindUnavailableError{"rewind target is not a user-originated turn"}
	}
	live := split.liveContent
	if text, ok := live.Text(); ok {
		live = TextContent(strings.TrimFunc(SanitizeContext(text), previewSpace))
	}
	if expected != nil && !sameContentValue(live, *expected) {
		return nil, &RewindConflictError{"rewind target changed before it could be persisted"}
	}
	if !preserve {
		return nil, nil
	}
	if !split.handoff {
		return nil, &RewindUnavailableError{"preserve_compaction_handoff requires an active composite carrier"}
	}
	handoff := &Message{Role: "user", Content: split.handoffContent, CompressedSummary: true,
		DisplayKind: sql.Null[string]{V: "hidden", Valid: true}}
	if ts, ok := floatCell(target.timestamp); ok {
		handoff.Timestamp = sql.Null[float64]{V: ts, Valid: true}
	}
	return handoff, nil
}

func activeTranscriptCounts(ctx context.Context, c Conn, sessionID string) (int, int, error) {
	rows, err := c.QueryContext(ctx, "SELECT tool_calls FROM messages WHERE session_id = ? AND active = 1", sessionID)
	if err != nil {
		return 0, 0, err
	}
	count, toolCalls := 0, 0
	for rows.Next() {
		var cell any
		if err := rows.Scan(&cell); err != nil {
			rows.Close()
			return 0, 0, err
		}
		count++
		toolCalls += toolCallsLen(cell, 1)
	}
	return count, toolCalls, rowsDone(rows)
}

func (s *Store) rewindIn(ctx context.Context, c Conn, sessionID string, targetMessageID int64, pins RewindPins) (*rewound, error) {
	if err := s.checkTranscriptWriteGuards(ctx, c, sessionID, writeGuards{rejectActiveTurnLease: true, rejectActiveCompressionLock: true}); err != nil {
		return nil, err
	}
	if pins.ExpectedActiveIDs != nil {
		active, err := queryInt64s(ctx, c, activeIDsSQL, sessionID)
		if err != nil {
			return nil, err
		}
		if !equalInt64s(active, pins.ExpectedActiveIDs) {
			return nil, &RewindConflictError{"active transcript changed before the rewind could be persisted"}
		}
	}
	target, err := queryMsgRow(c.QueryContext(ctx, "SELECT * FROM messages WHERE id = ? AND session_id = ?", targetMessageID, sessionID))
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, &RewindUnavailableError{fmt.Sprintf("message %d not found in session %s", targetMessageID, sessionID)}
	}
	if role := target.roleText(); role != "user" {
		return nil, &RewindUnavailableError{fmt.Sprintf("rewind target must be a 'user' message (got role=%s, id=%d)", quotedText(role), targetMessageID)}
	}
	var replacement *Message
	if pins.PreserveCompactionHandoff || pins.ExpectedTargetContent != nil {
		if replacement, err = splitRewindTarget(target, pins.ExpectedTargetContent, pins.PreserveCompactionHandoff); err != nil {
			return nil, err
		}
	}
	out := &rewound{target: target}
	if out.ids, err = queryInt64s(ctx, c, "SELECT id FROM messages WHERE session_id = ? AND id >= ? AND active = 1", sessionID, targetMessageID); err != nil {
		return nil, err
	}
	for _, chunk := range chunks(out.ids, sqlInChunk) {
		if _, err := c.ExecContext(ctx, "UPDATE messages SET active = 0 WHERE id IN ("+bindMarks(len(chunk))+")", int64Args(chunk)...); err != nil {
			return nil, err
		}
	}
	if replacement != nil {
		if _, _, err := s.insertMessageRows(ctx, c, sessionID, []*Message{replacement}, true); err != nil {
			return nil, err
		}
		out.replacementID = sql.Null[int64]{V: replacement.RowID, Valid: true}
	}
	if _, err := c.ExecContext(ctx, "UPDATE sessions SET rewind_count = COALESCE(rewind_count, 0) + 1 WHERE id = ?", sessionID); err != nil {
		return nil, err
	}
	count, toolCalls, err := activeTranscriptCounts(ctx, c, sessionID)
	if err != nil {
		return nil, err
	}
	if _, err := c.ExecContext(ctx, setCountersSQL+" WHERE id = ?", count, toolCalls, sessionID); err != nil {
		return nil, err
	}
	if err := c.QueryRowContext(ctx, "SELECT MAX(id) FROM messages WHERE session_id = ? AND active = 1", sessionID).Scan(&out.newHead); err != nil {
		return nil, err
	}
	return out, nil
}
