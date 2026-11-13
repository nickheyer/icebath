package buzz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Platform value of the Buzz gateway adapter
const platformBuzz = gateway.Platform("buzz")

const (
	chatKind            = 9
	fetchLimit          = 50
	dmDiscoveryEvery    = 5
	memberCacheTTL      = 60 * time.Second
	profileNameTTL      = 300 * time.Second
	threadRootCacheSize = 512
)

// Chat messages plus forum posts and comments, the kinds that carry conversation content
var dispatchKinds = map[int64]bool{chatKind: true, 45001: true, 45003: true}

// One cached value stamped with the time it was fetched
type ttlEntry[V any] struct {
	at    time.Time
	value V
}

// Buzz relay adapter driving the buzz CLI, fed by an authenticated NIP-42 subscription or by polling
type Adapter struct {
	*gateway.BasePlatformAdapter

	extra             map[string]any
	relayURL          string
	cliPath           string
	channels          []string
	pollInterval      time.Duration
	requireMention    bool
	replyToMode       string
	transport         string
	allowedPubkeys    map[string]bool
	reactionOnly      map[string]bool
	attachmentOrigins map[origin]bool

	mu               sync.Mutex
	privateKey       string
	authTag          string
	selfPubkey       string
	selfNpub         string
	displayName      string
	state            *orderedMap[*channelState]
	restricted       map[string]bool
	restoredCursors  map[string]savedCursor
	channelNames     map[string]string
	channelMeta      map[string]map[string]any
	userNames        map[string]string
	memberCache      map[string]ttlEntry[[]string]
	profileNameCache map[string]ttlEntry[string]
	threadRoots      *orderedMap[string]
	membershipSince  int64
	pollCount        int

	discoverMu sync.Mutex
	taskMu     sync.Mutex
	pollTask   *gateway.AdapterTask
	wsTask     *gateway.AdapterTask

	cursorWriteMu   sync.Mutex
	cursorSnapshots int
	cursorWritten   int
}

// Identity of this agent on the relay
type identity struct {
	pubkey, npub, displayName string
}

// Adapter reading its settings from the profile scoped env and the platform extra, env winning outside secondary profiles
func NewAdapter(ctx context.Context, config *gateway.PlatformConfig) (*Adapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &Adapter{}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, platformBuzz, a)
	a.Traits.SerialReceiveLoop = true
	a.extra = config.Extra
	if a.extra == nil {
		a.extra = map[string]any{}
	}
	s := &settings{ctx: ctx, extra: a.extra}
	a.relayURL = s.relayURL()
	a.attachmentOrigins = map[origin]bool{}
	for _, h := range splitCSV(a.extra["attachment_hosts"]) {
		if str, ok := h.(string); ok {
			if o, ok := attachmentOrigin(str); ok {
				a.attachmentOrigins[o] = true
			}
		}
	}
	if o, ok := attachmentOrigin(a.relayURL); ok {
		a.attachmentOrigins[o] = true
	}
	a.cliPath = s.cliPath()
	a.channels = s.channels()
	a.pollInterval = secondsDuration(s.pollInterval())
	a.requireMention = s.requireMention()
	a.replyToMode = s.replyToMode(config)
	a.transport = s.transport()
	a.allowedPubkeys = pubkeySet(s.or("BUZZ_ALLOWED_USERS", "allowed_users", []any{}))
	a.reactionOnly = pubkeySet(s.or("BUZZ_REACTION_ONLY_USERS", "reaction_only_users", []any{}))
	if s.err != nil {
		return nil, s.err
	}
	a.state = newOrderedMap[*channelState]()
	a.restricted = map[string]bool{}
	a.restoredCursors = map[string]savedCursor{}
	a.channelNames = map[string]string{}
	a.channelMeta = map[string]map[string]any{}
	a.userNames = map[string]string{}
	a.memberCache = map[string]ttlEntry[[]string]{}
	a.profileNameCache = map[string]ttlEntry[string]{}
	a.threadRoots = newOrderedMap[string]()
	return a, nil
}

// Duration of a seconds count, an unbounded count saturating
func secondsDuration(seconds float64) time.Duration {
	if seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(seconds * float64(time.Second))
}

// Reply threading mode the runner reads to decide whether progress bubbles thread
func (a *Adapter) ReplyToMode() string { return a.replyToMode }

// Hex form of an allowlist entry written as hex or npub, empty when it is neither
func (a *Adapter) NormalizeUserID(entry string) string { return normalizeUserRef(entry) }

// Snapshot of this agent's relay identity
func (a *Adapter) identity() identity {
	a.mu.Lock()
	defer a.mu.Unlock()
	return identity{a.selfPubkey, a.selfNpub, a.displayName}
}

// Runs one buzz CLI command with this adapter's relay and key, resolving the key on first use
func (a *Adapter) runCLI(ctx context.Context, args []string, input *string) (cliResult, error) {
	a.mu.Lock()
	key, tag := a.privateKey, a.authTag
	a.mu.Unlock()
	if key == "" {
		s := &settings{ctx: ctx, extra: a.extra}
		key = s.privateKey()
		var err error
		if tag, err = s.authTag(); err != nil {
			return cliResult{}, err
		}
		if s.err != nil {
			return cliResult{}, s.err
		}
		a.mu.Lock()
		a.privateKey, a.authTag = key, tag
		a.mu.Unlock()
	}
	return runBuzz(ctx, a.cliPath, args, a.relayURL, key, tag, input)
}

// Decoded stdout of a successful CLI run, the fallback for a failed run or malformed output
func (a *Adapter) cliJSON(ctx context.Context, args []string, fallback any) (any, error) {
	res, err := a.runCLI(ctx, args, nil)
	if err != nil {
		return nil, err
	}
	if res.code != 0 {
		return fallback, nil
	}
	return jsonOr(res.stdout, fallback), nil
}

// Logs and records a fatal connect error
func (a *Adapter) connectFailed(ctx context.Context, code, detail, log string, retryable bool) (bool, error) {
	slog.Error(log)
	a.SetFatalError(ctx, code, detail, retryable)
	return false, nil
}

// Verifies the relay identity, seeds every watched conversation and starts the inbound transport
func (a *Adapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	a.stopTransports(ctx)
	if a.relayURL == "" {
		return a.connectFailed(ctx, "config_missing", "BUZZ_RELAY_URL must be set", "Buzz: relay URL must be configured", false)
	}
	if a.cliPath == "" {
		return a.connectFailed(ctx, "cli_missing", "buzz CLI binary not found",
			"Buzz: buzz CLI binary not found (set BUZZ_CLI_PATH or put 'buzz' on PATH)", false)
	}
	s := &settings{ctx: ctx, extra: a.extra}
	key := s.privateKey()
	tag, err := s.authTag()
	var tagErr *authTagError
	if errors.As(err, &tagErr) {
		return a.connectFailed(ctx, "config_invalid", err.Error(), "Buzz: invalid owner-auth configuration — "+err.Error(), false)
	}
	if err != nil {
		return false, err
	}
	if s.err != nil {
		return false, s.err
	}
	a.mu.Lock()
	a.privateKey, a.authTag = key, tag
	a.mu.Unlock()
	if key == "" {
		return a.connectFailed(ctx, "config_missing", "BUZZ_PRIVATE_KEY must be set",
			"Buzz: no private key (set BUZZ_PRIVATE_KEY or a credentials file)", false)
	}
	res, err := a.runCLI(ctx, []string{"users", "get"}, nil)
	if err != nil {
		return false, err
	}
	if res.code != 0 {
		message := cliErrorMessage(res.stderr, res.code, "")
		return a.connectFailed(ctx, "connect_failed", message,
			fmt.Sprintf("Buzz: failed to fetch own profile from %s — %s", a.relayURL, message), res.code == 2)
	}
	profiles := parseJSONList(res.stdout)
	if len(profiles) == 0 || !truthy(profiles[0]["pubkey"]) {
		return a.connectFailed(ctx, "connect_failed", "buzz users get returned no profile",
			"Buzz: 'users get' returned no profile — is the key a member of this community?", true)
	}
	pubkey := strings.ToLower(textOf(profiles[0]["pubkey"]))
	a.mu.Lock()
	a.selfPubkey = pubkey
	a.displayName = strip(textOf(profiles[0]["display_name"]))
	a.selfNpub = hexToNpub(pubkey)
	a.mu.Unlock()
	short := clipRunes(pubkey, 8)
	acquired, err := a.AcquirePlatformLock(ctx, "buzz", a.relayURL+":"+pubkey, fmt.Sprintf("Buzz identity %s… on %s", short, a.relayURL))
	if err != nil || !acquired {
		return false, err
	}
	res, err = a.runCLI(ctx, []string{"channels", "list"}, nil)
	if err != nil {
		return false, err
	}
	if res.code != 0 {
		message := cliErrorMessage(res.stderr, res.code, "")
		return a.connectFailed(ctx, "connect_failed", message, "Buzz: failed to list channels — "+message, res.code == 2)
	}
	var listed []string
	a.mu.Lock()
	a.channelNames = map[string]string{}
	for _, ch := range parseJSONList(res.stdout) {
		if id := textOf(ch["channel_id"]); truthy(ch["channel_id"]) {
			name := id
			if truthy(ch["name"]) {
				name = textOf(ch["name"])
			}
			if _, dup := a.channelNames[id]; !dup {
				listed = append(listed, id)
			}
			a.channelNames[id] = name
			a.channelMeta[id] = ch
		}
	}
	a.mu.Unlock()
	watch := a.channels
	if len(watch) == 0 {
		watch = listed
	}
	if len(watch) == 0 {
		return a.connectFailed(ctx, "config_missing", "no Buzz channels to watch",
			"Buzz: no channels to watch (configure BUZZ_CHANNELS or join a channel)", false)
	}
	restored := loadCursors(ctx, pubkey, a.relayURL)
	a.mu.Lock()
	a.restoredCursors = restored
	a.mu.Unlock()
	for _, channelID := range watch {
		a.mu.Lock()
		skip := a.restricted[channelID]
		a.mu.Unlock()
		if skip {
			slog.Debug(fmt.Sprintf("Buzz: skipping restricted channel %s (relay rejected subscription)", channelID))
			continue
		}
		if err := a.seedChannel(ctx, channelID, "group"); err != nil {
			return false, err
		}
	}
	a.discoverMu.Lock()
	err = a.discoverDMs(ctx, true)
	a.discoverMu.Unlock()
	if err != nil {
		return false, err
	}
	a.saveCursors(ctx)
	transportUsed := "poll"
	if a.transport == "auto" || a.transport == "websocket" {
		if a.startWebsocket(ctx) {
			transportUsed = "websocket"
		} else if ctx.Err() != nil {
			return false, ctx.Err()
		} else if a.transport == "websocket" {
			a.SetFatalError(ctx, "ws_auth_failed", "Buzz WebSocket transport did not authenticate (transport=websocket)", true)
			if err := a.Disconnect(ctx); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	if transportUsed == "poll" {
		task := gateway.SpawnAdapterTask(ctx, a.pollLoop)
		a.taskMu.Lock()
		a.pollTask = task
		a.taskMu.Unlock()
	}
	a.MarkConnected(ctx, "")
	id := a.identity()
	who := id.displayName
	if who == "" {
		who = clipRunes(id.npub, 16)
	}
	a.mu.Lock()
	watching := a.state.len()
	a.mu.Unlock()
	cadence := ""
	if transportUsed != "websocket" {
		cadence = fmt.Sprintf(", poll interval %.1fs", a.pollInterval.Seconds())
	}
	slog.Info(fmt.Sprintf("Buzz: connected to %s as %s, watching %d channel(s) via %s%s", a.relayURL, who, watching, transportUsed, cadence))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Cancels the WebSocket and poll tasks and waits for them to unwind
func (a *Adapter) stopTransports(ctx context.Context) {
	a.taskMu.Lock()
	ws, poll := a.wsTask, a.pollTask
	a.wsTask, a.pollTask = nil, nil
	a.taskMu.Unlock()
	gateway.CancelTask(ctx, ws)
	gateway.CancelTask(ctx, poll)
}

// Stops the inbound transport, releases the identity lock and drops the per connection state
func (a *Adapter) Disconnect(ctx context.Context) error {
	a.MarkDisconnected(ctx)
	a.ReleasePlatformLock()
	a.stopTransports(ctx)
	a.mu.Lock()
	a.state = newOrderedMap[*channelState]()
	a.pollCount = 0
	a.mu.Unlock()
	return nil
}

// Channel name, conversation type and id of a chat
func (a *Adapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	a.mu.Lock()
	name, known := a.channelNames[chatID]
	a.mu.Unlock()
	if !known && a.cliPath != "" {
		data, err := a.cliJSON(ctx, []string{"channels", "get", "--channel", chatID}, map[string]any{})
		if err != nil {
			return nil, err
		}
		if m, ok := data.(map[string]any); ok && truthy(m["name"]) {
			name = textOf(m["name"])
			a.mu.Lock()
			a.channelNames[chatID] = name
			a.mu.Unlock()
		}
	}
	if name == "" {
		name = chatID
	}
	chatType := "group"
	a.mu.Lock()
	if st, ok := a.state.get(chatID); ok {
		chatType = st.chatType
	}
	a.mu.Unlock()
	return map[string]any{"name": name, "type": chatType, "chat_id": chatID}, nil
}
