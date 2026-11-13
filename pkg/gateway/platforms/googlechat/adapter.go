package googlechat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	platformName         = "google_chat"
	maxTextLength        = 4000
	rateLimitWarnHits    = 5
	reconnectMaxAttempts = 10
	reconnectBaseDelay   = 2 * time.Second
	reconnectMaxDelay    = 120 * time.Second
	defaultMaxMessages   = 1
	defaultMaxBytes      = 16 * 1024 * 1024
	typingConsumed       = "<consumed>"
	botIDCacheFile       = "google_chat_bot_id.json"
	threadCountsFile     = "google_chat_thread_counts.json"
)

var subscriptionPathRe = regexp.MustCompile(`^projects/([^/]+)/subscriptions/([^/]+)$`)

// Google Chat bot over Pub/Sub pull or authenticated HTTP events in, Chat REST out, files uploaded as the asking user
type GoogleChatAdapter struct {
	*gateway.BasePlatformAdapter

	maxMessages        int
	maxBytes           int
	bootstrapSpaces    string
	debugRaw           bool
	httpEventsURL      string
	httpEventsAudience string
	httpEventsSAEmails string

	dedup        *gateway.MessageDeduplicator
	threadCounts *threadCountStore

	mu                sync.Mutex
	runCtx            context.Context
	stopRun           context.CancelFunc
	pullDone          chan struct{}
	shuttingDown      bool
	api               *googleAPI
	projectID         string
	subscription      string
	botUserID         string
	lastInboundThread map[string]string
	lastSenderByChat  map[string]string
	typingMessages    map[string]string
	typingInflight    map[string]chan struct{}
	orphanTyping      map[string][]string
	rateLimitHits     map[string]int
	userCreds         map[string]*userCredentials
	legacyCreds       *userCredentials
}

// Class attributes the Google Chat adapter declares over the base defaults
func adapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.MaxMessageLength = maxTextLength
	traits.SplitsLongMessages = true
	return traits
}

// Text of a config value, numbers and booleans in their plain form
func textOf(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// Whether a config value is set to something other than empty, zero or false
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case int:
		return t != 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

// Extra value when set, else the scoped env value, else the fallback, stripped
func settingString(ctx context.Context, extra map[string]any, key, env, fallback string) (string, error) {
	if v := strings.TrimSpace(textOf(extra[key])); truthy(extra[key]) && v != "" {
		return v, nil
	}
	if env != "" {
		v, err := gateway.GetScopedSecret(ctx, env, "", false)
		if err != nil {
			return "", err
		}
		if v = strings.TrimSpace(v); v != "" {
			return v, nil
		}
	}
	return strings.TrimSpace(fallback), nil
}

// Integer setting from extra then the scoped env, the default when unset or not an integer
func settingInt(ctx context.Context, extra map[string]any, key, env string, def int) (int, error) {
	raw, err := settingString(ctx, extra, key, env, strconv.Itoa(def))
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def, nil
	}
	return n, nil
}

// Adapter reading its flow control, bootstrap, debug and HTTP event settings from extra and the profile scoped env
func NewGoogleChatAdapter(ctx context.Context, config *gateway.PlatformConfig) (*GoogleChatAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &GoogleChatAdapter{dedup: gateway.NewMessageDeduplicator(gateway.DefaultDedupMaxSize, gateway.DefaultDedupTTLSeconds),
		threadCounts: &threadCountStore{path: filepath.Join(home.Dir(ctx), threadCountsFile)}, lastInboundThread: map[string]string{},
		lastSenderByChat: map[string]string{}, typingMessages: map[string]string{}, typingInflight: map[string]chan struct{}{},
		orphanTyping: map[string][]string{}, rateLimitHits: map[string]int{}, userCreds: map[string]*userCredentials{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.Platform(platformName), a)
	a.Traits = adapterTraits()
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	var err error
	if a.maxMessages, err = settingInt(ctx, extra, "max_messages", "GOOGLE_CHAT_MAX_MESSAGES", defaultMaxMessages); err != nil {
		return nil, err
	}
	if a.maxBytes, err = settingInt(ctx, extra, "max_bytes", "GOOGLE_CHAT_MAX_BYTES", defaultMaxBytes); err != nil {
		return nil, err
	}
	if a.bootstrapSpaces, err = settingString(ctx, extra, "bootstrap_spaces", "GOOGLE_CHAT_BOOTSTRAP_SPACES", ""); err != nil {
		return nil, err
	}
	debug, err := gateway.GetScopedSecret(ctx, "GOOGLE_CHAT_DEBUG_RAW", "", false)
	if err != nil {
		return nil, err
	}
	a.debugRaw = truthy(extra["debug_raw"]) || debug != ""
	if a.httpEventsURL, err = settingString(ctx, extra, "http_events_url", "GOOGLE_CHAT_HTTP_EVENTS_URL", ""); err != nil {
		return nil, err
	}
	if a.httpEventsAudience, err = settingString(ctx, extra, "http_events_audience", "GOOGLE_CHAT_HTTP_EVENTS_AUDIENCE", a.httpEventsURL); err != nil {
		return nil, err
	}
	emails, err := settingString(ctx, extra, "http_events_service_account_email", "GOOGLE_CHAT_HTTP_EVENTS_SERVICE_ACCOUNT_EMAIL", "")
	if err != nil {
		return nil, err
	}
	a.httpEventsSAEmails = strings.ToLower(emails)
	return a, nil
}

// Project and subscription path, the subscription empty for HTTP inbound
func validateInbound(extra map[string]any) (string, string, error) {
	project := strings.TrimSpace(textOf(extra["project_id"]))
	subscription := strings.TrimSpace(textOf(extra["subscription_name"]))
	if subscription != "" {
		m := subscriptionPathRe.FindStringSubmatch(subscription)
		if m == nil {
			return "", "", errors.New("GOOGLE_CHAT_SUBSCRIPTION_NAME must match 'projects/<project>/subscriptions/<sub>'.")
		}
		if project != "" && m[1] != project {
			return "", "", errors.New("project_id in GOOGLE_CHAT_PROJECT_ID does not match the project embedded in GOOGLE_CHAT_SUBSCRIPTION_NAME.")
		}
		if project == "" {
			project = m[1]
		}
		return project, subscription, nil
	}
	if strings.TrimSpace(textOf(extra["http_events_url"])) != "" {
		return project, "", nil
	}
	if project == "" {
		return "", "", errors.New("GOOGLE_CHAT_PROJECT_ID (or GOOGLE_CLOUD_PROJECT) is not set.")
	}
	return "", "", errors.New("GOOGLE_CHAT_SUBSCRIPTION_NAME (or GOOGLE_CHAT_SUBSCRIPTION) is not set. " +
		"Set GOOGLE_CHAT_HTTP_EVENTS_URL for HTTP callback mode.")
}

// Service account credential failure classified for the connect and standalone messages
type credentialError struct {
	kind string
	err  error
	path string
}

func (e *credentialError) Error() string {
	switch e.kind {
	case "inline_invalid":
		return fmt.Sprintf("Inline SA JSON is not valid JSON: %v", e.err)
	case "not_found":
		return "Service Account JSON file not found at configured path."
	case "file_invalid":
		return fmt.Sprintf("Service Account JSON file is not valid JSON: %v", e.err)
	case "sa_invalid":
		return fmt.Sprintf("Service Account JSON is not a usable service account key: %v", e.err)
	case "adc_foreign":
		return "Google Chat ADC skipped for this profile: service-account credentials are set in the process environment " +
			"but not in this profile's secret scope. Set GOOGLE_CHAT_SERVICE_ACCOUNT_JSON in this profile's .env."
	}
	return "No Service Account credentials configured and Application Default Credentials are unavailable. Set " +
		fmt.Sprintf("GOOGLE_CHAT_SERVICE_ACCOUNT_JSON or run `gcloud auth application-default login`. ADC error: %v", e.err)
}

// Short detail of a credential failure for the standalone sender
func (e *credentialError) detail() string {
	switch e.kind {
	case "inline_invalid":
		return fmt.Sprintf("inline SA JSON is invalid: %v", e.err)
	case "not_found":
		return "SA JSON file not found at " + e.path
	case "file_invalid":
		return fmt.Sprintf("SA JSON file is invalid: %v", e.err)
	case "sa_invalid":
		return fmt.Sprintf("SA JSON is not a usable service account key: %v", e.err)
	case "adc_foreign":
		return "ADC skipped for this profile: service-account credentials are set in the process environment but not in this profile's secret scope"
	}
	return fmt.Sprintf("no SA credentials configured and Application Default Credentials are unavailable: %v", e.err)
}

// Service account credentials from a path or inline JSON, else Application Default Credentials
func loadServiceAccount(ctx context.Context, value string) (*googleCredentials, error) {
	if value == "" {
		if provider.IsMultiplexActive(ctx) && (os.Getenv("GOOGLE_CHAT_SERVICE_ACCOUNT_JSON") != "" || os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "") {
			return nil, &credentialError{kind: "adc_foreign"}
		}
		creds, err := defaultCredentials(ctx, chatScopes)
		if err != nil {
			return nil, &credentialError{kind: "adc_failed", err: err}
		}
		return creds, nil
	}
	var info map[string]any
	if strings.HasPrefix(strings.TrimLeft(value, " \t\r\n"), "{") {
		if err := json.Unmarshal([]byte(value), &info); err != nil {
			return nil, &credentialError{kind: "inline_invalid", err: err}
		}
	} else {
		raw, err := os.ReadFile(value)
		if err != nil {
			return nil, &credentialError{kind: "not_found", path: value}
		}
		if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &info); err != nil {
			return nil, &credentialError{kind: "file_invalid", err: err, path: value}
		}
	}
	creds, err := serviceAccountCredentials(info, chatScopes)
	if err != nil {
		return nil, &credentialError{kind: "sa_invalid", err: err, path: value}
	}
	return creds, nil
}

// Configured service account path or inline JSON from extra, then the scoped env
func serviceAccountValue(ctx context.Context, extra map[string]any, envNames ...string) (string, error) {
	if v := strings.TrimSpace(textOf(extra["service_account_json"])); v != "" {
		return v, nil
	}
	for _, name := range envNames {
		v, err := gateway.GetScopedSecret(ctx, name, "", false)
		if err != nil {
			return "", err
		}
		if v = strings.TrimSpace(v); v != "" {
			return v, nil
		}
	}
	return "", nil
}

// Records a redacted connect failure as fatal and answers the false connect result
func (a *GoogleChatAdapter) connectFailed(ctx context.Context, logPrefix string, err error, code string, retryable bool) bool {
	msg := redact(err.Error())
	slog.Error(fmt.Sprintf("[GoogleChat] %s: %s", logPrefix, msg))
	a.SetFatalError(ctx, code, msg, retryable)
	return false
}

// Validates config, authenticates, probes the subscription, resolves the bot id and starts the Pub/Sub pull
func (a *GoogleChatAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	extra := a.Config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	project, subscription, err := validateInbound(extra)
	if err != nil {
		return a.connectFailed(ctx, "Config validation failed", err, "config_invalid", false), nil
	}
	saValue, err := serviceAccountValue(ctx, extra, "GOOGLE_APPLICATION_CREDENTIALS")
	if err != nil {
		return false, err
	}
	creds, err := loadServiceAccount(ctx, saValue)
	if err != nil {
		return a.connectFailed(ctx, "Config validation failed", err, "config_invalid", false), nil
	}
	if saValue == "" {
		slog.Info("[GoogleChat] No SA JSON configured; using Application Default Credentials")
	}
	if subscription != "" {
		sum := sha256.Sum256([]byte(subscription))
		locked, err := a.AcquirePlatformLock(ctx, platformName, hex.EncodeToString(sum[:])[:16], "Google Chat Pub/Sub subscription")
		if err != nil || !locked {
			return false, err
		}
	}
	runCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	api := &googleAPI{client: gateway.TrustEnvHTTPClient(ctx, 0), auth: creds}
	a.mu.Lock()
	a.runCtx, a.stopRun, a.shuttingDown = runCtx, stop, false
	a.api, a.projectID, a.subscription = api, project, subscription
	a.mu.Unlock()
	a.loadUserOAuth(ctx)
	a.threadCounts.load()
	if subscription != "" {
		if err := api.getSubscription(ctx, subscription); err != nil {
			stop()
			a.ReleasePlatformLock()
			switch errStatus(err) {
			case 404:
				a.SetFatalError(ctx, "subscription_not_found", "Pub/Sub subscription not found at configured path", false)
				return false, nil
			case 403:
				a.SetFatalError(ctx, "subscription_permission", "Service Account lacks roles/pubsub.subscriber on the subscription", false)
				return false, nil
			}
			return a.connectFailed(ctx, "subscription.get failed", err, "subscription_check", true), nil
		}
	}
	botID := a.loadCachedBotID(ctx)
	if botID == "" {
		if botID = a.resolveBotUserID(ctx); botID != "" {
			a.saveCachedBotID(ctx, botID)
		} else {
			slog.Info("[GoogleChat] bot_user_id not yet resolved; will resolve on first addedToSpace or member lookup")
		}
	}
	a.mu.Lock()
	a.botUserID = botID
	if subscription != "" {
		done := make(chan struct{})
		a.pullDone = done
		go func() {
			defer close(done)
			a.runSupervisor(runCtx)
		}()
	}
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	inbound, subShown, botShown := "http", "<none>", botID
	if subscription != "" {
		inbound, subShown = "pubsub", "<redacted>"
	}
	if botShown == "" {
		botShown = "<unresolved>"
	}
	if project == "" {
		project = "<unset>"
	}
	slog.Info(fmt.Sprintf("[GoogleChat] Connected; project=%s, inbound=%s, subscription=%s, bot_user_id=%s, flow_control(msgs=%d, bytes=%d)",
		project, inbound, subShown, botShown, a.maxMessages, a.maxBytes))
	a.WirePluginHandlers(ctx, nil)
	registerLive(a, home.Dir(ctx))
	return true, nil
}

// Stops the Pub/Sub pull, waits for it to drain and releases the subscription lock
func (a *GoogleChatAdapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	a.shuttingDown = true
	stop, done := a.stopRun, a.pullDone
	a.pullDone = nil
	a.mu.Unlock()
	unregisterLive(a)
	if stop != nil {
		stop()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			slog.Warn("[GoogleChat] Pub/Sub pull did not stop within 15s")
		}
	}
	a.MarkDisconnected(ctx)
	a.ReleasePlatformLock()
	slog.Info("[GoogleChat] Disconnected")
	return nil
}

// Connected service account client, nil before connect
func (a *GoogleChatAdapter) client() *googleAPI {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.api
}

// Loads the legacy single user grant and logs the per user grants on disk, failures only costing native attachments
func (a *GoogleChatAdapter) loadUserOAuth(ctx context.Context) {
	legacy := loadUserCredentials(ctx, "")
	a.mu.Lock()
	a.legacyCreds = legacy
	a.mu.Unlock()
	if legacy != nil {
		slog.Info("[GoogleChat] Legacy user OAuth loaded — fallback attachment delivery enabled")
	}
	if authorized := listAuthorizedEmails(ctx); len(authorized) > 0 {
		slog.Info(fmt.Sprintf("[GoogleChat] %d per-user OAuth tokens on disk: %s", len(authorized), strings.Join(authorized, ", ")))
	} else if legacy == nil {
		slog.Info("[GoogleChat] No user OAuth tokens at setup — file attachments will degrade to text-only fallback. " +
			"Each user runs /setup-files once in their own DM to enable native attachments.")
	}
}

// Bot user id from the per profile cache file
func (a *GoogleChatAdapter) loadCachedBotID(ctx context.Context) string {
	raw, err := os.ReadFile(filepath.Join(home.Dir(ctx), botIDCacheFile))
	if err != nil {
		return ""
	}
	var data struct {
		BotUserID string `json:"bot_user_id"`
	}
	if json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), &data) != nil {
		return ""
	}
	return data.BotUserID
}

// Persists the bot user id in the per profile cache file
func (a *GoogleChatAdapter) saveCachedBotID(ctx context.Context, botID string) {
	raw, _ := json.Marshal(map[string]string{"bot_user_id": botID})
	if err := writeFileAtomic(ctx, filepath.Join(home.Dir(ctx), botIDCacheFile), raw, 0o644, 0o700); err != nil {
		slog.Debug(fmt.Sprintf("[GoogleChat] Could not persist bot_user_id cache: %v", err))
	}
}

// Bot users/{id} from the memberships of the home channel then the bootstrap spaces, empty when none answers
func (a *GoogleChatAdapter) resolveBotUserID(ctx context.Context) string {
	var spaces []string
	if hc := a.Config.HomeChannel; hc != nil && hc.ChatID != "" {
		spaces = append(spaces, hc.ChatID)
	}
	for _, s := range strings.Split(a.bootstrapSpaces, ",") {
		if s = strings.TrimSpace(s); s != "" {
			spaces = append(spaces, s)
		}
	}
	api := a.client()
	for _, space := range spaces {
		members, err := api.listMembers(ctx, space)
		if err != nil {
			slog.Debug(fmt.Sprintf("[GoogleChat] members.list failed on %s: %s", space, redact(err.Error())))
			continue
		}
		for _, m := range members {
			if m.Type == "BOT" && m.Name != "" {
				return m.Name
			}
		}
	}
	return ""
}

// Records the bot id from an added to space membership event and clears a forbidden send fault
func (a *GoogleChatAdapter) noteAddedToSpace(ctx context.Context, member *chatUser) {
	if member != nil && member.Type == "BOT" && member.Name != "" {
		a.mu.Lock()
		unset := a.botUserID == ""
		if unset {
			a.botUserID = member.Name
		}
		a.mu.Unlock()
		if unset {
			a.saveCachedBotID(ctx, member.Name)
		}
	}
	if code := a.FatalErrorCode(); code != nil && *code == "chat_forbidden" {
		slog.Info("[GoogleChat] Re-added to a space; clearing the forbidden send fault")
		a.MarkConnected(ctx, "")
	}
}

// Chat name, dm or group type and id of a space, a group of the id itself when the lookup fails
func (a *GoogleChatAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	api := a.client()
	if api == nil {
		return map[string]any{"name": chatID, "type": "group", "chat_id": chatID}, nil
	}
	space, err := api.getSpace(ctx, chatID)
	if err != nil {
		slog.Debug(fmt.Sprintf("[GoogleChat] get_chat_info failed: %s", redact(err.Error())))
		return map[string]any{"name": chatID, "type": "group", "chat_id": chatID}, nil
	}
	name := space.DisplayName
	if name == "" {
		name = chatID
	}
	return map[string]any{"name": name, "type": chatTypeOf(space), "chat_id": chatID}, nil
}

// dm for direct message spaces, group for everything else
func chatTypeOf(space *chatSpace) string {
	switch space.kind() {
	case "DIRECT_MESSAGE", "DM":
		return "dm"
	}
	return "group"
}

// Markdown converted to the Google Chat dialect
func (a *GoogleChatAdapter) FormatMessage(content string) string { return formatMessage(content) }

// Connected adapters of this process by profile home, the standalone sender delegating media to them
var live = struct {
	sync.Mutex
	byHome map[string]*GoogleChatAdapter
}{byHome: map[string]*GoogleChatAdapter{}}

func registerLive(a *GoogleChatAdapter, homeDir string) {
	live.Lock()
	live.byHome[homeDir] = a
	live.Unlock()
}

func unregisterLive(a *GoogleChatAdapter) {
	live.Lock()
	for k, v := range live.byHome {
		if v == a {
			delete(live.byHome, k)
		}
	}
	live.Unlock()
}

// Connected adapter of the profile home, nil when none runs in this process
func liveAdapter(homeDir string) *GoogleChatAdapter {
	live.Lock()
	defer live.Unlock()
	return live.byHome[homeDir]
}
