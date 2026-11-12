package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Discord HTTP failure rendered the way discord.py words it, status, reason, code and flattened errors
type discordHTTPException struct {
	Status int
	Reason string
	Code   int
	Text   string
}

func (e *discordHTTPException) Error() string {
	s := fmt.Sprintf("%d %s (error code: %d)", e.Status, e.Reason, e.Code)
	if e.Text != "" {
		s += ": " + e.Text
	}
	return s
}

// Nested Discord validation errors flattened into dotted keys with their joined messages
func flattenErrorDict(d map[string]any, key string, out *[][2]string) {
	for k, v := range d {
		newKey := k
		if key != "" {
			newKey = key + "." + k
		}
		if sub, ok := v.(map[string]any); ok {
			if errs, has := sub["_errors"].([]any); has {
				var msgs []string
				for _, x := range errs {
					if m, ok := x.(map[string]any); ok {
						msg, _ := m["message"].(string)
						msgs = append(msgs, msg)
					}
				}
				*out = append(*out, [2]string{newKey, strings.Join(msgs, " ")})
				continue
			}
			flattenErrorDict(sub, newKey, out)
			continue
		}
		*out = append(*out, [2]string{newKey, pyrepr.Str(v)})
	}
}

// Rewrites a discordgo REST error into the discord.py exception text, other errors unchanged
func dpyError(err error) error {
	var rest *discordgo.RESTError
	if !errors.As(err, &rest) || rest.Response == nil {
		return err
	}
	exc := &discordHTTPException{Status: rest.Response.StatusCode, Reason: http.StatusText(rest.Response.StatusCode)}
	var body map[string]any
	if json.Unmarshal(rest.ResponseBody, &body) != nil {
		exc.Text = string(rest.ResponseBody)
		return exc
	}
	if code, ok := body["code"].(float64); ok {
		exc.Code = int(code)
	}
	base, _ := body["message"].(string)
	exc.Text = base
	if errs, ok := body["errors"].(map[string]any); ok && len(errs) > 0 {
		var items [][2]string
		flattenErrorDict(errs, "", &items)
		lines := make([]string, 0, len(items))
		for _, it := range items {
			lines = append(lines, fmt.Sprintf("In %s: %s", it[0], it[1]))
		}
		exc.Text = base + "\n" + strings.Join(lines, "\n")
	}
	return exc
}

// Splitter long messages go through, the base truncate_message
var truncateMessage = gateway.TruncateMessage

// GFM tables become bullet lists since Discord renders no pipe tables
func (a *DiscordAdapter) FormatMessage(content string) string {
	if content == "" {
		return content
	}
	return gateway.ConvertTableToBullets(content)
}

// Live client, nil when disconnected
func (a *DiscordAdapter) liveClient() *discordgo.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// Python int of a snowflake text, the ValueError text when it is not an integer
func snowflakeInt(raw string) (string, error) {
	n, ok := pyint.Parse(raw)
	if !ok {
		return "", fmt.Errorf("invalid literal for int() with base 10: %s", pyrepr.Repr(raw))
	}
	return n.String(), nil
}

// Cached channel first then a REST fetch, accepting a pasted channel link
func (a *DiscordAdapter) resolveChannel(ctx context.Context, channelID string) (*discordgo.Channel, error) {
	if id, ok := gateway.DiscordChannelIDFromLink(pystr.Strip(channelID)); ok && id != "" {
		channelID = id
	}
	id, err := snowflakeInt(channelID)
	if err != nil {
		return nil, err
	}
	if ch := a.stateChannel(id); ch != nil {
		return ch, nil
	}
	client := a.liveClient()
	if client == nil {
		return nil, errors.New("'NoneType' object has no attribute 'fetch_channel'")
	}
	ch, err := client.Channel(id, discordgo.WithContext(ctx))
	if err != nil {
		return nil, dpyError(err)
	}
	return ch, nil
}

// Reply reference built from ids with no fetch, sends to deleted targets degrading to the retry path
func messageReferenceFromIDs(messageID string, ch *discordgo.Channel) (*discordgo.MessageReference, error) {
	id, err := snowflakeInt(messageID)
	if err != nil {
		return nil, err
	}
	no := false
	ref := &discordgo.MessageReference{MessageID: id, FailIfNotExists: &no}
	if ch != nil {
		ref.ChannelID = ch.ID
		ref.GuildID = ch.GuildID
	}
	return ref, nil
}

// Reply anchor honoring reply_to_mode, nil when off or the id is not an integer
func (a *DiscordAdapter) replyReferenceForSend(replyTo string, ch *discordgo.Channel) *discordgo.MessageReference {
	if replyTo == "" || a.replyToMode == "off" {
		return nil
	}
	ref, err := messageReferenceFromIDs(replyTo, ch)
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not build reply-to reference: %s", err))
		return nil
	}
	return ref
}

// Keeps the first chunks under the split cap and replaces the rest with a truncation notice
func (a *DiscordAdapter) capSplitChunks(ctx context.Context, chunks []string) []string {
	if len(chunks) <= discordMaxSplitMessages {
		return chunks
	}
	kept := append([]string(nil), chunks[:discordMaxSplitMessages-1]...)
	dropped := 0
	for _, c := range chunks[discordMaxSplitMessages-1:] {
		dropped += utf8.RuneCountInString(c)
	}
	notice := "\n\n" + i18n.T(ctx, "platform.discord.limits.response_truncated", "", map[string]any{
		"max_messages": fmt.Sprint(discordMaxSplitMessages), "dropped_chars": fmt.Sprint(dropped)})
	if text := a.WarningText(ctx, notice, nil, "", "", nil); text != nil && *text != "" {
		kept = append(kept, notice)
	}
	return kept
}

// Posts one message to a channel with the adapter's allowed mentions
func (a *DiscordAdapter) channelSend(ctx context.Context, channelID, content string, ref *discordgo.MessageReference) (*discordgo.Message, error) {
	client := a.liveClient()
	if client == nil {
		return nil, errors.New("Not connected")
	}
	msg, err := client.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Content: content, Reference: ref, AllowedMentions: a.allowedMentions}, discordgo.WithContext(ctx))
	if err != nil {
		return nil, dpyError(err)
	}
	return msg, nil
}

// Sends to a channel or the metadata thread, forum channels getting a new thread post
func (a *DiscordAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if a.liveClient() == nil {
		return gateway.SendResult{Success: false, Error: "send_path_degraded", Retryable: true}, nil
	}
	notify := metadata != nil && pyrepr.Truthy(metadata["notify"])
	if pystr.Strip(content) == "" {
		slog.Warn(fmt.Sprintf("[%s] Dropped empty message to chat=%s (caller bug). Call site:\n%s", a.Name(), chatID, debug.Stack()))
		return a.recordResponseAsync(ctx, replyTo, gateway.SendResult{Success: false, Error: "Refusing to send empty message"}, content, notify, metadata), nil
	}
	result, err := a.sendInner(ctx, chatID, content, replyTo, metadata, notify)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to send Discord message: %s", a.Name(), err))
		if isDiscordTransportError(err) {
			result = gateway.SendResult{Success: false, Error: "send_path_degraded", Retryable: true}
		} else {
			result = gateway.SendResult{Success: false, Error: err.Error()}
		}
		return a.recordResponseAsync(ctx, replyTo, result, content, notify, metadata), nil
	}
	return result, nil
}

// Body of send whose errors the caller classifies as transport or permanent
func (a *DiscordAdapter) sendInner(ctx context.Context, chatID, content, replyTo string, metadata map[string]any, final bool) (gateway.SendResult, error) {
	threadID := ""
	if metadata != nil && pyrepr.Truthy(metadata["thread_id"]) {
		threadID = pyrepr.Str(metadata["thread_id"])
	}
	nonconversational := metadataMarksNonconversational(metadata)
	target := chatID
	if threadID != "" {
		target = threadID
	}
	ch, err := a.resolveChannel(ctx, target)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if ch == nil {
		if threadID != "" {
			return gateway.SendResult{Success: false, Error: fmt.Sprintf("Thread %s not found", threadID)}, nil
		}
		return gateway.SendResult{Success: false, Error: fmt.Sprintf("Channel %s not found", chatID)}, nil
	}
	if isForumParent(ch) {
		return a.recordResponseAsync(ctx, replyTo, a.sendToForum(ctx, ch, content), content, final, metadata), nil
	}
	chunks := a.capSplitChunks(ctx, truncateMessage(a.FormatMessage(content), discordMaxMessageLength, nil))
	var messageIDs []any
	reference := a.replyReferenceForSend(replyTo, ch)
	for i, chunk := range chunks {
		chunkRef := reference
		if a.replyToMode != "all" && i != 0 {
			chunkRef = nil
		}
		msg, err := a.channelSend(ctx, ch.ID, chunk, chunkRef)
		if err != nil {
			if chunkRef == nil || !isReplyReferenceRejected(err) {
				return gateway.SendResult{}, err
			}
			slog.Warn(fmt.Sprintf("[%s] Reply target %s rejected the reply reference; retrying send without reply reference", a.Name(), replyTo))
			reference = nil
			if msg, err = a.channelSend(ctx, ch.ID, chunk, nil); err != nil {
				return gateway.SendResult{}, err
			}
		}
		messageIDs = append(messageIDs, msg.ID)
	}
	if len(messageIDs) > 0 {
		if nonconversational {
			ids := make([]string, 0, len(messageIDs))
			for _, id := range messageIDs {
				ids = append(ids, id.(string))
			}
			a.nonconversationalMessages.markMany(ctx, ids)
		} else if !looksLikeNonconversationalHistoryMessage(content) {
			a.mu.Lock()
			a.lastSelfMessageID[target] = messageIDs[len(messageIDs)-1].(string)
			a.mu.Unlock()
		}
	}
	result := gateway.SendResult{Success: true, RawResponse: map[string]any{"message_ids": messageIDs}}
	if len(messageIDs) > 0 {
		result.MessageID = messageIDs[0]
	}
	return a.recordResponseAsync(ctx, replyTo, result, content, final, metadata), nil
}

// Forum thread created by the create thread call and its starter message
type forumThread struct {
	thread  *discordgo.Channel
	starter *discordgo.Message
}

// Thread channel, thread id, starter message and starter id of a created forum thread
func forumThreadParts(ft *forumThread) (*discordgo.Channel, string, *discordgo.Message, string) {
	threadID := ""
	if ft.thread != nil {
		threadID = ft.thread.ID
	}
	messageID := threadID
	if ft.starter != nil && ft.starter.ID != "" {
		messageID = ft.starter.ID
	}
	return ft.thread, threadID, ft.starter, messageID
}

// Creates a forum thread whose starter message carries the content and files
func (a *DiscordAdapter) forumCreateThread(ctx context.Context, forumID, name, content string, files []*discordgo.File) (*forumThread, error) {
	client := a.liveClient()
	if client == nil {
		return nil, errors.New("Not connected")
	}
	payload := map[string]any{"name": name, "message": map[string]any{"content": content, "allowed_mentions": a.allowedMentions}}
	if content == "" {
		payload["message"] = map[string]any{"allowed_mentions": a.allowedMentions}
	}
	endpoint := discordgo.EndpointChannelThreads(forumID)
	var body []byte
	var err error
	if len(files) > 0 {
		contentType, multipart, merr := discordgo.MultipartBodyWithJSON(payload, files)
		if merr != nil {
			return nil, merr
		}
		body, err = client.RequestRaw(http.MethodPost, endpoint, contentType, multipart, endpoint, 0, discordgo.WithContext(ctx))
	} else {
		body, err = client.RequestWithBucketID(http.MethodPost, endpoint, payload, endpoint, discordgo.WithContext(ctx))
	}
	if err != nil {
		return nil, dpyError(err)
	}
	var resp struct {
		discordgo.Channel
		Message *discordgo.Message `json:"message"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	thread := resp.Channel
	return &forumThread{thread: &thread, starter: resp.Message}, nil
}

// Creates a forum thread post from the message, chunk failures listed under warnings
func (a *DiscordAdapter) sendToForum(ctx context.Context, forum *discordgo.Channel, content string) gateway.SendResult {
	chunks := a.capSplitChunks(ctx, truncateMessage(a.FormatMessage(content), discordMaxMessageLength, nil))
	threadName := deriveForumThreadName(ctx, content)
	starter := threadName
	if len(chunks) > 0 {
		starter = chunks[0]
	}
	ft, err := a.forumCreateThread(ctx, forum.ID, threadName, starter, nil)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to create forum thread in %s: %s", a.Name(), forum.ID, err))
		return gateway.SendResult{Success: false, Error: fmt.Sprintf("Forum thread creation failed: %s", err)}
	}
	threadChannel, threadID, _, messageID := forumThreadParts(ft)
	messageIDs := []any{messageID}
	var warnings []any
	for _, chunk := range chunks[min(1, len(chunks)):] {
		msg, err := a.channelSend(ctx, threadChannel.ID, chunk, nil)
		if err != nil {
			warning := fmt.Sprintf("Failed to send follow-up chunk to forum thread %s: %s", threadID, err)
			slog.Warn(fmt.Sprintf("[%s] %s", a.Name(), warning))
			warnings = append(warnings, warning)
			continue
		}
		messageIDs = append(messageIDs, msg.ID)
	}
	raw := map[string]any{"message_ids": messageIDs, "thread_id": threadID}
	if len(warnings) > 0 {
		raw["warnings"] = warnings
	}
	return gateway.SendResult{Success: true, MessageID: messageIDs[0], RawResponse: raw}
}

// Creates a forum thread whose starter message carries file attachments
func (a *DiscordAdapter) forumPostFile(ctx context.Context, forum *discordgo.Channel, threadName, content string, files []*discordgo.File) gateway.SendResult {
	if threadName == "" {
		hint := content
		if pystr.Strip(hint) == "" && len(files) > 0 {
			hint = files[0].Name
		}
		if pystr.Strip(hint) != "" {
			threadName = deriveForumThreadName(ctx, hint)
		} else {
			threadName = i18n.T(ctx, "platform.discord.forum.default_title", "", nil)
		}
	}
	ft, err := a.forumCreateThread(ctx, forum.ID, threadName, content, files)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to create forum thread with file in %s: %s", a.Name(), forum.ID, err))
		return gateway.SendResult{Success: false, Error: fmt.Sprintf("Forum thread creation failed: %s", err)}
	}
	_, threadID, starter, messageID := forumThreadParts(ft)
	if len(files) > 0 && (starter == nil || len(starter.Attachments) == 0) {
		filename := files[0].Name
		shown := filename
		if shown == "" {
			shown = "file"
		}
		slog.Warn(fmt.Sprintf("[%s] Forum thread %s starter has no attachments for %s", a.Name(), threadID, shown))
		errText := "Discord created the forum thread but attached no files"
		if filename != "" {
			errText += " (" + filename + ")"
		}
		var mid any
		if messageID != "" {
			mid = messageID
		}
		return gateway.SendResult{Success: false, Error: errText, MessageID: mid, RawResponse: map[string]any{"thread_id": threadID}}
	}
	return gateway.SendResult{Success: true, MessageID: messageID, RawResponse: map[string]any{"thread_id": threadID}}
}

// Thread name from the first message line without heading marks, capped at 100 characters
func deriveForumThreadName(ctx context.Context, message string) string {
	firstLine := pystr.Strip(strings.SplitN(pystr.Strip(message), "\n", 2)[0])
	firstLine = pystr.Strip(strings.TrimLeft(firstLine, "#"))
	if firstLine == "" {
		firstLine = i18n.T(ctx, "platform.discord.forum.default_title", "", nil)
	}
	return cut(firstLine, 100)
}

// Edits one message content
func (a *DiscordAdapter) editContent(ctx context.Context, channelID, messageID, content string) error {
	client := a.liveClient()
	if client == nil {
		return errors.New("Not connected")
	}
	_, err := client.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: messageID, Channel: channelID, Content: &content}, discordgo.WithContext(ctx))
	return dpyError(err)
}

// Edits a sent message, truncating previews mid stream and splitting oversized final edits
func (a *DiscordAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	return a.EditMessageWithMetadata(ctx, chatID, messageID, content, finalize, nil)
}

// Edit whose metadata carries the internal reply anchor a final edit records against
func (a *DiscordAdapter) EditMessageWithMetadata(ctx context.Context, chatID, messageID, content string, finalize bool, metadata map[string]any) (gateway.SendResult, error) {
	if a.liveClient() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	result, err := func() (gateway.SendResult, error) {
		ch, err := a.resolveChannel(ctx, chatID)
		if err != nil {
			return gateway.SendResult{}, err
		}
		mid, err := snowflakeInt(messageID)
		if err != nil {
			return gateway.SendResult{}, err
		}
		formatted := a.FormatMessage(content)
		key := [2]string{chatID, messageID}
		saturated := false
		if finalize {
			a.mu.Lock()
			delete(a.lastOverflowPreview, key)
			a.mu.Unlock()
		}
		if utf8.RuneCountInString(formatted) > discordMaxMessageLength {
			if finalize {
				return a.editOverflowSplit(ctx, ch, mid, messageID, content), nil
			}
			formatted = truncateMessage(formatted, discordMaxMessageLength, nil)[0]
			saturated = true
			a.mu.Lock()
			same := a.lastOverflowPreview[key] == formatted
			a.mu.Unlock()
			if same {
				return gateway.SendResult{Success: true, MessageID: messageID}, nil
			}
		} else if !finalize {
			a.mu.Lock()
			delete(a.lastOverflowPreview, key)
			a.mu.Unlock()
		}
		if editErr := a.editContent(ctx, ch.ID, mid, formatted); editErr != nil {
			if !isLengthOverflowError(editErr) {
				return gateway.SendResult{}, editErr
			}
			if finalize {
				return a.editOverflowSplit(ctx, ch, mid, messageID, content), nil
			}
			truncated := truncateMessage(formatted, discordMaxMessageLength, nil)[0]
			a.mu.Lock()
			same := a.lastOverflowPreview[key] == truncated
			a.mu.Unlock()
			if same {
				return gateway.SendResult{Success: true, MessageID: messageID}, nil
			}
			if err := a.editContent(ctx, ch.ID, mid, truncated); err != nil {
				return gateway.SendResult{}, err
			}
			a.mu.Lock()
			a.lastOverflowPreview[key] = truncated
			a.mu.Unlock()
		} else if saturated {
			a.mu.Lock()
			a.lastOverflowPreview[key] = formatted
			a.mu.Unlock()
		}
		result := gateway.SendResult{Success: true, MessageID: messageID}
		if finalize {
			replyTo := ""
			if metadata != nil && pyrepr.Truthy(metadata["reply_to_message_id"]) {
				replyTo = pyrepr.Str(metadata["reply_to_message_id"])
			}
			a.recordResponseAsync(ctx, replyTo, result, content, true, nil)
		}
		return result, nil
	}()
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Failed to edit Discord message %s: %s", a.Name(), messageID, err))
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	return result, nil
}

// Whether Discord refused the reply anchor as a system message or a deleted target
func isReplyReferenceRejected(err error) bool {
	text := err.Error()
	return (strings.Contains(text, "error code: 50035") && strings.Contains(text, "Cannot reply to a system message")) ||
		strings.Contains(text, "error code: 10008")
}

// Whether an edit or send failed for exceeding the length limit
func isLengthOverflowError(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "error code: 50035") && (strings.Contains(text, "2000 or fewer") || strings.Contains(text, "fewer in length"))
}

// Delivers an oversized final edit as an edited first chunk plus reply chained continuations
func (a *DiscordAdapter) editOverflowSplit(ctx context.Context, ch *discordgo.Channel, mid, messageID, content string) gateway.SendResult {
	formatted := a.FormatMessage(content)
	chunks := a.capSplitChunks(ctx, truncateMessage(formatted, discordMaxMessageLength, nil))
	if len(chunks) <= 1 {
		text := formatted
		if len(chunks) == 1 {
			text = chunks[0]
		}
		if err := a.editContent(ctx, ch.ID, mid, text); err != nil {
			slog.Error(fmt.Sprintf("[%s] Failed to edit Discord message %s: %s", a.Name(), messageID, err))
			return gateway.SendResult{Success: false, Error: err.Error()}
		}
		return gateway.SendResult{Success: true, MessageID: messageID}
	}
	if err := a.editContent(ctx, ch.ID, mid, chunks[0]); err != nil {
		slog.Error(fmt.Sprintf("[%s] Overflow split: first-chunk edit failed: %s", a.Name(), err))
		return gateway.SendResult{Success: false, Error: err.Error()}
	}
	var continuation []any
	delivered := 1
	prevID := mid
	for _, chunk := range chunks[1:] {
		ref, _ := messageReferenceFromIDs(prevID, ch)
		sent, err := a.channelSend(ctx, ch.ID, chunk, ref)
		if err != nil {
			slog.Warn(fmt.Sprintf("[%s] Overflow continuation send failed (%s); retrying without reply reference", a.Name(), err))
			if sent, err = a.channelSend(ctx, ch.ID, chunk, nil); err != nil {
				slog.Warn(fmt.Sprintf("[%s] Overflow split: stopped at %d/%d chunks delivered: %s", a.Name(), delivered, len(chunks), err))
				lastID := any(messageID)
				if len(continuation) > 0 {
					lastID = continuation[len(continuation)-1]
				}
				return gateway.SendResult{Success: true, MessageID: lastID, ContinuationMessageIDs: continuation, RawResponse: map[string]any{
					"partial_overflow": true, "delivered_chunks": delivered, "total_chunks": len(chunks),
					"last_message_id": lastID, "continuation_message_ids": continuation}}
			}
		}
		continuation = append(continuation, sent.ID)
		delivered++
		prevID = sent.ID
	}
	lastID := any(messageID)
	if len(continuation) > 0 {
		lastID = continuation[len(continuation)-1]
	}
	if !looksLikeNonconversationalHistoryMessage(content) {
		a.mu.Lock()
		a.lastSelfMessageID[ch.ID] = lastID.(string)
		a.mu.Unlock()
	}
	slog.Debug(fmt.Sprintf("[%s] Overflow split delivered %d chunks; last_id=%s", a.Name(), delivered, lastID))
	return gateway.SendResult{Success: true, MessageID: lastID, ContinuationMessageIDs: continuation}
}

// Adds an emoji reaction to the message, false on any failure
func (a *DiscordAdapter) addReaction(ctx context.Context, m *inboundMessage, emoji string) bool {
	client := a.liveClient()
	if m == nil || client == nil {
		return false
	}
	if err := client.MessageReactionAdd(m.ChannelID, m.ID, emoji, discordgo.WithContext(ctx)); err != nil {
		slog.Debug(fmt.Sprintf("[%s] add_reaction failed (%s): %s", a.Name(), emoji, dpyError(err)))
		return false
	}
	return true
}

// Removes the bot's own emoji reaction from the message, false on any failure
func (a *DiscordAdapter) removeReaction(ctx context.Context, m *inboundMessage, emoji string) bool {
	client := a.liveClient()
	if m == nil || client == nil || a.selfUser() == nil {
		return false
	}
	if err := client.MessageReactionRemove(m.ChannelID, m.ID, emoji, "@me", discordgo.WithContext(ctx)); err != nil {
		slog.Debug(fmt.Sprintf("[%s] remove_reaction failed (%s): %s", a.Name(), emoji, dpyError(err)))
		return false
	}
	return true
}

// Whether outcome reactions are on via the reactions extra or DISCORD_REACTIONS
func (a *DiscordAdapter) reactionsEnabled(ctx context.Context) bool {
	return a.extraOrEnvFlag(ctx, "reactions", "DISCORD_REACTIONS", "true", false)
}

// Adds the in progress reaction and records durable handling state
func (a *DiscordAdapter) OnProcessingStart(ctx context.Context, event *gateway.MessageEvent) error {
	m := eventRawMessage(event)
	acked := false
	if a.reactionsEnabled(ctx) && m != nil {
		acked = a.addReaction(ctx, m, "👀")
	}
	a.recordDiscordProcessingStart(ctx, event, acked)
	return nil
}

// Swaps the in progress reaction for the outcome reaction and records durable state
func (a *DiscordAdapter) OnProcessingComplete(ctx context.Context, event *gateway.MessageEvent, outcome gateway.ProcessingOutcome) error {
	a.recordDiscordProcessingComplete(ctx, event, outcome)
	if !a.reactionsEnabled(ctx) {
		return nil
	}
	if m := eventRawMessage(event); m != nil {
		a.removeReaction(ctx, m, "👀")
		switch outcome {
		case gateway.ProcessingSuccess:
			a.addReaction(ctx, m, "✅")
		case gateway.ProcessingFailure:
			a.addReaction(ctx, m, "❌")
		}
	}
	return nil
}
