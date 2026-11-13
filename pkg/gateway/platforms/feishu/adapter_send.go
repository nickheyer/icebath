package feishu

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	feishuImageUploadType = "message"
	feishuFileUploadType  = "stream"
)

var feishuOpusUploadExtensions = map[string]bool{".ogg": true, ".opus": true}
var feishuMediaUploadExtensions = map[string]bool{".mp4": true, ".mov": true, ".avi": true, ".m4v": true}
var feishuDocUploadTypes = map[string]string{".pdf": "pdf", ".doc": "doc", ".docx": "doc", ".xls": "xls", ".xlsx": "xls", ".ppt": "ppt", ".pptx": "ppt"}

var approvalChoiceMap = map[string]string{"approve_once": "once", "approve_session": "session", "approve_always": "always", "deny": "deny"}
var approvalLabelKeys = map[string]string{"once": "platform.feishu.approval.resolved_once", "session": "platform.feishu.approval.resolved_session",
	"always": "platform.feishu.approval.resolved_always", "deny": "platform.feishu.approval.resolved_deny"}
var eaCardActions = map[string]string{"once": "approve_once", "session": "approve_session", "always": "approve_always", "deny": "deny"}

// Typed SDK response with its code envelope, nil standing for no response
type larkResp struct {
	apiOutcome
	resp any
}

func wrapResp(resp any) *larkResp {
	o := outcomeOf(resp)
	if !o.present {
		return nil
	}
	return &larkResp{apiOutcome: o, resp: resp}
}

func responseSucceeded(r *larkResp) bool { return r != nil && r.success() }

// String field of the response data when the call succeeded, nil otherwise
func extractResponseField(r *larkResp, field string) *string {
	if !responseSucceeded(r) {
		return nil
	}
	v := reflect.Indirect(reflect.ValueOf(r.resp)).FieldByName("Data")
	if !v.IsValid() || v.IsNil() {
		return nil
	}
	f := reflect.Indirect(v).FieldByName(field)
	if !f.IsValid() {
		return nil
	}
	if s, ok := f.Interface().(*string); ok {
		return s
	}
	return nil
}

func respCode(r *larkResp) any {
	if r == nil {
		return "unknown"
	}
	return r.code
}

func respMsg(r *larkResp, def string) string {
	if r == nil {
		return def
	}
	return r.msg
}

// Failed send result carrying the override error or the response code and message
func responseErrorResult(r *larkResp, defaultMessage, overrideError string) gateway.SendResult {
	var raw any
	if r != nil {
		raw = r
	}
	if overrideError != "" {
		return gateway.SendResult{Success: false, Error: overrideError, RawResponse: raw}
	}
	return gateway.SendResult{Success: false, Error: fmt.Sprintf("[%s] %s", pyrepr.Str(respCode(r)), respMsg(r, defaultMessage)), RawResponse: raw}
}

// Send result of a response, its message id on success
func finalizeSendResult(r *larkResp, defaultMessage string) gateway.SendResult {
	if !responseSucceeded(r) {
		return responseErrorResult(r, defaultMessage, "")
	}
	var id any
	if p := extractResponseField(r, "MessageId"); p != nil {
		id = *p
	}
	return gateway.SendResult{Success: true, MessageID: id, RawResponse: r}
}

func dumpsNoASCII(v any) string {
	out, _ := pyjson.Dumps(v, pyjson.EnsureASCII(false))
	return out
}

func textPayload(text string) string {
	o := pyjson.NewObject()
	o.Set("text", text)
	return dumpsNoASCII(o)
}

// Stripped content, Feishu text being plain by default
func (a *FeishuAdapter) FormatMessage(content string) string { return pystr.Strip(content) }

// Sends chunked text or markdown posts, falling back to plain text when the API rejects a post payload
func (a *FeishuAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if a.currentClient() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	formatted := a.FormatMessage(content)
	chunks := gateway.TruncateMessage(formatted, maxMessageLength, nil)
	preferPost := reSearch(markdownHintRe, formatted)
	sendPlain := func(chunk string) (*larkResp, error) {
		return a.feishuSendWithRetry(ctx, chatID, "text", textPayload(stripMarkdownToPlainText(chunk)), replyTo, metadata)
	}
	var last *larkResp
	for _, chunk := range chunks {
		msgType, payload := a.buildOutboundPayload(chunk, preferPost)
		response, err := a.feishuSendWithRetry(ctx, chatID, msgType, payload, replyTo, metadata)
		if err != nil {
			if msgType != "post" || !reSearch(postContentInvalidRe, err.Error()) {
				slog.Error("[Feishu] Send error: " + err.Error())
				return gateway.SendResult{Success: false, Error: err.Error()}, nil
			}
			slog.Warn("[Feishu] Invalid post payload rejected by API; falling back to plain text")
			if response, err = sendPlain(chunk); err != nil {
				slog.Error("[Feishu] Send error: " + err.Error())
				return gateway.SendResult{Success: false, Error: err.Error()}, nil
			}
		}
		if msgType == "post" && !responseSucceeded(response) && reSearch(postContentInvalidRe, respMsg(response, "")) {
			slog.Warn("[Feishu] Post payload rejected by API response; falling back to plain text")
			if response, err = sendPlain(chunk); err != nil {
				slog.Error("[Feishu] Send error: " + err.Error())
				return gateway.SendResult{Success: false, Error: err.Error()}, nil
			}
		}
		last = response
	}
	return finalizeSendResult(last, "send failed"), nil
}

// Updates a sent text or post message, retrying as plain text when the post body is rejected
func (a *FeishuAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	client := a.currentClient()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	content = a.FormatMessage(content)
	update := func(msgType, payload string) (gateway.SendResult, error) {
		req := buildUpdateMessageRequest(messageID, buildUpdateMessageBody(msgType, payload))
		var resp *larkim.UpdateMessageResp
		err := a.runBlocking(ctx, func(ctx context.Context) error {
			var e error
			resp, e = client.UpdateMessage(ctx, req)
			return e
		})
		if err != nil {
			return gateway.SendResult{}, err
		}
		return finalizeSendResult(wrapResp(resp), "update failed"), nil
	}
	msgType, payload := a.buildOutboundPayload(content, false)
	result, err := update(msgType, payload)
	if err == nil && !result.Success && msgType == "post" && reSearch(postContentInvalidRe, pyrepr.Str(orEmpty(result.Error))) {
		slog.Warn("[Feishu] Invalid post update payload rejected by API; falling back to plain text")
		result, err = update("text", textPayload(stripMarkdownToPlainText(content)))
	}
	if err != nil {
		slog.Error(fmt.Sprintf("[Feishu] Failed to edit message %s: %s", messageID, err))
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	if result.Success {
		result.MessageID = messageID
	}
	return result, nil
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// Deletes a bot posted message, false when the API rejects it or the call fails
func (a *FeishuAdapter) DeleteMessage(ctx context.Context, chatID, messageID string) (bool, error) {
	client := a.currentClient()
	if client == nil || messageID == "" {
		return false, nil
	}
	req := larkim.NewDeleteMessageReqBuilder().MessageId(messageID).Build()
	var resp *larkim.DeleteMessageResp
	err := a.runBlocking(ctx, func(ctx context.Context) error {
		var e error
		resp, e = client.DeleteMessage(ctx, req)
		return e
	})
	if err != nil {
		slog.Debug(fmt.Sprintf("[Feishu] Failed to delete message %s: %s", messageID, err))
		return false, nil
	}
	r := wrapResp(resp)
	if responseSucceeded(r) {
		return true, nil
	}
	var code, msg any
	if r != nil {
		code, msg = r.code, r.msg
	}
	slog.Debug(fmt.Sprintf("[Feishu] Delete of message %s rejected: code=%s msg=%s", messageID, pyrepr.Str(code), pyrepr.Str(msg)))
	return false, nil
}

// Empty header, the card header carrying the approval title
func (a *FeishuAdapter) EAHeader(ctx context.Context) string { return "" }

// Bold localized reason label
func (a *FeishuAdapter) EAReasonLabel(ctx context.Context) string {
	return "**" + i18n.T(ctx, "gateway.exec_approval.reason_label", "", nil) + ":** "
}

// Smart deny sentence with its label part in bold
func (a *FeishuAdapter) EASmartDenyLine(ctx context.Context) string {
	line := i18n.T(ctx, "gateway.exec_approval.smart_deny_line", "", nil)
	label, rest, found := strings.Cut(line, ":")
	if found {
		return "\n\n" + "**" + label + ":**" + rest
	}
	return "\n\n" + line
}

// Approval button labels with check and cross prefixes and the shorter Feishu tier words
func (a *FeishuAdapter) EAActionLabelsFor(ctx context.Context) map[string]string {
	return map[string]string{"once": "✅ " + i18n.T(ctx, "gateway.exec_approval.action_once", "", nil),
		"session": "✅ " + i18n.T(ctx, "platform.feishu.approval.action_session", "", nil),
		"always":  "✅ " + i18n.T(ctx, "platform.feishu.approval.action_always", "", nil),
		"deny":    "❌ " + i18n.T(ctx, "gateway.exec_approval.action_deny", "", nil)}
}

// Approval button card whose values route the click back to the waiting agent
func (a *FeishuAdapter) SendExecApprovalPrompt(ctx context.Context, prompt *gateway.ExecApprovalPrompt) (gateway.SendResult, error) {
	if a.currentClient() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	a.mu.Lock()
	a.approvalCounter++
	approvalID := a.approvalCounter
	a.mu.Unlock()
	actions := make([]any, 0, len(prompt.Actions))
	for _, act := range prompt.Actions {
		style := act.Style
		if style == "" {
			style = "default"
		}
		value := pyjson.NewObject()
		value.Set("hermes_action", eaCardActions[act.Choice])
		value.Set("approval_id", int64(approvalID))
		actions = append(actions, cardButton(act.Label, style, value))
	}
	card := buildCard("⚠️ "+i18n.T(ctx, "gateway.exec_approval.header", "", nil), "orange", prompt.Text, actions)
	body := strings.ReplaceAll(prompt.Text, a.EADeadlineLine(ctx), "")
	result, err := a.sendInteractiveCard(ctx, prompt.ChatID, card, prompt.Metadata, "send_exec_approval failed", a.approvalState, approvalID,
		prompt.SessionKey, map[string]string{"body": body})
	if err != nil {
		slog.Warn("[Feishu] send_exec_approval failed: " + err.Error())
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	return result, nil
}

// Sends a button card and records where it went so a click can be validated
func (a *FeishuAdapter) sendInteractiveCard(ctx context.Context, chatID string, card *pyjson.Object, metadata map[string]any, failure string,
	states map[int]map[string]string, stateID int, sessionKey string, extra map[string]string) (gateway.SendResult, error) {
	response, err := a.feishuSendWithRetry(ctx, chatID, "interactive", dumpsNoASCII(card), "", metadata)
	if err != nil {
		return gateway.SendResult{}, err
	}
	result := finalizeSendResult(response, failure)
	if result.Success {
		msgID, _ := result.MessageID.(string)
		state := map[string]string{"session_key": sessionKey, "message_id": msgID, "chat_id": chatID}
		for k, v := range extra {
			state[k] = v
		}
		a.mu.Lock()
		states[stateID] = state
		a.mu.Unlock()
	}
	return result, nil
}

// Update prompt card with yes and no buttons and the default answer hint
func buildUpdatePromptCard(ctx context.Context, prompt, def string, promptID int) *pyjson.Object {
	hint := ""
	if def != "" {
		hint = i18n.T(ctx, "platform.feishu.update_prompt.default_hint", "", map[string]any{"default": def})
	}
	btn := func(label, answer, btnType string) *pyjson.Object {
		value := pyjson.NewObject()
		value.Set("hermes_update_prompt_action", answer)
		value.Set("update_prompt_id", int64(promptID))
		return cardButton(label, btnType, value)
	}
	actions := []any{btn(i18n.T(ctx, "platform.feishu.update_prompt.answer_yes", "", nil), "y", "primary"),
		btn(i18n.T(ctx, "platform.feishu.update_prompt.answer_no", "", nil), "n", "danger")}
	return buildCard(i18n.T(ctx, "platform.feishu.update_prompt.title", "", nil), "orange", prompt+hint, actions)
}

// Sends an interactive update prompt with yes and no buttons
func (a *FeishuAdapter) SendUpdatePrompt(ctx context.Context, chatID, prompt, def, sessionKey string, metadata map[string]any) (gateway.SendResult, error) {
	if a.currentClient() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	a.mu.Lock()
	a.updatePromptCounter++
	promptID := a.updatePromptCounter
	a.mu.Unlock()
	card := buildUpdatePromptCard(ctx, prompt, def, promptID)
	result, err := a.sendInteractiveCard(ctx, chatID, card, metadata, "send_update_prompt failed", a.updatePromptState, promptID, sessionKey, nil)
	if err != nil {
		slog.Warn("[Feishu] send_update_prompt failed: " + err.Error())
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	return result, nil
}

// Card shown in place of the approval buttons once resolved, the request body kept above the decision
func buildResolvedApprovalCard(ctx context.Context, choice, userName string, body *string) *pyjson.Object {
	icon, template := "✅", "green"
	if choice == "deny" {
		icon, template = "❌", "red"
	}
	key, ok := approvalLabelKeys[choice]
	if !ok {
		key = "platform.feishu.approval.resolved_fallback"
	}
	label := i18n.T(ctx, key, "", nil)
	decided := i18n.T(ctx, "platform.feishu.approval.resolved_by_user", "", map[string]any{"icon": icon, "label": label, "user": userName})
	markdown := decided
	if body != nil && *body != "" {
		markdown = *body + "\n\n" + decided
	}
	return buildCard(icon+" "+label, template, markdown, nil)
}

// Card shown in place of the update prompt buttons once answered
func buildResolvedUpdatePromptCard(ctx context.Context, answer, userName string) *pyjson.Object {
	yes := answer == "y"
	icon, word, template := "❌", "platform.feishu.update_prompt.word_no", "red"
	if yes {
		icon, word, template = "✅", "platform.feishu.update_prompt.word_yes", "green"
	}
	title := i18n.T(ctx, "platform.feishu.update_prompt.answered_title", "", map[string]any{"icon": icon, "answer": i18n.T(ctx, word, "", nil)})
	return buildCard(title, template, i18n.T(ctx, "platform.feishu.update_prompt.answered_by", "", map[string]any{"user": userName}), nil)
}

// Writes the update answer for the detached update process through a temp file rename
func writeUpdatePromptResponse(ctx context.Context, answer string) error {
	responsePath := filepath.Join(home.Dir(ctx), ".update_response")
	tmpPath := responsePath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(answer), 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, responsePath)
}

// Native voice message, non Opus audio transcoded first, plain audio files sent as attachments
func (a *FeishuAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if isVoice, ok := extra["is_voice"].(bool); ok && !isVoice {
		return a.SendDocument(ctx, chatID, audioPath, caption, "", replyTo, metadata, nil)
	}
	transcoded := ""
	ext := strings.ToLower(pypath.Suffix(audioPath))
	if !feishuOpusUploadExtensions[ext] {
		if out, err := gateway.TranscodeToOggOpus(ctx, audioPath, "32k", 60*time.Second, ""); err == nil && out != "" {
			transcoded = out
			audioPath = out
		}
	}
	if transcoded != "" {
		defer os.Remove(transcoded)
	}
	return a.sendUploadedFileMessage(ctx, chatID, audioPath, replyTo, metadata, caption, "", "audio")
}

// Sends a document or file attachment
func (a *FeishuAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendUploadedFileMessage(ctx, chatID, filePath, replyTo, metadata, caption, fileName, "file")
}

// Sends a video file as a media message
func (a *FeishuAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendUploadedFileMessage(ctx, chatID, videoPath, replyTo, metadata, caption, "", "media")
}

// Uploads a local image and sends its key, captioned images going out as a post
func (a *FeishuAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	client := a.currentClient()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if _, err := os.Stat(imagePath); err != nil {
		return gateway.SendResult{Success: false, Error: "Image file not found: " + imagePath}, nil
	}
	fail := func(err error) (gateway.SendResult, error) {
		slog.Error(fmt.Sprintf("[Feishu] Failed to send image %s: %s", imagePath, err))
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	data, err := os.ReadFile(imagePath)
	if err != nil {
		return fail(err)
	}
	req := larkim.NewCreateImageReqBuilder().Body(larkim.NewCreateImageReqBodyBuilder().ImageType(feishuImageUploadType).
		Image(bytes.NewReader(data)).Build()).Build()
	var upload *larkim.CreateImageResp
	if err := a.runBlocking(ctx, func(ctx context.Context) error {
		var e error
		upload, e = client.CreateImage(ctx, req)
		return e
	}); err != nil {
		return fail(err)
	}
	uploadResp := wrapResp(upload)
	imageKey := extractResponseField(uploadResp, "ImageKey")
	if imageKey == nil || *imageKey == "" {
		return responseErrorResult(uploadResp, "image upload failed", "Feishu image upload missing image_key"), nil
	}
	keyPayload := pyjson.NewObject()
	keyPayload.Set("image_key", *imageKey)
	tag := pyjson.NewObject()
	tag.Set("tag", "img")
	tag.Set("image_key", *imageKey)
	msg, err := a.sendUploadedKey(ctx, chatID, replyTo, metadata, caption, "image", keyPayload, tag)
	if err != nil {
		return fail(err)
	}
	return finalizeSendResult(msg, "image send failed"), nil
}

// No typing indicator exists in the Feishu bot API
func (a *FeishuAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return nil
}

// Downloads a remote image and sends it natively, the base link send standing in when the download fails
func (a *FeishuAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	imagePath, err := a.downloadRemoteImage(ctx, imageURL)
	if err != nil {
		slog.Error(fmt.Sprintf("[Feishu] Failed to download image %s: %s", imageURL, err))
		return a.BasePlatformAdapter.SendImage(ctx, chatID, imageURL, caption, replyTo, metadata)
	}
	return a.SendImageFile(ctx, chatID, imagePath, caption, replyTo, metadata, nil)
}

// Sends an animation as a downloadable file with the downgrade notice, Feishu having no GIF bubble
func (a *FeishuAdapter) SendAnimation(ctx context.Context, chatID, animationURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	filePath, fileName, err := a.downloadRemoteDocument(ctx, animationURL, ".gif", "animation.gif")
	if err != nil {
		slog.Error(fmt.Sprintf("[Feishu] Failed to download animation %s: %s", animationURL, err))
		return a.BasePlatformAdapter.SendAnimation(ctx, chatID, animationURL, caption, replyTo, metadata)
	}
	visible := i18n.T(ctx, "platform.feishu.media.gif_downgraded", "", nil)
	var hidden *string
	if caption != "" {
		visible = i18n.T(ctx, "platform.feishu.media.gif_downgraded_caption", "", map[string]any{"caption": caption})
		hidden = &caption
	}
	degraded := a.WarningText(ctx, visible, hidden, "", "", nil)
	degradedCaption := ""
	if degraded != nil {
		degradedCaption = *degraded
	}
	return a.SendDocument(ctx, chatID, filePath, degradedCaption, fileName, replyTo, metadata, nil)
}

// Chat name and dm, group or forum type from the API, cached per chat, the bare id when unavailable
func (a *FeishuAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	fallback := map[string]any{"chat_id": chatID, "name": chatID, "type": "dm"}
	client := a.currentClient()
	if client == nil {
		return fallback, nil
	}
	a.mu.Lock()
	cached, ok := a.chatInfoCache[chatID]
	a.mu.Unlock()
	if ok {
		return copyInfo(cached), nil
	}
	req := larkim.NewGetChatReqBuilder().ChatId(chatID).Build()
	var resp *larkim.GetChatResp
	if err := a.runBlocking(ctx, func(ctx context.Context) error {
		var e error
		resp, e = client.GetChat(ctx, req)
		return e
	}); err != nil {
		slog.Warn(fmt.Sprintf("[Feishu] Failed to get chat info for %s", chatID))
		return fallback, nil
	}
	r := wrapResp(resp)
	if !responseSucceeded(r) {
		slog.Warn(fmt.Sprintf("[Feishu] Failed to get chat info for %s: [%s] %s", chatID, pyrepr.Str(respCode(r)), respMsg(r, "chat lookup failed")))
		return fallback, nil
	}
	var rawType, name string
	if resp.Data != nil {
		rawType = pystr.Lower(pystr.Strip(sdkStr(resp.Data.ChatType)))
		name = sdkStr(resp.Data.Name)
	}
	if name == "" {
		name = chatID
	}
	var rawTypeVal any
	if rawType != "" {
		rawTypeVal = rawType
	}
	info := map[string]any{"chat_id": chatID, "name": name, "type": mapChatType(rawType), "raw_type": rawTypeVal}
	a.mu.Lock()
	a.chatInfoCache[chatID] = info
	a.mu.Unlock()
	return copyInfo(info), nil
}

func copyInfo(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Post with markdown rows when asked or when the text looks like markdown, plain text otherwise
func (a *FeishuAdapter) buildOutboundPayload(content string, preferPost bool) (string, string) {
	if preferPost || reSearch(markdownHintRe, content) {
		return "post", buildMarkdownPostPayload(content)
	}
	return "text", textPayload(content)
}

// OGG Opus duration in milliseconds from the last granule position at 48 kHz, zero for anything else
func getAudioDurationMs(filePath string) int {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return 0
	}
	pos, last := 0, int64(0)
	for pos < len(data)-27 {
		idx := bytes.Index(data[pos:], []byte("OggS"))
		if idx < 0 {
			break
		}
		pos += idx
		if pos+27 > len(data) {
			break
		}
		granule := int64(binary.LittleEndian.Uint64(data[pos+6 : pos+14]))
		numSegments := int(data[pos+26])
		if granule > 0 {
			last = granule
		}
		if pos+27+numSegments > len(data) {
			break
		}
		sum := 0
		for _, b := range data[pos+27 : pos+27+numSegments] {
			sum += int(b)
		}
		pos += numSegments + sum
	}
	if last > 0 {
		return int(float64(last) / 48000 * 1000)
	}
	return 0
}

// Uploads a local file and sends its key, retrying thread audio as a reply and then as a plain chat send
func (a *FeishuAdapter) sendUploadedFileMessage(ctx context.Context, chatID, filePath, replyTo string, metadata map[string]any,
	caption, fileName, outboundType string) (gateway.SendResult, error) {
	client := a.currentClient()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if _, err := os.Stat(filePath); err != nil {
		return gateway.SendResult{Success: false, Error: "File not found: " + filePath}, nil
	}
	displayName := fileName
	if displayName == "" {
		displayName = filepath.Base(filePath)
	}
	uploadType, messageType := resolveOutboundFileRouting(displayName, outboundType)
	fail := func(err error) (gateway.SendResult, error) {
		slog.Error(fmt.Sprintf("[Feishu] Failed to send file %s: %s", filePath, err))
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	duration := 0
	if uploadType == "opus" {
		duration = getAudioDurationMs(filePath)
	}
	fh, err := os.Open(filePath)
	if err != nil {
		return fail(err)
	}
	bodyBuilder := larkim.NewCreateFileReqBodyBuilder().FileType(uploadType).FileName(displayName).File(fh)
	if duration > 0 {
		bodyBuilder = bodyBuilder.Duration(duration)
	}
	req := larkim.NewCreateFileReqBuilder().Body(bodyBuilder.Build()).Build()
	var upload *larkim.CreateFileResp
	err = a.runBlocking(ctx, func(ctx context.Context) error {
		var e error
		upload, e = client.CreateFile(ctx, req)
		return e
	})
	fh.Close()
	if err != nil {
		return fail(err)
	}
	uploadResp := wrapResp(upload)
	fileKey := extractResponseField(uploadResp, "FileKey")
	if fileKey == nil || *fileKey == "" {
		return responseErrorResult(uploadResp, "file upload failed", "Feishu file upload missing file_key"), nil
	}
	keyPayload := pyjson.NewObject()
	keyPayload.Set("file_key", *fileKey)
	tag := pyjson.NewObject()
	tag.Set("tag", "media")
	tag.Set("file_key", *fileKey)
	tag.Set("file_name", displayName)
	msg, err := a.sendUploadedKey(ctx, chatID, replyTo, metadata, caption, messageType, keyPayload, tag)
	if err != nil {
		return fail(err)
	}
	threadID := metadata["thread_id"]
	if caption == "" && !responseSucceeded(msg) && msg != nil && msg.code == 99992402 && messageType == "audio" && pyrepr.Truthy(threadID) {
		payload := dumpsNoASCII(keyPayload)
		threadMsgID := ""
		if v := metadata["reply_to_message_id"]; pyrepr.Truthy(v) {
			threadMsgID = pyrepr.Str(v)
		} else if id := a.fetchLastMessageInThread(ctx, pyrepr.Str(threadID)); id != nil {
			threadMsgID = *id
		}
		if threadMsgID != "" {
			slog.Info("[Feishu] Audio: retrying via reply API in thread")
			if msg, err = a.feishuSendWithRetry(ctx, chatID, "audio", payload, threadMsgID, metadata); err != nil {
				return fail(err)
			}
		}
		if !responseSucceeded(msg) {
			slog.Warn("[Feishu] Audio send failed in thread, retrying with chat_id")
			if msg, err = a.feishuSendWithRetry(ctx, chatID, "audio", payload, "", nil); err != nil {
				return fail(err)
			}
		}
	}
	return finalizeSendResult(msg, "file send failed"), nil
}

// Sends an uploaded key as a captioned post or as a bare key message
func (a *FeishuAdapter) sendUploadedKey(ctx context.Context, chatID, replyTo string, metadata map[string]any, caption, keyMsgType string,
	keyPayload, mediaTag *pyjson.Object) (*larkResp, error) {
	msgType, payload := keyMsgType, dumpsNoASCII(keyPayload)
	if caption != "" {
		msgType, payload = "post", buildMediaPostPayload(caption, mediaTag)
	}
	return a.feishuSendWithRetry(ctx, chatID, msgType, payload, replyTo, metadata)
}

// Last message id of a thread for reply based routing, nil when unavailable
func (a *FeishuAdapter) fetchLastMessageInThread(ctx context.Context, threadID string) *string {
	client := a.currentClient()
	if client == nil || threadID == "" {
		return nil
	}
	req := larkim.NewListMessageReqBuilder().ContainerIdType("thread").ContainerId(threadID).PageSize(1).Build()
	var resp *larkim.ListMessageResp
	if err := a.runBlocking(ctx, func(ctx context.Context) error {
		var e error
		resp, e = client.ListMessage(ctx, req)
		return e
	}); err != nil {
		slog.Debug(fmt.Sprintf("[Feishu] Failed to fetch last message in thread %s: %s", threadID, err))
		return nil
	}
	if responseSucceeded(wrapResp(resp)) && resp.Data != nil && len(resp.Data.Items) > 0 && resp.Data.Items[0] != nil {
		return resp.Data.Items[0].MessageId
	}
	return nil
}

// Reply to the target or thread anchor, otherwise create in the thread, user or chat the id names
func (a *FeishuAdapter) sendRawMessage(ctx context.Context, chatID, msgType, payload, replyTo string, metadata map[string]any) (*larkResp, error) {
	client := a.currentClient()
	if client == nil {
		return nil, errors.New("'NoneType' object has no attribute 'im'")
	}
	threadID := metadata["thread_id"]
	effective := replyTo
	if effective == "" && pyrepr.Truthy(threadID) {
		if v := metadata["reply_to_message_id"]; pyrepr.Truthy(v) {
			effective = pyrepr.Str(v)
		}
	}
	if effective != "" {
		body := larkim.NewReplyMessageReqBodyBuilder().Content(payload).MsgType(msgType).ReplyInThread(pyrepr.Truthy(threadID)).
			Uuid(uuid.NewString()).Build()
		req := larkim.NewReplyMessageReqBuilder().MessageId(effective).Body(body).Build()
		var resp *larkim.ReplyMessageResp
		err := a.runBlocking(ctx, func(ctx context.Context) error {
			var e error
			resp, e = client.ReplyMessage(ctx, req)
			return e
		})
		return wrapResp(resp), err
	}
	var receiveID, receiveIDType string
	switch {
	case pyrepr.Truthy(threadID):
		receiveID, receiveIDType = pyrepr.Str(threadID), "thread_id"
	case strings.HasPrefix(chatID, "feishu_user_id:"):
		receiveID, receiveIDType = strings.SplitN(chatID, ":", 2)[1], "user_id"
	case strings.HasPrefix(chatID, "ou_"):
		receiveID, receiveIDType = chatID, "open_id"
	default:
		receiveID, receiveIDType = chatID, "chat_id"
	}
	body := larkim.NewCreateMessageReqBodyBuilder().ReceiveId(receiveID).MsgType(msgType).Content(payload).Uuid(uuid.NewString()).Build()
	req := larkim.NewCreateMessageReqBuilder().ReceiveIdType(receiveIDType).Body(body).Build()
	var resp *larkim.CreateMessageResp
	err := a.runBlocking(ctx, func(ctx context.Context) error {
		var e error
		resp, e = client.CreateMessage(ctx, req)
		return e
	})
	return wrapResp(resp), err
}

// Sends with three attempts, falling back from a withdrawn reply target to a new chat message outside threads
func (a *FeishuAdapter) feishuSendWithRetry(ctx context.Context, chatID, msgType, payload, replyTo string, metadata map[string]any) (*larkResp, error) {
	var lastErr error
	active := replyTo
	for attempt := 0; attempt < feishuSendAttempts; attempt++ {
		response, err := a.sendRawMessage(ctx, chatID, msgType, payload, active, metadata)
		if err == nil && active != "" && !responseSucceeded(response) && response != nil && feishuReplyFallbackCodes[response.code] {
			if threadID := metadata["thread_id"]; pyrepr.Truthy(threadID) {
				slog.Warn(fmt.Sprintf("[Feishu] Reply to %s failed in thread %s (code %d — message withdrawn/missing); "+
					"skipping top-level fallback to avoid creating a new topic", active, pyrepr.Str(threadID), response.code))
				return response, nil
			}
			slog.Warn(fmt.Sprintf("[Feishu] Reply to %s failed (code %d — message withdrawn/missing); falling back to new message in chat %s",
				active, response.code, chatID))
			active = ""
			response, err = a.sendRawMessage(ctx, chatID, msgType, payload, "", metadata)
		}
		if err == nil {
			return response, nil
		}
		lastErr = err
		if msgType == "post" && reSearch(postContentInvalidRe, err.Error()) {
			return nil, err
		}
		if attempt >= feishuSendAttempts-1 {
			return nil, err
		}
		wait := 1 << attempt
		slog.Warn(fmt.Sprintf("[Feishu] Send attempt %d/%d failed for chat %s; retrying in %ds: %s", attempt+1, feishuSendAttempts, chatID, wait, err))
		if !sleepCtx(ctx, time.Duration(wait)*time.Second) {
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("Feishu send failed")
	}
	return nil, lastErr
}

func buildUpdateMessageBody(msgType, content string) *larkim.UpdateMessageReqBody {
	return larkim.NewUpdateMessageReqBodyBuilder().MsgType(msgType).Content(content).Build()
}

func buildUpdateMessageRequest(messageID string, body *larkim.UpdateMessageReqBody) *larkim.UpdateMessageReq {
	return larkim.NewUpdateMessageReqBuilder().MessageId(messageID).Body(body).Build()
}

// Caption post with the media tag appended as its own row
func buildMediaPostPayload(caption string, mediaTag *pyjson.Object) string {
	rows := append(buildMarkdownPostRows(caption), []any{mediaTag})
	locale := pyjson.NewObject()
	locale.Set("content", rows)
	root := pyjson.NewObject()
	root.Set("zh_cn", locale)
	return dumpsNoASCII(root)
}

// Upload file type and message type of a file, Opus becoming audio only on a voice send
func resolveOutboundFileRouting(filePath, requestedType string) (string, string) {
	ext := strings.ToLower(pypath.Suffix(filePath))
	if feishuOpusUploadExtensions[ext] && requestedType == "audio" {
		return "opus", "audio"
	}
	if feishuMediaUploadExtensions[ext] {
		return "mp4", "media"
	}
	if t, ok := feishuDocUploadTypes[ext]; ok {
		return t, "file"
	}
	return feishuFileUploadType, "file"
}

func (a *FeishuAdapter) downloadRemoteImage(ctx context.Context, imageURL string) (string, error) {
	ext := guessRemoteExtension(imageURL, ".jpg")
	return gateway.CacheImageFromURL(ctx, imageURL, ext, 2)
}

// Downloads a remote file behind the SSRF guard and caches it as a document, returning the path and name
func (a *FeishuAdapter) downloadRemoteDocument(ctx context.Context, fileURL, defaultExt, preferredName string) (string, string, error) {
	env := gateway.URLSafetyEnv(ctx)
	if !tools.IsSafeURL(env, fileURL) {
		return "", "", fmt.Errorf("Blocked unsafe URL (SSRF protection): %s", pystr.Slice(fileURL, 0, 80))
	}
	client := tools.NewSSRFSafeClient(env, 30*time.Second, true)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; IcebathAgent/1.0)")
	req.Header.Set("Accept", "*/*")
	resp, err := pyhttpx.Do(client, req, 30*time.Second)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", "", errors.New(pyhttpx.RaiseForStatusMessage(resp.StatusCode, resp.URL, resp.Header.Get("Location")))
	}
	filename := deriveRemoteFilename(fileURL, resp.Header.Get("Content-Type"), preferredName, defaultExt)
	path, err := gateway.CacheDocumentFromBytesAsync(ctx, resp.Content, filename)
	return path, filename, err
}

// Known media or document extension of a URL path, def otherwise
func guessRemoteExtension(rawURL, def string) string {
	ext := strings.ToLower(pypath.Suffix(strings.SplitN(rawURL, "?", 2)[0]))
	if imageExtensions[ext] || audioExtensions[ext] || videoExtensions[ext] {
		return ext
	}
	if _, ok := mediacache.SupportedDocumentTypes[ext]; ok {
		return ext
	}
	return def
}

// File name of a URL path, given an extension from the content type or the default when it has none
func deriveRemoteFilename(fileURL, contentType, defaultName, defaultExt string) string {
	candidate := pypath.Name(strings.SplitN(fileURL, "?", 2)[0])
	if candidate == "" {
		candidate = defaultName
	}
	if pypath.Suffix(candidate) == "" {
		guessed := pymimetypes.GuessExtension(pystr.Lower(pystr.Strip(strings.SplitN(contentType, ";", 2)[0])), true)
		if guessed == "" {
			guessed = defaultExt
		}
		candidate += guessed
	}
	return candidate
}
