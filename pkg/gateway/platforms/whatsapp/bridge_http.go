package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Host values the bridge accepts, loopback aliases only against DNS rebinding
var acceptedHostValues = map[string]bool{"localhost": true, "127.0.0.1": true, "[::1]": true, "::1": true}

// Routes of the bridge HTTP API behind the loopback Host header check
func (b *bridgeHost) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /messages", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, b.drainQueues()) })
	mux.HandleFunc("POST /send", b.handleSend)
	mux.HandleFunc("POST /edit", b.handleEdit)
	mux.HandleFunc("POST /send-media", b.handleSendMedia)
	mux.HandleFunc("POST /send-poll", b.handleSendPoll)
	mux.HandleFunc("POST /send-location", b.handleSendLocation)
	mux.HandleFunc("POST /typing", b.handleTyping)
	mux.HandleFunc("POST /read", b.handleRead)
	mux.HandleFunc("GET /chat/{id}", b.handleChat)
	mux.HandleFunc("GET /health", b.handleHealth)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimSpace(r.Host)
		if raw == "" {
			writeJSON(w, 400, jsonObj("error", "Missing Host header"))
			return
		}
		hostOnly := raw
		if i := strings.LastIndex(raw, ":"); i >= 0 {
			hostOnly = raw[:i]
		}
		hostOnly = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(hostOnly, "["), "]"))
		if !acceptedHostValues[hostOnly] {
			writeJSON(w, 400, jsonObj("error", "Invalid Host header. Bridge accepts loopback hosts only."))
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Compact JSON response with the status code
func writeJSON(w http.ResponseWriter, status int, payload any) {
	text, err := pyjson.Dumps(payload, pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	if err != nil {
		text = `{"error":"` + strings.ReplaceAll(err.Error(), `"`, `'`) + `"}`
		status = 500
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	io.WriteString(w, text)
}

// Request JSON object, empty for a non JSON content type, an error for a malformed body
func readBody(r *http.Request) (*pyjson.Object, error) {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return pyjson.NewObject(), nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 100*1024))
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return pyjson.NewObject(), nil
	}
	v, err := pyjson.LoadsBytes(raw)
	if err != nil {
		return nil, err
	}
	switch x := v.(type) {
	case *pyjson.Object:
		return x, nil
	case []any:
		return pyjson.NewObject(), nil
	}
	return nil, errors.New("strict JSON body must be an object or array")
}

// Connected socket for a sending route, writing the 503 answer when there is none
func (b *bridgeHost) requireConnected(w http.ResponseWriter, text string) *whatsmeow.Client {
	client, state := b.socket()
	if client == nil || state != "connected" {
		writeJSON(w, 503, jsonObj("error", text))
		return nil
	}
	return client
}

// Parsed request body, writing the 400 answer for a malformed one
func (b *bridgeHost) body(w http.ResponseWriter, r *http.Request) *pyjson.Object {
	obj, err := readBody(r)
	if err != nil {
		writeJSON(w, 400, jsonObj("error", err.Error()))
		return nil
	}
	return obj
}

// Own JID of the linked device without its device part
func ownJID(client *whatsmeow.Client) types.JID {
	if client.Store.ID == nil {
		return types.EmptyJID
	}
	return client.Store.ID.ToNonAD()
}

// Mention JIDs of a request value, only for a non empty array
func mentionList(v any) []string {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make([]string, len(arr))
	for i, m := range arr {
		out[i] = jsString(m)
	}
	return out
}

// Text message carrying the mentions and the native quote of a stored message
func (b *bridgeHost) buildTextSendPayload(client *whatsmeow.Client, text string, replyTo any, mentions []string) *waE2E.Message {
	msg := &waE2E.ExtendedTextMessage{Text: proto.String(text)}
	var ci *waE2E.ContextInfo
	if len(mentions) > 0 {
		ci = &waE2E.ContextInfo{MentionedJID: mentions}
	}
	if id := jsTruthyString(replyTo); id != "" {
		if quoted, ok := b.messageStore.get(id, true); ok && quoted.message != nil {
			if ci == nil {
				ci = &waE2E.ContextInfo{}
			}
			participant := quoted.info.Chat
			switch {
			case quoted.info.IsFromMe:
				participant = ownJID(client)
			case quoted.info.IsGroup:
				participant = quoted.info.Sender.ToNonAD()
			}
			ci.StanzaID = proto.String(quoted.info.ID)
			ci.Participant = proto.String(participant.String())
			ci.QuotedMessage = quoted.message
		}
	}
	msg.ContextInfo = ci
	return &waE2E.Message{ExtendedTextMessage: msg}
}

// Sends one message on the serial queue and returns its id
func (b *bridgeHost) sendOne(client *whatsmeow.Client, chat types.JID, msg *waE2E.Message) (string, time.Time, error) {
	var at time.Time
	id, err := b.sendWithTimeout(func(ctx context.Context) (string, error) {
		resp, err := client.SendMessage(ctx, chat, msg)
		at = resp.Timestamp
		return resp.ID, err
	})
	return id, at, err
}

// Records a sent message as ours, in the quote store and as acknowledged by the server
func (b *bridgeHost) trackSent(client *whatsmeow.Client, chat types.JID, id string, msg *waE2E.Message, at time.Time) {
	if id == "" {
		return
	}
	b.recentlySent.remember(id)
	b.messageStore.put(id, &storedMessage{info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat,
		Sender: ownJID(client), IsFromMe: true, IsGroup: chat.Server == types.GroupServer}, ID: id, Timestamp: at}, message: msg})
	b.onServerAck(chat.String(), id, at)
}

// Chat JID of a request, an error for an id the library cannot address
func parseChat(raw string) (types.JID, error) {
	jid, err := types.ParseJID(raw)
	if err != nil {
		return types.EmptyJID, err
	}
	if jid.Server == "" {
		return types.EmptyJID, fmt.Errorf("invalid jid: %s", raw)
	}
	return jid, nil
}

func (b *bridgeHost) handleSend(w http.ResponseWriter, r *http.Request) {
	client := b.requireConnected(w, "Not connected to WhatsApp")
	if client == nil {
		return
	}
	req := b.body(w, r)
	if req == nil {
		return
	}
	chatID, message := jsTruthyString(req.GetOr("chatId", nil)), jsTruthyString(req.GetOr("message", nil))
	if chatID == "" || message == "" {
		writeJSON(w, 400, jsonObj("error", "chatId and message are required"))
		return
	}
	chat, err := parseChat(chatID)
	if err != nil {
		writeJSON(w, 500, jsonObj("error", err.Error()))
		return
	}
	chunks := b.split(b.formatOutgoingMessage(message))
	ids := []any{}
	var last any
	for i, chunk := range chunks {
		var replyTo any
		var mentions []string
		if i == 0 {
			replyTo, mentions = req.GetOr("replyTo", nil), mentionList(req.GetOr("mentions", nil))
		}
		payload := b.buildTextSendPayload(client, chunk, replyTo, mentions)
		id, at, err := b.sendOne(client, chat, payload)
		if err != nil {
			writeJSON(w, 500, jsonObj("error", err.Error()))
			return
		}
		b.trackSent(client, chat, id, payload, at)
		if id != "" {
			ids = append(ids, id)
			last = id
		}
		if len(chunks) > 1 && i < len(chunks)-1 {
			b.chunkDelay()
		}
	}
	resp := jsonObj("success", true, "messageIds", ids)
	if last != nil {
		resp = jsonObj("success", true, "messageId", last, "messageIds", ids)
	}
	writeJSON(w, 200, resp)
}

func (b *bridgeHost) handleEdit(w http.ResponseWriter, r *http.Request) {
	client := b.requireConnected(w, "Not connected to WhatsApp")
	if client == nil {
		return
	}
	req := b.body(w, r)
	if req == nil {
		return
	}
	chatID, messageID, message := jsTruthyString(req.GetOr("chatId", nil)), jsTruthyString(req.GetOr("messageId", nil)), jsTruthyString(req.GetOr("message", nil))
	if chatID == "" || messageID == "" || message == "" {
		writeJSON(w, 400, jsonObj("error", "chatId, messageId, and message are required"))
		return
	}
	chat, err := parseChat(chatID)
	if err != nil {
		writeJSON(w, 500, jsonObj("error", err.Error()))
		return
	}
	chunks := b.split(b.formatOutgoingMessage(message))
	ids := []any{}
	edit := client.BuildEdit(chat, messageID, &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(chunks[0])}})
	if _, _, err := b.sendOne(client, chat, edit); err != nil {
		writeJSON(w, 500, jsonObj("error", err.Error()))
		return
	}
	for i := 1; i < len(chunks); i++ {
		payload := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(chunks[i])}}
		id, at, err := b.sendOne(client, chat, payload)
		if err != nil {
			writeJSON(w, 500, jsonObj("error", err.Error()))
			return
		}
		if id != "" {
			b.recentlySent.remember(id)
			b.onServerAck(chat.String(), id, at)
			ids = append(ids, id)
		}
		if i < len(chunks)-1 {
			b.chunkDelay()
		}
	}
	writeJSON(w, 200, jsonObj("success", true, "messageIds", ids))
}

// Converts a file with ffmpeg into a fresh temp file and returns its bytes
func ffmpegConvert(input, prefix, ext string, args ...string) ([]byte, error) {
	tmp := filepath.Join(os.TempDir(), prefix+randomHex(6)+ext)
	defer os.Remove(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	argv := append(append([]string{"-y", "-i", input}, args...), tmp)
	if out, err := exec.CommandContext(ctx, "ffmpeg", argv...).CombinedOutput(); err != nil {
		if len(out) > 0 {
			return nil, fmt.Errorf("Command failed: ffmpeg %s\n%s", strings.Join(argv, " "), strings.TrimSpace(string(out)))
		}
		return nil, err
	}
	return os.ReadFile(tmp)
}

// Uploaded media message of one kind with caption, mime and the extra fields of that kind
func (b *bridgeHost) uploadMedia(ctx context.Context, client *whatsmeow.Client, kind string, data []byte, mime, caption, fileName string, gif, ptt bool, mentions []string) (*waE2E.Message, error) {
	appInfo := map[string]whatsmeow.MediaType{"image": whatsmeow.MediaImage, "video": whatsmeow.MediaVideo,
		"audio": whatsmeow.MediaAudio, "document": whatsmeow.MediaDocument}[kind]
	up, err := client.Upload(ctx, data, appInfo)
	if err != nil {
		return nil, err
	}
	var ci *waE2E.ContextInfo
	if len(mentions) > 0 {
		ci = &waE2E.ContextInfo{MentionedJID: mentions}
	}
	opt := func(s string) *string {
		if s == "" {
			return nil
		}
		return proto.String(s)
	}
	length := proto.Uint64(uint64(len(data)))
	switch kind {
	case "image":
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: length,
			Mimetype: proto.String(mime), Caption: opt(caption), ContextInfo: ci}}, nil
	case "video":
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: length,
			Mimetype: proto.String(mime), Caption: opt(caption), GifPlayback: proto.Bool(gif), ContextInfo: ci}}, nil
	case "audio":
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: length,
			Mimetype: proto.String(mime), PTT: proto.Bool(ptt), ContextInfo: ci}}, nil
	}
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath),
		MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: length,
		Mimetype: proto.String(mime), Caption: opt(caption), FileName: proto.String(fileName), ContextInfo: ci}}, nil
}

// Mime of an outbound file extension with the kind default
func mimeFor(ext, def string) string {
	if m := bridgeMimeMap[ext]; m != "" {
		return m
	}
	return def
}

func (b *bridgeHost) handleSendMedia(w http.ResponseWriter, r *http.Request) {
	client := b.requireConnected(w, "Not connected to WhatsApp")
	if client == nil {
		return
	}
	req := b.body(w, r)
	if req == nil {
		return
	}
	chatID, filePath := jsTruthyString(req.GetOr("chatId", nil)), jsTruthyString(req.GetOr("filePath", nil))
	if chatID == "" || filePath == "" {
		writeJSON(w, 400, jsonObj("error", "chatId and filePath are required"))
		return
	}
	if _, err := os.Stat(filePath); err != nil {
		writeJSON(w, 404, jsonObj("error", "File not found: "+filePath))
		return
	}
	chat, err := parseChat(chatID)
	if err != nil {
		writeJSON(w, 500, jsonObj("error", err.Error()))
		return
	}
	buffer, err := os.ReadFile(filePath)
	if err != nil {
		writeJSON(w, 500, jsonObj("error", err.Error()))
		return
	}
	lowered := strings.ToLower(filePath)
	ext := lowered[strings.LastIndex(lowered, ".")+1:]
	kind := jsTruthyString(req.GetOr("mediaType", nil))
	if kind == "" {
		kind = inferMediaType(ext)
	}
	caption, fileName := jsTruthyString(req.GetOr("caption", nil)), jsTruthyString(req.GetOr("fileName", nil))
	mentions := mentionList(req.GetOr("mentions", nil))
	type plan struct {
		kind, mime, fileName string
		data                 []byte
		gif, ptt             bool
	}
	var p plan
	switch kind {
	case "image":
		p = plan{kind: "image", mime: mimeFor(ext, "image/jpeg"), data: buffer}
		if ext == "gif" {
			p.mime = mimeFor(ext, "image/gif")
			if mp4, convErr := ffmpegConvert(filePath, "icebath_gif_", ".mp4", "-movflags", "faststart", "-pix_fmt", "yuv420p", "-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2"); convErr == nil {
				p = plan{kind: "video", mime: "video/mp4", data: mp4, gif: true}
			} else {
				b.console.log("[bridge] gif conversion failed, sending as image/gif: %s", convErr.Error())
			}
		}
	case "video":
		p = plan{kind: "video", mime: mimeFor(ext, "video/mp4"), data: buffer}
	case "audio":
		audio, audioExt := buffer, ext
		if audioExt != "ogg" && audioExt != "opus" {
			if ogg, convErr := ffmpegConvert(filePath, "icebath_voice_", ".ogg", "-ar", "48000", "-ac", "1", "-c:a", "libopus"); convErr == nil {
				audio, audioExt = ogg, "ogg"
			} else {
				b.console.log("[bridge] ffmpeg conversion failed, sending as file attachment: %s", convErr.Error())
			}
		}
		opus := audioExt == "ogg" || audioExt == "opus"
		mime := "audio/mpeg"
		if opus {
			mime = "audio/ogg; codecs=opus"
		}
		p = plan{kind: "audio", mime: mime, data: audio, ptt: opus}
	default:
		name := fileName
		if name == "" {
			name = filepath.Base(filePath)
		}
		p = plan{kind: "document", mime: mimeFor(ext, "application/octet-stream"), fileName: name, data: buffer}
	}
	var payload *waE2E.Message
	id, err := b.sendWithTimeout(func(ctx context.Context) (string, error) {
		msg, err := b.uploadMedia(ctx, client, p.kind, p.data, p.mime, caption, p.fileName, p.gif, p.ptt, mentions)
		if err != nil {
			return "", err
		}
		payload = msg
		resp, err := client.SendMessage(ctx, chat, msg)
		return resp.ID, err
	})
	if err != nil {
		writeJSON(w, 500, jsonObj("error", err.Error()))
		return
	}
	b.trackSent(client, chat, id, payload, time.Now())
	writeJSON(w, 200, jsonObj("success", true, "messageId", nilIfEmpty(id)))
}

// The string, nil when empty, for JSON fields JavaScript leaves undefined
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (b *bridgeHost) handleSendPoll(w http.ResponseWriter, r *http.Request) {
	client := b.requireConnected(w, "Not connected to WhatsApp")
	if client == nil {
		return
	}
	req := b.body(w, r)
	if req == nil {
		return
	}
	chatID := jsTruthyString(req.GetOr("chatId", nil))
	options, isList := req.GetOr("options", nil).([]any)
	if chatID == "" || !jsTruthy(req.GetOr("question", nil)) || !isList {
		writeJSON(w, 400, jsonObj("error", "chatId, question, and options are required"))
		return
	}
	question, opts, count, err := buildPollPayload(req.GetOr("question", nil), options, req.GetOr("selectableCount", nil))
	if err != nil {
		writeJSON(w, 400, jsonObj("error", err.Error()))
		return
	}
	chat, err := parseChat(chatID)
	if err != nil {
		writeJSON(w, 400, jsonObj("error", err.Error()))
		return
	}
	payload := client.BuildPollCreation(question, opts, count)
	id, at, err := b.sendOne(client, chat, payload)
	if err != nil {
		writeJSON(w, 400, jsonObj("error", err.Error()))
		return
	}
	b.trackSent(client, chat, id, payload, at)
	writeJSON(w, 200, jsonObj("success", true, "messageId", nilIfEmpty(id)))
}

func (b *bridgeHost) handleSendLocation(w http.ResponseWriter, r *http.Request) {
	client := b.requireConnected(w, "Not connected to WhatsApp")
	if client == nil {
		return
	}
	req := b.body(w, r)
	if req == nil {
		return
	}
	lat, hasLat := req.Get("latitude")
	lon, hasLon := req.Get("longitude")
	chatID := jsTruthyString(req.GetOr("chatId", nil))
	if chatID == "" || !hasLat || !hasLon {
		writeJSON(w, 400, jsonObj("error", "chatId, latitude, and longitude are required"))
		return
	}
	latitude, longitude, err := buildLocationPayload(lat, lon)
	if err != nil {
		writeJSON(w, 400, jsonObj("error", err.Error()))
		return
	}
	chat, err := parseChat(chatID)
	if err != nil {
		writeJSON(w, 400, jsonObj("error", err.Error()))
		return
	}
	loc := &waE2E.LocationMessage{DegreesLatitude: proto.Float64(latitude), DegreesLongitude: proto.Float64(longitude)}
	if name := jsTruthyString(req.GetOr("name", nil)); name != "" {
		loc.Name = proto.String(name)
	}
	if address := jsTruthyString(req.GetOr("address", nil)); address != "" {
		loc.Address = proto.String(address)
	}
	payload := &waE2E.Message{LocationMessage: loc}
	id, at, err := b.sendOne(client, chat, payload)
	if err != nil {
		writeJSON(w, 400, jsonObj("error", err.Error()))
		return
	}
	b.trackSent(client, chat, id, payload, at)
	writeJSON(w, 200, jsonObj("success", true, "messageId", nilIfEmpty(id)))
}

func (b *bridgeHost) handleTyping(w http.ResponseWriter, r *http.Request) {
	client := b.requireConnected(w, "Not connected")
	if client == nil {
		return
	}
	req := b.body(w, r)
	if req == nil {
		return
	}
	chatID := jsTruthyString(req.GetOr("chatId", nil))
	if chatID == "" {
		writeJSON(w, 400, jsonObj("error", "chatId required"))
		return
	}
	chat, err := parseChat(chatID)
	if err == nil {
		err = client.SendChatPresence(b.ctx, chat, types.ChatPresenceComposing, types.ChatPresenceMediaText)
	}
	writeJSON(w, 200, jsonObj("success", err == nil))
}

func (b *bridgeHost) handleRead(w http.ResponseWriter, r *http.Request) {
	client := b.requireConnected(w, "Not connected")
	if client == nil {
		return
	}
	req := b.body(w, r)
	if req == nil {
		return
	}
	key, _ := req.GetOr("key", nil).(*pyjson.Object)
	if !b.cfg.sendReadReceipts || key == nil || jsTruthy(key.GetOr("fromMe", nil)) || !jsTruthy(key.GetOr("id", nil)) || !jsTruthy(key.GetOr("remoteJid", nil)) {
		writeJSON(w, 200, jsonObj("success", true, "marked", false))
		return
	}
	chat, err := parseChat(jsString(key.GetOr("remoteJid", nil)))
	if err == nil {
		sender := types.EmptyJID
		if chat.Server == types.GroupServer && jsTruthy(key.GetOr("participant", nil)) {
			sender, err = parseChat(jsString(key.GetOr("participant", nil)))
		}
		if err == nil {
			err = client.MarkRead(b.ctx, []types.MessageID{jsString(key.GetOr("id", nil))}, time.Now(), chat, sender)
		}
	}
	if err != nil {
		b.console.log("[bridge] failed to send read receipt: %s", err.Error())
		writeJSON(w, 500, jsonObj("error", "Failed to send read receipt"))
		return
	}
	writeJSON(w, 200, jsonObj("success", true, "marked", true))
}

func (b *bridgeHost) handleChat(w http.ResponseWriter, r *http.Request) {
	chatID := r.PathValue("id")
	isGroup := strings.HasSuffix(chatID, "@g.us")
	client, _ := b.socket()
	if isGroup && client != nil {
		if jid, err := parseChat(chatID); err == nil {
			if info, err := client.GetGroupInfo(b.ctx, jid); err == nil {
				ids := []any{}
				for _, p := range info.Participants {
					ids = append(ids, p.JID.String())
				}
				writeJSON(w, 200, jsonObj("name", info.Name, "isGroup", true, "participants", ids))
				return
			}
		}
	}
	name, _, _ := strings.Cut(chatID, "@")
	writeJSON(w, 200, jsonObj("name", name, "isGroup", isGroup, "participants", []any{}))
}

func (b *bridgeHost) handleHealth(w http.ResponseWriter, r *http.Request) {
	_, state := b.socket()
	session, _ := filepath.Abs(b.cfg.session)
	writeJSON(w, 200, jsonObj("status", state, "queueLength", b.queueLength(), "uptime", time.Since(b.started).Seconds(),
		"scriptHash", b.scriptHash, "sendReadReceipts", b.cfg.sendReadReceipts, "session", session,
		"capabilities", jsonObj("outboundMentions", true)))
}
