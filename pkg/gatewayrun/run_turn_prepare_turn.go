package gatewayrun

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Inputs to the agent run assembled by the prepare phase
type preparedTurn struct {
	history                []map[string]any
	contextPrompt          string
	messageText            any
	persistUserMessage     any
	persistUserTimestamp   any
	persistUserDisplayKind any
	persistenceSessionID   *string
	persistenceOwner       *string
	titleUserMessage       *string
}

// Skill names an event binds, a single name or a list of them
func autoSkillNames(auto any) []string {
	switch v := auto.(type) {
	case string:
		return []string{v}
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, name := range v {
			out = append(out, pyrepr.Str(name))
		}
		return out
	}
	return nil
}

// Transcript rows of a loaded history as dict rows
func transcriptRows(rows []any) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if m, ok := row.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Optional string as its JSON value, None for nil
func optJSON(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Prepared agent run inputs, or a reply to send instead, or neither when the turn drops
func (r *GatewayRunner) hmwaPrepareTurn(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, entry *gateway.SessionEntry, sessionKey, quickKey string, runGeneration int) (*preparedTurn, *string, *tools.Env, error) {
	wasAutoReset, isNewSession, err := r.hmwaOpenSession(ctx, entry, sessionKey, source)
	if err != nil {
		return nil, nil, nil, err
	}
	tools.RestoreGatewayYolo(sessionKey, entry.Yolo, false)
	sc := gateway.BuildSessionContext(ctx, source, r.config, entry)
	sessionEnv, err := r.setSessionEnv(ctx, sc)
	if err != nil {
		return nil, nil, nil, err
	}
	persistUserDisplayKind := gateway.DisplayKindForEvent(ctx, gateway.DisplayEventOf(event))
	redactPII := false
	func() {
		defer func() { _ = recover() }()
		if privacy, ok := gateway.LoadGatewayConfigMap(ctx, "")["privacy"].(map[string]any); ok {
			redactPII = pyrepr.Truthy(privacy["redact_pii"])
		}
	}()
	reusePins := event.Internal || event.InheritChannelPin
	if source.Platform != gateway.PlatformLocal && reusePins && sessionKey != "" {
		sid := entry.SessionID
		r.rehydratePromptPins(ctx, sessionKey, &sid)
	}
	contextPrompt := ""
	if source.Platform != gateway.PlatformLocal {
		contextPrompt = r.pinnedSessionContextPrompt(ctx, sc, redactPII, sessionKey, reusePins)
	}
	var turnSidecarNotes []string
	if wasAutoReset {
		r.hmwaDeliverAutoResetNotice(ctx, entry, source, &turnSidecarNotes)
	}
	var titleUserMessage *string
	if event.Text != "" {
		text := event.Text
		titleUserMessage = &text
	}
	if isNewSession && pyrepr.Truthy(event.AutoSkill) {
		r.hmwaAutoLoadSkills(ctx, event, autoSkillNames(event.AutoSkill), quickKey, sessionKey)
	}
	if err := r.hmwaAcquireTurnLease(ctx, quickKey, runGeneration, entry, sessionEnv); err != nil {
		return nil, nil, sessionEnv, err
	}
	r.markDurableActiveTurn(ctx, event, entry.SessionKey)
	loaded, err := r.AsyncSessionStore().Store().LoadTranscript(ctx, entry.SessionID)
	var history []map[string]any
	if err == nil {
		history, err = r.hmwaRunSessionHygiene(ctx, event, source, entry, sessionKey, transcriptRows(loaded), quickKey, runGeneration)
	}
	if err != nil {
		var readErr *gateway.TranscriptReadError
		if !errors.As(err, &readErr) {
			return nil, nil, sessionEnv, err
		}
		r.clearSessionEnv(sessionEnv)
		reply := gateway.RecordUnexecutedFailure(ctx, i18n.T(ctx, "gateway.errors.history_unavailable", "", nil), "", nil)
		return nil, &reply, sessionEnv, nil
	}
	if err := r.hmwaFirstContactNotes(ctx, source, history, &turnSidecarNotes, event.Internal); err != nil {
		return nil, nil, sessionEnv, err
	}
	vcNote, _, err := r.voiceChannelSidecarNote(ctx, event, source, sessionKey)
	if err != nil {
		return nil, nil, sessionEnv, err
	}
	if vcNote != "" {
		turnSidecarNotes = append(turnSidecarNotes, vcNote)
	}
	prepared, err := r.prepareProfileScopedInboundMessageText(ctx, event, source, sessionKey)
	if err != nil {
		return nil, nil, sessionEnv, err
	}
	if prepared == nil {
		return nil, event.GatewayTurnRefusal(), sessionEnv, nil
	}
	messageText, persistUserMessage, persistUserTimestamp := r.hmwaApplyMessageTimestamp(ctx, event, *prepared)
	if len(turnSidecarNotes) > 0 && sessionKey != "" {
		r.setPendingTurnSidecarNotes(sessionKey, turnSidecarNotes)
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return nil, nil, sessionEnv, err
	}
	generation := runGeneration
	r.bindAdapterRunGeneration(adapter, sessionKey, &generation)
	var owner string
	if event.MessageID != nil && *event.MessageID != "" {
		namespace, err := pyjson.Dumps([]any{string(source.Platform), optJSON(source.Profile), optJSON(source.ScopeID),
			source.ChatID, optJSON(source.ThreadID), *event.MessageID})
		if err != nil {
			return nil, nil, sessionEnv, err
		}
		owner = uuid.NewSHA1(uuid.NameSpaceURL, []byte(namespace)).String()
	} else {
		owner = uuid.NewString()
	}
	sessionID := entry.SessionID
	return &preparedTurn{history: history, contextPrompt: contextPrompt, messageText: messageText,
		persistUserMessage: persistUserMessage, persistUserTimestamp: persistUserTimestamp,
		persistUserDisplayKind: persistUserDisplayKind, persistenceSessionID: &sessionID, persistenceOwner: &owner,
		titleUserMessage: titleUserMessage}, nil, sessionEnv, nil
}
