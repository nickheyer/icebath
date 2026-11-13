package simplex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Message content types whose text field carries the message body
var textBearingTypes = map[string]bool{"text": true, "file": true, "image": true, "voice": true, "link": true, "video": true}

// Content types that carry an attachment and arrive without text
var attachmentTypes = map[string]bool{"image": true, "file": true, "voice": true, "video": true}

// Receive file statuses after which the daemon will never finish the download
var deadFileStatuses = map[string]bool{"rcvCancelled": true, "rcvError": true, "rcvAborted": true}

// Runs daemon events one at a time in arrival order, the serial receive loop the gateway expects
func (a *Adapter) processEvents(ctx context.Context) {
	defer a.wg.Done()
	queue := a.queue
	for {
		frame, ok := queue.pop(ctx)
		if !ok {
			return
		}
		a.handleFrame(ctx, frame)
	}
}

// Dispatches one daemon event by its type, a failing handler logged without stopping the loop
func (a *Adapter) handleFrame(ctx context.Context, frame map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error(fmt.Sprintf("[simplex] error handling daemon event: %v", r))
		}
	}()
	resp := respOf(frame)
	kind := str(resp, "type")
	if kind == "" {
		kind = str(frame, "type")
	}
	var err error
	switch kind {
	case "receivedContactRequest", "contactRequest":
		err = a.onContactRequest(ctx, resp)
	case "rcvFileDescrReady":
		a.onFileDescrReady(ctx, resp)
	case "newChatItems":
		items, isList := resp["chatItems"].([]any)
		if !isList {
			items = []any{resp["chatItems"]}
		}
		for _, raw := range items {
			if item, ok := raw.(map[string]any); ok {
				a.logItemError(a.handleChatItem(ctx, item, false))
			}
		}
	case "newChatItem":
		a.logItemError(a.handleChatItem(ctx, obj(resp, "chatItem"), false))
	case "rcvFileComplete":
		item := obj(resp, "chatItem")
		a.forgetRequested(idText(obj(obj(item, "chatItem"), "file")["fileId"]))
		a.logItemError(a.handleChatItem(ctx, item, false))
	case "rcvFileError", "rcvFileSndCancelled":
		item := obj(resp, "chatItem_")
		if len(item) == 0 {
			item = obj(resp, "chatItem")
		}
		a.forgetRequested(idText(obj(resp, "rcvFileTransfer")["fileId"]))
		if len(item) > 0 {
			slog.Warn(fmt.Sprintf("[simplex] attachment transfer ended with %s, delivering the message without it", kind))
			a.logItemError(a.handleChatItem(ctx, item, true))
		}
	case "":
	default:
		slog.Debug("[simplex] unhandled event type: " + kind)
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("[simplex] %s handling failed: %v", kind, err))
	}
}

// Logs a chat item that could not be turned into a gateway message
func (a *Adapter) logItemError(err error) {
	if err != nil {
		slog.Error(fmt.Sprintf("[simplex] error processing chat item: %v", err))
	}
}

// Accepts an incoming contact request when auto accept is on
func (a *Adapter) onContactRequest(ctx context.Context, resp map[string]any) error {
	if !a.autoAccept {
		slog.Debug("[simplex] contact request left for manual acceptance (SIMPLEX_AUTO_ACCEPT is off)")
		return nil
	}
	reqID := idText(obj(resp, "contactRequest")["contactRequestId"])
	if reqID == "" {
		return nil
	}
	c := a.liveClient()
	if c == nil {
		return errNotConnected
	}
	slog.Info("[simplex] auto-accepting contact request " + redactID(reqID))
	_, err := c.command(ctx, "/_accept "+reqID, commandTimeout)
	return err
}

// Starts an XFTP download as soon as its description is ready, the chat item arriving later
func (a *Adapter) onFileDescrReady(ctx context.Context, resp map[string]any) {
	if !a.chatAllowed(obj(obj(resp, "chatItem"), "chatInfo")) {
		return
	}
	fileID := idText(obj(resp, "rcvFileTransfer")["fileId"])
	if fileID == "" || a.requested[fileID] {
		return
	}
	slog.Debug("[simplex] rcvFileDescrReady for fileId=" + fileID + ", accepting transfer")
	if err := a.receiveFile(ctx, fileID); err != nil && !alreadyReceiving(err) {
		slog.Warn(fmt.Sprintf("[simplex] accepting file %s failed: %v", fileID, err))
	}
}

// Asks the daemon to download a file once per file id
func (a *Adapter) receiveFile(ctx context.Context, fileID string) error {
	c := a.liveClient()
	if c == nil {
		return errNotConnected
	}
	a.requested[fileID] = true
	a.requestedOrder = append(a.requestedOrder, fileID)
	if len(a.requestedOrder) > requestedFilesCap {
		delete(a.requested, a.requestedOrder[0])
		a.requestedOrder = a.requestedOrder[1:]
	}
	_, err := c.command(ctx, "/freceive "+fileID, commandTimeout)
	return err
}

// Drops a file id once its transfer finished
func (a *Adapter) forgetRequested(fileID string) {
	if fileID != "" {
		delete(a.requested, fileID)
	}
}

// Whether a daemon error reports the file is already downloading
func alreadyReceiving(err error) bool {
	var de *daemonError
	return errors.As(err, &de) && de.kind == "fileAlreadyReceiving"
}

// Whether the chat is a direct chat or a group the allowlist admits
func (a *Adapter) chatAllowed(chatInfo map[string]any) bool {
	if str(chatInfo, "type") != "group" {
		return true
	}
	id := idText(obj(chatInfo, "groupInfo")["groupId"])
	return len(a.groupAllow) > 0 && (a.groupAllow["*"] || a.groupAllow[id])
}

// Mime type of an attachment extension, image and audio by extension and octet stream otherwise
func mimeForExt(ext string) string {
	switch {
	case imageExts[ext]:
		return "image/" + strings.TrimPrefix(ext, ".")
	case audioExts[ext]:
		return "audio/" + strings.TrimPrefix(ext, ".")
	}
	return "application/octet-stream"
}

// Turns a received chat item into a gateway message, holding attachments back until the daemon stored them
func (a *Adapter) handleChatItem(ctx context.Context, item map[string]any, transferFailed bool) error {
	chatInfo := obj(item, "chatInfo")
	data := obj(item, "chatItem")
	direction := str(obj(data, "chatDir"), "type")
	if direction == "directSnd" || direction == "groupSnd" {
		return nil
	}
	content := obj(data, "content")
	if str(content, "type") != "rcvMsgContent" {
		return nil
	}
	msgContent := obj(content, "msgContent")
	msgType := str(msgContent, "type")
	text := ""
	if textBearingTypes[msgType] {
		text = str(msgContent, "text")
	}
	if text == "" && !attachmentTypes[msgType] {
		return nil
	}
	chatType := str(chatInfo, "type")
	var chatID, chatName, senderID, senderName string
	switch chatType {
	case "direct":
		contact := obj(chatInfo, "contact")
		chatID = idText(contact["contactId"])
		senderID = chatID
		senderName = displayName(contact, "profile")
		chatName = senderName
	case "group":
		groupInfo := obj(chatInfo, "groupInfo")
		groupID := idText(groupInfo["groupId"])
		chatID = "group:" + groupID
		member := obj(obj(data, "chatDir"), "groupMember")
		senderID = str(member, "memberId")
		senderName = displayName(member, "memberProfile")
		chatName = displayName(groupInfo, "groupProfile")
		if chatName == "" {
			chatName = chatID
		}
		if len(a.groupAllow) == 0 {
			slog.Debug("[simplex] ignoring group message (no SIMPLEX_GROUP_ALLOWED)")
			return nil
		}
		if !a.groupAllow["*"] && !a.groupAllow[groupID] {
			slog.Debug("[simplex] group " + redactID(groupID) + " not in allowlist")
			return nil
		}
	default:
		slog.Debug("[simplex] unhandled chat type: " + chatType)
		return nil
	}
	if senderID == "" {
		slog.Debug("[simplex] ignoring message with no sender")
		return nil
	}
	var mediaURLs, mediaTypes []string
	if file, hasFile := data["file"].(map[string]any); hasFile && !transferFailed {
		path := str(obj(file, "fileSource"), "filePath")
		fileID := idText(file["fileId"])
		ext := strings.ToLower(filepath.Ext(path))
		if ext == "" {
			ext = strings.ToLower(filepath.Ext(str(file, "fileName")))
		}
		if path == "" && fileID != "" {
			status := str(obj(file, "fileStatus"), "type")
			if !deadFileStatuses[status] {
				return a.awaitAttachment(ctx, item, fileID, status)
			}
			slog.Warn(fmt.Sprintf("[simplex] attachment %s is %s, delivering the message without it", fileID, status))
		}
		if path != "" {
			mediaURLs = append(mediaURLs, path)
			mediaTypes = append(mediaTypes, mimeForExt(ext))
		}
	}
	if text == "" && len(mediaURLs) == 0 {
		slog.Warn("[simplex] dropping a message whose only content was an attachment that never arrived")
		return nil
	}
	msgKind := gateway.MessageTypeText
	if len(mediaTypes) > 0 {
		switch {
		case strings.HasPrefix(mediaTypes[0], "audio/"):
			msgKind = gateway.MessageTypeVoice
		case strings.HasPrefix(mediaTypes[0], "image/"):
			msgKind = gateway.MessageTypePhoto
		default:
			msgKind = gateway.MessageTypeDocument
		}
	}
	sourceType := "dm"
	if chatType == "group" {
		sourceType = "group"
	}
	userName := senderName
	if userName == "" {
		userName = senderID
	}
	source := a.BuildSource(ctx, chatID, gateway.SourceFields{ChatName: &chatName, ChatType: sourceType, UserID: &senderID, UserName: &userName})
	event := gateway.NewMessageEvent(text)
	event.MessageType, event.Source, event.RawMessage = msgKind, source, item
	event.MediaURLs, event.MediaTypes = mediaURLs, mediaTypes
	event.Timestamp = itemTime(obj(data, "meta"))
	preview := text
	if runes := []rune(preview); len(runes) > 50 {
		preview = string(runes[:50])
	}
	slog.Debug(fmt.Sprintf("[simplex] message from %s in %s: %s", redactID(senderID), chatID, preview))
	if msgKind == gateway.MessageTypeText {
		return a.EnqueueTextEvent(ctx, event)
	}
	return a.HandleMessage(ctx, event)
}

// Requests the download of a pending attachment so rcvFileComplete delivers the message with it
func (a *Adapter) awaitAttachment(ctx context.Context, item map[string]any, fileID, status string) error {
	if status != "rcvInvitation" || a.requested[fileID] {
		slog.Debug("[simplex] attachment " + fileID + " still downloading, waiting for rcvFileComplete")
		return nil
	}
	slog.Info("[simplex] attachment " + fileID + " not yet received, accepting transfer")
	err := a.receiveFile(ctx, fileID)
	if err == nil || alreadyReceiving(err) {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	a.forgetRequested(fileID)
	slog.Warn(fmt.Sprintf("[simplex] accepting attachment %s failed: %v, delivering the message without it", fileID, err))
	return a.handleChatItem(ctx, item, true)
}

// Item timestamp from its meta, now when missing or unparseable
func itemTime(meta map[string]any) time.Time {
	raw := str(meta, "itemTs")
	if raw == "" {
		raw = str(meta, "createdAt")
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t
	}
	return time.Now().UTC()
}
