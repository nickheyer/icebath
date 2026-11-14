package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Routes whose thinking blocks carry upstream verified signatures and heal a signature 400 durably
var anthSignedReplayRoutes = map[string]bool{"native": true, "minimax": true}

// Reports whether the route replays and the model keeps signed thinking from earlier turns
func nativeAnthropicPreservesPriorThinking(baseURL, model string) bool {
	return routeReplaysPriorThinking(anthropicThinkingRoute(baseURL, model), model)
}

// Session model config key holding the rejected thinking fingerprints
const anthRejectedThinkingConfigKey = "_anthropic_rejected_thinking"

var anthRejectedThinkingCarriers = []string{"reasoning_details", "anthropic_content_blocks", "_anthropic_content_blocks"}

// Coarse sha256 fingerprint of a signed thinking block, empty for anything else
func anthThinkingFingerprint(block any) string {
	m, ok := pyjson.Dict(block)
	if !ok {
		return ""
	}
	kind := asStr(m["type"])
	if !thinkingBlockTypes[kind] {
		return ""
	}
	field := "signature"
	if kind != "thinking" {
		field = "data"
	}
	value := m[field]
	if value == nil {
		return ""
	}
	if raw, isBytes := value.([]byte); isBytes && len(raw) == 0 {
		return ""
	}
	payload, isStr := value.(string)
	if !isStr {
		payload = pyrepr.Repr(value)
	} else if payload == "" {
		return ""
	}
	sum := sha256.Sum256(pystr.EncodeUTF8Replace(kind + "\x00" + payload))
	return hex.EncodeToString(sum[:])
}

// Reports whether a route tracks rejected thinking durably, signed replay routes on the Messages wire only
func tracksRejectedThinking(mode APIMode, baseURL, model string) bool {
	return mode == ModeAnthropicMessages && anthSignedReplayRoutes[anthropicThinkingRoute(baseURL, model)]
}

// RejectedThinkingStore persists rejected thinking fingerprints in a session's model config
type RejectedThinkingStore interface {
	ModelConfigValue(sessionID, key string) (any, bool)
	PatchModelConfig(sessionID string, patch map[string]any) error
}

// RejectedThinkingFingerprints returns the persisted rejection fingerprints of a session
func RejectedThinkingFingerprints(store RejectedThinkingStore, sessionID string) map[string]bool {
	out := map[string]bool{}
	if sessionID == "" || store == nil {
		return out
	}
	raw, ok := store.ModelConfigValue(sessionID, anthRejectedThinkingConfigKey)
	if !ok {
		return out
	}
	list, ok := asList(raw)
	if !ok {
		return out
	}
	for _, v := range list {
		if s, ok := v.(string); ok && s != "" {
			out[s] = true
		}
	}
	return out
}

// RejectedThinkingTracker is the per agent holder of rejected thinking fingerprints keyed by session
type RejectedThinkingTracker struct {
	Mode            APIMode
	BaseURL         string
	Model           string
	SessionID       string
	Store           RejectedThinkingStore
	PersistDisabled bool
	// Clears the prompt token anchor once the provider visible history changed shape
	ResetUsageAnchor func()
	mu               sync.Mutex
	boundSession     string
	bound            bool
	rejected         map[string]bool
}

// Shared live fingerprint set of a session, the persisted set when the holder is bound elsewhere
func (t *RejectedThinkingTracker) sessionRejectedThinking(sessionID string) map[string]bool {
	if t.bound && t.boundSession == sessionID {
		return t.rejected
	}
	return RejectedThinkingFingerprints(t.Store, sessionID)
}

// SessionRejectedThinking returns the in memory fingerprints of a session, else the persisted ones
func (t *RejectedThinkingTracker) SessionRejectedThinking(sessionID string) map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionRejectedThinking(sessionID)
}

func (t *RejectedThinkingTracker) bind(sessionID string, rejected map[string]bool) {
	t.boundSession, t.rejected, t.bound = sessionID, rejected, true
}

// Live set for the current session, rebinding when the session id rotated
func (t *RejectedThinkingTracker) current() map[string]bool {
	rejected := t.sessionRejectedThinking(t.SessionID)
	t.bind(t.SessionID, rejected)
	return rejected
}

func (t *RejectedThinkingTracker) persist(rejected map[string]bool) {
	if t.PersistDisabled || t.SessionID == "" || t.Store == nil {
		return
	}
	keys := make([]string, 0, len(rejected))
	for k := range rejected {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	values := make([]any, len(keys))
	for i, k := range keys {
		values[i] = k
	}
	if err := t.Store.PatchModelConfig(t.SessionID, map[string]any{anthRejectedThinkingConfigKey: values}); err != nil {
		catalogLog.Debug("Anthropic thinking suppression persist failed", "error", err)
	}
}

// Readable thinking text of an assistant message's replay carrier joined by blank lines
func anthMirroredReadableThinking(message map[string]any) (string, bool) {
	_, carrier := assistantReplayCarrier(message, convertOptions{})
	var readable []string
	for _, b := range carrier {
		m, ok := asMap(b)
		if !ok || asStr(m["type"]) != "thinking" {
			continue
		}
		if text, ok := m["thinking"].(string); ok && text != "" {
			readable = append(readable, text)
		}
	}
	if len(readable) == 0 {
		return "", false
	}
	return strings.Join(readable, "\n\n"), true
}

// StripRejectedThinking drops rejected thinking blocks from one request copy, every block when rejected is nil
func StripRejectedThinking(message map[string]any, rejected map[string]bool) int {
	if message == nil {
		return 0
	}
	mirror, hasMirror := "", false
	if asStr(message["role"]) == "assistant" {
		mirror, hasMirror = anthMirroredReadableThinking(message)
	}
	removed := 0
	for _, key := range anthRejectedThinkingCarriers {
		blocks, ok := asList(message[key])
		if !ok {
			continue
		}
		kept := make([]any, 0, len(blocks))
		for _, block := range blocks {
			m, isMap := asMap(block)
			if isMap && thinkingBlockTypes[asStr(m["type"])] {
				fp := anthThinkingFingerprint(block)
				if rejected == nil || (fp != "" && rejected[fp]) {
					removed++
					continue
				}
			}
			kept = append(kept, block)
		}
		if len(kept) > 0 {
			message[key] = kept
		} else {
			delete(message, key)
		}
	}
	if removed > 0 && hasMirror {
		for _, key := range []string{"reasoning", "reasoning_content"} {
			if s, ok := message[key].(string); ok && s == mirror {
				delete(message, key)
			}
		}
	}
	return removed
}

// ApplyRejectedThinkingSuppression filters rejected thinking from a request copy rebuilt from history
func (t *RejectedThinkingTracker) ApplyRejectedThinkingSuppression(messages []map[string]any) int {
	if t == nil || messages == nil || !tracksRejectedThinking(t.Mode, t.BaseURL, t.Model) {
		return 0
	}
	t.mu.Lock()
	rejected := t.current()
	t.mu.Unlock()
	if len(rejected) == 0 {
		return 0
	}
	total := 0
	for _, m := range messages {
		total += StripRejectedThinking(m, rejected)
	}
	return total
}

// RememberRejectedThinking fingerprints the rejected request and repairs its retry copy without touching history
func (t *RejectedThinkingTracker) RememberRejectedThinking(apiMessages []map[string]any) int {
	if t == nil || apiMessages == nil {
		return 0
	}
	current := map[string]bool{}
	for _, m := range apiMessages {
		for _, key := range anthRejectedThinkingCarriers {
			blocks, ok := asList(m[key])
			if !ok {
				continue
			}
			for _, block := range blocks {
				if fp := anthThinkingFingerprint(block); fp != "" {
					current[fp] = true
				}
			}
		}
	}
	if t.ResetUsageAnchor != nil {
		t.ResetUsageAnchor()
	}
	total := 0
	if len(current) == 0 {
		for _, m := range apiMessages {
			total += StripRejectedThinking(m, nil)
		}
		return total
	}
	t.mu.Lock()
	rejected := t.current()
	for fp := range current {
		rejected[fp] = true
	}
	t.persist(rejected)
	t.mu.Unlock()
	for _, m := range apiMessages {
		total += StripRejectedThinking(m, rejected)
	}
	return total
}

// CarryRejectedThinkingToSession moves the fingerprints of a compressed parent session onto the current one
func (t *RejectedThinkingTracker) CarryRejectedThinkingToSession(oldSessionID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var rejected map[string]bool
	if t.bound && t.boundSession == oldSessionID {
		rejected = make(map[string]bool, len(t.rejected))
		for k := range t.rejected {
			rejected[k] = true
		}
	} else {
		rejected = RejectedThinkingFingerprints(t.Store, oldSessionID)
	}
	if len(rejected) == 0 {
		return
	}
	for k := range t.current() {
		rejected[k] = true
	}
	t.bind(t.SessionID, rejected)
	t.persist(rejected)
}

// TracksRejectedThinking reports whether this holder's route heals signature rejections durably
func (t *RejectedThinkingTracker) TracksRejectedThinking() bool {
	return t != nil && tracksRejectedThinking(t.Mode, t.BaseURL, t.Model)
}
