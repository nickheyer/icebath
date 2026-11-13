package line

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// SDK message object every outbound bubble is built from
type messagingMessage = messaging_api.MessageInterface

// Size cap, size error and missing public URL error of one outbound media kind
type outboundMedia struct {
	maxBytes int64
	sizeErr  string
	urlErr   string
}

// Outbound media kinds with their LINE limits and errors
var outboundMediaKinds = map[string]outboundMedia{
	"image": {lineImageMaxBytes, "image exceeds 10 MB LINE limit",
		"LINE_PUBLIC_URL must be set to send images (LINE only accepts publicly reachable HTTPS URLs)"},
	"audio": {lineAVMaxBytes, "audio exceeds 200 MB LINE limit", "LINE_PUBLIC_URL must be set to send audio"},
	"video": {lineAVMaxBytes, "video exceeds 200 MB LINE limit", "LINE_PUBLIC_URL must be set to send video"},
}

// Text send that fills a pending postback entry unless it is an interim send, else a live wire send
func (a *LineAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	if a.client == nil {
		a.mu.Unlock()
		return gateway.SendResult{Success: false, Error: "LINE adapter not connected"}, nil
	}
	pendingRid := a.pendingButtons[chatID]
	if pendingRid != "" && !isInterimSend(content, metadata) {
		if entry := a.cache.Get(pendingRid); entry != nil && entry.state == StatePending {
			a.cache.SetReady(pendingRid, content)
			a.mu.Unlock()
			return gateway.SendResult{Success: true, MessageID: pendingRid}, nil
		}
		delete(a.pendingButtons, chatID)
	}
	a.mu.Unlock()
	return a.sendTextChunks(ctx, chatID, content, false)
}

// Content as stripped and chunked text bubbles under the text send contract
func (a *LineAdapter) sendTextChunks(ctx context.Context, chatID, content string, forcePush bool) (gateway.SendResult, error) {
	return a.sendMessages(ctx, chatID, textMessages(content), forcePush, true)
}

// Sends message objects five per call, the reply token first and push for the rest
func (a *LineAdapter) sendMessages(ctx context.Context, chatID string, messages []messagingMessage, forcePush, text bool) (gateway.SendResult, error) {
	client := a.currentClient()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "LINE adapter not connected"}, nil
	}
	if len(messages) == 0 {
		return gateway.SendResult{Success: true}, nil
	}
	var batches [][]messagingMessage
	for i := 0; i < len(messages); i += lineMaxMessagesPerCall {
		batches = append(batches, messages[i:min(i+lineMaxMessagesPerCall, len(messages))])
	}
	token, usedReply := a.consumeReplyToken(chatID)
	start := 0
	if usedReply && !forcePush {
		if err := client.reply(ctx, token, batches[0]); err != nil {
			slog.Info(fmt.Sprintf("LINE: reply token rejected (%v); falling back to push", err))
		} else {
			if text {
				return gateway.SendResult{Success: true, MessageID: token}, nil
			}
			start = 1
		}
	}
	for i := start; i < len(batches); i++ {
		if err := client.push(ctx, chatID, batches[i]); err != nil {
			if i > 0 {
				slog.Warn(fmt.Sprintf("LINE: push for follow-up batch failed: %v", err))
			} else if text {
				slog.Error(fmt.Sprintf("LINE: push send failed: %v", err))
			}
			return gateway.SendResult{Success: false, Error: err.Error()}, nil
		}
	}
	return gateway.SendResult{Success: true}, nil
}

// Registers a local file for HTTPS serving after evicting expired tokens and returns its URL token
func (a *LineAdapter) registerMedia(filePath string, cleanup bool) (string, error) {
	resolved, err := pypath.Resolve(filePath)
	if err != nil {
		return "", err
	}
	now := nowSeconds()
	a.mu.Lock()
	defer a.mu.Unlock()
	for token, entry := range a.mediaTokens {
		if now > entry.expiresAt {
			delete(a.mediaTokens, token)
			if a.mediaTempPaths[entry.path] {
				delete(a.mediaTempPaths, entry.path)
				unlinkQuietly(entry.path)
			}
		}
	}
	token := tokenURLSafe()
	a.mediaTokens[token] = mediaToken{path: resolved, expiresAt: now + a.mediaTTL}
	if cleanup {
		a.mediaTempPaths[resolved] = true
	}
	return token, nil
}

// Public URL of a registered media token, the profile prefix or the bind address when no public URL is set
func (a *LineAdapter) mediaURL(token, filename string) string {
	var base string
	if a.publicBaseURL != "" {
		base = a.publicBaseURL
	} else if ingress := a.SharedIngress(); ingress != nil && ingress.Base != "" {
		base = ingress.Base
	} else {
		host := "127.0.0.1"
		if !a.missingPublicURL() {
			host = *a.webhookHost
		}
		if a.webhookPort == 443 {
			base = "https://" + host
		} else {
			base = fmt.Sprintf("https://%s:%d", host, a.webhookPort)
		}
	}
	return base + defaultMediaPathPrefix + "/" + token + "/" + pyurl.Quote(filename, "")
}

// URL serving a local file under its own name
func (a *LineAdapter) serveFile(path string) (string, error) {
	resolved, err := pypath.Resolve(path)
	if err != nil {
		return "", err
	}
	token, err := a.registerMedia(resolved, false)
	if err != nil {
		return "", err
	}
	return a.mediaURL(token, filepath.Base(path)), nil
}

// Whether no public URL is set and the bind host is a wildcard or the dual stack default
func (a *LineAdapter) missingPublicURL() bool {
	if a.publicBaseURL != "" {
		return false
	}
	if ingress := a.SharedIngress(); ingress != nil && ingress.Base != "" {
		return false
	}
	return a.webhookHost == nil || wildcardHosts[*a.webhookHost]
}

// Preflight of an outbound media file, the failed send result when it is missing, oversized or unservable
func (a *LineAdapter) checkMediaFile(kind, filePath string) (string, *gateway.SendResult) {
	spec := outboundMediaKinds[kind]
	st, err := os.Stat(filePath)
	if err != nil || !st.Mode().IsRegular() {
		return "", &gateway.SendResult{Success: false, Error: fmt.Sprintf("%s file not found: %s", kind, filePath)}
	}
	for _, check := range []struct {
		failed bool
		err    string
	}{{st.Size() > spec.maxBytes, spec.sizeErr}, {a.currentClient() == nil, "LINE adapter not connected"},
		{a.missingPublicURL(), spec.urlErr}} {
		if check.failed {
			return "", &gateway.SendResult{Success: false, Error: check.err}
		}
	}
	return filePath, nil
}

// Whether path lies inside root by path components
func isRelativeTo(path, root string) bool {
	if root == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// Serves a registered file inside the temp dir, tmp or ICEBATH_HOME, 404, 410 or 403 otherwise
func (a *LineAdapter) handleMedia(ctx context.Context, request *gateway.WebRequest) (*gateway.WebResponse, error) {
	token := request.MatchInfo["token"]
	a.mu.Lock()
	entry, ok := a.mediaTokens[token]
	if !ok || entry.path == "" {
		a.mu.Unlock()
		return gateway.WebTextResponse(404, "not found", "", nil), nil
	}
	if nowSeconds() > entry.expiresAt {
		delete(a.mediaTokens, token)
		a.mu.Unlock()
		return gateway.WebTextResponse(410, "gone", "", nil), nil
	}
	a.mu.Unlock()
	if st, err := os.Stat(entry.path); err != nil || !st.Mode().IsRegular() {
		return gateway.WebTextResponse(404, "not found", "", nil), nil
	}
	hermesHome, err := pypath.Resolve(home.Dir(ctx))
	if err != nil {
		return nil, err
	}
	resolved, err := pypath.Resolve(entry.path)
	if err != nil {
		return nil, err
	}
	tempdir, err := pypath.Gettempdir()
	if err != nil {
		return nil, err
	}
	roots := []string{hermesHome}
	for _, dir := range []string{tempdir, "/tmp"} {
		r, err := pypath.Resolve(dir)
		if err != nil {
			return nil, err
		}
		roots = append(roots, r)
	}
	allowed := false
	for _, r := range roots {
		if isRelativeTo(resolved, r) {
			allowed = true
			break
		}
	}
	if !allowed {
		slog.Warn(fmt.Sprintf("LINE: refusing to serve outside allowed roots: %s", resolved))
		return gateway.WebTextResponse(403, "forbidden", "", nil), nil
	}
	contentType, _, _ := pymimetypes.GuessType(entry.path, true)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return gateway.WebFileResponse(request, entry.path, map[string]string{"Content-Type": contentType})
}

// Native image bubble served from a public HTTPS URL with an optional caption bubble
func (a *LineAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	path, failed := a.checkMediaFile("image", imagePath)
	if failed != nil {
		return *failed, nil
	}
	url, err := a.serveFile(path)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if !strings.HasPrefix(strings.ToLower(url), "https://") {
		return gateway.SendResult{Success: false, Error: "LINE image URL must be HTTPS: " + url}, nil
	}
	msgs := []messagingMessage{&messaging_api.ImageMessage{OriginalContentUrl: url, PreviewImageUrl: url}}
	if caption != "" {
		msgs = append(msgs, textMessage(caption))
	}
	return a.sendMessages(ctx, chatID, msgs, false, false)
}

// Native audio bubble with the duration_ms keyword, 1000 by default
func (a *LineAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	path, failed := a.checkMediaFile("audio", audioPath)
	if failed != nil {
		return *failed, nil
	}
	var durationMS int64 = 1000
	if v, ok := extra["duration_ms"]; ok {
		n, err := pyint.Int64(v)
		if err != nil {
			return gateway.SendResult{}, err
		}
		durationMS = n
	}
	url, err := a.serveFile(path)
	if err != nil {
		return gateway.SendResult{}, err
	}
	return a.sendMessages(ctx, chatID, []messagingMessage{&messaging_api.AudioMessage{OriginalContentUrl: url, Duration: durationMS}}, false, false)
}

// Native video bubble with the preview_path keyword or a served 1x1 PNG as its required preview
func (a *LineAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	path, failed := a.checkMediaFile("video", videoPath)
	if failed != nil {
		return *failed, nil
	}
	previewPath, _ := extra["preview_path"].(string)
	var previewURL string
	var err error
	if st, statErr := os.Stat(previewPath); previewPath != "" && statErr == nil && st.Mode().IsRegular() {
		if previewURL, err = a.serveFile(previewPath); err != nil {
			return gateway.SendResult{}, err
		}
	} else {
		if previewURL, err = a.servePreviewPNG(); err != nil {
			return gateway.SendResult{}, err
		}
	}
	url, err := a.serveFile(path)
	if err != nil {
		return gateway.SendResult{}, err
	}
	msg := &messaging_api.VideoMessage{OriginalContentUrl: url, PreviewImageUrl: previewURL}
	return a.sendMessages(ctx, chatID, []messagingMessage{msg}, false, false)
}

// Writes the fallback PNG to a temp file registered for cleanup and returns its URL
func (a *LineAdapter) servePreviewPNG() (string, error) {
	dir, err := pypath.Gettempdir()
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "tmp*.png")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	if _, err := tmp.Write(fallbackPNGPreview); err != nil {
		tmp.Close()
		unlinkQuietly(name)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		unlinkQuietly(name)
		return "", err
	}
	token, err := a.registerMedia(name, true)
	if err != nil {
		unlinkQuietly(name)
		return "", err
	}
	return a.mediaURL(token, "preview.png"), nil
}
