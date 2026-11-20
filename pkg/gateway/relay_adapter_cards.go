package gateway

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Task card frame result, a SendResult when the op is unadvertised, transportless or failed in transport
func (a *RelayAdapter) cardFrame(ctx context.Context, chatID, op string, replyTo *string, metadata map[string]any, fields map[string]any) (map[string]any, *SendResult, error) {
	if !a.SupportsNativeTaskCards() {
		return nil, &SendResult{Success: false, Error: "connector does not advertise task_card"}, nil
	}
	if a.transport == nil {
		return nil, &SendResult{Success: false, Error: "no transport"}, nil
	}
	rt := ""
	if replyTo != nil {
		rt = *replyTo
	}
	frame := map[string]any{"op": op, "chat_id": chatID, "card_id": relayCardKey(rt, metadata)}
	for k, v := range fields {
		frame[k] = v
	}
	frame["metadata"] = a.withScope(chatID, metadata)
	result, err := a.outbound(ctx, chatID, frame)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, err
		}
		return nil, &SendResult{Success: false, Error: op + " transport error: " + relayExcStr(err)}, nil
	}
	return result, nil, nil
}

// Card metadata threaded under the reply target unless a thread is already set
func relayTaskCardMetadata(replyTo *string, metadata map[string]any) map[string]any {
	merged := map[string]any{}
	for k, v := range metadata {
		merged[k] = v
	}
	if _, has := merged["thread_ts"]; replyTo != nil && *replyTo != "" && !has {
		merged["thread_ts"] = *replyTo
	}
	return merged
}

// Whether a chat can host a card, a Slack chat only inside a thread
func (a *RelayAdapter) NativeTaskCardDestinationSupported(chatID string, replyTo *string, metadata map[string]any) *bool {
	ok := true
	if a.chatPlatform(chatID) != relaySlack {
		return &ok
	}
	md := relayTaskCardMetadata(replyTo, metadata)
	thread, has := md["thread_id"]
	if !has || thread == nil {
		thread = md["thread_ts"]
	}
	_, ok = thread.(string)
	return &ok
}

// Sends the task card progress chunks, a decline carried in the result
func (a *RelayAdapter) SendNativeTaskCardProgress(ctx context.Context, chatID string, tasks []map[string]string, title string, replyTo *string, metadata map[string]any, fallbackText string) (SendResult, error) {
	merged := relayTaskCardMetadata(replyTo, metadata)
	chunks := make([]any, len(tasks))
	for i, task := range tasks {
		row := map[string]any{}
		for k, v := range task {
			row[k] = v
		}
		chunks[i] = row
	}
	result, sr, err := a.cardFrame(ctx, chatID, "task_card", replyTo, merged, map[string]any{"chunks": chunks})
	if err != nil || sr != nil {
		return relayDeref(sr), err
	}
	if pyrepr.Truthy(result["success"]) {
		return SendResult{Success: true}, nil
	}
	if IsEgressDecline(result) {
		LogDecline("task_card", chatID, result)
	}
	errText := pyrepr.Or(result["error"], DeclineError(result))
	return SendResult{Success: false, Error: pyrepr.Str(pyrepr.Or(errText, "task_card failed")), RawResponse: result}, nil
}

// Closes the card stream of the turn, the connector result carried back
func (a *RelayAdapter) StopNativeTaskCardProgress(ctx context.Context, chatID string, replyTo *string, metadata map[string]any) (SendResult, error) {
	md := map[string]any{}
	for k, v := range metadata {
		md[k] = v
	}
	result, sr, err := a.cardFrame(ctx, chatID, "task_card_stop", replyTo, md, nil)
	if err != nil || sr != nil {
		return relayDeref(sr), err
	}
	return SendResult{Success: pyrepr.Truthy(result["success"]), Error: result["error"], RawResponse: result}, nil
}

// Seals an orphaned open stream with the text, a no op when nothing is armed
func (a *RelayAdapter) AbandonOpenDraft(ctx context.Context, chatID, content string, metadata map[string]any) (SendResult, error) {
	key, ok := a.matchOpenDraft(chatID, metadata)
	if !ok {
		return SendResult{Success: true}, nil
	}
	result, err := a.sealOpenDraft(ctx, chatID, content, metadata, key)
	if err != nil {
		if ctx.Err() != nil {
			return SendResult{}, err
		}
		return SendResult{Success: false, Error: fmt.Sprintf("abandon seal transport error: %s", relayExcStr(err))}, nil
	}
	return result, nil
}

func relayDeref(r *SendResult) SendResult {
	if r == nil {
		return SendResult{}
	}
	return *r
}
