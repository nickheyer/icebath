package ntfy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	dedupWindowSeconds = 300
	dedupMaxSize       = 1000
	streamIdleTimeout  = 90 * time.Second
	streamHealthyAfter = 60 * time.Second
	attachmentTimeout  = 60 * time.Second
	maxStreamLine      = 1 << 20
)

// Reconnect delays of the subscription, the last one repeating
var reconnectBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}

// Inbound message kind of an attachment mime prefix
var mediaKinds = []struct {
	prefix string
	kind   gateway.MessageType
}{{"image/", gateway.MessageTypePhoto}, {"audio/", gateway.MessageTypeVoice}, {"video/", gateway.MessageTypeVideo}}

// Subscription answer that no retry can fix
type fatalStatus struct {
	code, detail, log string
}

func (e *fatalStatus) Error() string { return e.detail }

// Adapter subscribes to an ntfy topic over the JSON stream and publishes replies to the publish topic
type Adapter struct {
	*gateway.BasePlatformAdapter

	server       string
	topic        string
	publishTopic string
	token        string
	markdown     bool
	dedup        *gateway.MessageDeduplicator

	mu     sync.Mutex
	client *http.Client
	cancel context.CancelFunc
	done   chan struct{}
	lastID string
}

// Adapter traits of ntfy, byte capped bodies, no edits and a serial stream reader
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.SerialReceiveLoop = true
	t.SplitsLongMessages = true
	t.SupportsMessageEditing = false
	t.MaxMessageLength = maxBodyBytes
	return t
}

// Adapter reading server, topics, token and markdown from extra and the scoped env
func NewAdapter(ctx context.Context, config *gateway.PlatformConfig) (*Adapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &Adapter{dedup: gateway.NewMessageDeduplicator(dedupMaxSize, dedupWindowSeconds)}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, platformNtfy, a)
	a.Traits = adapterTraits()
	extra := config.Extra
	var err error
	if a.server, err = serverURL(ctx, extra); err != nil {
		return nil, err
	}
	if a.topic, err = setting(ctx, extra, "topic", "NTFY_TOPIC", ""); err != nil {
		return nil, err
	}
	if a.publishTopic, err = setting(ctx, extra, "publish_topic", "NTFY_PUBLISH_TOPIC", ""); err != nil {
		return nil, err
	}
	if a.publishTopic == "" {
		a.publishTopic = a.topic
	}
	if a.token, err = setting(ctx, extra, "token", "NTFY_TOKEN", ""); err != nil {
		return nil, err
	}
	if a.markdown, err = markdownEnabled(ctx, extra); err != nil {
		return nil, err
	}
	return a, nil
}

// Message length in bytes, the unit ntfy caps bodies in
func (a *Adapter) MessageLenFn() func(string) int { return byteLen }

// HTTP client of the live subscription, nil while disconnected
func (a *Adapter) httpClient() *http.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// Starts the reconnecting topic subscription
func (a *Adapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.topic == "" {
		slog.Warn(fmt.Sprintf("[%s] NTFY_TOPIC not configured", a.Name()))
		return false, nil
	}
	a.stopStream()
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	a.mu.Lock()
	a.client = &http.Client{Transport: newTransport(ctx)}
	a.cancel, a.done = cancel, done
	a.mu.Unlock()
	go func() {
		defer close(done)
		a.runStream(sctx)
	}()
	a.MarkConnected(ctx, "")
	slog.Info(fmt.Sprintf("[%s] Connected — subscribing to %s/%s", a.Name(), a.server, a.topic))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Cancels the subscription and waits for its reader to finish
func (a *Adapter) stopStream() {
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.cancel, a.done = nil, nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Stops the subscription, drops the client and the dedup cache and marks the adapter disconnected
func (a *Adapter) Disconnect(ctx context.Context) error {
	a.stopStream()
	a.mu.Lock()
	client := a.client
	a.client = nil
	a.mu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
	a.dedup.Clear()
	a.MarkDisconnected(ctx)
	slog.Info(fmt.Sprintf("[%s] Disconnected", a.Name()))
	return nil
}

// Keeps the subscription open, backing off between attempts and stopping on an unrecoverable answer
func (a *Adapter) runStream(ctx context.Context) {
	idx := 0
	for ctx.Err() == nil {
		start := time.Now()
		err := a.consumeStream(ctx)
		if ctx.Err() != nil {
			return
		}
		var fatal *fatalStatus
		if errors.As(err, &fatal) {
			slog.Error(fatal.log)
			a.SetFatalError(ctx, fatal.code, fatal.detail, false)
			_ = a.NotifyFatalError(ctx)
			return
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("[%s] Stream error: %v", a.Name(), err))
		}
		if time.Since(start) >= streamHealthyAfter {
			idx = 0
		}
		delay := reconnectBackoff[min(idx, len(reconnectBackoff)-1)]
		slog.Info(fmt.Sprintf("[%s] Reconnecting in %ds...", a.Name(), int(delay.Seconds())))
		if !sleepCtx(ctx, delay) {
			return
		}
		idx++
	}
}

// Waits the duration, false when the context ends first
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Topic path of one or more comma separated topics, each segment escaped
func topicPath(topic string) string {
	parts := strings.Split(topic, ",")
	for i, p := range parts {
		parts[i] = url.PathEscape(strings.TrimSpace(p))
	}
	return strings.Join(parts, ",")
}

// Id of the last streamed message, where a resumed subscription picks up
func (a *Adapter) since() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastID
}

// Records the id of a streamed message for the next resume
func (a *Adapter) setSince(id string) {
	a.mu.Lock()
	a.lastID = id
	a.mu.Unlock()
}

// Fatal answer of a subscription status, nil for statuses a retry may fix
func (a *Adapter) fatalFor(status int) *fatalStatus {
	switch status {
	case http.StatusUnauthorized:
		return &fatalStatus{code: "ntfy_unauthorized", detail: "ntfy server rejected auth (401). Check NTFY_TOKEN.",
			log: fmt.Sprintf("[%s] Authentication failed (401) — stopping reconnect loop. Check NTFY_TOKEN.", a.Name())}
	case http.StatusForbidden:
		return &fatalStatus{code: "ntfy_forbidden", detail: fmt.Sprintf("ntfy server denied access to topic '%s' (403). Check NTFY_TOKEN permissions.", a.topic),
			log: fmt.Sprintf("[%s] Access denied (403) to %s — stopping reconnect loop.", a.Name(), a.topic)}
	case http.StatusNotFound:
		return &fatalStatus{code: "ntfy_topic_not_found", detail: fmt.Sprintf("ntfy topic '%s' returned 404. Check NTFY_TOPIC.", a.topic),
			log: fmt.Sprintf("[%s] Topic not found (404): %s — stopping reconnect loop.", a.Name(), a.topic)}
	}
	return nil
}

// One streaming subscription resumed after the last seen message, dispatching message events until it ends
func (a *Adapter) consumeStream(ctx context.Context) error {
	client := a.httpClient()
	if client == nil {
		return errors.New("HTTP client not initialized")
	}
	query := url.Values{"poll": {"false"}}
	if since := a.since(); since != "" {
		query.Set("since", since)
	}
	streamURL := a.server + "/" + topicPath(a.topic) + "/json?" + query.Encode()
	slog.Debug(fmt.Sprintf("[%s] Opening stream to %s", a.Name(), streamURL))
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var idle atomic.Bool
	timer := time.AfterFunc(streamIdleTimeout, func() {
		idle.Store(true)
		cancel()
	})
	defer timer.Stop()
	idleErr := fmt.Errorf("no data from %s within %s", a.server, streamIdleTimeout)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, streamURL, nil)
	if err != nil {
		return err
	}
	if auth := authHeader(a.token); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		if idle.Load() {
			return idleErr
		}
		return err
	}
	defer resp.Body.Close()
	if fatal := a.fatalFor(resp.StatusCode); fatal != nil {
		return fatal
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, a.server)
	}
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, err := readLine(reader)
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			timer.Stop()
			var event map[string]any
			if json.Unmarshal(trimmed, &event) == nil && textOf(event["event"]) == "message" {
				a.onMessage(ctx, event)
			}
			timer.Reset(streamIdleTimeout)
		}
		if err != nil {
			switch {
			case errors.Is(err, io.EOF):
				return nil
			case idle.Load():
				return idleErr
			case ctx.Err() != nil:
				return nil
			}
			return err
		}
	}
}

// Next newline terminated line, an error for a line beyond the stream line cap
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxStreamLine {
			return nil, fmt.Errorf("stream line exceeds %d bytes", maxStreamLine)
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

// Whether the event tags carry the echo tag of this adapter's own publishes
func hasEchoTag(raw any) bool {
	tags, _ := raw.([]any)
	for _, t := range tags {
		if textOf(t) == echoTag {
			return true
		}
	}
	return false
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

// Cached path and mime type of an attachment, server files fetched with the token, others SSRF guarded
func (a *Adapter) downloadAttachment(ctx context.Context, raw any) (string, string, bool) {
	att, ok := raw.(map[string]any)
	if !ok {
		return "", "", false
	}
	attURL := textOf(att["url"])
	if attURL == "" {
		return "", "", false
	}
	client := a.httpClient()
	auth := ""
	if strings.HasPrefix(attURL, a.server+"/") {
		auth = authHeader(a.token)
	} else {
		env := gateway.URLSafetyEnv(ctx)
		if !tools.IsSafeURL(env, attURL) {
			slog.Warn(fmt.Sprintf("[%s] Blocked unsafe attachment URL (SSRF protection)", a.Name()))
			return "", "", false
		}
		client = tools.NewSSRFSafeClient(env, attachmentTimeout, true)
	}
	if client == nil {
		return "", "", false
	}
	rctx, cancel := context.WithTimeout(ctx, attachmentTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, attURL, nil)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Bad attachment URL: %v", a.Name(), err))
		return "", "", false
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Attachment download failed for %s: %v", a.Name(), gateway.SafeURLForLog(attURL, 80), err))
		return "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		slog.Warn(fmt.Sprintf("[%s] Attachment download failed for %s: HTTP %d", a.Name(), gateway.SafeURLForLog(attURL, 80), resp.StatusCode))
		return "", "", false
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Attachment download failed for %s: %v", a.Name(), gateway.SafeURLForLog(attURL, 80), err))
		return "", "", false
	}
	mimeType := textOf(att["type"])
	if mimeType == "" {
		mimeType, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	name := textOf(att["name"])
	if name == "" {
		name = attURL[strings.LastIndex(attURL, "/")+1:]
	}
	path, err := cacheAttachment(ctx, data, name, mimeType)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Attachment cache failed: %v", a.Name(), err))
		return "", "", false
	}
	return path, mimeType, true
}

// Hands one streamed message to the gateway as a topic scoped message, skipping duplicates and own publishes
func (a *Adapter) onMessage(ctx context.Context, event map[string]any) {
	msgID := textOf(event["id"])
	if msgID != "" {
		a.setSince(msgID)
	} else {
		msgID = randomID(32)
	}
	if a.dedup.IsDuplicate(msgID) {
		slog.Debug(fmt.Sprintf("[%s] Duplicate message %s, skipping", a.Name(), msgID))
		return
	}
	if hasEchoTag(event["tags"]) {
		slog.Debug(fmt.Sprintf("[%s] Skipping own message (echo tag)", a.Name()))
		return
	}
	text := strings.TrimSpace(textOf(event["message"]))
	kind := gateway.MessageTypeText
	var mediaURLs, mediaTypes []string
	if path, mimeType, ok := a.downloadAttachment(ctx, event["attachment"]); ok {
		mediaURLs, mediaTypes = []string{path}, []string{mimeType}
		kind = gateway.MessageTypeDocument
		for _, mk := range mediaKinds {
			if strings.HasPrefix(mimeType, mk.prefix) {
				kind = mk.kind
				break
			}
		}
	}
	if text == "" && len(mediaURLs) == 0 {
		slog.Debug(fmt.Sprintf("[%s] Empty message body, skipping", a.Name()))
		return
	}
	topic := textOf(event["topic"])
	if topic == "" {
		topic = a.topic
	}
	source := a.BuildSource(ctx, topic, gateway.SourceFields{ChatName: &topic, ChatType: "dm", UserID: &topic, UserName: &topic, MessageID: &msgID})
	msg := gateway.NewMessageEvent(text)
	msg.MessageType, msg.Source, msg.MessageID, msg.RawMessage = kind, source, &msgID, event
	msg.MediaURLs, msg.MediaTypes = mediaURLs, mediaTypes
	if unix, ok := event["time"].(float64); ok && unix > 0 {
		msg.Timestamp = time.Unix(int64(unix), 0).UTC()
	}
	slog.Debug(fmt.Sprintf("[%s] Message on topic %s: %.80s", a.Name(), topic, text))
	if err := a.HandleMessage(ctx, msg); err != nil && ctx.Err() == nil {
		slog.Warn(fmt.Sprintf("[%s] Message handling failed for %s: %v", a.Name(), msgID, err))
	}
}

// Topic a reply publishes to, the metadata override, then the publish topic, then the chat
func (a *Adapter) targetTopic(chatID string, metadata map[string]any) string {
	if t := textOf(metadata["publish_topic"]); t != "" {
		return t
	}
	if a.publishTopic != "" {
		return a.publishTopic
	}
	return chatID
}

// Send result of one publish outcome, transport failures logged
func (a *Adapter) publishResult(ctx context.Context, p publishRequest) (gateway.SendResult, error) {
	client := a.httpClient()
	if client == nil {
		return gateway.SendResult{Success: false, Error: "HTTP client not initialized"}, nil
	}
	id, failure, err := publish(ctx, client, a.server, a.token, p)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		slog.Error(fmt.Sprintf("[%s] Send error: %v", a.Name(), err))
		return gateway.SendResult{Success: false, Error: err.Error(), Retryable: true}, nil
	}
	if failure != "" {
		slog.Warn(fmt.Sprintf("[%s] Send failed %s", a.Name(), failure))
		return gateway.SendResult{Success: false, Error: failure, Retryable: gateway.IsRetryableErrorText(failure)}, nil
	}
	return gateway.SendResult{Success: true, MessageID: id}, nil
}

// Publishes the content in body sized chunks to the publish topic
func (a *Adapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if a.httpClient() == nil {
		return gateway.SendResult{Success: false, Error: "HTTP client not initialized"}, nil
	}
	if content == "" {
		return gateway.SendResult{Success: true}, nil
	}
	topic := a.targetTopic(chatID, metadata)
	result, err := gateway.SendChunks(ctx, bodyChunks(content), func(ctx context.Context, chunk string) (*gateway.SendResult, error) {
		r, err := a.publishResult(ctx, publishRequest{topic: topic, body: []byte(chunk), markdown: a.markdown})
		if err != nil {
			return nil, err
		}
		return &r, nil
	})
	if err != nil {
		return gateway.SendResult{}, err
	}
	return *result, nil
}

// Chat info of a topic
func (a *Adapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"name": chatID, "type": "dm"}, nil
}

// Uploads a local file as an attachment of one publish with the caption as its message
func (a *Adapter) sendFile(ctx context.Context, chatID, path, caption, fileName string, metadata map[string]any) (gateway.SendResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] Local file unreadable: %s: %v", a.Name(), path, err))
		return gateway.SendResult{Success: false, Error: "File not found: " + path}, nil
	}
	return a.publishResult(ctx, filePublish(a.targetTopic(chatID, metadata), path, caption, fileName, a.markdown, data))
}

// Publishes the caption with the image URL as an external attachment
func (a *Adapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.publishResult(ctx, publishRequest{topic: a.targetTopic(chatID, metadata), body: []byte(caption), attach: imageURL, markdown: a.markdown})
}

// Uploads a local image as an attachment
func (a *Adapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFile(ctx, chatID, imagePath, caption, "", metadata)
}

// Uploads a local document under its display name as an attachment
func (a *Adapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFile(ctx, chatID, filePath, caption, fileName, metadata)
}

// Uploads a local audio file as an attachment
func (a *Adapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFile(ctx, chatID, audioPath, caption, "", metadata)
}

// Uploads a local video as an attachment
func (a *Adapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFile(ctx, chatID, videoPath, caption, "", metadata)
}
