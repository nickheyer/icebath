package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
)

var numberedLineRE = regexp.MustCompile(`^\d+\.\s`)

// Markdown with a blank line before numbered lists and dedented code fences, quirks of the DingTalk renderer
func normalizeMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for i, line := range lines {
		prev := ""
		if i > 0 {
			prev = strings.TrimSpace(lines[i-1])
		}
		trimmed := strings.TrimSpace(line)
		if prev != "" && numberedLineRE.MatchString(trimmed) && !numberedLineRE.MatchString(prev) {
			out = append(out, "")
		}
		if strings.HasPrefix(trimmed, "```") {
			line = strings.TrimLeftFunc(line, unicode.IsSpace)
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// Short random id for messages the platform gives no id for
func shortID() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }

// Failed send result of an error, flow control marked retryable with its wait
func apiFailure(err error) gateway.SendResult {
	r := gateway.SendResult{Success: false, Error: err.Error()}
	var ae *apiError
	if errors.As(err, &ae) && ae.rateLimited() {
		kind := "rate_limited"
		r.Retryable, r.RetryAfter, r.ErrorKind = true, ae.retryAfter, &kind
	}
	return r
}

// Recipient of robot API messages for a chat, from its last message, the conversation book or the id itself
func (a *DingTalkAdapter) targetFor(chatID string) robotTarget {
	if msg := a.messageContext(chatID); msg != nil {
		if msg.isGroup() && msg.ConversationID != "" {
			return robotTarget{conversationID: msg.ConversationID}
		}
		if msg.SenderStaffID != "" {
			return robotTarget{userID: msg.SenderStaffID}
		}
	}
	if c, ok := a.book.lookup(chatID); ok {
		if c.Group {
			return robotTarget{conversationID: chatID}
		}
		if c.StaffID != "" {
			return robotTarget{userID: c.StaffID}
		}
	}
	return targetFromID(chatID)
}

// Sends text as AI cards when configured, else through the session webhook, else through the robot API
func (a *DingTalkAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	isFinal := replyTo != ""
	chunks := gateway.TruncateMessage(content, maxMessageLength, a.MessageLenFn())
	msg := a.messageContext(chatID)
	cards := a.cardTemplateID != "" && msg != nil && a.currentAPI() != nil
	if cards {
		a.closeStreamingSiblings(ctx, chatID)
	}
	var last gateway.SendResult
	for i, chunk := range chunks {
		finalize := i < len(chunks)-1 || isFinal
		if cards {
			if r, ok := a.createAndStreamCard(ctx, msg, chunk, finalize); ok {
				if !finalize {
					a.trackStreamingCard(chatID, fmt.Sprint(r.MessageID), chunk)
				}
				last = r
				continue
			}
			slog.Warn(fmt.Sprintf("[%s] AI Card send failed, falling back to webhook", a.Name()))
			cards = false
		}
		r, err := a.sendText(ctx, chatID, chunk, metadata)
		if err != nil {
			return gateway.SendResult{}, err
		}
		if !r.Success {
			if i > 0 {
				r.RawResponse = map[string]any{"partial_overflow": true, "delivered_chunks": i, "total_chunks": len(chunks)}
			}
			return r, nil
		}
		last = r
	}
	if isFinal {
		a.fireDoneReaction(ctx, chatID)
	}
	return last, nil
}

// Text through the session webhook of the metadata or the chat, the robot API when there is none or it is refused
func (a *DingTalkAdapter) sendText(ctx context.Context, chatID, text string, metadata map[string]any) (gateway.SendResult, error) {
	webhook, fromMeta := "", false
	if s, ok := metadata["session_webhook"].(string); ok && s != "" {
		webhook, fromMeta = s, true
	} else {
		webhook = a.validWebhook(chatID)
	}
	if webhook != "" {
		r, refused, err := a.postWebhook(ctx, webhook, text)
		if err != nil || !refused {
			return r, err
		}
		if !fromMeta {
			a.forgetWebhook(chatID, webhook)
		}
		slog.Info(fmt.Sprintf("[%s] Session webhook refused the reply (%v), sending through the robot API", a.Name(), r.Error))
	}
	return a.robotSend(ctx, chatID, []robotMessage{{key: "sampleMarkdown",
		param: map[string]any{"title": messageTitle, "text": normalizeMarkdown(text)}}})
}

// Posts markdown to a session webhook, refused true when the webhook itself was rejected
func (a *DingTalkAdapter) postWebhook(ctx context.Context, webhook, text string) (gateway.SendResult, bool, error) {
	a.mu.Lock()
	client := a.httpClient
	a.mu.Unlock()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "HTTP client not initialized"}, false, nil
	}
	payload, err := json.Marshal(map[string]any{"msgtype": "markdown",
		"markdown": map[string]string{"title": messageTitle, "text": normalizeMarkdown(text)}})
	if err != nil {
		return gateway.SendResult{}, false, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, webhook, bytes.NewReader(payload))
	if err != nil {
		return gateway.SendResult{Success: false, Error: err.Error()}, true, nil
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, false, ctx.Err()
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return gateway.SendResult{Success: false, Error: "Timeout sending message to DingTalk"}, false, nil
		}
		slog.Error(fmt.Sprintf("[%s] Send error: %v", a.Name(), err))
		return gateway.SendResult{Success: false, Error: err.Error()}, false, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	body := clip(string(raw), 200)
	if resp.StatusCode >= 300 {
		slog.Warn(fmt.Sprintf("[%s] Send failed HTTP %d: %s", a.Name(), resp.StatusCode, body))
		r := apiFailure(parseAPIError(resp, raw))
		r.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, body)
		refused := resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests
		return r, refused, nil
	}
	var out struct {
		ErrCode int64  `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if json.Unmarshal(raw, &out) == nil && out.ErrCode != 0 {
		e := &apiError{status: resp.StatusCode, code: strconv.FormatInt(out.ErrCode, 10), message: out.ErrMsg}
		if e.rateLimited() {
			wait := 60.0
			e.retryAfter = &wait
			return apiFailure(e), false, nil
		}
		return apiFailure(e), true, nil
	}
	return gateway.SendResult{Success: true, MessageID: shortID()}, false, nil
}

// One robot API message template and its parameters
type robotMessage struct {
	key   string
	param map[string]any
}

// Sends robot API messages to the chat in order, the last process query key as the message id
func (a *DingTalkAdapter) robotSend(ctx context.Context, chatID string, msgs []robotMessage) (gateway.SendResult, error) {
	api := a.currentAPI()
	if api == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	target := a.targetFor(chatID)
	var last string
	for _, m := range msgs {
		key, err := api.sendRobot(ctx, target, m.key, m.param)
		if err != nil {
			if ctx.Err() != nil {
				return gateway.SendResult{}, ctx.Err()
			}
			slog.Warn(fmt.Sprintf("[%s] Robot send to %s failed: %v", a.Name(), clip(chatID, 24), err))
			return apiFailure(err), nil
		}
		last = key
	}
	return gateway.SendResult{Success: true, MessageID: firstNonEmpty(last, shortID())}, nil
}

// Creates, delivers and streams an AI card, finalize false leaving it open for later edits
func (a *DingTalkAdapter) createAndStreamCard(ctx context.Context, msg *chatbotMessage, content string, finalize bool) (gateway.SendResult, bool) {
	api := a.currentAPI()
	if api == nil {
		return gateway.SendResult{}, false
	}
	var target robotTarget
	switch {
	case msg.isGroup():
		target = robotTarget{conversationID: msg.ConversationID}
	case msg.SenderStaffID != "":
		target = robotTarget{userID: msg.SenderStaffID}
	default:
		slog.Warn(fmt.Sprintf("[%s] AI Card skipped: missing sender_staff_id for DM", a.Name()))
		return gateway.SendResult{}, false
	}
	outTrackID := "icebath_" + shortID()
	err := api.createCard(ctx, a.cardTemplateID, outTrackID)
	if err == nil {
		err = api.deliverCard(ctx, outTrackID, target)
	}
	if err == nil {
		err = api.streamCard(ctx, outTrackID, content, finalize)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] AI Card create failed: %v", a.Name(), err))
		return gateway.SendResult{}, false
	}
	state := "created (streaming)"
	if finalize {
		state = "created+finalized"
	}
	slog.Info(fmt.Sprintf("[%s] AI Card %s: %s", a.Name(), state, outTrackID))
	return gateway.SendResult{Success: true, MessageID: outTrackID}, true
}

// Remembers an open streaming card so the next send or a final edit closes it
func (a *DingTalkAdapter) trackStreamingCard(chatID, outTrackID, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.streamingCards[chatID] == nil {
		a.streamingCards[chatID] = map[string]string{}
	}
	a.streamingCards[chatID][outTrackID] = content
}

// Forgets a closed streaming card
func (a *DingTalkAdapter) untrackStreamingCard(chatID, outTrackID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.streamingCards[chatID], outTrackID)
	if len(a.streamingCards[chatID]) == 0 {
		delete(a.streamingCards, chatID)
	}
}

// Finalizes every open streaming card of the chat with its last content
func (a *DingTalkAdapter) closeStreamingSiblings(ctx context.Context, chatID string) {
	a.mu.Lock()
	cards := a.streamingCards[chatID]
	delete(a.streamingCards, chatID)
	api := a.api
	a.mu.Unlock()
	if len(cards) == 0 || api == nil {
		return
	}
	for outTrackID, content := range cards {
		if err := api.streamCard(ctx, outTrackID, content, true); err != nil {
			slog.Debug(fmt.Sprintf("[%s] Sibling close failed for %s: %v", a.Name(), outTrackID, err))
			continue
		}
		slog.Debug(fmt.Sprintf("[%s] AI Card sibling closed: %s", a.Name(), outTrackID))
	}
}

// Streams new content into an AI card, finalize closing it and firing the done reaction
func (a *DingTalkAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	api := a.currentAPI()
	if a.cardTemplateID == "" || api == nil {
		return gateway.SendResult{Success: false, Error: "AI Cards are not configured for message editing"}, nil
	}
	if messageID == "" {
		return gateway.SendResult{Success: false, Error: "message_id required"}, nil
	}
	if err := api.streamCard(ctx, messageID, content, finalize); err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[%s] Card edit failed: %v", a.Name(), err))
		return apiFailure(err), nil
	}
	if finalize {
		a.untrackStreamingCard(chatID, messageID)
		slog.Debug(fmt.Sprintf("[%s] AI Card finalized (edit): %s", a.Name(), messageID))
		a.fireDoneReaction(ctx, chatID)
	} else {
		a.trackStreamingCard(chatID, messageID, content)
	}
	return gateway.SendResult{Success: true, MessageID: messageID}, nil
}

// Remote image rendered inline through markdown under its caption
func (a *DingTalkAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	block := "![image](" + imageURL + ")"
	if caption != "" {
		block = caption + "\n\n" + block
	}
	return a.Send(ctx, chatID, block, replyTo, metadata)
}

// Local image uploaded and sent as an image message, a file when it is no image DingTalk can show
func (a *DingTalkAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	sc, err := newScratch()
	if err != nil {
		return gateway.SendResult{}, err
	}
	defer sc.cleanup()
	upload, ok, err := prepareImage(sc, imagePath)
	if err != nil {
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	if !ok {
		return a.sendFile(ctx, sc, chatID, imagePath, filepath.Base(imagePath), caption, replyTo)
	}
	return a.sendMedia(ctx, chatID, caption, replyTo, func(ctx context.Context, api *openAPI) ([]robotMessage, error) {
		id, err := api.uploadMedia(ctx, "image", upload, filepath.Base(upload))
		if err != nil {
			return nil, err
		}
		return []robotMessage{{key: "sampleImageMsg", param: map[string]any{"photoURL": id}}}, nil
	})
}

// Local document uploaded and sent as a file message under its given name
func (a *DingTalkAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	sc, err := newScratch()
	if err != nil {
		return gateway.SendResult{}, err
	}
	defer sc.cleanup()
	return a.sendFile(ctx, sc, chatID, filePath, firstNonEmpty(fileName, filepath.Base(filePath)), caption, replyTo)
}

// Audio sent as voice messages within the voice limits, a file when it cannot become one
func (a *DingTalkAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	sc, err := newScratch()
	if err != nil {
		return gateway.SendResult{}, err
	}
	defer sc.cleanup()
	clips, ok, err := prepareVoice(ctx, sc, audioPath)
	if err != nil {
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	if !ok {
		slog.Info(fmt.Sprintf("[%s] %s is no voice DingTalk can play, sending it as a file", a.Name(), filepath.Base(audioPath)))
		return a.sendFile(ctx, sc, chatID, audioPath, filepath.Base(audioPath), caption, replyTo)
	}
	return a.sendMedia(ctx, chatID, caption, replyTo, func(ctx context.Context, api *openAPI) ([]robotMessage, error) {
		msgs := make([]robotMessage, 0, len(clips))
		for _, c := range clips {
			id, err := api.uploadMedia(ctx, "voice", c.path, filepath.Base(c.path))
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, robotMessage{key: "sampleAudio", param: map[string]any{"mediaId": id,
				"duration": strconv.FormatInt(max(1, c.durationMs), 10)}})
		}
		return msgs, nil
	})
}

// Video sent as a video message with a cover, a file when it cannot become one
func (a *DingTalkAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	sc, err := newScratch()
	if err != nil {
		return gateway.SendResult{}, err
	}
	defer sc.cleanup()
	video, ok, err := prepareVideo(ctx, sc, videoPath)
	if err != nil {
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	if !ok {
		slog.Info(fmt.Sprintf("[%s] %s is no video DingTalk can play, sending it as a file", a.Name(), filepath.Base(videoPath)))
		return a.sendFile(ctx, sc, chatID, videoPath, filepath.Base(videoPath), caption, replyTo)
	}
	return a.sendMedia(ctx, chatID, caption, replyTo, func(ctx context.Context, api *openAPI) ([]robotMessage, error) {
		vid, err := api.uploadMedia(ctx, "video", video.path, "video.mp4")
		if err != nil {
			return nil, err
		}
		pic, err := api.uploadMedia(ctx, "image", video.cover, "cover.jpg")
		if err != nil {
			return nil, err
		}
		return []robotMessage{{key: "sampleVideo", param: map[string]any{"duration": strconv.FormatInt(video.seconds, 10),
			"videoMediaId": vid, "videoType": "mp4", "picMediaId": pic}}}, nil
	})
}

// File uploaded as a plain file, zipped when DingTalk refuses its type or it only fits compressed
func (a *DingTalkAdapter) sendFile(ctx context.Context, sc *scratch, chatID, path, name, caption, replyTo string) (gateway.SendResult, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return gateway.SendResult{Success: false, Error: "File not found: " + path}, nil
	}
	return a.sendMedia(ctx, chatID, caption, replyTo, func(ctx context.Context, api *openAPI) ([]robotMessage, error) {
		return uploadFileMessage(ctx, api, sc, path, name, info.Size())
	})
}

// Robot file message of an uploaded file, zipped when refused for its type or too large uncompressed
func uploadFileMessage(ctx context.Context, api *openAPI, sc *scratch, path, name string, size int64) ([]robotMessage, error) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	zipped := false
	zipIt := func() error {
		var err error
		if path, name, err = zipWrap(sc, path, name); err != nil {
			return err
		}
		zipped, ext = true, "zip"
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		if st.Size() > maxUploadBytes {
			return fmt.Errorf("%s is larger than the 20 MB DingTalk upload limit even compressed", name)
		}
		return nil
	}
	if size > maxUploadBytes {
		if err := zipIt(); err != nil {
			return nil, err
		}
	}
	id, err := api.uploadMedia(ctx, "file", path, name)
	var ae *apiError
	if err != nil && !zipped && !uploadFileTypes[ext] && errors.As(err, &ae) {
		if zerr := zipIt(); zerr != nil {
			return nil, zerr
		}
		id, err = api.uploadMedia(ctx, "file", path, name)
	}
	if err != nil {
		return nil, err
	}
	return []robotMessage{{key: "sampleFile", param: map[string]any{"mediaId": id, "fileName": name, "fileType": ext}}}, nil
}

// Caption as text then the built robot messages, the done reaction firing for a final reply
func (a *DingTalkAdapter) sendMedia(ctx context.Context, chatID, caption, replyTo string,
	build func(ctx context.Context, api *openAPI) ([]robotMessage, error)) (gateway.SendResult, error) {
	api := a.currentAPI()
	if api == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	if caption != "" {
		r, err := a.Send(ctx, chatID, caption, "", nil)
		if err != nil || !r.Success {
			return r, err
		}
	}
	msgs, err := build(ctx, api)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[%s] Media upload failed: %v", a.Name(), err))
		return apiFailure(err), nil
	}
	r, err := a.robotSend(ctx, chatID, msgs)
	if err == nil && r.Success && replyTo != "" {
		a.fireDoneReaction(ctx, chatID)
	}
	return r, err
}
