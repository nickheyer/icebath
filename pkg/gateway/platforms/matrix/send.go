package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Sends the formatted content in UTF-8 byte bounded chunks, retrying once after a key share in E2EE rooms
func (a *MatrixAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if content == "" {
		return gateway.SendResult{Success: true}, nil
	}
	var lastEventID any
	for _, chunk := range gateway.TruncateMessage(a.FormatMessage(content), a.maxMessageLength, a.MessageLenFn()) {
		msgContent := a.buildTextMessageContent(chunk, "m.text")
		applyRelationMetadata(msgContent, replyTo, metadata)
		eventID, err := a.sendRoomMessage(ctx, chatID, msgContent)
		if err == nil {
			lastEventID = eventID
			slog.Info(fmt.Sprintf("Matrix: sent event %s to %s", eventID, chatID))
			continue
		}
		crypto := a.cryptoMachine()
		if !(a.encryptionOn() && crypto != nil) {
			slog.Error(fmt.Sprintf("Matrix: failed to send to %s: %v", chatID, err))
			return gateway.SendResult{Success: false, Error: err.Error()}, nil
		}
		if err = crypto.shareKeys(ctx); err == nil {
			eventID, err = a.sendRoomMessage(ctx, chatID, msgContent)
		}
		if err != nil {
			slog.Error(fmt.Sprintf("Matrix: failed to send to %s after retry: %v", chatID, err))
			return gateway.SendResult{Success: false, Error: err.Error()}, nil
		}
		lastEventID = eventID
		slog.Info(fmt.Sprintf("Matrix: sent event %s to %s (after key share)", eventID, chatID))
	}
	return gateway.SendResult{Success: true, MessageID: lastEventID}, nil
}

// Sends one m.room.message event within forty five seconds and returns its event id
func (a *MatrixAdapter) sendRoomMessage(ctx context.Context, chatID string, content map[string]any) (string, error) {
	client := a.currentClient()
	if client == nil {
		return "", errors.New("'NoneType' object has no attribute 'send_message_event'")
	}
	sctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	resp, err := client.SendMessageEvent(sctx, id.RoomID(chatID), event.EventMessage, content)
	if err != nil {
		return "", err
	}
	return string(resp.EventID), nil
}

// Seed message whose event id becomes the handoff thread id, marked so replies bypass require mention
func (a *MatrixAdapter) CreateHandoffThread(ctx context.Context, parentChatID, name string) (string, bool, error) {
	if a.currentClient() == nil {
		return "", false, nil
	}
	title := pystr.Strip(name)
	if title == "" {
		title = i18n.T(ctx, "platform.matrix.handoff.default_name", "", nil)
	}
	result, err := a.Send(ctx, parentChatID, title, "", nil)
	if err != nil {
		return "", false, err
	}
	root := ""
	if result.Success && result.MessageID != nil {
		root = strOf(result.MessageID)
	}
	if root == "" {
		return "", false, nil
	}
	if err := a.threads.MarkAsync(ctx, root); err != nil {
		return "", false, err
	}
	return root, true, nil
}

// Edits a sent message with m.replace, shrinking the fallback copy when the event would pass the cap
func (a *MatrixAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	formatted := a.FormatMessage(content)
	newContent := a.buildTextMessageContent(formatted, "m.text")
	msgContent := map[string]any{"msgtype": "m.text", "body": "* " + formatted, "m.new_content": newContent}
	if mentions, ok := newContent["m.mentions"]; ok {
		msgContent["m.mentions"] = mentions
	}
	if body, ok := newContent["formatted_body"]; ok {
		msgContent["format"] = "org.matrix.custom.html"
		msgContent["formatted_body"] = "* " + strOf(body)
	}
	msgContent["m.relates_to"] = map[string]any{"rel_type": "m.replace", "event_id": messageID}
	if contentBytes(msgContent) > maxContentBytes {
		delete(msgContent, "format")
		delete(msgContent, "formatted_body")
		msgContent["body"] = ""
		if contentBytes(msgContent) > maxContentBytes {
			delete(msgContent, "m.mentions")
		}
		room := maxContentBytes - contentBytes(msgContent)
		raw := []byte(formatted)
		raw = raw[:min(len(raw), max(0, room/6))]
		msgContent["body"] = "* " + strings.ToValidUTF8(string(raw), "") + "…"
	}
	return a.sendContentEvent(ctx, chatID, msgContent), nil
}

// Downloads an external image with redirect and size guards and uploads it natively
func (a *MatrixAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), imageURL) {
		slog.Warn("Matrix: blocked unsafe image URL (SSRF protection)")
		return a.BasePlatformAdapter.SendImage(ctx, chatID, imageURL, caption, replyTo, metadata)
	}
	data, ct, fname, err := a.downloadExternalMediaWithCap(ctx, imageURL)
	if err != nil {
		slog.Warn(fmt.Sprintf("Matrix: failed to download image %s: %v", redactURLForLog(imageURL), err))
		fallback := i18n.T(ctx, "platform.matrix.media.image_download_failed", "", nil)
		return a.EmitMediaWarning(ctx, chatID, fallback, caption, replyTo, metadata, nil)
	}
	return a.uploadAndSend(ctx, chatID, data, fname, ct, "m.image", caption, replyTo, metadata, false, nil), nil
}

// External image bytes, content type and filename under redirect re-validation and the media size cap
func (a *MatrixAdapter) downloadExternalMediaWithCap(ctx context.Context, rawURL string) ([]byte, string, string, error) {
	env := gateway.URLSafetyEnv(ctx)
	if !tools.IsSafeURL(env, rawURL) {
		return nil, "", "", errors.New("blocked unsafe media URL")
	}
	fname := rawURL[strings.LastIndex(rawURL, "/")+1:]
	fname, _, _ = strings.Cut(fname, "?")
	if fname == "" {
		fname = "image.png"
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if a.proxyURL != "" {
		connector, err := gateway.NewProxyConnector(a.proxyURL)
		if err != nil {
			return nil, "", "", err
		}
		transport = connector.Transport()
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	fetchURL := rawURL
	for range 20 {
		data, ct, location, err := a.fetchCapped(ctx, client, fetchURL)
		if err != nil {
			return nil, "", "", err
		}
		if location == nil {
			return data, ct, fname, nil
		}
		if *location == "" {
			return nil, "", "", errors.New("redirect missing Location")
		}
		next, err := pyurl.Join(fetchURL, *location)
		if err != nil {
			return nil, "", "", err
		}
		fetchURL = next
		if !tools.IsSafeURL(env, fetchURL) {
			return nil, "", "", errors.New("blocked unsafe redirect URL")
		}
	}
	return nil, "", "", errors.New("too many redirects")
}

// One hop of the guarded download, the Location header when the response redirects
func (a *MatrixAdapter) fetchCapped(ctx context.Context, client *http.Client, fetchURL string) ([]byte, string, *string, error) {
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return nil, "", nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 301, 302, 303, 307, 308:
		location := resp.Header.Get("Location")
		return nil, "", &location, nil
	}
	if resp.StatusCode >= 400 {
		return nil, "", nil, fmt.Errorf("%d, message='%s', url='%s'", resp.StatusCode, strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)+" "), fetchURL)
	}
	if size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err == nil && size > a.maxMediaBytes {
		return nil, "", nil, fmt.Errorf("media exceeds Matrix limit (%d > %d bytes)", size, a.maxMediaBytes)
	}
	var parts []byte
	buf := make([]byte, 65536)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if int64(len(parts)+n) > a.maxMediaBytes {
				return nil, "", nil, fmt.Errorf("media exceeds Matrix limit (> %d bytes)", a.maxMediaBytes)
			}
			parts = append(parts, buf[:n]...)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, "", nil, rerr
		}
	}
	contentType := "application/octet-stream"
	if header := resp.Header.Get("Content-Type"); header != "" {
		if mt, _, err := mime.ParseMediaType(header); err == nil {
			contentType = mt
		} else {
			contentType = header
		}
	}
	contentType, _, _ = strings.Cut(contentType, ";")
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if !strings.HasPrefix(contentType, "image/") {
		return nil, "", nil, errors.New("external media is not an image")
	}
	return parts, contentType, nil, nil
}

// Uploads a local image natively
func (a *MatrixAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, imagePath, "m.image", caption, replyTo, "", metadata, false)
}

// Sends each image with a numbered caption, succeeding when at least one lands
func (a *MatrixAdapter) SendMultipleImages(ctx context.Context, chatID string, images []gateway.ImageRef, metadata map[string]any, humanDelay float64) (gateway.SendResult, error) {
	if len(images) == 0 {
		return gateway.SendResult{Success: false, Error: "no images to send"}, nil
	}
	total := len(images)
	delivered := false
	for idx, img := range images {
		n := idx + 1
		if humanDelay > 0 && n > 1 {
			select {
			case <-ctx.Done():
				return gateway.SendResult{}, ctx.Err()
			case <-time.After(time.Duration(humanDelay * float64(time.Second))):
			}
		}
		caption := img.Alt
		if img.Alt != "" && total > 1 {
			caption = fmt.Sprintf("%s (%d/%d)", img.Alt, n, total)
		}
		var result gateway.SendResult
		var err error
		if strings.HasPrefix(img.URL, "file://") {
			path, uerr := url.PathUnescape(img.URL[7:])
			if uerr != nil {
				path = pyurl.UnquoteLatin1(img.URL[7:])
			}
			result, err = a.SendImageFile(ctx, chatID, path, caption, "", metadata, nil)
		} else {
			result, err = a.SendImage(ctx, chatID, img.URL, caption, "", metadata)
		}
		if err != nil {
			return gateway.SendResult{}, err
		}
		if !result.Success {
			slog.Warn(fmt.Sprintf("Matrix: failed to send image %d/%d: %v", n, total, result.Error))
		}
		delivered = delivered || result.Success
	}
	if delivered {
		return gateway.SendResult{Success: true}, nil
	}
	return gateway.SendResult{Success: false, Error: "all images failed to send"}, nil
}

// Uploads a local file as an m.file attachment
func (a *MatrixAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, filePath, "m.file", caption, replyTo, fileName, metadata, false)
}

// Uploads audio as an MSC3245 voice bubble transcoded to Ogg Opus, or as plain m.audio when is_voice is false
func (a *MatrixAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if v, ok := extra["is_voice"]; ok && v == false {
		return a.sendLocalFile(ctx, chatID, audioPath, "m.audio", caption, replyTo, "", metadata, false)
	}
	convertedPath := ""
	lower := strings.ToLower(audioPath)
	if !strings.HasSuffix(lower, ".ogg") && !strings.HasSuffix(lower, ".oga") && !strings.HasSuffix(lower, ".opus") {
		if out, err := gateway.TranscodeToOggOpus(ctx, audioPath, "48k", 30*time.Second, ""); err == nil {
			convertedPath = out
		}
	}
	if convertedPath != "" {
		defer os.Remove(convertedPath)
		withOgg, err := pypath.WithSuffix(audioPath, ".ogg")
		if err != nil {
			return gateway.SendResult{}, err
		}
		return a.sendLocalFile(ctx, chatID, convertedPath, "m.audio", caption, replyTo, pypath.PathName(withOgg), metadata, true)
	}
	return a.sendLocalFile(ctx, chatID, audioPath, "m.audio", caption, replyTo, "", metadata, true)
}

// Uploads a local video natively
func (a *MatrixAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendLocalFile(ctx, chatID, videoPath, "m.video", caption, replyTo, "", metadata, false)
}

// Uploads bytes, encrypting them for E2EE rooms, and sends the media event with voice metadata when flagged
func (a *MatrixAdapter) uploadAndSend(ctx context.Context, roomID string, data []byte, filename, contentType, msgtype, caption,
	replyTo string, metadata map[string]any, isVoice bool, voiceMetadata map[string]any) gateway.SendResult {
	if int64(len(data)) > a.maxMediaBytes {
		return a.mediaTooLarge(int64(len(data)))
	}
	client := a.currentClient()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "'NoneType' object has no attribute 'upload_media'"}
	}
	uploadData := data
	var encryptedFile *attachment.EncryptedFile
	if a.roomNeedsEncryptedUpload(ctx, roomID) {
		encryptedFile = attachment.NewEncryptedFile()
		uploadData = encryptedFile.Encrypt(data)
	}
	resp, err := client.UploadMedia(ctx, mautrix.ReqUploadMedia{ContentBytes: uploadData, ContentType: contentType,
		FileName: filename, ContentLength: int64(len(uploadData))})
	if err != nil {
		slog.Error(fmt.Sprintf("Matrix: upload failed: %v", err))
		return gateway.SendResult{Success: false, Error: err.Error()}
	}
	mxcURL := resp.ContentURI.String()
	body := caption
	if body == "" {
		body = filename
	}
	info := map[string]any{"mimetype": contentType, "size": len(data)}
	msgContent := map[string]any{"msgtype": msgtype, "body": body, "info": info}
	if encryptedFile != nil {
		serialized, err := serializeEncryptedFile(encryptedFile)
		if err != nil {
			slog.Error(fmt.Sprintf("Matrix: attachment encryption failed: %v", err))
			return gateway.SendResult{Success: false, Error: err.Error()}
		}
		serialized["url"] = mxcURL
		msgContent["file"] = serialized
	} else {
		msgContent["url"] = mxcURL
	}
	if isVoice {
		msgContent["org.matrix.msc3245.voice"] = map[string]any{}
		audioMetadata := map[string]any{}
		for _, k := range []string{"duration", "waveform"} {
			if v, ok := voiceMetadata[k]; ok && v != nil {
				audioMetadata[k] = v
			}
		}
		if d, ok := audioMetadata["duration"]; ok {
			info["duration"] = d
		}
		if len(audioMetadata) > 0 {
			msgContent["org.matrix.msc1767.audio"] = audioMetadata
		}
	}
	applyRelationMetadata(msgContent, replyTo, metadata)
	return a.sendContentEvent(ctx, roomID, msgContent)
}

// JSON object form of encrypted file metadata
func serializeEncryptedFile(ef *attachment.EncryptedFile) (map[string]any, error) {
	raw, err := json.Marshal(ef)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Whether E2EE is on, the olm machine loaded and the state store marks the room encrypted
func (a *MatrixAdapter) roomNeedsEncryptedUpload(ctx context.Context, roomID string) bool {
	if !(a.encryptionOn() && a.cryptoMachine() != nil) {
		return false
	}
	a.mu.Lock()
	store := a.stateStore
	client := a.client
	a.mu.Unlock()
	if client == nil || store == nil {
		return false
	}
	encrypted, err := store.IsEncrypted(ctx, id.RoomID(roomID))
	return err == nil && encrypted
}

// Failure result for media over the configured cap
func (a *MatrixAdapter) mediaTooLarge(size int64) gateway.SendResult {
	return gateway.SendResult{Success: false, Error: fmt.Sprintf("Media file exceeds Matrix limit (%d > %d bytes)", size, a.maxMediaBytes)}
}

// Sends a prebuilt m.room.message payload as a result
func (a *MatrixAdapter) sendContentEvent(ctx context.Context, roomID string, content map[string]any) gateway.SendResult {
	client := a.currentClient()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "'NoneType' object has no attribute 'send_message_event'"}
	}
	resp, err := client.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, content)
	if err != nil {
		return gateway.SendResult{Success: false, Error: err.Error()}
	}
	return gateway.SendResult{Success: true, MessageID: string(resp.EventID)}
}

// Uploads a host file, warning in chat without the path when it is missing
func (a *MatrixAdapter) sendLocalFile(ctx context.Context, roomID, filePath, msgtype, caption, replyTo, fileName string,
	metadata map[string]any, isVoice bool) (gateway.SendResult, error) {
	p := pypath.Expanduser(filePath)
	st, err := os.Stat(p)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] upload fallback: media file not found for %s", a.Name(), filePath))
		text := i18n.T(ctx, "platform.shared.media.attachment_failed", "", nil)
		return a.EmitMediaWarning(ctx, roomID, text, caption, replyTo, metadata, nil)
	}
	fileSize := st.Size()
	if fileSize > a.maxMediaBytes {
		return a.mediaTooLarge(fileSize), nil
	}
	fname := fileName
	if fname == "" {
		fname = pypath.PathName(p)
	}
	var voiceMetadata map[string]any
	if isVoice {
		voiceMetadata = matrixVoiceMetadataForFile(p)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return gateway.SendResult{}, err
	}
	contentType, _, _ := pymimetypes.GuessType(fname, true)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	return a.uploadAndSend(ctx, roomID, data, fname, contentType, msgtype, caption, replyTo, metadata, isVoice, voiceMetadata), nil
}

// Exec approval header with the warning sign in bold markdown
func (a *MatrixAdapter) EAHeader(ctx context.Context) string {
	return "⚠️ **" + i18n.T(ctx, "gateway.exec_approval.header", "", nil) + "**\n"
}

// Sends the text, registers its prompt under the resulting event and seeds the bot reaction controls
func (a *MatrixAdapter) sendReactionPrompt(ctx context.Context, chatID, text string, metadata map[string]any,
	register func(messageID string, requester *string, expiresAt float64) *orderedStrings, emojis []string, label string) (gateway.SendResult, error) {
	result, err := a.Send(ctx, chatID, text, "", metadata)
	if err != nil || !result.Success || !pyTruthy(result.MessageID) {
		return result, err
	}
	messageID := strOf(result.MessageID)
	var requester *string
	if v := metadata["requester_user_id"]; pyTruthy(v) {
		if s := strOf(v); s != "" {
			requester = &s
		}
	}
	botReactions := register(messageID, requester, monotonic()+float64(max(a.approvalTimeoutSeconds, 0)))
	for _, emoji := range emojis {
		if reactionEventID := a.sendReaction(ctx, chatID, messageID, emoji); reactionEventID != "" {
			a.mu.Lock()
			botReactions.set(emoji, reactionEventID)
			a.mu.Unlock()
		}
	}
	return result, nil
}

var (
	eaReactions    = map[string]string{"once": "✅", "session": "🌀", "always": "♾️", "deny": "❌"}
	eaLegendKeys   = map[string]string{"once": "platform.matrix.approval.legend_once", "session": "platform.matrix.approval.legend_session", "always": "platform.matrix.approval.legend_always", "deny": "platform.matrix.approval.legend_deny"}
	eaTypedHintKey = map[string]string{"once": "platform.matrix.approval.typed_hint_once", "session": "platform.matrix.approval.typed_hint_session", "always": "platform.matrix.approval.typed_hint_always"}
)

// Reaction driven exec approval prompt with one seeded reaction per offered choice
func (a *MatrixAdapter) SendExecApprovalPrompt(ctx context.Context, prompt *gateway.ExecApprovalPrompt) (gateway.SendResult, error) {
	if a.currentClient() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	choices := prompt.Choices()
	tier := "once"
	if !prompt.SmartDenied {
		switch {
		case containsStr(choices, "always"):
			tier = "always"
		case containsStr(choices, "session"):
			tier = "session"
		}
	}
	legends := make([]string, 0, len(choices))
	reactions := make([]string, 0, len(choices))
	for _, c := range choices {
		legends = append(legends, i18n.T(ctx, eaLegendKeys[c], "", nil))
		reactions = append(reactions, eaReactions[c])
	}
	text := prompt.Text + "\n\n" + i18n.T(ctx, eaTypedHintKey[tier], "", nil) + "\n\n" +
		i18n.T(ctx, "platform.matrix.approval.legend_intro", "", nil) + "\n" + strings.Join(legends, "\n")
	sessionKey, chatID := prompt.SessionKey, prompt.ChatID
	register := func(messageID string, requester *string, expiresAt float64) *orderedStrings {
		a.mu.Lock()
		defer a.mu.Unlock()
		if old, ok := a.approvalPromptBySession[sessionKey]; ok && old != "" {
			delete(a.approvalPromptsByEvent, old)
		}
		a.approvalPromptBySession[sessionKey] = messageID
		p := &matrixApprovalPrompt{promptState{sessionKey: sessionKey, chatID: chatID, messageID: messageID, requesterUserID: requester,
			expiresAt: &expiresAt, botReactionEvents: newOrderedStrings()}}
		a.approvalPromptsByEvent[messageID] = p
		return p.botReactionEvents
	}
	return a.sendReactionPrompt(ctx, chatID, text, prompt.Metadata, register, reactions, "approval")
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var (
	providerLabelFn         = provider.GetProviderLabel
	providerPickerRowSuffix = provider.PickerRowSuffix
)

// Reaction based model picker over the first ten provider models
func (a *MatrixAdapter) SendModelPicker(ctx context.Context, chatID string, providers []map[string]any, currentModel, currentProvider, sessionKey string,
	onModelSelected func(ctx context.Context, chatID, modelID, providerSlug string) (string, error), metadata map[string]any) (gateway.SendResult, error) {
	if a.currentClient() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	type flatChoice struct{ modelID, slug, name string }
	var flat []flatChoice
	rowsBySlug := map[string]map[string]any{}
	var planLines []string
	for _, p := range providers {
		slug := ""
		if pyTruthy(p["slug"]) {
			slug = strOf(p["slug"])
		}
		name := slug
		if pyTruthy(p["name"]) {
			name = strOf(p["name"])
		}
		models, _ := p["models"].([]any)
		for _, modelID := range models {
			flat = append(flat, flatChoice{strOf(modelID), slug, name})
		}
		rowsBySlug[slug] = p
		if pyTruthy(p["plan_line"]) {
			planLines = append(planLines, "⚠️ "+strOf(p["plan_line"]))
		}
	}
	flat = flat[:min(len(flat), len(matrixModelPickerReactions))]
	if len(flat) == 0 {
		return a.Send(ctx, chatID, i18n.T(ctx, "platform.matrix.picker.no_models", "", nil), "", metadata)
	}
	providerLabel := providerLabelFn(ctx, currentProvider)
	unknown := i18n.T(ctx, "platform.shared.unknown", "", nil)
	or := func(v string) string {
		if v != "" {
			return v
		}
		return unknown
	}
	lines := []string{i18n.T(ctx, "platform.matrix.picker.title", "", nil),
		i18n.T(ctx, "platform.matrix.picker.current_model", "", map[string]any{"model": or(currentModel)}),
		i18n.T(ctx, "platform.matrix.picker.provider", "", map[string]any{"provider": or(providerLabel)})}
	lines = append(lines, planLines...)
	lines = append(lines, "", i18n.T(ctx, "platform.matrix.picker.react_model", "", nil))
	choices := map[string][]string{}
	var emojis []string
	for i, c := range flat {
		emoji := matrixModelPickerReactions[i]
		choices[emoji] = []string{c.modelID, c.slug}
		emojis = append(emojis, emoji)
		var row any
		if r, ok := rowsBySlug[c.slug]; ok {
			row = r
		}
		line := emoji + " `" + c.modelID + "` — " + c.name
		if suffix := providerPickerRowSuffix(row, c.modelID); suffix != "" {
			line += " · " + suffix
		}
		lines = append(lines, line)
	}
	onSelected := func(ctx context.Context, chatID string, args ...string) (string, error) {
		return onModelSelected(ctx, chatID, args[0], args[1])
	}
	return a.sendPicker(ctx, chatID, lines, choices, emojis, sessionKey, onSelected, metadata, a.modelPickerPromptsByEvent, "model picker")
}

// Sends picker lines, registers the picker prompt under the event and seeds its reactions
func (a *MatrixAdapter) sendPicker(ctx context.Context, chatID string, lines []string, choices map[string][]string, emojis []string, sessionKey string,
	onSelected func(ctx context.Context, chatID string, args ...string) (string, error), metadata map[string]any,
	registry map[string]*matrixPickerPrompt, label string) (gateway.SendResult, error) {
	register := func(messageID string, requester *string, expiresAt float64) *orderedStrings {
		a.mu.Lock()
		defer a.mu.Unlock()
		p := &matrixPickerPrompt{promptState: promptState{chatID: chatID, messageID: messageID, sessionKey: sessionKey,
			requesterUserID: requester, expiresAt: &expiresAt, botReactionEvents: newOrderedStrings()}, choices: choices, onSelected: onSelected}
		registry[messageID] = p
		return p.botReactionEvents
	}
	return a.sendReactionPrompt(ctx, chatID, strings.Join(lines, "\n"), metadata, register, emojis, label)
}

// Reaction based choice picker for reasoning and fast mode selections
func (a *MatrixAdapter) SendChoicePicker(ctx context.Context, chatID, title string, choices []map[string]any, sessionKey string,
	onChoiceSelected func(ctx context.Context, chatID, value string) (string, error), metadata map[string]any) (gateway.SendResult, error) {
	if a.currentClient() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	emojiChoices := map[string][]string{}
	var emojis []string
	lines := []string{title, ""}
	for i, choice := range choices {
		if i >= len(matrixChoicePickerReactions) {
			break
		}
		emoji := matrixChoicePickerReactions[i]
		value := ""
		if pyTruthy(choice["value"]) {
			value = strOf(choice["value"])
		}
		label := value
		if pyTruthy(choice["label"]) {
			label = strOf(choice["label"])
		}
		if pyTruthy(choice["is_current"]) {
			label = i18n.T(ctx, "platform.matrix.picker.current_suffix", "", map[string]any{"label": label})
		}
		emojiChoices[emoji] = []string{value}
		emojis = append(emojis, emoji)
		lines = append(lines, emoji+" "+label)
	}
	if len(emojiChoices) == 0 {
		return gateway.SendResult{Success: false, Error: "No choices"}, nil
	}
	lines = append(lines, "", i18n.T(ctx, "platform.matrix.picker.react_choice", "", nil))
	onSelected := func(ctx context.Context, chatID string, args ...string) (string, error) {
		return onChoiceSelected(ctx, chatID, args[0])
	}
	return a.sendPicker(ctx, chatID, lines, emojiChoices, emojis, sessionKey, onSelected, metadata, a.choicePickerPromptsByEvent, "choice picker")
}
