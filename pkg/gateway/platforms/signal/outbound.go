package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
)

const (
	reactionWorking = "👀"
	reactionSuccess = "✅"
	reactionFailure = "❌"
)

// Logs an RPC failure at warning, or at debug for quiet callers like typing
func logRPCFailure(method string, err error, quiet bool) {
	level := slog.LevelWarn
	if quiet {
		level = slog.LevelDebug
	}
	var rpcErr *rpcError
	if errors.As(err, &rpcErr) {
		slog.Log(context.Background(), level, fmt.Sprintf("Signal RPC error (%s): %d %s", method, rpcErr.Code, rpcErr.Message))
		return
	}
	slog.Log(context.Background(), level, fmt.Sprintf("Signal RPC %s failed: %v", method, err))
}

// Calls a method on the live client, logging failures
func (a *SignalAdapter) call(ctx context.Context, method string, params map[string]any, id string, quiet bool) (json.RawMessage, error) {
	c := a.client()
	if c == nil {
		slog.Warn("Signal: RPC called but client not connected")
		return nil, errors.New("signal client not connected")
	}
	raw, err := c.call(ctx, method, params, id, 0)
	if err != nil && ctx.Err() == nil {
		logRPCFailure(method, err, quiet)
	}
	return raw, err
}

// Service id signal-cli prefers for a contact entry of the phone number, empty when absent
func contactServiceID(contact map[string]any, phone string) string {
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
	id := str(contact, "uuid")
	if id == "" {
		id = str(contact, "serviceId")
	}
	if profile, ok := contact["profile"].(map[string]any); ok && id == "" {
		if id = str(profile, "serviceId"); id == "" {
			id = str(profile, "uuid")
		}
	}
	if !isServiceID(id) || (str(contact, "number") != phone && str(contact, "recipient") != phone) {
		return ""
	}
	return id
}

// Recipient for a direct chat, a known service id upgrading an E.164 number through the contact list
func (a *SignalAdapter) resolveRecipient(ctx context.Context, chatID string) string {
	if strings.HasPrefix(chatID, "group:") || !looksLikeE164(chatID) {
		return chatID
	}
	known := func() (string, bool) {
		a.recipientMu.Lock()
		defer a.recipientMu.Unlock()
		if id := a.uuidByNumber[chatID]; id != "" {
			return id, true
		}
		at, missed := a.unresolved[chatID]
		return chatID, missed && time.Since(at) < unresolvedRecipientTTL
	}
	if id, ok := known(); ok {
		return id
	}
	a.resolveMu.Lock()
	defer a.resolveMu.Unlock()
	if id, ok := known(); ok {
		return id
	}
	raw, err := a.call(ctx, "listContacts", map[string]any{"account": a.account, "allRecipients": true}, "", false)
	if err != nil {
		return chatID
	}
	var contacts []map[string]any
	_ = json.Unmarshal(raw, &contacts)
	for _, contact := range contacts {
		if number, _ := contact["number"].(string); number != "" {
			if id := contactServiceID(contact, number); id != "" {
				a.rememberRecipient(number, id)
			}
		}
	}
	if id := a.cachedUUID(chatID); id != "" {
		return id
	}
	a.recipientMu.Lock()
	a.unresolved[chatID] = time.Now()
	a.recipientMu.Unlock()
	return chatID
}

// Account and routing params of a chat, direct recipients upgraded to a service id when resolve is set
func (a *SignalAdapter) target(ctx context.Context, chatID string, resolve bool) map[string]any {
	recipient := chatID
	if resolve {
		recipient = a.resolveRecipient(ctx, chatID)
	}
	return targetParams(map[string]any{"account": a.account}, chatID, recipient)
}

// Copy of params with extra keys set
func withParams(params map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(params)+len(kv)/2)
	for k, v := range params {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// Sets textStyle for one range and textStyles for several
func setStyles(params map[string]any, styles []textStyle) {
	switch len(styles) {
	case 0:
	case 1:
		params["textStyle"] = styles[0].String()
	default:
		list := make([]string, len(styles))
		for i, st := range styles {
			list[i] = st.String()
		}
		params["textStyles"] = list
	}
}

// Runs a send, tracking its timestamp on delivery, a failed result carrying the reason otherwise
func (a *SignalAdapter) rpcSend(ctx context.Context, params map[string]any, failError string) (*sendOutcome, *gateway.SendResult) {
	raw, err := a.call(ctx, "send", params, "", false)
	if err != nil {
		res := gateway.SendResult{Success: false, Error: failError + ": " + err.Error()}
		if rl := rateLimitFromError(err); rl != nil && rl.retryAfter > 0 {
			res.RetryAfter = &rl.retryAfter
		}
		return nil, &res
	}
	outcome := decodeSendOutcome(raw)
	delivered, failure, rl := outcome.verdict()
	if !delivered {
		res := gateway.SendResult{Success: false, Error: failure, RawResponse: outcome.raw}
		if rl != nil && rl.retryAfter > 0 {
			res.RetryAfter = &rl.retryAfter
		}
		return nil, &res
	}
	if failure != "" {
		slog.Warn(fmt.Sprintf("Signal: delivered with some recipients failing: %s", failure))
	}
	a.trackSent(outcome.response.Timestamp)
	return outcome, nil
}

// Markdown converted to Signal styles and sent in UTF-16 bounded chunks
func (a *SignalAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	a.stopTypingIndicator(ctx, chatID)
	if strings.TrimSpace(content) == "" {
		return gateway.SendResult{Success: true}, nil
	}
	text, styles := markdownToSignal(content)
	return a.sendChunks(ctx, chatID, splitFormatted(text, styles, maxMessageLength)), nil
}

// Sends chunks in order, a failure after delivered chunks keeping the rest for a resume
func (a *SignalAdapter) sendChunks(ctx context.Context, chatID string, chunks []formattedChunk) gateway.SendResult {
	base := a.target(ctx, chatID, true)
	var last *sendOutcome
	for i, chunk := range chunks {
		params := withParams(base, "message", chunk.text)
		setStyles(params, chunk.styles)
		slog.Info(fmt.Sprintf("[Signal] Sending response chunk %d/%d (%d chars) to %s", i+1, len(chunks), len([]rune(chunk.text)), chatID))
		outcome, failed := a.rpcSend(ctx, params, "RPC send failed")
		if failed != nil {
			if i > 0 {
				raw := map[string]any{"partial_overflow": true, "delivered_chunks": i, "total_chunks": len(chunks),
					"pending_chunks": chunks[i:]}
				if prior, ok := failed.RawResponse.(map[string]any); ok {
					raw["response"] = prior
				}
				failed.RawResponse = raw
			}
			return *failed
		}
		last = outcome
	}
	result := gateway.SendResult{Success: true}
	if last != nil {
		result.RawResponse = last.raw
	}
	return result
}

// Delivers the chunks a partly failed send left undelivered
func (a *SignalAdapter) ResumePartialSend(ctx context.Context, chatID string, result gateway.SendResult, replyTo string, metadata map[string]any) (*gateway.SendResult, error) {
	raw, ok := result.RawResponse.(map[string]any)
	if !ok {
		return nil, nil
	}
	pending, ok := raw["pending_chunks"].([]formattedChunk)
	if !ok || len(pending) == 0 {
		return nil, nil
	}
	resumed := a.sendChunks(ctx, chatID, pending)
	return &resumed, nil
}

// Typing indicator with per chat backoff, three straight failures pausing the RPC for an exponential cooldown
func (a *SignalAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.typingMu.Lock()
	state := a.typing[chatID]
	if state == nil {
		state = &typingBackoff{}
		a.typing[chatID] = state
	}
	skip, fails := time.Now().Before(state.skipUntil), state.failures
	a.typingMu.Unlock()
	if skip {
		return nil
	}
	_, err := a.call(ctx, "sendTyping", a.target(ctx, chatID, true), "typing", fails > 0)
	a.typingMu.Lock()
	defer a.typingMu.Unlock()
	if err == nil {
		delete(a.typing, chatID)
		return nil
	}
	if state = a.typing[chatID]; state == nil {
		state = &typingBackoff{}
		a.typing[chatID] = state
	}
	state.failures++
	if state.failures >= 3 {
		state.skipUntil = time.Now().Add(min(60*time.Second, (16*time.Second)<<min(state.failures-3, 2)))
	}
	return nil
}

// Sends an explicit typing stop so the indicator clears at once, then resets the chat's backoff
func (a *SignalAdapter) stopTypingIndicator(ctx context.Context, chatID string) {
	if a.client() != nil {
		_, _ = a.call(ctx, "sendTyping", withParams(a.target(ctx, chatID, true), "stop", true), "typing-stop", true)
	}
	a.typingMu.Lock()
	delete(a.typing, chatID)
	a.typingMu.Unlock()
}

// Typing stop the base typing loop calls when a turn ends
func (a *SignalAdapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.stopTypingIndicator(ctx, chatID)
	return nil
}

// Local path of an image URL, file URLs used in place and remote ones downloaded into the cache
func (a *SignalAdapter) resolveImagePath(ctx context.Context, imageURL string) (string, error) {
	var path string
	if rest, ok := strings.CutPrefix(imageURL, "file://"); ok {
		unescaped, err := url.PathUnescape(rest)
		if err != nil {
			return "", fmt.Errorf("invalid file URL: %w", err)
		}
		path = unescaped
	} else {
		cached, err := gateway.CacheImageFromURL(ctx, imageURL, ".jpg", 2)
		if err != nil {
			return "", fmt.Errorf("download failed: %w", err)
		}
		path = cached
	}
	info, err := os.Stat(path)
	if err != nil || path == "" {
		return "", errors.New("Image file not found")
	}
	if info.Size() > maxAttachmentBytes {
		return "", fmt.Errorf("Image too large (%d bytes)", info.Size())
	}
	return path, nil
}

// Image by URL sent as a native attachment
func (a *SignalAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	a.stopTypingIndicator(ctx, chatID)
	path, err := a.resolveImagePath(ctx, imageURL)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("Signal: failed to prepare image %s: %v", gateway.SafeURLForLog(imageURL, 80), err))
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	return a.sendFile(ctx, chatID, path, caption, "RPC send with attachment failed"), nil
}

// One local file sent as an attachment with an optional caption
func (a *SignalAdapter) sendFile(ctx context.Context, chatID, path, caption, failError string) gateway.SendResult {
	params := withParams(a.target(ctx, chatID, true), "message", caption, "attachments", []string{path})
	outcome, failed := a.rpcSend(ctx, params, failError)
	if failed != nil {
		return *failed
	}
	return gateway.SendResult{Success: true, RawResponse: outcome.raw}
}

// Local file of any kind sent as an attachment after existence and size checks
func (a *SignalAdapter) sendAttachment(ctx context.Context, chatID, path, label, caption string) gateway.SendResult {
	a.stopTypingIndicator(ctx, chatID)
	info, err := os.Stat(path)
	if err != nil {
		return gateway.SendResult{Success: false, Error: fmt.Sprintf("%s file not found: %s", label, path)}
	}
	if info.Size() > maxAttachmentBytes {
		return gateway.SendResult{Success: false, Error: fmt.Sprintf("%s too large (%d bytes)", label, info.Size())}
	}
	return a.sendFile(ctx, chatID, path, caption, "RPC send "+strings.ToLower(label)+" failed")
}

// Local document sent as an attachment
func (a *SignalAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendAttachment(ctx, chatID, filePath, "File", caption), nil
}

// Local image sent as an attachment
func (a *SignalAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendAttachment(ctx, chatID, imagePath, "Image", caption), nil
}

// Audio sent as an attachment, Signal having no separate voice message call
func (a *SignalAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendAttachment(ctx, chatID, audioPath, "Audio", caption), nil
}

// Local video sent as an attachment
func (a *SignalAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendAttachment(ctx, chatID, videoPath, "Video", caption), nil
}

// Images sent in attachment batches paced by the shared rate limit scheduler, success when any batch lands
func (a *SignalAdapter) SendMultipleImages(ctx context.Context, chatID string, images []gateway.ImageRef, metadata map[string]any, humanDelay float64) (gateway.SendResult, error) {
	if len(images) == 0 {
		return gateway.SendResult{Success: false, Error: "no images to send"}, nil
	}
	sched := sharedScheduler()
	slog.Info(fmt.Sprintf("Signal send_multiple_images: received %d image(s) for %.30s, scheduler state: %s", len(images), chatID, sched))
	a.stopTypingIndicator(ctx, chatID)
	var paths []string
	for _, img := range images {
		path, err := a.resolveImagePath(ctx, img.URL)
		if err != nil {
			if ctx.Err() != nil {
				return gateway.SendResult{}, ctx.Err()
			}
			slog.Warn(fmt.Sprintf("Signal: skipping image %s: %v", gateway.SafeURLForLog(img.URL, 80), err))
			continue
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		slog.Error(fmt.Sprintf("Signal: no valid images in batch of %d", len(images)))
		return gateway.SendResult{Success: false, Error: "no valid images in batch"}, nil
	}
	slog.Info(fmt.Sprintf("Signal send_multiple_images: %d/%d images valid, sending in chunks", len(paths), len(images)))
	c := a.client()
	if c == nil {
		return gateway.SendResult{Success: false, Error: "signal client not connected"}, nil
	}
	base := a.target(ctx, chatID, true)
	batches := attachmentBatches(paths)
	delivered := false
	for i, batch := range batches {
		n := len(batch)
		wait := sched.estimateWait(n)
		slog.Debug(fmt.Sprintf("Signal batch %d/%d: %d attachments, estimated wait=%.1fs", i+1, len(batches), n, wait.Seconds()))
		if wait >= batchPacingNoticeThreshold {
			notice := i18n.T(ctx, "platform.signal.batch_pacing_notice", "", map[string]any{"wait": formatWait(ctx, wait),
				"batch": i + 1, "total": len(batches)})
			if _, err := a.EmitWarning(ctx, chatID, notice, "", nil, ""); err != nil {
				slog.Warn(fmt.Sprintf("Signal: failed to send pacing notice: %v", err))
			}
		}
		res := sendBatch(ctx, sched, c, withParams(base, "message", "", "attachments", batch), n, fmt.Sprintf("%d/%d", i+1, len(batches)))
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		if res.outcome != nil {
			a.trackSent(res.outcome.response.Timestamp)
			delivered = true
		}
	}
	if !delivered {
		return gateway.SendResult{Success: false, Error: "all Signal attachment batches failed"}, nil
	}
	return gateway.SendResult{Success: true}, nil
}

// Reaction RPC on a message by author and timestamp, routed without a service id upgrade
func (a *SignalAdapter) react(ctx context.Context, chatID, emoji, author string, timestamp int64, remove bool) bool {
	params := targetParams(map[string]any{"account": a.account, "emoji": emoji, "targetAuthor": author, "targetTimestamp": timestamp},
		chatID, chatID)
	if remove {
		params["remove"] = true
	}
	if _, err := a.call(ctx, "sendReaction", params, "", false); err != nil {
		slog.Debug(fmt.Sprintf("Signal: sendReaction failed (chat=%.20s, emoji=%s)", chatID, emoji))
		return false
	}
	return true
}

// Author and timestamp of the inbound message an event came from
func reactionTarget(event *gateway.MessageEvent) (string, int64, bool) {
	raw, ok := event.RawMessage.(map[string]any)
	if !ok || event.Source == nil {
		return "", 0, false
	}
	sender, _ := raw["sender"].(string)
	ts, _ := raw["timestamp_ms"].(int64)
	return sender, ts, sender != "" && ts != 0
}

// Whether outcome reactions may fire, SIGNAL_REACTIONS on and the sender authorized so a stranger never sees the bot
func (a *SignalAdapter) reactionsEnabled(ctx context.Context, event *gateway.MessageEvent) bool {
	flag, err := gateway.GetScopedSecret(ctx, "SIGNAL_REACTIONS", "true", false)
	if err != nil {
		slog.Debug(fmt.Sprintf("Signal: could not read SIGNAL_REACTIONS: %v", err))
		return false
	}
	switch strings.ToLower(strings.TrimSpace(flag)) {
	case "false", "0", "no":
		return false
	}
	if event.Source == nil || event.Source.UserID == nil || *event.Source.UserID == "" {
		return true
	}
	sender := *event.Source.UserID
	if a.HasAuthorizationCheck() {
		chatType, chatID := event.Source.ChatType, event.Source.ChatID
		verdict := a.IsSenderAuthorized(sender, &chatType, &chatID, gateway.AuthorizationOpts{})
		return verdict != nil && *verdict
	}
	return a.dmAllow["*"] || a.dmAllow[sender]
}

// Reacts with eyes when processing begins
func (a *SignalAdapter) OnProcessingStart(ctx context.Context, event *gateway.MessageEvent) error {
	author, ts, ok := reactionTarget(event)
	if ok && a.reactionsEnabled(ctx, event) {
		a.react(ctx, event.Source.ChatID, reactionWorking, author, ts, false)
	}
	return nil
}

// Swaps eyes for a check or cross, a cancelled turn keeping the eyes
func (a *SignalAdapter) OnProcessingComplete(ctx context.Context, event *gateway.MessageEvent, outcome gateway.ProcessingOutcome) error {
	if outcome == gateway.ProcessingCancelled || !a.reactionsEnabled(ctx, event) {
		return nil
	}
	author, ts, ok := reactionTarget(event)
	if !ok {
		return nil
	}
	chatID := event.Source.ChatID
	a.react(ctx, chatID, reactionWorking, author, ts, true)
	switch outcome {
	case gateway.ProcessingSuccess:
		a.react(ctx, chatID, reactionSuccess, author, ts, false)
	case gateway.ProcessingFailure:
		a.react(ctx, chatID, reactionFailure, author, ts, false)
	}
	return nil
}

// Name and type of a chat from the contact or group list, the chat id naming it when signal-cli knows no name
func (a *SignalAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	if gid, ok := strings.CutPrefix(chatID, "group:"); ok {
		name := chatID
		if raw, err := a.call(ctx, "listGroups", map[string]any{"account": a.account, "groupId": []string{gid}}, "", false); err == nil {
			var groups []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			_ = json.Unmarshal(raw, &groups)
			for _, g := range groups {
				if g.ID == gid && strings.TrimSpace(g.Name) != "" {
					name = g.Name
				}
			}
		}
		return map[string]any{"name": name, "type": "group", "chat_id": chatID}, nil
	}
	name := chatID
	params := map[string]any{"account": a.account, "recipient": []string{chatID}, "allRecipients": true}
	if raw, err := a.call(ctx, "listContacts", params, "", false); err == nil {
		var contacts []struct {
			Number  string `json:"number"`
			UUID    string `json:"uuid"`
			Name    string `json:"name"`
			Profile *struct {
				GivenName  string `json:"givenName"`
				FamilyName string `json:"familyName"`
			} `json:"profile"`
		}
		_ = json.Unmarshal(raw, &contacts)
		for _, c := range contacts {
			if c.Number != chatID && c.UUID != chatID {
				continue
			}
			a.rememberRecipient(c.Number, c.UUID)
			switch {
			case strings.TrimSpace(c.Name) != "":
				name = c.Name
			case c.Profile != nil && strings.TrimSpace(c.Profile.GivenName+" "+c.Profile.FamilyName) != "":
				name = strings.TrimSpace(c.Profile.GivenName + " " + c.Profile.FamilyName)
			}
		}
	}
	return map[string]any{"name": name, "type": "dm", "chat_id": chatID}, nil
}
