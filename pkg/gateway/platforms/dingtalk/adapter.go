package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
)

const (
	maxMessageLength    = 20000
	sessionWebhooksMax  = 500
	messageContextsMax  = 500
	circuitBreakerTrips = 5
	circuitBreakerDelay = 300 * time.Second
	stableSession       = 60 * time.Second
	messageTitle        = "Icebath"
)

// Reconnect delays of the stream loop before the circuit breaker trips
var reconnectBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, 60 * time.Second}

// Hosts a session webhook may point at
var sessionWebhookRE = regexp.MustCompile(`^https://(?:api|oapi)\.dingtalk\.com/`)

var truthyText = map[string]bool{"true": true, "1": true, "yes": true, "on": true}

// Session webhook of a chat and its expiry in epoch milliseconds
type sessionWebhook struct {
	url       string
	expiresAt int64
}

// Insertion ordered map capped at a size by evicting its oldest key
type boundedMap[V any] struct {
	max   int
	order []string
	items map[string]V
}

func newBoundedMap[V any](max int) *boundedMap[V] {
	return &boundedMap[V]{max: max, items: map[string]V{}}
}

func (m *boundedMap[V]) get(key string) (V, bool) {
	v, ok := m.items[key]
	return v, ok
}

// Stores the value, a new key evicting the oldest one past the cap
func (m *boundedMap[V]) set(key string, v V) {
	if _, ok := m.items[key]; !ok {
		if len(m.items) >= m.max && len(m.order) > 0 {
			delete(m.items, m.order[0])
			m.order = m.order[1:]
		}
		m.order = append(m.order, key)
	}
	m.items[key] = v
}

func (m *boundedMap[V]) remove(key string) {
	if _, ok := m.items[key]; !ok {
		return
	}
	delete(m.items, key)
	for i, k := range m.order {
		if k == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

func (m *boundedMap[V]) clear() {
	m.order, m.items = nil, map[string]V{}
}

// DingTalkAdapter connects one DingTalk app in stream mode and replies through session webhooks, AI cards and the robot API
type DingTalkAdapter struct {
	*gateway.BasePlatformAdapter

	clientID        string
	clientSecret    string
	robotCode       string
	cardTemplateID  string
	mentionPatterns []*regexp2.Regexp
	allowedUsers    map[string]bool
	dedup           *gateway.MessageDeduplicator
	book            *conversationBook

	mu             sync.Mutex
	httpClient     *http.Client
	downloads      *http.Client
	api            *openAPI
	stream         *StreamClient
	streamTask     *gateway.AdapterTask
	webhooks       *boundedMap[sessionWebhook]
	contexts       *boundedMap[*chatbotMessage]
	doneFired      map[string]bool
	streamingCards map[string]map[string]string
	chatTails      map[string]chan struct{}
	bgTasks        map[*gateway.AdapterTask]struct{}
}

// Traits of the adapter, message editing and explicit finalize only with AI cards
func dingtalkTraits(cards bool) gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.MaxMessageLength = maxMessageLength
	traits.SupportsMessageEditing = cards
	traits.RequiresEditFinalize = cards
	return traits
}

// Client id and secret from the platform extra first, then the profile scoped env
func credentials(ctx context.Context, extra map[string]any) (string, string, error) {
	id := strings.TrimSpace(str(extra["client_id"]))
	secret := strings.TrimSpace(str(extra["client_secret"]))
	var err error
	if id == "" {
		if id, err = gateway.GetScopedSecret(ctx, "DINGTALK_CLIENT_ID", "", false); err != nil {
			return "", "", err
		}
	}
	if secret == "" {
		if secret, err = gateway.GetScopedSecret(ctx, "DINGTALK_CLIENT_SECRET", "", false); err != nil {
			return "", "", err
		}
	}
	return strings.TrimSpace(id), strings.TrimSpace(secret), nil
}

// Adapter reading credentials, gating and AI card settings from the platform config and the profile env
func NewDingTalkAdapter(ctx context.Context, config *gateway.PlatformConfig) (*DingTalkAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	if config.Extra == nil {
		config.Extra = map[string]any{}
	}
	a := &DingTalkAdapter{webhooks: newBoundedMap[sessionWebhook](sessionWebhooksMax),
		contexts: newBoundedMap[*chatbotMessage](messageContextsMax), doneFired: map[string]bool{},
		streamingCards: map[string]map[string]string{}, chatTails: map[string]chan struct{}{},
		bgTasks: map[*gateway.AdapterTask]struct{}{}, dedup: gateway.NewMessageDeduplicator(1000, gateway.DefaultDedupTTLSeconds)}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformDingTalk, a)
	extra := config.Extra
	var err error
	if a.clientID, a.clientSecret, err = credentials(ctx, extra); err != nil {
		return nil, err
	}
	a.robotCode = firstNonEmpty(strings.TrimSpace(str(extra["robot_code"])), a.clientID)
	a.cardTemplateID = strings.TrimSpace(str(extra["card_template_id"]))
	a.Traits = dingtalkTraits(a.cardTemplateID != "")
	if a.mentionPatterns, err = compileMentionPatterns(ctx, extra, a.Name()); err != nil {
		return nil, err
	}
	rawUsers, err := gateway.ExtraOrSecret(ctx, extra, "allowed_users", "DINGTALK_ALLOWED_USERS", "", false)
	if err != nil {
		return nil, err
	}
	a.allowedUsers = map[string]bool{}
	for u := range gateway.CoerceAllowSet(rawUsers) {
		a.allowedUsers[strings.ToLower(u)] = true
	}
	a.book = loadConversationBook(ctx)
	return a, nil
}

// Wake word patterns from the config list or the env as JSON, lines or comma separated values
func compileMentionPatterns(ctx context.Context, extra map[string]any, name string) ([]*regexp2.Regexp, error) {
	raw, present := extra["mention_patterns"]
	if !present || raw == nil {
		env, err := gateway.GetScopedSecret(ctx, "DINGTALK_MENTION_PATTERNS", "", false)
		if err != nil {
			return nil, err
		}
		env = strings.TrimSpace(env)
		if env == "" {
			return nil, nil
		}
		var decoded any
		if json.Unmarshal([]byte(env), &decoded) == nil {
			raw = decoded
		} else {
			parts := splitNonEmpty(env, "\n")
			if len(parts) == 0 {
				parts = splitNonEmpty(env, ",")
			}
			raw = parts
		}
	}
	label := "dingtalk"
	return gateway.CompileMentionPatterns(raw, gateway.MentionPatternOptions{LogPrefix: name, PlatformLabel: &label, DisplayLabel: "DingTalk"}), nil
}

// Trimmed non empty parts of the text split on the separator
func splitNonEmpty(text, sep string) []any {
	var out []any
	for _, part := range strings.Split(text, sep) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Current OpenAPI client, nil before connect
func (a *DingTalkAdapter) currentAPI() *openAPI {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.api
}

// Opens the stream connection under the app lock and starts the receive loop, false when it cannot connect
func (a *DingTalkAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.clientID == "" || a.clientSecret == "" {
		msg := "DINGTALK_CLIENT_ID and DINGTALK_CLIENT_SECRET required"
		slog.Warn(fmt.Sprintf("[%s] %s", a.Name(), msg))
		a.SetFatalError(ctx, "dingtalk_missing_credentials", msg, false)
		return false, nil
	}
	acquired, err := a.AcquirePlatformLock(ctx, "dingtalk-app", a.clientID, "DingTalk app "+a.clientID)
	if err != nil {
		return false, err
	}
	if !acquired {
		return false, nil
	}
	httpClient := gateway.LimitedHTTPClient(gateway.PlatformHTTPXLimits(), true)
	dialer := &websocket.Dialer{HandshakeTimeout: 30 * time.Second, Proxy: http.ProxyFromEnvironment}
	if !gateway.GatewayTrustEnv(ctx) {
		if tr, ok := httpClient.Transport.(*http.Transport); ok {
			tr.Proxy = nil
		}
		dialer.Proxy = nil
	}
	api := newOpenAPI(a.clientID, a.clientSecret, a.robotCode, httpClient)
	stream := newStreamClient(a.clientID, a.clientSecret, httpClient, dialer)
	stream.register(botMessageTopic, a.onChatbotFrame, true)
	timeout := 60 * time.Second
	a.mu.Lock()
	a.httpClient, a.api, a.stream = httpClient, api, stream
	a.downloads = gateway.PlatformHTTPClient(ctx, gateway.PlatformHTTPClientOptions{Timeout: &timeout})
	a.mu.Unlock()
	if a.cardTemplateID != "" {
		slog.Info(fmt.Sprintf("[%s] AI Cards enabled with template: %s", a.Name(), a.cardTemplateID))
	}
	a.WirePluginHandlers(ctx, stream)
	conn, err := stream.establish(ctx)
	if err != nil {
		a.ReleasePlatformLock()
		a.recordStreamFailure(ctx, err)
		return false, nil
	}
	a.MarkConnected(ctx, "")
	task := gateway.SpawnAdapterTask(context.WithoutCancel(ctx), func(tctx context.Context) error {
		a.runStream(tctx, stream, conn)
		return nil
	})
	a.mu.Lock()
	a.streamTask = task
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[%s] Connected via Stream Mode", a.Name()))
	return true, nil
}

// Records why the stream could not be opened, refused credentials as permanent and anything else as retryable
func (a *DingTalkAdapter) recordStreamFailure(ctx context.Context, err error) {
	var se *streamError
	if errors.As(err, &se) && se.credentialsRejected() {
		msg := fmt.Sprintf("DingTalk rejected the client credentials, check DINGTALK_CLIENT_ID and DINGTALK_CLIENT_SECRET (%v)", err)
		slog.Error(fmt.Sprintf("[%s] %s", a.Name(), msg))
		a.SetFatalError(ctx, "dingtalk_auth_failed", msg, false)
		return
	}
	msg := fmt.Sprintf("DingTalk stream connection failed: %v", err)
	slog.Error(fmt.Sprintf("[%s] %s", a.Name(), msg))
	a.SetFatalError(ctx, "dingtalk_connect_failed", msg, true)
}

// Reconnect bookkeeping of the stream loop
type reconnectState struct {
	backoffIdx  int
	consecutive int
	lastKind    string
	degraded    bool
}

// Serves the stream and reconnects with backoff, a breaker slowing identical failures until the error changes
func (a *DingTalkAdapter) runStream(ctx context.Context, stream *StreamClient, conn *websocket.Conn) {
	st := &reconnectState{}
	for {
		started := time.Now()
		err := stream.serve(ctx, conn)
		if ctx.Err() != nil {
			return
		}
		if err == nil || time.Since(started) >= stableSession {
			st.backoffIdx, st.consecutive, st.lastKind = 0, 0, ""
		}
		if err != nil && !a.awaitReconnect(ctx, st, err) {
			return
		}
		for {
			conn, err = stream.establish(ctx)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return
			}
			var se *streamError
			if errors.As(err, &se) && se.credentialsRejected() {
				a.recordStreamFailure(ctx, err)
				if nerr := a.NotifyFatalError(ctx); nerr != nil {
					slog.Debug(fmt.Sprintf("[%s] Fatal error handler failed: %v", a.Name(), nerr))
				}
				return
			}
			if !a.awaitReconnect(ctx, st, err) {
				return
			}
		}
		if st.degraded {
			st.degraded = false
			a.MarkConnected(ctx, "")
			slog.Info(fmt.Sprintf("[%s] Stream reconnected", a.Name()))
		}
	}
}

// Logs a stream failure and sleeps its backoff, false when the loop was stopped while waiting
func (a *DingTalkAdapter) awaitReconnect(ctx context.Context, st *reconnectState, err error) bool {
	kind := "error"
	var se *streamError
	if errors.As(err, &se) {
		kind = se.kind
	}
	if kind == st.lastKind {
		st.consecutive++
	} else {
		st.consecutive, st.lastKind = 1, kind
	}
	if !st.degraded {
		st.degraded = true
		a.MarkDegraded(ctx)
	}
	var delay time.Duration
	switch {
	case st.consecutive <= circuitBreakerTrips:
		slog.Warn(fmt.Sprintf("[%s] Stream client error: %v", a.Name(), err))
		delay = reconnectBackoff[min(st.backoffIdx, len(reconnectBackoff)-1)]
		st.backoffIdx++
		slog.Info(fmt.Sprintf("[%s] Reconnecting in %ds...", a.Name(), int(delay.Seconds())))
	case st.consecutive == circuitBreakerTrips+1:
		slog.Error(fmt.Sprintf("[%s] Stream client error repeated %d times: %v. Circuit breaker tripped, retrying every %ds "+
			"silently until the error changes.", a.Name(), st.consecutive, err, int(circuitBreakerDelay.Seconds())))
		delay = circuitBreakerDelay
	default:
		delay = circuitBreakerDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Stops the stream, finalizes open AI cards and releases the app lock
func (a *DingTalkAdapter) Disconnect(ctx context.Context) error {
	a.MarkDisconnected(ctx)
	a.mu.Lock()
	stream, task := a.stream, a.streamTask
	a.streamTask = nil
	tasks := make([]*gateway.AdapterTask, 0, len(a.bgTasks))
	for t := range a.bgTasks {
		tasks = append(tasks, t)
	}
	a.mu.Unlock()
	if stream != nil {
		stream.closeConn()
	}
	wait := func(t *gateway.AdapterTask) {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := t.Wait(wctx); err != nil && wctx.Err() != nil {
			slog.Debug(fmt.Sprintf("[%s] Task did not exit cleanly during disconnect", a.Name()))
		}
	}
	if task != nil {
		task.Cancel()
		wait(task)
	}
	for _, t := range tasks {
		t.Cancel()
		wait(t)
	}
	a.mu.Lock()
	chats := make([]string, 0, len(a.streamingCards))
	for chatID := range a.streamingCards {
		chats = append(chats, chatID)
	}
	a.mu.Unlock()
	for _, chatID := range chats {
		a.closeStreamingSiblings(ctx, chatID)
	}
	a.mu.Lock()
	if a.httpClient != nil {
		a.httpClient.CloseIdleConnections()
	}
	if a.downloads != nil {
		a.downloads.CloseIdleConnections()
	}
	a.httpClient, a.downloads, a.api, a.stream = nil, nil, nil, nil
	a.webhooks.clear()
	a.contexts.clear()
	a.doneFired, a.streamingCards, a.bgTasks = map[string]bool{}, map[string]map[string]string{}, map[*gateway.AdapterTask]struct{}{}
	a.mu.Unlock()
	a.dedup.Clear()
	a.ReleasePlatformLock()
	slog.Info(fmt.Sprintf("[%s] Disconnected", a.Name()))
	return nil
}

// Starts a tracked fire and forget task the disconnect cancels
func (a *DingTalkAdapter) spawnBG(ctx context.Context, fn func(ctx context.Context)) {
	task := gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
		fn(tctx)
		return nil
	})
	a.mu.Lock()
	a.bgTasks[task] = struct{}{}
	a.mu.Unlock()
	task.AddDoneCallback(func() {
		a.mu.Lock()
		delete(a.bgTasks, task)
		a.mu.Unlock()
	})
}

// Acks a chatbot callback at once and hands the message to admission
func (a *DingTalkAdapter) onChatbotFrame(ctx context.Context, frame *Frame) (int, string) {
	msg, err := parseChatbotMessage(frame.Data)
	if err != nil {
		slog.Error(fmt.Sprintf("[%s] Error preparing incoming message: %v", a.Name(), err))
		return AckSystemException, "error"
	}
	a.admit(ctx, msg)
	return AckOK, "OK"
}

// Gates, records and dispatches one inbound message in arrival order per chat
func (a *DingTalkAdapter) admit(ctx context.Context, msg *chatbotMessage) {
	name := a.Name()
	msgID := msg.MessageID
	if msgID == "" {
		msgID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if a.dedup.IsDuplicate(msgID) {
		slog.Debug(fmt.Sprintf("[%s] Duplicate message %s, skipping", name, msgID))
		return
	}
	chatID, isGroup := msg.chatID(), msg.isGroup()
	if !a.isUserAllowed(msg.SenderID, msg.SenderStaffID) {
		slog.Debug(fmt.Sprintf("[%s] Dropping message from non-allowlisted user staff_id=%s sender_id=%s", name, msg.SenderStaffID, msg.SenderID))
		return
	}
	text := msg.text()
	if !a.shouldProcess(ctx, msg, text, isGroup, chatID) {
		slog.Debug(fmt.Sprintf("[%s] Dropping group message that failed mention gate message_id=%s chat_id=%s", name, msgID, chatID))
		return
	}
	if chatID != "" {
		a.rememberChat(chatID, msg)
	}
	msgType, atts := msg.attachments()
	if text == "" && len(atts) == 0 {
		slog.Debug(fmt.Sprintf("[%s] Empty message, skipping", name))
		return
	}
	if msg.MessageID != "" && msg.ConversationID != "" {
		thinking := i18n.T(ctx, "platform.dingtalk.emotion.thinking", "", nil)
		a.spawnBG(ctx, func(c context.Context) { a.sendEmotion(c, msg.MessageID, msg.ConversationID, thinking, false) })
	}
	prev, mine := a.chainChat(chatID)
	task := gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
		defer a.releaseChat(chatID, mine)
		err := a.deliverInbound(tctx, msg, msgID, chatID, text, msgType, atts, prev)
		if err != nil && tctx.Err() == nil {
			slog.Error(fmt.Sprintf("[%s] Error processing incoming message: %v", name, err))
		}
		return err
	})
	a.TrackBackgroundTask(task)
}

// Builds the message event with its downloaded media and hands it to the gateway after the chat's previous message
func (a *DingTalkAdapter) deliverInbound(ctx context.Context, msg *chatbotMessage, msgID, chatID, text string,
	msgType gateway.MessageType, atts []attachment, prev <-chan struct{}) error {
	urls, types, names := a.fetchAttachments(ctx, msg, atts)
	userName := firstNonEmpty(msg.SenderNick, msg.SenderID)
	senderID := msg.SenderID
	fields := gateway.SourceFields{ChatType: "dm", UserID: &senderID, UserName: &userName, MessageID: &msgID}
	if msg.isGroup() {
		fields.ChatType = "group"
	}
	if msg.ConversationTitle != "" {
		title := msg.ConversationTitle
		fields.ChatName = &title
	}
	if msg.SenderStaffID != "" {
		staff := msg.SenderStaffID
		fields.UserIDAlt = &staff
	}
	event := gateway.NewMessageEvent(text)
	event.MessageType = msgType
	event.Source = a.BuildSource(ctx, chatID, fields)
	event.MessageID = &msgID
	event.RawMessage = msg.raw
	event.MediaURLs, event.MediaTypes = urls, types
	for _, n := range names {
		if n != "" {
			event.MediaFilenames = names
			break
		}
	}
	event.Timestamp = msg.timestamp()
	preview := "(media)"
	if text != "" {
		preview = clip(text, 80)
	}
	slog.Debug(fmt.Sprintf("[%s] Message from %s in %s: %s", a.Name(), userName, clip(chatID, 20), preview))
	if prev != nil {
		select {
		case <-prev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return a.HandleMessage(ctx, event)
}

// Previous inbound task of the chat and the completion channel of the new one
func (a *DingTalkAdapter) chainChat(chatID string) (<-chan struct{}, chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	mine := make(chan struct{})
	prev := a.chatTails[chatID]
	a.chatTails[chatID] = mine
	if prev == nil {
		return nil, mine
	}
	return prev, mine
}

// Marks the inbound task done and drops it as the chat's tail
func (a *DingTalkAdapter) releaseChat(chatID string, mine chan struct{}) {
	close(mine)
	a.mu.Lock()
	if a.chatTails[chatID] == mine {
		delete(a.chatTails, chatID)
	}
	a.mu.Unlock()
}

// Stores the chat's latest message, session webhook and conversation facts, resetting its done reaction
func (a *DingTalkAdapter) rememberChat(chatID string, msg *chatbotMessage) {
	a.mu.Lock()
	a.contexts.set(chatID, msg)
	delete(a.doneFired, chatID)
	if msg.SessionWebhook != "" && sessionWebhookRE.MatchString(msg.SessionWebhook) {
		a.webhooks.set(chatID, sessionWebhook{url: msg.SessionWebhook, expiresAt: msg.WebhookExpiresAt})
	}
	a.mu.Unlock()
	c := conversation{Group: msg.isGroup(), Title: msg.ConversationTitle}
	if !c.Group {
		c.StaffID = msg.SenderStaffID
	}
	a.book.record(chatID, c)
}

// Latest inbound message of a chat
func (a *DingTalkAdapter) messageContext(chatID string) *chatbotMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	msg, _ := a.contexts.get(chatID)
	return msg
}

// Session webhook of the chat unless it expires within five minutes
func (a *DingTalkAdapter) validWebhook(chatID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	info, ok := a.webhooks.get(chatID)
	if !ok {
		return ""
	}
	if info.expiresAt > 0 && time.Now().UnixMilli()+5*60*1000 >= info.expiresAt {
		a.webhooks.remove(chatID)
		return ""
	}
	return info.url
}

// Drops a session webhook DingTalk refused
func (a *DingTalkAdapter) forgetWebhook(chatID, url string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if info, ok := a.webhooks.get(chatID); ok && info.url == url {
		a.webhooks.remove(chatID)
	}
}

// Whether the sender or staff id is on the allowlist, an empty list or a star allowing everyone
func (a *DingTalkAdapter) isUserAllowed(senderID, staffID string) bool {
	if len(a.allowedUsers) == 0 || a.allowedUsers["*"] {
		return true
	}
	for _, id := range []string{senderID, staffID} {
		if id != "" && a.allowedUsers[strings.ToLower(id)] {
			return true
		}
	}
	return false
}

// Per profile setting from the scoped env, then the platform extra, then the default
func (a *DingTalkAdapter) setting(ctx context.Context, key, env string, def any) any {
	v, err := gateway.ExtraOrSecret(ctx, a.Config.Extra, key, env, def, false)
	if err != nil {
		slog.Debug(fmt.Sprintf("[%s] Reading %s failed: %v", a.Name(), env, err))
		return def
	}
	return v
}

// Whether group chats need an explicit trigger before the bot answers
func (a *DingTalkAdapter) requireMention(ctx context.Context) bool {
	switch v := a.setting(ctx, "require_mention", "DINGTALK_REQUIRE_MENTION", "false").(type) {
	case string:
		return truthyText[strings.ToLower(v)]
	case bool:
		return v
	case int, int64, float64:
		return v != 0
	case nil:
		return false
	}
	return true
}

// Group trigger rules, the chat allowlist a hard gate, then free response, mention off, an at mention or a wake word
func (a *DingTalkAdapter) shouldProcess(ctx context.Context, msg *chatbotMessage, text string, isGroup bool, chatID string) bool {
	if !isGroup {
		return true
	}
	allowed := gateway.CoerceAllowSet(a.setting(ctx, "allowed_chats", "DINGTALK_ALLOWED_CHATS", ""))
	if len(allowed) > 0 && chatID != "" && !allowed[chatID] {
		return false
	}
	free := gateway.CoerceAllowSet(a.setting(ctx, "free_response_chats", "DINGTALK_FREE_RESPONSE_CHATS", ""))
	return (chatID != "" && free[chatID]) || !a.requireMention(ctx) || msg.IsInAtList || a.matchesMentionPatterns(text)
}

// Whether any wake word pattern matches the text
func (a *DingTalkAdapter) matchesMentionPatterns(text string) bool {
	if text == "" {
		return false
	}
	for _, re := range a.mentionPatterns {
		if ok, err := re.MatchString(text); err == nil && ok {
			return true
		}
	}
	return false
}

// Adds or recalls a text emotion on a message, failures logged at debug
func (a *DingTalkAdapter) sendEmotion(ctx context.Context, msgID, conversationID, name string, recall bool) {
	api := a.currentAPI()
	if api == nil || msgID == "" || conversationID == "" {
		return
	}
	action := "reply"
	if recall {
		action = "recall"
	}
	if err := api.emotion(ctx, msgID, conversationID, name, recall); err != nil {
		slog.Debug(fmt.Sprintf("[%s] Emotion %s failed: %v", a.Name(), action, err))
		return
	}
	slog.Info(fmt.Sprintf("[%s] Emotion %s %s on msg=%s", a.Name(), action, name, clip(msgID, 24)))
}

// Swaps the thinking reaction for the done reaction on the chat's last message once per inbound message
func (a *DingTalkAdapter) fireDoneReaction(ctx context.Context, chatID string) {
	a.mu.Lock()
	if a.doneFired[chatID] {
		a.mu.Unlock()
		return
	}
	a.doneFired[chatID] = true
	msg, _ := a.contexts.get(chatID)
	a.mu.Unlock()
	if msg == nil || msg.MessageID == "" || msg.ConversationID == "" {
		return
	}
	thinking := i18n.T(ctx, "platform.dingtalk.emotion.thinking", "", nil)
	done := i18n.T(ctx, "platform.dingtalk.emotion.done", "", nil)
	a.spawnBG(ctx, func(c context.Context) {
		a.sendEmotion(c, msg.MessageID, msg.ConversationID, thinking, true)
		a.sendEmotion(c, msg.MessageID, msg.ConversationID, done, false)
	})
}

// Name and type of a conversation from what the adapter has seen of it
func (a *DingTalkAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	if c, ok := a.book.lookup(chatID); ok {
		kind := "dm"
		if c.Group {
			kind = "group"
		}
		return map[string]any{"name": firstNonEmpty(c.Title, chatID), "type": kind}, nil
	}
	kind := "dm"
	if strings.Contains(strings.ToLower(chatID), "group") {
		kind = "group"
	}
	return map[string]any{"name": chatID, "type": kind}, nil
}

var _ gateway.PlatformAdapter = (*DingTalkAdapter)(nil)
