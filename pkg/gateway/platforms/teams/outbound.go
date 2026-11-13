package teams

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Approval choices to the card action ids their buttons carry
var eaCardActions = map[string]string{"once": "approve_once", "session": "approve_session", "always": "approve_always", "deny": "deny"}

// Shared button styles to adaptive card action styles
var eaCardStyles = map[string]string{"primary": "positive", "danger": "destructive"}

// Whether the adapter has a live app
func (a *TeamsAdapter) appReady() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.app != nil
}

// SendResult message id of a sent activity, nil when the connector gave none
func sentID(result *sentActivity) any {
	if result == nil || result.id == "" {
		return nil
	}
	return result.id
}

// Tracks an outbound activity id so require_mention exempts replies to the bot's own messages
func (a *TeamsAdapter) rememberSent(result *sentActivity) {
	if result == nil || result.id == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sentIDs = append(a.sentIDs, result.id)
	if len(a.sentIDs) > sentIDsKept {
		a.sentIDs = a.sentIDs[len(a.sentIDs)-sentIDsKept:]
	}
}

// Sends through the cached conversation reference of the chat, else as a proactive App.send
func (a *TeamsAdapter) sendViaConvRef(ctx context.Context, chatID string, activity map[string]any) (*sentActivity, error) {
	a.mu.Lock()
	ref, ok := a.convRefs[chatID]
	a.mu.Unlock()
	var result *sentActivity
	var err error
	if ok {
		result, err = a.createActivity(ctx, ref, activity)
	} else {
		result, err = a.appSend(ctx, chatID, activity)
	}
	if err != nil {
		return nil, err
	}
	a.rememberSent(result)
	return result, nil
}

// Sends an adaptive card through the stored conversation reference when there is one
func (a *TeamsAdapter) sendCard(ctx context.Context, chatID string, card *pyjson.Object) (*sentActivity, error) {
	if !a.appReady() {
		return nil, nil
	}
	return a.sendViaConvRef(ctx, chatID, cardActivity(card))
}

// Text chunks as message activities, threaded under a numeric reply id with a flat send fallback
func (a *TeamsAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if !a.appReady() {
		return gateway.SendResult{Success: false, Error: "Teams app not initialized"}, nil
	}
	var lastMessageID any
	for _, chunk := range gateway.TruncateMessage(a.FormatMessage(content), a.Traits.MaxMessageLength, a.MessageLenFn()) {
		var result *sentActivity
		var err error
		if replyTo != "" && strIsDigit(replyTo) && replyTo != "0" {
			var threaded string
			if threaded, err = toThreadedConversationID(chatID, replyTo); err == nil {
				result, err = a.appSend(ctx, threaded, textActivity(chunk))
			}
			if err != nil {
				slog.Debug(fmt.Sprintf("Teams reply() failed, falling back to flat send: %v", err))
				result, err = a.appSend(ctx, chatID, textActivity(chunk))
			}
		} else {
			result, err = a.appSend(ctx, chatID, textActivity(chunk))
		}
		if err != nil {
			return gateway.SendResult{Success: false, Error: err.Error(), Retryable: true}, nil
		}
		lastMessageID = sentID(result)
		a.rememberSent(result)
	}
	return gateway.SendResult{Success: true, MessageID: lastMessageID}, nil
}

// Typing activity to the chat, failures ignored
func (a *TeamsAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	if a.appReady() {
		_, _ = a.appSend(ctx, chatID, map[string]any{"type": "typing"})
	}
	return nil
}

// Approval prompt as an adaptive card whose buttons carry only the prompt id and the choice
func (a *TeamsAdapter) SendExecApprovalPrompt(ctx context.Context, prompt *gateway.ExecApprovalPrompt) (gateway.SendResult, error) {
	if !a.appReady() {
		return gateway.SendResult{Success: false, Error: "Teams app not initialized"}, nil
	}
	cmd := gateway.TruncatePreview(prompt.Command, eaCmdBudget, "...")
	promptID := strings.ReplaceAll(uuid.NewString(), "-", "")
	a.mu.Lock()
	a.approvalPreviews[promptID] = [2]string{cmd, prompt.Description}
	a.approvalOrder = append(a.approvalOrder, promptID)
	if len(a.approvalPreviews) > eaPreviewCap {
		oldest := a.approvalOrder[0]
		a.approvalOrder = a.approvalOrder[1:]
		delete(a.approvalPreviews, oldest)
	}
	a.mu.Unlock()
	actions := []any{}
	for _, action := range prompt.Actions {
		data := pyjson.ObjectOf("session_key", prompt.SessionKey, "prompt_id", promptID, "hermes_action", eaCardActions[action.Choice])
		btn := pyjson.ObjectOf("type", "Action.Execute", "title", action.Label, "verb", "hermes_approve", "data", data)
		if action.Style != "" {
			btn.Set("style", eaCardStyles[action.Style])
		}
		actions = append(actions, btn)
	}
	body := approvalBody(ctx, cmd, prompt.Description, true)
	body = append(body, textBlock(gateway.FormatApprovalDeadlineLine(ctx, gateway.ApprovalTimeoutSeconds(ctx)), "", false))
	if prompt.SmartDenied {
		body = append(body, textBlock(pystr.Strip(a.EASmartDenyLine(ctx)), "", false))
	}
	result, err := a.sendCard(ctx, prompt.ChatID, adaptiveCard(body, actions))
	if err != nil {
		slog.Error(fmt.Sprintf("[teams] send_exec_approval failed: %v", err))
		return gateway.SendResult{Success: false, Error: err.Error(), Retryable: true}, nil
	}
	return gateway.SendResult{Success: true, MessageID: sentID(result)}, nil
}

// Media file or URL as a Teams attachment, URLs by reference and local files as a base64 data URI
func (a *TeamsAdapter) sendMediaAttachment(ctx context.Context, chatID, source, defaultMime, caption, mediaLabel string) (gateway.SendResult, error) {
	if !a.appReady() {
		return gateway.SendResult{Success: false, Error: "Teams app not initialized"}, nil
	}
	result, err := func() (*sentActivity, error) {
		var contentURL, mimeType string
		if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
			contentURL = source
			mimeType, _, _ = pymimetypes.GuessType(strings.SplitN(source, "?", 2)[0], true)
			if mimeType == "" {
				mimeType = defaultMime
			}
		} else {
			path := strings.TrimPrefix(source, "file://")
			mimeType, _, _ = pymimetypes.GuessType(path, true)
			if mimeType == "" {
				mimeType = defaultMime
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			contentURL = "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)
		}
		activity := map[string]any{"type": "message",
			"attachments": []any{pyjson.ObjectOf("contentType", mimeType, "contentUrl", contentURL)}}
		if caption != "" {
			activity["text"] = caption
		}
		return a.sendViaConvRef(ctx, chatID, activity)
	}()
	if err != nil {
		slog.Error(fmt.Sprintf("[teams] send_%s failed: %v", mediaLabel, err))
		return gateway.SendResult{Success: false, Error: err.Error(), Retryable: true}, nil
	}
	return gateway.SendResult{Success: true, MessageID: sentID(result)}, nil
}

// Image URL or path as an attachment
func (a *TeamsAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.sendMediaAttachment(ctx, chatID, imageURL, "image/png", caption, "image")
}

// Local image as an attachment
func (a *TeamsAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.SendImage(ctx, chatID, imagePath, caption, replyTo, nil)
}

// Video as an attachment
func (a *TeamsAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaAttachment(ctx, chatID, videoPath, "video/mp4", caption, "video")
}

// Audio as an attachment
func (a *TeamsAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaAttachment(ctx, chatID, audioPath, "audio/mpeg", caption, "voice")
}

// Document as an attachment
func (a *TeamsAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendMediaAttachment(ctx, chatID, filePath, "application/octet-stream", caption, "document")
}
