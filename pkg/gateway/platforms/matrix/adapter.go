package matrix

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const processedEventsMax = 1000

// Goroutine the adapter can cancel and wait for
type bgTask struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Starts fn on a goroutine under a cancellable child of ctx
func startTask(ctx context.Context, fn func(ctx context.Context)) *bgTask {
	tctx, cancel := context.WithCancel(ctx)
	t := &bgTask{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(t.done)
		fn(tctx)
	}()
	return t
}

// Whether the task finished
func (t *bgTask) isDone() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Gateway adapter for Matrix on any homeserver
type MatrixAdapter struct {
	*gateway.BasePlatformAdapter

	mu sync.Mutex

	ctx              context.Context
	maxMessageLength int
	splitThreshold   int

	homeserver         string
	accessToken        string
	userID             string
	password           string
	e2eeMode           string
	encryption         bool
	deviceID           string
	deviceIDUnverified bool

	client     *mautrix.Client
	stateStore mautrix.StateStore
	cryptoDB   io.Closer
	crypto     e2eeMachine
	storeDir   string

	syncTask        *bgTask
	inviteJoinTasks map[string]*bgTask
	closing         bool
	startupTS       float64
	lateGraceDrops  int
	lateGraceSkew   float64
	clockSkewWarned bool
	lastSyncTS      float64

	dmRooms                  map[string]bool
	roomIdentities           map[string]*MatrixRoomIdentity
	roomIdentityCachedAt     map[string]float64
	roomIdentityTTLSeconds   float64
	roomIdentityCacheMax     int
	joinedRooms              map[string]bool
	processedEvents          []string
	processedEventsSet       map[string]bool
	warnedEncryptedDropRooms map[string]bool

	threads              *gateway.ThreadParticipationTracker
	parkedVoices         *ParkedVoices
	requireMention       bool
	threadRequireMention bool
	freeRooms            map[string]bool
	allowedRooms         map[string]bool
	allowRoomMentions    bool
	autoThread           bool
	dmAutoThread         bool
	dmMentionThreads     bool
	matrixSessionScope   string
	processNotices       bool
	reactionsEnabled     bool

	pendingReactions              map[[2]string]string
	reactionRedactionDelaySeconds float64
	reactionRedactionTasks        map[*bgTask]struct{}
	proxyURL                      string
	maxMediaBytes                 int64

	approvalReactionMap        map[string]string
	approvalPromptsByEvent     map[string]*matrixApprovalPrompt
	approvalPromptBySession    map[string]string
	approvalRequireSender      bool
	approvalTimeoutSeconds     int64
	modelPickerPromptsByEvent  map[string]*matrixPickerPrompt
	choicePickerPromptsByEvent map[string]*matrixPickerPrompt

	allowedUserIDs      map[string]bool
	allowedRoomIDs      map[string]bool
	ignoredUserPatterns []*regexp2.Regexp
}

// Profile home directory of the store holding E2EE keys and sync state
func storeDirFor(ctx context.Context) string {
	return home.HermesDir(home.Dir(ctx), "platforms/matrix/store", "matrix/store")
}

// Store directory pinned to the active profile and cached so later reads report the store in use
func (a *MatrixAdapter) resolveStoreDir(ctx context.Context) string {
	dir := storeDirFor(ctx)
	a.mu.Lock()
	a.storeDir = dir
	a.mu.Unlock()
	return dir
}

// Path of the SQLite crypto store under the pinned or profile store directory
func (a *MatrixAdapter) cryptoDBPath() string {
	a.mu.Lock()
	dir := a.storeDir
	a.mu.Unlock()
	if dir == "" {
		dir = storeDirFor(a.ctx)
	}
	return filepath.Join(dir, "crypto.db")
}

// Adapter for one Matrix account resolved from config and the profile scoped env
func NewMatrixAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (*MatrixAdapter, error) {
	if cfg.Extra == nil {
		cfg.Extra = map[string]any{}
	}
	a := &MatrixAdapter{ctx: ctx}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformMatrix, a)
	a.maxMessageLength = resolveMaxMessageLength(ctx, cfg)
	a.Traits.MaxMessageLength = a.maxMessageLength
	a.Traits.SerialReceiveLoop = true
	a.Traits.SupportsCodeBlocks = true
	a.Traits.SplitsLongMessages = true
	a.Traits.TypedCommandPrefix = "!"
	a.Traits.EACmdBudget = 2000
	a.splitThreshold = max(100, a.maxMessageLength-100)
	scoped := func(name string) string {
		v, _ := gateway.GetScopedSecret(ctx, name, "", false)
		return pystr.Strip(v)
	}
	a.homeserver = strings.TrimRight(extraStr(cfg.Extra, "homeserver", scoped("MATRIX_HOMESERVER")), "/")
	a.accessToken = tokenOf(cfg.Token)
	if a.accessToken == "" {
		a.accessToken = scoped("MATRIX_ACCESS_TOKEN")
	}
	a.userID = extraStr(cfg.Extra, "user_id", scoped("MATRIX_USER_ID"))
	a.password = extraStr(cfg.Extra, "password", scoped("MATRIX_PASSWORD"))
	a.e2eeMode = resolveE2EEMode(ctx, cfg.Extra)
	a.encryption = a.e2eeMode != "off"
	a.deviceID = extraStr(cfg.Extra, "device_id", scoped("MATRIX_DEVICE_ID"))
	a.inviteJoinTasks = map[string]*bgTask{}
	a.resetClockSkewDetector()
	a.dmRooms = map[string]bool{}
	a.roomIdentities = map[string]*MatrixRoomIdentity{}
	a.roomIdentityCachedAt = map[string]float64{}
	a.roomIdentityTTLSeconds = envFloat(ctx, "MATRIX_ROOM_IDENTITY_TTL_SECONDS", 60.0)
	a.roomIdentityCacheMax = matrixRoomIdentityCacheMaximum
	a.joinedRooms = map[string]bool{}
	a.processedEventsSet = map[string]bool{}
	a.warnedEncryptedDropRooms = map[string]bool{}
	a.threads = gateway.NewThreadParticipationTracker(ctx, "matrix", 500)
	a.parkedVoices = NewParkedVoices()
	a.requireMention = parseRequireMention(ctx, cfg)
	a.threadRequireMention = parseThreadRequireMention(ctx, cfg)
	a.freeRooms = extraCSVSet(ctx, cfg, "free_response_rooms", "MATRIX_FREE_RESPONSE_ROOMS")
	a.allowedRooms = extraCSVSet(ctx, cfg, "allowed_rooms", "MATRIX_ALLOWED_ROOMS")
	a.allowRoomMentions = envTruthy(ctx, "MATRIX_ALLOW_ROOM_MENTIONS", "false")
	a.autoThread = extraTruthy(ctx, cfg, "auto_thread", "MATRIX_AUTO_THREAD", "true")
	a.dmAutoThread = envTruthy(ctx, "MATRIX_DM_AUTO_THREAD", "false")
	a.dmMentionThreads = extraTruthy(ctx, cfg, "dm_mention_threads", "MATRIX_DM_MENTION_THREADS", "false")
	rawScope, _ := gateway.ExtraOrSecret(ctx, cfg.Extra, "session_scope", "MATRIX_SESSION_SCOPE", "auto", true)
	a.matrixSessionScope = pystr.Lower(pystr.Strip(pyrepr.Str(rawScope)))
	if a.matrixSessionScope != "auto" && a.matrixSessionScope != "room" && a.matrixSessionScope != "thread" {
		a.matrixSessionScope = "auto"
	}
	a.processNotices = extraTruthy(ctx, cfg, "process_notices", "MATRIX_PROCESS_NOTICES", "false")
	rawReactions, _ := gateway.ExtraOrSecret(ctx, cfg.Extra, "reactions", "MATRIX_REACTIONS", "true", true)
	switch pystr.Lower(pyrepr.Str(rawReactions)) {
	case "false", "0", "no":
	default:
		a.reactionsEnabled = true
	}
	a.pendingReactions = map[[2]string]string{}
	a.reactionRedactionDelaySeconds = 5.0
	a.reactionRedactionTasks = map[*bgTask]struct{}{}
	proxyURL, err := gateway.ResolveProxyURL(ctx, "MATRIX_PROXY", nil, "")
	if err != nil {
		return nil, err
	}
	a.proxyURL = proxyURL
	if a.proxyURL != "" {
		slog.Info(fmt.Sprintf("Matrix: proxy configured — %s", a.proxyURL))
	}
	a.maxMediaBytes = envInt(ctx, "MATRIX_MAX_MEDIA_BYTES", matrixDefaultMaxMediaBytes)
	delay, err := pyint.ParseFloat(envOr("ICEBATH_MATRIX_TEXT_BATCH_DELAY_SECONDS", "0.6"))
	if err != nil {
		return nil, err
	}
	split, err := pyint.ParseFloat(envOr("ICEBATH_MATRIX_TEXT_BATCH_SPLIT_DELAY_SECONDS", "2.0"))
	if err != nil {
		return nil, err
	}
	a.SetTextBatchDelays(delay, split)
	a.approvalReactionMap = map[string]string{
		"✅": "once", "\U0001f300": "session", "♾️": "always", "♾": "always",
		"❌": "deny", "❎": "deny"}
	a.approvalPromptsByEvent = map[string]*matrixApprovalPrompt{}
	a.approvalPromptBySession = map[string]string{}
	a.approvalRequireSender = envTruthy(ctx, "MATRIX_APPROVAL_REQUIRE_SENDER", "true")
	a.approvalTimeoutSeconds = envInt(ctx, "MATRIX_APPROVAL_TIMEOUT_SECONDS", 300)
	a.modelPickerPromptsByEvent = map[string]*matrixPickerPrompt{}
	a.choicePickerPromptsByEvent = map[string]*matrixPickerPrompt{}
	a.allowedUserIDs = extraCSVSet(ctx, cfg, "allowed_users", "MATRIX_ALLOWED_USERS")
	a.allowedRoomIDs = map[string]bool{}
	for r := range a.allowedRooms {
		a.allowedRoomIDs[r] = true
	}
	rawPatterns, _ := gateway.ExtraOrSecret(ctx, cfg.Extra, "ignore_user_patterns", "MATRIX_IGNORE_USER_PATTERNS", "", true)
	for _, pattern := range sortedKeys(csvSet(rawPatterns)) {
		re, err := pyre.Compile(pattern)
		if err != nil {
			slog.Warn(fmt.Sprintf("Matrix: ignoring invalid MATRIX_IGNORE_USER_PATTERNS entry %s: %v", pyrepr.Repr(pattern), err))
			continue
		}
		a.ignoredUserPatterns = append(a.ignoredUserPatterns, re)
	}
	return a, nil
}

// Configured token as a string, empty when unset
func tokenOf(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	return strOf(v)
}

// Process env value or the default when unset
func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// Config extra string, the fallback when it is missing or empty
func extraStr(extra map[string]any, key, fallback string) string {
	if v := extra[key]; pyTruthy(v) {
		return strOf(v)
	}
	return fallback
}

// Whether the event id was already processed, tracking it otherwise with the newest thousand kept
func (a *MatrixAdapter) isDuplicateEvent(eventID string) bool {
	if eventID == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.processedEventsSet[eventID] {
		return true
	}
	if len(a.processedEvents) == processedEventsMax {
		delete(a.processedEventsSet, a.processedEvents[0])
		a.processedEvents = a.processedEvents[1:]
	}
	a.processedEvents = append(a.processedEvents, eventID)
	a.processedEventsSet[eventID] = true
	return false
}

// Scoped env then config extra then the default, with true, 1 or yes semantics
func extraTruthy(ctx context.Context, cfg *gateway.PlatformConfig, key, envName, def string) bool {
	configured, _ := gateway.ExtraOrSecret(ctx, cfg.Extra, key, envName, def, true)
	if b, ok := configured.(bool); ok {
		return b
	}
	switch pystr.Lower(pyrepr.Str(configured)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// YAML bool or off style string from config extra, nil when unset
func configuredBool(cfg *gateway.PlatformConfig, key string) *bool {
	configured, ok := cfg.Extra[key]
	if !ok || configured == nil {
		return nil
	}
	var out bool
	switch v := configured.(type) {
	case bool:
		out = v
	case string:
		switch pystr.Lower(v) {
		case "false", "0", "no", "off":
		default:
			out = true
		}
	default:
		out = pyTruthy(v)
	}
	return &out
}

// Whether a configured value is anything other than a false or off spelling
func notFalseSpelling(configured any) bool {
	if b, ok := configured.(bool); ok {
		return b
	}
	switch pystr.Lower(pyrepr.Str(configured)) {
	case "false", "0", "no", "off":
		return false
	}
	return true
}

// Require mention from the scoped env, then config extra, then true
func parseRequireMention(ctx context.Context, cfg *gateway.PlatformConfig) bool {
	configured, _ := gateway.ExtraOrSecret(ctx, cfg.Extra, "require_mention", "MATRIX_REQUIRE_MENTION", true, true)
	return notFalseSpelling(configured)
}

// Thread require mention from the scoped env, then config extra, then false
func parseThreadRequireMention(ctx context.Context, cfg *gateway.PlatformConfig) bool {
	configured, _ := gateway.ExtraOrSecret(ctx, cfg.Extra, "thread_require_mention", "MATRIX_THREAD_REQUIRE_MENTION", false, true)
	return notFalseSpelling(configured)
}

// UTF-8 bytes, the unit of the homeserver event cap
func (a *MatrixAdapter) MessageLenFn() func(string) int { return utf8Len }

// HTTP client whose proxy applies to every request, the env proxy honored when no proxy is configured
func newMatrixHTTPClient(ctx context.Context, proxyURL string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	switch {
	case proxyURL == "":
		if gateway.GatewayTrustEnv(ctx) {
			transport.Proxy = http.ProxyFromEnvironment
		} else {
			transport.Proxy = nil
		}
	case strings.HasPrefix(pystr.Lower(strings.SplitN(proxyURL, "://", 2)[0]), "socks"):
		connector, err := gateway.NewProxyConnector(proxyURL)
		if err != nil {
			return nil, err
		}
		transport = connector.Transport()
	default:
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, err
		}
		transport.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: transport, Timeout: 180 * time.Second}, nil
}

// Room name and dm or group type of the chat
func (a *MatrixAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	identity := a.resolveRoomIdentity(ctx, chatID, false)
	kind := "group"
	if identity.ChatType == "dm" {
		kind = "dm"
	}
	return map[string]any{"name": identity.DisplayName, "type": kind}, nil
}

// Redacted health snapshot of auth, sync, E2EE, policy and media settings
func (a *MatrixAdapter) GetDiagnostics(ctx context.Context) map[string]any {
	now := nowSeconds()
	a.mu.Lock()
	defer a.mu.Unlock()
	tokenPresent := a.accessToken != ""
	userID := a.userID
	if userID == "" && a.client != nil {
		userID = string(a.client.UserID)
	}
	deviceID := a.deviceID
	if deviceID == "" && a.client != nil {
		deviceID = string(a.client.DeviceID)
	}
	preview := func(present bool) string {
		if present {
			return "***"
		}
		return ""
	}
	var lastSyncAge any
	if a.lastSyncTS != 0 {
		lastSyncAge = max(0.0, now-a.lastSyncTS)
	}
	dir := a.storeDir
	if dir == "" {
		dir = storeDirFor(a.ctx)
	}
	return map[string]any{
		"platform": "matrix", "homeserver": a.homeserver,
		"auth": map[string]any{
			"access_token_present": tokenPresent, "password_present": a.password != "",
			"token_preview": preview(tokenPresent), "user_id": userID,
			"device_id_present": deviceID != "", "device_id_preview": preview(pystr.Strip(deviceID) != "")},
		"sync": map[string]any{
			"connected": a.client != nil, "joined_room_count": len(a.joinedRooms),
			"last_sync_age_seconds": lastSyncAge},
		"e2ee": map[string]any{
			"mode": a.e2eeMode, "enabled": a.encryption, "deps_available": checkE2EEDeps(),
			"crypto_store_path":       filepath.Join(dir, "crypto.db"),
			"recovery_key_configured": pystr.Strip(scopedRecoveryKey(ctx)) != ""},
		"policy": map[string]any{
			"allowed_user_count": len(a.allowedUserIDs), "allowed_room_count": len(a.allowedRoomIDs),
			"ignored_user_pattern_count": len(a.ignoredUserPatterns),
			"require_mention":            a.requireMention, "free_response_room_count": len(a.freeRooms),
			"allow_room_mentions": a.allowRoomMentions, "process_notices": a.processNotices,
			"allow_public_rooms": envTruthy(ctx, "MATRIX_ALLOW_PUBLIC_ROOMS", "")},
		"media": map[string]any{"max_media_bytes": a.maxMediaBytes}}
}

// Sets the typing notification with the timeout in milliseconds, errors swallowed
func (a *MatrixAdapter) setTyping(ctx context.Context, chatID string, timeout int) {
	client := a.currentClient()
	if client == nil {
		return
	}
	_, _ = client.UserTyping(ctx, id.RoomID(chatID), timeout > 0, time.Duration(timeout)*time.Millisecond)
}

// Typing indicator for thirty seconds
func (a *MatrixAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.setTyping(ctx, chatID, 30000)
	return nil
}

// Clears the typing indicator
func (a *MatrixAdapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.setTyping(ctx, chatID, 0)
	return nil
}

// Connected client, nil when disconnected
func (a *MatrixAdapter) currentClient() *mautrix.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client
}

// Wall clock seconds since the epoch
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Keys of a set in sorted order
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Whether E2EE is still enabled, optional mode turning it off when setup degrades
func (a *MatrixAdapter) encryptionOn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.encryption
}

// Quiet period before a pending batch dispatches, chunks near the outbound limit waiting the split delay
func (a *MatrixAdapter) TextBatchDelayFor(pending *gateway.MessageEvent) float64 {
	lastLen := 0
	if pending != nil {
		lastLen = pending.LastChunkLen()
	}
	delay, split := a.TextBatchDelays()
	if lastLen >= a.splitThreshold {
		return split
	}
	return delay
}
