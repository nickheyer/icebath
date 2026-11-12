package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Lifecycle hook probe the platform event observer consults
var hasHook = plugins.HasHook

// Builds a fresh application over the shared request pair and processor, the PTB builder build
func (a *TelegramAdapter) buildApp(token string, general, polling *ptbRequest, processor UpdateProcessor) (*ptbApp, *TelegramApplication, error) {
	var tgApp *TelegramApplication
	deliver := func(ctx context.Context, update *models.Update) {
		if update == nil || tgApp == nil {
			return
		}
		u := &Update{UpdateID: update.ID, Raw: update}
		if m := effectiveMessage(update); m != nil {
			u.Chat = &Chat{ID: m.Chat.ID}
		} else if chat := effectiveChatOf(update); chat != nil {
			u.Chat = &Chat{ID: chat.ID}
		}
		if err := tgApp.FetchUpdate(ctx, u); err != nil {
			slog.Debug(fmt.Sprintf("[%s] Telegram update dispatch error", a.Name()), "error", err)
		}
	}
	app, err := newPTBApp(token, defaultBotAPIServer, general, polling, deliver)
	if err != nil {
		return nil, nil, err
	}
	extra := a.Config.Extra
	if baseURL := extraString(extra, "base_url"); baseURL != "" {
		fileURL := baseURL
		if v, ok := extra["base_file_url"]; ok {
			fileURL = pyrepr.Str(v)
		}
		app.useBaseURLs(token, baseURL, fileURL, pyTruthy(extra["local_mode"]))
	} else if pyTruthy(extra["local_mode"]) {
		app.localMode = true
	}
	tgApp = NewTelegramApplication(app.sdk.ID(), processor, a, a.admission)
	return app, tgApp, nil
}

// Chat an update without an effective message belongs to, the PTB effective_chat of reactions and member updates
func effectiveChatOf(u *models.Update) *models.Chat {
	switch {
	case u.MessageReaction != nil:
		return &u.MessageReaction.Chat
	case u.MessageReactionCount != nil:
		return &u.MessageReactionCount.Chat
	case u.MyChatMember != nil:
		return &u.MyChatMember.Chat
	case u.ChatMember != nil:
		return &u.ChatMember.Chat
	case u.ChatJoinRequest != nil:
		return &u.ChatJoinRequest.Chat
	case u.ChatBoost != nil:
		return &u.ChatBoost.Chat
	case u.RemovedChatBoost != nil:
		return &u.RemovedChatBoost.Chat
	}
	return nil
}

// Installs a freshly built application as the live one
func (a *TelegramAdapter) installApp(app *ptbApp, tgApp *TelegramApplication) {
	a.mu.Lock()
	a.app, a.tgApp = app, tgApp
	a.mu.Unlock()
	a.polling.mu.Lock()
	a.polling.app = app
	a.polling.mu.Unlock()
	a.setBot(app.sdk)
}

// Connects through long polling, or a webhook listener when TELEGRAM_WEBHOOK_URL is set
func (a *TelegramAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	a.polling.mu.Lock()
	a.polling.teardownStarted, a.polling.webhookMode = false, false
	a.polling.recoveryCtx = context.WithoutCancel(ctx)
	a.polling.mu.Unlock()
	token := tokenText(a.Config.Token)
	if token == "" {
		slog.Error(fmt.Sprintf("[%s] No bot token configured", a.Name()))
		a.SetFatalError(ctx, "missing_credentials", "No bot token configured", false)
		return false, nil
	}
	ok, err := a.connectBody(ctx, token, isReconnect)
	if err == nil {
		return ok, nil
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return false, err
	}
	a.ReleasePlatformLock()
	safeError := redactTelegramErrorText(err)
	if looksLikeAuthError(err) {
		message := fmt.Sprintf("Telegram bot token rejected: %s. The token is invalid or was revoked — generate a new one "+
			"with @BotFather and update TELEGRAM_BOT_TOKEN.", safeError)
		a.SetFatalError(ctx, "telegram_auth_error", message, false)
	} else {
		a.SetFatalError(ctx, "telegram_connect_error", "Telegram startup failed: "+safeError, true)
	}
	slog.Error(fmt.Sprintf("[%s] Failed to connect to Telegram: %s", a.Name(), safeError))
	return false, nil
}

// Connect body whose every failure the caller classifies into a fatal error
func (a *TelegramAdapter) connectBody(ctx context.Context, token string, isReconnect bool) (bool, error) {
	acquired, err := a.AcquirePlatformLock(ctx, "telegram-bot-token", token, "Telegram bot token")
	if err != nil {
		return false, err
	}
	if !acquired {
		return false, nil
	}
	extra := a.Config.Extra
	if baseURL := extraString(extra, "base_url"); baseURL != "" {
		slog.Info(fmt.Sprintf("[%s] Using custom Telegram base_url: %s", a.Name(), pyrepr.Str(extra["base_url"])))
	}
	if pyTruthy(extra["local_mode"]) {
		slog.Info(fmt.Sprintf("[%s] Using Telegram local_mode (read files from disk)", a.Name()))
	}
	general, polling, err := buildPTBRequests(ctx, a.Name(), extra, a.polling)
	if err != nil {
		return false, err
	}
	processor := BuildUpdateProcessor(extra, a.Name())
	app, tgApp, err := a.buildApp(token, general, polling, processor)
	if err != nil {
		return false, err
	}
	a.installApp(app, tgApp)
	a.WirePluginHandlers(ctx, tgApp)
	a.registerHandlers(tgApp)
	rebuild := func() (*ptbApp, *TelegramApplication, error) { return a.buildApp(token, general, polling, processor) }
	if err := a.initializeAppWithRetries(ctx, rebuild); err != nil {
		return false, err
	}
	a.mu.Lock()
	started := a.tgApp
	a.mu.Unlock()
	started.Start()
	webhookURL, err := gateway.GetScopedSecret(ctx, "TELEGRAM_WEBHOOK_URL", "", true)
	if err != nil {
		return false, err
	}
	if webhookURL = pystr.Strip(webhookURL); webhookURL != "" {
		if err := a.startWebhookMode(ctx, webhookURL, isReconnect); err != nil {
			return false, err
		}
	} else if err := a.startPollingMode(ctx, isReconnect); err != nil {
		return false, err
	}
	a.MarkConnected(ctx, "")
	webhook := a.isWebhookMode()
	mode := "polling"
	if webhook {
		mode = "webhook"
	}
	slog.Warn(fmt.Sprintf("[%s] Connected to Telegram (%s mode)", a.Name(), mode))
	if !webhook {
		a.restartHeartbeat(ctx)
	}
	a.noteBotUsername(a.cachedBotUsername())
	now := monotonicNow()
	a.mu.Lock()
	a.botIdentityCheckedAt = &now
	a.mu.Unlock()
	if webhook {
		a.mu.Lock()
		prior := a.identityRefreshTask
		a.mu.Unlock()
		if prior != nil && !prior.Done() {
			prior.Cancel()
		}
		task := gateway.SpawnAdapterTask(context.WithoutCancel(ctx), a.botIdentityRefreshLoop)
		a.mu.Lock()
		a.identityRefreshTask = task
		a.mu.Unlock()
	}
	a.startPostConnectHousekeeping(ctx)
	return true, nil
}

// Whether the live connection runs in webhook mode
func (a *TelegramAdapter) isWebhookMode() bool {
	a.polling.mu.Lock()
	defer a.polling.mu.Unlock()
	return a.polling.webhookMode
}

// Cancels any live heartbeat and starts the polling heartbeat loop in its place
func (a *TelegramAdapter) restartHeartbeat(ctx context.Context) {
	a.polling.mu.Lock()
	prior := a.polling.heartbeatTask
	a.polling.mu.Unlock()
	if prior != nil && !prior.Done() {
		prior.Cancel()
	}
	task := gateway.SpawnAdapterTask(context.WithoutCancel(ctx), func(c context.Context) error {
		a.polling.pollingHeartbeatLoop(c)
		return nil
	})
	a.polling.mu.Lock()
	a.polling.heartbeatTask = task
	a.polling.mu.Unlock()
}

// Initializes the application with a bounded retry ladder, rebuilding it after each failed attempt
func (a *TelegramAdapter) initializeAppWithRetries(ctx context.Context, rebuild func() (*ptbApp, *TelegramApplication, error)) error {
	const maxConnect = 8
	initTimeout := config.EnvFloat("ICEBATH_TELEGRAM_INIT_TIMEOUT", 30.0)
	totalBudget := initTimeout*maxConnect + 120.0
	totalDeadline := monotonicNow() + totalBudget
	timedOut := fmt.Sprintf("Telegram initialization timed out after %d attempts (%.0fs each)", maxConnect, initTimeout)
	for attempt := 0; attempt < maxConnect; attempt++ {
		if monotonicNow() >= totalDeadline {
			return &osError{msg: fmt.Sprintf("%s — total connect watchdog deadline (%.0fs) exceeded. "+
				"Check network connectivity to api.telegram.org or set ICEBATH_TELEGRAM_HTTP_CONNECT_TIMEOUT / "+
				"ICEBATH_TELEGRAM_INIT_TIMEOUT to a lower value.", timedOut, totalBudget)}
		}
		slog.Warn(fmt.Sprintf("[%s] Connecting to Telegram (attempt %d/%d)…", a.Name(), attempt+1, maxConnect))
		a.mu.Lock()
		app := a.app
		a.mu.Unlock()
		_, err := awaitWithThreadDeadline(ctx, func(c context.Context) (any, error) { return nil, app.Initialize(c) }, initTimeout,
			func(c context.Context) error { shutdownAbandonedApp(c, app); return nil }, "telegram-init", true)
		if err == nil {
			return nil
		}
		var timeout *deadlineTimeoutError
		wait := math.Min(math.Pow(2, float64(attempt)), 15)
		switch {
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil:
			slog.Warn(fmt.Sprintf("[%s] Connect attempt %d/%d interrupted by %s — propagating", a.Name(), attempt+1, maxConnect, "CancelledError"))
			return err
		case errors.As(err, &timeout):
			if attempt >= maxConnect-1 {
				return &osError{msg: timedOut + ". Check network connectivity to api.telegram.org " +
					"or set ICEBATH_TELEGRAM_HTTP_CONNECT_TIMEOUT to a lower value."}
			}
			slog.Warn(fmt.Sprintf("[%s] Connect attempt %d/%d timed out after %.0fs — retrying in %ds", a.Name(), attempt+1, maxConnect,
				initTimeout, int(wait)))
		default:
			if (!isOSError(err) && !looksLikeNetworkError(err)) || attempt >= maxConnect-1 {
				return err
			}
			slog.Warn(fmt.Sprintf("[%s] Connect attempt %d/%d failed: %s — retrying in %ds", a.Name(), attempt+1, maxConnect,
				pyrepr.ExceptionMessage(err), int(wait)))
		}
		if attempt < maxConnect-1 {
			newApp, newTgApp, buildErr := rebuild()
			if buildErr != nil {
				return buildErr
			}
			a.installApp(newApp, newTgApp)
			a.WirePluginHandlers(ctx, newTgApp)
			a.registerHandlers(newTgApp)
			shutdownAbandonedApp(context.WithoutCancel(ctx), app)
		}
		if !sleepCtx(ctx, wait) {
			slog.Warn(fmt.Sprintf("[%s] Connect attempt %d/%d interrupted by %s — propagating", a.Name(), attempt+1, maxConnect, "CancelledError"))
			return ctx.Err()
		}
	}
	return nil
}

// Whether this connection discards the queued updates, logging the cold boot decision
func (a *TelegramAdapter) coldBootDropPending(isReconnect bool) bool {
	return coldBootDropPending(a.Name(), a.dropPendingOnColdBoot, isReconnect)
}

// Starts the webhook listener, refusing to run fail open without TELEGRAM_WEBHOOK_SECRET
func (a *TelegramAdapter) startWebhookMode(ctx context.Context, webhookURL string, isReconnect bool) error {
	port := int(config.EnvInt("TELEGRAM_WEBHOOK_PORT", 8443))
	host := pystr.Strip(os.Getenv("TELEGRAM_WEBHOOK_HOST"))
	if host == "" {
		host = extraString(a.Config.Extra, "webhook_host")
	}
	secret, err := gateway.GetScopedSecret(ctx, "TELEGRAM_WEBHOOK_SECRET", "", true)
	if err != nil {
		return err
	}
	if secret = pystr.Strip(secret); secret == "" {
		return &runtimeError{msg: "TELEGRAM_WEBHOOK_SECRET is required when TELEGRAM_WEBHOOK_URL is set. Without it, the " +
			"webhook endpoint accepts forged updates from anyone who can reach it — see " +
			"https://github.com/NousResearch/hermes-agent/security/advisories/GHSA-3vpc-7q5r-276h.\n\n" +
			"Generate a secret and set it in your .env:\n  export TELEGRAM_WEBHOOK_SECRET=\"$(openssl rand -hex 32)\"\n\n" +
			"Then register it with Telegram when setting the webhook via setWebhook's secret_token parameter."}
	}
	path := "/telegram"
	if parsed, perr := url.Parse(webhookURL); perr == nil && parsed.Path != "" {
		path = parsed.Path
	}
	a.mu.Lock()
	app := a.app
	a.mu.Unlock()
	if err := app.updater.StartWebhook(ctx, host, port, path, webhookURL, secret, a.coldBootDropPending(isReconnect)); err != nil {
		return err
	}
	a.polling.mu.Lock()
	a.polling.webhookMode, a.polling.progressAccepting, a.polling.sendPathDegraded = true, false, false
	a.polling.mu.Unlock()
	listen := host
	if listen == "" {
		listen = "* (all interfaces, IPv4+IPv6)"
	}
	slog.Info(fmt.Sprintf("[%s] Webhook server listening on %s:%d%s", a.Name(), listen, port, path))
	return nil
}

// Clears any stale webhook and starts resilient long polling with the recovery error callback
func (a *TelegramAdapter) startPollingMode(ctx context.Context, isReconnect bool) error {
	if _, err := a.polling.deleteStaleWebhook(ctx, !isReconnect); err != nil {
		return err
	}
	p := a.polling
	var errorCallback func(error)
	errorCallback = func(err error) {
		if p.isTeardownStarted() || p.recoveryInFlight() {
			return
		}
		if looksLikePollingConflict(err) {
			p.disarmPTBRetryLoop()
			p.spawnPollingRecovery(func(c context.Context) error { return p.handlePollingConflict(c, err) })
		} else if looksLikeNetworkError(err) {
			slog.Warn(fmt.Sprintf("[%s] Telegram network error, scheduling reconnect: %s", a.Name(), redactTelegramErrorText(err)))
			p.spawnPollingRecovery(func(c context.Context) error { p.handlePollingNetworkError(c, err); return nil })
		} else {
			slog.Error(fmt.Sprintf("[%s] Telegram polling error: %s", a.Name(), redactTelegramErrorText(err)))
		}
	}
	p.mu.Lock()
	p.errorCallbackRef = errorCallback
	p.mu.Unlock()
	started, err := p.startPollingResilient(ctx, a.coldBootDropPending(isReconnect), errorCallback, !isReconnect)
	if err != nil {
		return err
	}
	if !started {
		slog.Warn(fmt.Sprintf("[%s] Connected in degraded Telegram mode: gateway is alive, polling will be retried in the background", a.Name()))
	}
	return nil
}

// Registers every core handler on the application and remembers them so a late plugin rewire lands before them
func (a *TelegramAdapter) registerHandlers(app *TelegramApplication) {
	before := map[Handler]bool{}
	for _, group := range app.groupSnapshot() {
		for _, h := range group {
			before[h] = true
		}
	}
	app.AddHandler(&messageHandler{filter: filterTextNotCommand, callback: a.boundCallback(a.handleTextMessage)}, 0)
	app.AddHandler(&messageHandler{filter: filterCommand, callback: a.boundCallback(a.handleCommand)}, 0)
	app.AddHandler(&messageHandler{filter: filterLocation, callback: a.boundCallback(a.handleLocationMessage)}, 0)
	app.AddHandler(&messageHandler{filter: filterMedia, callback: a.boundCallback(a.handleMediaMessage)}, 0)
	app.AddHandler(&messageHandler{filter: filterForumTopicCreated, callback: a.boundCallback(a.handleForumTopicCreated)}, 0)
	app.AddHandler(&callbackQueryHandler{callback: a.boundCallback(a.handleCallbackQuery)}, 0)
	app.AddHandler(&inlineQueryHandler{callback: a.boundCallback(a.handleInlineQuery)}, 0)
	app.AddHandler(NewTypeHandler[*Update](a.boundCallback(a.onPlatformUpdate)), 99)
	core := map[Handler]struct{}{}
	for _, group := range app.groupSnapshot() {
		for _, h := range group {
			if !before[h] {
				core[h] = struct{}{}
			}
		}
	}
	a.mu.Lock()
	a.coreHandlers = core
	a.mu.Unlock()
}

// Wires plugin handler factories, hoisting handlers a late factory added ahead of the first core handler of their group
func (a *TelegramAdapter) WirePluginHandlers(ctx context.Context, native any) {
	app, _ := native.(*TelegramApplication)
	a.mu.Lock()
	coreIDs := a.coreHandlers
	a.mu.Unlock()
	if app == nil || len(coreIDs) == 0 {
		a.BasePlatformAdapter.WirePluginHandlers(ctx, native)
		return
	}
	before := app.handlerGroups()
	a.BasePlatformAdapter.WirePluginHandlers(ctx, native)
	app.mu.Lock()
	defer app.mu.Unlock()
	for group, current := range app.handlers {
		prior := before[group]
		priorIDs := map[Handler]bool{}
		for _, h := range prior {
			priorIDs[h] = true
		}
		var added []Handler
		for _, h := range current {
			if !priorIDs[h] {
				added = append(added, h)
			}
		}
		firstCore := -1
		for i, h := range prior {
			if _, ok := coreIDs[h]; ok {
				firstCore = i
				break
			}
		}
		if len(added) == 0 || firstCore < 0 {
			continue
		}
		merged := append(append(append([]Handler{}, prior[:firstCore]...), added...), prior[firstCore:]...)
		app.handlers[group] = merged
	}
}

// Copy of every handler group keyed by group number
func (t *TelegramApplication) handlerGroups() map[int][]Handler {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := map[int][]Handler{}
	for g, hs := range t.handlers {
		out[g] = append([]Handler(nil), hs...)
	}
	return out
}

// Keeps the cached username fresh in webhook mode where no heartbeat calls getMe
func (a *TelegramAdapter) botIdentityRefreshLoop(ctx context.Context) error {
	for {
		if !sleepCtx(ctx, botIdentityTTLSeconds) {
			return nil
		}
		if a.polling.isTeardownStarted() || a.HasFatalError() {
			return nil
		}
		a.refreshBotIdentity(ctx, true)
	}
}

// Starts the deferred post connect housekeeping unless a run is still in flight
func (a *TelegramAdapter) startPostConnectHousekeeping(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.postConnectTask != nil && !a.postConnectTask.Done() {
		return
	}
	a.postConnectTask = gateway.SpawnAdapterTask(context.WithoutCancel(ctx), a.runPostConnectHousekeeping)
}

// Command menu, status indicator and DM topics off the connect path, every step non fatal
func (a *TelegramAdapter) runPostConnectHousekeeping(ctx context.Context) error {
	defer func() {
		a.mu.Lock()
		if a.postConnectTask == gateway.CurrentAdapterTask(ctx) {
			a.postConnectTask = nil
		}
		a.mu.Unlock()
	}()
	if err := a.registerCommandMenu(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[%s] Could not register Telegram command menu: %s", a.Name(), redactTelegramErrorText(err)))
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	a.setStatusIndicator(ctx, true)
	if err := a.setupDMTopics(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[%s] DM topics setup failed (non-fatal): %s", a.Name(), pyrepr.ExceptionMessage(err)))
	}
	return ctx.Err()
}

// Bot command rows with descriptions cut at the Telegram cap
func botCommands(menu []CatalogItem) []models.BotCommand {
	out := make([]models.BotCommand, 0, len(menu))
	for _, item := range menu {
		out = append(out, models.BotCommand{Command: item.Name, Description: clipRunes(item.Description, botCommandDescriptionLimit)})
	}
	return out
}

// Hash of the active language and the menu payload
func menuFingerprint(ctx context.Context, menu []CatalogItem) string {
	rows := make([]any, 0, len(menu))
	for _, item := range menu {
		rows = append(rows, pyrepr.Tuple{item.Name, item.Description})
	}
	payload := pyrepr.Repr([]any{i18n.GetLanguage(ctx), rows})
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// Registers the command menu in the default, private and group scopes
func (a *TelegramAdapter) registerCommandMenu(ctx context.Context) error {
	b := a.currentBot()
	if b == nil {
		return nil
	}
	maxCommands := menuCommands().TelegramMenuMaxCommands(ctx)
	menu, hidden, err := menuCommands().TelegramMenuCommands(ctx, maxCommands)
	if err != nil {
		return err
	}
	commands := botCommands(menu)
	fingerprint := menuFingerprint(ctx, menu)
	a.mu.Lock()
	if fingerprint != a.commandMenuFingerprint {
		a.forumCommandRegistered = map[int64]bool{}
		a.commandMenuFingerprint = fingerprint
	}
	a.mu.Unlock()
	scopes := []struct {
		name  string
		scope models.BotCommandScope
	}{{"BotCommandScopeDefault", &models.BotCommandScopeDefault{}}, {"BotCommandScopeAllPrivateChats", &models.BotCommandScopeAllPrivateChats{}},
		{"BotCommandScopeAllGroupChats", &models.BotCommandScopeAllGroupChats{}}}
	for _, s := range scopes {
		if _, err := b.SetMyCommands(ctx, &bot.SetMyCommandsParams{Commands: commands, Scope: s.scope}); err != nil {
			slog.Warn(fmt.Sprintf("[%s] set_my_commands FAILED for scope %s: %s", a.Name(), s.name, pyrepr.ExceptionMessage(ptbError(err))))
			continue
		}
		slog.Info(fmt.Sprintf("[%s] set_my_commands OK for scope %s (%d cmds)", a.Name(), s.name, len(commands)))
	}
	if hidden > 0 {
		slog.Info(fmt.Sprintf("[%s] Telegram menu: %d commands registered, %d hidden (over %d limit). Use /commands for full list.",
			a.Name(), len(menu), hidden, maxCommands))
	}
	return nil
}

// Sets the bot short description to the online or offline text when the status indicator is enabled
func (a *TelegramAdapter) setStatusIndicator(ctx context.Context, online bool) {
	if !a.statusIndicatorEnabled {
		return
	}
	b := a.currentBot()
	if b == nil {
		return
	}
	text := a.statusOfflineText
	if online {
		text = a.statusOnlineText
	}
	text = clipRunes(text, 120)
	if _, err := b.SetMyShortDescription(ctx, &bot.SetMyShortDescriptionParams{ShortDescription: text}); err != nil {
		slog.Debug(fmt.Sprintf("[%s] Failed to set bot status indicator to %s: %s", a.Name(), pyrepr.Repr(text), redactTelegramErrorText(ptbError(err))))
		return
	}
	slog.Info(fmt.Sprintf("[%s] Set bot status indicator to %s", a.Name(), pyrepr.Repr(text)))
}

// Unique unfinished tasks of the candidates other than the current one
func collectLiveTasks(candidates []*gateway.AdapterTask, current *gateway.AdapterTask) []*gateway.AdapterTask {
	seen := map[*gateway.AdapterTask]bool{}
	var out []*gateway.AdapterTask
	for _, t := range candidates {
		if t == nil || t.Done() || t == current || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// Awaits one disconnect step, detaching it on timeout so teardown advances
func (a *TelegramAdapter) awaitDisconnectStep(ctx context.Context, fn func(ctx context.Context) error, timeout float64, step string) bool {
	task := gateway.SpawnAdapterTask(context.WithoutCancel(ctx), fn)
	waitCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, seconds(timeout))
	}
	defer cancel()
	err := task.Wait(waitCtx)
	if task.Done() {
		return true
	}
	task.Cancel()
	if ctx.Err() != nil {
		return false
	}
	_ = err
	slog.Warn(fmt.Sprintf("[%s] %s timed out after %.1fs during disconnect; continuing teardown", a.Name(), step, timeout))
	return false
}

// Cancels and bounded awaits a task, the caller clearing its slot
func (a *TelegramAdapter) cancelTask(ctx context.Context, task *gateway.AdapterTask, label string) {
	if task == nil || task.Done() {
		return
	}
	task.Cancel()
	a.awaitDisconnectStep(ctx, func(c context.Context) error { return task.Wait(c) }, disconnectStepTimeout, label)
}

// Cancels every delayed delivery task family and salvages buffered inbound events into the hold queue
func (a *TelegramAdapter) cancelPendingDeliveryTasks(ctx context.Context) error {
	current := gateway.CurrentAdapterTask(ctx)
	a.mu.Lock()
	var candidates []*gateway.AdapterTask
	for _, t := range a.mediaGroupTasks {
		candidates = append(candidates, t)
	}
	for _, t := range a.pendingPhotoBatchTasks {
		candidates = append(candidates, t)
	}
	a.mu.Unlock()
	candidates = append(candidates, a.TextBatchTasks()...)
	a.mu.Lock()
	for _, t := range a.committedFlushTasks {
		candidates = append(candidates, t)
	}
	a.mu.Unlock()
	a.polling.mu.Lock()
	candidates = append(candidates, a.polling.errorTask, a.polling.verifierTask)
	a.polling.mu.Unlock()
	candidates = append(candidates, a.HeldInbound.RedispatchTask())
	pending := collectLiveTasks(candidates, current)
	for _, t := range pending {
		t.Cancel()
	}
	for _, t := range pending {
		_ = t.Wait(context.WithoutCancel(ctx))
	}
	a.mu.Lock()
	photos, groups := mapValues(a.pendingPhotoBatches), mapValues(a.mediaGroupEvents)
	a.mu.Unlock()
	texts := a.TextBatchEvents()
	if a.IsPermanentFatal() {
		if n := len(texts) + len(photos) + len(groups); n > 0 {
			slog.Warn(fmt.Sprintf("[Telegram] Non-retryable fatal teardown; discarding %d pending inbound batch(es)", n))
		}
	} else {
		for _, batch := range []struct {
			events []*gateway.MessageEvent
			where  string
		}{{texts, "text-batch-teardown"}, {photos, "photo-batch-teardown"}, {groups, "media-group-teardown"}} {
			for _, event := range batch.events {
				a.HoldInboundEvent(ctx, event, batch.where, true)
			}
		}
	}
	a.mu.Lock()
	a.mediaGroupTasks, a.mediaGroupEvents = map[string]*gateway.AdapterTask{}, map[string]*gateway.MessageEvent{}
	a.pendingPhotoBatchTasks, a.pendingPhotoBatches = map[string]*gateway.AdapterTask{}, map[string]*gateway.MessageEvent{}
	a.mu.Unlock()
	a.ClearTextBatches()
	a.polling.mu.Lock()
	if a.polling.errorTask != current {
		a.polling.errorTask = nil
	}
	if a.polling.verifierTask != current {
		a.polling.verifierTask = nil
	}
	a.polling.mu.Unlock()
	a.HeldInbound.clearRedispatchTaskExcept(current)
	return nil
}

// Values of a string keyed event map
func mapValues(m map[string]*gateway.MessageEvent) []*gateway.MessageEvent {
	out := make([]*gateway.MessageEvent, 0, len(m))
	for _, e := range m {
		out = append(out, e)
	}
	return out
}

// Stops polling or the webhook, cancels delayed deliveries and tears the application down
func (a *TelegramAdapter) Disconnect(ctx context.Context) error {
	a.MarkDisconnected(ctx)
	p := a.polling
	p.mu.Lock()
	p.teardownStarted = true
	p.progressAccepting = false
	p.generation++
	p.progress = newPollEvent()
	p.sendPathDegraded = true
	p.mu.Unlock()
	a.ReleasePlatformLock()
	current := gateway.CurrentAdapterTask(ctx)
	p.mu.Lock()
	lifecycle := collectLiveTasks([]*gateway.AdapterTask{p.errorTask, p.verifierTask}, current)
	p.mu.Unlock()
	for _, t := range lifecycle {
		t.Cancel()
	}
	if len(lifecycle) > 0 {
		a.awaitDisconnectStep(ctx, func(c context.Context) error {
			for _, t := range lifecycle {
				_ = t.Wait(c)
			}
			return nil
		}, disconnectStepTimeout, "lifecycle-task cancel")
	}
	p.mu.Lock()
	if p.errorTask != current {
		p.errorTask = nil
	}
	if p.verifierTask != current {
		p.verifierTask = nil
	}
	p.progressAccepting = false
	p.sendPathDegraded = true
	heartbeat := p.heartbeatTask
	p.mu.Unlock()
	a.mu.Lock()
	postConnect, identity := a.postConnectTask, a.identityRefreshTask
	a.mu.Unlock()
	if postConnect != nil && !postConnect.Done() {
		postConnect.Cancel()
		a.awaitDisconnectStep(ctx, func(c context.Context) error { _ = postConnect.Wait(c); return nil }, disconnectStepTimeout,
			"post-connect cancel")
	}
	a.mu.Lock()
	a.postConnectTask = nil
	a.mu.Unlock()
	a.cancelTask(ctx, heartbeat, "heartbeat cancel")
	p.mu.Lock()
	p.heartbeatTask = nil
	p.mu.Unlock()
	a.cancelTask(ctx, identity, "identity-refresh cancel")
	a.mu.Lock()
	a.identityRefreshTask = nil
	a.mu.Unlock()
	a.awaitDisconnectStep(ctx, func(c context.Context) error { a.setStatusIndicator(c, false); return nil }, disconnectStepTimeout,
		"status-indicator update")
	a.awaitDisconnectStep(ctx, a.cancelPendingDeliveryTasks, disconnectStepTimeout, "pending-delivery cancel")
	a.mu.Lock()
	app, tgApp := a.app, a.tgApp
	a.mu.Unlock()
	if app != nil {
		if app.updater != nil && app.updater.Running() {
			stopErr := make(chan error, 1)
			a.awaitDisconnectStep(ctx, func(c context.Context) error { err := app.updater.Stop(c); stopErr <- err; return err },
				p.timings.updaterStopTimeout, "updater.stop()")
			select {
			case err := <-stopErr:
				if err != nil {
					slog.Warn(fmt.Sprintf("[%s] updater.stop() failed during disconnect: %s", a.Name(), redactTelegramErrorText(err)))
				}
			default:
			}
		}
		if tgApp != nil && tgApp.Running() {
			a.awaitDisconnectStep(ctx, tgApp.Stop, disconnectStepTimeout, "app.stop()")
		}
		a.awaitDisconnectStep(ctx, app.Shutdown, disconnectStepTimeout, "app.shutdown()")
	}
	a.mu.Lock()
	a.app, a.tgApp = nil, nil
	a.mu.Unlock()
	p.mu.Lock()
	p.app = nil
	p.mu.Unlock()
	a.setBot(nil)
	if flush := a.admission.ReceiptFlush(); flush != nil && !flush.Done() {
		a.awaitDisconnectStep(ctx, func(c context.Context) error { return flush.Wait(c) }, disconnectStepTimeout, "update-receipt flush")
	}
	slog.Info(fmt.Sprintf("[%s] Disconnected from Telegram", a.Name()))
	return nil
}

// Posts one raw Bot API method through the general request and returns its decoded result, the PTB do_api_request
func (a *TelegramAdapter) doAPIRequest(ctx context.Context, method string, payload map[string]any) (any, error) {
	a.mu.Lock()
	app := a.app
	a.mu.Unlock()
	if app == nil {
		return nil, &runtimeError{msg: "Not connected"}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	endpoint := app.serverURL + "/bot" + app.sdk.Token() + "/" + method
	req, err := newJSONRequest(ctx, endpoint, body)
	if err != nil {
		return nil, err
	}
	resp, err := app.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
	}
	raw, err := readAll(resp)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, newTelegramError("TelegramError", "Invalid server response", err)
	}
	if !envelope.OK {
		return nil, ptbResponseError(resp.StatusCode, raw)
	}
	var result any
	if len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, &result); err != nil {
			return nil, newTelegramError("TelegramError", "Invalid server response", err)
		}
	}
	return result, nil
}

// Routes a gateway_platform_event per inbound update with an internal auth source, never raising into dispatch
func (a *TelegramAdapter) onPlatformUpdate(ctx context.Context, update *Update) error {
	if a.admission.CurrentClaim(ctx) == nil {
		a.polling.recordUpdateDispatched()
	}
	handler := a.PlatformEventHandler()
	if handler == nil {
		return nil
	}
	if !hasHook(ctx, "gateway_platform_event") {
		return nil
	}
	fields, err := updateFields(update.Raw)
	if err != nil {
		a.failUpdatePreparation(ctx)
		slog.Debug(fmt.Sprintf("[%s] gateway_platform_event normalize error", a.Name()), "error", err)
		return nil
	}
	event := normalizePlatformEvent(fields)
	if event == nil {
		return nil
	}
	source, err := a.sourceForPlatformEventAuth(ctx, update.Raw)
	if err == nil {
		a.AcceptUpdate(ctx)
		err = handler(ctx, event, source)
	}
	if err != nil {
		a.failUpdatePreparation(ctx)
		slog.Debug(fmt.Sprintf("[%s] gateway_platform_event dispatch error", a.Name()), "error", err)
	}
	return nil
}

// Python ValueError raised by the platform event auth boundary
type valueError struct{ msg string }

func (e *valueError) Error() string      { return e.msg }
func (e *valueError) PyTypeName() string { return "ValueError" }

// Auth source of a reaction or edit update, a ValueError for update types without an extractor
func (a *TelegramAdapter) sourceForPlatformEventAuth(ctx context.Context, update *models.Update) (*gateway.SessionSource, error) {
	if update.MessageReaction != nil {
		return a.core.sourceFromReactionForAuth(ctx, update)
	}
	if update.EditedMessage != nil {
		source := a.core.sourceFromMessageForAuth(update.EditedMessage)
		if source.UserID == nil || *source.UserID == "" || source.ChatID == "" {
			return nil, &valueError{msg: "gateway_platform_event message_edited requires editor and chat identities"}
		}
		return source, nil
	}
	return nil, &valueError{msg: "gateway_platform_event source extraction has no extractor for this update type"}
}

// JSON POST request of one Bot API method
func newJSONRequest(ctx context.Context, endpoint string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// Whole response body
func readAll(resp *http.Response) ([]byte, error) { return io.ReadAll(resp.Body) }
