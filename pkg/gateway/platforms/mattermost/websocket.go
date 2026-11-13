package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
)

const (
	reconnectBaseDelay = 2 * time.Second
	reconnectMaxDelay  = 60 * time.Second
	reconnectJitter    = 0.2
	wsPingInterval     = 30 * time.Second
	wsReadTimeout      = 45 * time.Second
	wsWriteTimeout     = 10 * time.Second
	wsEventBuffer      = 256
)

// Inbound message kind of the first matching attachment mime prefix
var mediaKinds = []struct {
	prefix string
	kind   gateway.MessageType
}{{"image/", gateway.MessageTypePhoto}, {"audio/", gateway.MessageTypeVoice}, {"video/", gateway.MessageTypeVideo}}

// Values that switch the channel mention requirement off
var mentionOffWords = map[string]bool{"false": true, "0": true, "no": true}

// Websocket rejection of the bot token, ending the reconnect loop
type authError struct{ reason string }

func (e *authError) Error() string { return "authentication rejected (" + e.reason + ")" }

// One running websocket listener with its cancel, completion and live socket
type wsStream struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	conn   *websocket.Conn
}

// Records the live socket of the current session
func (s *wsStream) setConn(conn *websocket.Conn) {
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
}

// Closes the live socket so a blocked read returns
func (s *wsStream) closeConn() {
	s.mu.Lock()
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// Starts the reconnecting websocket listener detached from the connect call
func (a *Adapter) startStream(ctx context.Context) {
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &wsStream{cancel: cancel, done: make(chan struct{})}
	a.mu.Lock()
	a.stream = s
	a.mu.Unlock()
	go func() {
		defer close(s.done)
		a.wsLoop(sctx, s)
	}()
}

// Cancels the listener, closes its socket and waits for it to finish
func (a *Adapter) stopStream() {
	a.mu.Lock()
	s := a.stream
	a.stream = nil
	a.mu.Unlock()
	if s == nil {
		return
	}
	s.cancel()
	s.closeConn()
	<-s.done
}

// Runs websocket sessions with jittered exponential backoff until cancelled or the token is rejected
func (a *Adapter) wsLoop(ctx context.Context, s *wsStream) {
	delay := reconnectBaseDelay
	for ctx.Err() == nil {
		err := a.wsSession(ctx, s)
		if ctx.Err() != nil {
			return
		}
		var auth *authError
		switch {
		case err == nil:
			delay = reconnectBaseDelay
		case errors.As(err, &auth):
			slog.Error(fmt.Sprintf("Mattermost WS auth failed (%s) — stopping reconnect", auth.reason))
			a.SetFatalError(ctx, "mattermost_auth_error", fmt.Sprintf("Mattermost WebSocket authentication rejected (%s). The bot token is "+
				"invalid, revoked, or lacks permission — check MATTERMOST_TOKEN and the bot account in the System Console.", auth.reason), false)
			_ = a.NotifyFatalError(ctx)
			return
		default:
			slog.Warn(fmt.Sprintf("Mattermost WS error: %v — reconnecting in %.0fs", err, delay.Seconds()))
		}
		jitter := time.Duration(rand.Float64() * reconnectJitter * float64(delay))
		if !sleepCtx(ctx, delay+jitter) {
			return
		}
		delay = min(delay*2, reconnectMaxDelay)
	}
}

// Websocket endpoint of the server URL, http schemes mapped to ws schemes
func (a *Adapter) wsURL() string {
	u := a.baseURL
	if strings.HasPrefix(u, "http") {
		u = "ws" + u[len("http"):]
	}
	return u + "/api/v4/websocket"
}

// One websocket session that authenticates and feeds events to a serial worker, nil on a clean close
func (a *Adapter) wsSession(ctx context.Context, s *wsStream) error {
	wsURL := a.wsURL()
	slog.Info("Mattermost: connecting to " + wsURL)
	dialer := websocket.Dialer{HandshakeTimeout: requestTimeout}
	if gateway.GatewayTrustEnv(ctx) {
		dialer.Proxy = http.ProxyFromEnvironment
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, http.Header{"Authorization": {"Bearer " + a.token}})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return &authError{reason: fmt.Sprintf("HTTP %d", resp.StatusCode)}
		}
		return err
	}
	s.setConn(conn)
	defer s.closeConn()
	if ctx.Err() != nil {
		return nil
	}
	challenge := map[string]any{"seq": 1, "action": "authentication_challenge", "data": map[string]any{"token": a.token}}
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	if err := conn.WriteJSON(challenge); err != nil {
		return err
	}
	slog.Info("Mattermost: WebSocket connected and authenticated")
	extend := func() { _ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout)) }
	extend()
	conn.SetPongHandler(func(string) error { extend(); return nil })
	conn.SetPingHandler(func(appData string) error {
		extend()
		err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(wsWriteTimeout))
		var netErr interface{ Timeout() bool }
		if errors.Is(err, websocket.ErrCloseSent) || (errors.As(err, &netErr) && netErr.Timeout()) {
			return nil
		}
		return err
	})
	events := make(chan map[string]any, wsEventBuffer)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for event := range events {
			a.handleEvent(ctx, event)
		}
	}()
	defer func() {
		close(events)
		<-workerDone
	}()
	pingStop := make(chan struct{})
	defer close(pingStop)
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout)) != nil {
					return
				}
			case <-pingStop:
				return
			}
		}
	}()
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var closed *websocket.CloseError
			if errors.As(err, &closed) {
				slog.Info(fmt.Sprintf("Mattermost: WebSocket closed (%d %s)", closed.Code, closed.Text))
				return nil
			}
			return err
		}
		extend()
		if kind != websocket.TextMessage && kind != websocket.BinaryMessage {
			continue
		}
		var event map[string]any
		if json.Unmarshal(data, &event) != nil || event == nil {
			continue
		}
		if textOf(event["seq_reply"]) == "1" && textOf(event["status"]) == "FAIL" {
			detail := "authentication challenge failed"
			if e, ok := event["error"].(map[string]any); ok && textOf(e["message"]) != "" {
				detail += ": " + textOf(e["message"])
			}
			return &authError{reason: detail}
		}
		select {
		case events <- event:
		case <-ctx.Done():
			return nil
		}
	}
}

// Channel post text with the bot mention removed, false when channel gating drops the post
func (a *Adapter) gate(ctx context.Context, channelID, text string) (string, bool) {
	extra := a.Config.Extra
	read := func(key, env, def string) any {
		v, err := gateway.ExtraOrSecret(ctx, extra, key, env, def, false)
		if err != nil {
			slog.Warn(fmt.Sprintf("Mattermost: cannot read %s: %v", env, err))
			return def
		}
		return v
	}
	if allowed := gateway.CoerceAllowSet(read("allowed_channels", "MATTERMOST_ALLOWED_CHANNELS", "")); len(allowed) > 0 && !allowed[channelID] {
		slog.Debug("Mattermost: ignoring message in non-allowed channel: " + channelID)
		return "", false
	}
	requireMention := !mentionOffWords[strings.ToLower(strings.TrimSpace(textOf(read("require_mention", "MATTERMOST_REQUIRE_MENTION", "true"))))]
	free := gateway.CoerceAllowSet(read("free_response_channels", "MATTERMOST_FREE_RESPONSE_CHANNELS", ""))
	botID, botName := a.botIdentity()
	var patterns []string
	for _, id := range []string{botName, botID} {
		if id != "" {
			patterns = append(patterns, "@"+id)
		}
	}
	lowered := strings.ToLower(text)
	mentioned := false
	for _, p := range patterns {
		if strings.Contains(lowered, strings.ToLower(p)) {
			mentioned = true
		}
	}
	if requireMention && !free[channelID] && !mentioned {
		slog.Debug("Mattermost: skipping non-DM message without @mention (channel=" + channelID + ")")
		return "", false
	}
	if mentioned {
		for _, p := range patterns {
			text = strings.TrimSpace(regexp.MustCompile(`(?i)`+regexp.QuoteMeta(p)).ReplaceAllString(text, ""))
		}
	}
	return text, true
}

// Cached path of attachment bytes by mime kind, keeping the original extension
func cacheAttachment(ctx context.Context, data []byte, name, mimeType string) (string, error) {
	ext := filepath.Ext(name)
	pick := func(def string) string {
		if ext == "" {
			return def
		}
		return ext
	}
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return gateway.CacheImageFromBytesAsync(ctx, data, pick(".png"))
	case strings.HasPrefix(mimeType, "audio/"):
		return gateway.CacheAudioFromBytesAsync(ctx, data, pick(".ogg"))
	case strings.HasPrefix(mimeType, "video/"):
		return gateway.CacheVideoFromBytesAsync(ctx, data, pick(".mp4"))
	}
	return gateway.CacheDocumentFromBytesAsync(ctx, data, name)
}

// Cached paths and mime types of a post's attachments, downloaded now since their URLs need the bot token
func (a *Adapter) downloadAttachments(ctx context.Context, raw any) ([]string, []string) {
	ids, _ := raw.([]any)
	var paths, types []string
	for _, item := range ids {
		fid := textOf(item)
		if fid == "" || strings.ContainsAny(fid, "/?#") || strings.Contains(fid, "..") {
			continue
		}
		client := a.httpClient()
		if client == nil {
			break
		}
		info, _ := a.call(ctx, http.MethodGet, "files/"+fid+"/info", nil)
		name := textOf(info["name"])
		if name == "" {
			name = "file_" + fid
		}
		mimeType := textOf(info["mime_type"])
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		status, data, err := doRequest(ctx, client, http.MethodGet, a.baseURL+"/api/v4/files/"+fid, a.token, "", nil, requestTimeout)
		if err != nil {
			slog.Warn(fmt.Sprintf("Mattermost: error downloading file %s: %v", fid, err))
			continue
		}
		if status >= 400 {
			slog.Warn(fmt.Sprintf("Mattermost: failed to download file %s: HTTP %d", fid, status))
			continue
		}
		path, err := cacheAttachment(ctx, data, name, mimeType)
		if err != nil {
			slog.Warn(fmt.Sprintf("Mattermost: error caching file %s: %v", fid, err))
			continue
		}
		paths = append(paths, path)
		types = append(types, mimeType)
	}
	return paths, types
}

// Message kind of an inbound post, commands first, then the first matching attachment kind
func messageKind(text string, mediaTypes []string) gateway.MessageType {
	if strings.HasPrefix(text, "/") {
		return gateway.MessageTypeCommand
	}
	if len(mediaTypes) == 0 {
		return gateway.MessageTypeText
	}
	for _, mk := range mediaKinds {
		for _, m := range mediaTypes {
			if strings.HasPrefix(m, mk.prefix) {
				return mk.kind
			}
		}
	}
	return gateway.MessageTypeDocument
}

// Turns a posted event into a message event, skipping own, system and redelivered posts and gating channels
func (a *Adapter) handleEvent(ctx context.Context, event map[string]any) {
	if textOf(event["event"]) != "posted" {
		return
	}
	data, _ := event["data"].(map[string]any)
	var post map[string]any
	if err := json.Unmarshal([]byte(textOf(data["post"])), &post); err != nil || post == nil {
		return
	}
	senderID, postID := textOf(post["user_id"]), textOf(post["id"])
	botID, _ := a.botIdentity()
	if senderID == botID || truthy(post["type"]) || a.dedup.IsDuplicate(postID) {
		return
	}
	channelID := textOf(post["channel_id"])
	channelType := textOf(data["channel_type"])
	if channelType == "" {
		channelType = "O"
	}
	isDM := channelType == "D"
	text := textOf(post["message"])
	if !isDM {
		var ok bool
		if text, ok = a.gate(ctx, channelID, text); !ok {
			return
		}
	}
	threadID := textOf(post["root_id"])
	if threadID == "" && a.replyMode == "thread" && !isDM {
		threadID = postID
	}
	if trimmed := strings.TrimLeftFunc(text, unicode.IsSpace); trimmed != text && strings.HasPrefix(trimmed, "/") {
		text = trimmed
	}
	mediaURLs, mediaTypes := a.downloadAttachments(ctx, post["file_ids"])
	chatType := channelTypes[channelType]
	if chatType == "" {
		chatType = "channel"
	}
	userName := strings.TrimLeft(textOf(data["sender_name"]), "@")
	if userName == "" {
		userName = senderID
	}
	var chatName *string
	if name := textOf(data["channel_display_name"]); name != "" {
		chatName = &name
	}
	source := a.BuildSource(ctx, channelID, gateway.SourceFields{ChatName: chatName, ChatType: chatType, UserID: &senderID,
		UserName: &userName, ThreadID: &threadID, MessageID: &postID})
	msg := gateway.NewMessageEvent(text)
	msg.MessageType = messageKind(text, mediaTypes)
	msg.Source, msg.RawMessage, msg.MessageID = source, post, &postID
	msg.MediaURLs, msg.MediaTypes = mediaURLs, mediaTypes
	if prompt, ok := gateway.ResolveChannelPrompt(a.Config.Extra, channelID, ""); ok {
		msg.ChannelPrompt = &prompt
	}
	if created, ok := post["create_at"].(float64); ok && created > 0 {
		msg.Timestamp = time.UnixMilli(int64(created))
	}
	if err := a.HandleMessage(ctx, msg); err != nil && ctx.Err() == nil {
		slog.Warn(fmt.Sprintf("Mattermost: message handling failed for post %s: %v", postID, err))
	}
}
