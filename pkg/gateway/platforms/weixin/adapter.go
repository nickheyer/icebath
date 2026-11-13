package weixin

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
)

const (
	maxConsecutiveFailures = 3
	retryDelay             = 2 * time.Second
	backoffDelay           = 30 * time.Second
	sessionExpiredPause    = 10 * time.Minute
	messageDedupTTL        = 300
	splitThreshold         = 1800
)

// Personal WeChat adapter over the Tencent iLink bot API
type Adapter struct {
	*gateway.BasePlatformAdapter
	*gateway.OwnAccessPolicyMixin

	homeDir        string
	accountID      string
	token          string
	baseURL        string
	cdnBaseURL     string
	splitMultiline bool

	chunkDelay       time.Duration
	chunkRetries     int
	chunkRetryDelay  time.Duration
	circuitThreshold int
	circuitWindow    time.Duration
	circuitOpen      time.Duration

	tokens *contextTokenStore
	typing typingTicketCache
	dedup  *gateway.MessageDeduplicator

	mu         sync.Mutex
	pollClient *http.Client
	sendClient *http.Client
	pollCancel context.CancelFunc
	pollDone   chan struct{}
	orderTail  map[string]chan struct{}

	sendGate     sync.Mutex
	circuitMu    sync.Mutex
	circuitUntil time.Time
	rateEvents   []time.Time
}

// Class attributes of the Weixin adapter over the base defaults
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.SupportsCodeBlocks = true
	t.SplitsLongMessages = true
	t.MaxMessageLength = maxMessageLength
	t.SupportsMessageEditing = false
	return t
}

// Text of a config value, empty for nil
func stringOf(v any) string {
	switch s := v.(type) {
	case nil:
		return ""
	case string:
		return s
	}
	return fmt.Sprint(v)
}

// Whether a config value is set to something other than an empty or zero value
func present(v any) bool {
	switch s := v.(type) {
	case nil:
		return false
	case string:
		return s != ""
	case bool:
		return s
	case int:
		return s != 0
	case int64:
		return s != 0
	case float64:
		return s != 0
	}
	return true
}

// Scoped WEIXIN env value of the key, then the config extra value, then the default, trimmed
func setting(ctx context.Context, extra map[string]any, key, def string) (string, error) {
	v, err := gateway.ExtraOrSecret(ctx, extra, key, "WEIXIN_"+strings.ToUpper(key), def, true)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(stringOf(v)), nil
}

// Bot token from the config token, the extra token, then the scoped WEIXIN_TOKEN
func resolveToken(ctx context.Context, cfg *gateway.PlatformConfig, extra map[string]any) (string, error) {
	if cfg != nil && present(cfg.Token) {
		return strings.TrimSpace(stringOf(cfg.Token)), nil
	}
	if present(extra["token"]) {
		return strings.TrimSpace(stringOf(extra["token"])), nil
	}
	token, err := gateway.GetScopedSecret(ctx, "WEIXIN_TOKEN", "", false)
	return strings.TrimSpace(token), err
}

// Allowlist entries of a comma separated string or a list, blanks dropped
func coerceList(v any) map[string]bool {
	out := map[string]bool{}
	var items []string
	switch s := v.(type) {
	case nil:
	case string:
		items = strings.Split(s, ",")
	case []any:
		for _, item := range s {
			items = append(items, stringOf(item))
		}
	case []string:
		items = s
	default:
		items = []string{stringOf(v)}
	}
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			out[item] = true
		}
	}
	return out
}

// Boolean of a config string, unknown values reading as the default
func coerceBool(value string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// Duration of a seconds setting, an error naming the key when it is not a number
func secondsSetting(ctx context.Context, extra map[string]any, key, def string) (time.Duration, error) {
	raw, err := setting(ctx, extra, key, def)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("weixin %s must be a number of seconds, got %q", key, raw)
	}
	return time.Duration(f * float64(time.Second)), nil
}

// Integer setting, an error naming the key when it is not an integer
func intSetting(ctx context.Context, extra map[string]any, key, def string) (int, error) {
	raw, err := setting(ctx, extra, key, def)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("weixin %s must be an integer, got %q", key, raw)
	}
	return n, nil
}

// Allowlist from the extra key when present, else from the scoped env var
func allowlistSetting(ctx context.Context, extra map[string]any, key, env string) (map[string]bool, error) {
	if v, ok := extra[key]; ok && v != nil {
		return coerceList(v), nil
	}
	raw, err := gateway.GetScopedSecret(ctx, env, "", false)
	if err != nil {
		return nil, err
	}
	return coerceList(raw), nil
}

// Adapter configured from the platform config, the scoped WEIXIN env and any credentials QR login saved
func NewAdapter(ctx context.Context, config *gateway.PlatformConfig) (*Adapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	policy, err := gateway.NewOwnAccessPolicyMixin("WeixinAdapter", "WEIXIN")
	if err != nil {
		return nil, err
	}
	homeDir := home.Dir(ctx)
	a := &Adapter{OwnAccessPolicyMixin: policy, homeDir: homeDir, tokens: newContextTokenStore(homeDir),
		dedup: gateway.NewMessageDeduplicator(gateway.DefaultDedupMaxSize, messageDedupTTL), orderTail: map[string]chan struct{}{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformWeixin, a)
	a.Traits = adapterTraits()
	if a.accountID, err = setting(ctx, extra, "account_id", ""); err != nil {
		return nil, err
	}
	if a.token, err = resolveToken(ctx, config, extra); err != nil {
		return nil, err
	}
	if a.baseURL, err = setting(ctx, extra, "base_url", defaultBaseURL); err != nil {
		return nil, err
	}
	if a.cdnBaseURL, err = setting(ctx, extra, "cdn_base_url", defaultCDNBaseURL); err != nil {
		return nil, err
	}
	a.baseURL, a.cdnBaseURL = strings.TrimRight(a.baseURL, "/"), strings.TrimRight(a.cdnBaseURL, "/")
	if a.chunkDelay, err = secondsSetting(ctx, extra, "send_chunk_delay_seconds", "1.5"); err != nil {
		return nil, err
	}
	if a.chunkRetries, err = intSetting(ctx, extra, "send_chunk_retries", "4"); err != nil {
		return nil, err
	}
	if a.chunkRetryDelay, err = secondsSetting(ctx, extra, "send_chunk_retry_delay_seconds", "1.0"); err != nil {
		return nil, err
	}
	if a.circuitThreshold, err = intSetting(ctx, extra, "rate_limit_circuit_threshold", "1"); err != nil {
		return nil, err
	}
	a.circuitThreshold = max(1, a.circuitThreshold)
	if a.circuitWindow, err = secondsSetting(ctx, extra, "rate_limit_circuit_window_seconds", "30.0"); err != nil {
		return nil, err
	}
	if a.circuitOpen, err = secondsSetting(ctx, extra, "rate_limit_circuit_open_seconds", "30.0"); err != nil {
		return nil, err
	}
	dmPolicy, err := setting(ctx, extra, "dm_policy", "pairing")
	if err != nil {
		return nil, err
	}
	groupPolicy, err := setting(ctx, extra, "group_policy", "disabled")
	if err != nil {
		return nil, err
	}
	policy.DMPolicy, policy.GroupPolicy = strings.ToLower(dmPolicy), strings.ToLower(groupPolicy)
	if policy.AllowFrom, err = allowlistSetting(ctx, extra, "allow_from", "WEIXIN_ALLOWED_USERS"); err != nil {
		return nil, err
	}
	if policy.GroupAllowFrom, err = allowlistSetting(ctx, extra, "group_allow_from", "WEIXIN_GROUP_ALLOWED_USERS"); err != nil {
		return nil, err
	}
	split, err := setting(ctx, extra, "split_multiline_messages", "")
	if err != nil {
		return nil, err
	}
	a.splitMultiline = coerceBool(split, false)
	a.ConfigureTextBatchDelays()
	if a.accountID != "" && a.token == "" {
		if saved := loadAccount(homeDir, a.accountID); saved != nil {
			a.token = strings.TrimSpace(saved.Token)
			if b := strings.TrimSpace(saved.BaseURL); b != "" {
				a.baseURL = strings.TrimRight(b, "/")
			}
		}
	}
	return a, nil
}

// The adapter gates DM and group intake itself
func (a *Adapter) EnforcesOwnAccessPolicy() bool { return true }

// Quiet period of a pending text batch, chunks near the iLink split size waiting longer
func (a *Adapter) TextBatchDelayFor(pending *gateway.MessageEvent) float64 {
	delay, split := a.TextBatchDelays()
	if pending != nil && pending.LastChunkLen() >= splitThreshold {
		return split
	}
	return delay
}

// HTTP client honouring gateway trust_env with idle sockets reaped after two seconds
func newHTTPClient(ctx context.Context) *http.Client {
	client := gateway.TrustEnvHTTPClient(ctx, 0)
	if t, ok := client.Transport.(*http.Transport); ok {
		t.IdleConnTimeout = 2 * time.Second
	}
	return client
}

// Long poll and send clients, nil while disconnected
func (a *Adapter) clients() (poll, send *http.Client) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pollClient, a.sendClient
}

// Validates credentials, takes the bot token lock and starts the long poll loop
func (a *Adapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	for _, check := range []struct {
		ok           bool
		code, reason string
	}{
		{a.token != "", "weixin_missing_token", "WEIXIN_TOKEN is required"},
		{a.accountID != "", "weixin_missing_account", "WEIXIN_ACCOUNT_ID is required"},
	} {
		if !check.ok {
			a.SetFatalError(ctx, check.code, "Weixin startup failed: "+check.reason, false)
			slog.Warn(fmt.Sprintf("[%s] Weixin startup failed: %s", a.Name(), check.reason))
			return false, nil
		}
	}
	acquired, err := a.AcquirePlatformLock(ctx, "weixin-bot-token", a.token, "Weixin bot token")
	if err != nil || !acquired {
		return false, err
	}
	a.stopPoll()
	pollCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	a.mu.Lock()
	a.pollClient, a.sendClient = newHTTPClient(ctx), newHTTPClient(ctx)
	a.pollCancel, a.pollDone = cancel, done
	a.mu.Unlock()
	a.tokens.restore(a.accountID)
	a.MarkConnected(ctx, "")
	go func() {
		defer close(done)
		a.pollLoop(pollCtx)
	}()
	registerLive(a)
	slog.Info(fmt.Sprintf("[%s] Connected account=%s base=%s", a.Name(), safeID(a.accountID), a.baseURL))
	if a.GroupPolicy != "disabled" {
		slog.Warn(fmt.Sprintf("[%s] WEIXIN_GROUP_POLICY=%s is set, but QR-login connects an iLink bot identity (e.g. ...@im.bot) "+
			"which typically cannot be invited into ordinary WeChat groups. iLink usually does not deliver "+
			"ordinary-group events for these accounts, so group messages may never reach Icebath regardless of "+
			"this policy. When group messages never arrive, iLink is withholding them from this bot identity.",
			a.Name(), a.GroupPolicy))
	}
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Cancels the long poll loop and waits for it to exit
func (a *Adapter) stopPoll() {
	a.mu.Lock()
	cancel, done := a.pollCancel, a.pollDone
	a.pollCancel, a.pollDone = nil, nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// Stops polling, drops pending text batches, closes the clients and releases the token lock
func (a *Adapter) Disconnect(ctx context.Context) error {
	unregisterLive(a)
	a.SetRunning(false)
	for _, task := range a.TextBatchTasks() {
		if !task.Done() {
			task.Cancel()
		}
	}
	a.ClearTextBatches()
	a.stopPoll()
	a.mu.Lock()
	poll, send := a.pollClient, a.sendClient
	a.pollClient, a.sendClient = nil, nil
	a.mu.Unlock()
	for _, c := range []*http.Client{poll, send} {
		if c != nil {
			c.CloseIdleConnections()
		}
	}
	a.ReleasePlatformLock()
	a.MarkDisconnected(ctx)
	slog.Info(fmt.Sprintf("[%s] Disconnected", a.Name()))
	return nil
}

// Sleeps for d, false when ctx ended first
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Swaps in a fresh long poll client and closes the old one's sockets
func (a *Adapter) recyclePollClient(ctx context.Context) {
	a.mu.Lock()
	old := a.pollClient
	a.pollClient = newHTTPClient(ctx)
	a.mu.Unlock()
	if old != nil {
		old.CloseIdleConnections()
	}
}

// Long polls getupdates until cancelled, dispatching each message and persisting the cursor as it advances
func (a *Adapter) pollLoop(ctx context.Context) {
	syncBuf := loadSyncBuf(a.homeDir, a.accountID)
	timeoutMS := longPollTimeoutMS
	failures := 0
	backoff := func() bool {
		streakDone := failures >= maxConsecutiveFailures
		delay := retryDelay
		if streakDone {
			delay = backoffDelay
		}
		if !sleepCtx(ctx, delay) {
			return false
		}
		if streakDone {
			failures = 0
		}
		return true
	}
	for ctx.Err() == nil {
		poll, _ := a.clients()
		if poll == nil {
			return
		}
		resp, err := getUpdates(ctx, poll, a.baseURL, a.token, syncBuf, timeoutMS)
		if err == nil {
			if suggested := resp.suggestedTimeoutMS(); suggested > 0 {
				timeoutMS = suggested
			}
			if !resp.ok() {
				if resp.sessionExpired() {
					slog.Error(fmt.Sprintf("[%s] Session expired; pausing for 10 minutes", a.Name()))
					if !sleepCtx(ctx, sessionExpiredPause) {
						return
					}
					failures = 0
					continue
				}
				failures++
				slog.Warn(fmt.Sprintf("[%s] getUpdates failed ret=%s errcode=%s errmsg=%s (%d/%d)", a.Name(), resp.Ret, resp.Errcode,
					resp.Errmsg, failures, maxConsecutiveFailures))
				if !backoff() {
					return
				}
				continue
			}
			failures = 0
			for _, raw := range resp.Msgs {
				a.dispatchInbound(ctx, raw)
			}
			buf := string(resp.GetUpdatesBuf)
			if buf == "" || buf == syncBuf {
				continue
			}
			syncBuf = buf
			if err = saveSyncBuf(a.homeDir, a.accountID, syncBuf); err == nil {
				continue
			}
		}
		if ctx.Err() != nil {
			return
		}
		failures++
		slog.Error(fmt.Sprintf("[%s] poll error (%d/%d): %v", a.Name(), failures, maxConsecutiveFailures, err))
		if !backoff() {
			return
		}
		if failures == 0 {
			a.recyclePollClient(ctx)
		}
	}
}

// Processes one raw inbound message after every earlier message of the same sender
func (a *Adapter) dispatchInbound(ctx context.Context, raw json.RawMessage) {
	var msg inboundMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		slog.Error(fmt.Sprintf("[%s] unreadable inbound message: %v", a.Name(), err))
		return
	}
	key := msg.FromUserID.trimmed()
	a.mu.Lock()
	prev := a.orderTail[key]
	done := make(chan struct{})
	a.orderTail[key] = done
	a.mu.Unlock()
	task := gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
		defer func() {
			close(done)
			a.mu.Lock()
			if a.orderTail[key] == done {
				delete(a.orderTail, key)
			}
			a.mu.Unlock()
		}()
		if prev != nil {
			select {
			case <-prev:
			case <-tctx.Done():
				return nil
			}
		}
		defer func() {
			if r := recover(); r != nil {
				slog.Error(fmt.Sprintf("[%s] unhandled inbound panic from=%s: %v", a.Name(), safeID(key), r))
			}
		}()
		if err := a.processMessage(tctx, &msg, raw); err != nil {
			slog.Error(fmt.Sprintf("[%s] unhandled inbound error from=%s: %v", a.Name(), safeID(key), err))
		}
		return nil
	})
	a.TrackBackgroundTask(task)
}

// Chat type and id of a message, a room or foreign recipient of a group typed message meaning group
func guessChatType(msg *inboundMessage, accountID string) (string, string) {
	roomID := string(msg.RoomID)
	if roomID == "" {
		roomID = string(msg.ChatRoomID)
	}
	roomID = strings.TrimSpace(roomID)
	if roomID != "" {
		return "group", roomID
	}
	if toUserID := msg.ToUserID.trimmed(); toUserID != "" && accountID != "" && toUserID != accountID && msg.MsgType == 1 {
		return "group", toUserID
	}
	return "dm", string(msg.FromUserID)
}

// Whether the item type carries media
func isMediaItemType(t flexInt) bool {
	return t == itemImage || t == itemVideo || t == itemFile || t == itemVoice
}

// Text body of an item list with quoted context, or the Weixin voice transcript when no audio is attached
func extractText(items []messageItem) string {
	for i := range items {
		item := &items[i]
		if item.Type != itemText {
			continue
		}
		text := ""
		if item.TextItem != nil {
			text = string(item.TextItem.Text)
		}
		ref := item.RefMsg
		if ref == nil {
			return text
		}
		title := string(ref.Title)
		if refItem := ref.MessageItem; refItem.present() {
			if isMediaItemType(refItem.Type) {
				if title != "" {
					return strings.TrimSpace("[引用媒体: " + title + "]\n" + text)
				}
				return strings.TrimSpace("[引用媒体]\n" + text)
			}
			var parts []string
			for _, p := range []string{title, extractText([]messageItem{*refItem})} {
				if p != "" {
					parts = append(parts, p)
				}
			}
			if len(parts) > 0 {
				return strings.TrimSpace("[引用: " + strings.Join(parts, " | ") + "]\n" + text)
			}
		}
		return text
	}
	for i := range items {
		item := &items[i]
		if item.Type != itemVoice || item.VoiceItem == nil {
			continue
		}
		if voiceText := string(item.VoiceItem.Text); !item.VoiceItem.Media.present() && voiceText != "" {
			return "[Voice transcription provided by Weixin]\n" + voiceText
		}
	}
	return ""
}

// Event kind from its media mimes, a slash prefix marking a command
func messageTypeFromMedia(mediaTypes []string, text string) gateway.MessageType {
	for _, rule := range []struct {
		prefix string
		kind   gateway.MessageType
	}{{"image/", gateway.MessageTypePhoto}, {"video/", gateway.MessageTypeVideo}, {"audio/", gateway.MessageTypeVoice}} {
		for _, m := range mediaTypes {
			if strings.HasPrefix(m, rule.prefix) {
				return rule.kind
			}
		}
	}
	switch {
	case len(mediaTypes) > 0:
		return gateway.MessageTypeDocument
	case strings.HasPrefix(text, "/"):
		return gateway.MessageTypeCommand
	}
	return gateway.MessageTypeText
}

// Mime types of the extensions routing decides on, used when the system mime table lacks them
var fallbackMimeTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".jpe": "image/jpeg", ".png": "image/png", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp", ".tif": "image/tiff", ".tiff": "image/tiff", ".heic": "image/heic",
	".heif": "image/heif", ".ico": "image/vnd.microsoft.icon", ".svg": "image/svg+xml", ".avif": "image/avif",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/quicktime", ".qt": "video/quicktime", ".avi": "video/x-msvideo",
	".mkv": "video/x-matroska", ".webm": "video/webm", ".3gp": "video/3gpp", ".mpeg": "video/mpeg", ".mpg": "video/mpeg",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".wav": "audio/x-wav", ".ogg": "audio/ogg", ".opus": "audio/opus",
	".flac": "audio/flac", ".aac": "audio/aac", ".amr": "audio/amr", ".silk": "audio/silk",
	".pdf": "application/pdf", ".txt": "text/plain", ".zip": "application/zip", ".json": "application/json",
	".doc": "application/msword", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls": "application/vnd.ms-excel", ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt": "application/vnd.ms-powerpoint", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// Content type of a file name from its extension, empty when unknown
func guessMimeType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		return ""
	}
	if t, _, err := mime.ParseMediaType(mime.TypeByExtension(ext)); err == nil && t != "" {
		return t
	}
	return fallbackMimeTypes[ext]
}

// Download timeout, fixed mime and cache writer of each inbound media item type
var inboundMedia = map[flexInt]struct {
	timeout time.Duration
	mime    string
	label   string
	cache   func(ctx context.Context, data []byte, name string) (string, error)
}{
	itemImage: {30 * time.Second, "image/jpeg", "image", func(ctx context.Context, data []byte, _ string) (string, error) {
		return gateway.CacheImageFromBytesAsync(ctx, data, ".jpg")
	}},
	itemVideo: {120 * time.Second, "video/mp4", "video", func(ctx context.Context, data []byte, _ string) (string, error) {
		return gateway.CacheDocumentFromBytesAsync(ctx, data, "video.mp4")
	}},
	itemFile: {60 * time.Second, "", "file", func(ctx context.Context, data []byte, name string) (string, error) {
		return gateway.CacheDocumentFromBytesAsync(ctx, data, name)
	}},
	itemVoice: {60 * time.Second, "audio/silk", "voice", func(ctx context.Context, data []byte, _ string) (string, error) {
		return gateway.CacheAudioFromBytesAsync(ctx, data, ".silk")
	}},
}

// Downloads, decrypts and caches one inbound media item, its cached path and mime or the note replacing it
func (a *Adapter) collectMedia(ctx context.Context, client *http.Client, item *messageItem) (string, string, string) {
	spec, ok := inboundMedia[item.Type]
	if !ok {
		return "", "", ""
	}
	payload := item.mediaPayload()
	if payload == nil {
		payload = &mediaPayload{}
	}
	filename := string(payload.FileName)
	if filename == "" {
		filename = "document.bin"
	}
	mimeType := spec.mime
	if mimeType == "" {
		if mimeType = guessMimeType(filename); mimeType == "" {
			mimeType = "application/octet-stream"
		}
	}
	path, err := func() (string, error) {
		var key []byte
		var err error
		switch {
		case item.Type == itemImage && strings.TrimSpace(string(payload.AESKey)) != "":
			key, err = parseHexAESKey(string(payload.AESKey))
		case payload.Media != nil && payload.Media.AESKey != "":
			key, err = parseAESKey(string(payload.Media.AESKey))
		}
		if err != nil {
			return "", err
		}
		data, err := downloadMedia(ctx, client, a.cdnBaseURL, payload.Media, key, spec.timeout)
		if err != nil {
			return "", err
		}
		return spec.cache(ctx, data, filename)
	}()
	if err != nil {
		slog.Warn(fmt.Sprintf("[%s] %s download failed: %v", a.Name(), spec.label, err))
		return "", "", missingMediaNote(item, spec.label)
	}
	return path, mimeType, ""
}

// Text standing in for media that could not be fetched, the Weixin transcript for a voice note that has one
func missingMediaNote(item *messageItem, label string) string {
	if item.Type == itemVoice && item.VoiceItem != nil && item.VoiceItem.Text != "" {
		return "[Voice transcription provided by Weixin]\n" + string(item.VoiceItem.Text)
	}
	return "[The user sent a WeChat " + label + " that could not be downloaded]"
}

// Fetches and caches the typing ticket of a peer, empty when none was issued
func (a *Adapter) fetchTypingTicket(ctx context.Context, client *http.Client, userID, contextToken, failureLabel string) string {
	ticket, err := getTypingTicket(ctx, client, a.baseURL, a.token, userID, contextToken)
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] %s for %s: %v", a.Name(), failureLabel, safeID(userID), err))
		return ""
	}
	if ticket != "" {
		a.typing.set(userID, ticket)
	}
	return ticket
}

// Gates, records the context token, downloads media and hands one inbound message to the gateway
func (a *Adapter) processMessage(ctx context.Context, msg *inboundMessage, raw json.RawMessage) error {
	poll, _ := a.clients()
	if poll == nil {
		return nil
	}
	senderID := msg.FromUserID.trimmed()
	messageID := msg.MessageID.trimmed()
	if senderID == "" || senderID == a.accountID || (messageID != "" && a.dedup.IsDuplicate(messageID)) {
		return nil
	}
	text := extractText(msg.ItemList)
	if text != "" {
		sum := md5.Sum([]byte(text))
		if a.dedup.IsDuplicate("content:" + senderID + ":" + hex.EncodeToString(sum[:])) {
			slog.Debug(fmt.Sprintf("[%s] Content-dedup: skipping duplicate message from %s", a.Name(), senderID))
			return nil
		}
	}
	chatType, chatID := guessChatType(msg, a.accountID)
	var allowed bool
	var err error
	if chatType == "group" {
		allowed, err = a.IsGroupAllowed(ctx, chatID, senderID)
	} else {
		allowed, err = a.IsDMIntakeAllowed(ctx, senderID)
	}
	if err != nil || !allowed {
		return err
	}
	contextToken := msg.ContextToken.trimmed()
	if contextToken != "" {
		a.tokens.set(a.accountID, senderID, contextToken)
	}
	if a.token != "" && a.typing.get(senderID) == "" {
		go a.fetchTypingTicket(context.WithoutCancel(ctx), poll, senderID, contextToken, "getConfig failed")
	}
	var mediaPaths, mediaTypes []string
	for i := range msg.ItemList {
		item := &msg.ItemList[i]
		candidates := []*messageItem{item}
		if item.RefMsg != nil && item.RefMsg.MessageItem != nil {
			candidates = append(candidates, item.RefMsg.MessageItem)
		}
		for _, candidate := range candidates {
			path, mimeType, note := a.collectMedia(ctx, poll, candidate)
			switch {
			case path != "":
				mediaPaths = append(mediaPaths, path)
				mediaTypes = append(mediaTypes, mimeType)
			case note != "" && !strings.Contains(text, note):
				text = strings.TrimSpace(text + "\n" + note)
			}
		}
	}
	if text == "" && len(mediaPaths) == 0 {
		return nil
	}
	source := a.BuildSource(ctx, chatID, gateway.SourceFields{ChatType: chatType, UserID: &senderID, UserName: &senderID})
	event := gateway.NewMessageEvent(text)
	event.MessageType = messageTypeFromMedia(mediaTypes, text)
	event.Source = source
	var rawMessage map[string]any
	if json.Unmarshal(raw, &rawMessage) == nil {
		event.RawMessage = rawMessage
	}
	if messageID != "" {
		event.MessageID = &messageID
	}
	event.MediaURLs, event.MediaTypes = mediaPaths, mediaTypes
	slog.Info(fmt.Sprintf("[%s] inbound from=%s type=%s media=%d", a.Name(), safeID(senderID), source.ChatType, len(mediaPaths)))
	if event.MessageType == gateway.MessageTypeText {
		return a.EnqueueTextEvent(ctx, event)
	}
	return a.HandleMessage(ctx, event)
}

// Name, dm or group type and id of a peer or chatroom
func (a *Adapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	chatType := "dm"
	if strings.HasSuffix(chatID, "@chatroom") {
		chatType = "group"
	}
	return map[string]any{"name": chatID, "type": chatType, "chat_id": chatID}, nil
}

// Markdown normalized and long prose wrapped for WeChat clients
func (a *Adapter) FormatMessage(content string) string { return formatForWeixin(content) }
