package buzz

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Author and content snippet of an event kept for NIP-10 parent lookups
type eventMeta struct {
	author  string
	snippet string
}

// Read position, seen ids and parent lookup cache of one watched conversation
type channelState struct {
	cursor   *ChannelCursor
	seen     map[string]bool
	metaKeys []string
	meta     map[string]eventMeta
}

// Fresh state of a conversation of the chat type with no high water mark
func newChannelState(chatType string) *channelState {
	return &channelState{cursor: &ChannelCursor{ChatType: chatType}, seen: map[string]bool{}, meta: map[string]eventMeta{}}
}

// Records an event id as seen once, keeping first seen order
func (s *channelState) addSeen(id string) {
	if !s.seen[id] {
		s.seen[id] = true
		s.cursor.Seen = append(s.cursor.Seen, id)
	}
}

// Drops the oldest seen ids and parent entries beyond the cap
func (s *channelState) trim() {
	for len(s.cursor.Seen) > seenCap {
		delete(s.seen, s.cursor.Seen[0])
		s.cursor.Seen = s.cursor.Seen[1:]
	}
	for len(s.metaKeys) > seenCap {
		delete(s.meta, s.metaKeys[0])
		s.metaKeys = s.metaKeys[1:]
	}
}

// Stores author and snippet of an event as the newest parent entry
func (s *channelState) storeMeta(eventID, author, snippet string) {
	if _, ok := s.meta[eventID]; ok {
		for i, k := range s.metaKeys {
			if k == eventID {
				s.metaKeys = append(s.metaKeys[:i], s.metaKeys[i+1:]...)
				break
			}
		}
	}
	s.meta[eventID] = eventMeta{author, snippet}
	s.metaKeys = append(s.metaKeys, eventID)
	for len(s.metaKeys) > seenCap {
		delete(s.meta, s.metaKeys[0])
		s.metaKeys = s.metaKeys[1:]
	}
}

// Buzz adapter, a NIP-42 websocket subscription with a CLI poll fallback in and the buzz CLI out
type BuzzAdapter struct {
	*gateway.BasePlatformAdapter

	extra             map[string]any
	relayURL          string
	attachmentOrigins map[origin]bool
	cliPath           string
	channels          []string
	homeChannel       string
	pollInterval      float64
	requireMention    bool
	replyToMode       string
	transport         string
	allowedPubkeys    map[string]bool
	reactionOnly      map[string]bool

	mu           sync.Mutex
	privateKey   string
	authTag      string
	selfPubkey   string
	selfNpub     string
	displayName  string
	pollTask     *gateway.AdapterTask
	wsTask       *gateway.AdapterTask
	wsReady      chan struct{}
	wsReadyOnce  *sync.Once
	membership   int64
	pollCount    int
	restricted   map[string]bool
	state        map[string]*channelState
	cursors      *CursorStore
	channelNames map[string]string
	channelMeta  map[string]map[string]any
	userNames    map[string]string
	memberCache  map[string]ttlEntry[[]string]
	profileNames map[string]ttlEntry[string]
	threadRoots  map[string]string
	threadOrder  []string
}

// Class attributes the Buzz adapter declares over the base defaults
func buzzAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.SerialReceiveLoop = true
	return traits
}

// Adapter for a platform config, its knobs from the buzz settings of the profile it serves
func NewBuzzAdapter(ctx context.Context, config *gateway.PlatformConfig) (*BuzzAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	a := &BuzzAdapter{extra: extra, attachmentOrigins: map[origin]bool{}, restricted: map[string]bool{},
		state: map[string]*channelState{}, cursors: &CursorStore{}, channelNames: map[string]string{},
		channelMeta: map[string]map[string]any{}, userNames: map[string]string{}, memberCache: map[string]ttlEntry[[]string]{},
		profileNames: map[string]ttlEntry[string]{}, threadRoots: map[string]string{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.Platform("buzz"), a)
	a.Traits = buzzAdapterTraits()
	sctx := gateway.SettingsContext(ctx)
	a.relayURL = configuredRelay(ctx)
	for _, h := range settings.BuzzAttachmentHosts.Strings(sctx) {
		if o, ok := attachmentOrigin(h); ok {
			a.attachmentOrigins[o] = true
		}
	}
	if o, ok := attachmentOrigin(a.relayURL); ok {
		a.attachmentOrigins[o] = true
	}
	a.cliPath = configuredCLIPath(ctx)
	a.channels = configuredChannels(ctx)
	var err error
	if a.homeChannel, err = configuredHomeChannel(ctx, extra); err != nil {
		return nil, err
	}
	poll, set := settings.BuzzPollInterval.Lookup(sctx)
	if !set || poll == 0 {
		poll = defaultPollInterval
	}
	a.pollInterval = max(minPollInterval, poll)
	a.requireMention = settings.BuzzRequireMention.Get(sctx)
	a.replyToMode = replyToMode(ctx)
	a.transport = settings.BuzzTransport.Get(sctx)
	a.allowedPubkeys = pubkeySet(settings.BuzzAllowedUsers.Get(sctx))
	a.reactionOnly = pubkeySet(settings.BuzzReactionOnlyUsers.Get(sctx))
	return a, nil
}

// Display name of the platform
func (a *BuzzAdapter) Name() string { return "Buzz" }

// Hex form of an allowlist entry given as hex or npub, the hook allowlist matching consults
func (a *BuzzAdapter) NormalizeUserID(entry string) string { return normalizeUserRef(entry) }

// Runs the buzz CLI with the resolved key and owner tag, resolving them first when unset
func (a *BuzzAdapter) runCLI(ctx context.Context, args []string, input *string) (cliResult, error) {
	a.mu.Lock()
	key, tag := a.privateKey, a.authTag
	a.mu.Unlock()
	if key == "" {
		var err error
		if key, err = resolvePrivateKey(ctx, a.extra); err != nil {
			return cliResult{}, err
		}
		if tag, err = resolveAuthTag(ctx, a.extra); err != nil {
			return cliResult{}, err
		}
		a.mu.Lock()
		a.privateKey, a.authTag = key, tag
		a.mu.Unlock()
	}
	return execBuzz(ctx, a.cliPath, args, a.relayURL, key, tag, input)
}

// Decoded stdout of a successful CLI run, the default otherwise
func (a *BuzzAdapter) cliJSON(ctx context.Context, args []string, def any) (any, error) {
	res, err := a.runCLI(ctx, args, nil)
	if err != nil {
		return nil, err
	}
	if res.code != 0 {
		return def, nil
	}
	return jsonOr(res.stdout, def), nil
}

// Logs and records a fatal connect error, always reporting failure
func (a *BuzzAdapter) connectFailed(ctx context.Context, code, detail, log string, retryable bool) (bool, error) {
	pkgLog().Error(log)
	a.SetFatalError(ctx, code, detail, retryable)
	return false, nil
}

// Verifies relay credentials, seeds high water marks and starts the websocket or the poll loop
func (a *BuzzAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.relayURL == "" {
		return a.connectFailed(ctx, "config_missing", "BUZZ_RELAY_URL must be set", "Buzz: relay URL must be configured", false)
	}
	if a.cliPath == "" {
		return a.connectFailed(ctx, "cli_missing", "buzz CLI binary not found",
			"Buzz: buzz CLI binary not found (set BUZZ_CLI_PATH or put 'buzz' on PATH)", false)
	}
	key, err := resolvePrivateKey(ctx, a.extra)
	if err != nil {
		return false, err
	}
	tag, err := resolveAuthTag(ctx, a.extra)
	var invalid *authTagError
	if errors.As(err, &invalid) {
		return a.connectFailed(ctx, "config_invalid", invalid.Error(), "Buzz: invalid owner-auth configuration — "+invalid.Error(), false)
	}
	if err != nil {
		return false, err
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
	if len(profiles) == 0 || !anyutil.IsTrue(profiles[0]["pubkey"]) {
		return a.connectFailed(ctx, "connect_failed", "buzz users get returned no profile",
			"Buzz: 'users get' returned no profile — is the key a member of this community?", true)
	}
	a.mu.Lock()
	a.selfPubkey = strings.ToLower(strOf(profiles[0]["pubkey"]))
	a.displayName = strings.TrimSpace(strOf(profiles[0]["display_name"]))
	a.selfNpub = hexToNpub(a.selfPubkey)
	selfPubkey := a.selfPubkey
	a.mu.Unlock()
	locked, err := a.AcquirePlatformLock(ctx, "buzz", a.relayURL+":"+selfPubkey,
		fmt.Sprintf("Buzz identity %s… on %s", textutil.First(selfPubkey, 8), a.relayURL))
	if err != nil {
		return false, err
	}
	if !locked {
		return false, nil
	}
	res, err = a.runCLI(ctx, []string{"channels", "list"}, nil)
	if err != nil {
		return false, err
	}
	if res.code != 0 {
		message := cliErrorMessage(res.stderr, res.code, "")
		return a.connectFailed(ctx, "connect_failed", message, "Buzz: failed to list channels — "+message, res.code == 2)
	}
	a.mu.Lock()
	a.channelNames = map[string]string{}
	var listed []string
	for _, ch := range parseJSONList(res.stdout) {
		if id := strOf(ch["channel_id"]); id != "" {
			name := strOf(ch["name"])
			if name == "" {
				name = id
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
	if err := a.loadCursors(ctx); err != nil {
		return false, err
	}
	for _, channelID := range watch {
		a.mu.Lock()
		skip := a.restricted[channelID]
		a.mu.Unlock()
		if skip {
			pkgLog().Debug("Buzz: skipping restricted channel " + channelID + " (relay rejected subscription)")
			continue
		}
		if err := a.seedChannel(ctx, channelID, "group"); err != nil {
			return false, err
		}
	}
	if err := a.discoverDMs(ctx, true); err != nil {
		return false, err
	}
	a.saveCursors(ctx)
	transportUsed := "poll"
	if a.transport == "auto" || a.transport == "websocket" {
		if a.startWebsocket(ctx) {
			transportUsed = "websocket"
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
		a.mu.Lock()
		a.pollTask = task
		a.mu.Unlock()
	}
	a.MarkConnected(ctx, "")
	a.mu.Lock()
	who := a.displayName
	if who == "" {
		who = textutil.First(a.selfNpub, 16)
	}
	watched := len(a.state)
	a.mu.Unlock()
	suffix := ""
	if transportUsed != "websocket" {
		suffix = fmt.Sprintf(", poll interval %.1fs", a.pollInterval)
	}
	pkgLog().Info(fmt.Sprintf("Buzz: connected to %s as %s, watching %d channel(s) via %s%s", a.relayURL, who, watched, transportUsed, suffix))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops the inbound transport and drops runtime state
func (a *BuzzAdapter) Disconnect(ctx context.Context) error {
	a.MarkDisconnected(ctx)
	func() {
		defer func() { _ = recover() }()
		a.ReleasePlatformLock()
	}()
	a.mu.Lock()
	wsTask, pollTask := a.wsTask, a.pollTask
	a.wsTask, a.pollTask = nil, nil
	a.mu.Unlock()
	gateway.CancelTask(ctx, wsTask)
	gateway.CancelTask(ctx, pollTask)
	a.mu.Lock()
	a.state = map[string]*channelState{}
	a.cursors.ChannelState, a.cursors.ChannelOrder = nil, nil
	a.pollCount = 0
	a.mu.Unlock()
	return nil
}

// Watches a conversation under the state and the cursor store, the caller holding the lock
func (a *BuzzAdapter) watchLocked(channelID string, st *channelState) {
	a.state[channelID] = st
	a.cursors.Watch(channelID, st.cursor)
}

// Drops a conversation from the state and the cursor store, the caller holding the lock
func (a *BuzzAdapter) forgetLocked(channelID string) {
	delete(a.state, channelID)
	a.cursors.Forget(channelID)
}

// Restores cursors persisted for this identity and relay
func (a *BuzzAdapter) loadCursors(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cursors.SelfPubkey, a.cursors.RelayURL = a.selfPubkey, a.relayURL
	return a.cursors.LoadCursors(ctx)
}

// Persists every watched cursor now
func (a *BuzzAdapter) saveCursors(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cursors.SaveCursors(ctx)
}

// Member public keys of a channel for mention resolution, recent traffic standing in when the member list is empty
func (a *BuzzAdapter) channelMemberPubkeys(ctx context.Context, chatID string) ([]string, error) {
	a.mu.Lock()
	cached, ok := ttlGet(a.memberCache, chatID, memberCacheTTL)
	a.mu.Unlock()
	if ok {
		return append([]string(nil), cached...), nil
	}
	var pks []string
	rows, err := a.cliJSON(ctx, []string{"channels", "members", "--channel", chatID}, []any{})
	if err != nil {
		return nil, err
	}
	list, _ := rows.([]any)
	for _, row := range list {
		if m, ok := row.(map[string]any); ok {
			pks = addPubkey(pks, nilIfMissing(m, "pubkey"))
		} else {
			pks = addPubkey(pks, row)
		}
	}
	if len(pks) > 0 {
		a.mu.Lock()
		a.memberCache[chatID] = ttlEntry[[]string]{time.Now(), append([]string(nil), pks...)}
		a.mu.Unlock()
		return pks, nil
	}
	msgs, err := a.cliJSON(ctx, []string{"messages", "get", "--channel", chatID, "--limit", "50"}, []any{})
	if err != nil {
		return nil, err
	}
	var candidates []string
	msgList, _ := msgs.([]any)
	for _, raw := range msgList {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		candidates = addPubkey(candidates, nilIfMissing(msg, "pubkey"))
		tags, _ := msg["tags"].([]any)
		for _, t := range tags {
			if tag, ok := t.([]any); ok && len(tag) > 1 && strOf(tag[0]) == "p" {
				candidates = addPubkey(candidates, strOf(tag[1]))
			}
		}
	}
	if len(candidates) > 0 {
		a.mu.Lock()
		a.memberCache[chatID] = ttlEntry[[]string]{time.Now(), append([]string(nil), candidates...)}
		a.mu.Unlock()
	}
	return candidates, nil
}

// Map value or nil when the key is missing
func nilIfMissing(m map[string]any, key string) any {
	v, ok := m[key]
	if !ok {
		return nil
	}
	return v
}

// Display name of a public key through users get, TTL cached
func (a *BuzzAdapter) profileDisplayName(ctx context.Context, pubkey string) (string, error) {
	a.mu.Lock()
	cached, ok := ttlGet(a.profileNames, pubkey, profileNameTTL)
	a.mu.Unlock()
	if ok {
		return cached, nil
	}
	name := ""
	profiles, err := a.cliJSON(ctx, []string{"users", "get", "--pubkey", pubkey}, []any{})
	if err != nil {
		return "", err
	}
	if list, ok := profiles.([]any); ok && len(list) > 0 {
		if p0, ok := list[0].(map[string]any); ok {
			name = firstNonEmpty(strOf(p0["display_name"]), strOf(p0["name"]))
			if name == "" && anyutil.IsTrue(p0["content"]) {
				if prof, ok := jsonOr(strOf(p0["content"]), map[string]any{}).(map[string]any); ok {
					name = firstNonEmpty(strOf(prof["display_name"]), strOf(prof["name"]))
				}
			}
		}
	}
	a.mu.Lock()
	a.profileNames[pubkey] = ttlEntry[string]{time.Now(), name}
	a.mu.Unlock()
	return name, nil
}

// First non blank value stripped
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// Member public keys the @Name tokens of the content resolve to, longest names first, ambiguous names tagging nobody
func (a *BuzzAdapter) mentionPubkeysFor(ctx context.Context, chatID, content string) ([]string, error) {
	if !strings.Contains(content, "@") {
		return nil, nil
	}
	members, err := a.channelMemberPubkeys(ctx, chatID)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	self := a.selfPubkey
	a.mu.Unlock()
	byName := map[string][]string{}
	display := map[string]string{}
	var keys []string
	for _, pk := range members {
		if pk == self {
			continue
		}
		name, err := a.profileDisplayName(ctx, pk)
		if err != nil {
			return nil, err
		}
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := byName[key]; !ok {
			keys = append(keys, key)
		}
		pks := byName[key]
		dup := false
		for _, have := range pks {
			dup = dup || have == pk
		}
		if !dup {
			byName[key] = append(pks, pk)
		}
		if _, ok := display[key]; !ok {
			display[key] = name
		}
	}
	stableSortByLenDesc(keys)
	var found []string
	text := content
	for _, key := range keys {
		pattern := mustRE2(`(?<![\w@])@`+regexp2.Escape(display[key])+`(?!\w)`, regexp2.IgnoreCase)
		if !re2Match(pattern, text) {
			continue
		}
		if pks := byName[key]; len(pks) == 1 {
			already := false
			for _, f := range found {
				already = already || f == pks[0]
			}
			if !already {
				found = append(found, pks[0])
			}
		}
		if replaced, err := pattern.Replace(text, "\x00", -1, -1); err == nil {
			text = replaced
		}
	}
	return found, nil
}

// Keys ordered longest first, equal lengths keeping their order
func stableSortByLenDesc(keys []string) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && len(keys[j]) > len(keys[j-1]); j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
}

// Sends with bounded recovery, dropping stale mentions, escaping an unresolved @token, then mentioning self
func (a *BuzzAdapter) runMessageSend(ctx context.Context, args []string, content string, mentionPubkeys []string) (cliResult, error) {
	var mentionArgs []string
	for _, pk := range mentionPubkeys {
		mentionArgs = append(mentionArgs, "--mention", pk)
	}
	res, err := a.runCLI(ctx, append(append([]string(nil), args...), mentionArgs...), &content)
	if err != nil || res.code == 0 {
		return res, err
	}
	if len(mentionArgs) > 0 && strings.Contains(res.stderr, "not channel members") {
		if res, err = a.runCLI(ctx, args, &content); err != nil || res.code == 0 {
			return res, err
		}
	}
	if escaped, ok := escapeUnresolvedPresentationMention(content, res.stderr); ok {
		pkgLog().Info("Buzz: retrying message after unresolved presentation-mention preflight")
		if res, err = a.runCLI(ctx, args, &escaped); err != nil || res.code == 0 {
			return res, err
		}
	}
	a.mu.Lock()
	self := a.selfPubkey
	a.mu.Unlock()
	if strings.Contains(res.stderr, "does not match a current channel member") && self != "" {
		return a.runCLI(ctx, append(append([]string(nil), args...), "--mention", self), &content)
	}
	return res, nil
}

// Publishes the content into the channel, threaded against the metadata thread, stream anchor or reply target
func (a *BuzzAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if content == "" {
		return gateway.SendResult{Success: false, Error: "Empty message"}, nil
	}
	anchor := replyTo
	if v := firstTruthy(metadata["thread_id"], metadata["reply_to_message_id"]); v != "" {
		anchor = v
	}
	args := append([]string{"messages", "send", "--channel", chatID, "--content", "-"}, a.replyArgs(anchor)...)
	mentions, err := a.mentionPubkeysFor(ctx, chatID, content)
	if err != nil {
		return gateway.SendResult{}, err
	}
	res, err := a.runMessageSend(ctx, args, content, mentions)
	if err != nil {
		return gateway.SendResult{}, err
	}
	result := a.sendResult(chatID, res, "")
	if result.Success {
		a.mu.Lock()
		self := a.selfPubkey
		a.mu.Unlock()
		a.rememberEventMeta(chatID, anyutil.String(result.MessageID), self, content)
	}
	return result, nil
}

// First truthy value as text, empty when none is
func firstTruthy(values ...any) string {
	for _, v := range values {
		if anyutil.IsTrue(v) {
			return anyutil.String(v)
		}
	}
	return ""
}

// --reply-to arguments for an anchor joined to its thread root, none when replies are off
func (a *BuzzAdapter) replyArgs(anchor string) []string {
	target := a.resolveReplyAnchor(anchor)
	if target != "" && a.replyToMode != "off" {
		return []string{"--reply-to", target}
	}
	return nil
}

// Send result of a messages send run, the verified id marked seen
func (a *BuzzAdapter) sendResult(chatID string, res cliResult, redactPath string) gateway.SendResult {
	if res.code != 0 {
		return gateway.SendResult{Success: false, Error: cliErrorMessage(res.stderr, res.code, redactPath), Retryable: res.code == 2}
	}
	eventID, receiptErr := parseSendReceipt(res.stdout)
	if receiptErr != "" {
		return gateway.SendResult{Success: false, Error: receiptErr}
	}
	a.markSeen(chatID, eventID)
	return gateway.SendResult{Success: true, MessageID: eventID}
}

// Buzz has no typing indicator
func (a *BuzzAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	return nil
}

// Adds an emoji reaction to a message
func (a *BuzzAdapter) AddReaction(ctx context.Context, chatID, messageID, emoji string) error {
	ok, err := a.SendReaction(ctx, chatID, messageID, emoji)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("Buzz refused the reaction")
	}
	return nil
}

// Adds a reaction through the CLI, a failure logged and reported false
func (a *BuzzAdapter) SendReaction(ctx context.Context, chatID, messageID, emoji string) (bool, error) {
	if a.cliPath == "" || emoji == "" || messageID == "" {
		return false, nil
	}
	res, err := a.runCLI(ctx, []string{"reactions", "add", "--event", messageID, "--emoji", emoji}, nil)
	if err != nil {
		return false, err
	}
	if res.code != 0 {
		pkgLog().Debug(fmt.Sprintf("Buzz: reaction add failed for message %s in %s — %s", textutil.First(messageID, 12), chatID, cliErrorMessage(res.stderr, res.code, "")))
	}
	return res.code == 0, nil
}

// Edits a sent message in place, reporting the original id the stream consumer keeps addressing
func (a *BuzzAdapter) EditMessage(ctx context.Context, chatID, messageID, content string, finalize bool) (gateway.SendResult, error) {
	if messageID == "" {
		return gateway.SendResult{Success: false, Error: "Buzz edit needs a message id"}, nil
	}
	if content == "" {
		return gateway.SendResult{Success: false, Error: "Empty message"}, nil
	}
	res, err := a.runCLI(ctx, []string{"messages", "edit", "--event", messageID, "--content=" + content}, nil)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if res.code != 0 {
		return gateway.SendResult{Success: false, Error: cliErrorMessage(res.stderr, res.code, ""), Retryable: res.code == 2}, nil
	}
	data, _ := jsonOr(res.stdout, map[string]any{}).(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	if anyutil.IsTrue(data["event_id"]) {
		a.markSeen(chatID, strOf(data["event_id"]))
	}
	accepted := true
	if v, ok := data["accepted"]; ok {
		accepted = anyutil.IsTrue(v)
	}
	return gateway.SendResult{Success: accepted, MessageID: messageID, RawResponse: data}, nil
}

// Deletes a sent message, the deletion event marked seen
func (a *BuzzAdapter) DeleteMessage(ctx context.Context, chatID, messageID string) (bool, error) {
	if messageID == "" {
		return false, nil
	}
	res, err := a.runCLI(ctx, []string{"messages", "delete", "--event", messageID}, nil)
	if err != nil {
		return false, err
	}
	if res.code != 0 {
		return false, nil
	}
	data, ok := jsonOr(res.stdout, map[string]any{}).(map[string]any)
	if !ok {
		return true, nil
	}
	if anyutil.IsTrue(data["event_id"]) {
		a.markSeen(chatID, strOf(data["event_id"]))
	}
	if v, ok := data["accepted"]; ok {
		return anyutil.IsTrue(v), nil
	}
	return true, nil
}

// Uploads a local image file or posts a URL as an image link
func (a *BuzzAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	if !strings.HasPrefix(imageURL, "http://") && !strings.HasPrefix(imageURL, "https://") {
		local := pathutil.ExpandUser(imageURL)
		if isFile(local) {
			return a.sendFileAttachment(ctx, chatID, local, caption, replyTo, metadata, false)
		}
	}
	text := imageURL
	if caption != "" {
		text = caption + "\n" + imageURL
	}
	return a.Send(ctx, chatID, text, replyTo, metadata)
}

// Whether the path names a regular file
func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// Uploads a local file as a native attachment, never naming the host path in chat visible errors
func (a *BuzzAdapter) sendFileAttachment(ctx context.Context, chatID, filePath, caption, replyTo string, metadata map[string]any, probe bool) (gateway.SendResult, error) {
	local := pathutil.ExpandUser(filePath)
	if probe && !isFile(local) {
		return gateway.SendResult{Success: false, Error: "Media file not found"}, nil
	}
	anchor := replyTo
	if v := firstTruthy(metadata["thread_id"]); v != "" {
		anchor = v
	}
	args := append([]string{"messages", "send", "--channel", chatID, "--file", local, "--content", "-"}, a.replyArgs(anchor)...)
	res, err := a.runMessageSend(ctx, args, caption, nil)
	if err != nil {
		return gateway.SendResult{}, err
	}
	return a.sendResult(chatID, res, local), nil
}

// Uploads a local image, a missing path taking the base fallback so host paths never reach chat
func (a *BuzzAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	local := pathutil.ExpandUser(imagePath)
	if isFile(local) {
		return a.sendFileAttachment(ctx, chatID, local, caption, replyTo, metadata, false)
	}
	return a.BasePlatformAdapter.SendImageFile(ctx, chatID, imagePath, caption, replyTo, metadata, extra)
}

// Uploads a local document as a native attachment
func (a *BuzzAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFileAttachment(ctx, chatID, filePath, caption, replyTo, metadata, true)
}

// Uploads a local video as a native attachment
func (a *BuzzAdapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFileAttachment(ctx, chatID, videoPath, caption, replyTo, metadata, true)
}

// Uploads a local audio file as a native attachment
func (a *BuzzAdapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.sendFileAttachment(ctx, chatID, audioPath, caption, replyTo, metadata, true)
}

// Name and type of a conversation, the name fetched through the CLI when unknown
func (a *BuzzAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	a.mu.Lock()
	st := a.state[chatID]
	name, known := a.channelNames[chatID]
	chatType := "group"
	if st != nil {
		chatType = st.cursor.ChatType
	}
	a.mu.Unlock()
	if !known && a.cliPath != "" {
		data, err := a.cliJSON(ctx, []string{"channels", "get", "--channel", chatID}, map[string]any{})
		if err != nil {
			return nil, err
		}
		if m, ok := data.(map[string]any); ok && anyutil.IsTrue(m["name"]) {
			name = strOf(m["name"])
			a.mu.Lock()
			a.channelNames[chatID] = name
			a.mu.Unlock()
		}
	}
	if name == "" {
		name = chatID
	}
	return map[string]any{"name": name, "type": chatType, "chat_id": chatID}, nil
}

// Marks an event id seen in its conversation
func (a *BuzzAdapter) markSeen(channelID, eventID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st := a.state[channelID]; st != nil {
		st.addSeen(eventID)
		st.trim()
	}
}

// Records author and snippet of an event for later parent lookups
func rememberEventLocked(st *channelState, event map[string]any) {
	eventID := strOf(event["id"])
	if eventID == "" {
		return
	}
	snippet := ""
	if content, ok := event["content"].(string); ok {
		snippet = textutil.First(content, eventMetaContentCap)
	}
	st.storeMeta(eventID, strings.ToLower(strOf(event["pubkey"])), snippet)
}

// Records an event this adapter sent as a parent so replies to it match without its echo
func (a *BuzzAdapter) rememberEventMeta(channelID, eventID, pubkey, content string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if st := a.state[channelID]; st != nil {
		rememberEventLocked(st, map[string]any{"id": eventID, "pubkey": pubkey, "content": content})
	}
}

// Caches the thread root of an inbound message id, oldest entries dropped beyond the cap
func (a *BuzzAdapter) recordThreadRoot(eventID string, event map[string]any) {
	if eventID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.threadRoots[eventID]; ok {
		for i, k := range a.threadOrder {
			if k == eventID {
				a.threadOrder = append(a.threadOrder[:i], a.threadOrder[i+1:]...)
				break
			}
		}
	}
	a.threadRoots[eventID] = extractThreadRoot(event)
	a.threadOrder = append(a.threadOrder, eventID)
	for len(a.threadOrder) > threadRootCache {
		delete(a.threadRoots, a.threadOrder[0])
		a.threadOrder = a.threadOrder[1:]
	}
}

// Thread root of the anchor when it was inside a thread, the anchor itself otherwise
func (a *BuzzAdapter) resolveReplyAnchor(anchor string) string {
	if anchor == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if root := a.threadRoots[anchor]; root != "" {
		return root
	}
	return anchor
}
