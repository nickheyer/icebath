package feishu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	maxMessageLength                 = 8000
	chatLockMaxSize                  = 1000
	maxTextInjectBytes               = 100 * 1024
	feishuConnectAttempts            = 3
	feishuSendAttempts               = 3
	feishuAppLockScope               = "feishu-app-id"
	feishuDedupTTLSeconds            = 24 * 60 * 60
	feishuSenderNameTTLSeconds       = 10 * 60
	feishuWebhookMaxBodyBytes        = 1 * 1024 * 1024
	feishuWebhookRateWindowSeconds   = 60
	feishuWebhookRateLimitMax        = 120
	feishuWebhookRateMaxKeys         = 4096
	feishuWebhookBodyTimeoutSeconds  = 30
	feishuWebhookAnomalyThreshold    = 25
	feishuWebhookAnomalyTTLSeconds   = 6 * 60 * 60
	feishuCardActionDedupTTLSeconds  = 15 * 60
	feishuReactionInProgress         = "Typing"
	feishuReactionFailure            = "CrossMark"
	feishuProcessingReactionCacheCap = 1024
	feishuMessageTextCacheSize       = 512
	feishuSDKExecutorWorkers         = 10
)

var imageExtensions = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true}
var audioExtensions = map[string]bool{".ogg": true, ".mp3": true, ".wav": true, ".m4a": true, ".aac": true, ".flac": true, ".opus": true, ".webm": true}
var videoExtensions = map[string]bool{".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true, ".m4v": true, ".3gp": true}

var feishuReplyFallbackCodes = map[int]bool{230011: true, 231003: true}

// Wall clock seconds since the epoch, the time.time the dedup, rate and TTL tables use
var nowFn = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// FeishuGroupRule is the per group policy controlling who may talk to the bot
type FeishuGroupRule struct {
	Policy         string
	Allowlist      map[string]bool
	Blacklist      map[string]bool
	RequireMention *bool
}

// FeishuAdapterSettings is every per profile setting the adapter reads at construction
type FeishuAdapterSettings struct {
	AppID                      string
	AppSecret                  string
	DomainName                 string
	ConnectionMode             string
	EncryptKey                 string
	VerificationToken          string
	GroupPolicy                string
	AllowedGroupUsers          map[string]bool
	BotOpenID                  string
	BotUserID                  string
	BotName                    string
	DedupCacheSize             int
	TextBatchDelaySeconds      float64
	TextBatchSplitDelaySeconds float64
	TextBatchMaxMessages       int
	TextBatchMaxChars          int
	MediaBatchDelaySeconds     float64
	WebhookHost                string
	WebhookPort                int
	WebhookPath                string
	WSReconnectNonce           int
	WSReconnectInterval        int
	WSPingInterval             *int
	WSPingTimeout              *int
	Admins                     map[string]bool
	GroupRules                 map[string]*FeishuGroupRule
	AllowBots                  string
	RequireMention             bool
	AllowAllDM                 bool
}

// Non empty stripped string forms of every value
func idSet(values any) map[string]bool {
	out := map[string]bool{}
	var items []any
	switch v := values.(type) {
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	case string:
		for _, r := range v {
			items = append(items, string(r))
		}
	case map[string]any:
		for k := range v {
			items = append(items, k)
		}
	}
	for _, item := range items {
		if s := strings.TrimSpace(anyutil.String(item)); s != "" {
			out[s] = true
		}
	}
	return out
}

// Whole seconds of a length of time
func wholeSeconds(d time.Duration) int { return int(d / time.Second) }

// Whole seconds of a length of time that may be unset
func optionalSeconds(d time.Duration, set bool) *int {
	if !set {
		return nil
	}
	n := wholeSeconds(d)
	return &n
}

// Settings of the profile served, the app's credentials from its scoped env first and the connection's config after
func loadSettings(ctx context.Context, extra map[string]any) (FeishuAdapterSettings, error) {
	var firstErr error
	credential := func(key, env string) string {
		v, err := gateway.ExtraOrSecret(ctx, extra, key, env, "", true)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		return strings.TrimSpace(anyutil.String(v))
	}
	sctx := gateway.SettingsContext(ctx)
	groupRules := map[string]*FeishuGroupRule{}
	rules := settings.FeishuGroupRules.Get(sctx)
	for _, chatID := range sortedKeys(rules) {
		cfg, ok := rules[chatID].(map[string]any)
		if !ok {
			continue
		}
		rule := &FeishuGroupRule{Policy: strings.ToLower(strings.TrimSpace(anyutil.String(mapGet(cfg, "policy", "open")))),
			Allowlist: idSet(mapGet(cfg, "allowlist", []any{})), Blacklist: idSet(mapGet(cfg, "blacklist", []any{}))}
		if v, ok := cfg["require_mention"]; ok {
			b := toBoolean(v)
			rule.RequireMention = &b
		}
		groupRules[chatID] = rule
	}
	pingInterval, pingIntervalSet := settings.FeishuWSPingInterval.Lookup(sctx)
	pingTimeout, pingTimeoutSet := settings.FeishuWSPingTimeout.Lookup(sctx)
	s := FeishuAdapterSettings{
		AppID:                      credential("app_id", "FEISHU_APP_ID"),
		AppSecret:                  credential("app_secret", "FEISHU_APP_SECRET"),
		DomainName:                 settings.FeishuDomain.Get(sctx),
		ConnectionMode:             settings.FeishuConnectionMode.Get(sctx),
		EncryptKey:                 credential("encrypt_key", "FEISHU_ENCRYPT_KEY"),
		VerificationToken:          credential("verification_token", "FEISHU_VERIFICATION_TOKEN"),
		GroupPolicy:                settings.FeishuGroupPolicy.Get(sctx),
		AllowedGroupUsers:          idSet(settings.FeishuAllowedUsers.Strings(sctx)),
		BotOpenID:                  strings.TrimSpace(settings.FeishuBotOpenID.Get(sctx)),
		BotUserID:                  strings.TrimSpace(settings.FeishuBotUserID.Get(sctx)),
		BotName:                    strings.TrimSpace(settings.FeishuBotName.Get(sctx)),
		DedupCacheSize:             settings.FeishuDedupCacheSize.Get(sctx),
		TextBatchDelaySeconds:      settings.FeishuTextBatchDelay.Get(sctx).Seconds(),
		TextBatchSplitDelaySeconds: settings.FeishuTextBatchSplitDelay.Get(sctx).Seconds(),
		TextBatchMaxMessages:       settings.FeishuTextBatchMaxMessages.Get(sctx),
		TextBatchMaxChars:          settings.FeishuTextBatchMaxChars.Get(sctx),
		MediaBatchDelaySeconds:     settings.FeishuMediaBatchDelay.Get(sctx).Seconds(),
		WebhookHost:                strings.TrimSpace(settings.FeishuWebhookHost.Get(sctx)),
		WebhookPort:                settings.FeishuWebhookPort.Get(sctx),
		WebhookPath:                strings.TrimSpace(settings.FeishuWebhookPath.Get(sctx)),
		WSReconnectNonce:           wholeSeconds(settings.FeishuWSReconnectNonce.Get(sctx)),
		WSReconnectInterval:        wholeSeconds(settings.FeishuWSReconnectInterval.Get(sctx)),
		WSPingInterval:             optionalSeconds(pingInterval, pingIntervalSet),
		WSPingTimeout:              optionalSeconds(pingTimeout, pingTimeoutSet),
		Admins:                     idSet(settings.FeishuAdmins.Strings(sctx)),
		GroupRules:                 groupRules,
		AllowBots:                  settings.FeishuAllowBots.Get(sctx),
		AllowAllDM:                 settings.GatewayAllowAllUsers.Get(sctx) || settings.FeishuAllowAllUsers.Get(sctx),
		RequireMention:             settings.FeishuRequireMention.Get(sctx),
	}
	if s.WebhookPath == "" {
		s.WebhookPath = settings.FeishuWebhookPath.Default
	}
	return s, firstErr
}

func mapGet(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Map keys in sorted order
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Adapter owned pool of blocking SDK calls, refusing work once the adapter is closing
type sdkExecutor struct {
	slots  chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
}

// Chat lock entry with a held flag for the LRU eviction choice
type chatLock struct {
	mu     sync.Mutex
	held   bool
	heldMu sync.Mutex
}

func (l *chatLock) lock() {
	l.mu.Lock()
	l.heldMu.Lock()
	l.held = true
	l.heldMu.Unlock()
}

func (l *chatLock) unlock() {
	l.heldMu.Lock()
	l.held = false
	l.heldMu.Unlock()
	l.mu.Unlock()
}

func (l *chatLock) locked() bool {
	l.heldMu.Lock()
	defer l.heldMu.Unlock()
	return l.held
}

// Adapter loop stand in, the context inbound callback work runs under until disconnect
type adapterLoop struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func (l *adapterLoop) closed() bool { return l.ctx.Err() != nil }

type nameEntry struct {
	name     string
	expireAt float64
}

type rateEntry struct {
	count       int
	windowStart float64
}

type anomalyEntry struct {
	count      int
	lastStatus string
	firstSeen  float64
}

// FeishuAdapter is the Feishu and Lark bot adapter over the lark Open Platform SDK
type FeishuAdapter struct {
	*gateway.BasePlatformAdapter
	mu sync.Mutex

	settings          FeishuAdapterSettings
	allowedGroupUsers map[string]bool
	admins            map[string]bool
	botOpenID         string
	botName           string

	client           larkAPI
	executorMu       sync.Mutex
	executor         *sdkExecutor
	executorClosing  bool
	wsClient         *wsLink
	wsFuture         *wsFuture
	wsSupervisor     context.CancelFunc
	wsRestartBackoff float64
	loop             *adapterLoop
	webhookRunner    *gateway.HTTPAppRunner
	eventHandler     *dispatcher.EventDispatcher

	seenMessageIDs   map[string]float64
	seenMessageOrder []string
	dedupStatePath   string
	dedupMu          sync.Mutex
	dedupPersistMu   sync.Mutex

	senderNameCache      map[string]nameEntry
	webhookRateCounts    map[string]rateEntry
	webhookAnomalyCounts map[string]anomalyEntry
	cardActionTokens     map[string]float64

	pendingInboundEvents   []*larkim.P2MessageReceiveV1
	pendingInboundMu       sync.Mutex
	pendingDrainScheduled  bool
	pendingInboundMaxDepth int
	pendingPollInterval    time.Duration
	pendingMaxWait         time.Duration

	chatLocks        map[string]*chatLock
	chatLockOrder    []string
	chatLockMaxSize  int
	chatInfoCache    map[string]map[string]any
	messageTextCache map[string]*string
	messageTextOrder []string
	appLockIdentity  *string

	textBatchCounts map[string]int
	mediaBatches    map[string]*gateway.MessageEvent
	mediaBatchTasks map[string]*gateway.AdapterTask

	approvalState       map[int]map[string]string
	approvalCounter     int
	updatePromptState   map[int]map[string]string
	updatePromptCounter int

	processingReactions     map[string]string
	processingReactionOrder []string

	warnedEmptyAllowlistDeny bool
}

// Class attributes the Feishu adapter declares over the base adapter defaults
func feishuAdapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.ServesProfilePrefix = true
	t.SupportsCodeBlocks = true
	t.SplitsLongMessages = true
	t.SerialReceiveLoop = true
	t.MaxMessageLength = maxMessageLength
	t.EACmdBudget = 3000
	return t
}

// Adapter for a platform config, its settings read from the config extra and the profile env
func NewFeishuAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (*FeishuAdapter, error) {
	a := &FeishuAdapter{
		wsRestartBackoff:       5.0,
		seenMessageIDs:         map[string]float64{},
		dedupStatePath:         filepath.Join(home.Dir(ctx), "feishu_seen_message_ids.json"),
		senderNameCache:        map[string]nameEntry{},
		webhookRateCounts:      map[string]rateEntry{},
		webhookAnomalyCounts:   map[string]anomalyEntry{},
		cardActionTokens:       map[string]float64{},
		pendingInboundMaxDepth: 1000,
		pendingPollInterval:    250 * time.Millisecond,
		pendingMaxWait:         120 * time.Second,
		chatLocks:              map[string]*chatLock{},
		chatLockMaxSize:        chatLockMaxSize,
		chatInfoCache:          map[string]map[string]any{},
		messageTextCache:       map[string]*string{},
		textBatchCounts:        map[string]int{},
		mediaBatches:           map[string]*gateway.MessageEvent{},
		mediaBatchTasks:        map[string]*gateway.AdapterTask{},
		approvalState:          map[int]map[string]string{},
		updatePromptState:      map[int]map[string]string{},
		processingReactions:    map[string]string{},
	}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformFeishu, a)
	a.Traits = feishuAdapterTraits()
	extra := map[string]any{}
	if cfg != nil && cfg.Extra != nil {
		extra = cfg.Extra
	}
	loaded, err := loadSettings(ctx, extra)
	if err != nil {
		return nil, err
	}
	a.applySettings(loaded)
	a.loadSeenMessageIDs()
	return a, nil
}

// Mirrors the settings onto the adapter
func (a *FeishuAdapter) applySettings(s FeishuAdapterSettings) {
	a.settings = s
	a.allowedGroupUsers = map[string]bool{}
	for k := range s.AllowedGroupUsers {
		a.allowedGroupUsers[k] = true
	}
	a.admins = map[string]bool{}
	for k := range s.Admins {
		a.admins[k] = true
	}
	a.botOpenID, a.botName = s.BotOpenID, s.BotName
	a.SetTextBatchDelays(s.TextBatchDelaySeconds, s.TextBatchSplitDelaySeconds)
}

// Event dispatcher wiring every subscribed message, reaction, card, membership and custom event
func (a *FeishuAdapter) buildEventHandler() *dispatcher.EventDispatcher {
	return dispatcher.NewEventDispatcher(a.settings.VerificationToken, a.settings.EncryptKey).
		OnP2MessageReadV1(func(ctx context.Context, e *larkim.P2MessageReadV1) error { a.onMessageReadEvent(); return nil }).
		OnP2MessageReceiveV1(func(ctx context.Context, e *larkim.P2MessageReceiveV1) error { a.onMessageEvent(e); return nil }).
		OnP2MessageReactionCreatedV1(func(ctx context.Context, e *larkim.P2MessageReactionCreatedV1) error {
			a.onReactionEvent("im.message.reaction.created_v1", reactionFromCreated(e))
			return nil
		}).
		OnP2MessageReactionDeletedV1(func(ctx context.Context, e *larkim.P2MessageReactionDeletedV1) error {
			a.onReactionEvent("im.message.reaction.deleted_v1", reactionFromDeleted(e))
			return nil
		}).
		OnP2CardActionTrigger(a.onCardActionTriggerSDK).
		OnP2ChatMemberBotAddedV1(func(ctx context.Context, e *larkim.P2ChatMemberBotAddedV1) error {
			a.onBotMembershipChange(botAddedChatID(e), "added to")
			return nil
		}).
		OnP2ChatMemberBotDeletedV1(func(ctx context.Context, e *larkim.P2ChatMemberBotDeletedV1) error {
			a.onBotMembershipChange(botDeletedChatID(e), "removed from")
			return nil
		}).
		OnP2ChatAccessEventBotP2pChatEnteredV1(func(ctx context.Context, e *larkim.P2ChatAccessEventBotP2pChatEnteredV1) error {
			pkgLog().Debug("[Feishu] User entered P2P chat with bot")
			return nil
		}).
		OnP2MessageRecalledV1(func(ctx context.Context, e *larkim.P2MessageRecalledV1) error {
			pkgLog().Debug("[Feishu] Message recalled by user")
			return nil
		}).
		OnCustomizedEvent("drive.notice.comment_add_v1", func(ctx context.Context, e *larkevent.EventReq) error {
			a.onDriveCommentEvent(string(e.Body))
			return nil
		}).
		OnCustomizedEvent("vc.bot.meeting_invited_v1", func(ctx context.Context, e *larkevent.EventReq) error {
			a.onMeetingInvitedEvent(string(e.Body))
			return nil
		})
}

var errExecutorClosing = errors.New("Feishu adapter is shutting down; SDK executor unavailable")

// Adapter owned executor, recreated after an outside shutdown and refused once the adapter is closing
func (a *FeishuAdapter) getSDKExecutor() (*sdkExecutor, error) {
	a.executorMu.Lock()
	defer a.executorMu.Unlock()
	if a.executorClosing {
		return nil, errExecutorClosing
	}
	if a.executor == nil || a.executor.ctx.Err() != nil {
		ctx, cancel := context.WithCancel(context.Background())
		a.executor = &sdkExecutor{slots: make(chan struct{}, feishuSDKExecutorWorkers), ctx: ctx, cancel: cancel}
	}
	return a.executor, nil
}

// Runs a blocking SDK call on a pool slot with the caller's profile scope, returning its error
func (a *FeishuAdapter) runBlocking(ctx context.Context, fn func(ctx context.Context) error) error {
	ex, err := a.getSDKExecutor()
	if err != nil {
		return err
	}
	select {
	case ex.slots <- struct{}{}:
	case <-ex.ctx.Done():
		return context.Canceled
	case <-ctx.Done():
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		defer func() { <-ex.slots }()
		done <- fn(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stops the adapter owned executor, cancelling work still waiting for a slot
func (a *FeishuAdapter) shutdownSDKExecutor() {
	a.executorMu.Lock()
	a.executorClosing = true
	ex := a.executor
	a.executor = nil
	a.executorMu.Unlock()
	if ex != nil {
		ex.cancel()
	}
}

func (a *FeishuAdapter) setExecutorClosing(on bool) {
	a.executorMu.Lock()
	a.executorClosing = on
	a.executorMu.Unlock()
}

// Connects in websocket or webhook mode under the app id lock, failing closed on bad settings
func (a *FeishuAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	a.setExecutorClosing(false)
	s := a.settings
	if s.AppID == "" || s.AppSecret == "" {
		pkgLog().Error("[Feishu] FEISHU_APP_ID or FEISHU_APP_SECRET not set")
		return false, nil
	}
	if s.ConnectionMode != "websocket" && s.ConnectionMode != "webhook" {
		pkgLog().Error(fmt.Sprintf("[Feishu] Unsupported connection mode %s. Supported modes: websocket, webhook.", s.ConnectionMode))
		return false, nil
	}
	if s.ConnectionMode == "webhook" && s.VerificationToken == "" && s.EncryptKey == "" {
		pkgLog().Error("[Feishu] Webhook mode requires FEISHU_VERIFICATION_TOKEN or FEISHU_ENCRYPT_KEY.")
		return false, nil
	}
	fail := func(err error) (bool, error) {
		a.releaseAppLock()
		message := "Feishu startup failed: " + err.Error()
		a.SetFatalError(ctx, "feishu_connect_error", message, true)
		pkgLog().Error("[Feishu] Failed to connect: " + err.Error())
		return false, nil
	}
	identity := s.AppID
	a.mu.Lock()
	a.appLockIdentity = &identity
	a.mu.Unlock()
	meta := jsonutil.NewObject()
	meta.Set("platform", string(gateway.PlatformFeishu))
	acquired, existing, err := gateway.AcquireScopedLock(ctx, feishuAppLockScope, identity, meta)
	if err != nil {
		return fail(err)
	}
	if !acquired {
		var ownerPID any
		if existing != nil {
			ownerPID, _ = existing.Get("pid")
		}
		message := "Another local Icebath gateway is already using this Feishu app_id"
		if anyutil.IsTrue(ownerPID) {
			message += " (PID " + anyutil.String(ownerPID) + ")."
		} else {
			message += "."
		}
		message += " Stop the other gateway before starting a second Feishu websocket client."
		pkgLog().Error("[Feishu] " + message)
		a.SetFatalError(ctx, "feishu_app_lock", message, false)
		return false, nil
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a.mu.Lock()
	a.loop = &adapterLoop{ctx: loopCtx, cancel: cancel}
	a.mu.Unlock()
	if err := a.connectWithRetry(ctx); err != nil {
		return fail(err)
	}
	a.MarkConnected(ctx, "")
	if s.ConnectionMode == "websocket" {
		supCtx, supCancel := context.WithCancel(loopCtx)
		a.mu.Lock()
		a.wsSupervisor = supCancel
		a.mu.Unlock()
		go a.superviseWebsocketThread(supCtx)
	}
	pkgLog().Info(fmt.Sprintf("[Feishu] Connected in %s mode (%s)", s.ConnectionMode, s.DomainName))
	a.WirePluginHandlers(ctx, a.currentClient().Native())
	return true, nil
}

func (a *FeishuAdapter) currentClient() larkAPI {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

func (a *FeishuAdapter) currentLoop() *adapterLoop {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.loop
}

// Sends the websocket close frame, tears down the link, stops batches and persists the dedup state
func (a *FeishuAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.mu.Lock()
	if a.wsSupervisor != nil {
		a.wsSupervisor()
		a.wsSupervisor = nil
	}
	a.mu.Unlock()
	a.cancelPendingTasks(ctx)
	a.resetBatchBuffers()
	a.mu.Lock()
	wsClient := a.wsClient
	a.mu.Unlock()
	a.disableWebsocketAutoReconnect()
	a.stopWebhookServer(ctx)
	a.teardownWSThread(ctx, wsClient)
	a.mu.Lock()
	a.wsFuture = nil
	if a.loop != nil {
		a.loop.cancel()
	}
	a.loop = nil
	a.eventHandler = nil
	a.mu.Unlock()
	a.shutdownSDKExecutor()
	a.persistSeenMessageIDs(ctx)
	a.releaseAppLock()
	a.MarkDisconnected(ctx)
	pkgLog().Info("[Feishu] Disconnected")
	return nil
}

// Close frame first, then the link's goroutines cancelled, then the run awaited for ten seconds
func (a *FeishuAdapter) teardownWSThread(ctx context.Context, link *wsLink) {
	if link != nil && link.alive() {
		closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := link.sendClose(closeCtx)
		cancel()
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			pkgLog().Warn("[Feishu] CLOSE frame not acknowledged within 5s — Feishu may briefly route messages to the stale " +
				"connection until server-side timeout")
		case err != nil:
			pkgLog().Debug("[Feishu] Could not send WebSocket CLOSE frame: " + err.Error())
		default:
			pkgLog().Debug("[Feishu] Sent WebSocket CLOSE frame to Feishu")
		}
	}
	if link != nil && link.alive() {
		pkgLog().Debug("[Feishu] Cancelling websocket thread tasks and stopping loop")
		link.stop()
	}
	a.mu.Lock()
	fut := a.wsFuture
	a.mu.Unlock()
	if fut == nil {
		return
	}
	pkgLog().Debug("[Feishu] Waiting for websocket thread to exit (timeout=10s)")
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-fut.done:
		if fut.err != nil {
			pkgLog().Debug("[Feishu] Websocket thread exited with error: " + fut.err.Error())
		} else {
			pkgLog().Debug("[Feishu] Websocket thread exited cleanly")
		}
	case <-timer.C:
		pkgLog().Warn("[Feishu] Websocket thread did not exit within 10s - may be stuck")
	case <-ctx.Done():
		pkgLog().Debug("[Feishu] Websocket thread cancelled during disconnect")
	}
}

// Cancels and awaits every pending text and media batch flush
func (a *FeishuAdapter) cancelPendingTasks(ctx context.Context) {
	cancelAll := func(tasks []*gateway.AdapterTask) {
		var pending []*gateway.AdapterTask
		for _, t := range tasks {
			if t != nil && !t.Done() {
				t.Cancel()
				pending = append(pending, t)
			}
		}
		for _, t := range pending {
			_ = t.Wait(ctx)
		}
	}
	cancelAll(a.TextBatchTasks())
	a.mu.Lock()
	media := make([]*gateway.AdapterTask, 0, len(a.mediaBatchTasks))
	for _, t := range a.mediaBatchTasks {
		media = append(media, t)
	}
	a.mu.Unlock()
	cancelAll(media)
	a.mu.Lock()
	a.mediaBatchTasks = map[string]*gateway.AdapterTask{}
	a.mu.Unlock()
}

func (a *FeishuAdapter) resetBatchBuffers() {
	a.ClearTextBatches()
	a.mu.Lock()
	a.textBatchCounts = map[string]int{}
	a.mediaBatches = map[string]*gateway.MessageEvent{}
	a.mu.Unlock()
}

// Turns the SDK reconnect ladder off and forgets the websocket client
func (a *FeishuAdapter) disableWebsocketAutoReconnect() {
	a.mu.Lock()
	link := a.wsClient
	a.wsClient = nil
	a.mu.Unlock()
	if link != nil {
		link.disableAutoReconnect()
	}
}

func (a *FeishuAdapter) stopWebhookServer(ctx context.Context) {
	a.mu.Lock()
	runner := a.webhookRunner
	a.webhookRunner = nil
	a.mu.Unlock()
	if runner != nil {
		if err := runner.Cleanup(ctx); err != nil {
			pkgLog().Debug("[Feishu] Webhook runner cleanup failed: " + err.Error())
		}
	}
}

// Three connect attempts with exponential backoff, tearing down partial state between attempts
func (a *FeishuAdapter) connectWithRetry(ctx context.Context) error {
	for attempt := 0; attempt < feishuConnectAttempts; attempt++ {
		var err error
		if a.settings.ConnectionMode == "websocket" {
			err = a.connectWebsocket(ctx)
		} else {
			err = a.connectWebhook(ctx)
		}
		if err == nil {
			return nil
		}
		a.SetRunning(false)
		a.disableWebsocketAutoReconnect()
		a.mu.Lock()
		a.wsFuture = nil
		a.mu.Unlock()
		a.stopWebhookServer(ctx)
		if attempt >= feishuConnectAttempts-1 {
			return err
		}
		wait := 1 << attempt
		pkgLog().Warn(fmt.Sprintf("[Feishu] Connect attempt %d/%d failed; retrying in %ds: %s", attempt+1, feishuConnectAttempts, wait, err))
		if !sleepCtx(ctx, time.Duration(wait)*time.Second) {
			return ctx.Err()
		}
	}
	return nil
}

// Rebuilds the websocket link with capped backoff whenever its run ends while the adapter is up
func (a *FeishuAdapter) superviseWebsocketThread(ctx context.Context) {
	backoff := a.wsRestartBackoff
	initial := backoff
	var lastDead *wsFuture
	for a.IsConnected() {
		a.mu.Lock()
		fut := a.wsFuture
		a.mu.Unlock()
		if fut == nil {
			return
		}
		select {
		case <-fut.done:
		case <-ctx.Done():
			return
		}
		a.mu.Lock()
		link := a.wsClient
		a.mu.Unlock()
		if !a.IsConnected() || link == nil {
			return
		}
		if fut != lastDead {
			pkgLog().Error(fmt.Sprintf("[Feishu] WebSocket client thread exited unexpectedly; restarting in %.0fs", backoff))
			lastDead = fut
			a.WriteRuntimeStatusSafe(ctx, "ws_link_lost", map[string]any{"platform_state": "retrying", "error_code": nil,
				"error_message": "Feishu websocket link lost; rebuilding"})
		}
		if !sleepCtx(ctx, time.Duration(backoff*float64(time.Second))) {
			return
		}
		if !a.IsConnected() {
			return
		}
		if err := a.connectWebsocket(ctx); err != nil {
			pkgLog().Warn("[Feishu] WebSocket restart failed (retrying): " + err.Error())
			backoff = min(backoff*2, 60.0)
		} else {
			backoff = initial
		}
	}
}

// Re-stamps connected once a rebuilt link reports its receive loop is up
func (a *FeishuAdapter) wsLinkUp(ctx context.Context, link *wsLink) {
	a.mu.Lock()
	current := a.wsClient
	a.mu.Unlock()
	if a.IsConnected() && current == link {
		a.MarkConnected(ctx, "")
	}
}

// Publishes the retrying state when the SDK's own reconnect ladder starts on the live link
func (a *FeishuAdapter) wsLinkRetrying(ctx context.Context, link *wsLink) {
	a.mu.Lock()
	current := a.wsClient
	a.mu.Unlock()
	if a.IsConnected() && current == link {
		a.WriteRuntimeStatusSafe(ctx, "ws_link_lost", map[string]any{"platform_state": "retrying", "error_code": nil,
			"error_message": "Feishu websocket link lost; reconnecting"})
	}
}

// Builds the client, hydrates the bot identity and runs a fresh websocket link on the executor
func (a *FeishuAdapter) connectWebsocket(ctx context.Context) error {
	domain, err := a.prepareClient()
	if err != nil {
		return err
	}
	loop := a.currentLoop()
	if loop == nil || loop.closed() {
		return errors.New("adapter loop is not ready")
	}
	a.hydrateBotIdentity(ctx)
	a.mu.Lock()
	handler := a.eventHandler
	a.mu.Unlock()
	link := newWSLink(a, domain, handler)
	a.mu.Lock()
	a.wsClient = link
	a.mu.Unlock()
	fut, err := a.submitWSRun(loop.ctx, link)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.wsFuture = fut
	a.mu.Unlock()
	return nil
}

// Builds the client, hydrates the bot identity and binds or publishes the webhook listener
func (a *FeishuAdapter) connectWebhook(ctx context.Context) error {
	if _, err := a.prepareClient(); err != nil {
		return err
	}
	a.hydrateBotIdentity(ctx)
	app := gateway.NewHTTPApp(feishuWebhookMaxBodyBytes)
	if err := app.AddPost(a.settings.WebhookPath, a.handleWebhookRequest); err != nil {
		return err
	}
	host := a.settings.WebhookHost
	runner, err := gateway.BindListener(ctx, a, app, &host, a.settings.WebhookPort, a.settings.WebhookPath, gateway.ListenOptions{})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.webhookRunner = runner
	a.mu.Unlock()
	return nil
}

// Lark client and event dispatcher for the adapter's domain, returning the SDK base URL
func (a *FeishuAdapter) prepareClient() (string, error) {
	domain := sdkDomain(a.settings.DomainName)
	client := a.buildLarkClient(domain)
	handler := a.buildEventHandler()
	a.mu.Lock()
	a.client, a.eventHandler = client, handler
	a.mu.Unlock()
	if handler == nil {
		return "", errors.New("failed to build Feishu event handler")
	}
	return domain, nil
}

func (a *FeishuAdapter) buildLarkClient(domain string) larkAPI {
	return buildLarkClient(a.settings.AppID, a.settings.AppSecret, domain)
}

// Releases the app id lock once, logging a failure
func (a *FeishuAdapter) releaseAppLock() {
	a.mu.Lock()
	identity := a.appLockIdentity
	a.appLockIdentity = nil
	a.mu.Unlock()
	if identity == nil || *identity == "" {
		return
	}
	gateway.ReleaseScopedLock(feishuAppLockScope, *identity)
}

// Sleeps d unless ctx ends first, reporting whether the full sleep happened
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Loads the persisted dedup table, dropping expired ids and keeping the newest up to the cache size
func (a *FeishuAdapter) loadSeenMessageIDs() {
	raw, err := os.ReadFile(a.dedupStatePath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var payload any
	if err == nil {
		var text string
		text, err = textutil.DecodeUTF8(textutil.TrimBOM(raw))
		if err == nil {
			payload, err = jsonutil.Decode(text)
		}
	}
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[Feishu] Failed to load persisted dedup state from %s", a.dedupStatePath))
		return
	}
	var seen any = jsonutil.NewObject()
	if d, ok := payload.(*jsonutil.Object); ok {
		seen = dgetDef(d, "message_ids", jsonutil.NewObject())
	}
	entries := map[string]float64{}
	switch v := seen.(type) {
	case []any:
		for _, item := range v {
			if s := strings.TrimSpace(anyutil.String(item)); s != "" {
				entries[s] = 0.0
			}
		}
	case *jsonutil.Object:
		for _, k := range v.Keys() {
			val, _ := v.Get(k)
			if strings.TrimSpace(k) == "" {
				continue
			}
			if _, isInt := anyutil.Int(val); !isInt {
				if _, isFloat := val.(float64); !isFloat {
					if _, isStr := val.(string); !isStr {
						continue
					}
				}
			}
			if f, ferr := anyutil.ToFloat(val); ferr == nil {
				entries[k] = f
			}
		}
	default:
		return
	}
	now, ttl := nowFn(), float64(feishuDedupTTLSeconds)
	valid := map[string]float64{}
	for m, ts := range entries {
		if ts == 0.0 || ttl <= 0 || now-ts < ttl {
			valid[m] = ts
		}
	}
	ids := make([]string, 0, len(valid))
	for k := range valid {
		ids = append(ids, k)
	}
	sort.SliceStable(ids, func(i, j int) bool { return valid[ids[i]] > valid[ids[j]] })
	if len(ids) > a.settings.DedupCacheSize {
		ids = ids[:a.settings.DedupCacheSize]
	}
	order := make([]string, len(ids))
	for i, id := range ids {
		order[len(ids)-1-i] = id
	}
	a.seenMessageOrder = order
	a.seenMessageIDs = map[string]float64{}
	for _, id := range ids {
		a.seenMessageIDs[id] = valid[id]
	}
}

// Writes the most recent dedup ids with their timestamps so the TTL survives restarts
func (a *FeishuAdapter) persistSeenMessageIDs(ctx context.Context) {
	if _, err := home.Mkdir(ctx, filepath.Dir(a.dedupStatePath)); err != nil {
		pkgLog().Warn(fmt.Sprintf("[Feishu] Failed to persist dedup state to %s", a.dedupStatePath))
		return
	}
	a.dedupMu.Lock()
	start := max(0, len(a.seenMessageOrder)-a.settings.DedupCacheSize)
	ids := jsonutil.NewObject()
	for _, k := range a.seenMessageOrder[start:] {
		if ts, ok := a.seenMessageIDs[k]; ok {
			ids.Set(k, ts)
		}
	}
	a.dedupMu.Unlock()
	payload := jsonutil.NewObject()
	payload.Set("message_ids", ids)
	if err := config.AtomicJSONWriteWith(ctx, a.dedupStatePath, payload); err != nil {
		pkgLog().Warn(fmt.Sprintf("[Feishu] Failed to persist dedup state to %s", a.dedupStatePath))
	}
}

// Reports a message id seen within the TTL, otherwise records it and flushes the table on the executor
func (a *FeishuAdapter) IsDuplicate(ctx context.Context, messageID string) (bool, error) {
	now, ttl := nowFn(), float64(feishuDedupTTLSeconds)
	a.dedupMu.Lock()
	if seenAt, ok := a.seenMessageIDs[messageID]; ok && (ttl <= 0 || now-seenAt < ttl) {
		a.dedupMu.Unlock()
		return true, nil
	}
	a.seenMessageIDs[messageID] = now
	a.seenMessageOrder = append(a.seenMessageOrder, messageID)
	for len(a.seenMessageOrder) > a.settings.DedupCacheSize {
		delete(a.seenMessageIDs, a.seenMessageOrder[0])
		a.seenMessageOrder = a.seenMessageOrder[1:]
	}
	a.dedupMu.Unlock()
	a.dedupPersistMu.Lock()
	defer a.dedupPersistMu.Unlock()
	if err := a.runBlocking(ctx, func(ctx context.Context) error { a.persistSeenMessageIDs(ctx); return nil }); err != nil {
		return false, err
	}
	return false, nil
}
