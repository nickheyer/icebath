package weixin

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	voiceExts = map[string]bool{".ogg": true, ".opus": true, ".mp3": true, ".wav": true, ".m4a": true, ".flac": true}
	videoExts = map[string]bool{".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true, ".3gp": true}
	imageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}
)

// Fresh unique id identifying one outbound message to iLink
func newClientID() string { return "icebath-weixin-" + strings.ReplaceAll(uuid.NewString(), "-", "") }

// Failed send result carrying the error text, a cancellation returned as the error
func sendFailure(ctx context.Context, err error) (gateway.SendResult, error) {
	if ctx.Err() != nil {
		return gateway.SendResult{}, ctx.Err()
	}
	return gateway.SendResult{Success: false, Error: err.Error()}, nil
}

// Seconds left on the rate limit circuit, zero when closed
func (a *Adapter) cooldownRemaining() time.Duration {
	a.circuitMu.Lock()
	defer a.circuitMu.Unlock()
	return max(0, time.Until(a.circuitUntil))
}

// Records a rate limit reply and reports whether the circuit is now open
func (a *Adapter) recordRateLimit() bool {
	now := time.Now()
	a.circuitMu.Lock()
	kept := a.rateEvents[:0]
	for _, at := range a.rateEvents {
		if !at.Before(now.Add(-a.circuitWindow)) {
			kept = append(kept, at)
		}
	}
	a.rateEvents = append(kept, now)
	tripped := len(a.rateEvents) >= a.circuitThreshold
	if tripped && a.circuitOpen > 0 {
		if until := time.Now().Add(a.circuitOpen); until.After(a.circuitUntil) {
			a.circuitUntil = until
		}
	}
	a.circuitMu.Unlock()
	return tripped && a.cooldownRemaining() > 0
}

// Closes the rate limit circuit after a delivered send
func (a *Adapter) resetRateLimit() {
	a.circuitMu.Lock()
	a.rateEvents = a.rateEvents[:0]
	a.circuitUntil = time.Time{}
	a.circuitMu.Unlock()
}

// Sends one text chunk under the send gate with retries, a tokenless stale session resend and the rate circuit
func (a *Adapter) sendTextChunk(ctx context.Context, client *http.Client, chatID, chunk, contextToken, clientID string) error {
	a.sendGate.Lock()
	defer a.sendGate.Unlock()
	var lastErr error
	retriedWithoutToken := false
	attempt := 0
	for {
		if remaining := a.cooldownRemaining(); remaining > 0 {
			return fmt.Errorf("iLink sendmessage rate limited; cooldown active for %.1fs", remaining.Seconds())
		}
		status, err := sendItems(ctx, client, a.baseURL, a.token, chatID, []outboundItem{textItem(chunk)}, contextToken, clientID)
		if err == nil && !status.ok() {
			if status.sessionExpired() && !retriedWithoutToken && contextToken != "" {
				retriedWithoutToken, contextToken = true, ""
				a.tokens.forget(a.accountID, chatID)
				slog.Warn(fmt.Sprintf("[%s] session expired for %s; retrying without context_token", a.Name(), safeID(chatID)))
				continue
			}
			if status.staleSession() {
				return sessionNotReadyError(status)
			}
			if !status.rateLimited() {
				err = sendMessageError(status)
			} else {
				reason := status.messageOr("rate limited")
				lastErr = fmt.Errorf("iLink sendmessage rate limited: ret=%s errcode=%s errmsg=%s", status.Ret, status.Errcode, reason)
				if a.recordRateLimit() {
					return fmt.Errorf("iLink sendmessage rate limited (ret=%s errcode=%s errmsg=%s); cooldown active for %.1fs",
						status.Ret, status.Errcode, reason, a.cooldownRemaining().Seconds())
				}
				if attempt >= a.chunkRetries {
					return lastErr
				}
				attempt++
				wait := a.chunkRetryDelay * 3
				slog.Warn(fmt.Sprintf("[%s] rate limited for %s; backing off %.1fs before retry", a.Name(), safeID(chatID), wait.Seconds()))
				if !sleepCtx(ctx, wait) {
					return ctx.Err()
				}
				continue
			}
		}
		if err == nil {
			a.resetRateLimit()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
		if attempt >= a.chunkRetries {
			return lastErr
		}
		attempt++
		wait := a.chunkRetryDelay * time.Duration(attempt)
		slog.Warn(fmt.Sprintf("[%s] send chunk failed to=%s attempt=%d/%d, retrying in %.2fs: %v", a.Name(), safeID(chatID), attempt,
			a.chunkRetries+1, wait.Seconds(), err))
		if wait > 0 && !sleepCtx(ctx, wait) {
			return ctx.Err()
		}
	}
}

// One outbound attachment found in reply text and the sender it goes through
type delivery struct {
	path    string
	isVoice bool
	label   string
}

// Attachments named by MEDIA tags and bare local paths, and the text left once they and markdown images are removed
func (a *Adapter) extractDeliveries(ctx context.Context, chatID, content string) ([]delivery, string, error) {
	scoped, release := a.MediaDeliveryScope(ctx, a.BuildSource(ctx, chatID, gateway.SourceFields{}))
	defer release()
	mediaFiles, cleaned, err := gateway.ExtractMedia(scoped, content)
	if err != nil {
		return nil, "", err
	}
	_, withoutImages := gateway.ExtractImages(cleaned)
	localFiles, final := gateway.ExtractLocalFiles(scoped, withoutImages)
	safeMedia, err := gateway.FilterMediaDeliveryPaths(scoped, mediaFiles)
	if err != nil {
		return nil, "", err
	}
	safeLocal, err := gateway.FilterLocalDeliveryPaths(scoped, localFiles, "")
	if err != nil {
		return nil, "", err
	}
	var out []delivery
	for _, m := range safeMedia {
		out = append(out, delivery{path: m.Path, isVoice: m.IsVoice, label: "media"})
	}
	for _, p := range safeLocal {
		out = append(out, delivery{path: p, label: "local file"})
	}
	return out, final, nil
}

// Delivers one attachment through the sender its extension or voice flag selects, the failure logged and returned
func (a *Adapter) sendDelivery(ctx context.Context, chatID string, d delivery, metadata map[string]any) (gateway.SendResult, error) {
	ext := strings.ToLower(filepath.Ext(d.path))
	var result gateway.SendResult
	var err error
	switch {
	case d.isVoice || voiceExts[ext]:
		result, err = a.SendVoice(ctx, chatID, d.path, "", "", metadata, nil)
	case videoExts[ext]:
		result, err = a.SendVideo(ctx, chatID, d.path, "", "", metadata, nil)
	case imageExts[ext]:
		result, err = a.SendImageFile(ctx, chatID, d.path, "", "", metadata, nil)
	default:
		result, err = a.SendDocument(ctx, chatID, d.path, "", "", "", metadata, nil)
	}
	if err != nil {
		return gateway.SendResult{}, err
	}
	if !result.Success {
		slog.Warn(fmt.Sprintf("[%s] %s delivery failed for %s: %v", a.Name(), d.label, d.path, result.Error))
	}
	return result, nil
}

// Sends attachments named in the content natively, then the remaining text in WeChat friendly chunks
func (a *Adapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	_, client := a.clients()
	if client == nil || a.token == "" {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	contextToken := a.tokens.get(a.accountID, chatID)
	deliveries, final, err := a.extractDeliveries(ctx, chatID, content)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] send failed to=%s: %v", a.Name(), safeID(chatID), err))
		return sendFailure(ctx, err)
	}
	var lastID any
	var failed *gateway.SendResult
	for _, d := range deliveries {
		result, err := a.sendDelivery(ctx, chatID, d, metadata)
		if err != nil {
			return gateway.SendResult{}, err
		}
		if result.Success {
			lastID = result.MessageID
		} else {
			failed = &result
		}
	}
	var chunks []string
	for _, c := range splitForDelivery(a.FormatMessage(final), maxMessageLength, a.splitMultiline) {
		if strings.TrimSpace(c) != "" {
			chunks = append(chunks, c)
		}
	}
	if len(chunks) == 0 && lastID == nil && failed != nil {
		return *failed, nil
	}
	for i, chunk := range chunks {
		clientID := newClientID()
		if err := a.sendTextChunk(ctx, client, chatID, chunk, contextToken, clientID); err != nil {
			slog.Error(fmt.Sprintf("[%s] send failed to=%s: %v", a.Name(), safeID(chatID), err))
			return sendFailure(ctx, err)
		}
		lastID = clientID
		if i < len(chunks)-1 && a.chunkDelay > 0 && !sleepCtx(ctx, a.chunkDelay) {
			return gateway.SendResult{}, ctx.Err()
		}
	}
	return gateway.SendResult{Success: true, MessageID: lastID}, nil
}

// Live ticket for the typing indicator of a peer, refreshed through getconfig once the cached one expired
func (a *Adapter) ensureTypingTicket(ctx context.Context, chatID string) string {
	if ticket := a.typing.get(chatID); ticket != "" {
		return ticket
	}
	_, client := a.clients()
	if client == nil || a.token == "" {
		return ""
	}
	return a.fetchTypingTicket(ctx, client, chatID, a.tokens.get(a.accountID, chatID), "typing ticket refresh failed")
}

// Starts or stops the WeChat typing indicator of a peer
func (a *Adapter) setTyping(ctx context.Context, chatID string, status int, label string) {
	ticket := a.ensureTypingTicket(ctx, chatID)
	_, client := a.clients()
	if ticket == "" || client == nil {
		return
	}
	if err := sendTyping(ctx, client, a.baseURL, a.token, chatID, ticket, status); err != nil {
		slog.Debug(fmt.Sprintf("[%s] typing %s failed for %s: %v", a.Name(), label, safeID(chatID), err))
	}
}

// Shows the typing indicator to the peer
func (a *Adapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.setTyping(ctx, chatID, typingStart, "start")
	return nil
}

// Clears the typing indicator of the peer
func (a *Adapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.setTyping(ctx, chatID, typingStop, "stop")
	return nil
}

// Downloads a public image URL into a temporary file named with the URL path extension
func (a *Adapter) downloadRemoteMedia(ctx context.Context, client *http.Client, rawURL string) (string, error) {
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), rawURL) {
		return "", fmt.Errorf("blocked unsafe URL (SSRF protection): %s", gateway.SafeURLForLog(rawURL, 80))
	}
	data, err := downloadBytes(ctx, client, rawURL, 30*time.Second)
	if err != nil {
		return "", err
	}
	suffix := ".bin"
	if u, err := url.Parse(rawURL); err == nil {
		if ext := path.Ext(u.Path); ext != "" && !strings.ContainsAny(ext, `*\`) {
			suffix = ext
		}
	}
	f, err := os.CreateTemp("", "icebath-weixin-*"+suffix)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// Image by URL or local path uploaded through the CDN, a downloaded copy removed afterwards
func (a *Adapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if !strings.HasPrefix(imageURL, "http://") && !strings.HasPrefix(imageURL, "https://") {
		filePath := strings.ReplaceAll(imageURL, "file://", "")
		if abs, err := filepath.Abs(filePath); err == nil {
			filePath = abs
		}
		return a.SendDocument(ctx, chatID, filePath, caption, "", replyTo, metadata, nil)
	}
	_, client := a.clients()
	if client == nil || a.token == "" {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	filePath, err := a.downloadRemoteMedia(ctx, client, imageURL)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] send_image failed to=%s: %v", a.Name(), safeID(chatID), err))
		return sendFailure(ctx, err)
	}
	defer os.Remove(filePath)
	return a.SendDocument(ctx, chatID, filePath, caption, "", replyTo, metadata, nil)
}

// Local image uploaded through the CDN as a native photo
func (a *Adapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFileResult(ctx, chatID, imagePath, caption, "", "send_image_file", false)
}

// Local file uploaded through the CDN as the item its mime type selects
func (a *Adapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFileResult(ctx, chatID, filePath, caption, fileName, "send_document", false)
}

// Local video uploaded through the CDN as an inline video
func (a *Adapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFileResult(ctx, chatID, videoPath, caption, "", "send_video", false)
}

// Audio uploaded as a playable file attachment captioned as standing in for a voice bubble
func (a *Adapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if caption == "" {
		if note := a.WarningText(ctx, i18n.T(ctx, "platform.weixin.voice_as_attachment", "", nil), nil, "", chatID, metadata); note != nil {
			caption = *note
		}
	}
	return a.sendFileResult(ctx, chatID, audioPath, caption, "", "send_voice", true)
}

// Upload of a local file wrapped as a send result
func (a *Adapter) sendFileResult(ctx context.Context, chatID, filePath, caption, fileName, label string, forceFile bool) (gateway.SendResult, error) {
	_, client := a.clients()
	if client == nil || a.token == "" {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	messageID, err := a.sendFile(ctx, client, chatID, filePath, caption, fileName, forceFile)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] %s failed to=%s: %v", a.Name(), label, safeID(chatID), err))
		return sendFailure(ctx, err)
	}
	return gateway.SendResult{Success: true, MessageID: messageID}, nil
}

// iLink upload category of a path, images and videos native, silk as voice unless forced to a file
func outboundMediaType(filePath string, forceFile bool) int {
	mimeType := guessMimeType(filePath)
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return mediaImage
	case strings.HasPrefix(mimeType, "video/"):
		return mediaVideo
	case strings.HasSuffix(filePath, ".silk") && !forceFile:
		return mediaVoice
	}
	return mediaFile
}

// Outbound item referencing an uploaded object for its media type, voice items described as 24 kHz silk
func mediaItem(mediaType int, media cdnMedia, ciphertextSize, plaintextSize int, fileName, rawMD5 string) outboundItem {
	switch mediaType {
	case mediaImage:
		return outboundItem{Type: itemImage, ImageItem: &imageOut{Media: media, MidSize: ciphertextSize}}
	case mediaVideo:
		return outboundItem{Type: itemVideo, VideoItem: &videoOut{Media: media, VideoSize: ciphertextSize, VideoMD5: rawMD5}}
	case mediaVoice:
		return outboundItem{Type: itemVoice, VoiceItem: &voiceOut{Media: media, EncodeType: 6, BitsPerSample: 16, SampleRate: 24000}}
	}
	return outboundItem{Type: itemFile, FileItem: &fileOut{Media: media, FileName: fileName, Len: fmt.Sprint(plaintextSize)}}
}

// Uploads an encrypted file to the CDN then sends caption and media item, returning the last client id
func (a *Adapter) sendFile(ctx context.Context, client *http.Client, chatID, filePath, caption, fileName string, forceFile bool) (string, error) {
	plaintext, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	mediaType := outboundMediaType(filePath, forceFile)
	var keyBytes, fileKeyBytes [16]byte
	if _, err := rand.Read(fileKeyBytes[:]); err != nil {
		return "", err
	}
	if _, err := rand.Read(keyBytes[:]); err != nil {
		return "", err
	}
	filekey, aesKey := hex.EncodeToString(fileKeyBytes[:]), keyBytes[:]
	sum := md5.Sum(plaintext)
	rawsize, rawMD5 := len(plaintext), hex.EncodeToString(sum[:])
	slot, err := getUploadURL(ctx, client, a.baseURL, a.token, chatID, mediaType, filekey, rawsize, rawMD5, ((rawsize+16)/16)*16, hex.EncodeToString(aesKey))
	if err != nil {
		return "", err
	}
	ciphertext, err := encryptECB(plaintext, aesKey)
	if err != nil {
		return "", err
	}
	uploadURL := string(slot.UploadFullURL)
	if uploadURL == "" && slot.UploadParam != "" {
		uploadURL = a.cdnBaseURL + "/upload?encrypted_query_param=" + quoteAll(string(slot.UploadParam)) + "&filekey=" + quoteAll(filekey)
	}
	if uploadURL == "" {
		return "", fmt.Errorf("getUploadUrl returned neither upload_param nor upload_full_url: %s", truncateRunes(slot.raw, 500))
	}
	encryptedParam, err := uploadCiphertext(ctx, client, uploadURL, ciphertext)
	if err != nil {
		return "", err
	}
	if fileName == "" {
		fileName = filepath.Base(filePath)
	}
	media := cdnMedia{EncryptQueryParam: flexString(encryptedParam), AESKey: flexString(base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(aesKey)))), EncryptType: 1}
	itemLists := [][]outboundItem{{mediaItem(mediaType, media, len(ciphertext), rawsize, fileName, rawMD5)}}
	if formatted := a.FormatMessage(caption); formatted != "" {
		itemLists = append([][]outboundItem{{textItem(formatted)}}, itemLists...)
	}
	contextToken := a.tokens.get(a.accountID, chatID)
	lastID := ""
	for _, items := range itemLists {
		lastID = newClientID()
		for {
			status, err := sendItems(ctx, client, a.baseURL, a.token, chatID, items, contextToken, lastID)
			if err != nil {
				return "", err
			}
			if status.ok() {
				break
			}
			if status.sessionExpired() && contextToken != "" {
				contextToken = ""
				a.tokens.forget(a.accountID, chatID)
				slog.Warn(fmt.Sprintf("[%s] session expired for %s; re-sending media without context_token", a.Name(), safeID(chatID)))
				continue
			}
			if status.staleSession() {
				return "", sessionNotReadyError(status)
			}
			return "", sendMessageError(status)
		}
	}
	return lastID, nil
}
