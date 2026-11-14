package yuanbao

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const textChunkRetries = 3

// Extensions a direct send delivers as an image message
var senderImageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true}

// Chunks within the limit split at paragraph boundaries and never inside a fence or a table
func chunkMarkdownText(text string, maxChars int) []string {
	return gateway.SplitTextFenceAware(text, maxChars, nil, true, false)
}

// Bytes, name and MIME type of a local file, a ValueError when the path names no file
func readLocalFile(adapter *YuanbaoAdapter, label, path, defaultName, filename string) ([]byte, string, string, error) {
	if !isFile(path) {
		return nil, "", "", valueError("File not found: " + path)
	}
	slog.Info(fmt.Sprintf("[%s] %s: reading %s", adapter.Name(), label, path))
	fileBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, "", "", &pyErr{pypath.OSErrorClass(err), pypath.OSErrorText(err)}
	}
	if filename == "" {
		filename = pypath.Basename(path)
	}
	if filename == "" {
		filename = defaultName
	}
	return fileBytes, filename, GuessMIMEType(filename), nil
}

// Media send strategy, how the bytes are acquired and how the message body is built from the upload
type mediaSendHandler interface {
	className() string
	needsCOSUpload() bool
	acquireFile(ctx context.Context, adapter *YuanbaoAdapter, kwargs map[string]any) ([]byte, string, string, error)
	buildMsgBody(upload *pyjson.Object, kwargs map[string]any) ([]any, error)
}

// Text keyword argument of a media send, empty when it is absent or not text
func kwargText(kwargs map[string]any, key string) string {
	text, _ := kwargs[key].(string)
	return text
}

// Value of a key the upload credentials must carry, the KeyError a missing key raises
func requiredKey(obj *pyjson.Object, key string) (any, error) {
	value, ok := obj.Get(key)
	if !ok {
		return nil, &KeyError{Key: key}
	}
	return value, nil
}

// Integer field of the upload result, zero when it is absent
func uploadInt(upload *pyjson.Object, key string) int64 {
	n, _ := upload.GetOr(key, int64(0)).(int64)
	return n
}

// TIMImageElem body of an uploaded image
func imageMsgBody(upload *pyjson.Object, kwargs map[string]any) ([]any, error) {
	urlText, err := protoText(upload.GetOr("url", ""))
	if err != nil {
		return nil, err
	}
	return BuildImageMsgBody(urlText, kwargText(kwargs, "file_uuid"), kwargText(kwargs, "filename"), uploadInt(upload, "size"),
		uploadInt(upload, "width"), uploadInt(upload, "height"), kwargText(kwargs, "content_type")), nil
}

// Image downloaded from a URL, uploaded and sent as a TIMImageElem
type imageURLHandler struct{}

func (imageURLHandler) className() string    { return "ImageUrlHandler" }
func (imageURLHandler) needsCOSUpload() bool { return true }
func (imageURLHandler) buildMsgBody(upload *pyjson.Object, kwargs map[string]any) ([]any, error) {
	return imageMsgBody(upload, kwargs)
}

func (imageURLHandler) acquireFile(ctx context.Context, adapter *YuanbaoAdapter, kwargs map[string]any) ([]byte, string, string, error) {
	imageURL := kwargText(kwargs, "image_url")
	slog.Info(fmt.Sprintf("[%s] ImageUrlHandler: downloading %s", adapter.Name(), imageURL))
	fileBytes, contentType, err := DownloadURL(ctx, gateway.URLSafetyEnv(ctx), imageURL, mediaMaxSizeMB)
	if err != nil {
		return nil, "", "", err
	}
	pathPart, _ := pystrPartition(imageURL, "?")
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = GuessMIMEType(pathPart)
	}
	filename := pypath.Basename(pathPart)
	if filename == "" {
		filename = "image.jpg"
	}
	return fileBytes, filename, contentType, nil
}

// Image read from a local path, uploaded and sent as a TIMImageElem
type imageFileHandler struct{}

func (imageFileHandler) className() string    { return "ImageFileHandler" }
func (imageFileHandler) needsCOSUpload() bool { return true }
func (imageFileHandler) buildMsgBody(upload *pyjson.Object, kwargs map[string]any) ([]any, error) {
	return imageMsgBody(upload, kwargs)
}

func (imageFileHandler) acquireFile(ctx context.Context, adapter *YuanbaoAdapter, kwargs map[string]any) ([]byte, string, string, error) {
	return readLocalFile(adapter, "ImageFileHandler", kwargText(kwargs, "image_path"), "image.jpg", "")
}

// Local file uploaded and sent as a TIMFileElem
type documentHandler struct{}

func (documentHandler) className() string    { return "DocumentHandler" }
func (documentHandler) needsCOSUpload() bool { return true }

func (documentHandler) acquireFile(ctx context.Context, adapter *YuanbaoAdapter, kwargs map[string]any) ([]byte, string, string, error) {
	return readLocalFile(adapter, "DocumentHandler", kwargText(kwargs, "file_path"), "document", kwargText(kwargs, "filename"))
}

func (documentHandler) buildMsgBody(upload *pyjson.Object, kwargs map[string]any) ([]any, error) {
	urlText, err := protoText(upload.GetOr("url", ""))
	if err != nil {
		return nil, err
	}
	return BuildFileMsgBody(urlText, kwargText(kwargs, "filename"), kwargText(kwargs, "file_uuid"), uploadInt(upload, "size")), nil
}

// Sticker sent as a TIMFaceElem with no upload
type stickerHandler struct{}

func (stickerHandler) className() string    { return "StickerHandler" }
func (stickerHandler) needsCOSUpload() bool { return false }

func (stickerHandler) acquireFile(ctx context.Context, adapter *YuanbaoAdapter, kwargs map[string]any) ([]byte, string, string, error) {
	return []byte{}, "sticker", "application/octet-stream", nil
}

// Body of the named sticker, else of the face index, else of a random sticker
func (stickerHandler) buildMsgBody(upload *pyjson.Object, kwargs map[string]any) ([]any, error) {
	if name, named := kwargs["sticker_name"].(string); named {
		sticker := GetStickerByName(name)
		if sticker == nil {
			return nil, valueError("Sticker not found: " + pyrepr.StrRepr(name))
		}
		return BuildStickerMsgBody(sticker)
	}
	if faceIndex, indexed := kwargs["face_index"].(int); indexed {
		return BuildFaceMsgBody(faceIndex, nil), nil
	}
	return BuildStickerMsgBody(GetRandomSticker(""))
}

// Error text of a file the upload refuses, empty for a file within the size cap
func validateMedia(fileBytes []byte, filename string) string {
	if len(fileBytes) == 0 {
		return "Empty file: " + filename
	}
	if len(fileBytes) > mediaMaxSizeMB*1024*1024 {
		return fmt.Sprintf("File too large: %s (%.1fMB > %dMB)", filename, float64(len(fileBytes))/1024/1024, mediaMaxSizeMB)
	}
	return ""
}

// Message body of a media send, the file uploaded to COS first for a handler that needs it
func mediaMsgBody(ctx context.Context, handler mediaSendHandler, adapter *YuanbaoAdapter, kwargs map[string]any) ([]any, string, error) {
	fileBytes, filename, contentType, err := handler.acquireFile(ctx, adapter, kwargs)
	if err != nil {
		return nil, "", err
	}
	if !handler.needsCOSUpload() {
		body, err := handler.buildMsgBody(pyjson.NewObject(), kwargs)
		return body, "", err
	}
	if refusal := validateMedia(fileBytes, filename); refusal != "" {
		return nil, refusal, nil
	}
	tokenData, err := adapter.getCachedToken(ctx)
	if err != nil {
		return nil, "", err
	}
	botID := textValue(pyrepr.Or(pyrepr.Or(dictGetOr(tokenData, "bot_id", ""), adapter.botIDValue()), ""))
	credentials, err := GetCOSCredentials(ctx, adapter.appKey, adapter.apiDomain, textValue(dictGetOr(tokenData, "token", "")), filename,
		nil, botID, adapter.routeEnv)
	if err != nil {
		return nil, "", err
	}
	bucket, err := requiredKey(credentials, "bucketName")
	if err != nil {
		return nil, "", err
	}
	region, err := requiredKey(credentials, "region")
	if err != nil {
		return nil, "", err
	}
	upload, err := UploadToCOS(ctx, fileBytes, filename, contentType, credentials, pyrepr.Str(bucket), pyrepr.Str(region))
	if err != nil {
		return nil, "", err
	}
	bodyKwargs := make(map[string]any, len(kwargs)+3)
	for key, value := range kwargs {
		bodyKwargs[key] = value
	}
	bodyKwargs["file_uuid"], bodyKwargs["filename"], bodyKwargs["content_type"] = MD5Hex(fileBytes), filename, contentType
	body, err := handler.buildMsgBody(upload, bodyKwargs)
	return body, "", err
}

// Shared media send flow, the file acquired, uploaded when needed and its body dispatched under the chat lock
func handleMediaSend(ctx context.Context, handler mediaSendHandler, adapter *YuanbaoAdapter, chatID, replyTo, caption string, kwargs map[string]any) (gateway.SendResult, error) {
	if adapter.connection.socket() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected", Retryable: true}, nil
	}
	adapter.outbound.slowNotifier.cancel(chatID)
	result, err := func() (gateway.SendResult, error) {
		msgBody, refusal, err := mediaMsgBody(ctx, handler, adapter, kwargs)
		if err != nil {
			return gateway.SendResult{}, err
		}
		if refusal != "" {
			return gateway.SendResult{Success: false, Error: refusal}, nil
		}
		if caption != "" {
			msgBody = append(msgBody, textElem(caption))
		}
		return adapter.outbound.sender.dispatchMsgBody(ctx, chatID, msgBody, replyTo, dictGetOr(kwargs, "group_code", ""))
	}()
	switch {
	case err == nil:
		return result, nil
	case ctx.Err() != nil:
		return gateway.SendResult{}, ctx.Err()
	case isValueError(err):
		return gateway.SendResult{Success: false, Error: errText(err)}, nil
	}
	slog.Error(fmt.Sprintf("[%s] %s.handle() failed: %s", adapter.Name(), handler.className(), errText(err)), "error", err)
	return gateway.SendResult{Success: false, Error: errTextOrName(err)}, nil
}

// heartbeatManager keeps the reply heartbeat of each chat running every two seconds until thirty idle seconds pass
type heartbeatManager struct {
	adapter *YuanbaoAdapter

	mu         sync.Mutex
	tasks      map[string]*gateway.AdapterTask
	lastActive map[string]float64
}

func newHeartbeatManager(adapter *YuanbaoAdapter) *heartbeatManager {
	return &heartbeatManager{adapter: adapter, tasks: map[string]*gateway.AdapterTask{}, lastActive: map[string]float64{}}
}

// Whether a socket is held and the bot id is known
func (h *heartbeatManager) ready() bool {
	return h.adapter.connection.socket() != nil && pyrepr.Truthy(h.adapter.botIDValue())
}

// Sends one running or finish heartbeat to the chat, a failure only logged and a cancelled caller sending nothing
func (h *heartbeatManager) sendHeartbeatOnce(ctx context.Context, chatID string, heartbeat int64) {
	adapter := h.adapter
	if !h.ready() || ctx.Err() != nil {
		return
	}
	err := func() error {
		fromAccount, err := protoText(adapter.botIDValue())
		if err != nil {
			return err
		}
		var encoded []byte
		if strings.HasPrefix(chatID, "group:") {
			encoded, err = EncodeSendGroupHeartbeat(fromAccount, chatID[len("group:"):], heartbeat, 0)
		} else {
			encoded, err = EncodeSendPrivateHeartbeat(fromAccount, strings.TrimPrefix(chatID, "direct:"), heartbeat)
		}
		if err != nil {
			return err
		}
		ws := adapter.connection.socket()
		if ws == nil {
			return attrError(nil, "send")
		}
		return ws.SendBytes(encoded)
	}()
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] send_heartbeat_once failed: %s", adapter.Name(), errText(err)))
		return
	}
	state := "FINISH"
	if heartbeat == WSHeartbeatRunning {
		state = "RUNNING"
	}
	slog.Debug(fmt.Sprintf("[%s] Reply heartbeat %s sent: chat=%s", adapter.Name(), state, chatID))
}

// Starts the periodic running sender of a chat or renews the one already running
func (h *heartbeatManager) start(ctx context.Context, chatID string) {
	if !h.ready() {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastActive[chatID] = wallClock()
	if existing := h.tasks[chatID]; existing == nil || existing.Done() {
		h.tasks[chatID] = gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error { return h.worker(tctx, chatID) })
	}
}

// Sends running until the chat goes unrenewed or the socket is lost, then finish unless the worker was cancelled
func (h *heartbeatManager) worker(ctx context.Context, chatID string) error {
	h.sendHeartbeatOnce(ctx, chatID, WSHeartbeatRunning)
	for sleep(ctx, replyHeartbeatIntervalS) == nil {
		h.mu.Lock()
		lastActive := h.lastActive[chatID]
		h.mu.Unlock()
		if wallClock()-lastActive > replyHeartbeatTimeoutS || h.adapter.connection.socket() == nil {
			break
		}
		h.sendHeartbeatOnce(ctx, chatID, WSHeartbeatRunning)
	}
	h.sendHeartbeatOnce(ctx, chatID, WSHeartbeatFinish)
	h.mu.Lock()
	delete(h.tasks, chatID)
	delete(h.lastActive, chatID)
	h.mu.Unlock()
	return nil
}

// Stops the running sender of a chat without a finish, the send that follows emits it after delivery
func (h *heartbeatManager) stop(ctx context.Context, chatID string) {
	h.mu.Lock()
	task := h.tasks[chatID]
	delete(h.tasks, chatID)
	h.mu.Unlock()
	gateway.CancelTask(ctx, task)
}

// Cancels every running sender and forgets the renewal times
func (h *heartbeatManager) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, task := range h.tasks {
		if !task.Done() {
			task.Cancel()
		}
	}
	clear(h.tasks)
	clear(h.lastActive)
}

// slowResponseNotifier sends a wait notice to a chat whose agent stays silent past the slow response timeout
type slowResponseNotifier struct {
	adapter *YuanbaoAdapter
	sender  *messageSender

	mu    sync.Mutex
	tasks map[string]*gateway.AdapterTask
}

func newSlowResponseNotifier(adapter *YuanbaoAdapter, sender *messageSender) *slowResponseNotifier {
	return &slowResponseNotifier{adapter: adapter, sender: sender, tasks: map[string]*gateway.AdapterTask{}}
}

// Restarts the timer of a chat
func (n *slowResponseNotifier) start(ctx context.Context, chatID string) {
	n.cancel(chatID)
	task := gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error { return n.notify(tctx, chatID) })
	n.mu.Lock()
	n.tasks[chatID] = task
	n.mu.Unlock()
}

// Waits out the timeout and sends the localized wait notice unless cancelled first
func (n *slowResponseNotifier) notify(ctx context.Context, chatID string) error {
	if err := sleep(ctx, slowResponseTimeoutS); err != nil {
		return nil
	}
	slog.Info(fmt.Sprintf("[%s] Agent response exceeded %ds for %s, sending wait notice", n.adapter.Name(), int(slowResponseTimeoutS), chatID))
	_, _ = n.sender.sendTextChunk(ctx, chatID, slowResponseMessage(ctx), "", "")
	return nil
}

// Cancels the timer of a chat
func (n *slowResponseNotifier) cancel(chatID string) {
	n.mu.Lock()
	task := n.tasks[chatID]
	delete(n.tasks, chatID)
	n.mu.Unlock()
	if task != nil && !task.Done() {
		task.Cancel()
	}
}

// Cancels every timer
func (n *slowResponseNotifier) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, task := range n.tasks {
		if !task.Done() {
			task.Cancel()
		}
	}
	clear(n.tasks)
}

// Outcome of one message body send, the message key on success and the error text on failure
type sendOutcome struct {
	success bool
	msgKey  any
	errText string
}

// Send result of an outcome
func (o sendOutcome) sendResult() gateway.SendResult {
	if o.success {
		return gateway.SendResult{Success: true, MessageID: o.msgKey}
	}
	return gateway.SendResult{Success: false, Error: o.errText}
}

// messageSender dispatches outbound text and media, one chat at a time, chunking text and retrying each chunk
type messageSender struct {
	adapter *YuanbaoAdapter

	mu        sync.Mutex
	chatLocks *pydict.Dict[string, *chatLock]
}

func newMessageSender(adapter *YuanbaoAdapter) *messageSender {
	return &messageSender{adapter: adapter, chatLocks: pydict.New[string, *chatLock]()}
}

// Lock of a chat, the least recently used unlocked entry evicted at capacity
func (s *messageSender) getChatLock(chatID string) *chatLock {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lock, known := s.chatLocks.Get(chatID); known {
		s.chatLocks.Delete(chatID)
		s.chatLocks.Set(chatID, lock)
		return lock
	}
	if s.chatLocks.Len() >= chatDictMaxSize {
		keys := s.chatLocks.Keys()
		victim := keys[0]
		for _, key := range keys {
			if lock, _ := s.chatLocks.Get(key); !lock.locked() {
				victim = key
				break
			}
		}
		s.chatLocks.Delete(victim)
	}
	lock := newChatLock()
	s.chatLocks.Set(chatID, lock)
	return lock
}

// Sends text in chunks under the chat lock, then the finish heartbeat once every chunk was delivered
func (s *messageSender) sendText(ctx context.Context, chatID, content, replyTo string, groupCode any) (gateway.SendResult, error) {
	adapter := s.adapter
	if adapter.connection.socket() == nil {
		return gateway.SendResult{Success: false, Error: "Not connected", Retryable: true}, nil
	}
	adapter.outbound.slowNotifier.cancel(chatID)
	lock := s.getChatLock(chatID)
	if err := lock.acquire(ctx); err != nil {
		return gateway.SendResult{}, err
	}
	failed, err := func() (*gateway.SendResult, error) {
		defer lock.release()
		contentToSend := stripCronWrapper(content)
		chunks := truncateMessage(contentToSend, maxTextChunk)
		sizes := make([]any, len(chunks))
		for i, chunk := range chunks {
			sizes[i] = int64(pystr.Len(chunk))
		}
		slog.Info(fmt.Sprintf("[%s] truncate_message: input=%d chars, max=%d, output=%d chunk(s) sizes=%s", adapter.Name(),
			pystr.Len(contentToSend), maxTextChunk, len(chunks), pyrepr.Repr(sizes)))
		for i, chunk := range chunks {
			chunkReplyTo := ""
			if i == 0 {
				chunkReplyTo = replyTo
			}
			result, err := s.sendTextChunk(ctx, chatID, chunk, chunkReplyTo, groupCode)
			if err != nil {
				return nil, err
			}
			if !result.Success {
				return &result, nil
			}
		}
		return nil, nil
	}()
	if err != nil {
		return gateway.SendResult{}, err
	}
	if failed != nil {
		return *failed, nil
	}
	adapter.outbound.heartbeat.sendHeartbeatOnce(ctx, chatID, WSHeartbeatFinish)
	return gateway.SendResult{Success: true}, nil
}

// Sends text, then each media file as an image or a document by its extension, for the send message tool
func (s *messageSender) sendDirect(ctx context.Context, chatID, message string, mediaFiles []gateway.MediaRef) (map[string]any, error) {
	adapter := s.adapter
	var last *gateway.SendResult
	if pystr.Strip(message) != "" {
		result, err := adapter.Send(ctx, chatID, message, "", nil)
		if err != nil {
			return nil, err
		}
		if !result.Success {
			return map[string]any{"error": "Yuanbao send failed: " + pyrepr.Str(result.Error)}, nil
		}
		last = &result
	}
	for _, media := range mediaFiles {
		var result gateway.SendResult
		var err error
		if senderImageExts[pystr.Lower(pypath.PathSuffix(media.Path))] {
			result, err = adapter.SendImageFile(ctx, chatID, media.Path, "", "", nil, nil)
		} else {
			result, err = adapter.SendDocument(ctx, chatID, media.Path, "", "", "", nil, nil)
		}
		if err != nil {
			return nil, err
		}
		if !result.Success {
			return map[string]any{"error": "Yuanbao media send failed: " + pyrepr.Str(result.Error)}, nil
		}
		last = &result
	}
	if last == nil {
		return map[string]any{"error": "No deliverable text or media remained after processing"}, nil
	}
	return map[string]any{"success": true, "platform": "yuanbao", "chat_id": chatID, "message_id": last.MessageID}, nil
}

// Dispatches a message body to the chat under its lock
func (s *messageSender) dispatchMsgBody(ctx context.Context, chatID string, msgBody []any, replyTo string, groupCode any) (gateway.SendResult, error) {
	lock := s.getChatLock(chatID)
	if err := lock.acquire(ctx); err != nil {
		return gateway.SendResult{}, err
	}
	defer lock.release()
	outcome, err := s.sendMsgBody(ctx, chatID, msgBody, replyTo, groupCode)
	if err != nil {
		return gateway.SendResult{}, err
	}
	return outcome.sendResult(), nil
}

// Sends one text chunk, retrying a failure after one then two seconds
func (s *messageSender) sendTextChunk(ctx context.Context, chatID, text, replyTo string, groupCode any) (gateway.SendResult, error) {
	name := s.adapter.Name()
	lastError := "Unknown error"
	for attempt := 0; attempt < textChunkRetries; attempt++ {
		outcome, err := func() (sendOutcome, error) {
			msgBody := []any{textElem(text)}
			if strings.HasPrefix(chatID, "group:") {
				mentioned, err := s.buildMsgBodyWithMentions(text, chatID[len("group:"):])
				if err != nil {
					return sendOutcome{}, err
				}
				msgBody = mentioned
			}
			return s.sendMsgBody(ctx, chatID, msgBody, replyTo, groupCode)
		}()
		switch {
		case err != nil && ctx.Err() != nil:
			return gateway.SendResult{}, ctx.Err()
		case err != nil:
			lastError = errText(err)
			slog.Warn(fmt.Sprintf("[%s] send_text_chunk attempt %d/%d exception: %s", name, attempt+1, textChunkRetries, lastError))
		case outcome.success:
			return outcome.sendResult(), nil
		default:
			lastError = outcome.errText
			slog.Warn(fmt.Sprintf("[%s] send_text_chunk attempt %d/%d failed: %s", name, attempt+1, textChunkRetries, lastError))
		}
		if attempt < textChunkRetries-1 {
			if err := sleep(ctx, float64(int(1)<<attempt)); err != nil {
				return gateway.SendResult{}, err
			}
		}
	}
	slog.Error(fmt.Sprintf("[%s] send_text_chunk max retries (%d) exceeded. Last error: %s", name, textChunkRetries, lastError))
	return gateway.SendResult{Success: false, Error: "Max retries exceeded: " + lastError}, nil
}

// Routes a message body to the group of a group chat id, else to the account of a direct one
func (s *messageSender) sendMsgBody(ctx context.Context, chatID string, msgBody []any, replyTo string, groupCode any) (sendOutcome, error) {
	if strings.HasPrefix(chatID, "group:") {
		return s.sendGroupMsgBody(ctx, chatID[len("group:"):], msgBody, replyTo)
	}
	return s.sendC2CMsgBody(ctx, strings.TrimPrefix(chatID, "direct:"), msgBody, groupCode)
}

// Message body of a group text with each mention of a cached member as a mention elem
func (s *messageSender) buildMsgBodyWithMentions(text, groupCode string) ([]any, error) {
	members := s.adapter.cachedMembers(groupCode)
	if len(members) == 0 {
		return []any{textElem(text)}, nil
	}
	type mention struct {
		nick string
		uid  any
	}
	byNick := map[string]mention{}
	for _, member := range members {
		fields, err := dictOf(member)
		if err != nil {
			return nil, err
		}
		nick := pyrepr.Or(pyrepr.Or(fields["nickname"], fields["nick_name"]), "")
		uid := pyrepr.Or(fields["user_id"], "")
		if !pyrepr.Truthy(nick) || !pyrepr.Truthy(uid) {
			continue
		}
		nickText, err := strMethod(nick, "lower")
		if err != nil {
			return nil, err
		}
		byNick[pystr.Lower(nickText)] = mention{nickText, uid}
	}
	var msgBody []any
	last := 0
	for _, match := range findAll(atUserRE, text) {
		start, length := match.ByteRange()
		if segment := pystr.Strip(text[last:start]); segment != "" {
			msgBody = append(msgBody, textElem(segment))
		}
		nickname := match.GroupByNumber(1).String()
		if member, known := byNick[pystr.Lower(nickname)]; known {
			data, err := pyjson.Dumps(pyjson.ObjectOf("elem_type", int64(1002), "text", "@"+member.nick, "user_id", member.uid))
			if err != nil {
				return nil, err
			}
			msgBody = append(msgBody, pyjson.ObjectOf("msg_type", "TIMCustomElem", "msg_content", pyjson.ObjectOf("data", data)))
		} else {
			msgBody = append(msgBody, textElem("@"+nickname))
		}
		last = start + length
	}
	if tail := pystr.Strip(text[last:]); tail != "" {
		msgBody = append(msgBody, textElem(tail))
	}
	if len(msgBody) == 0 {
		return []any{textElem(text)}, nil
	}
	return msgBody, nil
}

// Sends a message body to an account, the group code marking a direct message that started in a group
func (s *messageSender) sendC2CMsgBody(ctx context.Context, toAccount string, msgBody []any, groupCode any) (sendOutcome, error) {
	reqID := fmt.Sprintf("c2c_%d", NextSeqNo())
	fromAccount, err := protoText(s.adapter.botIDValue())
	if err != nil {
		return sendOutcome{}, err
	}
	group, err := protoText(groupCode)
	if err != nil {
		return sendOutcome{}, err
	}
	encoded, err := EncodeSendC2CMessage(toAccount, msgBody, fromAccount, reqID, 0, nil, group, "")
	if err != nil {
		return sendOutcome{}, err
	}
	return dispatchEncoded(ctx, s.adapter, encoded, reqID)
}

// Sends a message body to a group, quoting the message it replies to
func (s *messageSender) sendGroupMsgBody(ctx context.Context, groupCode string, msgBody []any, replyTo string) (sendOutcome, error) {
	reqID := fmt.Sprintf("grp_%d", NextSeqNo())
	fromAccount, err := protoText(s.adapter.botIDValue())
	if err != nil {
		return sendOutcome{}, err
	}
	encoded, err := EncodeSendGroupMessage(groupCode, msgBody, fromAccount, reqID, "", "", nil, replyTo, "")
	if err != nil {
		return sendOutcome{}, err
	}
	return dispatchEncoded(ctx, s.adapter, encoded, reqID)
}

// Sends encoded bytes over the socket and reports the message key of the response or the failure text
func dispatchEncoded(ctx context.Context, adapter *YuanbaoAdapter, encoded []byte, reqID string) (sendOutcome, error) {
	response, err := adapter.connection.sendBizRequest(ctx, encoded, reqID, defaultSendTimeout)
	switch {
	case err != nil && ctx.Err() != nil:
		return sendOutcome{}, ctx.Err()
	case err != nil && isTimeout(err):
		return sendOutcome{errText: "Request timeout after " + pyrepr.Float(defaultSendTimeout) + "s"}, nil
	case err != nil:
		return sendOutcome{errText: errText(err)}, nil
	}
	fields, err := dictOf(response)
	if err != nil {
		return sendOutcome{errText: errText(err)}, nil
	}
	return sendOutcome{success: true, msgKey: dictGetOr(fields, "msg_id", "")}, nil
}

// One chunk for content within the limit, else table and fence aware chunks with their page indicators stripped
func truncateMessage(content string, maxLength int) []string {
	if pystr.Len(content) <= maxLength {
		return []string{content}
	}
	chunks := chunkMarkdownText(content, maxLength)
	for i, chunk := range chunks {
		if stripped, err := indicatorRE.Replace(chunk, "", -1, -1); err == nil {
			chunks[i] = stripped
		}
	}
	if len(chunks) == 0 {
		return []string{content}
	}
	return chunks
}

// Body between the header divider and the footer of a scheduler cron wrapper, the content itself for any other shape
func stripCronWrapper(content string) string {
	if !strings.HasPrefix(content, cronWrapperHeaderPrefix) {
		return content
	}
	dividerPos := strings.Index(content, cronWrapperDivider)
	footerPos := strings.LastIndex(content, cronWrapperFooterPrefix)
	if dividerPos < 0 || footerPos < 0 || footerPos <= dividerPos || !strings.Contains(content[:dividerPos], "\n(job_id: ") {
		return content
	}
	if body := pystr.Strip(content[dividerPos+len(cronWrapperDivider) : footerPos]); body != "" {
		return body
	}
	return content
}

// Forgets every chat lock
func (s *messageSender) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatLocks.Clear()
}

// outboundManager composes the sender, the reply heartbeat and the slow response notifier
type outboundManager struct {
	sender       *messageSender
	heartbeat    *heartbeatManager
	slowNotifier *slowResponseNotifier
}

func newOutboundManager(adapter *YuanbaoAdapter) *outboundManager {
	sender := newMessageSender(adapter)
	return &outboundManager{sender: sender, heartbeat: newHeartbeatManager(adapter), slowNotifier: newSlowResponseNotifier(adapter, sender)}
}

// Closes the sender, the heartbeat and the notifier
func (o *outboundManager) close() {
	o.sender.close()
	o.heartbeat.close()
	o.slowNotifier.close()
}
