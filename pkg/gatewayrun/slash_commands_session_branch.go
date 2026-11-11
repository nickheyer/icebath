package gatewayrun

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Message fields a branch child row copies from its parent row
var branchCopiedFields = []string{"content", "tool_calls", "tool_call_id", "finish_reason", "reasoning",
	"reasoning_content", "reasoning_details", "codex_reasoning_items", "codex_message_items", "timestamp", "data_use"}

// Adapter tracking the threads its bot has participated in
type threadParticipationAdapter interface {
	Threads() *gateway.ThreadParticipationTracker
}

// Branch child row keeping the api content sidecar so the first branch turn replays the parent's wire bytes
func branchRow(msg map[string]any) map[string]any {
	row := make(map[string]any, len(branchCopiedFields)+3)
	for _, k := range branchCopiedFields {
		row[k] = msg[k]
	}
	row["role"] = "user"
	if role, ok := msg["role"]; ok {
		row["role"] = role
	}
	row["tool_name"] = pyrepr.Or(msg["tool_name"], msg["name"])
	row["api_content"] = nil
	if sidecar, ok := agent.ExtractAPIContentSidecar(msg); ok {
		row["api_content"] = sidecar
	}
	return row
}

// Timestamped id of a new branch session with a random six hex suffix
func newBranchSessionID() (string, error) {
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	return time.Now().Format("20060102_150405") + "_" + hex.EncodeToString(suffix), nil
}

// Forks the session into an independent copy, in a new sibling thread where the platform has threads
func (r *GatewayRunner) handleBranchCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	asyncDB := r.sessionDB(ctx)
	if asyncDB == nil {
		return sessionDBUnavailableReply(ctx), nil
	}
	db := asyncDB.DB()
	source := event.Source
	sessionKey, err := r.sessionKeyForSource(ctx, source)
	if err != nil {
		return "", err
	}
	store := r.slashStore()
	current, err := store.GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", err
	}
	raw, err := store.LoadTranscript(ctx, current.SessionID)
	if err != nil {
		var readErr *gateway.TranscriptReadError
		if !errors.As(err, &readErr) {
			return "", err
		}
		return historyUnreadable(ctx), nil
	}
	history := transcriptMaps(raw)
	if len(history) == 0 {
		return i18n.T(ctx, "gateway.branch.no_conversation", "", nil), nil
	}
	newSessionID, err := newBranchSessionID()
	if err != nil {
		return "", err
	}
	stayHere, branchTitle := parseBranchArgs(event.GetCommandArgs())
	if branchTitle == "" {
		currentTitle, _, err := db.GetSessionTitle(ctx, current.SessionID)
		if err != nil {
			return "", err
		}
		if currentTitle == "" {
			currentTitle = "branch"
		}
		if branchTitle, err = db.GetNextTitleInLineage(ctx, currentTitle); err != nil {
			return "", err
		}
	}
	parentSessionID := current.SessionID
	var destSource *gateway.SessionSource
	if !stayHere {
		if destSource, err = r.branchOpenThread(ctx, source, branchTitle); err != nil {
			return "", err
		}
	}
	inPlace := destSource == nil
	if inPlace {
		destSource = source
	}
	destKey := sessionKey
	if !inPlace {
		if destKey, err = r.sessionKeyForSource(ctx, destSource); err != nil {
			return "", err
		}
	}
	originJSON := func() (out string) {
		defer func() {
			if recover() != nil {
				out = ""
			}
		}()
		origin := destSource
		if inPlace {
			origin = source
			if current.Origin != nil {
				origin = current.Origin
			}
		}
		text, err := pyjson.Dumps(origin.ToDict())
		if err != nil {
			return ""
		}
		return text
	}()
	if err := func() error {
		parent, err := db.GetSession(ctx, parentSessionID)
		if err != nil {
			return err
		}
		platform := string(source.Platform)
		if platform == "" {
			platform = "gateway"
		}
		displayName := ""
		if current.DisplayName != nil {
			displayName = *current.DisplayName
		}
		systemPrompt := ""
		if parent != nil {
			systemPrompt = rowStr(parent, "system_prompt")
		}
		return db.CreateSession(state.NewSession{ID: newSessionID, Source: platform, ModelConfig: map[string]any{"_branched_from": parentSessionID},
			ParentSessionID: parentSessionID, UserID: sattr(destSource.UserID), SessionKey: destKey, ChatID: destSource.ChatID,
			ChatType: destSource.ChatType, ThreadID: sattr(destSource.ThreadID), OriginJSON: originJSON, DisplayName: displayName,
			SystemPrompt: systemPrompt})
	}(); err != nil {
		runLog().Error(fmt.Sprintf("Failed to create branch session: %s", pyrepr.ExceptionMessage(err)))
		return i18n.T(ctx, "gateway.branch.create_failed", "", map[string]any{"error": pyrepr.ExceptionMessage(err)}), nil
	}
	rows := make([]map[string]any, len(history))
	for i, msg := range history {
		rows[i] = branchRow(msg)
	}
	chunkRows := 500
	_, _ = db.AppendMessagesBatch(ctx, newSessionID, rows, state.AppendGuards{}, &chunkRows)
	_, _ = db.SetSessionTitle(ctx, newSessionID, branchTitle)
	if !inPlace {
		if _, err := store.GetOrCreateSession(ctx, destSource, false, true); err != nil {
			return "", err
		}
	}
	newEntry, err := store.SwitchSession(ctx, destKey, newSessionID, nil, true)
	if err != nil {
		return "", err
	}
	if newEntry == nil {
		return i18n.T(ctx, "gateway.branch.switch_failed", "", nil), nil
	}
	r.clearSessionBoundarySecurityState(ctx, destKey)
	r.evictCachedAgent(ctx, destKey, false, false)
	msgCount := 0
	for _, m := range history {
		if m["role"] == "user" {
			msgCount++
		}
	}
	kwargs := map[string]any{"title": branchTitle, "count": msgCount, "parent": parentSessionID, "new": newSessionID}
	if inPlace {
		key := "gateway.branch.branched_many"
		if msgCount == 1 {
			key = "gateway.branch.branched_one"
		}
		reply := i18n.T(ctx, key, "", kwargs)
		if !stayHere && branchThreadPlatforms[source.Platform] {
			reply += "\n" + i18n.T(ctx, "gateway.branch.thread_fallback", "", nil)
		}
		return reply, nil
	}
	key := "gateway.branch.branched_thread_many"
	if msgCount == 1 {
		key = "gateway.branch.branched_thread_one"
	}
	kwargs["thread"] = formatThreadRef(source.Platform, sattr(destSource.ThreadID))
	return i18n.T(ctx, key, "", kwargs), nil
}

// Opens the sibling thread a plain branch clones into, nil when this chat cannot host one
func (r *GatewayRunner) branchOpenThread(ctx context.Context, source *gateway.SessionSource, title string) (*gateway.SessionSource, error) {
	parentID := branchThreadParent(source)
	if parentID == nil {
		return nil, nil
	}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil || adapter == nil {
		return nil, err
	}
	threadID, _, err := adapter.CreateHandoffThread(ctx, *parentID, title)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Branch: create_handoff_thread failed on %s; branching in place", string(source.Platform)), "exc_info", err)
		return nil, nil
	}
	if threadID == "" {
		return nil, nil
	}
	if holder, ok := adapter.(threadParticipationAdapter); ok {
		if threads := holder.Threads(); threads != nil {
			if err := threads.MarkAsync(ctx, threadID); err != nil {
				return nil, err
			}
		}
	}
	return branchDestSource(source, *parentID, threadID, title), nil
}
