package bluebubbles

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Whether path names an existing regular file
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Sends a file through the BlueBubbles multipart attachment upload, the caption following as text
func (a *BlueBubblesAdapter) sendAttachment(ctx context.Context, chatID, filePath, filename, caption string, isAudioMessage bool) (gateway.SendResult, error) {
	client := a.httpClient()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if !isFile(filePath) {
		return gateway.SendResult{Success: false, Error: "File not found: " + filePath}, nil
	}
	guid, err := a.resolveChatGUID(ctx, chatID)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if guid == "" {
		return gateway.SendResult{Success: false, Error: "Chat not found: " + chatID}, nil
	}
	fname := filename
	if fname == "" {
		fname = pypath.Basename(filePath)
	}
	result, err := a.uploadAttachment(ctx, client, chatID, filePath, fname, guid, caption, isAudioMessage)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		return gateway.SendResult{Success: false, Error: pyrepr.ExceptionMessage(err)}, nil
	}
	return result, nil
}

// Upload request of one attachment and the send result its JSON answer describes
func (a *BlueBubblesAdapter) uploadAttachment(ctx context.Context, client *http.Client, chatID, filePath, fname, guid, caption string, isAudioMessage bool) (gateway.SendResult, error) {
	payload, err := os.ReadFile(filePath)
	if err != nil {
		return gateway.SendResult{}, &pyErr{pypath.OSErrorClass(err), pypath.OSErrorText(err)}
	}
	parts := []gateway.MultipartPart{{Name: "chatGuid", Data: []byte(guid)}, {Name: "name", Data: []byte(fname)},
		{Name: "tempGuid", Data: []byte(strings.ReplaceAll(uuid.NewString(), "-", ""))}}
	if isAudioMessage {
		parts = append(parts, gateway.MultipartPart{Name: "isAudioMessage", Data: []byte("true")})
	}
	parts = append(parts, gateway.MultipartPart{Name: "attachment", Filename: fname, ContentType: "application/octet-stream", Data: payload})
	body, contentType, err := gateway.EncodeHTTPXMultipart(parts)
	if err != nil {
		return gateway.SendResult{}, err
	}
	rawURL := a.apiURL("/api/v1/message/attachment")
	resp, err := doRequest(ctx, client, http.MethodPost, rawURL, body, contentType, 120*time.Second)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if err := raiseForStatus(resp, rawURL); err != nil {
		return gateway.SendResult{}, err
	}
	result, err := resp.JSON()
	if err != nil {
		return gateway.SendResult{}, err
	}
	if caption != "" {
		if _, err := a.Send(ctx, chatID, caption, "", nil); err != nil {
			return gateway.SendResult{}, err
		}
	}
	rm, err := dictOf(result)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if pyrepr.Equal(rm["status"], int64(200)) {
		var msgID any
		if rd, isDict := pyjson.Dict(pyrepr.Or(rm["data"], map[string]any{})); isDict {
			msgID = rd["guid"]
		}
		return gateway.SendResult{Success: true, MessageID: msgID, RawResponse: result}, nil
	}
	return gateway.SendResult{Success: false, Error: getOr(rm, "message", "Attachment upload failed")}, nil
}

// Image URL cached and sent as an attachment, the base text send when caching fails
func (a *BlueBubblesAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	path, err := cacheImageFromURL(ctx, imageURL, ".jpg", 2)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		return a.BasePlatformAdapter.SendImage(ctx, chatID, imageURL, caption, replyTo, nil)
	}
	return a.sendAttachment(ctx, chatID, path, "", caption, false)
}

// Local image sent as an attachment
func (a *BlueBubblesAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendAttachment(ctx, chatID, imagePath, "", caption, false)
}

// Animated image by URL sent the way an image is, iMessage having no separate animation send
func (a *BlueBubblesAdapter) SendAnimation(ctx context.Context, chatID, animationURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.SendImage(ctx, chatID, animationURL, caption, replyTo, metadata)
}

// Audio sent as an iMessage voice memo unless is_voice is exactly false for a plain audio file
func (a *BlueBubblesAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	isVoice, isBool := extra["is_voice"].(bool)
	return a.sendAttachment(ctx, chatID, audioPath, "", caption, !isBool || isVoice)
}

// Local video sent as an attachment
func (a *BlueBubblesAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendAttachment(ctx, chatID, videoPath, "", caption, false)
}

// Local document sent as an attachment under its given file name
func (a *BlueBubblesAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendAttachment(ctx, chatID, filePath, fileName, caption, false)
}

// Typing indicator through the private API helper
func (a *BlueBubblesAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.privateAPIChatCall(ctx, chatID, "typing", http.MethodPost)
	return nil
}

// Typing indicator cleared through the private API helper
func (a *BlueBubblesAdapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.privateAPIChatCall(ctx, chatID, "typing", http.MethodDelete)
	return nil
}

// Chat marked read through the private API helper, true when the call was made
func (a *BlueBubblesAdapter) MarkRead(ctx context.Context, chatID string) bool {
	return a.privateAPIChatCall(ctx, chatID, "read", http.MethodPost)
}

// Inbound attachment downloaded into the image, audio or document cache, empty on failure
func (a *BlueBubblesAdapter) downloadAttachmentToCache(ctx context.Context, attGUID string, attMeta map[string]any) (string, error) {
	a.mu.Lock()
	client := a.followClient
	a.mu.Unlock()
	if client == nil {
		return "", nil
	}
	path, err := func() (string, error) {
		rawURL := a.apiURL("/api/v1/attachment/" + pyurl.Quote(attGUID, "") + "/download")
		resp, err := doRequest(ctx, client, http.MethodGet, rawURL, nil, "", 60*time.Second)
		if err != nil {
			return "", err
		}
		if err := raiseForStatus(resp, rawURL); err != nil {
			return "", err
		}
		mime, err := lowerText(attMeta["mimeType"])
		if err != nil {
			return "", err
		}
		switch {
		case strings.HasPrefix(mime, "image/"):
			return cacheImageFromBytes(ctx, resp.Content, closedExt(mime, imageExtOverrides, ".jpg"))
		case strings.HasPrefix(mime, "audio/"):
			return cacheAudioFromBytes(ctx, resp.Content, closedExt(mime, audioExtOverrides, ".mp3"))
		}
		name := textOf(getOr(attMeta, "transferName", ""))
		if name == "" {
			name = "file_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
		}
		return cacheDocumentFromBytes(ctx, resp.Content, name)
	}()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[bluebubbles] failed to download attachment %s: %s", redact(attGUID), pyrepr.ExceptionMessage(err)))
		return "", nil
	}
	return path, nil
}

// Lowercased text of an optional string field, the AttributeError a non string value raises
func lowerText(v any) (string, error) {
	if !pyrepr.Truthy(v) {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", noAttr(v, "lower")
	}
	return pystr.Lower(s), nil
}
