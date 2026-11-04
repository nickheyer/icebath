package commands

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// ConfirmTimeout is how long a pending slash confirm stays resolvable
const ConfirmTimeout = 300 * time.Second

// ConfirmHandler runs the confirmed command for a choice of once, always or cancel
type ConfirmHandler func(ctx context.Context, choice string) (string, error)

// PendingConfirm is one slash command awaiting the user's choice
type PendingConfirm struct {
	ConfirmID string
	Command   string
	Handler   ConfirmHandler
	CreatedAt time.Time
}

// ConfirmStore holds pending slash confirms per session so adapters resolve them without the runner
type ConfirmStore struct {
	mu      sync.Mutex
	pending map[string]PendingConfirm
	now     func() time.Time
}

// Confirms is the process wide pending confirm store
var Confirms = NewConfirmStore()

// NewConfirmStore returns an empty store
func NewConfirmStore() *ConfirmStore {
	return &ConfirmStore{pending: map[string]PendingConfirm{}, now: time.Now}
}

// Register records a pending confirm, superseding any prior one for the session
func (s *ConfirmStore) Register(sessionKey, confirmID, command string, handler ConfirmHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[sessionKey] = PendingConfirm{ConfirmID: confirmID, Command: command, Handler: handler, CreatedAt: s.now()}
}

// Pending returns the pending confirm for a session
func (s *ConfirmStore) Pending(sessionKey string) (PendingConfirm, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[sessionKey]
	return p, ok
}

// Clear drops the pending confirm for a session without running it
func (s *ConfirmStore) Clear(sessionKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, sessionKey)
}

func (s *ConfirmStore) stale(p PendingConfirm, timeout time.Duration) bool {
	return s.now().Sub(p.CreatedAt) > timeout
}

// ClearIfStale drops a pending confirm older than the timeout and reports whether it did
func (s *ConfirmStore) ClearIfStale(sessionKey string, timeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pending[sessionKey]
	if ok && s.stale(p, timeout) {
		delete(s.pending, sessionKey)
		return true
	}
	return false
}

// Resolve runs the pending handler with a choice, false when stale, superseded or already resolved
func (s *ConfirmStore) Resolve(ctx context.Context, sessionKey, confirmID, choice string, timeout time.Duration) (string, bool) {
	s.mu.Lock()
	p, ok := s.pending[sessionKey]
	if !ok || p.ConfirmID != confirmID {
		s.mu.Unlock()
		return "", false
	}
	delete(s.pending, sessionKey)
	if s.stale(p, timeout) {
		s.mu.Unlock()
		return "", false
	}
	s.mu.Unlock()
	if p.Handler == nil {
		return "", false
	}
	result, err := p.Handler(ctx, choice)
	if err != nil {
		return fmt.Sprintf("❌ Error handling confirmation: %v", err), true
	}
	return result, true
}

// ConfirmChoice is one answer offered by a slash confirm prompt
type ConfirmChoice struct {
	Key    string
	Label  string
	Detail string
}

// DestructiveChoices are the answers of the destructive slash confirm
var DestructiveChoices = []ConfirmChoice{
	{"once", "Approve Once", "proceed this time only"},
	{"always", "Always Approve", "proceed and silence this prompt permanently"},
	{"cancel", "Cancel", "keep current conversation"},
}

var confirmAliases = map[string]string{
	"1": "once", "once": "once", "approve": "once", "yes": "once", "y": "once", "ok": "once",
	"2": "always", "always": "always", "remember": "always",
	"3": "cancel", "cancel": "cancel", "nevermind": "cancel", "no": "cancel", "n": "cancel",
}

// NormalizeConfirmChoice maps raw typed input to an offered choice key
func NormalizeConfirmChoice(raw string, choices []ConfirmChoice) (string, bool) {
	lowered := strings.ToLower(strings.TrimSpace(raw))
	if lowered == "" {
		return "", false
	}
	allowed := map[string]bool{}
	for _, c := range choices {
		allowed[c.Key] = true
	}
	if normalized, ok := confirmAliases[lowered]; ok && allowed[normalized] {
		return normalized, true
	}
	if allowed[lowered] {
		return lowered, true
	}
	return "", false
}

// DestructiveConfirmRequired reads approvals.destructive_slash_confirm, on by default
func DestructiveConfirmRequired(cfg config.Tree) bool {
	return cfg.Bool("approvals.destructive_slash_confirm", true)
}

// ApprovalGateOn reads approvals.<key>, on by default
func ApprovalGateOn(cfg config.Tree, key string) bool {
	return cfg.Bool("approvals."+key, true)
}

// DestructiveTitle is the modal title for a destructive slash command
func DestructiveTitle(command string) string {
	return "⚠️  /" + command + " — destroys conversation state"
}

// DestructiveAlwaysMessage confirms the persisted opt out on the CLI
const DestructiveAlwaysMessage = "🔒 Future /clear, /new, /reset, and /undo will run without confirmation."

// ReEnableHint tells the user how to turn a silenced approval back on
func ReEnableHint(setting string) string {
	return "   Re-enable via `approvals." + setting + ": true` in config.yaml."
}

// PersistFailedProceeding is printed when the opt out could not be saved
const PersistFailedProceeding = "⚠️  Couldn't persist opt-out — proceeding once."

// ConfirmCancelledText is the CLI line for a cancelled destructive command
func ConfirmCancelledText(command string) string {
	return "🟡 /" + command + " cancelled. Conversation unchanged."
}

// ConfirmNoInputText is the CLI line when the modal closed with no answer
func ConfirmNoInputText(command string) string { return "🟡 /" + command + " cancelled (no input)." }

// ConfirmUnrecognizedText is the CLI line for an answer outside the choices
func ConfirmUnrecognizedText(raw, command string) string {
	return "🟡 Unrecognized choice '" + raw + "'. /" + command + " cancelled."
}

// GatewayDestructivePrompt is the chat prompt asking to confirm a destructive command
func GatewayDestructivePrompt(command, detail, prefix string) string {
	return "⚠️ **Confirm /" + command + "**\n\n" + detail + "\n\nChoose:\n• **Approve Once** — proceed this time only\n" +
		"• **Always Approve** — proceed and silence this prompt permanently\n• **Cancel** — keep current conversation\n\n" +
		"_Text fallback: reply `" + prefix + "approve`, `" + prefix + "always`, or `" + prefix + "cancel`._"
}

// GatewayDestructiveCancelled is the chat reply for a cancelled destructive command
func GatewayDestructiveCancelled(command string) string {
	return "🟡 /" + command + " cancelled. Conversation unchanged."
}

// GatewayDestructiveOptOutNote is appended after an always answer, wording depending on persistence
func GatewayDestructiveOptOutNote(persisted bool) string {
	if persisted {
		return "\n\nℹ️ Future /clear, /new, /reset, and /undo will run without confirmation. Re-enable via `approvals.destructive_slash_confirm: true` in config.yaml."
	}
	return "\n\n⚠️ Could not save that preference (config.yaml is not writable), so /clear, /new, /reset, and /undo will ask again next time. To silence it permanently, set `approvals.destructive_slash_confirm: false` in config.yaml."
}

// DestructiveDetails are the chat details for the destructive session commands
var DestructiveDetails = map[string]string{
	"new":      "This starts a fresh session and discards the current conversation history.",
	"undo_one": "This removes the last user/assistant exchange from history.",
}

// DestructiveUndoDetail describes an undo of several turns
func DestructiveUndoDetail(count int) string {
	if count == 1 {
		return DestructiveDetails["undo_one"]
	}
	return fmt.Sprintf("This removes the last %d user turns from history.", count)
}

// GatewayUnknownCommandText is the chat reply for an unknown slash command
func GatewayUnknownCommandText(command string) string {
	return "Unknown command `/" + command + "`. Type /commands to see what's available, or resend without the leading slash to send as a regular message."
}

// GatewayHookBlockedText is the chat reply when a plugin hook denies a command
func GatewayHookBlockedText(command string) string {
	return "Command `/" + command + "` was blocked by a hook."
}
