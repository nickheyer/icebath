package weixin

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	voiceExts       = map[string]bool{".ogg": true, ".opus": true, ".mp3": true, ".wav": true, ".m4a": true, ".flac": true}
	videoExts       = map[string]bool{".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true, ".3gp": true}
	imageExts       = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}
	directImageExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true}
)

// Thirty two lowercase hex digits of a random UUID
func uuidHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b)
}

// Client id of one outbound message
func newClientID() string { return "icebath-weixin-" + uuidHex() }

// Sends one local file through the sender its extension selects, voice flagged media always as voice
func (a *WeixinAdapter) deliverLocalFile(ctx context.Context, chatID, path string, isVoice bool, metadata map[string]any) error {
	ext := pystr.Lower(pypath.Suffix(path))
	var err error
	switch {
	case isVoice || voiceExts[ext]:
		_, err = a.SendVoice(ctx, chatID, path, "", "", metadata, nil)
	case videoExts[ext]:
		_, err = a.SendVideo(ctx, chatID, path, "", "", metadata, nil)
	case imageExts[ext]:
		_, err = a.SendImageFile(ctx, chatID, path, "", "", metadata, nil)
	default:
		_, err = a.SendDocument(ctx, chatID, path, "", "", "", metadata, nil)
	}
	return err
}

// Delivers the media and local files a reply names, then its text in paced chunks echoing the peer's context token
func (a *WeixinAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if _, send := a.sessions(); send == nil || a.token == "" {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	contextToken := a.tokenStore.get(a.accountID, chatID)
	type delivery struct {
		path    string
		isVoice bool
		label   string
	}
	var deliveries []delivery
	scopeCtx, release := a.MediaDeliveryScope(ctx, a.BuildSource(ctx, chatID, gateway.SourceFields{}))
	mediaFiles, cleanedContent, err := gateway.ExtractMedia(scopeCtx, content)
	var finalContent string
	if err == nil {
		_, withoutImages := gateway.ExtractImages(cleanedContent)
		var localFiles []string
		localFiles, finalContent = gateway.ExtractLocalFiles(scopeCtx, withoutImages)
		var safeMedia []gateway.MediaRef
		if safeMedia, err = gateway.FilterMediaDeliveryPaths(scopeCtx, mediaFiles); err == nil {
			for _, m := range safeMedia {
				deliveries = append(deliveries, delivery{m.Path, m.IsVoice, "media"})
			}
			var safeLocal []string
			if safeLocal, err = gateway.FilterLocalDeliveryPaths(scopeCtx, localFiles, ""); err == nil {
				for _, p := range safeLocal {
					deliveries = append(deliveries, delivery{p, false, "local file"})
				}
			}
		}
	}
	release()
	if err != nil {
		return gateway.SendResult{}, err
	}
	var lastMessageID any
	sendErr := func() error {
		for _, d := range deliveries {
			if err := a.deliverLocalFile(ctx, chatID, d.path, d.isVoice, metadata); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				slog.Warn(fmt.Sprintf("[%s] %s delivery failed for %s: %s", a.Name(), d.label, d.path, errText(err)))
			}
		}
		var chunks []string
		for _, chunk := range a.splitText(a.FormatMessage(finalContent)) {
			if pystr.Strip(chunk) != "" {
				chunks = append(chunks, chunk)
			}
		}
		for idx, chunk := range chunks {
			clientID := newClientID()
			if err := a.sendTextChunk(ctx, chatID, chunk, contextToken, clientID); err != nil {
				return err
			}
			lastMessageID = clientID
			if idx < len(chunks)-1 && a.sendChunkDelaySeconds > 0 {
				if err := sleep(ctx, a.sendChunkDelaySeconds); err != nil {
					return err
				}
			}
		}
		return nil
	}()
	if sendErr != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		slog.Error(fmt.Sprintf("[%s] send failed to=%s: %s", a.Name(), safeID(chatID), errText(sendErr)))
		return gateway.SendResult{Success: false, Error: errText(sendErr)}, nil
	}
	return gateway.SendResult{Success: true, MessageID: lastMessageID}, nil
}

// Fresh typing ticket of a chat, refreshed through getconfig once the cached one expired
func (a *WeixinAdapter) ensureTypingTicket(ctx context.Context, chatID string) string {
	if ticket := a.typingCache.get(chatID); ticket != "" {
		return ticket
	}
	_, send := a.sessions()
	if send == nil || a.token == "" {
		return ""
	}
	return a.fetchTypingTicket(ctx, send, chatID, a.tokenStore.get(a.accountID, chatID), "typing ticket refresh failed")
}

// Posts a typing status for a chat when a typing ticket is available, a failure only logged
func (a *WeixinAdapter) setTyping(ctx context.Context, chatID string, status int, label string) error {
	ticket := a.ensureTypingTicket(ctx, chatID)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if ticket == "" {
		return nil
	}
	_, send := a.sessions()
	var err error
	if send == nil {
		err = &pyErr{"AttributeError", "'NoneType' object has no attribute 'post'"}
	} else {
		_, err = apiPost(ctx, send, a.baseURL, epSendTyping, pyjson.ObjectOf("ilink_user_id", chatID, "typing_ticket", ticket, "status", int64(status)),
			a.token, configTimeoutMS)
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Debug(fmt.Sprintf("[%s] typing %s failed for %s: %s", a.Name(), label, safeID(chatID), errText(err)))
	}
	return nil
}

// Shows the typing indicator in a chat
func (a *WeixinAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return a.setTyping(ctx, chatID, typingStart, "start")
}

// Clears the typing indicator of a chat
func (a *WeixinAdapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return a.setTyping(ctx, chatID, typingStop, "stop")
}

// Sends an image from a remote URL or a local path as a document, a downloaded copy removed afterwards
func (a *WeixinAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	cleanup := strings.HasPrefix(imageURL, "http://") || strings.HasPrefix(imageURL, "https://")
	var filePath string
	if cleanup {
		downloaded, err := a.downloadRemoteMedia(ctx, imageURL)
		if err != nil {
			return gateway.SendResult{}, err
		}
		filePath = downloaded
	} else {
		filePath = strings.ReplaceAll(imageURL, "file://", "")
		if !pypath.IsAbs(filePath) {
			abs, err := filepath.Abs(filePath)
			if err != nil {
				return gateway.SendResult{}, err
			}
			filePath = abs
		}
	}
	if cleanup {
		defer func() {
			if filePath != "" {
				_ = os.Remove(filePath)
			}
		}()
	}
	return a.SendDocument(ctx, chatID, filePath, caption, "", "", metadata, nil)
}

// Sends a local image as a document
func (a *WeixinAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.SendDocument(ctx, chatID, imagePath, caption, "", "", metadata, nil)
}

// Sends a file and wraps the outcome, not connected without a session or token
func (a *WeixinAdapter) sendFileResult(ctx context.Context, chatID, path, caption, label string, forceFileAttachment bool) (gateway.SendResult, error) {
	if _, send := a.sessions(); send == nil || a.token == "" {
		return gateway.SendResult{Success: false, Error: "Not connected"}, nil
	}
	messageID, err := a.sendFile(ctx, chatID, path, caption, forceFileAttachment)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		slog.Error(fmt.Sprintf("[%s] %s failed to=%s: %s", a.Name(), label, safeID(chatID), errText(err)))
		return gateway.SendResult{Success: false, Error: errText(err)}, nil
	}
	return gateway.SendResult{Success: true, MessageID: messageID}, nil
}

// Sends a document through the encrypted CDN
func (a *WeixinAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFileResult(ctx, chatID, filePath, caption, "send_document", false)
}

// Sends a video through the encrypted CDN
func (a *WeixinAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFileResult(ctx, chatID, videoPath, caption, "send_video", false)
}

// Sends audio as a file attachment, captioned as a voice message when no caption is given
func (a *WeixinAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	if caption == "" {
		if text := a.Self().WarningText(ctx, i18n.T(ctx, "platform.weixin.voice_as_attachment", "", nil), nil, "", "", nil); text != nil {
			caption = *text
		}
	}
	return a.sendFileResult(ctx, chatID, audioPath, caption, "send_voice", true)
}

// Downloads a remote media URL into a temporary file after the SSRF check and returns its path
func (a *WeixinAdapter) downloadRemoteMedia(ctx context.Context, rawURL string) (string, error) {
	if !tools.IsSafeURL(gateway.URLSafetyEnv(ctx), rawURL) {
		return "", valueError("Blocked unsafe URL (SSRF protection): " + rawURL)
	}
	_, send := a.sessions()
	if send == nil {
		return "", &pyErr{"AssertionError", ""}
	}
	data, err := downloadBytes(ctx, send, rawURL, 30)
	if err != nil {
		return "", err
	}
	suffix := pypath.Suffix(strings.SplitN(rawURL, "?", 2)[0])
	if suffix == "" {
		suffix = ".bin"
	}
	handle, err := os.CreateTemp("", "tmp*"+suffix)
	if err != nil {
		return "", err
	}
	_, werr := handle.Write(data)
	if cerr := handle.Close(); werr == nil {
		werr = cerr
	}
	return handle.Name(), werr
}

// Outbound media item of an uploaded file with the fields its item type carries
func mediaItem(itemType int, encryptQueryParam, aesKeyForAPI string, fields *pyjson.Object) *pyjson.Object {
	keys := map[int]string{itemFile: "file_item", itemImage: "image_item", itemVideo: "video_item", itemVoice: "voice_item"}
	body := pyjson.ObjectOf("media", pyjson.ObjectOf("encrypt_query_param", encryptQueryParam, "aes_key", aesKeyForAPI, "encrypt_type", int64(1)))
	body.Update(fields)
	return pyjson.ObjectOf("type", int64(itemType), keys[itemType], body)
}

// Upload media type and outbound item type of a path, audio and unknown types going out as file attachments
func outboundMediaBuilder(path string, forceFileAttachment bool) (int, int, error) {
	mime, _, err := pymimetypes.GuessType(path, true)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case strings.HasPrefix(mime, "image/"):
		return mediaImage, itemImage, nil
	case strings.HasPrefix(mime, "video/"):
		return mediaVideo, itemVideo, nil
	case strings.HasSuffix(path, ".silk") && !forceFileAttachment:
		return mediaVoice, itemVoice, nil
	}
	return mediaFile, itemFile, nil
}

// Encrypts and uploads a file to the CDN, then sends its caption and media item, returning the last client id
func (a *WeixinAdapter) sendFile(ctx context.Context, chatID, path, caption string, forceFileAttachment bool) (string, error) {
	_, send := a.sessions()
	if send == nil {
		return "", &pyErr{"AssertionError", ""}
	}
	plaintext, err := os.ReadFile(path)
	if err != nil {
		return "", &pyErr{pypath.OSErrorClass(err), pypath.OSErrorText(err)}
	}
	mediaType, itemType, err := outboundMediaBuilder(path, forceFileAttachment)
	if err != nil {
		return "", err
	}
	filekeyBytes, aesKey := make([]byte, 16), make([]byte, 16)
	_, _ = rand.Read(filekeyBytes)
	_, _ = rand.Read(aesKey)
	filekey, aesKeyHex := hex.EncodeToString(filekeyBytes), hex.EncodeToString(aesKey)
	sum := md5.Sum(plaintext)
	rawsize, rawfilemd5 := len(plaintext), hex.EncodeToString(sum[:])
	uploadResponse, err := getUploadURL(ctx, send, a.baseURL, a.token, chatID, mediaType, filekey, rawsize, rawfilemd5, ((rawsize+16)/16)*16, aesKeyHex)
	if err != nil {
		return "", err
	}
	uploadParam := strOf(uploadResponse.Vals["upload_param"])
	ciphertext, err := aes128ECBEncrypt(plaintext, aesKey)
	if err != nil {
		return "", err
	}
	uploadURL := strOf(uploadResponse.Vals["upload_full_url"])
	if uploadURL == "" && uploadParam != "" {
		uploadURL = pystr.RstripChars(a.cdnBaseURL, "/") + "/upload?encrypted_query_param=" + pyurl.Quote(uploadParam, "") + "&filekey=" + pyurl.Quote(filekey, "")
	}
	if uploadURL == "" {
		return "", runtimeError("getUploadUrl returned neither upload_param nor upload_full_url: " + pyrepr.Str(uploadResponse))
	}
	encryptedQueryParam, err := uploadCiphertext(ctx, send, ciphertext, uploadURL)
	if err != nil {
		return "", err
	}
	contextToken := a.tokenStore.get(a.accountID, chatID)
	aesKeyForAPI := base64.StdEncoding.EncodeToString([]byte(aesKeyHex))
	var fields *pyjson.Object
	switch itemType {
	case itemFile:
		fields = pyjson.ObjectOf("file_name", pypath.Name(path), "len", fmt.Sprint(rawsize))
	case itemImage:
		fields = pyjson.ObjectOf("mid_size", int64(len(ciphertext)))
	case itemVideo:
		fields = pyjson.ObjectOf("video_size", int64(len(ciphertext)), "play_length", int64(0), "video_md5", rawfilemd5)
	case itemVoice:
		fields = pyjson.ObjectOf("encode_type", nil, "bits_per_sample", nil, "sample_rate", nil, "playtime", int64(0))
		if mediaType == mediaVoice && strings.HasSuffix(path, ".silk") {
			fields.Set("encode_type", int64(6))
			fields.Set("bits_per_sample", int64(16))
			fields.Set("sample_rate", int64(24000))
		}
	}
	itemLists := [][]any{{mediaItem(itemType, encryptedQueryParam, aesKeyForAPI, fields)}}
	if caption != "" {
		itemLists = append([][]any{{pyjson.ObjectOf("type", int64(itemText), "text_item", pyjson.ObjectOf("text", a.FormatMessage(caption)))}}, itemLists...)
	}
	lastMessageID := ""
	for _, itemList := range itemLists {
		lastMessageID = newClientID()
		for {
			resp, err := sendItems(ctx, send, a.baseURL, a.token, chatID, itemList, contextToken, lastMessageID)
			if err != nil {
				return "", err
			}
			ret, errcode := retAndErrcode(resp)
			if (ret == nil || equalsInt(ret, 0)) && (errcode == nil || equalsInt(errcode, 0)) {
				break
			}
			if isSessionExpired(resp, ret, errcode) && contextToken != "" {
				contextToken = ""
				a.tokenStore.drop(a.accountID, chatID)
				slog.Warn(fmt.Sprintf("[%s] session expired for %s; re-sending media without context_token", a.Name(), safeID(chatID)))
				continue
			}
			errmsg := pyrepr.Or(resp.Vals["errmsg"], resp.Vals["msg"])
			if isStaleSessionRet(ret, errcode, errmsg) {
				return "", sessionNotReadyError(ret, errcode, errmsg)
			}
			return "", runtimeError(fmt.Sprintf("iLink sendmessage error: ret=%s errcode=%s errmsg=%s", pyrepr.Str(ret), pyrepr.Str(errcode),
				pyrepr.Str(pyrepr.Or(errmsg, "unknown error"))))
		}
	}
	return lastMessageID, nil
}

// Name, type and id of a chat, a chatroom suffix marking a group
func (a *WeixinAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	chatType := "dm"
	if strings.HasSuffix(chatID, "@chatroom") {
		chatType = "group"
	}
	return map[string]any{"name": chatID, "type": chatType, "chat_id": chatID}, nil
}

// Markdown normalized and long lines wrapped for copying in WeChat clients
func (a *WeixinAdapter) FormatMessage(content string) string {
	return wrapCopyFriendlyLinesForWeixin(normalizeMarkdownBlocks(content))
}
