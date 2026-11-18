package googlechat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	maxTextLength          = 4000
	rateLimitWarnThreshold = 5
	maxReconnectAttempts   = 10
	reconnectBaseDelay     = 2.0
	reconnectMaxDelay      = 120.0
	legacyUserIdentity     = "__legacy__"
	typingConsumedSentinel = "<consumed>"
)

// Subscription resource path with its project and name
var subscriptionPathRE = regexp.MustCompile(`^projects/([^/]+)/subscriptions/([^/]+)$`)

// Message type of a MIME type, documents for anything that is not image, audio or video
func mimeForMessageType(mime string) gateway.MessageType {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return gateway.MessageTypePhoto
	case strings.HasPrefix(mime, "audio/"):
		return gateway.MessageTypeAudio
	case strings.HasPrefix(mime, "video/"):
		return gateway.MessageTypeVideo
	}
	return gateway.MessageTypeDocument
}

// Google Chat adapter, Pub/Sub pull or authenticated HTTP callbacks in and the Chat REST API out
type GoogleChatAdapter struct {
	*gateway.BasePlatformAdapter

	maxMessages              int
	maxBytes                 int
	bootstrapSpaces          string
	debugRaw                 bool
	httpEventsURL            string
	httpEventsAudience       string
	httpEventsServiceAccount string
	threadCounts             *threadCountStore
	dedup                    *gateway.MessageDeduplicator

	mu                sync.Mutex
	scopeCtx          context.Context
	api               *restClient
	projectID         string
	subscriptionPath  string
	botUserID         string
	supervisor        *gateway.AdapterTask
	shuttingDown      bool
	userCreds         *userCreds
	userAPI           *restClient
	userCredsByEmail  map[string]*userCreds
	userAPIByEmail    map[string]*restClient
	lastSenderByChat  map[string]string
	typingMessages    map[string]string
	typingInflight    map[string]chan struct{}
	orphanTyping      map[string][]string
	clarifyState      map[string]string
	rateLimitHits     map[string]int
	lastInboundThread map[string]string
	inflightBytes     int
	inflightCond      *sync.Cond
}

// Class attributes the Google Chat adapter declares over the base defaults
func googleChatAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.MaxMessageLength = maxTextLength
	return traits
}

// Integer setting from the extra then the scoped env, the default when it does not parse
func intSetting(ctx context.Context, extra map[string]any, key, env string, def int) int {
	raw := extra[key]
	if !pyrepr.Truthy(raw) {
		v, err := gateway.GetScopedSecret(ctx, env, strconv.Itoa(def), false)
		if err != nil {
			return def
		}
		raw = v
	}
	switch n := raw.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if i, err := strconv.Atoi(pystr.Strip(n)); err == nil {
			return i
		}
	}
	return def
}

// String setting from the extra then the scoped env then the fallback, stripped
func strSetting(ctx context.Context, extra map[string]any, key, env, fallback string) string {
	if v, ok := extra[key].(string); ok && v != "" {
		return pystr.Strip(v)
	}
	if v, err := gateway.GetScopedSecret(ctx, env, "", false); err == nil && v != "" {
		return pystr.Strip(v)
	}
	return pystr.Strip(fallback)
}

// Adapter for a platform config with the profile scoped settings snapshotted now
func NewGoogleChatAdapter(ctx context.Context, config *gateway.PlatformConfig) (*GoogleChatAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	if config.Extra == nil {
		config.Extra = map[string]any{}
	}
	a := &GoogleChatAdapter{threadCounts: newThreadCountStore(ctx), dedup: gateway.NewMessageDeduplicator(2000, 300),
		userCredsByEmail: map[string]*userCreds{}, userAPIByEmail: map[string]*restClient{}, lastSenderByChat: map[string]string{},
		typingMessages: map[string]string{}, typingInflight: map[string]chan struct{}{}, orphanTyping: map[string][]string{},
		clarifyState: map[string]string{}, rateLimitHits: map[string]int{}, lastInboundThread: map[string]string{}}
	a.inflightCond = sync.NewCond(&a.mu)
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.Platform("google_chat"), a)
	a.Traits = googleChatAdapterTraits()
	extra := config.Extra
	a.maxMessages = intSetting(ctx, extra, "max_messages", "GOOGLE_CHAT_MAX_MESSAGES", 1)
	a.maxBytes = intSetting(ctx, extra, "max_bytes", "GOOGLE_CHAT_MAX_BYTES", 16*1024*1024)
	a.bootstrapSpaces = strSetting(ctx, extra, "bootstrap_spaces", "GOOGLE_CHAT_BOOTSTRAP_SPACES", "")
	debug, _ := gateway.GetScopedSecret(ctx, "GOOGLE_CHAT_DEBUG_RAW", "", false)
	a.debugRaw = pyrepr.Truthy(extra["debug_raw"]) || debug != ""
	a.httpEventsURL = strSetting(ctx, extra, "http_events_url", "GOOGLE_CHAT_HTTP_EVENTS_URL", "")
	a.httpEventsAudience = strSetting(ctx, extra, "http_events_audience", "GOOGLE_CHAT_HTTP_EVENTS_AUDIENCE", a.httpEventsURL)
	a.httpEventsServiceAccount = strings.ToLower(strSetting(ctx, extra, "http_events_service_account_email", "GOOGLE_CHAT_HTTP_EVENTS_SERVICE_ACCOUNT_EMAIL", ""))
	return a, nil
}

// Service account credential from the extra or the env key path, application default credentials otherwise
func (a *GoogleChatAdapter) loadSACredentials(ctx context.Context) (*restClient, error) {
	saPath := pyrepr.Str(a.Config.Extra["service_account_json"])
	if !pyrepr.Truthy(a.Config.Extra["service_account_json"]) {
		saPath, _ = gateway.GetScopedSecret(ctx, "GOOGLE_APPLICATION_CREDENTIALS", "", false)
	}
	creds, err := loadSACredentialsFrom(ctx, saPath)
	var classified *saCredentialError
	if errors.As(err, &classified) {
		return nil, errors.New(saErrorText(saErrorMessages, classified, saPath))
	}
	if err != nil {
		return nil, err
	}
	if saPath == "" {
		pkgLog().Info("[GoogleChat] No SA JSON configured; using Application Default Credentials")
	}
	return newRESTClient(creds), nil
}

// Project id and subscription path, the latter empty for HTTP inbound
func (a *GoogleChatAdapter) validateConfig() (string, string, error) {
	extra := a.Config.Extra
	projectID := pystr.Strip(pyrepr.Str(orEmpty(extra["project_id"])))
	subscription := pystr.Strip(pyrepr.Str(orEmpty(extra["subscription_name"])))
	httpEventsURL := pystr.Strip(pyrepr.Str(orEmpty(extra["http_events_url"])))
	if subscription != "" {
		m := subscriptionPathRE.FindStringSubmatch(subscription)
		if m == nil {
			return "", "", errors.New("GOOGLE_CHAT_SUBSCRIPTION_NAME must match 'projects/<project>/subscriptions/<sub>'.")
		}
		if projectID != "" && m[1] != projectID {
			return "", "", errors.New("project_id in GOOGLE_CHAT_PROJECT_ID does not match the project embedded in GOOGLE_CHAT_SUBSCRIPTION_NAME.")
		}
		if projectID == "" {
			projectID = m[1]
		}
		return projectID, subscription, nil
	}
	if httpEventsURL != "" {
		return projectID, "", nil
	}
	if projectID == "" {
		return "", "", errors.New("GOOGLE_CHAT_PROJECT_ID (or GOOGLE_CLOUD_PROJECT) is not set.")
	}
	return "", "", errors.New("GOOGLE_CHAT_SUBSCRIPTION_NAME (or GOOGLE_CHAT_SUBSCRIPTION) is not set. Set GOOGLE_CHAT_HTTP_EVENTS_URL for HTTP callback mode.")
}

// Empty text for nil
func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// Bot id cache file of the profile
func botIDCachePath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "google_chat_bot_id.json")
}

// Cached users id of the bot, empty when none is stored
func loadCachedBotID(ctx context.Context) string {
	raw, err := os.ReadFile(botIDCachePath(ctx))
	if err != nil {
		return ""
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return ""
	}
	var data map[string]any
	if json.Unmarshal([]byte(text), &data) != nil {
		return ""
	}
	id, _ := data["bot_user_id"].(string)
	return id
}

// Persists the bot id, a failure only logged
func saveCachedBotID(ctx context.Context, botUserID string) {
	path := botIDCachePath(ctx)
	data, _ := json.Marshal(map[string]string{"bot_user_id": botUserID})
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		err = os.WriteFile(path, data, 0o644)
		if err == nil {
			return
		}
	}
	pkgLog().Debug("[GoogleChat] Could not persist bot_user_id cache")
}

// Bot users id from the members of the home channel then the bootstrap spaces
func (a *GoogleChatAdapter) resolveBotUserID(ctx context.Context) string {
	var spaces []string
	if hc := a.Config.HomeChannel; hc != nil && hc.ChatID != "" {
		spaces = append(spaces, hc.ChatID)
	}
	for _, s := range strings.Split(a.bootstrapSpaces, ",") {
		if s = pystr.Strip(s); s != "" {
			spaces = append(spaces, s)
		}
	}
	for _, space := range spaces {
		members, err := a.api.listMembers(ctx, space, 50)
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("[GoogleChat] members.list failed on %s: %s", space, redactSensitive(err.Error())))
			continue
		}
		list, _ := members["memberships"].([]any)
		for _, raw := range list {
			m, _ := raw.(map[string]any)
			member, _ := m["member"].(map[string]any)
			if member["type"] == "BOT" && pyrepr.Truthy(member["name"]) {
				return pyrepr.Str(member["name"])
			}
		}
	}
	return ""
}

// Logs a redacted connect failure, records it as fatal and reports failure
func (a *GoogleChatAdapter) connectFailed(ctx context.Context, logFmt string, err error, code string, retryable bool) (bool, error) {
	msg := redactSensitive(err.Error())
	pkgLog().Error(fmt.Sprintf(logFmt, msg))
	a.SetFatalError(ctx, code, msg, retryable)
	return false, nil
}

// Loads the shared single user OAuth token, a failure only degrading attachments to a notice
func (a *GoogleChatAdapter) loadUserOAuth(ctx context.Context) {
	creds := loadUserCredentials(ctx, "")
	a.mu.Lock()
	if creds != nil {
		a.userCreds, a.userAPI = creds, newRESTClient(creds.creds)
	}
	a.mu.Unlock()
	if creds != nil {
		pkgLog().Info("[GoogleChat] Legacy user OAuth loaded — fallback attachment delivery enabled")
	}
	if authorized := listAuthorizedEmails(ctx); len(authorized) > 0 {
		pkgLog().Info(fmt.Sprintf("[GoogleChat] %d per-user OAuth tokens on disk: %s", len(authorized), strings.Join(authorized, ", ")))
	} else if creds == nil {
		pkgLog().Info("[GoogleChat] No user OAuth tokens at setup — file attachments will degrade to text-only fallback. " +
			"Each user runs /setup-files once in their own DM to enable native attachments.")
	}
}

// Verifies the subscription exists and the service account can read it
func (a *GoogleChatAdapter) checkSubscription(ctx context.Context, path string) (bool, error) {
	err := a.api.getSubscription(ctx, path)
	switch httpStatus(err) {
	case 0:
		if err == nil {
			return true, nil
		}
	case 404:
		a.SetFatalError(ctx, "subscription_not_found", "Pub/Sub subscription not found at configured path", false)
		return false, nil
	case 403:
		a.SetFatalError(ctx, "subscription_permission", "Service Account lacks roles/pubsub.subscriber on the subscription", false)
		return false, nil
	}
	return a.connectFailed(ctx, "[GoogleChat] subscription.get failed: %s", err, "subscription_check", true)
}

// Validates config, authenticates, starts the Pub/Sub pull and resolves the bot id
func (a *GoogleChatAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	a.mu.Lock()
	a.scopeCtx, a.shuttingDown = context.WithoutCancel(ctx), false
	a.mu.Unlock()
	projectID, subscriptionPath, err := a.validateConfig()
	if err != nil {
		return a.connectFailed(ctx, "[GoogleChat] Config validation failed: %s", err, "config_invalid", false)
	}
	api, err := a.loadSACredentials(ctx)
	if err != nil {
		return a.connectFailed(ctx, "[GoogleChat] Config validation failed: %s", err, "config_invalid", false)
	}
	a.mu.Lock()
	a.projectID, a.subscriptionPath, a.api = projectID, subscriptionPath, api
	a.mu.Unlock()
	a.loadUserOAuth(ctx)
	a.threadCounts.load()
	if subscriptionPath != "" {
		if ok, err := a.checkSubscription(ctx, subscriptionPath); !ok || err != nil {
			return ok, err
		}
	}
	botID := loadCachedBotID(ctx)
	if botID == "" {
		if botID = a.resolveBotUserID(ctx); botID != "" {
			saveCachedBotID(ctx, botID)
		} else {
			pkgLog().Info("[GoogleChat] bot_user_id not yet resolved; will resolve on first addedToSpace or member lookup")
		}
	}
	a.mu.Lock()
	a.botUserID = botID
	if subscriptionPath != "" {
		a.supervisor = gateway.SpawnAdapterTask(ctx, a.runSupervisor)
	}
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	inbound, sub := "http", "<none>"
	if subscriptionPath != "" {
		inbound, sub = "pubsub", "<redacted>"
	}
	shownProject, shownBot := projectID, botID
	if shownProject == "" {
		shownProject = "<unset>"
	}
	if shownBot == "" {
		shownBot = "<unresolved>"
	}
	pkgLog().Info(fmt.Sprintf("[GoogleChat] Connected; project=%s, inbound=%s, subscription=%s, bot_user_id=%s, flow_control(msgs=%d, bytes=%d)",
		shownProject, inbound, sub, shownBot, a.maxMessages, a.maxBytes))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops pulling, waits briefly for the supervisor and marks the adapter disconnected
func (a *GoogleChatAdapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	a.shuttingDown = true
	task := a.supervisor
	a.supervisor = nil
	a.inflightCond.Broadcast()
	a.mu.Unlock()
	if task != nil {
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		gateway.CancelTask(waitCtx, task)
		cancel()
	}
	a.MarkDisconnected(ctx)
	pkgLog().Info("[GoogleChat] Disconnected")
	return nil
}

// Pulls from the subscription with exponential backoff and full jitter, fatal after the attempt budget
func (a *GoogleChatAdapter) runSupervisor(ctx context.Context) error {
	attempt := 0
	for {
		a.mu.Lock()
		stop := a.shuttingDown
		a.mu.Unlock()
		if stop || ctx.Err() != nil {
			return nil
		}
		err := a.pullOnce(ctx)
		if err == nil {
			if attempt > 0 {
				pkgLog().Info(fmt.Sprintf("[GoogleChat] Pub/Sub stream reconnected after %d attempts", attempt))
			}
			attempt = 0
			continue
		}
		if ctx.Err() != nil {
			return nil
		}
		switch httpStatus(err) {
		case 401:
			a.SetFatalError(ctx, "pubsub_auth", "Pub/Sub authentication failed; check service-account credentials and gateway logs", false)
			return nil
		case 403:
			a.SetFatalError(ctx, "pubsub_permission", "SA lacks pubsub.subscriber on the subscription", false)
			return nil
		}
		attempt++
		pkgLog().Warn(fmt.Sprintf("[GoogleChat] Pub/Sub stream died (attempt %d/%d): %s", attempt, maxReconnectAttempts, redactSensitive(err.Error())))
		if attempt >= maxReconnectAttempts {
			a.SetFatalError(ctx, "pubsub_reconnect_exhausted", fmt.Sprintf("Pub/Sub reconnect failed %d times; giving up", attempt), false)
			return nil
		}
		delay := min(reconnectMaxDelay, reconnectBaseDelay*float64(int(1)<<(attempt-1)))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Duration(rand.Float64() * delay * float64(time.Second))):
		}
	}
}

// One pull of up to the flow control message count, waiting first while dispatched bytes exceed the byte budget
func (a *GoogleChatAdapter) pullOnce(ctx context.Context) error {
	a.mu.Lock()
	for a.inflightBytes >= a.maxBytes && !a.shuttingDown && ctx.Err() == nil {
		a.inflightCond.Wait()
	}
	path := a.subscriptionPath
	a.mu.Unlock()
	messages, err := a.api.pull(ctx, path, max(a.maxMessages, 1))
	if err != nil {
		return err
	}
	for _, m := range messages {
		a.handlePubSubMessage(ctx, path, m)
	}
	return nil
}

// Parses one Pub/Sub envelope and hands it on, acked unless the adapter is shutting down
func (a *GoogleChatAdapter) handlePubSubMessage(ctx context.Context, path string, message pulledMessage) {
	a.mu.Lock()
	stopping := a.shuttingDown
	a.mu.Unlock()
	if stopping {
		_ = a.api.nack(ctx, path, message.AckID)
		return
	}
	ack := func() {
		if err := a.api.acknowledge(ctx, path, message.AckID); err != nil {
			pkgLog().Debug("[GoogleChat] ack failed", "error", redactSensitive(err.Error()))
		}
	}
	data, err := base64.StdEncoding.DecodeString(message.Message.Data)
	var envelope map[string]any
	if err == nil {
		err = json.Unmarshal(data, &envelope)
	}
	if err != nil || envelope == nil {
		pkgLog().Error("[GoogleChat] Could not parse Pub/Sub envelope", "error", err)
		ack()
		return
	}
	ceType := message.Message.Attributes["ce-type"]
	pkgLog().Debug(fmt.Sprintf("[GoogleChat] Envelope keys=%v, ce-type=%s", mapKeys(envelope), ceType))
	if a.debugRaw {
		dump, _ := json.Marshal(envelope)
		redacted := tools.RedactSensitiveTextWith(&tools.Env{Home: home.Dir(ctx)}, string(dump), tools.RedactOptions{Force: true})
		pkgLog().Debug("[GoogleChat] RAW envelope (redacted): " + pystr.Slice(redacted, 0, 2000))
	}
	switch {
	case strings.Contains(ceType, "membership") || strings.Contains(ceType, "MEMBERSHIP"):
		chat, _ := envelope["chat"].(map[string]any)
		mpl, _ := chat["membershipPayload"].(map[string]any)
		space, _ := mpl["space"].(map[string]any)
		spaceName := pyrepr.Str(orDefault(space["name"], "?"))
		if strings.Contains(ceType, "created") {
			membership, _ := mpl["membership"].(map[string]any)
			member, _ := membership["member"].(map[string]any)
			a.mu.Lock()
			unresolved := a.botUserID == ""
			if member["type"] == "BOT" && unresolved && pyrepr.Truthy(member["name"]) {
				a.botUserID = pyrepr.Str(member["name"])
				a.mu.Unlock()
				saveCachedBotID(ctx, pyrepr.Str(member["name"]))
			} else {
				a.mu.Unlock()
			}
			pkgLog().Info("[GoogleChat] ADDED_TO_SPACE " + spaceName)
		} else {
			pkgLog().Info("[GoogleChat] REMOVED_FROM_SPACE " + spaceName)
		}
	case strings.Contains(ceType, "widget") || strings.Contains(strings.ToLower(ceType), "card"):
		pkgLog().Info("[GoogleChat] Card/widget event ack'd")
	default:
		if msg, env, ok := a.prepareInbound(envelope, ceType, true); ok {
			size := len(data)
			a.mu.Lock()
			a.inflightBytes += size
			scope := a.scopeCtx
			a.mu.Unlock()
			if scope == nil {
				scope = ctx
			}
			go func() {
				defer func() {
					a.mu.Lock()
					a.inflightBytes -= size
					a.inflightCond.Broadcast()
					a.mu.Unlock()
				}()
				if err := a.dispatchMessage(scope, msg, env); err != nil {
					pkgLog().Error("[GoogleChat] Background inbound processing failed", "error", err)
				}
			}()
		}
	}
	ack()
}

// Map value or the default when missing or nil
func orDefault(v any, def any) any {
	if v == nil {
		return def
	}
	return v
}

// Keys of a map for debug logs
func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Message, space and format of an envelope, false for unknown or non message envelopes
func extractMessagePayload(envelope map[string]any) (map[string]any, map[string]any, string, bool) {
	if chat, ok := envelope["chat"].(map[string]any); ok {
		if wrapper, ok := chat["messagePayload"].(map[string]any); ok && len(wrapper) > 0 {
			msg, _ := wrapper["message"].(map[string]any)
			if msg == nil {
				msg = map[string]any{}
			}
			space, _ := wrapper["space"].(map[string]any)
			if len(space) == 0 {
				space, _ = msg["space"].(map[string]any)
			}
			return msg, space, "workspace_addons", true
		}
	}
	if msg, ok := envelope["message"].(map[string]any); ok {
		if pyrepr.Str(orDefault(envelope["type"], "")) != "MESSAGE" {
			return nil, nil, "", false
		}
		space, _ := envelope["space"].(map[string]any)
		if len(space) == 0 {
			space, _ = msg["space"].(map[string]any)
		}
		return msg, space, "native_chat_api", true
	}
	_, hasEventType := envelope["event_type"]
	_, hasSender := envelope["sender_email"]
	if !hasEventType && !hasSender {
		return nil, nil, "", false
	}
	if pyrepr.Str(orDefault(envelope["event_type"], "MESSAGE")) != "MESSAGE" {
		return nil, nil, "", false
	}
	senderEmail := pystr.Strip(pyrepr.Str(orEmpty(envelope["sender_email"])))
	text := pyrepr.Str(orEmpty(envelope["text"]))
	senderType := strings.ToUpper(pystr.Strip(pyrepr.Str(orDefault(nonEmpty(envelope["sender_type"]), "HUMAN"))))
	if senderType != "HUMAN" && senderType != "BOT" {
		senderType = "HUMAN"
	}
	surrogate := senderEmail
	if surrogate == "" {
		surrogate = "unknown"
	}
	surrogate = strings.ReplaceAll(strings.ReplaceAll(surrogate, "@", "_at_"), ".", "_")
	display := pyrepr.Str(orDefault(nonEmpty(envelope["sender_display_name"]), nil))
	if !pyrepr.Truthy(envelope["sender_display_name"]) {
		display = senderEmail
		if display == "" {
			display = "Unknown"
		}
	}
	msg := map[string]any{"name": pyrepr.Str(orEmpty(envelope["message_name"])),
		"sender": map[string]any{"name": "users/relay-" + surrogate, "email": senderEmail, "displayName": display, "type": senderType},
		"text":   text, "argumentText": text}
	if pyrepr.Truthy(envelope["thread_name"]) {
		msg["thread"] = map[string]any{"name": envelope["thread_name"]}
	}
	space := map[string]any{"name": pyrepr.Str(orEmpty(envelope["space_name"])), "spaceType": orDefault(envelope["space_type"], "SPACE")}
	return msg, space, "relay_flat", true
}

// Nil for falsy values so defaults apply
func nonEmpty(v any) any {
	if !pyrepr.Truthy(v) {
		return nil
	}
	return v
}

// Extracted, self filtered and de-duplicated inbound message and its envelope, both carrying the space
func (a *GoogleChatAdapter) prepareInbound(envelope map[string]any, ceType string, logDebug bool) (map[string]any, map[string]any, bool) {
	msg, space, _, ok := extractMessagePayload(envelope)
	if !ok {
		if logDebug {
			pkgLog().Debug(fmt.Sprintf("[GoogleChat] Envelope did not match a known message format; ce-type=%s, keys=%v", ceType, mapKeys(envelope)))
		}
		return nil, nil, false
	}
	if sender, _ := msg["sender"].(map[string]any); sender["type"] == "BOT" {
		return nil, nil, false
	}
	if name := pyrepr.Str(orEmpty(msg["name"])); name != "" && a.dedup.IsDuplicate(name) {
		if logDebug {
			pkgLog().Debug("[GoogleChat] Dedup drop for " + name)
		}
		return nil, nil, false
	}
	msgCopy, envCopy := copyMap(msg), copyMap(envelope)
	for _, d := range []map[string]any{msgCopy, envCopy} {
		if _, has := d["space"]; !has && len(space) > 0 {
			d["space"] = space
		}
	}
	return msgCopy, envCopy, true
}

// Shallow copy of a map
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
