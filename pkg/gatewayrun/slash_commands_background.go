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
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Starts the prompt as a background agent with its own session, answering in this chat when done
func (r *GatewayRunner) handleBackgroundCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	prompt := pystr.Strip(event.GetCommandArgs())
	if prompt == "" {
		return i18n.T(ctx, "gateway.background.usage", "", nil), nil
	}
	suffix := make([]byte, 3)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	taskID := fmt.Sprintf("bg_%s_%s", time.Now().Format("150405"), hex.EncodeToString(suffix))
	source := event.Source
	anchor := gateway.ReplyAnchorForEvent(event)
	mediaURLs := append([]string{}, event.MediaURLs...)
	mediaTypes := append([]string{}, event.MediaTypes...)
	r.trackBackgroundTask(ctx, "background:"+taskID, func(ctx context.Context) (any, error) {
		return nil, r.runBackgroundTask(ctx, prompt, source, taskID, anchor, mediaURLs, mediaTypes)
	})
	return i18n.T(ctx, "gateway.background.started", "", map[string]any{"preview": preview(prompt, 60), "task_id": taskID}), nil
}

// Answers a side question from a transcript snapshot in the background, leaving live history untouched
func (r *GatewayRunner) handleBtwCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	question := pystr.Strip(event.GetCommandArgs())
	if question == "" {
		return i18n.T(ctx, "gateway.btw.usage", "", nil), nil
	}
	source := event.Source
	entry, err := r.sessionStore.GetOrCreateSession(ctx, source, false, true)
	if err != nil {
		return "", err
	}
	history, err := r.sessionStore.LoadTranscript(ctx, entry.SessionID)
	if err != nil {
		var readErr *gateway.TranscriptReadError
		if errors.As(err, &readErr) {
			return historyUnreadable(ctx), nil
		}
		return "", err
	}
	if len(history) == 0 {
		return i18n.T(ctx, "gateway.btw.no_history", "", nil), nil
	}
	var model any
	rt := map[string]any{}
	if m, runtime, err := r.resolveSessionAgentRuntime(ctx, source, "", nil, true); err == nil {
		model, rt = m, runtime
	}
	if !pyrepr.Truthy(rt["api_key"]) {
		return i18n.T(ctx, "gateway.btw.no_provider", "", nil), nil
	}
	mainRuntime := map[string]any{"model": model, "provider": rt["provider"], "base_url": rt["base_url"],
		"api_key": rt["api_key"], "api_mode": rt["api_mode"], "session_id": entry.SessionID}
	snapshot := make([]map[string]any, 0, len(history))
	for _, item := range history {
		if m, ok := item.(map[string]any); ok {
			snapshot = append(snapshot, m)
		}
	}
	var parent *agent.AIAgent
	if key, err := r.sessionKeyForSource(ctx, source); err == nil {
		parent = r.cachedAgentFor(key, false)
	}
	threadMetadata := r.replyMetadata(event)
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return "", err
	}
	shown := preview(question, 60)
	r.trackBackgroundTask(ctx, "btw", func(ctx context.Context) (any, error) {
		opts := agent.NewSideQuestionOptions()
		opts.Parent, opts.MainRuntime = parent, mainRuntime
		answer, err := agent.AnswerSideQuestion(ctx, question, snapshot, opts)
		var reply string
		if err != nil {
			runLog().Warn(fmt.Sprintf("/btw side question failed: %s", pyrepr.ExceptionMessage(err)))
			reply = i18n.T(ctx, "gateway.btw.failed", "", map[string]any{"preview": shown, "error": pyrepr.ExceptionMessage(err)})
		} else {
			reply = i18n.T(ctx, "gateway.btw.answer", "", map[string]any{"preview": shown, "answer": answer})
		}
		if adapter != nil {
			if _, err := adapter.Send(ctx, source.ChatID, reply, "", threadMetadata); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return i18n.T(ctx, "gateway.btw.started", "", map[string]any{"preview": shown}), nil
}

// Reply for a transcript that could not be read
func historyUnreadable(ctx context.Context) string {
	return i18n.T(ctx, "gateway.shared.history_unreadable", "", nil)
}
