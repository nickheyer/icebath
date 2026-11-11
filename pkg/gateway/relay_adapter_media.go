package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"maps"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var relayLevelWarning, relayLevelInfo = slog.LevelWarn, slog.LevelInfo

// Media client built lazily from the dial URL and gateway credentials, nil when unavailable
func (a *RelayAdapter) getMediaClient(ctx context.Context) *RelayMediaClient {
	a.mu.Lock()
	cached := a.mediaClient
	a.mu.Unlock()
	if cached != nil {
		return cached
	}
	url, err := RelayURLChecked(ctx)
	if err != nil {
		relayAdapterLog().Debug("relay media client init failed", "exc_info", err)
		return nil
	}
	gatewayID, secret := RelayConnectionAuth(ctx)
	if url == "" {
		return nil
	}
	client := NewRelayMediaClient(RelayMediaBaseURL(url), gatewayID, secret)
	if !client.Enabled() {
		return nil
	}
	a.mu.Lock()
	a.mediaClient = client
	a.mu.Unlock()
	return client
}

// One media object through the send_media op, nil when the lane is unavailable and a not delivered reason for a dropped local file
func (a *RelayAdapter) sendMedia(ctx context.Context, chatID, mediaKind, source string, sourceIsPath bool, caption, filename string, replyTo *string, metadata map[string]any) (*SendResult, *relayMediaNotDelivered, error) {
	if a.transport == nil || !a.Descriptor().SupportsOp("send_media") {
		return nil, nil, nil
	}
	sourceURL := source
	if sourceIsPath {
		client := a.getMediaClient(ctx)
		if client == nil {
			return nil, nil, nil
		}
		var name *string
		if filename != "" {
			name = &filename
		}
		uploaded, why, err := client.UploadWithError(ctx, source, nil, name)
		if err != nil {
			return nil, nil, err
		}
		if uploaded == "" {
			return nil, &relayMediaNotDelivered{reason: "relay media upload failed: " + why}, nil
		}
		sourceURL = uploaded
	}
	mediaMetadata := maps.Clone(metadata)
	if mediaMetadata == nil {
		mediaMetadata = map[string]any{}
	}
	effectiveReplyTo := a.prepareSlackEgress(chatID, replyTo, mediaMetadata)
	action := map[string]any{"op": "send_media", "chat_id": chatID, "media_kind": mediaKind, "source_url": sourceURL,
		"content": caption, "reply_to": relayWireReply(effectiveReplyTo), "metadata": a.withScope(chatID, mediaMetadata)}
	if filename != "" {
		action["filename"] = filename
	}
	result := a.gatedOp(ctx, chatID, action, relayGateOptions{declineLevel: &relayLevelWarning, surfaceDeclines: true, surfaceAmbiguous: true})
	if result == nil {
		if sourceIsPath {
			return nil, &relayMediaNotDelivered{reason: "connector did not deliver the attachment"}, nil
		}
		return nil, nil, nil
	}
	if pyrepr.Truthy(result["ambiguous"]) {
		relayAdapterLog().Warn(fmt.Sprintf("relay send_media to %s unconfirmed (ack lost): %s", chatID, pyrepr.Str(result["error"])))
		return &SendResult{Success: true, RawResponse: map[string]any{"ambiguous": true, "error": result["error"]}}, nil, nil
	}
	if !pyrepr.Truthy(result["success"]) {
		return &SendResult{Success: false, Error: DeclineError(result), RawResponse: result}, nil, nil
	}
	return &SendResult{Success: true, MessageID: result["message_id"], RawResponse: result}, nil, nil
}

// Image URL through the send_media lane, else the base text and URL behaviour
func (a *RelayAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (SendResult, error) {
	result, _, err := a.sendMedia(ctx, chatID, "image", imageURL, false, caption, "", relayOptReply(replyTo), metadata)
	if err != nil {
		return SendResult{}, err
	}
	if result != nil {
		return *result, nil
	}
	return a.BasePlatformAdapter.SendImage(ctx, chatID, imageURL, caption, replyTo, metadata)
}

func (a *RelayAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (SendResult, error) {
	return a.sendLocalMedia(ctx, chatID, "image", imagePath, caption, replyTo, metadata, "", func() (SendResult, error) {
		return a.BasePlatformAdapter.SendImageFile(ctx, chatID, imagePath, caption, replyTo, metadata, extra)
	})
}

func (a *RelayAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (SendResult, error) {
	return a.sendLocalMedia(ctx, chatID, "voice", audioPath, caption, replyTo, metadata, "", func() (SendResult, error) {
		return a.BasePlatformAdapter.SendVoice(ctx, chatID, audioPath, caption, replyTo, metadata, extra)
	})
}

func (a *RelayAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (SendResult, error) {
	return a.sendLocalMedia(ctx, chatID, "video", videoPath, caption, replyTo, metadata, "", func() (SendResult, error) {
		return a.BasePlatformAdapter.SendVideo(ctx, chatID, videoPath, caption, replyTo, metadata, extra)
	})
}

func (a *RelayAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (SendResult, error) {
	return a.sendLocalMedia(ctx, chatID, "document", filePath, caption, replyTo, metadata, fileName, func() (SendResult, error) {
		return a.BasePlatformAdapter.SendDocument(ctx, chatID, filePath, caption, fileName, replyTo, metadata, extra)
	})
}

// Local file through the send_media lane, else the base fallback notice, a file the lane dropped reported failed
func (a *RelayAdapter) sendLocalMedia(ctx context.Context, chatID, mediaKind, path, caption, replyTo string, metadata map[string]any, filename string, fallback func() (SendResult, error)) (SendResult, error) {
	result, notDelivered, err := a.sendMedia(ctx, chatID, mediaKind, path, true, caption, filename, relayOptReply(replyTo), metadata)
	if err != nil {
		return SendResult{}, err
	}
	if result != nil {
		return *result, nil
	}
	shown, err := fallback()
	if err != nil {
		return SendResult{}, err
	}
	if notDelivered == nil {
		return shown, nil
	}
	return SendResult{Success: false, MessageID: shown.MessageID, Error: "attachment not delivered: " + notDelivered.reason,
		RawResponse: map[string]any{"failure_notice_sent": shown.Success, "fallback": shown.RawResponse}}, nil
}
