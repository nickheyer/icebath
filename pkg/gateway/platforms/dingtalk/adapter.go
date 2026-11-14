package dingtalk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	maxMessageLength = 20000
	// Identical errors escaping the stream client before the breaker trips
	reconnectCircuitBreakerTrips = 5
	// Seconds between attempts while the breaker is tripped
	reconnectCircuitBreakerDelay = 300
	// Seconds identical stream client log records are collapsed within
	sdkLogRepeatWindow  = 300.0
	sessionWebhooksMax  = 500
	emotionID           = "2659900"
	emotionBG           = "im_bg_1"
	noLocalUploadFormat = "DingTalk session webhook replies do not support local %s. Only markdown/text replies are supported without OpenAPI %s."
)

var (
	// Seconds to wait before each reconnect attempt, the last value repeating
	reconnectBackoff  = []int{2, 5, 10, 30, 60}
	dingtalkWebhookRE = pyre.MustCompile(`^https://(?:api|oapi)\.dingtalk\.com/`)
	truthyValues      = map[string]bool{"true": true, "1": true, "yes": true, "on": true}
	numberedRE        = pyre.MustCompile(`^\d+\.\s`)
)

// Hex of a fresh uuid4
func uuidHex() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }

// Whether an error chain names the websockets context manager mismatch within four links
func isSDKIncompat(exc error) bool {
	for i := 0; i < 4; i++ {
		if exc == nil {
			return false
		}
		if pyrepr.ExceptionName(exc) == "TypeError" && strings.Contains(pyrepr.ExceptionMessage(exc), "asynchronous context manager") {
			return true
		}
		exc = errors.Unwrap(exc)
	}
	return false
}

// Key of one collapsed log record, its level, leading text and error class
type sdkLogKey struct {
	level slog.Level
	msg   string
	exc   string
}

// Collapse state shared by a guard and the handlers derived from it
type sdkLogState struct {
	onException func(exc error)
	window      float64
	started     time.Time

	mu   sync.Mutex
	seen map[sdkLogKey]*[2]float64
}

// Log handler collapsing repeated stream client records and reporting their errors to a callback
type sdkLogGuard struct {
	next  slog.Handler
	state *sdkLogState
}

// Guard over a handler with the repeat window in seconds
func newSDKLogGuard(next slog.Handler, onException func(exc error), window float64) *sdkLogGuard {
	return &sdkLogGuard{next: next, state: &sdkLogState{onException: onException, window: window, started: time.Now(),
		seen: map[sdkLogKey]*[2]float64{}}}
}

func (g *sdkLogGuard) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo && g.next.Enabled(ctx, level)
}

func (g *sdkLogGuard) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &sdkLogGuard{next: g.next.WithAttrs(attrs), state: g.state}
}

func (g *sdkLogGuard) WithGroup(name string) slog.Handler {
	return &sdkLogGuard{next: g.next.WithGroup(name), state: g.state}
}

// Passes a record on unless an identical one was emitted inside the window, the first record after it naming the suppressed count
func (g *sdkLogGuard) Handle(ctx context.Context, record slog.Record) error {
	if g.filter(&record) {
		return g.next.Handle(ctx, record)
	}
	return nil
}

// Whether the record is emitted, never failing the caller
func (g *sdkLogGuard) filter(record *slog.Record) (emit bool) {
	defer func() {
		if recover() != nil {
			emit = true
		}
	}()
	msg := record.Message
	var exc error
	record.Attrs(func(attr slog.Attr) bool {
		if err, ok := attr.Value.Any().(error); ok && attr.Key == "error" {
			exc = err
			return false
		}
		return true
	})
	st := g.state
	if exc != nil && st.onException != nil {
		st.onException(exc)
	}
	key := sdkLogKey{level: record.Level, msg: pystr.Slice(msg, 0, 500)}
	if exc != nil {
		key.exc = pyrepr.ExceptionName(exc)
	}
	now := time.Since(st.started).Seconds()
	st.mu.Lock()
	defer st.mu.Unlock()
	entry := st.seen[key]
	if entry != nil && now-entry[0] < st.window {
		entry[1]++
		return false
	}
	if entry != nil && entry[1] != 0 {
		record.Message = fmt.Sprintf("%s (suppressed %d identical repeats)", msg, int(entry[1]))
	}
	if len(st.seen) >= 256 {
		clear(st.seen)
	}
	st.seen[key] = &[2]float64{now, 0}
	return true
}

// Stripped non blank items of a list, a JSON list literal or a comma separated value
func csvSet(raw any) map[string]bool {
	raw = gateway.DecodeJSONListLiteral(raw)
	parts, ok := raw.([]any)
	if !ok {
		for _, part := range strings.Split(pyrepr.Str(raw), ",") {
			parts = append(parts, part)
		}
	}
	out := map[string]bool{}
	for _, part := range parts {
		if s := pystr.Strip(pyrepr.Str(part)); s != "" {
			out[s] = true
		}
	}
	return out
}

// The stream and HTTP clients are compiled in, a passive probe that never installs
func DingTalkDepsPresent() bool { return true }

// The stream and HTTP clients are compiled in, credentials gated separately
func EnsureDingTalkDeps(ctx context.Context) (bool, error) { return true, nil }

// Client id and secret from the extra first, then the scoped env
func credentials(ctx context.Context, extra map[string]any) (any, any, error) {
	clientID, clientSecret := extra["client_id"], extra["client_secret"]
	if !pyrepr.Truthy(clientID) {
		scoped, err := gateway.GetScopedSecret(ctx, "DINGTALK_CLIENT_ID", "", false)
		if err != nil {
			return nil, nil, err
		}
		clientID = scoped
	}
	if !pyrepr.Truthy(clientSecret) {
		scoped, err := gateway.GetScopedSecret(ctx, "DINGTALK_CLIENT_SECRET", "", false)
		if err != nil {
			return nil, nil, err
		}
		clientSecret = scoped
	}
	return clientID, clientSecret, nil
}

// Whether the clients are available and the scoped env carries both credentials
func CheckDingTalkRequirements(ctx context.Context) bool {
	ok, err := EnsureDingTalkDeps(ctx)
	if err != nil || !ok {
		return false
	}
	clientID, clientSecret, err := credentials(ctx, nil)
	return err == nil && pyrepr.Truthy(clientID) && pyrepr.Truthy(clientSecret)
}

// Session webhook of a chat with its expiry in milliseconds
type sessionWebhook struct {
	url       string
	expiredMS any
}

// Stream Mode adapter, messages in through the gateway socket and replies out through the session webhook or AI cards
type DingTalkAdapter struct {
	*gateway.BasePlatformAdapter

	clientID        any
	clientSecret    any
	mentionPatterns []*regexp2.Regexp
	allowedUsers    map[string]bool
	robotCode       any
	cardTemplateID  any
	dedup           *gateway.MessageDeduplicator

	mu              sync.Mutex
	streamClient    *streamClient
	streamTask      *gateway.AdapterTask
	sdkLoopTask     *gateway.AdapterTask
	httpClient      *http.Client
	cardSDK         *openAPIClient
	robotSDK        *openAPIClient
	sessionWebhooks *pydict.Dict[string, sessionWebhook]
	messageContexts map[string]*IncomingMessage
	doneEmojiFired  map[string]bool
	streamingCards  map[string]*pydict.Dict[string, string]
	bgTasks         map[*gateway.AdapterTask]struct{}
}

// Class attributes the DingTalk adapter declares over the base defaults
func dingtalkAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.MaxMessageLength = maxMessageLength
	return traits
}

// Adapter for a platform config reading credentials, group gating and the AI card template from extra then env
func NewDingTalkAdapter(ctx context.Context, config *gateway.PlatformConfig) (*DingTalkAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &DingTalkAdapter{dedup: gateway.NewMessageDeduplicator(1000, gateway.DefaultDedupTTLSeconds),
		sessionWebhooks: pydict.New[string, sessionWebhook](), messageContexts: map[string]*IncomingMessage{},
		doneEmojiFired: map[string]bool{}, streamingCards: map[string]*pydict.Dict[string, string]{},
		bgTasks: map[*gateway.AdapterTask]struct{}{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformDingTalk, a)
	a.Traits = dingtalkAdapterTraits()
	a.Traits.SupportsMessageEditing, a.Traits.RequiresEditFinalize = false, false
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	var err error
	if a.clientID, a.clientSecret, err = credentials(ctx, extra); err != nil {
		return nil, err
	}
	if a.mentionPatterns, err = a.compileMentionPatterns(ctx); err != nil {
		return nil, err
	}
	allowed, err := a.csvSetting(ctx, "allowed_users", "DINGTALK_ALLOWED_USERS")
	if err != nil {
		return nil, err
	}
	a.allowedUsers = map[string]bool{}
	for item := range allowed {
		a.allowedUsers[pystr.Lower(item)] = true
	}
	a.robotCode = extra["robot_code"]
	if !pyrepr.Truthy(a.robotCode) {
		a.robotCode = a.clientID
	}
	a.cardTemplateID = extra["card_template_id"]
	return a, nil
}

// Whether AI cards are configured, the only way a sent message can be edited
func (a *DingTalkAdapter) supportsMessageEditing() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return pyrepr.Truthy(a.cardTemplateID) && a.cardSDK != nil
}

// Connects through Stream Mode, starting the stream task and the card and robot clients
func (a *DingTalkAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if !pyrepr.Truthy(a.clientID) || !pyrepr.Truthy(a.clientSecret) {
		slog.Warn(fmt.Sprintf("[%s] DINGTALK_CLIENT_ID and DINGTALK_CLIENT_SECRET required", a.Name()))
		return false, nil
	}
	client := newStreamClient(a.clientID, a.clientSecret)
	a.mu.Lock()
	a.httpClient = gateway.LimitedHTTPClient(gateway.PlatformHTTPXLimits(), false)
	a.streamClient = client
	if pyrepr.Truthy(a.cardTemplateID) {
		a.cardSDK = newOpenAPIClient()
	}
	a.robotSDK = newOpenAPIClient()
	a.mu.Unlock()
	if pyrepr.Truthy(a.cardTemplateID) {
		slog.Info(fmt.Sprintf("[%s] Card SDK initialized with template: %s", a.Name(), pyrepr.Str(a.cardTemplateID)))
	} else {
		slog.Info(fmt.Sprintf("[%s] Robot SDK initialized (media download)", a.Name()))
	}
	editing := a.supportsMessageEditing()
	a.Traits.SupportsMessageEditing, a.Traits.RequiresEditFinalize = editing, editing
	client.registerCallbackHandler(chatbotTopic, &incomingHandler{adapter: a})
	a.MarkConnected(ctx, "")
	task := gateway.SpawnAdapterTask(ctx, a.runStream)
	a.mu.Lock()
	a.streamTask = task
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[%s] Connected via Stream Mode", a.Name()))
	a.WirePluginHandlers(ctx, client)
	return true, nil
}

// Runs the stream loop with the log guard installed on the stream client logger
func (a *DingTalkAdapter) runStream(ctx context.Context) error {
	a.mu.Lock()
	a.sdkLoopTask = gateway.CurrentAdapterTask(ctx)
	client := a.streamClient
	a.mu.Unlock()
	if client == nil {
		return nil
	}
	original := client.log()
	guard := newSDKLogGuard(original.Handler(), func(exc error) { a.onSDKError(ctx, exc, true) }, sdkLogRepeatWindow)
	client.swapLogger(slog.New(guard))
	defer client.swapLogger(original)
	return a.runStreamLoop(ctx, client)
}

// Restarts the stream client with backoff, a run of identical errors tripping a quiet circuit breaker
func (a *DingTalkAdapter) runStreamLoop(ctx context.Context, client *streamClient) error {
	backoffIdx, consecutiveSameError := 0, 0
	lastErrorType := ""
	for a.IsConnected() {
		slog.Debug(fmt.Sprintf("[%s] Starting stream client...", a.Name()))
		err := client.start(ctx)
		if ctx.Err() != nil {
			if code := a.FatalErrorCode(); code != nil && *code == "dingtalk_stream_error" {
				return a.NotifyFatalError(context.WithoutCancel(ctx))
			}
			return nil
		}
		if err != nil {
			if !a.IsConnected() {
				return nil
			}
			if isSDKIncompat(err) {
				a.onSDKError(ctx, err, false)
				return a.NotifyFatalError(ctx)
			}
			errorType := pyrepr.ExceptionName(err)
			if errorType == lastErrorType {
				consecutiveSameError++
			} else {
				consecutiveSameError, lastErrorType = 1, errorType
			}
			if consecutiveSameError <= reconnectCircuitBreakerTrips {
				slog.Warn(fmt.Sprintf("[%s] Stream client error: %s", a.Name(), pyrepr.ExceptionMessage(err)))
			} else if consecutiveSameError == reconnectCircuitBreakerTrips+1 {
				slog.Error(fmt.Sprintf("[%s] Stream client error repeated %d times: %s — circuit breaker tripped; "+
					"retrying every %ds silently until the error changes.", a.Name(), consecutiveSameError, pyrepr.ExceptionMessage(err),
					reconnectCircuitBreakerDelay))
			}
		}
		if !a.IsConnected() {
			return nil
		}
		if consecutiveSameError > reconnectCircuitBreakerTrips {
			if err := sleepSeconds(ctx, reconnectCircuitBreakerDelay); err != nil {
				return err
			}
		} else {
			delay := reconnectBackoff[min(backoffIdx, len(reconnectBackoff)-1)]
			slog.Info(fmt.Sprintf("[%s] Reconnecting in %ds...", a.Name(), delay))
			if err := sleepSeconds(ctx, float64(delay)); err != nil {
				return err
			}
			backoffIdx++
		}
	}
	return nil
}

// Hands a websockets incompatibility of the stream client to the gateway as a fatal error and stops the stream loop
func (a *DingTalkAdapter) onSDKError(ctx context.Context, exc error, cancel bool) {
	if !isSDKIncompat(exc) {
		return
	}
	if code := a.FatalErrorCode(); code != nil && *code == "dingtalk_stream_error" {
		return
	}
	msg := fmt.Sprintf("dingtalk-stream cannot open its websocket with the installed websockets package (%s). "+
		"Icebath pins dingtalk-stream==0.24.3 with websockets==15.0.1; reinstall the dingtalk extra "+
		"so those versions are used (e.g. `pip install 'icebath-agent[dingtalk]'`).", pyrepr.ExceptionMessage(exc))
	slog.Error(fmt.Sprintf("[%s] %s", a.Name(), msg))
	a.SetFatalError(ctx, "dingtalk_stream_error", msg, false)
	a.mu.Lock()
	task := a.sdkLoopTask
	a.mu.Unlock()
	if cancel && task != nil && !task.Done() {
		task.Cancel()
	}
}

// Runs fn and swallows its failure, logging it at debug under the format when one is given
func (a *DingTalkAdapter) quiet(fn func() error, debugFmt string, args ...any) {
	if err := fn(); err != nil && debugFmt != "" {
		slog.Debug(fmt.Sprintf(debugFmt, append(append([]any{a.Name()}, args...), pyrepr.ExceptionMessage(err))...))
	}
}

// Stops the stream, cancels background work, finalizes open cards and clears every per chat store
func (a *DingTalkAdapter) Disconnect(ctx context.Context) error {
	a.SetRunning(false)
	a.MarkDisconnected(ctx)
	a.mu.Lock()
	client, streamTask := a.streamClient, a.streamTask
	a.mu.Unlock()
	if client != nil {
		if ws := client.currentWebsocket(); ws != nil {
			ws.Close()
		}
	}
	if streamTask != nil {
		streamTask.Cancel()
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := streamTask.Wait(wctx); err != nil {
			slog.Debug(fmt.Sprintf("[%s] stream task did not exit cleanly during disconnect", a.Name()))
		}
		cancel()
		a.mu.Lock()
		a.streamTask = nil
		a.mu.Unlock()
	}
	a.mu.Lock()
	tasks := make([]*gateway.AdapterTask, 0, len(a.bgTasks))
	for task := range a.bgTasks {
		tasks = append(tasks, task)
	}
	a.mu.Unlock()
	for _, task := range tasks {
		task.Cancel()
	}
	for _, task := range tasks {
		_ = task.Wait(ctx)
	}
	a.mu.Lock()
	chatIDs := make([]string, 0, len(a.streamingCards))
	for chatID := range a.streamingCards {
		chatIDs = append(chatIDs, chatID)
	}
	a.mu.Unlock()
	for _, chatID := range chatIDs {
		a.quiet(func() error { return a.closeStreamingSiblings(ctx, chatID) },
			"[%s] Failed to finalize streaming card on disconnect for %s: %s", chatID)
	}
	a.mu.Lock()
	httpClient := a.httpClient
	a.httpClient, a.streamClient = nil, nil
	a.sessionWebhooks.Clear()
	clear(a.messageContexts)
	clear(a.streamingCards)
	clear(a.doneEmojiFired)
	clear(a.bgTasks)
	a.mu.Unlock()
	if httpClient != nil {
		httpClient.CloseIdleConnections()
	}
	a.dedup.Clear()
	slog.Info(fmt.Sprintf("[%s] Disconnected", a.Name()))
	return nil
}

// List or CSV setting from the env first, then the config extra key
func (a *DingTalkAdapter) csvSetting(ctx context.Context, key, envName string) (map[string]bool, error) {
	raw, err := gateway.ExtraOrSecret(ctx, a.Config.Extra, key, envName, "", false)
	if err != nil {
		return nil, err
	}
	return csvSet(raw), nil
}

// Whether group chats need an explicit bot trigger
func (a *DingTalkAdapter) requireMention(ctx context.Context) (bool, error) {
	configured, err := gateway.ExtraOrSecret(ctx, a.Config.Extra, "require_mention", "DINGTALK_REQUIRE_MENTION", "false", false)
	if err != nil {
		return false, err
	}
	if s, ok := configured.(string); ok {
		return truthyValues[pystr.Lower(s)], nil
	}
	return pyrepr.Truthy(configured), nil
}

// Group chat whitelist, a non empty one a hard gate even for a mention
func (a *DingTalkAdapter) allowedChats(ctx context.Context) (map[string]bool, error) {
	return a.csvSetting(ctx, "allowed_chats", "DINGTALK_ALLOWED_CHATS")
}

// Wake word patterns from the config list, or from the env as JSON, lines or CSV
func (a *DingTalkAdapter) compileMentionPatterns(ctx context.Context) ([]*regexp2.Regexp, error) {
	var patterns any
	if a.Config.Extra != nil {
		patterns = a.Config.Extra["mention_patterns"]
	}
	if patterns == nil {
		scoped, err := gateway.GetScopedSecret(ctx, "DINGTALK_MENTION_PATTERNS", "", false)
		if err != nil {
			return nil, err
		}
		if raw := pystr.Strip(scoped); raw != "" {
			if parsed, err := pyjson.Loads(raw); err == nil {
				patterns = pyjson.Plain(parsed)
			} else {
				parts := []any{}
				for _, part := range pystr.Splitlines(raw, false) {
					if s := pystr.Strip(part); s != "" {
						parts = append(parts, s)
					}
				}
				if len(parts) == 0 {
					for _, part := range strings.Split(raw, ",") {
						if s := pystr.Strip(part); s != "" {
							parts = append(parts, s)
						}
					}
				}
				patterns = parts
			}
		}
	}
	if patterns == nil {
		return nil, nil
	}
	label := "dingtalk"
	return gateway.CompileMentionPatterns(patterns, gateway.MentionPatternOptions{LogPrefix: a.Name(), PlatformLabel: &label,
		DisplayLabel: "DingTalk"}), nil
}

// Lowercased text of a sender id, the AttributeError lower raises on a non string id
func lowerID(v any) (string, error) {
	if !pyrepr.Truthy(v) {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", attributeError(v, "lower")
	}
	return pystr.Lower(s), nil
}

// Whether the sender or their staff id is on the allowlist, an empty or starred allowlist allowing everyone
func (a *DingTalkAdapter) isUserAllowed(senderID, senderStaffID any) (bool, error) {
	if len(a.allowedUsers) == 0 || a.allowedUsers["*"] {
		return true, nil
	}
	for _, raw := range []any{senderID, senderStaffID} {
		id, err := lowerID(raw)
		if err != nil {
			return false, err
		}
		if id != "" && a.allowedUsers[id] {
			return true, nil
		}
	}
	return false, nil
}

// Whether the text hits a wake word pattern
func (a *DingTalkAdapter) messageMatchesMentionPatterns(text string) (bool, error) {
	if text == "" {
		return false, nil
	}
	for _, pattern := range a.mentionPatterns {
		m, err := pattern.FindStringMatch(text)
		if err != nil {
			return false, err
		}
		if m != nil {
			return true, nil
		}
	}
	return false, nil
}

// Group trigger rules, the chat whitelist a hard gate before free response, mention off, at mention or wake word
func (a *DingTalkAdapter) shouldProcessMessage(ctx context.Context, message *IncomingMessage, text string, isGroup bool, chatID string) (bool, error) {
	if !isGroup {
		return true, nil
	}
	allowed, err := a.allowedChats(ctx)
	if err != nil {
		return false, err
	}
	if len(allowed) > 0 && chatID != "" && !allowed[chatID] {
		return false, nil
	}
	free, err := a.csvSetting(ctx, "free_response_chats", "DINGTALK_FREE_RESPONSE_CHATS")
	if err != nil {
		return false, err
	}
	if chatID != "" && free[chatID] {
		return true, nil
	}
	required, err := a.requireMention(ctx)
	if err != nil {
		return false, err
	}
	if !required || pyrepr.Truthy(message.IsInAtList) {
		return true, nil
	}
	return a.messageMatchesMentionPatterns(text)
}

// Starts fire and forget work tracked for cancellation at disconnect
func (a *DingTalkAdapter) spawnBG(ctx context.Context, fn func(ctx context.Context) error) {
	task := gateway.SpawnAdapterTask(ctx, fn)
	a.mu.Lock()
	a.bgTasks[task] = struct{}{}
	a.mu.Unlock()
	task.AddDoneCallback(func() {
		a.mu.Lock()
		delete(a.bgTasks, task)
		a.mu.Unlock()
	})
}

// Finalizes the open streaming cards of a chat, each failure logged at debug
func (a *DingTalkAdapter) closeStreamingSiblings(ctx context.Context, chatID string) error {
	a.mu.Lock()
	cards := a.streamingCards[chatID]
	delete(a.streamingCards, chatID)
	a.mu.Unlock()
	if cards == nil || cards.Len() == 0 {
		return nil
	}
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return err
	}
	if !pyrepr.Truthy(token) {
		return nil
	}
	for outTrackID, lastContent := range cards.All() {
		if err := a.streamCardContent(ctx, outTrackID, token, lastContent, true); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Debug(fmt.Sprintf("[%s] Sibling close failed for %s: %s", a.Name(), outTrackID, pyrepr.ExceptionMessage(err)))
			continue
		}
		slog.Debug(fmt.Sprintf("[%s] AI Card sibling closed: %s", a.Name(), outTrackID))
	}
	return nil
}

// Swaps the thinking reaction for the done reaction on the chat's inbound message, once per turn
func (a *DingTalkAdapter) fireDoneReaction(ctx context.Context, chatID string) {
	a.mu.Lock()
	if a.doneEmojiFired[chatID] {
		a.mu.Unlock()
		return
	}
	a.doneEmojiFired[chatID] = true
	msg := a.messageContexts[chatID]
	a.mu.Unlock()
	if msg == nil {
		return
	}
	msgID, conversationID := orEmpty(msg.MessageID), orEmpty(msg.ConversationID)
	if !pyrepr.Truthy(msgID) || !pyrepr.Truthy(conversationID) {
		return
	}
	thinking := i18n.T(ctx, "platform.dingtalk.emotion.thinking", "", nil)
	done := i18n.T(ctx, "platform.dingtalk.emotion.done", "", nil)
	a.spawnBG(ctx, func(tctx context.Context) error {
		if err := a.sendEmotion(tctx, pyrepr.Str(msgID), pyrepr.Str(conversationID), thinking, true); err != nil {
			return err
		}
		return a.sendEmotion(tctx, pyrepr.Str(msgID), pyrepr.Str(conversationID), done, false)
	})
}

// Turns one inbound chatbot message into a message event after dedup, allowlist and group trigger gating
func (a *DingTalkAdapter) onMessage(ctx context.Context, message *IncomingMessage) error {
	msgID := pyrepr.Str(message.MessageID)
	if !pyrepr.Truthy(message.MessageID) {
		msgID = uuidHex()
	}
	if a.dedup.IsDuplicate(msgID) {
		slog.Debug(fmt.Sprintf("[%s] Duplicate message %s, skipping", a.Name(), msgID))
		return nil
	}
	conversationID, senderID := orEmpty(message.ConversationID), orEmpty(message.SenderID)
	senderNick, senderStaffID := orEmpty(message.SenderNick), orEmpty(message.SenderStaffID)
	isGroup := pyrepr.Str(message.ConversationType) == "2"
	if !pyrepr.Truthy(senderNick) {
		senderNick = senderID
	}
	chatIDValue := conversationID
	if !pyrepr.Truthy(chatIDValue) {
		chatIDValue = senderID
	}
	chatID := pyrepr.Str(chatIDValue)
	allowed, err := a.isUserAllowed(senderID, senderStaffID)
	if err != nil {
		return err
	}
	if !allowed {
		slog.Debug(fmt.Sprintf("[%s] Dropping message from non-allowlisted user staff_id=%s sender_id=%s", a.Name(),
			pyrepr.Str(senderStaffID), pyrepr.Str(senderID)))
		return nil
	}
	early, err := ExtractText(&message.ChatbotMessage)
	if err != nil {
		return err
	}
	process, err := a.shouldProcessMessage(ctx, message, early, isGroup, chatID)
	if err != nil {
		return err
	}
	if !process {
		slog.Debug(fmt.Sprintf("[%s] Dropping group message that failed mention gate message_id=%s chat_id=%s", a.Name(), msgID, chatID))
		return nil
	}
	if chatID != "" {
		a.mu.Lock()
		a.messageContexts[chatID] = message
		delete(a.doneEmojiFired, chatID)
		a.mu.Unlock()
	}
	if webhook := orEmpty(message.SessionWebhook); pyrepr.Truthy(webhook) && chatID != "" {
		webhookText, ok := webhook.(string)
		if !ok {
			return &gateway.PyError{Kind: "TypeError", Msg: fmt.Sprintf("expected string or bytes-like object, got '%s'", pyrepr.TypeName(webhook))}
		}
		matched, err := dingtalkWebhookRE.MatchString(webhookText)
		if err != nil {
			return err
		}
		if matched {
			var expired any = message.SessionWebhookExpiredTime
			if !pyrepr.Truthy(expired) {
				expired = 0
			}
			a.mu.Lock()
			if a.sessionWebhooks.Len() >= sessionWebhooksMax {
				a.sessionWebhooks.Delete(a.sessionWebhooks.Keys()[0])
			}
			a.sessionWebhooks.Set(chatID, sessionWebhook{url: webhookText, expiredMS: expired})
			a.mu.Unlock()
		}
	}
	if err := a.resolveMediaCodes(ctx, message); err != nil {
		return err
	}
	text, err := a.extractText(message)
	if err != nil {
		return err
	}
	msgType, mediaURLs, mediaTypes, err := a.extractMedia(message)
	if err != nil {
		return err
	}
	if text == "" && len(mediaURLs) == 0 {
		slog.Debug(fmt.Sprintf("[%s] Empty message, skipping", a.Name()))
		return nil
	}
	fields := gateway.SourceFields{ChatType: "dm"}
	if isGroup {
		fields.ChatType = "group"
	}
	if message.ConversationTitle != nil {
		title := pyrepr.Str(message.ConversationTitle)
		fields.ChatName = &title
	}
	userID, userName := pyrepr.Str(senderID), pyrepr.Str(senderNick)
	fields.UserID, fields.UserName, fields.MessageID = &userID, &userName, &msgID
	if pyrepr.Truthy(senderStaffID) {
		staff := pyrepr.Str(senderStaffID)
		fields.UserIDAlt = &staff
	}
	source := a.BuildSource(ctx, chatID, fields)
	timestamp := time.Now().UTC()
	if pyrepr.Truthy(message.CreateAt) {
		if millis, err := pyint.Int64(message.CreateAt); err == nil {
			if at := time.UnixMilli(millis).UTC(); at.Year() >= 1 && at.Year() <= 9999 {
				timestamp = at
			}
		} else if pe, ok := err.(*pyint.Error); !ok || (pe.Kind != "ValueError" && pe.Kind != "TypeError") {
			return err
		}
	}
	where, preview := "?", "(media)"
	if chatID != "" {
		where = pystr.Slice(chatID, 0, 20)
	}
	if text != "" {
		preview = pystr.Slice(text, 0, 80)
	}
	slog.Debug(fmt.Sprintf("[%s] Message from %s in %s: %s", a.Name(), userName, where, preview))
	event := gateway.NewMessageEvent(text)
	event.MessageType, event.Source, event.RawMessage = msgType, source, message
	event.MessageID = &msgID
	event.MediaURLs, event.MediaTypes, event.Timestamp = mediaURLs, mediaTypes, timestamp
	return a.HandleMessage(ctx, event)
}

// Plain text of a message from either SDK shape
func (a *DingTalkAdapter) extractText(message *IncomingMessage) (string, error) {
	return ExtractText(&message.ChatbotMessage)
}

// Message type, media references and mime types of a message
func (a *DingTalkAdapter) extractMedia(message *IncomingMessage) (gateway.MessageType, []string, []string, error) {
	return ExtractMedia(&message.ChatbotMessage)
}

// Sends a reply through an AI card when configured, otherwise as markdown through the session webhook
func (a *DingTalkAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	cardSDK, httpClient := a.cardSDK, a.httpClient
	currentMessage := a.messageContexts[chatID]
	a.mu.Unlock()
	cardEnabled := pyrepr.Truthy(a.cardTemplateID) && cardSDK != nil
	slog.Debug(fmt.Sprintf("[%s] send() chat_id=%s card_enabled=%s", a.Name(), chatID, pyrepr.Str(cardEnabled)))
	webhookValue := metadata["session_webhook"]
	if !pyrepr.Truthy(webhookValue) {
		webhookValue = ""
		if info := a.getValidWebhook(chatID); info != nil {
			webhookValue = info.url
		}
	}
	if !pyrepr.Truthy(webhookValue) {
		slog.Warn(fmt.Sprintf("[%s] No valid session_webhook for chat_id=%s", a.Name(), chatID))
		return gateway.SendResult{Success: false, Error: "No valid session_webhook available. Reply must follow an incoming message."}, nil
	}
	if httpClient == nil {
		return gateway.SendResult{Success: false, Error: "HTTP client not initialized"}, nil
	}
	isFinalReply := replyTo != ""
	if cardEnabled && currentMessage != nil {
		if err := a.closeStreamingSiblings(ctx, chatID); err != nil {
			return gateway.SendResult{}, err
		}
		result, err := a.createAndStreamCard(ctx, chatID, currentMessage, content, isFinalReply)
		if err != nil {
			return gateway.SendResult{}, err
		}
		if result != nil && result.Success {
			if isFinalReply {
				a.fireDoneReaction(ctx, chatID)
			} else {
				a.mu.Lock()
				cards := a.streamingCards[chatID]
				if cards == nil {
					cards = pydict.New[string, string]()
					a.streamingCards[chatID] = cards
				}
				cards.Set(pyrepr.Str(result.MessageID), content)
				a.mu.Unlock()
			}
			return *result, nil
		}
		slog.Warn(fmt.Sprintf("[%s] AI Card send failed, falling back to webhook", a.Name()))
	}
	slog.Debug(fmt.Sprintf("[%s] Sending via webhook", a.Name()))
	payload := pyjson.ObjectOf("msgtype", "markdown", "markdown", pyjson.ObjectOf("title", "Icebath",
		"text", normalizeMarkdown(pystr.Slice(content, 0, maxMessageLength))))
	resp, err := postJSON(ctx, httpClient, pyrepr.Str(webhookValue), payload, 15*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		var timeout *pyhttpx.TimeoutError
		if errors.As(err, &timeout) {
			return gateway.SendResult{Success: false, Error: "Timeout sending message to DingTalk"}, nil
		}
		text := pyhttpx.ErrorText(err)
		slog.Error(fmt.Sprintf("[%s] Send error: %s", a.Name(), text))
		return gateway.SendResult{Success: false, Error: text}, nil
	}
	if resp.StatusCode < 300 {
		if isFinalReply {
			a.fireDoneReaction(ctx, chatID)
		}
		return gateway.SendResult{Success: true, MessageID: uuidHex()[:12]}, nil
	}
	body := pystr.Slice(resp.Text(), 0, 200)
	slog.Warn(fmt.Sprintf("[%s] Send failed HTTP %d: %s", a.Name(), resp.StatusCode, body))
	return gateway.SendResult{Success: false, Error: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, body)}, nil
}

// One JSON POST on an httpx style client with a per phase timeout
func postJSON(ctx context.Context, client *http.Client, target string, payload *pyjson.Object, timeout time.Duration) (*pyhttpx.Response, error) {
	encoded, err := pyjson.Dumps(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader([]byte(encoded)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return pyhttpx.Do(client, req, timeout)
}

// DingTalk has no typing indicator
func (a *DingTalkAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return nil
}

// Renders a remote image inline through markdown, the session webhook having no native attachments
func (a *DingTalkAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	content := "![image](" + imageURL + ")"
	if caption != "" {
		content = caption + "\n\n" + content
	}
	return a.Send(ctx, chatID, content, replyTo, metadata)
}

// Webhook replies cannot upload local images
func (a *DingTalkAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: false, Error: fmt.Sprintf(noLocalUploadFormat, "image uploads", "media upload")}, nil
}

// Webhook replies cannot upload local files
func (a *DingTalkAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: false, Error: fmt.Sprintf(noLocalUploadFormat, "file attachments", "message send")}, nil
}

// Name and chat type of a conversation id
func (a *DingTalkAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	chatType := "dm"
	if strings.Contains(pystr.Lower(chatID), "group") {
		chatType = "group"
	}
	return map[string]any{"name": chatID, "type": chatType}, nil
}

// Session webhook of a chat that stays valid for five more minutes, an expiring one forgotten
func (a *DingTalkAdapter) getValidWebhook(chatID string) *sessionWebhook {
	a.mu.Lock()
	defer a.mu.Unlock()
	info, ok := a.sessionWebhooks.Get(chatID)
	if !ok {
		return nil
	}
	if expired, isNumber := pyrepr.Int(info.expiredMS); isNumber && expired.Sign() > 0 &&
		expired.IsInt64() && time.Now().UnixMilli()+5*60*1000 >= expired.Int64() {
		a.sessionWebhooks.Delete(chatID)
		return nil
	}
	return &info
}

// Creates, delivers and streams an AI card, nil when no token, no deliver target or any call fails
func (a *DingTalkAdapter) createAndStreamCard(ctx context.Context, chatID string, message *IncomingMessage, content string, finalize bool) (*gateway.SendResult, error) {
	result, err := func() (*gateway.SendResult, error) {
		token, err := a.getAccessToken(ctx)
		if err != nil || !pyrepr.Truthy(token) {
			return nil, err
		}
		a.mu.Lock()
		cardSDK := a.cardSDK
		a.mu.Unlock()
		outTrackID := "icebath_" + uuidHex()[:12]
		isGroup := pyrepr.Str(message.ConversationType) == "2"
		senderStaffID := orEmpty(message.SenderStaffID)
		if err := cardSDK.createCard(ctx, token, a.cardTemplateID, outTrackID); err != nil {
			return nil, err
		}
		var openSpaceID, deliverKey string
		var deliverModel *pyjson.Object
		switch {
		case isGroup:
			openSpaceID = "dtv1.card//IM_GROUP." + pyrepr.Str(orEmpty(message.ConversationID))
			deliverKey, deliverModel = "imGroupOpenDeliverModel", pyjson.ObjectOf("robotCode", a.robotCode)
		case pyrepr.Truthy(senderStaffID):
			openSpaceID = "dtv1.card//IM_ROBOT." + pyrepr.Str(senderStaffID)
			deliverKey, deliverModel = "imRobotOpenDeliverModel", pyjson.ObjectOf("spaceType", "IM_ROBOT")
		default:
			slog.Warn(fmt.Sprintf("[%s] AI Card skipped: missing sender_staff_id for DM", a.Name()))
			return nil, nil
		}
		if err := cardSDK.deliverCard(ctx, token, outTrackID, openSpaceID, deliverKey, deliverModel); err != nil {
			return nil, err
		}
		if err := a.streamCardContent(ctx, outTrackID, token, content, finalize); err != nil {
			return nil, err
		}
		state := "created (streaming)"
		if finalize {
			state = "created+finalized"
		}
		slog.Info(fmt.Sprintf("[%s] AI Card %s: %s", a.Name(), state, outTrackID))
		return &gateway.SendResult{Success: true, MessageID: outTrackID}, nil
	}()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[%s] AI Card create failed: %s\n%s", a.Name(), pyrepr.ExceptionMessage(err), debug.Stack()))
		return nil, nil
	}
	return result, nil
}

// Streams updated content to the AI card a send created, a final edit closing it and firing the done reaction
func (a *DingTalkAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	if !a.supportsMessageEditing() {
		return gateway.SendResult{Success: false, Error: "AI Cards are not configured for message editing"}, nil
	}
	var token any
	if messageID != "" {
		var err error
		if token, err = a.getAccessToken(ctx); err != nil {
			return gateway.SendResult{}, err
		}
	}
	if !pyrepr.Truthy(token) {
		if messageID == "" {
			return gateway.SendResult{Success: false, Error: "message_id required"}, nil
		}
		return gateway.SendResult{Success: false, Error: "No access token"}, nil
	}
	if err := a.streamCardContent(ctx, messageID, token, content, finalize); err != nil {
		if ctx.Err() != nil {
			return gateway.SendResult{}, ctx.Err()
		}
		slog.Warn(fmt.Sprintf("[%s] Card edit failed: %s", a.Name(), pyrepr.ExceptionMessage(err)))
		return gateway.SendResult{Success: false, Error: pyrepr.ExceptionMessage(err)}, nil
	}
	if finalize {
		a.mu.Lock()
		if cards := a.streamingCards[chatID]; cards != nil {
			cards.Delete(messageID)
			if cards.Len() == 0 {
				delete(a.streamingCards, chatID)
			}
		}
		a.mu.Unlock()
		slog.Debug(fmt.Sprintf("[%s] AI Card finalized (edit): %s", a.Name(), messageID))
		a.fireDoneReaction(ctx, chatID)
	} else {
		a.mu.Lock()
		cards := a.streamingCards[chatID]
		if cards == nil {
			cards = pydict.New[string, string]()
			a.streamingCards[chatID] = cards
		}
		cards.Set(messageID, content)
		a.mu.Unlock()
	}
	return gateway.SendResult{Success: true, MessageID: messageID}, nil
}

// Streams content to an existing AI card
func (a *DingTalkAdapter) streamCardContent(ctx context.Context, outTrackID string, token any, content string, finalize bool) error {
	a.mu.Lock()
	cardSDK := a.cardSDK
	a.mu.Unlock()
	if cardSDK == nil {
		return attributeError(nil, "streaming_update_with_options_async")
	}
	return cardSDK.streamingUpdate(ctx, token, outTrackID, uuid.NewString(), pystr.Slice(content, 0, maxMessageLength), finalize)
}

// App access token through the stream client's cached getter, nil when it fails or the client is gone
func (a *DingTalkAdapter) getAccessToken(ctx context.Context) (any, error) {
	a.mu.Lock()
	client := a.streamClient
	a.mu.Unlock()
	if client == nil {
		return nil, nil
	}
	token, err := client.getAccessToken(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Error(fmt.Sprintf("[%s] Failed to get access token: %s", a.Name(), pyrepr.ExceptionMessage(err)))
		return nil, nil
	}
	return token, nil
}

// Adds or recalls an emoji reaction on a message, every failure logged at debug
func (a *DingTalkAdapter) sendEmotion(ctx context.Context, openMsgID, openConversationID, emojiName string, recall bool) error {
	a.mu.Lock()
	robotSDK := a.robotSDK
	a.mu.Unlock()
	if robotSDK == nil || openMsgID == "" || openConversationID == "" {
		return nil
	}
	action := "reply"
	if recall {
		action = "recall"
	}
	token, err := a.getAccessToken(ctx)
	if err == nil && pyrepr.Truthy(token) {
		err = robotSDK.robotEmotion(ctx, token, recall, a.robotCode, openMsgID, openConversationID, emojiName)
		if err == nil {
			slog.Info(fmt.Sprintf("[%s] _send_emotion: %s %s on msg=%s", a.Name(), action, emojiName, pystr.Slice(openMsgID, 0, 24)))
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Debug(fmt.Sprintf("[%s] _send_emotion %s failed", a.Name(), action), "error", err)
	}
	return nil
}

// Resolves every download code of a message to a real URL in place, all codes in parallel
func (a *DingTalkAdapter) resolveMediaCodes(ctx context.Context, message *IncomingMessage) error {
	token, err := a.getAccessToken(ctx)
	if err != nil {
		return err
	}
	if !pyrepr.Truthy(token) {
		return nil
	}
	robotCode := message.RobotCode
	if !pyrepr.Truthy(robotCode) {
		robotCode = a.clientID
	}
	var wg sync.WaitGroup
	for _, ref := range CollectDownloadCodes(&message.ChatbotMessage) {
		var code any
		switch container := ref.Container.(type) {
		case *ImageContent:
			code = container.DownloadCode
		case map[string]any:
			code = container[ref.Key]
		}
		if !pyrepr.Truthy(code) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.fetchDownloadURL(ctx, code, robotCode, token, ref)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// Fetches the download URL of one code and writes it back over the code
func (a *DingTalkAdapter) fetchDownloadURL(ctx context.Context, code, robotCode, token any, ref DownloadCodeRef) {
	a.mu.Lock()
	robotSDK := a.robotSDK
	a.mu.Unlock()
	if robotSDK == nil {
		slog.Warn(fmt.Sprintf("[%s] Robot SDK not initialized, cannot resolve media code", a.Name()))
		return
	}
	body, err := robotSDK.robotMessageFileDownload(ctx, token, code, robotCode)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Error resolving media code %s: %s", a.Name(), pyrepr.Str(code), pyrepr.ExceptionMessage(err)))
		return
	}
	if body == nil || body.Len() == 0 {
		slog.Warn(fmt.Sprintf("[%s] Failed to download media: empty response for code %s", a.Name(), pyrepr.Str(code)))
		return
	}
	url, _ := body.Get("downloadUrl")
	if !pyrepr.Truthy(url) {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch container := ref.Container.(type) {
	case *ImageContent:
		container.DownloadCode = pyrepr.Str(url)
	case map[string]any:
		container[ref.Key] = url
	}
}

// Works around renderer quirks, a blank line before a numbered list and fenced code lines dedented
func normalizeMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	numbered := func(s string) bool {
		m, err := numberedRE.MatchString(s)
		return err == nil && m
	}
	for i, line := range lines {
		prev := ""
		if i > 0 {
			prev = pystr.Strip(lines[i-1])
		}
		if prev != "" && numbered(pystr.Strip(line)) && !numbered(prev) {
			out = append(out, "")
		}
		if strings.HasPrefix(pystr.Strip(line), "```") {
			out = append(out, pystr.Lstrip(line))
		} else {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// Chatbot callback handler forwarding each message to the adapter
type incomingHandler struct {
	adapter *DingTalkAdapter
}

// Start hook the stream client runs on every handler before opening the socket
func (h *incomingHandler) preStart() {}

// Converts a callback into a chatbot message, dispatches it in the background and acknowledges at once
func (h *incomingHandler) process(ctx context.Context, message *streamMessage) (int, string) {
	chatbotMsg, err := h.prepare(ctx, message)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Error preparing incoming message", h.adapter.Name()), "error", err)
		return ackStatusSystemError, "error"
	}
	gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
		h.safeOnMessage(tctx, chatbotMsg)
		return nil
	})
	return ackStatusOK, "OK"
}

// Builds the chatbot message of a callback, backfilling the webhook and mention flag and starting the thinking reaction
func (h *incomingHandler) prepare(ctx context.Context, message *streamMessage) (*IncomingMessage, error) {
	data := message.data
	if text, ok := data.(string); ok {
		parsed, err := pyjson.Loads(text)
		if err != nil {
			return nil, err
		}
		data = parsed
	}
	chatbotMsg, err := incomingMessageFromDict(data)
	if err != nil {
		return nil, err
	}
	fields, _ := data.(*pyjson.Object)
	if fields == nil {
		fields = pyjson.NewObject()
	}
	webhook := fields.GetOr("sessionWebhook", nil)
	if !pyrepr.Truthy(webhook) {
		webhook = fields.GetOr("session_webhook", nil)
	}
	if pyrepr.Truthy(webhook) && !pyrepr.Truthy(chatbotMsg.SessionWebhook) {
		chatbotMsg.SessionWebhook = webhook
	}
	if !pyrepr.Truthy(chatbotMsg.IsInAtList) && pyrepr.Truthy(fields.GetOr("isInAtList", nil)) {
		chatbotMsg.IsInAtList = true
	}
	msgID, conversationID := orEmpty(chatbotMsg.MessageID), orEmpty(chatbotMsg.ConversationID)
	if pyrepr.Truthy(msgID) && pyrepr.Truthy(conversationID) {
		thinking := i18n.T(ctx, "platform.dingtalk.emotion.thinking", "", nil)
		h.adapter.spawnBG(ctx, func(tctx context.Context) error {
			return h.adapter.sendEmotion(tctx, pyrepr.Str(msgID), pyrepr.Str(conversationID), thinking, false)
		})
	}
	return chatbotMsg, nil
}

// Processes one message, a failure logged with its error
func (h *incomingHandler) safeOnMessage(ctx context.Context, chatbotMsg *IncomingMessage) {
	if err := h.adapter.onMessage(ctx, chatbotMsg); err != nil {
		slog.Error(fmt.Sprintf("[%s] Error processing incoming message", h.adapter.Name()), "error", err)
	}
}
