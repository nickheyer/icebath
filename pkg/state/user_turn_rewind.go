package state

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const rewindHistoryChanged = "session history changed before the rewind could be persisted"

// IndexError and ValueError of the rewind prefix helper
var (
	ErrUserTurnIndex         = errors.New("user turn index is outside the transcript")
	ErrNotUserOriginatedTurn = errors.New("selected row is not a user-originated turn")
)

// ValueError a retry raises before any destructive mutation
type retryRefusal struct{ msg string }

func (e *retryRefusal) Error() string      { return e.msg }
func (e *retryRefusal) PyTypeName() string { return "ValueError" }

// Python ValueError reasons a retry refuses with before any destructive mutation
var (
	ErrRetryNonText        error = &retryRefusal{"retry does not support non-text content"}
	ErrRetryMediaPart      error = &retryRefusal{"retry does not support media or unknown content parts"}
	ErrRetryAnnotatedPart  error = &retryRefusal{"retry cannot losslessly flatten annotated text parts"}
	ErrRetryPartWithoutTxt error = &retryRefusal{"retry text parts must contain text"}
	ErrRetryNoText         error = &retryRefusal{"retry found no text to send"}
)

var (
	retryTextPartTypes        = map[any]bool{"text": true, "input_text": true, "output_text": true}
	persistImagePartTypes     = map[string]bool{"image": true, "image_url": true, "input_image": true}
	ephemeralScaffoldingFlags = []string{
		"_empty_recovery_synthetic", "_empty_terminal_sentinel", "_thinking_prefill", "_verification_stop_synthetic",
		"_pre_verify_synthetic", "_kanban_stop_synthetic", "_dropped_toolcall_nudge",
	}
)

// Live human authored projection of a user row, nil when the row carries none
func UserOriginatedTurnView(message any) map[string]any {
	m, ok := message.(map[string]any)
	if !ok {
		return nil
	}
	_, live := SplitUserOriginatedTurn(m)
	return live
}

// Rewind prefix and canonical live view for index, a composite carrier keeping its handoff at the head
func HistoryBeforeUserOriginatedTurn(messages []map[string]any, index int) ([]map[string]any, map[string]any, error) {
	if index < 0 || index >= len(messages) {
		return nil, nil, ErrUserTurnIndex
	}
	handoff, live := SplitUserOriginatedTurn(messages[index])
	if live == nil {
		return nil, nil, ErrNotUserOriginatedTurn
	}
	live["content"] = TypedPromptContent(live["content"], messages[index])
	prefix := make([]map[string]any, 0, index+1)
	for _, m := range messages[:index] {
		copied := make(map[string]any, len(m))
		for k, v := range m {
			copied[k] = v
		}
		prefix = append(prefix, copied)
	}
	if handoff != nil {
		prefix = append(prefix, handoff)
	}
	return prefix, live, nil
}

// Lossless retry text of live user content, media and annotated or unknown parts failing closed
func RetryableUserText(content any) (string, error) {
	var parts []any
	switch c := content.(type) {
	case string:
		parts = []any{c}
	case []any:
		parts = c
	case []map[string]any:
		for _, p := range c {
			parts = append(parts, p)
		}
	default:
		return "", ErrRetryNonText
	}
	var chunks strings.Builder
	for _, part := range parts {
		if s, ok := part.(string); ok {
			chunks.WriteString(s)
			continue
		}
		p, ok := contentDict(part)
		if !ok {
			return "", ErrRetryNonText
		}
		if !retryTextPartTypes[p["type"]] {
			return "", ErrRetryMediaPart
		}
		for k := range p {
			if k != "type" && k != "text" {
				return "", ErrRetryAnnotatedPart
			}
		}
		text, isStr := p["text"].(string)
		if !isStr {
			return "", ErrRetryPartWithoutTxt
		}
		chunks.WriteString(text)
	}
	text := chunks.String()
	if pystr.Strip(text) == "" {
		return "", ErrRetryNoText
	}
	return text, nil
}

// Reports internal recovery scaffolding that must never reach the durable transcript
func IsEphemeralScaffolding(msg any) bool {
	m, ok := msg.(map[string]any)
	if !ok {
		return false
	}
	for _, flag := range ephemeralScaffoldingFlags {
		if pyTruthy(m[flag]) {
			return true
		}
	}
	return false
}

// True for the multimodal tool result envelope carrying a content list
func IsMultimodalToolResult(value any) bool {
	m, ok := contentDict(value)
	if !ok {
		return false
	}
	flag, ok := m["_multimodal"].(bool)
	if !ok || !flag {
		return false
	}
	_, isList := m["content"].([]any)
	return isList
}

// Text view of a multimodal envelope, its summary, else its text parts, else a fixed marker
func MultimodalEnvelopeSummary(value any) string {
	m, _ := contentDict(value)
	if summary, ok := m["text_summary"]; ok && pyTruthy(summary) {
		return fmt.Sprint(summary)
	}
	var parts []string
	for _, p := range m["content"].([]any) {
		if part, ok := contentDict(p); ok {
			if t, _ := part["type"].(string); t == "text" {
				parts = append(parts, fmt.Sprint(part["text"]))
			}
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	return "[multimodal tool result]"
}

// Text only db projection, multimodal envelopes as their summary and images as screenshot markers
func DurableContent(content any) any {
	if IsMultimodalToolResult(content) {
		return MultimodalEnvelopeSummary(content)
	}
	parts, ok := content.([]any)
	if !ok {
		return content
	}
	var txt []string
	for _, p := range parts {
		part, ok := contentDict(p)
		if !ok {
			continue
		}
		kind, _ := part["type"].(string)
		switch {
		case kind == "text":
			text, has := part["text"]
			if !has {
				text = ""
			}
			txt = append(txt, pyrepr.Str(text))
		case persistImagePartTypes[kind]:
			txt = append(txt, imageMarker(part))
		}
	}
	if len(txt) == 0 {
		return nil
	}
	return strings.Join(txt, "\n")
}

// Screenshot marker carrying a caller chosen detail so a replay rebuilds the part the live turn sent
func imageMarker(part map[string]any) string {
	detail := part["detail"]
	if !pyTruthy(detail) {
		detail = nil
		if url, ok := contentDict(part["image_url"]); ok {
			detail = url["detail"]
		}
	}
	if s, ok := detail.(string); ok && (s == "low" || s == "high" || s == "auto") {
		return "[screenshot detail=" + s + "]"
	}
	return "[screenshot]"
}

// Dict view of a content part decoded as a row object, an ordered object or a plain map
func contentDict(v any) (map[string]any, bool) {
	switch d := v.(type) {
	case map[string]any:
		return d, true
	case pyObject:
		m := make(map[string]any, len(d))
		for _, kv := range d {
			m[kv.Key] = kv.Value
		}
		return m, true
	case *pyjson.Object:
		if d == nil {
			return nil, false
		}
		return d.Vals, true
	}
	return nil, false
}

// Items in key order so pyrepr compares, prints and names a row object as the dict it decodes
func (o pyObject) PyItems() iter.Seq2[any, any] {
	return func(yield func(any, any) bool) {
		for _, kv := range o {
			if !yield(kv.Key, kv.Value) {
				return
			}
		}
	}
}

// Requested user turn is not a rewindable target of the active transcript, a ValueError subclass
type RewindTargetUnavailableError struct{ Msg string }

func (e *RewindTargetUnavailableError) Error() string      { return e.Msg }
func (e *RewindTargetUnavailableError) PyTypeName() string { return "RewindTargetUnavailableError" }

// Python RuntimeError raised when the warm and durable histories disagree or an identity is missing
type RewindRuntimeError struct{ Msg string }

func (e *RewindRuntimeError) Error() string      { return e.Msg }
func (e *RewindRuntimeError) PyTypeName() string { return "RuntimeError" }

// Keyword arguments of a user turn rewind
type RewindUserTurnOptions struct {
	WarmHistory                        []map[string]any
	HasWarmHistory                     bool
	RequireRetryable, RequireComposite bool
	AdoptRowIDs                        bool
}

// Every read and check a rewind makes before its write
type RewindPlan struct {
	TargetRowID           int64
	ExpectedActiveIDs     []int64
	ExpectedTargetContent any
	Scaffold              map[string]any
	Prefix                []map[string]any
	DurablePrefix         []map[string]any
	LiveView              map[string]any
	LiveText              *string
	DurableUser           []int
	UserOrdinal           int
	prefixIsDurable       bool
}

// Installed history, live projection and retry text of a committed rewind with its counts
type RewindOutcome struct {
	Prefix       []map[string]any
	LiveView     map[string]any
	LiveText     string
	RewoundCount int
	TurnsUndone  int
}

// Indices of the user originated turns of a history
func userTurnIndices(messages []map[string]any) []int {
	var out []int
	for i, m := range messages {
		if UserOriginatedTurnView(m) != nil {
			out = append(out, i)
		}
	}
	return out
}

// Indices of the user originated turns of a history, the gateway session store's rewind addressing
func UserTurnIndices(messages []map[string]any) []int { return userTurnIndices(messages) }

// Content projected as the durable row stores it so a warm row and its durable twin compare equal
func rewindComparisonContent(message map[string]any) any {
	role, _ := message["role"].(string)
	return loadedViewContent(role, DurableContent(message["content"]))
}

// Durable projection of a message's content for the warm versus live rewind comparison
func RewindComparisonContent(message map[string]any) any { return rewindComparisonContent(message) }

// Refusal text of a rewind whose warm view diverged from the durable rows
func RewindHistoryChanged() string { return rewindHistoryChanged }

// Plans the rewind to just before a user turn, every read and check made and nothing written
func (d *SessionDB) PlanUserTurnRewind(ctx context.Context, sessionID string, userOrdinal int, o RewindUserTurnOptions) (*RewindPlan, error) {
	expectedActiveIDs, err := d.GetActiveMessageIDs(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	stored, err := d.GetMessagesAsConversation(ctx, sessionID, ConversationOptions{IncludeRowIDs: true})
	if err != nil {
		return nil, err
	}
	durable, err := d.GetMessagesAsConversation(ctx, sessionID, ConversationOptions{IncludeRowIDs: true, RepairAlternation: true})
	if err != nil {
		return nil, err
	}
	durableUser := userTurnIndices(durable)
	if userOrdinal < 0 {
		userOrdinal = max(len(durableUser)+userOrdinal, 0)
	}
	if userOrdinal >= len(durableUser) {
		return nil, &RewindTargetUnavailableError{"target user message is no longer in session history"}
	}
	targetIndex := durableUser[userOrdinal]
	target := durable[targetIndex]
	durablePrefix, liveView, err := HistoryBeforeUserOriginatedTurn(durable, targetIndex)
	if err != nil {
		return nil, err
	}
	scaffold, _ := SplitUserOriginatedTurn(target)
	if o.RequireComposite && scaffold == nil {
		return nil, &RewindTargetUnavailableError{"target user message is not a compaction carrier"}
	}
	plan := &RewindPlan{Prefix: durablePrefix, prefixIsDurable: true}
	if o.HasWarmHistory {
		var warm []map[string]any
		for _, m := range o.WarmHistory {
			if !IsEphemeralScaffolding(m) {
				warm = append(warm, m)
			}
		}
		warmUser := userTurnIndices(warm)
		if len(warmUser) != len(durableUser) {
			return nil, &RewindRuntimeError{rewindHistoryChanged}
		}
		prefix, warmLiveView, err := HistoryBeforeUserOriginatedTurn(warm, warmUser[userOrdinal])
		if err != nil {
			return nil, err
		}
		if !pyrepr.Equal(rewindComparisonContent(liveView), rewindComparisonContent(warmLiveView)) {
			return nil, &RewindRuntimeError{rewindHistoryChanged}
		}
		plan.Prefix, plan.prefixIsDurable = prefix, false
	}
	if o.RequireRetryable {
		text, err := RetryableUserText(liveView["content"])
		if err != nil {
			return nil, err
		}
		plan.LiveText = &text
	}
	targetRowID, ok := rowIDOf(target["_row_id"])
	if !ok {
		return nil, &RewindRuntimeError{"rewind target has no durable row identity"}
	}
	var storedView map[string]any
	found := false
	for _, m := range stored {
		if id, isInt := rowIDOf(m["_row_id"]); isInt && id == targetRowID {
			storedView, found = UserOriginatedTurnView(m), true
			break
		}
	}
	if !found || storedView == nil {
		return nil, &RewindRuntimeError{rewindHistoryChanged}
	}
	plan.TargetRowID, plan.ExpectedActiveIDs, plan.ExpectedTargetContent = targetRowID, expectedActiveIDs, storedView["content"]
	plan.Scaffold, plan.DurablePrefix, plan.LiveView = scaffold, durablePrefix, liveView
	plan.DurableUser, plan.UserOrdinal = durableUser, userOrdinal
	return plan, nil
}

// Integer row identity of a value, false for anything Python would not take as an int
func rowIDOf(value any) (int64, bool) {
	if _, isBool := value.(bool); isBool {
		return 0, false
	}
	switch v := value.(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}

// Soft deletes from a user turn onward, validated against the warm history, a composite carrier keeping its handoff
func (d *SessionDB) RewindUserTurn(ctx context.Context, sessionID string, userOrdinal int, o RewindUserTurnOptions) (*RewindOutcome, error) {
	plan, err := d.PlanUserTurnRewind(ctx, sessionID, userOrdinal, o)
	if err != nil {
		return nil, err
	}
	prefix, durablePrefix := plan.Prefix, plan.DurablePrefix
	result, err := d.RewindToMessage(ctx, sessionID, plan.TargetRowID, RewindOptions{
		PreserveCompactionHandoff: plan.Scaffold != nil, ExpectedActiveIDs: plan.ExpectedActiveIDs,
		ExpectedTargetContent: plan.ExpectedTargetContent})
	if err != nil {
		var typed interface{ PyTypeName() string }
		if errors.As(err, &typed) && typed.PyTypeName() == "ValueError" {
			return nil, &RewindTargetUnavailableError{err.Error()}
		}
		return nil, err
	}
	if plan.Scaffold != nil {
		raw, _ := result.Get("replacement_message_id")
		replacementID, isInt := rowIDOf(raw)
		if !isInt || len(durablePrefix) == 0 {
			return nil, &RewindRuntimeError{"rewind did not retain its compaction handoff"}
		}
		if len(prefix) == 0 {
			return nil, fmt.Errorf("IndexError: list assignment index out of range")
		}
		head := durablePrefix[len(durablePrefix)-1]
		head["_row_id"], head[DBPersistedMarker] = replacementID, true
		if uid, _ := result.Get("replacement_message_uid"); pyTruthy(uid) {
			head[MessageUID] = uid
		}
		prefix[len(prefix)-1] = head
	}
	if o.AdoptRowIDs && !plan.prefixIsDurable && len(prefix) == len(durablePrefix) && warmMatchesDurable(prefix, durablePrefix) {
		for i, warm := range prefix {
			if id, isInt := rowIDOf(durablePrefix[i]["_row_id"]); isInt {
				warm["_row_id"] = id
			}
			if uid, ok := MessageUIDOrNone(durablePrefix[i]); ok && uid != "" {
				warm[MessageUID] = uid
			}
		}
	}
	liveText := ""
	if plan.LiveText != nil {
		liveText = *plan.LiveText
	} else {
		liveText = FlattenMessageText(plan.LiveView["content"], "\n")
	}
	count, _ := result.Get("rewound_count")
	rewound, _ := count.(int)
	return &RewindOutcome{Prefix: prefix, LiveView: plan.LiveView, LiveText: liveText,
		RewoundCount: rewound, TurnsUndone: len(plan.DurableUser) - plan.UserOrdinal}, nil
}

// Whether each warm row has its durable twin's role, display kind presence and comparison content
func warmMatchesDurable(prefix, durablePrefix []map[string]any) bool {
	for i, warm := range prefix {
		durable := durablePrefix[i]
		if !pyrepr.Equal(warm["role"], durable["role"]) || pyTruthy(warm["display_kind"]) != pyTruthy(durable["display_kind"]) {
			return false
		}
		if !pyrepr.Equal(rewindComparisonContent(warm), rewindComparisonContent(durable)) {
			return false
		}
	}
	return true
}
