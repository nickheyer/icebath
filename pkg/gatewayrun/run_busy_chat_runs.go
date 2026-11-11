package gatewayrun

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Denial message when the source may not run the canonical command, empty when it may
func (r *GatewayRunner) checkSlashAccess(ctx context.Context, source *gateway.SessionSource, canonicalCmd string) string {
	if canonicalCmd == "" {
		return ""
	}
	policy := gateway.PolicyForRunnerSource(r, source)
	var userID any
	if source.UserID != nil {
		userID = *source.UserID
	}
	if !policy.Enabled || policy.CanRun(userID, canonicalCmd) {
		return ""
	}
	platform := "?"
	if source.Platform != "" {
		platform = string(source.Platform)
	}
	runLog().Info(fmt.Sprintf("Slash command /%s denied for %s:%s (not admin, not in user_allowed_commands)", canonicalCmd, platform, derefOrNoneText(source.UserID)))
	allowed := make([]string, 0, len(policy.UserAllowedCommands))
	for cmd := range policy.UserAllowedCommands {
		allowed = append(allowed, cmd)
	}
	sort.Strings(allowed)
	var suffix string
	if len(allowed) > 0 {
		shown := allowed
		if len(shown) > 12 {
			shown = shown[:12]
		}
		names := make([]string, len(shown))
		for i, c := range shown {
			names[i] = "/" + c
		}
		more := ""
		if len(allowed) > 12 {
			more = "\u2026"
		}
		suffix = i18n.T(ctx, "gateway.unauthorized.admin_only_can_run", "", nil) + strings.Join(names, ", ") + more +
			i18n.T(ctx, "gateway.unauthorized.admin_only_whoami_hint", "", nil)
	} else {
		suffix = i18n.T(ctx, "gateway.unauthorized.admin_only_none", "", nil)
	}
	return i18n.T(ctx, "gateway.unauthorized.admin_only", "", map[string]any{"command": canonicalCmd, "suffix": suffix})
}

// Python str of an optional field, None for an unset one
func derefOrNoneText(p *string) string {
	if p == nil {
		return "None"
	}
	return *p
}

// One other running turn of the caller's chat, its key, chat type and the key text after the chat id
type sameChatRun struct{ key, chatType, tail string }

// Every other running turn in the caller's chat, namespaced by the caller's own session key
func (r *GatewayRunner) sameChatRuns(ctx context.Context, source *gateway.SessionSource, ownKey string) ([]sameChatRun, error) {
	chatID := source.ChatID
	if chatID == "" {
		return nil, nil
	}
	if source.ChatType == "dm" && source.Platform == gateway.PlatformWhatsApp {
		canonical, err := gateway.CanonicalWhatsAppIdentifier(ctx, chatID)
		if err != nil {
			return nil, err
		}
		if canonical != "" {
			chatID = canonical
		}
	}
	parts := strings.SplitN(ownKey, ":", 3)
	if len(parts) > 2 {
		parts = parts[:2]
	}
	prefix := strings.Join(parts, ":") + ":" + string(source.Platform) + ":"
	var scopeID *string
	if source.ScopeID != nil && *source.ScopeID != "" {
		scopeID = source.ScopeID
	}
	keys := make([]string, 0)
	for key := range r.snapshotRunningAgents() {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var runs []sameChatRun
	for _, key := range keys {
		if key == ownKey {
			continue
		}
		if chatType, tail, ok := sameChatKeySlots(key, prefix, chatID, scopeID); ok {
			runs = append(runs, sameChatRun{key, chatType, tail})
		}
	}
	return runs, nil
}

// Keys of runs that belong to other participants of the caller's own thread
func siblingThreadRunKeys(source *gateway.SessionSource, runs []sameChatRun) []string {
	threadID := ""
	if source.ThreadID != nil {
		threadID = *source.ThreadID
	}
	if threadID == "" || source.ChatType == "" {
		return []string{}
	}
	keys := []string{}
	for _, run := range runs {
		if run.chatType == source.ChatType && tailHasSlot(run.tail, threadID) {
			keys = append(keys, run.key)
		}
	}
	return keys
}

// Keys of runs in any session of the same chat, limited to the caller's thread when it stops from inside one
func chatScopedRunKeys(source *gateway.SessionSource, runs []sameChatRun) []string {
	threadID := ""
	if source.ThreadID != nil {
		threadID = *source.ThreadID
	}
	keys := []string{}
	for _, run := range runs {
		if threadID == "" || run.tail == "" || tailHasSlot(run.tail, threadID) {
			keys = append(keys, run.key)
		}
	}
	return keys
}

// Whether a Telegram restart command is a redelivery of one this gateway already handled
func (r *GatewayRunner) isStaleRestartRedelivery(event *gateway.MessageEvent) bool {
	if event == nil || event.Source == nil || event.PlatformUpdateID == nil || event.Source.Platform != gateway.PlatformTelegram {
		return false
	}
	markerPath := filepath.Join(home.Process(), ".restart_last_processed.json")
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		if os.IsNotExist(err) {
			if r.bootedFromRestart && wallClock()-r.startupTime < 60 {
				r.bootedFromRestart = false
				return true
			}
		}
		return false
	}
	parsed, err := pyjson.Loads(strings.TrimPrefix(string(raw), "\ufeff"))
	if err != nil {
		return false
	}
	data, ok := pyjson.AsObject(parsed)
	if !ok {
		return false
	}
	platform, _ := data.Get("platform")
	recorded, _ := data.Get("update_id")
	recordedUID, isInt := pyjsonInt(recorded)
	if platform != "telegram" || !isInt || *event.PlatformUpdateID > recordedUID {
		return false
	}
	if r.bootedFromRestart {
		r.bootedFromRestart = false
		return true
	}
	requestedAt, _ := data.Get("requested_at")
	switch v := requestedAt.(type) {
	case int64:
		return !(wallClock()-float64(v) > 300)
	case int:
		return !(wallClock()-float64(v) > 300)
	case float64:
		return !(wallClock()-v > 300)
	}
	return true
}

// Integer value of a decoded JSON number clamped to int64, false for floats, bools and every other type
func pyjsonInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case *big.Int:
		if n.IsInt64() {
			return n.Int64(), true
		}
		if n.Sign() > 0 {
			return math.MaxInt64, true
		}
		return math.MinInt64, true
	}
	return 0, false
}

// Note appended after an always confirmation, saying whether the opt out was saved
func destructiveOptoutNote(ctx context.Context, persisted bool) string {
	if persisted {
		return i18n.T(ctx, "gateway.confirm.destructive_optout", "", nil)
	}
	return i18n.T(ctx, "gateway.confirm.destructive_optout_save_failed", "", nil)
}

// Raw config.yaml read fresh from disk for gate lookups, empty when it cannot be loaded
func (r *GatewayRunner) readUserConfig(ctx context.Context) map[string]any {
	tree, err := config.Load(home.Dir(ctx), nil)
	if err != nil || tree == nil {
		return map[string]any{}
	}
	return map[string]any(tree)
}
