package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Discards orphan turn markers then consumes the clean exit receipt, failing closed
func (r *GatewayRunner) consumeCleanShutdownMarker(ctx context.Context, markerPath string) (int, error) {
	discarded, err := r.sessionStore.DiscardActiveTurnMarkers(ctx)
	if err != nil {
		return 0, err
	}
	if err := os.Remove(markerPath); err != nil {
		return 0, err
	}
	return discarded, nil
}

// Recovers only the turns the dead process left marked, returning the resumed and ledgered counts
func (r *GatewayRunner) recoverUncleanSessions(ctx context.Context) (int, int) {
	maxAge := max(60*60, int(max(1.0, floatEnv("ICEBATH_AGENT_TIMEOUT", 1800))*2))
	ledgered, err := r.ledgerCrashLeftReplies(ctx, maxAge)
	logSuppressed(slog.LevelWarn, err, "Crash-left reply recovery on startup failed: %s")
	resumed, err := r.sessionStore.RecoverInterruptedTurns(ctx, maxAge)
	logSuppressed(slog.LevelWarn, err, "Exact active-turn recovery on startup failed: %s")
	return resumed, ledgered
}

// Turn marker the dead process left on one routing entry
type crashMarkedTurn struct {
	key, sessionID, token string
	started               float64
	origin                *gateway.SessionSource
	profile               *string
}

// Settles every marked turn whose final reply persisted, ledgering the owed reply and clearing its marker
func (r *GatewayRunner) ledgerCrashLeftReplies(ctx context.Context, maxAgeSeconds int) (int, error) {
	ledgerOn := gateway.LedgerEnabled(ctx, nil)
	cutoff := float64(time.Now().UnixNano())/1e9 - float64(maxAgeSeconds)
	var marked []crashMarkedTurn
	r.sessionStore.FilterLoadedEntries(ctx, func(e *gateway.SessionEntry) bool {
		started, stamped := e.ActiveTurnStartedTimestamp()
		if e.ActiveTurnToken != nil && *e.ActiveTurnToken != "" && stamped && e.Origin != nil && !e.Suspended {
			marked = append(marked, crashMarkedTurn{e.SessionKey, e.SessionID, *e.ActiveTurnToken, started, e.Origin, e.TransportProfile})
		}
		return false
	})
	ledgered := 0
	for _, turn := range marked {
		if turn.started < cutoff {
			continue
		}
		decision, _ := r.sessionStore.GetSessionMetadata(ctx, turn.key, "room_silence_decision", map[string]any{}).(map[string]any)
		history, err := r.sessionStore.LoadTranscript(ctx, turn.sessionID)
		if err != nil {
			return ledgered, err
		}
		token := turn.token
		text, err := r.crashLeftReply(ctx, history, turn.started, turn.origin, decision, &token)
		if err != nil {
			return ledgered, err
		}
		if text == nil || (*text != "" && !ledgerOn) {
			continue
		}
		if *text != "" {
			if err := gateway.RecordCrashLeftReply(ctx, gateway.DeliveryObligation{
				ObligationID: gateway.ComputeObligationID(turn.key, "crash:"+turn.token, *text), SessionKey: turn.key,
				Platform: string(turn.origin.Platform), ChatID: turn.origin.ChatID, ThreadID: turn.origin.ThreadID,
				Content: *text, AdapterProfile: turn.profile}, turn.started); err != nil {
				return ledgered, err
			}
		}
		cleared, err := r.sessionStore.ClearTurnActive(ctx, turn.key, turn.token)
		if err != nil {
			return ledgered, err
		}
		if cleared && *text != "" {
			ledgered++
		}
	}
	return ledgered, nil
}

// Message value of a transcript row, an empty map for anything else
func historyMessage(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case *pyjson.Object:
		if plain, ok := pyjson.Plain(m).(map[string]any); ok {
			return plain
		}
	}
	return map[string]any{}
}

// What a crash left turn owes as live delivery would judge it, nil when no final reply persisted after started
func (r *GatewayRunner) crashLeftReply(ctx context.Context, history []any, started float64, origin *gateway.SessionSource, silenceDecision map[string]any, activeToken *string) (*string, error) {
	var visible []map[string]any
	for _, row := range history {
		m := historyMessage(row)
		if role := m["role"]; role != "session_meta" && role != "system" {
			visible = append(visible, m)
		}
	}
	last := map[string]any{}
	if len(visible) > 0 {
		last = visible[len(visible)-1]
	}
	content, isText := last["content"].(string)
	stamp, _ := pydatetime.CoerceEpoch(last["timestamp"], "", "")
	if last["role"] != "assistant" || pyrepr.Truthy(last["tool_calls"]) || !isText || stamp < started {
		return nil, nil
	}
	prompt := map[string]any{}
	for i := len(visible) - 1; i >= 0; i-- {
		if visible[i]["role"] == "user" {
			prompt = visible[i]
			break
		}
	}
	displayMetadata, _ := prompt["display_metadata"].(map[string]any)
	if gateway.IsMachineryDisplayKind(prompt["display_kind"]) {
		scoped, release, err := r.MediaDeliveryScopeForSource(ctx, origin)
		if err != nil {
			runLog().Debug(fmt.Sprintf("Crash-left reply: no routed scope for %s", origin.ChatID), "exc_info", err)
			scoped, release = ctx, func() {}
		}
		muted := config.DiagnosticTurnMuted(scoped, displayMetadata, string(origin.Platform), nil)
		release()
		if muted {
			empty := ""
			return &empty, nil
		}
	}
	if gateway.IsIntentionalSilenceResponse(content) {
		decision := silenceDecision
		if decision == nil {
			decision = map[string]any{}
		}
		proven := activeToken != nil && decision["token"] == *activeToken &&
			pyjson.Equal(decision["timestamp"], last["timestamp"]) && decision["silent"] == true
		var replyExpected *bool
		if expected, ok := displayMetadata["reply_expected"].(bool); ok {
			replyExpected = &expected
		}
		text := ""
		if !gateway.SilenceAllowed(prompt["display_kind"], replyExpected, &proven, map[string]any{}) {
			text = unexpectedSilenceReply(ctx)
		}
		return &text, nil
	}
	reply := pyrepr.Str(gateway.StripTrailingSilenceMarker(content))
	stripped, err := gateway.StripMediaDirectives(ctx, sanitizeGatewayFinalResponse(ctx, origin.Platform, reply))
	if err != nil {
		return nil, err
	}
	if text := pystr.Strip(stripped); text != "" {
		return &text, nil
	}
	return nil, nil
}
