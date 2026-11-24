package weixin

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	mimeutil "github.com/nickheyer/icebath/pkg/utils/mime"
)

// Connected adapters by bot token, the registry standalone sends reuse a live session through
var liveAdapters = struct {
	sync.Mutex
	byToken map[string]*WeixinAdapter
}{byToken: map[string]*WeixinAdapter{}}

// Media cache writers the adapter stores inbound media through, swappable like module globals
var (
	cacheImageFromBytes    = gateway.CacheImageFromBytesAsync
	cacheAudioFromBytes    = gateway.CacheAudioFromBytesAsync
	cacheDocumentFromBytes = gateway.CacheDocumentFromBytesAsync
)

// Item key, download timeout, fixed MIME type and log label of one inbound media item type
type inboundMediaSpec struct {
	itemKey        string
	timeoutSeconds float64
	mime           string
	label          string
	cache          func(ctx context.Context, data []byte, name string) (string, error)
}

// Inbound media dispatch by item type, files guessing their MIME type from the file name
var inboundMedia = map[int64]inboundMediaSpec{
	itemImage: {"image_item", 30.0, "image/jpeg", "image", func(ctx context.Context, data []byte, _ string) (string, error) {
		return cacheImageFromBytes(ctx, data, ".jpg")
	}},
	itemVideo: {"video_item", 120.0, "video/mp4", "video", func(ctx context.Context, data []byte, _ string) (string, error) {
		return cacheDocumentFromBytes(ctx, data, "video.mp4")
	}},
	itemFile: {"file_item", 60.0, "", "file", func(ctx context.Context, data []byte, name string) (string, error) {
		return cacheDocumentFromBytes(ctx, data, name)
	}},
	itemVoice: {"voice_item", 60.0, "audio/silk", "voice", func(ctx context.Context, data []byte, _ string) (string, error) {
		return cacheAudioFromBytes(ctx, data, ".silk")
	}},
}

// Class attributes of the Weixin adapter
func adapterTraits() gateway.AdapterTraits {
	t := gateway.DefaultAdapterTraits()
	t.SupportsCodeBlocks = true
	t.SplitsLongMessages = true
	t.MaxMessageLength = maxMessageLength
	t.SupportsMessageEditing = false
	return t
}

// WeixinAdapter bridges a personal WeChat account over the iLink bot API, long polling for inbound messages
type WeixinAdapter struct {
	*gateway.BasePlatformAdapter
	*gateway.OwnAccessPolicyMixin

	home        string
	tokenStore  *contextTokenStore
	typingCache *typingTicketCache
	dedup       *gateway.MessageDeduplicator

	accountID, token, baseURL, cdnBaseURL string
	sendChunkDelaySeconds                 float64
	sendChunkRetries                      int
	sendChunkRetryDelaySeconds            float64
	rateLimitCircuitThreshold             int
	rateLimitCircuitWindowSeconds         float64
	rateLimitCircuitOpenSeconds           float64
	splitMultilineMessages                bool

	sendTextGate sync.Mutex

	mu                    sync.Mutex
	pollSession           *ilinkSession
	sendSession           *ilinkSession
	pollTask              *gateway.AdapterTask
	rateLimitCircuitUntil float64
	rateLimitEvents       []float64
}

// Adapter configured from the platform config, the scoped WEIXIN env and the account saved by QR login
func NewWeixinAdapter(ctx context.Context, cfg *gateway.PlatformConfig) (*WeixinAdapter, error) {
	if cfg == nil {
		cfg = gateway.NewPlatformConfig()
	}
	a := &WeixinAdapter{home: home.Dir(ctx), typingCache: newTypingTicketCache(),
		dedup: gateway.NewMessageDeduplicator(messageDedupDefaultSize, messageDedupTTLSeconds)}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformWeixin, a)
	a.Traits = adapterTraits()
	policy, err := gateway.NewOwnAccessPolicyMixin("WeixinAdapter", "WEIXIN")
	if err != nil {
		return nil, err
	}
	a.OwnAccessPolicyMixin = policy
	extra := cfg.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	if a.tokenStore, err = newContextTokenStore(a.home); err != nil {
		return nil, err
	}
	setting := func(key, def string) string {
		if err != nil {
			return ""
		}
		var value string
		value, err = extraOrSecret(ctx, extra, key, def)
		return value
	}
	a.accountID = setting("account_id", "")
	tokenRaw := anyutil.Or(cfg.Token, extra["token"])
	if err == nil && !anyutil.IsTrue(tokenRaw) {
		tokenRaw, err = gateway.GetScopedSecret(ctx, "WEIXIN_TOKEN", "", false)
	}
	a.token = strings.TrimSpace(anyutil.String(tokenRaw))
	a.baseURL = strings.TrimRight(settings.WeixinBaseURL.Get(gateway.SettingsContext(ctx)), "/")
	a.cdnBaseURL = strings.TrimRight(settings.WeixinCdnBaseURL.Get(gateway.SettingsContext(ctx)), "/")
	sctx := gateway.SettingsContext(ctx)
	a.sendChunkDelaySeconds = settings.WeixinSendChunkDelay.Get(sctx).Seconds()
	a.sendChunkRetries = settings.WeixinSendChunkRetries.Get(sctx)
	a.sendChunkRetryDelaySeconds = settings.WeixinSendChunkRetryDelay.Get(sctx).Seconds()
	a.rateLimitCircuitThreshold = settings.WeixinRateLimitCircuitThreshold.Get(sctx)
	a.rateLimitCircuitWindowSeconds = settings.WeixinRateLimitCircuitWindow.Get(sctx).Seconds()
	a.rateLimitCircuitOpenSeconds = settings.WeixinRateLimitCircuitOpen.Get(sctx).Seconds()
	a.DMPolicy = strings.ToLower(settings.WeixinDmPolicy.Get(sctx))
	a.GroupPolicy = strings.ToLower(settings.WeixinGroupPolicy.Get(sctx))
	for _, entry := range gateway.SettingList(ctx, settings.WeixinAllowFrom) {
		a.AllowFrom[entry] = true
	}
	for _, entry := range gateway.SettingList(ctx, settings.WeixinGroupAllowFrom) {
		a.GroupAllowFrom[entry] = true
	}
	a.splitMultilineMessages = settings.WeixinSplitMultilineMessages.Get(gateway.SettingsContext(ctx))
	if err != nil {
		return nil, err
	}
	a.ConfigureTextBatchDelays()
	if a.accountID != "" && a.token == "" {
		persisted, err := loadWeixinAccount(a.home, a.accountID)
		if err != nil {
			return nil, err
		}
		if persisted != nil && persisted.Len() > 0 {
			a.token = textOf(persisted.Value("token"))
			a.baseURL = strings.TrimRight(strings.TrimSpace(anyutil.String(anyutil.Or(persisted.Value("base_url"), a.baseURL))), "/")
		}
	}
	return a, nil
}

// The adapter gates DM and group access itself
func (a *WeixinAdapter) EnforcesOwnAccessPolicy() bool {
	return a.OwnAccessPolicyMixin.EnforcesOwnAccessPolicy()
}

// Quiet period of a pending batch, chunks near the iLink split point waiting the split delay
func (a *WeixinAdapter) TextBatchDelayFor(pending *gateway.MessageEvent) float64 {
	lastLen := 0
	if pending != nil {
		lastLen = pending.LastChunkLen()
	}
	delay, split := a.TextBatchDelays()
	if lastLen >= weixinSplitThreshold {
		return split
	}
	return delay
}

// Open sessions of the adapter, nil while disconnected
func (a *WeixinAdapter) sessions() (poll, send *ilinkSession) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pollSession, a.sendSession
}

// Takes the bot token lock, opens the sessions, restores the context tokens and starts the poll loop
func (a *WeixinAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	for _, check := range []struct {
		ok           bool
		code, reason string
	}{
		{checkWeixinRequirements(), "weixin_missing_dependency", "aiohttp and cryptography are required"},
		{a.token != "", "weixin_missing_token", "WEIXIN_TOKEN is required"},
		{a.accountID != "", "weixin_missing_account", "WEIXIN_ACCOUNT_ID is required"},
	} {
		if !check.ok {
			a.SetFatalError(ctx, check.code, "Weixin startup failed: "+check.reason, false)
			pkgLog().Warn(fmt.Sprintf("[%s] Weixin startup failed: %s", a.Name(), check.reason))
			return false, nil
		}
	}
	acquired, err := a.AcquirePlatformLock(ctx, "weixin-bot-token", a.token, "Weixin bot token")
	switch {
	case err != nil && ctx.Err() != nil:
		return false, ctx.Err()
	case err != nil:
		pkgLog().Debug(fmt.Sprintf("[%s] Token lock unavailable (non-fatal): %s", a.Name(), err.Error()))
	case !acquired:
		return false, nil
	}
	a.mu.Lock()
	a.pollSession, a.sendSession = newSession(ctx), newSession(ctx)
	a.mu.Unlock()
	if err := a.tokenStore.restore(a.accountID); err != nil {
		return false, err
	}
	a.SetRunning(true)
	poll := gateway.SpawnAdapterTask(ctx, a.pollLoop)
	a.mu.Lock()
	a.pollTask = poll
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	liveAdapters.Lock()
	liveAdapters.byToken[a.token] = a
	liveAdapters.Unlock()
	pkgLog().Info(fmt.Sprintf("[%s] Connected account=%s base=%s", a.Name(), safeID(a.accountID), a.baseURL))
	if a.GroupPolicy != "disabled" {
		pkgLog().Warn(fmt.Sprintf("[%s] WEIXIN_GROUP_POLICY=%s is set, but QR-login connects an iLink bot identity (e.g. ...@im.bot) "+
			"which typically cannot be invited into ordinary WeChat groups. iLink usually does not deliver "+
			"ordinary-group events for these accounts, so group messages may never reach Icebath regardless of "+
			"this policy. If group delivery doesn't work, the limitation is on the iLink side, not in Icebath.", a.Name(), a.GroupPolicy))
	}
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Drops the live registration, cancels the batch flushes and the poll loop, closes the sessions and releases the lock
func (a *WeixinAdapter) Disconnect(ctx context.Context) error {
	liveAdapters.Lock()
	if liveAdapters.byToken[a.token] == a {
		delete(liveAdapters.byToken, a.token)
	}
	liveAdapters.Unlock()
	a.SetRunning(false)
	for _, task := range a.TextBatchTasks() {
		if !task.Done() {
			task.Cancel()
		}
	}
	a.ClearTextBatches()
	a.mu.Lock()
	poll := a.pollTask
	a.pollTask = nil
	a.mu.Unlock()
	gateway.CancelTask(ctx, poll)
	a.mu.Lock()
	sessions := []*ilinkSession{a.pollSession, a.sendSession}
	a.pollSession, a.sendSession = nil, nil
	a.mu.Unlock()
	for _, session := range sessions {
		if session != nil && !session.isClosed() {
			session.close()
		}
	}
	a.ReleasePlatformLock()
	a.MarkDisconnected(ctx)
	pkgLog().Info(fmt.Sprintf("[%s] Disconnected", a.Name()))
	return nil
}

// Long polls getupdates, dispatching each message and persisting the cursor, backing off on failures
func (a *WeixinAdapter) pollLoop(ctx context.Context) error {
	syncBuf, err := loadSyncBuf(a.home, a.accountID)
	if err != nil {
		return err
	}
	timeoutMS := longPollTimeoutMS
	consecutiveFailures := 0
	backoff := func() (int, error) {
		streakDone := consecutiveFailures >= maxConsecutiveFailures
		delay := float64(retryDelaySeconds)
		if streakDone {
			delay = backoffDelaySeconds
		}
		if err := sleep(ctx, delay); err != nil {
			return 0, err
		}
		if streakDone {
			return 0, nil
		}
		return consecutiveFailures, nil
	}
	for a.IsConnected() {
		pollErr := func() error {
			session, _ := a.sessions()
			if session == nil {
				return errSessionClosed
			}
			response, err := getUpdates(ctx, session, a.baseURL, a.token, syncBuf, timeoutMS)
			if err != nil {
				return err
			}
			if suggested, ok := response.Value("longpolling_timeout_ms").(int64); ok && suggested > 0 {
				timeoutMS = int(suggested)
			}
			ret, errcode := getOr(response, "ret", int64(0)), getOr(response, "errcode", int64(0))
			if !zeroOrNone(ret) || !zeroOrNone(errcode) {
				if isSessionExpired(response, ret, errcode) {
					pkgLog().Error(fmt.Sprintf("[%s] Session expired; pausing for 10 minutes", a.Name()))
					if err := sleep(ctx, sessionExpiredPauseSeconds); err != nil {
						return err
					}
					consecutiveFailures = 0
					return nil
				}
				consecutiveFailures++
				pkgLog().Warn(fmt.Sprintf("[%s] getUpdates failed ret=%s errcode=%s errmsg=%s (%d/%d)", a.Name(), anyutil.String(ret), anyutil.String(errcode),
					anyutil.String(getOr(response, "errmsg", "")), consecutiveFailures, maxConsecutiveFailures))
				var err error
				consecutiveFailures, err = backoff()
				return err
			}
			consecutiveFailures = 0
			if msgs := response.Value("msgs"); anyutil.IsTrue(msgs) {
				list, ok := msgs.([]any)
				if !ok {
					return fmt.Errorf("getupdates msgs: expected an array, got %s", anyutil.Kind(msgs))
				}
				for _, message := range list {
					gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
						a.processMessageSafe(tctx, message)
						return nil
					})
				}
			}
			if cursor := response.Value("get_updates_buf"); anyutil.IsTrue(cursor) && !anyutil.Equal(anyutil.String(cursor), syncBuf) {
				syncBuf = anyutil.String(cursor)
				return saveSyncBuf(ctx, a.home, a.accountID, anyutil.String(cursor))
			}
			return nil
		}()
		if ctx.Err() != nil {
			break
		}
		if pollErr == nil {
			continue
		}
		consecutiveFailures++
		pkgLog().Error(fmt.Sprintf("[%s] poll error (%d/%d): %s", a.Name(), consecutiveFailures, maxConsecutiveFailures, pollErr.Error()))
		var err error
		if consecutiveFailures, err = backoff(); err != nil {
			break
		}
		if consecutiveFailures == 0 {
			a.recyclePollSession(ctx)
		}
	}
	return nil
}

// Value under key or the default when the key is absent
func getOr(o *jsonutil.Object, key string, def any) any {
	if v, ok := o.Get(key); ok {
		return v
	}
	return def
}

// Swaps in a fresh poll session before closing the old one so no task ever sees a closed session
func (a *WeixinAdapter) recyclePollSession(ctx context.Context) {
	if !a.IsConnected() {
		return
	}
	a.mu.Lock()
	old := a.pollSession
	a.pollSession = newSession(ctx)
	a.mu.Unlock()
	if old != nil && !old.isClosed() {
		old.close()
	}
}

// Processes one inbound message, logging any failure with its sender
func (a *WeixinAdapter) processMessageSafe(ctx context.Context, message any) {
	err := a.processMessage(ctx, message)
	if err == nil || ctx.Err() != nil {
		return
	}
	var from any
	if m, ok := jsonutil.AsObject(message); ok {
		from = m.Value("from_user_id")
	}
	pkgLog().Error(fmt.Sprintf("[%s] unhandled inbound error from=%s: %s", a.Name(), safeID(from), err.Error()), "error", err)
}

// Dedups, gates and converts one inbound message, caching its context token and media before dispatch
func (a *WeixinAdapter) processMessage(ctx context.Context, raw any) error {
	message, ok := jsonutil.AsObject(raw)
	if !ok {
		return fmt.Errorf("inbound message: expected an object, got %s", anyutil.Kind(raw))
	}
	pollSession, _ := a.sessions()
	if pollSession == nil {
		return errSessionClosed
	}
	senderID := textOf(message.Value("from_user_id"))
	messageID := textOf(message.Value("message_id"))
	if senderID == "" || senderID == a.accountID || (messageID != "" && a.dedup.IsDuplicate(messageID)) {
		return nil
	}
	var itemList []any
	if items := message.Value("item_list"); anyutil.IsTrue(items) {
		if itemList, ok = items.([]any); !ok {
			return fmt.Errorf("item_list: expected an array, got %s", anyutil.Kind(items))
		}
	}
	text, err := extractText(itemList)
	if err != nil {
		return err
	}
	if text != "" {
		sum := md5.Sum([]byte(text))
		if a.dedup.IsDuplicate("content:" + senderID + ":" + hex.EncodeToString(sum[:])) {
			pkgLog().Debug(fmt.Sprintf("[%s] Content-dedup: skipping duplicate message from %s", a.Name(), senderID))
			return nil
		}
	}
	chatType, effectiveChatID := guessChatType(message, a.accountID)
	var allowed bool
	if chatType == "group" {
		allowed, err = a.IsGroupAllowed(ctx, effectiveChatID, "")
	} else {
		allowed, err = a.IsDMIntakeAllowed(ctx, senderID)
	}
	if err != nil || !allowed {
		return err
	}
	contextToken := textOf(message.Value("context_token"))
	if contextToken != "" {
		a.tokenStore.set(ctx, a.accountID, senderID, contextToken)
	}
	if session, _ := a.sessions(); session != nil && a.token != "" && a.typingCache.get(senderID) == "" {
		gateway.SpawnAdapterTask(ctx, func(tctx context.Context) error {
			a.fetchTypingTicket(tctx, session, senderID, contextToken, "getConfig failed")
			return nil
		})
	}
	mediaPaths, mediaTypes := []string{}, []string{}
	items, err := itemObjects(itemList)
	if err != nil {
		return err
	}
	for _, item := range items {
		ref, err := objOrEmpty(item.Value("ref_msg"))
		if err != nil {
			return err
		}
		candidates := []*jsonutil.Object{item}
		if refItem, isDict := jsonutil.AsObject(ref.Value("message_item")); isDict {
			candidates = append(candidates, refItem)
		}
		for _, candidate := range candidates {
			path, mime, err := a.collectMedia(ctx, candidate)
			if err != nil {
				return err
			}
			if path != "" {
				mediaPaths, mediaTypes = append(mediaPaths, path), append(mediaTypes, mime)
			}
		}
	}
	if text == "" && len(mediaPaths) == 0 {
		return nil
	}
	source := a.BuildSource(ctx, effectiveChatID, gateway.SourceFields{ChatType: chatType, UserID: &senderID, UserName: &senderID})
	event := gateway.NewMessageEvent(text)
	event.MessageType, event.Source, event.RawMessage = messageTypeFromMedia(mediaTypes, text), source, raw
	if messageID != "" {
		event.MessageID = &messageID
	}
	event.MediaURLs, event.MediaTypes, event.Timestamp = mediaPaths, mediaTypes, time.Now()
	pkgLog().Info(fmt.Sprintf("[%s] inbound from=%s type=%s media=%d", a.Name(), safeID(senderID), source.ChatType, len(mediaPaths)))
	if event.MessageType == gateway.MessageTypeText {
		return a.EnqueueTextEvent(ctx, event)
	}
	return a.HandleMessage(ctx, event)
}

// Cached path and MIME type of one media item, empty for an item type that carries no media
func (a *WeixinAdapter) collectMedia(ctx context.Context, item *jsonutil.Object) (string, string, error) {
	itemType, ok := item.Value("type").(int64)
	if !ok {
		return "", "", nil
	}
	spec, known := inboundMedia[itemType]
	if !known {
		return "", "", nil
	}
	return a.downloadMedia(ctx, item, spec)
}

// Downloads and decrypts one inbound media item into the cache, an empty path and the MIME type when it fails
func (a *WeixinAdapter) downloadMedia(ctx context.Context, item *jsonutil.Object, spec inboundMediaSpec) (string, string, error) {
	payload, err := objOrEmpty(item.Value(spec.itemKey))
	if err != nil {
		return "", "", err
	}
	media, err := objOrEmpty(payload.Value("media"))
	if err != nil {
		return "", "", err
	}
	filename := "document.bin"
	if name := payload.Value("file_name"); anyutil.IsTrue(name) {
		filename = anyutil.String(name)
	}
	mime := spec.mime
	if mime == "" {
		if mime = mimeutil.TypeByName(filename); mime == "" {
			mime = "application/octet-stream"
		}
	}
	path, err := func() (string, error) {
		aesKeyB64 := media.Value("aes_key")
		if rawKey := payload.Value("aeskey"); spec.itemKey == "image_item" && anyutil.IsTrue(rawKey) {
			keyBytes, err := hex.DecodeString(anyutil.String(rawKey))
			if err != nil {
				return "", err
			}
			if encoded := base64.StdEncoding.EncodeToString(keyBytes); encoded != "" {
				aesKeyB64 = encoded
			}
		}
		session, _ := a.sessions()
		if session == nil {
			return "", errSessionClosed
		}
		data, err := downloadAndDecryptMedia(ctx, session, a.cdnBaseURL, media.Value("encrypt_query_param"), aesKeyB64, media.Value("full_url"), spec.timeoutSeconds)
		if err != nil {
			return "", err
		}
		return spec.cache(ctx, data, filename)
	}()
	if err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		pkgLog().Warn(fmt.Sprintf("[%s] %s download failed: %s", a.Name(), spec.label, err.Error()))
		return "", mime, nil
	}
	return path, mime, nil
}

// Fetches and caches the typing ticket of a user, empty when getconfig fails or returns none
func (a *WeixinAdapter) fetchTypingTicket(ctx context.Context, session *ilinkSession, userID, contextToken, failureLabel string) string {
	response, err := getConfig(ctx, session, a.baseURL, a.token, userID, contextToken)
	if err != nil {
		if ctx.Err() == nil {
			pkgLog().Debug(fmt.Sprintf("[%s] %s for %s: %s", a.Name(), failureLabel, safeID(userID), err.Error()))
		}
		return ""
	}
	ticket := strOf(response.Value("typing_ticket"))
	if ticket != "" {
		a.typingCache.set(userID, ticket)
	}
	return ticket
}

// Messages a reply is delivered as under the adapter's length cap and split mode
func (a *WeixinAdapter) splitText(content string) []string {
	return splitTextForWeixinDelivery(content, maxMessageLength, a.splitMultilineMessages)
}

// Seconds the rate limit circuit stays open
func (a *WeixinAdapter) rateLimitCooldownRemaining() float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return math.Max(0.0, a.rateLimitCircuitUntil-monotonic())
}

// Records a rate limit hit and reports whether the circuit is now open
func (a *WeixinAdapter) recordRateLimitEvent() bool {
	now := monotonic()
	a.mu.Lock()
	events := a.rateLimitEvents[:0:0]
	for _, ts := range a.rateLimitEvents {
		if ts >= now-a.rateLimitCircuitWindowSeconds {
			events = append(events, ts)
		}
	}
	a.rateLimitEvents = append(events, now)
	tripped := len(a.rateLimitEvents) >= a.rateLimitCircuitThreshold
	if tripped && a.rateLimitCircuitOpenSeconds > 0 {
		a.rateLimitCircuitUntil = math.Max(a.rateLimitCircuitUntil, monotonic()+a.rateLimitCircuitOpenSeconds)
	}
	a.mu.Unlock()
	return tripped && a.rateLimitCooldownRemaining() > 0
}

// ret and errcode of a send response
func retAndErrcode(resp *jsonutil.Object) (any, any) {
	if resp == nil || resp.Len() == 0 {
		return nil, nil
	}
	return resp.Value("ret"), resp.Value("errcode")
}

// Sends one text chunk under the text gate with retries, a stale session resent once without its context token
func (a *WeixinAdapter) sendTextChunk(ctx context.Context, chatID, chunk, contextToken, clientID string) error {
	a.sendTextGate.Lock()
	defer a.sendTextGate.Unlock()
	var lastError error
	retriedWithoutToken := false
	attempt := 0
	for {
		if remaining := a.rateLimitCooldownRemaining(); remaining > 0 {
			return runtimeError(fmt.Sprintf("iLink sendmessage rate limited; cooldown active for %.1fs", a.rateLimitCooldownRemaining()))
		}
		_, session := a.sessions()
		var resp *jsonutil.Object
		var err error
		if session == nil {
			err = errSessionClosed
		} else {
			resp, err = sendMessage(ctx, session, a.baseURL, a.token, chatID, chunk, contextToken, clientID)
		}
		if err == nil {
			ret, errcode := retAndErrcode(resp)
			if (ret == nil || equalsInt(ret, 0)) && (errcode == nil || equalsInt(errcode, 0)) {
				a.mu.Lock()
				a.rateLimitEvents, a.rateLimitCircuitUntil = nil, 0.0
				a.mu.Unlock()
				return nil
			}
			errmsg := anyutil.Or(resp.Value("errmsg"), resp.Value("msg"))
			if isSessionExpired(resp, ret, errcode) && !retriedWithoutToken && contextToken != "" {
				retriedWithoutToken, contextToken = true, ""
				a.tokenStore.drop(a.accountID, chatID)
				pkgLog().Warn(fmt.Sprintf("[%s] session expired for %s; retrying without context_token", a.Name(), safeID(chatID)))
				continue
			}
			if isStaleSessionRet(ret, errcode, errmsg) {
				lastError = sessionNotReadyError(ret, errcode, errmsg)
				break
			}
			if !equalsInt(ret, rateLimitErrcode) && !equalsInt(errcode, rateLimitErrcode) {
				err = runtimeError(fmt.Sprintf("iLink sendmessage error: ret=%s errcode=%s errmsg=%s", anyutil.String(ret), anyutil.String(errcode),
					anyutil.String(anyutil.Or(errmsg, "unknown error"))))
			} else {
				limited := anyutil.String(anyutil.Or(errmsg, "rate limited"))
				lastError = runtimeError(fmt.Sprintf("iLink sendmessage rate limited: ret=%s errcode=%s errmsg=%s", anyutil.String(ret), anyutil.String(errcode), limited))
				if a.recordRateLimitEvent() {
					lastError = runtimeError(fmt.Sprintf("iLink sendmessage rate limited (ret=%s errcode=%s errmsg=%s); cooldown active for %.1fs",
						anyutil.String(ret), anyutil.String(errcode), limited, a.rateLimitCooldownRemaining()))
					break
				}
				if attempt >= a.sendChunkRetries {
					break
				}
				attempt++
				wait := a.sendChunkRetryDelaySeconds * 3
				pkgLog().Warn(fmt.Sprintf("[%s] rate limited for %s; backing off %.1fs before retry", a.Name(), safeID(chatID), wait))
				if serr := sleep(ctx, wait); serr != nil {
					return serr
				}
				continue
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastError = err
		if attempt >= a.sendChunkRetries {
			break
		}
		attempt++
		wait := a.sendChunkRetryDelaySeconds * float64(attempt)
		pkgLog().Warn(fmt.Sprintf("[%s] send chunk failed to=%s attempt=%d/%d, retrying in %.2fs: %s", a.Name(), safeID(chatID), attempt,
			a.sendChunkRetries+1, wait, a.proxySafeErrText(ctx, err)))
		if wait > 0 {
			if serr := sleep(ctx, wait); serr != nil {
				return serr
			}
		}
	}
	return lastError
}
