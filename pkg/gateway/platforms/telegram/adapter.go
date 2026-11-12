package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/dlclark/regexp2/v2"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	maxMessageLength           = 4096
	telegramSplitThreshold     = 4000
	mediaGroupWaitSeconds      = 0.8
	reconnectWaitSeconds       = 15.0
	reconnectPollInterval      = 0.5
	imgJPEGQuality             = 85
	imgMaxDimension            = 1600
	imgCompressThresholdBytes  = 1048576
	textBatchFastLen           = 320
	textBatchFastDelayS        = 0.18
	textBatchShortLen          = 1024
	textBatchShortDelayS       = 0.24
	statusMessageIDsMax        = 2000
	disconnectStepTimeout      = 2.0
	textSendDeadline           = 30.0
	floodInlineWaitCapSecs     = 5.0
	botCommandDescriptionLimit = 256
	botIdentityTTLSeconds      = 300.0
	botIdentityProbeTimeout    = 15.0
	providerPageSize           = 10
	modelPageSize              = 8
)

// Telegram bot adapter over go-telegram/bot with MarkdownV2 replies, forum and DM topics and media
type TelegramAdapter struct {
	*gateway.BasePlatformAdapter
	*HeldInbound

	core    *adapterCore
	pacer   *sendPacer
	polling *pollingLifecycle
	topics  *TopicRouting

	bot       atomic.Pointer[bot.Bot]
	botUserID atomic.Int64

	mu            sync.Mutex
	app           *ptbApp
	tgApp         *TelegramApplication
	admission     *UpdateAdmission
	coreHandlers  map[Handler]struct{}
	webhookServer *webhookServer

	mentionPatterns         []*regexp2.Regexp
	replyToMode             string
	disableLinkPreviews     bool
	richMessagesEnabled     bool
	allowCJKRichMessages    bool
	richDraftsEnabled       bool
	richSendDisabled        atomic.Bool
	richDraftDisabled       atomic.Bool
	typingRetriggerInterval float64
	retriggerTasks          map[string]*gateway.AdapterTask
	retriggerAt             map[string]float64
	mediaBatchDelaySeconds  float64
	pendingPhotoBatches     map[string]*gateway.MessageEvent
	pendingPhotoBatchTasks  map[string]*gateway.AdapterTask
	mediaGroupEvents        map[string]*gateway.MessageEvent
	mediaGroupTasks         map[string]*gateway.AdapterTask
	committedFlushTasks     map[[2]string]*gateway.AdapterTask
	textBatchDelaySeconds   float64
	textBatchSplitDelay     float64
	dropDelayedDeliveries   atomic.Bool
	identityRefreshTask     *gateway.AdapterTask
	postConnectTask         *gateway.AdapterTask
	botUsernameObserved     string
	botIdentityCheckedAt    *float64
	dmTopics                *pydict.Dict[string, int64]
	forumCommandRegistered  map[int64]bool
	commandMenuFingerprint  string
	forumLock               sync.Mutex
	statusIndicatorEnabled  bool
	statusOnlineText        string
	statusOfflineText       string
	dropPendingOnColdBoot   bool
	dmTopicsConfig          []any
	dmTopicChatIDs          map[string]bool
	maxDocBytes             int64
	modelPickerState        map[string]*modelPickerState
	choicePickerState       map[string]*choicePickerState
	approvalState           map[int]string
	approvalCounter         int
	slashConfirmState       map[string]string
	clarifyState            map[string]string
	notificationsMode       string
	statusMessageIDs        *pydict.Dict[[2]string, string]
	lastOverflowPreview     map[[2]string]string
	topicTransferFn         TopicTransferFn
	reconnectWaitSeconds    float64
	reconnectPollInterval   float64
	homeDir                 string
}

// Adapter of the platform config with every hermes __init__ default and extra derived setting
func NewTelegramAdapter(ctx context.Context, cfg *gateway.PlatformConfig) *TelegramAdapter {
	a := &TelegramAdapter{reconnectWaitSeconds: reconnectWaitSeconds, reconnectPollInterval: reconnectPollInterval,
		notificationsMode: "important", retriggerTasks: map[string]*gateway.AdapterTask{}, retriggerAt: map[string]float64{},
		pendingPhotoBatches: map[string]*gateway.MessageEvent{}, pendingPhotoBatchTasks: map[string]*gateway.AdapterTask{},
		mediaGroupEvents: map[string]*gateway.MessageEvent{}, mediaGroupTasks: map[string]*gateway.AdapterTask{},
		committedFlushTasks: map[[2]string]*gateway.AdapterTask{}, dmTopics: pydict.New[string, int64](),
		forumCommandRegistered: map[int64]bool{}, modelPickerState: map[string]*modelPickerState{},
		choicePickerState: map[string]*choicePickerState{}, approvalState: map[int]string{}, slashConfirmState: map[string]string{},
		clarifyState: map[string]string{}, statusMessageIDs: pydict.New[[2]string, string](), lastOverflowPreview: map[[2]string]string{},
		topics: &TopicRouting{}, coreHandlers: map[Handler]struct{}{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformTelegram, a)
	a.Traits.SupportsCodeBlocks, a.Traits.SplitsLongMessages, a.Traits.MaxMessageLength = true, true, maxMessageLength
	a.Traits.RequiresEditFinalize = true
	a.Traits.EACodeOpen, a.Traits.EACodeClose, a.Traits.EAReasonBudget = "<pre>", "</pre>\n\n", 500
	a.HeldInbound = NewHeldInbound(a)
	a.core = &adapterCore{base: a.BasePlatformAdapter}
	a.polling = newPollingLifecycle(a)
	extra := cfg.Extra
	a.homeDir = home.Dir(ctx)
	a.admission = NewUpdateAdmission(a.homeDir)
	a.mentionPatterns = a.compileMentionPatterns(ctx)
	a.replyToMode = "first"
	if mode := cfg.ReplyToMode; pyTruthy(mode) {
		a.replyToMode = pyrepr.Str(mode)
	}
	a.disableLinkPreviews = a.core.coerceBoolExtra("disable_link_previews", false)
	a.richMessagesEnabled = a.core.coerceBoolExtra("rich_messages", false)
	a.allowCJKRichMessages = a.core.coerceBoolExtra("allow_cjk_rich_messages", false)
	a.richDraftsEnabled = a.core.coerceBoolExtra("rich_drafts", false)
	maxCooldown, maxRetrigger := 300.0, 30.0
	a.pacer = newSendPacer(a.CoerceFloatExtra("typing_cooldown_seconds", 30.0, 1.0, &maxCooldown))
	a.typingRetriggerInterval = a.CoerceFloatExtra("typing_retrigger_min_interval_seconds", 2.0, 0.0, &maxRetrigger)
	a.mediaBatchDelaySeconds = config.EnvFloat("ICEBATH_TELEGRAM_MEDIA_BATCH_DELAY_SECONDS", 0.8)
	minDelay, maxDelay := 0.08, gateway.TextBatchMaxDelayS
	a.textBatchDelaySeconds = envFloatClamped("ICEBATH_TELEGRAM_TEXT_BATCH_DELAY_SECONDS", gateway.TextBatchDefaultDelayS, &minDelay, &maxDelay)
	minSplit, maxSplit := a.textBatchDelaySeconds, gateway.TextBatchMaxSplitDelayS
	a.textBatchSplitDelay = envFloatClamped("ICEBATH_TELEGRAM_TEXT_BATCH_SPLIT_DELAY_SECONDS", gateway.TextBatchDefaultSplitDelayS, &minSplit, &maxSplit)
	a.SetTextBatchDelays(a.textBatchDelaySeconds, a.textBatchSplitDelay)
	a.statusIndicatorEnabled = pyTruthy(extra["status_indicator"])
	a.statusOnlineText, a.statusOfflineText = "Online", "Offline"
	if v, ok := extra["status_online"]; ok {
		a.statusOnlineText = pyrepr.Str(v)
	}
	if v, ok := extra["status_offline"]; ok {
		a.statusOfflineText = pyrepr.Str(v)
	}
	a.dropPendingOnColdBoot = a.core.coerceBoolExtra("drop_pending_on_cold_boot", true)
	a.dmTopicsConfig = []any{}
	if list, ok := extra["dm_topics"].([]any); ok {
		a.dmTopicsConfig = list
	}
	a.dmTopicChatIDs = map[string]bool{}
	for _, raw := range a.dmTopicsConfig {
		if entry, ok := raw.(map[string]any); ok {
			if cid, has := entry["chat_id"]; has {
				a.dmTopicChatIDs[pyrepr.Str(cid)] = true
			}
		}
	}
	a.maxDocBytes = 20 * 1024 * 1024
	if pyTruthy(extra["base_url"]) {
		a.maxDocBytes = 2 * 1024 * 1024 * 1024
	}
	return a
}

// Live SDK bot, nil while disconnected
func (a *TelegramAdapter) currentBot() *bot.Bot { return a.bot.Load() }

// Installs the bot and its user id, nil clearing both
func (a *TelegramAdapter) setBot(b *bot.Bot) {
	a.bot.Store(b)
	if b == nil {
		a.botUserID.Store(0)
		return
	}
	a.botUserID.Store(b.ID())
}

// Bot user id of the live bot, ok false without one
func (a *TelegramAdapter) botID() (int64, bool) {
	if a.currentBot() == nil {
		return 0, false
	}
	return a.botUserID.Load(), true
}

// Username the SDK cached from getMe, empty without a bot
func (a *TelegramAdapter) cachedBotUsername() string {
	a.mu.Lock()
	app := a.app
	a.mu.Unlock()
	if app == nil || a.currentBot() == nil {
		return ""
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.botUser == nil {
		return ""
	}
	return app.botUser.Username
}

// Telegram measures message length in UTF-16 code units
func (a *TelegramAdapter) MessageLenFn() func(string) int { return gateway.UTF16Len }

// True from polling generation start until the first proven getUpdates round trip
func (a *TelegramAdapter) SendPathDegraded() bool { return a.polling.isSendPathDegraded() }

// Marks connected, re-enables delayed delivery and redispatches the held inbound events
func (a *TelegramAdapter) MarkConnected(ctx context.Context, listenerBase string) {
	a.dropDelayedDeliveries.Store(false)
	a.BasePlatformAdapter.MarkConnected(ctx, listenerBase)
	a.ScheduleHeldInboundRedispatch(ctx)
}

// Marks disconnected so delayed flushes hold instead of dispatching
func (a *TelegramAdapter) MarkDisconnected(ctx context.Context) {
	a.dropDelayedDeliveries.Store(true)
	a.BasePlatformAdapter.MarkDisconnected(ctx)
}

// Records the fatal error and discards the hold queue when no reconnect can drain it
func (a *TelegramAdapter) SetFatalError(ctx context.Context, code, message string, retryable bool) {
	a.dropDelayedDeliveries.Store(true)
	a.BasePlatformAdapter.SetFatalError(ctx, code, message, retryable)
	if !retryable {
		if n := a.HeldInbound.Clear(); n > 0 {
			slog.Warn(fmt.Sprintf("[Telegram] Non-retryable fatal (%s); discarding %d held inbound message(s)", code, n))
		}
	}
}

// True after a non retryable fatal error, holds must discard rather than queue
func (a *TelegramAdapter) IsPermanentFatal() bool {
	if !a.HasFatalError() {
		return false
	}
	return !a.FatalErrorRetryable()
}

// True once teardown or a fatal error started, delayed flushes hold their events
func (a *TelegramAdapter) ShouldDropDelayedDelivery() bool { return a.dropDelayedDeliveries.Load() }

// Gateway runner surface listing the live adapters by platform
type liveAdapters interface {
	Adapters() *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]
}

// Live adapter that replaced this one in the runner, nil when none
func (a *TelegramAdapter) replacementTelegramAdapter() *TelegramAdapter {
	runner, ok := a.GatewayRunner.(liveAdapters)
	if !ok || runner == nil {
		return nil
	}
	adapters := runner.Adapters()
	if adapters == nil {
		return nil
	}
	current, _ := adapters.Get(a.Platform)
	live, _ := current.(*TelegramAdapter)
	if live == nil || live == a || live.currentBot() == nil {
		return nil
	}
	return live
}

// Waits for a bot or a replacement adapter, false on expiry or a permanent fatal error
func (a *TelegramAdapter) waitForReconnection(ctx context.Context) (bool, error) {
	if a.currentBot() != nil || a.replacementTelegramAdapter() != nil {
		return true, nil
	}
	if a.IsPermanentFatal() {
		return false, nil
	}
	waitS, pollS := a.reconnectWaitSeconds, a.reconnectPollInterval
	slog.Info(fmt.Sprintf("[%s] Not connected — waiting for reconnection (up to %.0fs)", a.Name(), waitS))
	waited := 0.0
	for waited < waitS {
		if !sleepCtx(ctx, pollS) {
			return false, ctx.Err()
		}
		waited += pollS
		if a.IsPermanentFatal() {
			return false, nil
		}
		if a.currentBot() != nil || a.replacementTelegramAdapter() != nil {
			slog.Info(fmt.Sprintf("[%s] Reconnected after %.1fs", a.Name(), waited))
			return true, nil
		}
	}
	slog.Warn(fmt.Sprintf("[%s] Still not connected after %.0fs", a.Name(), waitS))
	return false, nil
}

// Marks the current update claim accepted, the handoff being irreversible for replay
func (a *TelegramAdapter) AcceptUpdate(ctx context.Context) {
	if claim := a.admission.CurrentClaim(ctx); claim != nil {
		claim.Accept()
	}
}

// Marks the current update claim failed, a caught preparation error being neither success nor refusal
func (a *TelegramAdapter) failUpdatePreparation(ctx context.Context) {
	if claim := a.admission.CurrentClaim(ctx); claim != nil {
		claim.Fail()
	}
}

// Accepts the update, applies the opt in root DM topic transfer and hands the event to the base pipeline
func (a *TelegramAdapter) HandleMessage(ctx context.Context, event *gateway.MessageEvent) error {
	a.AcceptUpdate(ctx)
	ApplyRootDMTopicTransfer(ctx, a, event)
	return a.BasePlatformAdapter.HandleMessage(ctx, event)
}

// Installs the runner's root DM topic transfer hook, nil clearing it
func (a *TelegramAdapter) SetTopicTransferFn(fn func(ctx context.Context, source *gateway.SessionSource) (string, bool, bool, error)) {
	a.mu.Lock()
	a.topicTransferFn = fn
	a.mu.Unlock()
}

// Root DM topic transfer hook the runner installed, nil when none
func (a *TelegramAdapter) TopicTransferFn() TopicTransferFn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.topicTransferFn
}

// Records the bot identity a heartbeat getMe returned
func (a *TelegramAdapter) NoteBotIdentity(user *models.User) {
	now := monotonicNow()
	a.mu.Lock()
	a.botIdentityCheckedAt = &now
	a.mu.Unlock()
	if user != nil {
		a.noteBotUsername(user.Username)
	}
}

// Configured bot token text, empty when unset
func tokenText(token any) string {
	if token == nil {
		return ""
	}
	if s, ok := token.(string); ok {
		return s
	}
	return pyrepr.Str(token)
}

// Whether the extra value is set and non empty
func extraString(extra map[string]any, key string) string {
	v, ok := extra[key]
	if !ok || !pyTruthy(v) {
		return ""
	}
	return pystr.Strip(pyrepr.Str(v))
}

// Bot API default server of the SDK the PTB default base url maps onto
const defaultBotAPIServer = "https://api.telegram.org"

// Joins strings with a separator after dropping empty ones
func joinNonEmpty(sep string, parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}
