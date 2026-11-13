package buzz

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Text of a metadata value, empty when it is absent or falsy
func metaText(metadata map[string]any, key string) string {
	if v := metadata[key]; truthy(v) {
		return textOf(v)
	}
	return ""
}

// First non empty string
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Appends a lowercased pubkey once, empty values skipped
func addPubkey(bucket []string, raw any) []string {
	pk := ""
	if truthy(raw) {
		pk = strings.ToLower(textOf(raw))
	}
	if pk == "" {
		return bucket
	}
	for _, existing := range bucket {
		if existing == pk {
			return bucket
		}
	}
	return append(bucket, pk)
}

// Mention candidates of a channel, its member list or else the authors and p tags of recent traffic, cached briefly
func (a *Adapter) channelMemberPubkeys(ctx context.Context, chatID string) ([]string, error) {
	a.mu.Lock()
	cached, ok := a.memberCache[chatID]
	a.mu.Unlock()
	if ok && time.Since(cached.at) < memberCacheTTL {
		return append([]string(nil), cached.value...), nil
	}
	store := func(pks []string) {
		a.mu.Lock()
		a.memberCache[chatID] = ttlEntry[[]string]{time.Now(), append([]string(nil), pks...)}
		a.mu.Unlock()
	}
	rows, err := a.cliJSON(ctx, []string{"channels", "members", "--channel", chatID}, []any{})
	if err != nil {
		return nil, err
	}
	var pks []string
	list, _ := rows.([]any)
	for _, row := range list {
		if m, isMap := row.(map[string]any); isMap {
			pks = addPubkey(pks, m["pubkey"])
		} else {
			pks = addPubkey(pks, row)
		}
	}
	if len(pks) > 0 {
		store(pks)
		return pks, nil
	}
	msgs, err := a.cliJSON(ctx, []string{"messages", "get", "--channel", chatID, "--limit", "50"}, []any{})
	if err != nil {
		return nil, err
	}
	var candidates []string
	msgList, _ := msgs.([]any)
	for _, raw := range msgList {
		msg, isMap := raw.(map[string]any)
		if !isMap {
			continue
		}
		candidates = addPubkey(candidates, msg["pubkey"])
		for _, tag := range eventTags(msg) {
			if len(tag) > 1 && tag[0] == "p" {
				candidates = addPubkey(candidates, textOf(tag[1]))
			}
		}
	}
	if len(candidates) > 0 {
		store(candidates)
	}
	return candidates, nil
}

// Display name of a member profile for mention resolution, cached so a rename is picked up within minutes
func (a *Adapter) profileDisplayName(ctx context.Context, pubkey string) (string, error) {
	a.mu.Lock()
	cached, ok := a.profileNameCache[pubkey]
	a.mu.Unlock()
	if ok && time.Since(cached.at) < profileNameTTL {
		return cached.value, nil
	}
	profiles, err := a.cliJSON(ctx, []string{"users", "get", "--pubkey", pubkey}, []any{})
	if err != nil {
		return "", err
	}
	name := ""
	pick := func(m map[string]any) string {
		if truthy(m["display_name"]) {
			return strip(textOf(m["display_name"]))
		}
		if truthy(m["name"]) {
			return strip(textOf(m["name"]))
		}
		return ""
	}
	if list, isList := profiles.([]any); isList && len(list) > 0 {
		if p0, isMap := list[0].(map[string]any); isMap {
			name = pick(p0)
			if content, isStr := p0["content"].(string); name == "" && isStr && content != "" {
				if prof, isMap := jsonOr(content, map[string]any{}).(map[string]any); isMap {
					name = pick(prof)
				}
			}
		}
	}
	a.mu.Lock()
	a.profileNameCache[pubkey] = ttlEntry[string]{time.Now(), name}
	a.mu.Unlock()
	return name, nil
}

// Member pubkeys genuinely mentioned by at sign display name, longer names first and ambiguous names tagging nobody
func (a *Adapter) mentionPubkeysFor(ctx context.Context, chatID, content string) ([]string, error) {
	if !strings.Contains(content, "@") {
		return nil, nil
	}
	members, err := a.channelMemberPubkeys(ctx, chatID)
	if err != nil {
		return nil, err
	}
	self := a.identity().pubkey
	byName := map[string][]string{}
	display := map[string]string{}
	var order []string
	for _, pk := range members {
		if pk == self {
			continue
		}
		name, err := a.profileDisplayName(ctx, pk)
		if err != nil {
			return nil, err
		}
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, seen := byName[key]; !seen {
			order = append(order, key)
			display[key] = name
		}
		dup := false
		for _, existing := range byName[key] {
			if existing == pk {
				dup = true
				break
			}
		}
		if !dup {
			byName[key] = append(byName[key], pk)
		}
	}
	var found []string
	text := content
	for _, key := range longestFirst(order) {
		spans := displayMentionSpans(text, display[key])
		if len(spans) == 0 {
			continue
		}
		if pks := byName[key]; len(pks) == 1 {
			dup := false
			for _, f := range found {
				if f == pks[0] {
					dup = true
				}
			}
			if !dup {
				found = append(found, pks[0])
			}
		}
		text = consumeSpans(text, spans)
	}
	return found, nil
}

// Publishes with bounded recovery, explicit mentions, then without them, then an escaped token, then a self mention
func (a *Adapter) runMessageSend(ctx context.Context, args []string, content string, mentions []string) (cliResult, error) {
	var mentionArgs []string
	for _, pk := range mentions {
		mentionArgs = append(mentionArgs, "--mention", pk)
	}
	res, err := a.runCLI(ctx, append(append([]string(nil), args...), mentionArgs...), &content)
	if err != nil || res.code == 0 {
		return res, err
	}
	if len(mentionArgs) > 0 && strings.Contains(res.stderr, "not channel members") {
		if res, err = a.runCLI(ctx, args, &content); err != nil || res.code == 0 {
			return res, err
		}
	}
	if escaped, ok := escapeUnresolvedPresentationMention(content, res.stderr); ok {
		slog.Info("Buzz: retrying message after unresolved presentation-mention preflight")
		if res, err = a.runCLI(ctx, args, &escaped); err != nil || res.code == 0 {
			return res, err
		}
	}
	if self := a.identity().pubkey; strings.Contains(res.stderr, "does not match a current channel member") && self != "" {
		return a.runCLI(ctx, append(append([]string(nil), args...), "--mention", self), &content)
	}
	return res, nil
}

// Reply arguments for an anchor, joining the anchor's thread and dropped when replies post flat
func (a *Adapter) replyArgs(anchor string) []string {
	target := a.resolveReplyAnchor(anchor)
	if target == "" || a.replyToMode == "off" {
		return nil
	}
	return []string{"--reply-to", target}
}

// Send result of a messages send run, the verified event id marked seen so its echo never dispatches
func (a *Adapter) sendResult(chatID string, res cliResult, redactPath string) gateway.SendResult {
	if res.code != 0 {
		return gateway.SendResult{Success: false, Error: cliErrorMessage(res.stderr, res.code, redactPath), Retryable: res.code == 2}
	}
	eventID, receiptErr := parseSendReceipt(res.stdout)
	if receiptErr != "" {
		return gateway.SendResult{Success: false, Error: receiptErr}
	}
	a.markSeen(chatID, eventID)
	return gateway.SendResult{Success: true, MessageID: eventID}
}

// Posts markdown text to a channel, threading under the anchor's thread and tagging the members it mentions
func (a *Adapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if content == "" {
		return gateway.SendResult{Success: false, Error: "Empty message"}, nil
	}
	anchor := firstNonEmpty(metaText(metadata, "thread_id"), metaText(metadata, "reply_to_message_id"), replyTo)
	args := append([]string{"messages", "send", "--channel", chatID, "--content", "-"}, a.replyArgs(anchor)...)
	mentions, err := a.mentionPubkeysFor(ctx, chatID, content)
	if err != nil {
		return gateway.SendResult{}, err
	}
	res, err := a.runMessageSend(ctx, args, content, mentions)
	if err != nil {
		return gateway.SendResult{}, err
	}
	result := a.sendResult(chatID, res, "")
	if result.Success {
		a.rememberEventMeta(chatID, textOf(result.MessageID), a.identity().pubkey, content)
	}
	return result, nil
}

// Adds an emoji reaction to an event, a failure logged and reported false
func (a *Adapter) sendReaction(ctx context.Context, chatID, messageID, emoji string) (bool, error) {
	if a.cliPath == "" || emoji == "" || messageID == "" {
		return false, nil
	}
	res, err := a.runCLI(ctx, []string{"reactions", "add", "--event", messageID, "--emoji", emoji}, nil)
	if err != nil {
		return false, err
	}
	if res.code != 0 {
		slog.Debug(fmt.Sprintf("Buzz: reaction add failed for message %s in %s — %s", clipRunes(messageID, 12), chatID,
			cliErrorMessage(res.stderr, res.code, "")))
	}
	return res.code == 0, nil
}

// Edits a sent message in place, answering with the original id the stream consumer keeps addressing
func (a *Adapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	if messageID == "" {
		return gateway.SendResult{Success: false, Error: "Buzz edit needs a message id"}, nil
	}
	if content == "" {
		return gateway.SendResult{Success: false, Error: "Empty message"}, nil
	}
	res, err := a.runCLI(ctx, []string{"messages", "edit", "--event", messageID, "--content=" + content}, nil)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if res.code != 0 {
		return gateway.SendResult{Success: false, Error: cliErrorMessage(res.stderr, res.code, ""), Retryable: res.code == 2}, nil
	}
	data, _ := jsonOr(res.stdout, map[string]any{}).(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	if truthy(data["event_id"]) {
		a.markSeen(chatID, textOf(data["event_id"]))
	}
	accepted := true
	if v, ok := data["accepted"]; ok {
		accepted = truthy(v)
	}
	return gateway.SendResult{Success: accepted, MessageID: messageID, RawResponse: data}, nil
}

// Deletes a sent message, its deletion event marked seen
func (a *Adapter) DeleteMessage(ctx context.Context, chatID, messageID string) (bool, error) {
	if messageID == "" {
		return false, nil
	}
	res, err := a.runCLI(ctx, []string{"messages", "delete", "--event", messageID}, nil)
	if err != nil {
		return false, err
	}
	if res.code != 0 {
		return false, nil
	}
	text := res.stdout
	if text == "" {
		text = "{}"
	}
	decoded, err := decodeJSON([]byte(text))
	if err != nil {
		return true, nil
	}
	data, _ := decoded.(map[string]any)
	if truthy(data["event_id"]) {
		a.markSeen(chatID, textOf(data["event_id"]))
	}
	if v, ok := data["accepted"]; ok {
		return truthy(v), nil
	}
	return true, nil
}

// Uploads a local file as a native attachment with an optional caption, host paths kept out of chat visible errors
func (a *Adapter) sendFileAttachment(ctx context.Context, chatID, filePath, caption, replyTo string, metadata map[string]any, probe bool) (gateway.SendResult, error) {
	local := expandUser(filePath)
	if probe && !isFile(local) {
		return gateway.SendResult{Success: false, Error: "Media file not found"}, nil
	}
	args := []string{"messages", "send", "--channel", chatID, "--file", local, "--content", "-"}
	args = append(args, a.replyArgs(firstNonEmpty(metaText(metadata, "thread_id"), replyTo))...)
	res, err := a.runMessageSend(ctx, args, caption, nil)
	if err != nil {
		return gateway.SendResult{}, err
	}
	return a.sendResult(chatID, res, local), nil
}

// Image upload for a local file, otherwise the URL posted as a markdown renderable link
func (a *Adapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if !strings.HasPrefix(imageURL, "http://") && !strings.HasPrefix(imageURL, "https://") {
		if local := expandUser(imageURL); isFile(local) {
			return a.sendFileAttachment(ctx, chatID, local, caption, replyTo, metadata, false)
		}
	}
	text := imageURL
	if caption != "" {
		text = caption + "\n" + imageURL
	}
	return a.Send(ctx, chatID, text, replyTo, metadata)
}

// Local image upload, a missing path taking the base notice so host paths never reach chat
func (a *Adapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if local := expandUser(imagePath); isFile(local) {
		return a.sendFileAttachment(ctx, chatID, local, caption, replyTo, metadata, false)
	}
	return a.BasePlatformAdapter.SendImageFile(ctx, chatID, imagePath, caption, replyTo, metadata, extra)
}

// Local document upload through the native file attachment path
func (a *Adapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFileAttachment(ctx, chatID, filePath, caption, replyTo, metadata, true)
}

// Local video upload through the native file attachment path
func (a *Adapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFileAttachment(ctx, chatID, videoPath, caption, replyTo, metadata, true)
}

// Local audio upload through the native file attachment path
func (a *Adapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFileAttachment(ctx, chatID, audioPath, caption, replyTo, metadata, true)
}
