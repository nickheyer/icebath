package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/state"
)

// UsageAnchorModelConfigKey is the session model config key holding the persisted usage anchor
const UsageAnchorModelConfigKey = "_usage_anchor"

// UsageAnchor is provider usage at capture plus the transcript position and prefix it priced
type UsageAnchor struct {
	PromptTokens     int
	CompletionTokens int
	BaseCount        int
	BaseLastRole     string
	BaseLastFP       string
	BasePrefixFP     string
}

// ToMap is the persisted anchor blob, an empty last role written as null
func (a UsageAnchor) ToMap() map[string]any {
	var role any
	if a.BaseLastRole != "" {
		role = a.BaseLastRole
	}
	return map[string]any{"prompt_tokens": a.PromptTokens, "completion_tokens": a.CompletionTokens, "base_count": a.BaseCount,
		"base_last_role": role, "base_last_fp": a.BaseLastFP, "base_prefix_fp": a.BasePrefixFP}
}

// Python json.dumps text with sorted keys, ASCII escapes and compact separators, str for unknown values
func usageAnchorDumps(v any) string {
	text, ok := pyJSONDumps(v)
	if !ok {
		text, _ = pyJSONDumps(fmt.Sprint(v))
	}
	var b strings.Builder
	for _, r := range text {
		switch {
		case r < 0x80:
			b.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

// Plain JSON shape of a typed value through its JSON tags
func usageAnchorPlain(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var out any
	if dec.Decode(&out) != nil {
		return fmt.Sprint(v)
	}
	return out
}

// MessageFingerprint is a stable digest of one message over its provider visible persisted fields
func MessageFingerprint(m *state.Message) string {
	if m == nil {
		return ""
	}
	payload := map[string]any{"role": m.Role}
	if m.Content != nil {
		payload["content"] = usageAnchorPlain(m.Content)
	}
	if m.APIContent != "" {
		payload["api_content"] = m.APIContent
	}
	if m.ToolCallID != "" {
		payload["tool_call_id"] = m.ToolCallID
	}
	if m.ToolCalls != nil {
		payload["tool_calls"] = usageAnchorPlain(m.ToolCalls)
	}
	sum := sha256.Sum256([]byte(usageAnchorDumps(payload)))
	return hex.EncodeToString(sum[:])
}

// Digest of the whole provider priced prefix, empty when the transcript is shorter than the base
func pricedPrefixFingerprint(messages []*state.Message, baseCount int) string {
	if baseCount <= 0 || len(messages) < baseCount {
		return ""
	}
	fps := make([]any, 0, baseCount)
	for _, m := range messages[:baseCount] {
		fp := MessageFingerprint(m)
		if fp == "" {
			return ""
		}
		var role any
		if m.Role != "" {
			role = m.Role
		}
		fps = append(fps, []any{role, fp})
	}
	sum := sha256.Sum256([]byte(usageAnchorDumps(fps)))
	return hex.EncodeToString(sum[:])
}

// CaptureUsageAnchor builds an anchor from provider reported usage, nil when usage or the transcript is unusable
func CaptureUsageAnchor(promptTokens, completionTokens any, messages []*state.Message) *UsageAnchor {
	pt, ok := anchorInt(promptTokens)
	if !ok {
		return nil
	}
	ct, ok := anchorInt(completionTokens)
	if !ok || pt <= 0 || len(messages) == 0 {
		return nil
	}
	last := messages[len(messages)-1]
	role := ""
	if last != nil {
		role = last.Role
	}
	return &UsageAnchor{PromptTokens: pt, CompletionTokens: max(0, ct), BaseCount: len(messages), BaseLastRole: role,
		BaseLastFP: MessageFingerprint(last), BasePrefixFP: pricedPrefixFingerprint(messages, len(messages))}
}

// Python int of a falsy, numeric or integer text value, false for anything int rejects
func anchorInt(v any) (int, bool) {
	if !auTruthy(v) {
		return 0, true
	}
	switch t := v.(type) {
	case string:
		n, ok := pyIntText(t)
		return int(n), ok
	case json.Number:
		if n, ok := pyIntText(string(t)); ok {
			return int(n), true
		}
	case int:
		return t, true
	case int64:
		return int(t), true
	}
	if !auIsNum(v) {
		return 0, false
	}
	f, err := auFloat(v)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return int(math.Trunc(f)), true
}

// Whether the transcript still carries the priced message and the full priced prefix of an anchor
func anchorMatches(messages []*state.Message, a *UsageAnchor) bool {
	if a.BaseCount <= 0 || len(messages) < a.BaseCount {
		return false
	}
	base := messages[a.BaseCount-1]
	if base == nil || base.Role != a.BaseLastRole {
		return false
	}
	if a.BaseLastFP == "" || MessageFingerprint(base) != a.BaseLastFP {
		return false
	}
	return a.BasePrefixFP != "" && pricedPrefixFingerprint(messages, a.BaseCount) == a.BasePrefixFP
}

// AnchoredContextTokens is anchored prompt plus completion tokens and an estimate of messages appended since
func AnchoredContextTokens(ctx context.Context, messages []*state.Message, anchor *UsageAnchor, chargeStaleThinking bool) (int, bool) {
	if anchor == nil || !anchorMatches(messages, anchor) {
		return 0, false
	}
	total := anchor.PromptTokens + anchor.CompletionTokens
	delta := messages[anchor.BaseCount:]
	if len(delta) > 0 && delta[0] != nil && delta[0].Role == "assistant" {
		delta = delta[1:]
	}
	if len(delta) > 0 {
		total += EstimateMessagesTokens(delta, CurrentImageTokenCost(ctx), chargeStaleThinking)
	}
	return total, true
}

// Validated anchor of a persisted blob, nil when it is not a usable anchor
func usageAnchorFromAny(v any) *UsageAnchor {
	var m map[string]any
	switch t := v.(type) {
	case map[string]any:
		m = t
	case *UsageAnchor:
		if t == nil {
			return nil
		}
		m = t.ToMap()
	case UsageAnchor:
		m = t.ToMap()
	default:
		return nil
	}
	pt, ok1 := anchorInt(m["prompt_tokens"])
	ct, ok2 := anchorInt(m["completion_tokens"])
	base, ok3 := anchorInt(m["base_count"])
	if !ok1 || !ok2 || !ok3 {
		return nil
	}
	fp, _ := m["base_last_fp"].(string)
	prefix, _ := m["base_prefix_fp"].(string)
	if pt <= 0 || base <= 0 || fp == "" || prefix == "" {
		return nil
	}
	role, _ := m["base_last_role"].(string)
	return &UsageAnchor{PromptTokens: pt, CompletionTokens: max(0, ct), BaseCount: base, BaseLastRole: role, BaseLastFP: fp, BasePrefixFP: prefix}
}

// UsageAnchorStore is the session row surface that persists and reads model config keys
type UsageAnchorStore interface {
	PatchModelConfig(id string, patch map[string]any) error
	GetModelConfigValue(id, key string) (any, bool, error)
}

// UsageAnchorState is the anchor state an agent carries and mirrors to its session row
type UsageAnchorState struct {
	Anchor          *UsageAnchor
	TurnBaseAnchor  *UsageAnchor
	PersistDisabled bool
	SessionID       string
	Store           UsageAnchorStore
}

// Persist writes or clears the session row anchor blob, failures logged and dropped
func (s *UsageAnchorState) Persist(anchor *UsageAnchor) {
	if s.PersistDisabled || s.SessionID == "" || s.Store == nil {
		return
	}
	var blob any
	if a := usageAnchorFromAny(anchor); a != nil {
		blob = a.ToMap()
	}
	if err := s.Store.PatchModelConfig(s.SessionID, map[string]any{UsageAnchorModelConfigKey: blob}); err != nil {
		modelsLog.Debug("usage anchor persist failed", "err", err)
	}
}

// Set installs an anchor, nil clearing it, and mirrors it to the session row
func (s *UsageAnchorState) Set(anchor *UsageAnchor, turnBase bool) {
	s.Anchor = anchor
	if turnBase || anchor == nil {
		s.TurnBaseAnchor = anchor
	}
	s.Persist(anchor)
}

// Restore adopts the persisted anchor while the history carries its priced prefix, clearing a stale blob
func (s *UsageAnchorState) Restore(history []*state.Message, haveHistory bool) {
	if s.Anchor != nil || s.PersistDisabled || s.SessionID == "" || s.Store == nil || !haveHistory {
		return
	}
	raw, _, err := s.Store.GetModelConfigValue(s.SessionID, UsageAnchorModelConfigKey)
	if err != nil {
		modelsLog.Debug("usage anchor load failed", "err", err)
		return
	}
	anchor := usageAnchorFromAny(raw)
	if anchor == nil {
		return
	}
	if anchorMatches(history, anchor) {
		s.Anchor = anchor
		return
	}
	s.Persist(nil)
}

// PersistedAnchorTokens is the anchored token figure from a session row anchor for callers without an agent
func PersistedAnchorTokens(ctx context.Context, store UsageAnchorStore, sessionID string, messages []*state.Message, haveMessages bool) (int, bool) {
	if sessionID == "" || store == nil || !haveMessages {
		return 0, false
	}
	raw, _, err := store.GetModelConfigValue(sessionID, UsageAnchorModelConfigKey)
	if err != nil {
		modelsLog.Debug("usage anchor load failed", "err", err)
		return 0, false
	}
	return AnchoredContextTokens(ctx, messages, usageAnchorFromAny(raw), true)
}
