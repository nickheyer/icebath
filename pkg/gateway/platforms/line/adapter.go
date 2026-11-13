package line

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Stashed reply token of a chat with its expiry
type replyToken struct {
	token     string
	expiresAt float64
}

// Registered media file with its expiry
type mediaToken struct {
	path      string
	expiresAt float64
}

// LINE Messaging API gateway adapter, a signature verified webhook in and reply or push out
type LineAdapter struct {
	*gateway.BasePlatformAdapter

	channelAccessToken    string
	channelSecret         string
	webhookHost           *string
	webhookPort           int
	webhookPath           string
	publicBaseURL         string
	allowAll              bool
	allowedUsers          map[string]bool
	allowedGroups         map[string]bool
	allowedRooms          map[string]bool
	slowResponseThreshold float64
	pendingText           string
	buttonLabel           string
	deliveredText         string
	interruptedText       string
	expiredText           string

	mu             sync.Mutex
	client         *lineClient
	runner         *gateway.WebAppRunner
	replyTokens    map[string]replyToken
	cache          *RequestCache
	dedup          *gateway.MessageDeduplicator
	botUserID      string
	mediaTokens    map[string]mediaToken
	mediaTempPaths map[string]bool
	mediaTTL       float64
	pendingButtons map[string]string
}

// Wall clock seconds since the epoch
var nowSeconds = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Class attributes the LINE adapter declares over the base defaults
func lineAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.ServesProfilePrefix = true
	return traits
}

// Scoped env value of name or the extra key, the Python env or extra fallback
func envOr(ctx context.Context, extra map[string]any, env, key string, def any) (any, error) {
	val, err := gateway.GetScopedSecret(ctx, env, "", false)
	if err != nil {
		return nil, err
	}
	if val != "" {
		return val, nil
	}
	if v, ok := extra[key]; ok {
		return v, nil
	}
	return def, nil
}

// Python set of an extra value restricted to its string members, the TypeError set raises otherwise
func setOf(v any) (map[string]bool, error) {
	out := map[string]bool{}
	switch t := v.(type) {
	case nil:
		return nil, &gateway.PyError{Kind: "TypeError", Msg: "'NoneType' object is not iterable"}
	case string:
		for _, r := range t {
			out[string(r)] = true
		}
	case []string:
		for _, s := range t {
			out[s] = true
		}
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				out[s] = true
			}
		}
	case map[string]any:
		for k := range t {
			out[k] = true
		}
	default:
		return nil, &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(v))}
	}
	return out, nil
}

// Scoped boolean env flag, the default when unset and true only for 1, true, yes or on
func truthyEnv(ctx context.Context, name string, def bool) (bool, error) {
	v, found, err := gateway.LookupScopedSecret(ctx, name, false)
	if err != nil || !found {
		return def, err
	}
	switch pystr.Lower(pystr.Strip(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	}
	return false, nil
}

// Channel access token and channel secret from scoped secrets then extra, non strings reading as unset
func credentials(ctx context.Context, config *gateway.PlatformConfig) (string, string, error) {
	var extra map[string]any
	if config != nil {
		extra = config.Extra
	}
	read := func(env, key string) (string, error) {
		val, err := gateway.GetScopedSecret(ctx, env, "", false)
		if err != nil {
			return "", err
		}
		var value any = val
		if val == "" {
			value = extra[key]
		}
		if s, ok := value.(string); ok {
			return pystr.Strip(s), nil
		}
		return "", nil
	}
	token, err := read("LINE_CHANNEL_ACCESS_TOKEN", "channel_access_token")
	if err != nil {
		return "", "", err
	}
	secret, err := read("LINE_CHANNEL_SECRET", "channel_secret")
	return token, secret, err
}

// Python int of a value, the default on TypeError or ValueError
func coerceInt(v any, def int) (int, error) {
	n, err := pyint.Int64(v)
	if pe, ok := err.(*pyint.Error); ok && (pe.Kind == "TypeError" || pe.Kind == "ValueError") {
		return def, nil
	} else if err != nil {
		return 0, err
	}
	if n > math.MaxInt || n < math.MinInt {
		return 0, &gateway.PyError{Kind: "OverflowError", Msg: "Python int too large to convert to C int"}
	}
	return int(n), nil
}

// Python float of a value, the default on TypeError or ValueError
func coerceFloat(v any, def float64) (float64, error) {
	f, err := pyint.Float(v)
	if pe, ok := err.(*pyint.Error); ok && (pe.Kind == "TypeError" || pe.Kind == "ValueError") {
		return def, nil
	} else if err != nil {
		return 0, err
	}
	return f, nil
}

// Adapter for a platform config reading credentials, bind address, allowlists and postback copy
func NewLineAdapter(ctx context.Context, config *gateway.PlatformConfig) (*LineAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &LineAdapter{replyTokens: map[string]replyToken{}, cache: NewRequestCache(),
		dedup: gateway.NewMessageDeduplicator(1000, math.Inf(1)), mediaTokens: map[string]mediaToken{},
		mediaTempPaths: map[string]bool{}, mediaTTL: mediaTokenTTLSeconds, pendingButtons: map[string]string{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.Platform("line"), a)
	a.Traits = lineAdapterTraits()
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	var err error
	if a.channelAccessToken, a.channelSecret, err = credentials(ctx, config); err != nil {
		return nil, err
	}
	host, err := envOr(ctx, extra, "LINE_HOST", "host", nil)
	if err != nil {
		return nil, err
	}
	if pyrepr.Truthy(host) {
		h := pyrepr.Str(host)
		a.webhookHost = &h
	}
	port, err := envOr(ctx, extra, "LINE_PORT", "port", defaultWebhookPort)
	if err != nil {
		return nil, err
	}
	if a.webhookPort, err = coerceInt(port, defaultWebhookPort); err != nil {
		return nil, err
	}
	a.webhookPath = defaultWebhookPath
	if v, ok := extra["webhook_path"]; ok {
		a.webhookPath = pyrepr.Str(v)
	}
	public, err := envOr(ctx, extra, "LINE_PUBLIC_URL", "public_url", "")
	if err != nil {
		return nil, err
	}
	if pyrepr.Truthy(public) {
		a.publicBaseURL = strings.TrimRight(pyrepr.Str(public), "/")
	}
	if a.allowAll, err = truthyEnv(ctx, "LINE_ALLOW_ALL_USERS", pyrepr.Truthy(extra["allow_all_users"])); err != nil {
		return nil, err
	}
	for _, list := range []struct {
		dst      *map[string]bool
		env, key string
	}{{&a.allowedUsers, "LINE_ALLOWED_USERS", "allowed_users"}, {&a.allowedGroups, "LINE_ALLOWED_GROUPS", "allowed_groups"},
		{&a.allowedRooms, "LINE_ALLOWED_ROOMS", "allowed_rooms"}} {
		raw, err := gateway.GetScopedSecret(ctx, list.env, "", false)
		if err != nil {
			return nil, err
		}
		merged := csvSet(raw)
		configured, ok := extra[list.key]
		if !ok {
			configured = []any{}
		}
		fromExtra, err := setOf(configured)
		if err != nil {
			return nil, err
		}
		for k := range fromExtra {
			merged[k] = true
		}
		*list.dst = merged
	}
	threshold, err := envOr(ctx, extra, "LINE_SLOW_RESPONSE_THRESHOLD", "slow_response_threshold", defaultSlowResponseThresh)
	if err != nil {
		return nil, err
	}
	if a.slowResponseThreshold, err = coerceFloat(threshold, defaultSlowResponseThresh); err != nil {
		return nil, err
	}
	for _, copyText := range []struct {
		dst               *string
		env, attr, keyOfT string
	}{{&a.pendingText, "LINE_PENDING_TEXT", "pending_text", defaultPendingReplyTextKey},
		{&a.buttonLabel, "LINE_BUTTON_LABEL", "button_label", defaultButtonLabelKey},
		{&a.deliveredText, "LINE_DELIVERED_TEXT", "delivered_text", defaultDeliveredTextKey},
		{&a.interruptedText, "LINE_INTERRUPTED_TEXT", "interrupted_text", defaultInterruptedTextKey},
		{&a.expiredText, "LINE_EXPIRED_TEXT", "expired_text", defaultExpiredTextKey}} {
		v, err := envOr(ctx, extra, copyText.env, copyText.attr, i18n.T(ctx, copyText.keyOfT, "", nil))
		if err != nil {
			return nil, err
		}
		*copyText.dst = pyrepr.Str(v)
	}
	return a, nil
}

// Records a fatal connect error and answers the false connect result
func (a *LineAdapter) fail(ctx context.Context, code, detail string, retryable bool) bool {
	a.SetFatalError(ctx, code, detail, retryable)
	return false
}

// Locks the channel, learns the bot user id and binds or publishes the webhook, media and health routes
func (a *LineAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.channelAccessToken == "" || a.channelSecret == "" {
		return a.fail(ctx, "config_missing", "LINE_CHANNEL_ACCESS_TOKEN and LINE_CHANNEL_SECRET must be set", false), nil
	}
	sum := sha256.Sum256([]byte(a.channelAccessToken))
	tokHash := hex.EncodeToString(sum[:])[:16]
	locked, err := a.AcquirePlatformLock(ctx, "line", tokHash, "LINE channel")
	if err != nil || !locked {
		return false, err
	}
	client := newLineClient(a.channelAccessToken)
	botUserID := client.getBotUserID(ctx)
	a.mu.Lock()
	a.client, a.botUserID = client, botUserID
	a.mu.Unlock()
	app := gateway.NewWebApplication(webhookBodyMaxBytes)
	if err := app.AddPost(a.webhookPath, a.handleWebhook); err != nil {
		return false, err
	}
	if err := app.AddGet(a.webhookPath+"/health", a.handleHealth); err != nil {
		return false, err
	}
	if err := app.AddGet(defaultMediaPathPrefix+"/{token}/{filename}", a.handleMedia); err != nil {
		return false, err
	}
	a.WirePluginHandlers(ctx, app)
	var opts gateway.ListenOptions
	if runtime.GOOS == "darwin" {
		reuse := false
		opts.ReuseAddress = &reuse
	}
	runner, err := gateway.BindListener(ctx, a, app, a.webhookHost, a.webhookPort, a.webhookPath, opts)
	if err != nil {
		hostText := "all IPv4+IPv6 interfaces"
		if a.webhookHost != nil {
			hostText = *a.webhookHost
		}
		return a.fail(ctx, "bind_failed", fmt.Sprintf("Could not bind LINE webhook on %s:%d: %v", hostText, a.webhookPort, err), true), nil
	}
	a.mu.Lock()
	a.runner = runner
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	if runner != nil {
		hostText := "* (all interfaces, IPv4+IPv6)"
		if a.webhookHost != nil {
			hostText = *a.webhookHost
		}
		public := ""
		if a.publicBaseURL != "" {
			public = fmt.Sprintf(" (public: %s)", a.publicBaseURL)
		}
		slog.Info(fmt.Sprintf("LINE: webhook listening on %s:%d%s%s", hostText, a.webhookPort, a.webhookPath, public))
	}
	return true, nil
}

// Stops the listener, deletes temporary media, forgets media tokens and releases the channel lock
func (a *LineAdapter) Disconnect(ctx context.Context) error {
	a.MarkDisconnected(ctx)
	a.mu.Lock()
	runner := a.runner
	a.runner = nil
	temps := a.mediaTempPaths
	a.mediaTempPaths = map[string]bool{}
	a.mediaTokens = map[string]mediaToken{}
	a.mu.Unlock()
	if runner != nil {
		_ = runner.Cleanup(ctx)
	}
	for path := range temps {
		unlinkQuietly(path)
	}
	a.ReleasePlatformLock()
	return nil
}

// Removes a file, ignoring any OS error
func unlinkQuietly(path string) { _ = os.Remove(path) }

// Pops a stashed reply token, used only while unexpired
func (a *LineAdapter) consumeReplyToken(chatID string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.consumeReplyTokenLocked(chatID)
}

// Pops a stashed reply token with the state lock held
func (a *LineAdapter) consumeReplyTokenLocked(chatID string) (string, bool) {
	stashed, ok := a.replyTokens[chatID]
	delete(a.replyTokens, chatID)
	if ok && stashed.token != "" && nowSeconds() < stashed.expiresAt {
		return stashed.token, true
	}
	return "", false
}

// Connected client, nil before connect
func (a *LineAdapter) currentClient() *lineClient {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// Loading animation for a DM chat
func (a *LineAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	if client := a.currentClient(); client != nil && chatID != "" {
		client.loading(ctx, chatID, 60)
	}
	return nil
}

// Chat info inferred from the id prefix, U user, C group and R room
func (a *LineAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	chatType := "dm"
	if chatID != "" {
		switch chatID[:1] {
		case "C":
			chatType = "group"
		case "R":
			chatType = "channel"
		}
	}
	return map[string]any{"name": chatID, "type": chatType}, nil
}

// Markdown LINE cannot render stripped with URLs preserved
func (a *LineAdapter) FormatMessage(content string) string {
	return stripMarkdownPreservingURLs(content)
}

// Base typing heartbeat with the slow LLM postback button fired once the threshold passes
func (a *LineAdapter) KeepTyping(ctx context.Context, chatID string, interval float64, metadata map[string]any, stopEvent *gateway.InterruptEvent) {
	client := a.currentClient()
	if a.slowResponseThreshold <= 0 || client == nil || chatID == "" {
		a.BasePlatformAdapter.KeepTyping(ctx, chatID, interval, metadata, stopEvent)
		return
	}
	postCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.firePostback(postCtx, client, chatID)
	}()
	defer func() {
		cancel()
		<-done
	}()
	a.BasePlatformAdapter.KeepTyping(ctx, chatID, interval, metadata, stopEvent)
}

// Burns the reply token on the postback button while a token remains and no button is outstanding
func (a *LineAdapter) firePostback(ctx context.Context, client *lineClient, chatID string) {
	timer := time.NewTimer(time.Duration(a.slowResponseThreshold * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	a.mu.Lock()
	_, hasToken := a.replyTokens[chatID]
	_, outstanding := a.pendingButtons[chatID]
	if !hasToken || outstanding {
		a.mu.Unlock()
		return
	}
	rid := a.cache.RegisterPending(chatID)
	a.pendingButtons[chatID] = rid
	token, used := a.consumeReplyTokenLocked(chatID)
	if !used {
		delete(a.pendingButtons, chatID)
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	msg := buildPostbackButtonMessage(a.pendingText, a.buttonLabel, rid, i18n.T(ctx, defaultButtonLabelKey, "", nil))
	if err := client.reply(ctx, token, []messagingMessage{msg}); err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Warn(fmt.Sprintf("LINE: postback button send failed: %v", err))
		a.mu.Lock()
		delete(a.pendingButtons, chatID)
		a.mu.Unlock()
		return
	}
	slog.Info(fmt.Sprintf("LINE: sent slow-LLM postback button for chat %s (rid=%s)", chatID, rid))
}

// Base interrupt that also settles an orphan pending postback with the interrupted text
func (a *LineAdapter) InterruptSessionActivity(ctx context.Context, sessionKey, chatID string, metadata map[string]any) {
	a.BasePlatformAdapter.InterruptSessionActivity(ctx, sessionKey, chatID, metadata)
	a.mu.Lock()
	defer a.mu.Unlock()
	rid := a.pendingButtons[chatID]
	delete(a.pendingButtons, chatID)
	if rid != "" {
		a.cache.SetError(rid, a.interruptedText)
	}
}
