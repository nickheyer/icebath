package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Whether the sender is the bot itself, also true while the bot user id is unresolved
func (a *MatrixAdapter) isSelfSender(sender string) bool {
	a.mu.Lock()
	own := pystr.Lower(pystr.Strip(a.userID))
	a.mu.Unlock()
	return own == "" || pystr.Lower(pystr.Strip(sender)) == own
}

// Whether the sender is an appservice, bridge or system identity, or a malformed id
func isSystemOrBridgeSender(sender string) bool {
	localpart, _, _ := strings.Cut(strings.TrimLeft(pystr.Strip(sender), "@"), ":")
	return localpart == "" || strings.HasPrefix(localpart, "_")
}

// Allowed rooms gate with DMs exempt so personal chats survive a project allowlist
func (a *MatrixAdapter) isAllowedMatrixRoomEvent(ctx context.Context, roomID string) bool {
	if len(a.allowedRoomIDs) == 0 || a.allowedRoomIDs[roomID] {
		return true
	}
	return a.isDMRoom(ctx, roomID)
}

// Clears the consecutive late drop count, its skew and the once only warning
func (a *MatrixAdapter) resetClockSkewDetector() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lateGraceDrops = 0
	a.lateGraceSkew = 0.0
	a.clockSkewWarned = false
}

// Warns once when grace drops keep arriving well after startup with a consistent skew
func (a *MatrixAdapter) noteLateGraceDrop(eventTS float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.clockSkewWarned || nowSeconds()-a.startupTS <= 30 {
		return
	}
	skew := a.startupTS - eventTS
	if !(5 < skew && skew < 86400) {
		return
	}
	if a.lateGraceDrops > 0 && absFloat(skew-a.lateGraceSkew) < 60 {
		a.lateGraceDrops++
	} else {
		a.lateGraceSkew = skew
		a.lateGraceDrops = 1
	}
	if a.lateGraceDrops >= 3 {
		slog.Warn(fmt.Sprintf("Matrix: dropped %d consecutive live events as 'too old' more than 30s after startup "+
			"(skew ≈ %.0fs). The host system clock is likely set ahead of real time, which causes "+
			"the startup grace filter to silently discard every incoming message. Run "+
			"`timedatectl set-ntp true` (or sync NTP) and restart the bot.", a.lateGraceDrops, skew))
		a.clockSkewWarned = true
	}
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// Map value of a JSON object key, empty when absent or not an object
func objectOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Gates an inbound room message on sender, room, dedup, startup grace, edits and notices, then routes it by msgtype
func (a *MatrixAdapter) onRoomMessage(ctx context.Context, evt *event.Event) error {
	roomID := string(evt.RoomID)
	sender := string(evt.Sender)
	slog.Debug(fmt.Sprintf("Matrix: callback fired — event %s from %s in %s", evt.ID, sender, roomID))
	if a.isSelfSender(sender) {
		return nil
	}
	if isSystemOrBridgeSender(sender) {
		slog.Debug(fmt.Sprintf("Matrix: ignoring system/bridge sender %s in %s", sender, roomID))
		return nil
	}
	for _, pattern := range a.ignoredUserPatterns {
		if reSearch(pattern, sender) {
			slog.Debug(fmt.Sprintf("Matrix: ignoring sender %s in %s due to configured ignore pattern", sender, roomID))
			return nil
		}
	}
	if !a.isAllowedMatrixRoomEvent(ctx, roomID) {
		slog.Info(fmt.Sprintf("Matrix: ignoring message from unauthorized room %s", roomID))
		return nil
	}
	eventID := string(evt.ID)
	if a.isDuplicateEvent(eventID) {
		return nil
	}
	eventTS := matrixEventTimestampSeconds(evt.Timestamp)
	a.mu.Lock()
	startup := a.startupTS
	a.mu.Unlock()
	if eventTS != 0 && eventTS < startup-startupGraceSeconds {
		a.noteLateGraceDrop(eventTS)
		return nil
	}
	sourceContent := evt.Content.Raw
	if sourceContent == nil {
		return nil
	}
	msgtype, _ := sourceContent["msgtype"].(string)
	relatesTo := objectOf(sourceContent["m.relates_to"])
	if relatesTo["rel_type"] == "m.replace" {
		return nil
	}
	if msgtype == "m.notice" && !a.processNotices {
		return nil
	}
	switch msgtype {
	case "m.image", "m.audio", "m.video", "m.file":
		return a.handleMediaMessage(ctx, roomID, sender, eventID, eventTS, sourceContent, relatesTo, msgtype, false)
	case "m.text", "m.notice":
		return a.handleTextMessage(ctx, roomID, sender, eventID, eventTS, sourceContent, relatesTo)
	}
	return nil
}

// Gating outcome of one inbound message, nil when the message is dropped
type messageContext struct {
	body        string
	isDM        bool
	chatType    string
	threadID    string
	displayName string
	source      *gateway.SessionSource
}

// Shared mention, thread and DM gating returning the routed context or nil to drop
func (a *MatrixAdapter) resolveMessageContext(ctx context.Context, roomID, sender, eventID, body string, sourceContent,
	relatesTo map[string]any, mentionClaimed bool, voiceGate *VoiceGate) (*messageContext, error) {
	identity := a.resolveRoomIdentity(ctx, roomID, false)
	isDM := a.isDMRoom(ctx, roomID)
	chatType := "group"
	if isDM {
		chatType = "dm"
	}
	threadID := threadRoot(relatesTo)
	isMentioned := mentionClaimed || a.contentMentionsBot(body, sourceContent)
	if !isDM {
		if len(a.allowedRooms) > 0 && !a.allowedRooms[roomID] {
			slog.Debug(fmt.Sprintf("Matrix: ignoring message %s in %s — room not in MATRIX_ALLOWED_ROOMS whitelist", eventID, roomID))
			return nil, nil
		}
		isFreeRoom := a.freeRooms[roomID]
		inBotThread := threadID != "" && a.threads.Contains(threadID)
		if a.requireMention && !isFreeRoom && !inBotThread {
			if !isMentioned && !strings.HasPrefix(body, "/") {
				if voiceGate != nil {
					a.parkedVoices.Park(roomID, sender, voiceGate, eventID, sourceContent, relatesTo)
				}
				slog.Debug(fmt.Sprintf("Matrix: ignoring message %s in %s — no @mention "+
					"(set MATRIX_REQUIRE_MENTION=false to disable)", eventID, roomID))
				return nil, nil
			}
		} else if a.threadRequireMention && inBotThread && !isFreeRoom && !isMentioned {
			slog.Debug(fmt.Sprintf("Matrix: ignoring message %s in thread %s — no @mention (thread_require_mention=true)", eventID, threadID))
			return nil, nil
		}
	}
	if isMentioned && a.requireMention {
		if pyTruthy(relatesTo["m.in_reply_to"]) {
			quoteBlock, replyText := splitReplyFallback(body)
			body = quoteBlock + a.stripMention(replyText)
		} else {
			body = a.stripMention(body)
		}
	}
	if threadID == "" {
		var synthetic bool
		if isDM {
			synthetic = (a.dmMentionThreads && isMentioned) || a.dmAutoThread
		} else {
			synthetic = a.matrixSessionScope == "thread" || (a.matrixSessionScope != "room" && a.autoThread)
		}
		if synthetic {
			threadID = eventID
		}
	}
	if voiceGate != nil {
		a.parkedVoices.Release(roomID, sender, voiceGate)
	}
	displayName := a.getDisplayName(ctx, roomID, sender)
	fields := gateway.SourceFields{ChatName: &identity.DisplayName, ChatType: chatType, UserID: &sender, UserName: &displayName,
		ChatTopic: identity.RoomTopic, GuildID: identity.ServerName, MessageID: &eventID}
	if threadID != "" {
		fields.ThreadID = &threadID
		fields.ParentChatID = &roomID
	}
	source := a.BuildSource(ctx, roomID, fields)
	if threadID != "" {
		if err := a.threads.MarkAsync(ctx, threadID); err != nil {
			return nil, err
		}
	}
	a.backgroundReadReceipt(ctx, roomID, eventID)
	return &messageContext{body: body, isDM: isDM, chatType: chatType, threadID: threadID, displayName: displayName, source: source}, nil
}

// Body without the reply fallback plus the replied event id, quoted text and its author id and name
func (a *MatrixAdapter) extractReplyContext(ctx context.Context, roomID, body string, relatesTo map[string]any) (string, *string, *string, *string, *string) {
	var replyTo, replyToText, replyToAuthorID, replyToAuthorName *string
	if v := objectOf(relatesTo["m.in_reply_to"])["event_id"]; pyTruthy(v) {
		s := strOf(v)
		replyTo = &s
	}
	if replyTo != nil && strings.HasPrefix(body, "> ") {
		replyToText, replyToAuthorID = extractReplyFallback(body)
		body = stripReplyFallback(body)
		if replyToAuthorID != nil {
			name := a.getDisplayName(ctx, roomID, *replyToAuthorID)
			replyToAuthorName = &name
		}
	}
	return body, replyTo, replyToText, replyToAuthorID, replyToAuthorName
}

// Media fields carried by an inbound media event, empty msgtype for text
type inboundMedia struct {
	messageType gateway.MessageType
	mediaURLs   []string
	mediaTypes  []string
	msgtype     string
}

// Gated and normalised inbound message event, nil to drop
func (a *MatrixAdapter) buildInboundEvent(ctx context.Context, roomID, sender, eventID, body string, sourceContent,
	relatesTo map[string]any, mctx *messageContext, media *inboundMedia) (*gateway.MessageEvent, error) {
	if mctx == nil {
		var err error
		mctx, err = a.resolveMessageContext(ctx, roomID, sender, eventID, body, sourceContent, relatesTo, false, nil)
		if err != nil {
			return nil, err
		}
	}
	if mctx == nil {
		return nil, nil
	}
	body, replyTo, replyToText, replyToAuthorID, replyToAuthorName := a.extractReplyContext(ctx, roomID, mctx.body, relatesTo)
	messageType := gateway.MessageTypeText
	if media == nil {
		body = normalizeMatrixBangCommand(ctx, body)
		if strings.HasPrefix(body, "/") {
			messageType = gateway.MessageTypeCommand
		}
	} else {
		messageType = media.messageType
		if isBareMediaFilename(media.msgtype, body) {
			body = ""
		}
	}
	ev := gateway.NewMessageEvent(body)
	ev.Source = mctx.source
	ev.RawMessage = sourceContent
	ev.MessageID = &eventID
	ev.ReplyToMessageID = replyTo
	ev.ReplyToText = replyToText
	ev.ReplyToAuthorID = replyToAuthorID
	ev.ReplyToAuthorName = replyToAuthorName
	ev.UserID = &sender
	displayName := mctx.displayName
	ev.UserName = &displayName
	ev.MessageType = messageType
	if media != nil {
		ev.MediaURLs = media.mediaURLs
		ev.MediaTypes = media.mediaTypes
	}
	return ev, nil
}

// Text message path, claiming a parked voice for a bare mention and batching plain text
func (a *MatrixAdapter) handleTextMessage(ctx context.Context, roomID, sender, eventID string, eventTS float64,
	sourceContent, relatesTo map[string]any) error {
	body, _ := sourceContent["body"].(string)
	if body == "" {
		return nil
	}
	if a.parkedVoices.Pending(roomID, sender) && pystr.Strip(a.stripMention(body)) == "" && a.contentMentionsBot(body, sourceContent) {
		limit := a.parkedVoices.Mark()
		a.parkedVoices.Settle(ctx, roomID, sender)
		if parked, ok := a.parkedVoices.Claim(roomID, sender, limit); ok {
			if err := a.handleMediaMessage(ctx, roomID, sender, parked.EventID, eventTS, parked.Content, parked.RelatesTo, "m.audio", true); err != nil {
				return err
			}
			a.backgroundReadReceipt(ctx, roomID, eventID)
			return nil
		}
	}
	ev, err := a.buildInboundEvent(ctx, roomID, sender, eventID, normalizeMatrixBangCommand(ctx, body), sourceContent, relatesTo, nil, nil)
	if err != nil || ev == nil {
		return err
	}
	if delay, _ := a.TextBatchDelays(); ev.MessageType == gateway.MessageTypeText && delay > 0 {
		return a.EnqueueTextEvent(ctx, ev)
	}
	return a.HandleMessage(ctx, ev)
}

// Media message path gating before any download, then caching the file for downstream tools
func (a *MatrixAdapter) handleMediaMessage(ctx context.Context, roomID, sender, eventID string, eventTS float64,
	sourceContent, relatesTo map[string]any, msgtype string, mentionClaimed bool) error {
	body, _ := sourceContent["body"].(string)
	url := strOf(sourceContent["url"])
	if url != "" && !strings.HasPrefix(url, "mxc://") {
		slog.Warn(fmt.Sprintf("[Matrix] Rejecting inbound media %s with non-MXC URL", eventID))
		return nil
	}
	contentInfo := objectOf(sourceContent["info"])
	eventMimetype, _ := contentInfo["mimetype"].(string)
	var eventSize int64
	if v := contentInfo["size"]; pyTruthy(v) {
		if n, err := pyint.Int64(v); err == nil {
			eventSize = n
		}
	}
	if eventSize != 0 && eventSize > a.maxMediaBytes {
		slog.Warn(fmt.Sprintf("[Matrix] Rejecting oversized inbound media %s (%d > %d bytes)", eventID, eventSize, a.maxMediaBytes))
		return nil
	}
	fileContent, fileIsObject := sourceContent["file"].(map[string]any)
	if url == "" && fileIsObject {
		url = strOf(fileContent["url"])
		if url != "" && !strings.HasPrefix(url, "mxc://") {
			slog.Warn(fmt.Sprintf("[Matrix] Rejecting inbound encrypted media %s with non-MXC URL", eventID))
			return nil
		}
	}
	isEncryptedMedia := fileIsObject && len(fileContent) > 0 && pyTruthy(fileContent["url"])
	msgType, mediaType, isVoiceMessage := classifyInboundMedia(msgtype, eventMimetype, sourceContent)
	var gate *VoiceGate
	if a.voiceMayPark(roomID, body, sourceContent, relatesTo, mentionClaimed) {
		gate = a.parkedVoices.Begin(roomID, sender)
	}
	mctx, err := a.resolveMessageContext(ctx, roomID, sender, eventID, body, sourceContent, relatesTo, mentionClaimed, gate)
	if gate != nil {
		a.parkedVoices.Release(roomID, sender, gate)
	}
	if err != nil || mctx == nil {
		return err
	}
	cachedPath := ""
	if url != "" {
		var encrypted map[string]any
		if isEncryptedMedia {
			encrypted = fileContent
		}
		path, err := a.downloadAndCacheMedia(ctx, url, eventID, encrypted, msgType, mediaType, isVoiceMessage, body)
		if err != nil {
			slog.Warn(fmt.Sprintf("[Matrix] Failed to cache media: %v", err))
		}
		cachedPath = path
	}
	httpURL := ""
	if url != "" && !isEncryptedMedia {
		httpURL = a.mxcToHTTP(url)
	}
	media := &inboundMedia{messageType: msgType, msgtype: msgtype}
	if cachedPath != "" {
		media.mediaURLs = []string{cachedPath}
	} else if httpURL != "" {
		media.mediaURLs = []string{httpURL}
	}
	if media.mediaURLs != nil {
		media.mediaTypes = []string{mediaType}
	}
	ev, err := a.buildInboundEvent(ctx, roomID, sender, eventID, body, sourceContent, relatesTo, mctx, media)
	if err != nil || ev == nil {
		return err
	}
	return a.HandleMessage(ctx, ev)
}

// Message type, mime type and voice flag of a Matrix media msgtype
func classifyInboundMedia(msgtype, eventMimetype string, sourceContent map[string]any) (gateway.MessageType, string, bool) {
	or := func(v, def string) string {
		if v != "" {
			return v
		}
		return def
	}
	switch msgtype {
	case "m.image":
		return gateway.MessageTypePhoto, or(eventMimetype, "image/png"), false
	case "m.audio":
		if hasVoiceMarker(sourceContent) {
			return gateway.MessageTypeVoice, or(eventMimetype, "audio/ogg"), true
		}
		return gateway.MessageTypeAudio, or(eventMimetype, "audio/ogg"), false
	case "m.video":
		return gateway.MessageTypeVideo, or(eventMimetype, "video/mp4"), false
	}
	return gateway.MessageTypeDocument, or(eventMimetype, "application/octet-stream"), false
}

// Downloads media, decrypting it when encrypted file metadata is given, into the local cache
func (a *MatrixAdapter) downloadAndCacheMedia(ctx context.Context, url, eventID string, encryptedFile map[string]any,
	msgType gateway.MessageType, mediaType string, isVoiceMessage bool, body string) (string, error) {
	client := a.currentClient()
	if client == nil {
		return "", errors.New("'NoneType' object has no attribute 'download_media'")
	}
	uri, err := id.ParseContentURI(url)
	if err != nil {
		return "", err
	}
	fileBytes, err := client.DownloadBytes(ctx, uri)
	if err != nil {
		return "", err
	}
	if fileBytes == nil {
		return "", nil
	}
	if encryptedFile != nil {
		var hashValue, keyValue string
		if hashes, ok := encryptedFile["hashes"].(map[string]any); ok {
			hashValue = strOf(hashes["sha256"])
		}
		if key, ok := encryptedFile["key"].(map[string]any); ok {
			keyValue = strOf(key["k"])
		} else {
			keyValue = strOf(encryptedFile["key"])
		}
		ivValue := strOf(encryptedFile["iv"])
		if keyValue == "" || hashValue == "" || ivValue == "" {
			slog.Warn(fmt.Sprintf("[Matrix] Encrypted media event missing decryption metadata for %s", eventID))
			return "", nil
		}
		if fileBytes, err = decryptAttachment(fileBytes, keyValue, hashValue, ivValue); err != nil {
			return "", err
		}
	}
	switch msgType {
	case gateway.MessageTypePhoto:
		extMap := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp"}
		ext, ok := extMap[mediaType]
		if !ok {
			ext = ".jpg"
		}
		cachedPath, err := gateway.CacheImageFromBytesAsync(ctx, fileBytes, ext)
		if err != nil {
			return "", err
		}
		slog.Info(fmt.Sprintf("[Matrix] Cached user image at %s", cachedPath))
		return cachedPath, nil
	case gateway.MessageTypeAudio, gateway.MessageTypeVoice:
		name := body
		if name == "" {
			name = "audio.ogg"
			if isVoiceMessage {
				name = "voice.ogg"
			}
		}
		ext := pypath.PathSuffix(name)
		if ext == "" {
			ext = ".ogg"
		}
		return gateway.CacheAudioFromBytesAsync(ctx, fileBytes, ext)
	}
	filename := body
	if filename == "" {
		filename = "document"
		if msgType == gateway.MessageTypeVideo {
			filename = "video.mp4"
		}
	}
	return gateway.CacheDocumentFromBytesAsync(ctx, fileBytes, filename)
}

// Plaintext of an encrypted attachment checked against its SHA-256 hash
func decryptAttachment(ciphertext []byte, key, hash, iv string) ([]byte, error) {
	raw, err := json.Marshal(map[string]any{
		"key": map[string]any{"kty": "oct", "key_ops": []string{"encrypt", "decrypt"}, "alg": "A256CTR", "k": key, "ext": true},
		"iv":  iv, "hashes": map[string]any{"sha256": hash}, "v": "v2"})
	if err != nil {
		return nil, err
	}
	var ef attachment.EncryptedFile
	if err := json.Unmarshal(raw, &ef); err != nil {
		return nil, err
	}
	return ef.Decrypt(ciphertext)
}

// Synchronous pre-check of the park branch of message context resolution
func (a *MatrixAdapter) voiceMayPark(roomID, body string, content, relatesTo map[string]any, mentionClaimed bool) bool {
	if mentionClaimed || !a.requireMention || !isVoiceEvent(content) {
		return false
	}
	if a.freeRooms[roomID] || (len(a.allowedRooms) > 0 && !a.allowedRooms[roomID]) {
		return false
	}
	if threadID := threadRoot(relatesTo); threadID != "" && a.threads.Contains(threadID) {
		return false
	}
	return !strings.HasPrefix(body, "/") && !a.contentMentionsBot(body, content)
}

// Membership events whose membership is invite reach the invite handler
func (a *MatrixAdapter) onMemberEvent(ctx context.Context, evt *event.Event) error {
	if evt.Content.Raw["membership"] != "invite" {
		return nil
	}
	return a.onInvite(ctx, evt)
}

// Joins rooms an authorized user invited the bot to, recording declared DMs in m.direct
func (a *MatrixAdapter) onInvite(ctx context.Context, evt *event.Event) error {
	roomID := string(evt.RoomID)
	isDirect := pyTruthy(evt.Content.Raw["is_direct"])
	inviter := string(evt.Sender)
	if !a.isAuthorizedUser(inviter, roomID) {
		slog.Warn(fmt.Sprintf("Matrix: rejecting invite to %s from unauthorized user %s", roomID, inviter))
		return nil
	}
	slog.Info(fmt.Sprintf("Matrix: invited to %s — joining (is_direct=%s)", roomID, pyBool(isDirect)))
	a.scheduleInviteJoin(ctx, roomID, isDirect && inviter != "", inviter)
	return nil
}

// Python spelling of a bool
func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// Joins the room unless already joined, leaving dead invites, the context error when cancelled
func (a *MatrixAdapter) joinRoomByID(ctx context.Context, roomID string) (bool, error) {
	a.mu.Lock()
	joined := a.joinedRooms[roomID]
	a.mu.Unlock()
	if roomID == "" || joined {
		return roomID != "", nil
	}
	client := a.currentClient()
	if client == nil {
		slog.Warn(fmt.Sprintf("Matrix: error joining %s: 'NoneType' object has no attribute 'join_room'", roomID))
		return false, nil
	}
	if _, err := client.JoinRoomByID(ctx, id.RoomID(roomID)); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("Matrix: error joining %s: %v", roomID, err))
		msg := pystr.Lower(err.Error())
		if strings.Contains(msg, "no servers") || strings.Contains(msg, "room not found") {
			if _, lerr := client.LeaveRoom(ctx, id.RoomID(roomID)); lerr == nil {
				slog.Info(fmt.Sprintf("Matrix: declined dead invite to %s", roomID))
			}
		}
		return false, nil
	}
	a.mu.Lock()
	a.joinedRooms[roomID] = true
	a.invalidateRoomIdentitiesLocked(roomID)
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("Matrix: joined %s", roomID))
	a.refreshDMCache(ctx)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return true, nil
}

// Joins an invite on a background task within forty five seconds without blocking sync
func (a *MatrixAdapter) scheduleInviteJoin(ctx context.Context, roomID string, isDirect bool, inviter string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	existing := a.inviteJoinTasks[roomID]
	if roomID == "" || a.joinedRooms[roomID] || (existing != nil && !existing.isDone()) {
		return
	}
	var task *bgTask
	ready := make(chan struct{})
	task = startTask(context.WithoutCancel(ctx), func(tctx context.Context) {
		<-ready
		defer func() {
			a.mu.Lock()
			if a.inviteJoinTasks[roomID] == task {
				delete(a.inviteJoinTasks, roomID)
			}
			a.mu.Unlock()
		}()
		jctx, cancel := context.WithTimeout(tctx, 45*time.Second)
		joined, err := a.joinRoomByID(jctx, roomID)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && tctx.Err() == nil {
			slog.Warn(fmt.Sprintf("Matrix: timed out joining invite %s", roomID))
			return
		}
		if err != nil {
			return
		}
		if joined && isDirect && inviter != "" {
			a.recordDMRoom(tctx, roomID, inviter)
		}
	})
	a.inviteJoinTasks[roomID] = task
	close(ready)
}

// Joins rooms still listed under rooms.invite after dispatch, gated on the inviter allowlist
func (a *MatrixAdapter) schedulePendingInviteJoins(ctx context.Context, resp *mautrix.RespSync) {
	roomIDs := make([]string, 0, len(resp.Rooms.Invite))
	for roomID := range resp.Rooms.Invite {
		roomIDs = append(roomIDs, string(roomID))
	}
	slices.Sort(roomIDs)
	for _, roomID := range roomIDs {
		a.mu.Lock()
		joined := a.joinedRooms[roomID]
		a.mu.Unlock()
		if joined {
			continue
		}
		isDirect, inviter := a.extractInviteDMSignal(resp.Rooms.Invite[id.RoomID(roomID)])
		if !a.isAuthorizedUser(inviter, roomID) {
			slog.Warn(fmt.Sprintf("Matrix: rejecting invite to %s from unauthorized user %s", roomID, inviter))
			continue
		}
		slog.Info(fmt.Sprintf("Matrix: reconciling pending invite for %s (is_direct=%s)", roomID, pyBool(isDirect)))
		a.scheduleInviteJoin(ctx, roomID, isDirect, inviter)
	}
}

// Direct flag and inviter from the stripped member event for the bot in the invite state
func (a *MatrixAdapter) extractInviteDMSignal(invitedRoom *mautrix.SyncInvitedRoom) (bool, string) {
	a.mu.Lock()
	userID := a.userID
	a.mu.Unlock()
	if userID == "" || invitedRoom == nil {
		return false, ""
	}
	for _, evt := range invitedRoom.State.Events {
		if evt == nil || evt.Type.Type != "m.room.member" || evt.StateKey == nil || *evt.StateKey != userID {
			continue
		}
		content := evt.Content.Raw
		if content == nil || content["membership"] != "invite" {
			continue
		}
		return pyTruthy(content["is_direct"]), string(evt.Sender)
	}
	return false, ""
}
