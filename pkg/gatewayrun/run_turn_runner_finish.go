package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Consumes a one shot per session note into the next user message, prepended to the turn message
func (t *TurnRunner) prependPendingNote(notes map[string]string) {
	tc := t.ctx
	if len(notes) == 0 || tc.sessionKey == "" {
		return
	}
	note, ok := notes[tc.sessionKey]
	if !ok {
		return
	}
	delete(notes, tc.sessionKey)
	if note != "" {
		tc.message = note + "\n\n" + tc.message
	}
}

// Whether the delivery platform reports a restore and asks what next instead of continuing the work
func (t *TurnRunner) resumeNoteInteractive(ctx context.Context) bool {
	adapter, err := t.runner.deliveryAdapterFor(ctx, t.ctx.source)
	if err != nil || adapter == nil {
		return true
	}
	return adapter.Base().Traits.InteractiveResume
}

// Messages of a result as rows, nil for a result that carries none
func resultMessages(result map[string]any) []map[string]any {
	switch messages := result["messages"].(type) {
	case []map[string]any:
		return messages
	case []any:
		out := make([]map[string]any, 0, len(messages))
		for _, m := range messages {
			if row, ok := m.(map[string]any); ok {
				out = append(out, row)
			}
		}
		return out
	}
	return nil
}

// Repairs the final response's media paths, publishes the result and finalizes the stream consumer with a completed final
func (t *TurnRunner) finishStreamConsumer(ctx context.Context, result map[string]any, agentHistory []map[string]any, consumer *gateway.GatewayStreamConsumer) {
	if final, ok := result["final_response"].(string); ok && result != nil {
		messages := resultMessages(result)
		asAny := make([]any, len(messages))
		for i, m := range messages {
			asAny[i] = m
		}
		result["final_response"] = gateway.RepairExplicitComputerUseMediaPathsAt(ctx, final, asAny, len(agentHistory))
	}
	t.ctx.setResult(result)
	if consumer == nil {
		return
	}
	var finalForStream *string
	if result != nil && !pyrepr.Truthy(result["failed"]) && !pyrepr.Truthy(result["interrupted"]) && result["completed"] != false {
		if final, ok := result["final_response"].(string); ok && pystr.Strip(final) != "" && final != "(empty)" {
			finalForStream = &final
		}
	}
	consumer.Finish(finalForStream)
}

// Restores a Telegram DM thread id from its topic binding after a compression split lost it
func (t *TurnRunner) restoreTelegramThreadIDAfterSplit(ctx context.Context, agentSessionID string) {
	tc := t.ctx
	defer func() {
		if r := recover(); r != nil {
			runLog().Debug(fmt.Sprintf("Failed to restore thread_id from binding after session split: %v", r))
		}
	}()
	db := t.runner.sessionDB(ctx)
	if db == nil {
		return
	}
	binding, err := db.DB().GetTelegramTopicBindingBySession(ctx, agentSessionID)
	if err != nil {
		runLog().Debug(fmt.Sprintf("Failed to restore thread_id from binding after session split: %s", err))
		return
	}
	if binding == nil {
		return
	}
	threadID, _ := binding.Get("thread_id")
	if !pyrepr.Truthy(threadID) {
		return
	}
	restored := pyrepr.Str(threadID)
	tc.source.ThreadID = &restored
	runLog().Debug(fmt.Sprintf("Restored source.thread_id=%s from binding after session split %s → %s", restored, tc.sessionID, agentSessionID))
}

// Platform context, channel prompt and channel system prompt joined, or the API admission's ephemeral prompt
func (t *TurnRunner) combinedEphemeralPrompt(ctx context.Context) string {
	tc := t.ctx
	if api := gateway.APIExecution(ctx); api != nil {
		settings, _ := api.Get("settings")
		var prompt any
		switch s := settings.(type) {
		case map[string]any:
			prompt = s["ephemeral_system_prompt"]
		case interface{ Get(string) (any, bool) }:
			prompt, _ = s.Get("ephemeral_system_prompt")
		}
		if pyrepr.Truthy(prompt) {
			return pyrepr.Str(prompt)
		}
		return ""
	}
	combined := tc.contextPrompt
	channel := ""
	if tc.channelPrompt != nil {
		channel = pystr.Strip(*tc.channelPrompt)
	}
	extras := []string{channel, t.runner.getSystemPromptForChannel(ctx, tc.source.Platform, tc.source.ChatID, tc.source.ThreadID, tc.source.ParentChatID)}
	for _, extra := range extras {
		if extra != "" {
			combined = pystr.Strip(combined + "\n\n" + extra)
		}
	}
	return combined
}

// Final response with this turn's producer tool media tags appended once each
func appendAutoMediaTags(finalResponse string, result map[string]any, agentHistory []map[string]any, historyMediaPaths map[string]struct{}) (string, error) {
	if strings.Contains(finalResponse, "MEDIA:") {
		return finalResponse, nil
	}
	tags, voice, err := collectAutoAppendMediaTags(resultMessages(result), len(agentHistory), historyMediaPaths)
	if err != nil || len(tags) == 0 {
		return finalResponse, err
	}
	var unique []string
	if voice {
		unique = append(unique, "[[audio_as_voice]]")
	}
	seen := map[string]bool{}
	for _, tag := range tags {
		if !seen[tag] {
			seen[tag] = true
			unique = append(unique, tag)
		}
	}
	return finalResponse + "\n" + strings.Join(unique, "\n"), nil
}

// Turn message as a multimodal content list when native image paths are pending, else the plain text
func (t *TurnRunner) nativeImageRunMessage(ctx context.Context) any {
	tc := t.ctx
	if api := gateway.APIExecution(ctx); api != nil {
		if content, _ := api.Get("content"); content != nil {
			if parts, ok := content.([]any); ok {
				return parts
			}
		}
	}
	images := t.runner.consumePendingNativeImagePaths(tc.sessionKey)
	if len(images) == 0 {
		return tc.message
	}
	parts, skipped, err := tools.BuildNativeContentParts(t.toolsEnv(), tc.message, images, nil)
	if err != nil {
		runLog().Warn(fmt.Sprintf("Native image attachment failed, falling back to text: %s", err))
		return tc.message
	}
	if len(skipped) > 0 {
		runLog().Warn(fmt.Sprintf("Native image attachment: skipped %d unreadable path(s): %s", len(skipped), pyrepr.Repr(skipped)))
	}
	for _, part := range parts {
		if p, ok := part.(map[string]any); ok && p["type"] == "image_url" {
			return parts
		}
	}
	return tc.message
}
