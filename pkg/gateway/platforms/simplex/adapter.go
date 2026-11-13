package simplex

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/mediacache"
)

const (
	platformName          gateway.Platform = "simplex"
	maxMessageLength                       = 8000
	defaultWSURL                           = "ws://127.0.0.1:5225"
	retryInitial                           = 2 * time.Second
	retryMax                               = 60 * time.Second
	healthInterval                         = 30 * time.Second
	staleThreshold                         = 300 * time.Second
	defaultTextBatchDelay                  = 0.8
	textBatchDelayEnv                      = "ICEBATH_SIMPLEX_TEXT_BATCH_DELAY"
	requestedFilesCap                      = 1000
)

var (
	mediaTagRE   = regexp.MustCompile(`MEDIA:(\S+)`)
	voiceTagExts = map[string]bool{".ogg": true, ".mp3": true, ".wav": true, ".m4a": true, ".opus": true}
	imageExts    = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true}
	audioExts    = map[string]bool{".mp3": true, ".wav": true, ".ogg": true, ".m4a": true, ".aac": true, ".opus": true}
)

// Live adapters by daemon URL so the standalone sender reuses an in process connection
var live = struct {
	sync.Mutex
	byURL map[string]*Adapter
}{byURL: map[string]*Adapter{}}

// SimpleX Chat adapter over the simplex-chat daemon WebSocket API
type Adapter struct {
	*gateway.BasePlatformAdapter

	wsURL      string
	autoAccept bool
	groupAllow map[string]bool

	mu           sync.Mutex
	client       *wsClient
	stopped      bool
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	queue        *eventQueue
	lastActivity atomic.Int64

	requested      map[string]bool
	requestedOrder []string
}

// Class attributes of the SimpleX adapter
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.MaxMessageLength = maxMessageLength
	t.SerialReceiveLoop = true
	return t
}

// Adapter configured from extra and the profile scoped SIMPLEX env
func NewAdapter(ctx context.Context, config *gateway.PlatformConfig) (*Adapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &Adapter{requested: map[string]bool{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, platformName, a)
	a.Traits = adapterTraits()
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	wsURL := strings.TrimSpace(str(extra, "ws_url"))
	if wsURL == "" {
		env, err := gateway.GetScopedSecret(ctx, "SIMPLEX_WS_URL", "", false)
		if err != nil {
			return nil, err
		}
		wsURL = strings.TrimSpace(env)
	}
	if wsURL == "" {
		wsURL = defaultWSURL
	}
	a.wsURL = strings.TrimRight(wsURL, "/")
	envAuto, found, err := gateway.LookupScopedSecret(ctx, "SIMPLEX_AUTO_ACCEPT", false)
	if err != nil {
		return nil, err
	}
	if found {
		a.autoAccept = acceptFlag(envAuto)
	} else {
		a.autoAccept = extraFlag(extra["auto_accept"], true)
	}
	groupAllowed, err := gateway.GetScopedSecret(ctx, "SIMPLEX_GROUP_ALLOWED", "", false)
	if err != nil {
		return nil, err
	}
	a.groupAllow = map[string]bool{}
	for _, id := range allowEntries(groupAllowed, extra["group_allowed"]) {
		a.groupAllow[id] = true
	}
	delay := defaultTextBatchDelay
	rawDelay, err := gateway.GetScopedSecret(ctx, textBatchDelayEnv, "", false)
	if err != nil {
		return nil, err
	}
	if rawDelay = strings.TrimSpace(rawDelay); rawDelay != "" {
		if delay, err = strconv.ParseFloat(rawDelay, 64); err != nil || delay < 0 {
			return nil, fmt.Errorf("%s must be a non-negative number of seconds, got %q", textBatchDelayEnv, rawDelay)
		}
	}
	a.SetTextBatchDelays(delay, delay)
	groups := "disabled"
	if len(a.groupAllow) > 0 {
		groups = "enabled"
	}
	slog.Info(fmt.Sprintf("[simplex] adapter initialized: url=%s auto_accept=%t groups=%s", a.wsURL, a.autoAccept, groups))
	return a, nil
}

// Auto accept value of the env var, off for 0, false, no and blank
func acceptFlag(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "":
		return false
	}
	return true
}

// Boolean of a config extra value, the default when unset
func extraFlag(v any, def bool) bool {
	switch x := v.(type) {
	case nil:
		return def
	case bool:
		return x
	case string:
		return acceptFlag(x)
	}
	return def
}

// Group ids from the env list, else from the extra list or comma string
func allowEntries(env string, extra any) []string {
	source := env
	if strings.TrimSpace(source) == "" {
		switch x := extra.(type) {
		case string:
			source = x
		case []any:
			parts := make([]string, 0, len(x))
			for _, v := range x {
				parts = append(parts, fmt.Sprint(v))
			}
			source = strings.Join(parts, ",")
		}
	}
	var out []string
	for _, part := range strings.Split(source, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Short form of an id for logs
func redactID(id string) string {
	if id == "" {
		return "<none>"
	}
	if len(id) <= 4 {
		return id
	}
	return id[:2] + "**" + id[len(id)-2:]
}

// Connects to the daemon and starts the receive, event and idle monitor loops
func (a *Adapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.wsURL == "" {
		slog.Error("[simplex] SIMPLEX_WS_URL is required")
		return false, nil
	}
	warnNameAllowlistEntries(ctx)
	first, err := dialClient(ctx, a.wsURL)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		slog.Error(fmt.Sprintf("[simplex] cannot reach daemon at %s: %v", a.wsURL, err))
		return false, nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a.mu.Lock()
	a.stopped, a.cancel, a.queue = false, cancel, newEventQueue()
	a.mu.Unlock()
	a.touch()
	a.wg.Add(3)
	go a.listen(runCtx, first)
	go a.processEvents(runCtx)
	go a.monitorIdle(runCtx)
	live.Lock()
	live.byURL[a.wsURL] = a
	live.Unlock()
	a.MarkConnected(ctx, "")
	slog.Info("[simplex] connected to " + a.wsURL)
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops the loops, closes the daemon connection and drops pending text batches
func (a *Adapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	a.stopped = true
	cancel, client := a.cancel, a.client
	a.cancel, a.client = nil, nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if client != nil {
		client.close()
	}
	a.wg.Wait()
	live.Lock()
	if live.byURL[a.wsURL] == a {
		delete(live.byURL, a.wsURL)
	}
	live.Unlock()
	for _, task := range a.TextBatchTasks() {
		task.Cancel()
	}
	a.ClearTextBatches()
	a.MarkDisconnected(ctx)
	slog.Info("[simplex] disconnected")
	return nil
}

// Records daemon traffic for the idle monitor
func (a *Adapter) touch() { a.lastActivity.Store(time.Now().UnixNano()) }

// Connection in use, nil while reconnecting or stopped
func (a *Adapter) liveClient() *wsClient {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// Installs a fresh connection unless the adapter stopped meanwhile
func (a *Adapter) adopt(c *wsClient) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return false
	}
	a.client = c
	return true
}

// Reads the daemon connection, reconnecting with jittered exponential backoff until stopped
func (a *Adapter) listen(ctx context.Context, c *wsClient) {
	defer a.wg.Done()
	backoff := retryInitial
	for {
		if c == nil {
			dialed, err := dialClient(ctx, a.wsURL)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn(fmt.Sprintf("[simplex] daemon connect failed: %v (reconnecting in %.0fs)", err, backoff.Seconds()))
				if !sleepJitter(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, retryMax)
				continue
			}
			c, backoff = dialed, retryInitial
			slog.Info("[simplex] daemon connection restored")
			a.WriteRuntimeStatusSafe(ctx, "connected", map[string]any{"platform_state": "connected", "error_code": nil, "error_message": nil})
		}
		c.touch = a.touch
		if !a.adopt(c) {
			c.close()
			return
		}
		a.touch()
		queue := a.queue
		err := c.readLoop(queue.push)
		a.mu.Lock()
		if a.client == c {
			a.client = nil
		}
		a.mu.Unlock()
		c.shutdown()
		if ctx.Err() != nil {
			return
		}
		slog.Warn(fmt.Sprintf("[simplex] daemon connection closed: %v (reconnecting in %.0fs)", err, backoff.Seconds()))
		a.WriteRuntimeStatusSafe(ctx, "reconnecting", map[string]any{"platform_state": "retrying",
			"error_code": "simplex_ws_disconnected", "error_message": "SimpleX daemon connection lost, reconnecting"})
		c = nil
		if !sleepJitter(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, retryMax)
	}
}

// Sleeps the delay plus up to a fifth of it, false when stopped first
func sleepJitter(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d + time.Duration(rand.Float64()*0.2*float64(d)))
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Logs long application silence without reconnecting since the daemon is legitimately quiet for hours
func (a *Adapter) monitorIdle(ctx context.Context) {
	defer a.wg.Done()
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if idle := time.Since(time.Unix(0, a.lastActivity.Load())); idle > staleThreshold {
				slog.Debug(fmt.Sprintf("[simplex] daemon connection application-idle for %.0fs", idle.Seconds()))
			}
		}
	}
}

// Text batches keyed by chat so rapid fire messages in one chat merge
func (a *Adapter) TextBatchKey(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	return string(platformName) + ":" + event.Source.ChatID, nil
}

// Sends the text and every MEDIA tag file, audio tags as voice notes and the rest as documents
func (a *Adapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	c := a.liveClient()
	if c == nil {
		return gateway.SendResult{Success: false, Error: errNotConnected.Error(), Retryable: true}, nil
	}
	var paths []string
	for _, m := range mediaTagRE.FindAllStringSubmatch(content, -1) {
		paths = append(paths, m[1])
	}
	if len(paths) > 0 {
		content = strings.TrimSpace(mediaTagRE.ReplaceAllString(content, ""))
	}
	s := sender{c}
	last := gateway.SendResult{Success: true}
	if content != "" {
		last = s.text(ctx, chatID, content)
		if !last.Success {
			return last, nil
		}
	}
	for _, path := range paths {
		var r gateway.SendResult
		if voiceTagExts[strings.ToLower(filepath.Ext(path))] {
			r = s.voice(ctx, chatID, path, "", 0)
		} else {
			r = s.document(ctx, chatID, path, "", "")
		}
		if !r.Success {
			return r, nil
		}
		last = r
	}
	return last, nil
}

// Replaces the text of a message this adapter sent
func (a *Adapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	c := a.liveClient()
	if c == nil {
		return gateway.SendResult{Success: false, Error: errNotConnected.Error(), Retryable: true}, nil
	}
	return sender{c}.edit(ctx, chatID, messageID, content), nil
}

// Image by URL, file URLs read in place and web URLs downloaded into the image cache first
func (a *Adapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	path, err := localImagePath(ctx, imageURL)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[simplex] failed to download image: %v", err))
		return gateway.SendResult{Success: false, Error: err.Error()}, nil
	}
	return a.sendImagePath(ctx, chatID, path, caption)
}

// Local path of an image URL, downloading http and https URLs
func localImagePath(ctx context.Context, imageURL string) (string, error) {
	if rest, ok := strings.CutPrefix(imageURL, "file://"); ok {
		return unescapePath(rest), nil
	}
	return gateway.CacheImageFromURL(ctx, imageURL, ".jpg", 2)
}

// Percent decoded file URL path, the raw text when it holds no valid escapes
func unescapePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '%' && i+2 < len(p) {
			if n, err := strconv.ParseUint(p[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 2
				continue
			}
		}
		b.WriteByte(p[i])
	}
	return b.String()
}

// Local image sent inline with a preview
func (a *Adapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendImagePath(ctx, chatID, imagePath, caption)
}

// Image path sent through the live connection
func (a *Adapter) sendImagePath(ctx context.Context, chatID, path, caption string) (gateway.SendResult, error) {
	c := a.liveClient()
	if c == nil {
		return gateway.SendResult{Success: false, Error: errNotConnected.Error(), Retryable: true}, nil
	}
	return sender{c}.image(ctx, chatID, path, caption), nil
}

// Video sent as a file attachment
func (a *Adapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.SendDocument(ctx, chatID, videoPath, caption, "", replyTo, metadata, extra)
}

// File attachment under its own or the given file name
func (a *Adapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	c := a.liveClient()
	if c == nil {
		return gateway.SendResult{Success: false, Error: errNotConnected.Error(), Retryable: true}, nil
	}
	return sender{c}.document(ctx, chatID, filePath, caption, fileName), nil
}

// Audio sent as an inline voice note with the duration extra in seconds
func (a *Adapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	c := a.liveClient()
	if c == nil {
		return gateway.SendResult{Success: false, Error: errNotConnected.Error(), Retryable: true}, nil
	}
	return sender{c}.voice(ctx, chatID, audioPath, caption, durationOf(extra["duration"])), nil
}

// Whole seconds of a duration value, zero when absent or invalid
func durationOf(v any) int {
	switch d := v.(type) {
	case int:
		return max(d, 0)
	case int64:
		return int(max(d, 0))
	case float64:
		return int(max(d, 0))
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(d)); err == nil {
			return max(n, 0)
		}
	}
	return 0
}

// Contacts by contactId and groups as group ids for the channel directory, nil while the daemon is unreachable
func (a *Adapter) ListChannels(ctx context.Context) (any, error) {
	c := a.liveClient()
	if c == nil {
		return nil, nil
	}
	contacts, err := listContacts(ctx, c, directoryTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Debug(fmt.Sprintf("[simplex] contact listing failed: %v", err))
		return nil, nil
	}
	channels := []any{}
	for _, contact := range contacts {
		id := idText(contact["contactId"])
		if id == "" {
			continue
		}
		name := displayName(contact, "profile")
		if name == "" {
			name = id
		}
		channels = append(channels, map[string]any{"id": id, "name": name, "type": "dm"})
	}
	groups, err := listGroups(ctx, c, directoryTimeout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Debug(fmt.Sprintf("[simplex] group listing failed: %v", err))
	}
	for _, group := range groups {
		id := idText(group["groupId"])
		name := displayName(group, "groupProfile")
		if name == "" {
			name = id
		}
		channels = append(channels, map[string]any{"id": "group:" + id, "name": name, "type": "group"})
	}
	return channels, nil
}

// Chat type and name of a contact or group:<id> chat
func (a *Adapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	if rest, ok := strings.CutPrefix(chatID, "group:"); ok {
		return map[string]any{"chat_id": chatID, "type": "group", "name": rest}, nil
	}
	return map[string]any{"chat_id": chatID, "type": "dm", "name": chatID}, nil
}

// Composed message sends over one daemon connection
type sender struct{ c *wsClient }

// Sends composed messages to the target and returns the new item id
func (s sender) items(ctx context.Context, chatID string, items []map[string]any, label string) gateway.SendResult {
	ref, err := chatRef(ctx, s.c, chatID)
	if err != nil {
		return gateway.SendResult{Success: false, Error: label + ": " + err.Error()}
	}
	payload, err := marshal(items)
	if err != nil {
		return gateway.SendResult{Success: false, Error: label + ": " + err.Error()}
	}
	resp, err := s.c.command(ctx, "/_send "+ref+" json "+string(payload), commandTimeout)
	if err != nil {
		slog.Warn(fmt.Sprintf("[simplex] send to %s failed: %v", redactID(chatID), err))
		return gateway.SendResult{Success: false, Error: label + ": " + err.Error()}
	}
	return gateway.SendResult{Success: true, MessageID: sentItemID(resp), RawResponse: resp}
}

// Item id of the first chat item a send reply carries, nil when absent
func sentItemID(resp map[string]any) any {
	list, _ := resp["chatItems"].([]any)
	for _, raw := range list {
		item, _ := raw.(map[string]any)
		if id := idText(obj(obj(item, "chatItem"), "meta")["itemId"]); id != "" {
			return id
		}
	}
	return nil
}

// Plain text message
func (s sender) text(ctx context.Context, chatID, text string) gateway.SendResult {
	return s.items(ctx, chatID, []map[string]any{{"msgContent": map[string]any{"type": "text", "text": text}}}, "Failed to send message")
}

// Updates a sent text item in place
func (s sender) edit(ctx context.Context, chatID, messageID, text string) gateway.SendResult {
	if !isDigits(messageID) {
		return gateway.SendResult{Success: false, Error: "SimpleX message id must be numeric, got " + strconv.Quote(messageID)}
	}
	ref, err := chatRef(ctx, s.c, chatID)
	if err != nil {
		return gateway.SendResult{Success: false, Error: "Failed to edit message: " + err.Error()}
	}
	payload, err := marshal(map[string]any{"msgContent": map[string]any{"type": "text", "text": text}, "mentions": map[string]any{}})
	if err != nil {
		return gateway.SendResult{Success: false, Error: "Failed to edit message: " + err.Error()}
	}
	resp, err := s.c.command(ctx, "/_update item "+ref+" "+messageID+" json "+string(payload), commandTimeout)
	if err != nil {
		return gateway.SendResult{Success: false, Error: "Failed to edit message: " + err.Error()}
	}
	return gateway.SendResult{Success: true, MessageID: messageID, RawResponse: resp}
}

// Inline image with a JPEG preview, non PNG and JPEG sources converted to PNG first
func (s sender) image(ctx context.Context, chatID, path, caption string) gateway.SendResult {
	if !isFile(path) {
		return gateway.SendResult{Success: false, Error: "Image file not found"}
	}
	sendPath, thumb, err := prepareImage(ctx, path)
	if err != nil {
		slog.Warn(fmt.Sprintf("[simplex] failed to prepare image: %v", err))
		return gateway.SendResult{Success: false, Error: "Failed to prepare image: " + err.Error()}
	}
	item := map[string]any{"filePath": sendPath, "msgContent": map[string]any{"type": "image", "image": thumb, "text": caption}}
	return s.items(ctx, chatID, []map[string]any{item}, "Failed to send image")
}

// File attachment, copied under fileName first when the caller names it differently
func (s sender) document(ctx context.Context, chatID, path, caption, fileName string) gateway.SendResult {
	if !isFile(path) {
		return gateway.SendResult{Success: false, Error: "File not found"}
	}
	sendPath, err := namedCopy(ctx, path, fileName)
	if err != nil {
		return gateway.SendResult{Success: false, Error: "Failed to send document: " + err.Error()}
	}
	item := map[string]any{"filePath": sendPath, "msgContent": map[string]any{"type": "file", "text": caption}}
	return s.items(ctx, chatID, []map[string]any{item}, "Failed to send document")
}

// Inline voice note
func (s sender) voice(ctx context.Context, chatID, path, caption string, duration int) gateway.SendResult {
	if !isFile(path) {
		return gateway.SendResult{Success: false, Error: "Voice file not found"}
	}
	item := map[string]any{"msgContent": map[string]any{"type": "voice", "text": caption, "duration": duration},
		"fileSource": map[string]any{"filePath": path}}
	return s.items(ctx, chatID, []map[string]any{item}, "Failed to send voice message")
}

// Media file sent by kind, images inline, audio as voice notes and everything else as files
func (s sender) media(ctx context.Context, chatID string, m gateway.MediaRef, forceDocument bool) gateway.SendResult {
	ext := strings.ToLower(filepath.Ext(m.Path))
	switch {
	case forceDocument:
		return s.document(ctx, chatID, m.Path, "", "")
	case imageExts[ext]:
		return s.image(ctx, chatID, m.Path, "")
	case audioExts[ext]:
		return s.voice(ctx, chatID, m.Path, "", 0)
	}
	return s.document(ctx, chatID, m.Path, "", "")
}

// Whether path names an existing regular file
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// Path the daemon reads the attachment from, a copy in the document cache named fileName when it differs
func namedCopy(ctx context.Context, path, fileName string) (string, error) {
	name := filepath.Base(strings.TrimSpace(fileName))
	if fileName == "" || name == "." || name == string(filepath.Separator) || name == filepath.Base(path) {
		return path, nil
	}
	root, err := mediacache.DocumentCacheDir(ctx, home.Dir(ctx))
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(root, "simplex_")
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, name)
	if err := os.WriteFile(out, data, 0o600); err != nil {
		return "", err
	}
	return out, nil
}

// Unbounded FIFO of daemon frames so the reader never waits on the event loop
type eventQueue struct {
	mu     sync.Mutex
	items  []map[string]any
	notify chan struct{}
}

func newEventQueue() *eventQueue { return &eventQueue{notify: make(chan struct{}, 1)} }

// Appends a frame and wakes the event loop
func (q *eventQueue) push(frame map[string]any) {
	q.mu.Lock()
	q.items = append(q.items, frame)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// Next frame in arrival order, false once the context ends
func (q *eventQueue) pop(ctx context.Context) (map[string]any, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			item := q.items[0]
			q.items[0] = nil
			q.items = q.items[1:]
			q.mu.Unlock()
			return item, true
		}
		q.mu.Unlock()
		select {
		case <-q.notify:
		case <-ctx.Done():
			return nil, false
		}
	}
}
