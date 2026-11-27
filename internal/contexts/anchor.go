package contexts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/sessions"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Key of the session's model settings the usage anchor is kept under
const AnchorKey = "_usage_anchor"

// What the provider counted of a request, and the messages it priced: how many, the newest and all of them, by their fingerprints
type Anchor struct {
	Prompt     int    `json:"prompt_tokens"`
	Completion int    `json:"completion_tokens"`
	Count      int    `json:"base_count"`
	LastRole   string `json:"base_last_role"`
	Last       string `json:"base_last_fp"`
	Prefix     string `json:"base_prefix_fp"`
}

// Digest of what of a message the model is sent: its role, text, text sent in its place, the call it answers and the calls it makes
func fingerprint(m *sessionsv1.Message) string {
	if m == nil {
		return ""
	}
	payload := map[string]any{"role": Role(m)}
	if m.GetText() != "" {
		payload["content"] = m.GetText()
	}
	if m.GetSent() != "" {
		payload["api_content"] = m.GetSent()
	}
	if m.GetToolCallId() != "" {
		payload["tool_call_id"] = m.GetToolCallId()
	}
	if calls := m.GetToolCalls(); len(calls) > 0 {
		var listed []map[string]string
		for _, call := range calls {
			listed = append(listed, map[string]string{"id": call.GetId(), "name": call.GetName(), "arguments": call.GetArguments()})
		}
		payload["tool_calls"] = listed
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Digest of the first count messages, empty when there are fewer
func prefix(messages []*sessionsv1.Message, count int) string {
	if count <= 0 || len(messages) < count {
		return ""
	}
	parts := make([][2]string, count)
	for i, m := range messages[:count] {
		parts[i] = [2]string{Role(m), fingerprint(m)}
	}
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Anchor of the provider's count of a request of the messages, nil when the count or the messages are of no use
func Capture(prompt, completion int, messages []*sessionsv1.Message) *Anchor {
	if prompt <= 0 || len(messages) == 0 {
		return nil
	}
	last := messages[len(messages)-1]
	return &Anchor{Prompt: prompt, Completion: max(completion, 0), Count: len(messages), LastRole: Role(last), Last: fingerprint(last),
		Prefix: prefix(messages, len(messages))}
}

// Whether the messages still begin with every message the anchor priced
func (a *Anchor) matches(messages []*sessionsv1.Message) bool {
	if a == nil || a.Count <= 0 || len(messages) < a.Count {
		return false
	}
	base := messages[a.Count-1]
	return Role(base) == a.LastRole && a.Last != "" && fingerprint(base) == a.Last && a.Prefix != "" && prefix(messages, a.Count) == a.Prefix
}

// Size of the context the messages make as the anchor's count with the messages since estimated, the reply the count covers left out;
// whether the anchor still holds for them, and whether any of it is estimated
func (a *Anchor) Tokens(messages []*sessionsv1.Message) (int, bool, bool) {
	if !a.matches(messages) {
		return 0, false, false
	}
	delta := messages[a.Count:]
	if len(delta) > 0 && delta[0].GetRole() == sessionsv1.Role_ROLE_ASSISTANT {
		delta = delta[1:]
	}
	return a.Prompt + a.Completion + Estimate(delta), true, len(delta) > 0
}

// Stores the anchor with the session's model settings, nil clearing it
func SaveAnchor(ctx context.Context, store sessions.Store, session string, anchor *Anchor) {
	if store == nil || session == "" {
		return
	}
	var value any
	if anchor != nil {
		value = map[string]any{"prompt_tokens": anchor.Prompt, "completion_tokens": anchor.Completion, "base_count": anchor.Count,
			"base_last_role": anchor.LastRole, "base_last_fp": anchor.Last, "base_prefix_fp": anchor.Prefix}
	}
	if err := store.Update(ctx, session, sessions.Change{ModelConfig: map[string]any{AnchorKey: value}}); err != nil {
		slog.Debug("usage anchor persist failed", "session", session, "error", err)
	}
}

// The anchor stored with the session when its messages still begin with what it priced; a stale one is cleared. Nil when none holds
func LoadAnchor(ctx context.Context, store sessions.Store, session string, messages []*sessionsv1.Message) *Anchor {
	if store == nil || session == "" || len(messages) == 0 {
		return nil
	}
	record, err := store.Get(ctx, session)
	if err != nil || record.GetModelConfig() == "" {
		return nil
	}
	var config map[string]any
	if json.Unmarshal([]byte(record.GetModelConfig()), &config) != nil {
		return nil
	}
	raw, ok := config[AnchorKey].(map[string]any)
	if !ok {
		return nil
	}
	anchor := &Anchor{Prompt: cast.ToInt(raw["prompt_tokens"]), Completion: max(cast.ToInt(raw["completion_tokens"]), 0), Count: cast.ToInt(raw["base_count"]),
		LastRole: cast.ToString(raw["base_last_role"]), Last: cast.ToString(raw["base_last_fp"]), Prefix: cast.ToString(raw["base_prefix_fp"])}
	if anchor.Prompt <= 0 || anchor.Count <= 0 || anchor.Last == "" || anchor.Prefix == "" {
		return nil
	}
	if !anchor.matches(messages) {
		SaveAnchor(ctx, store, session, nil)
		return nil
	}
	return anchor
}
