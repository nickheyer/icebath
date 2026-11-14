package yuanbao

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	defaultMemberListLimit = 200
	dedupTTLSeconds        = 300
)

// Members of one group with the wall clock time they were fetched
type memberCacheEntry struct {
	ts      float64
	members []any
}

// YuanbaoAdapter is the Yuanbao bot adapter over a persistent signed websocket connection
type YuanbaoAdapter struct {
	*gateway.BasePlatformAdapter

	MediaResolveConcurrency int

	appKey          string
	appSecret       string
	wsURL           string
	apiDomain       string
	routeEnv        string
	memberCacheTTL  float64
	connection      *connectionManager
	outbound        *outboundManager
	accessPolicy    *AccessPolicy
	inboundPipeline *InboundPipeline
	dedup           *gateway.MessageDeduplicator

	mu                 sync.Mutex
	botID              any
	autoSethomeDone    bool
	inboundTasks       map[*gateway.AdapterTask]struct{}
	memberCache        map[string]memberCacheEntry
	groupQueues        map[string]*groupQueue
	processingMsgIDs   *pydict.Dict[string, string]
	processingMsgTexts map[string]string
	msgContentCache    *pydict.Dict[string, string]
}

// Running adapter of the unscoped process and of each profile home
var activeAdapters = struct {
	sync.Mutex
	unscoped *YuanbaoAdapter
	byHome   map[string]*YuanbaoAdapter
}{byHome: map[string]*YuanbaoAdapter{}}

// Running adapter of a home override, the unscoped one for an empty override
func activeFor(override string) *YuanbaoAdapter {
	activeAdapters.Lock()
	defer activeAdapters.Unlock()
	if override == "" {
		return activeAdapters.unscoped
	}
	return activeAdapters.byHome[home.Key(override)]
}

// Running adapter of the profile home the context is scoped to, the unscoped one outside a profile scope
func GetActive(ctx context.Context) *YuanbaoAdapter { return activeFor(home.Override(ctx)) }

// Registers the running adapter under the profile home of the context, nil clearing that slot
func SetActive(ctx context.Context, adapter *YuanbaoAdapter) {
	override := home.Override(ctx)
	activeAdapters.Lock()
	defer activeAdapters.Unlock()
	switch {
	case override == "":
		activeAdapters.unscoped = adapter
	case adapter == nil:
		delete(activeAdapters.byHome, home.Key(override))
	default:
		activeAdapters.byHome[home.Key(override)] = adapter
	}
}

// Drops the adapter from every slot it is registered in
func clearActive(adapter *YuanbaoAdapter) {
	activeAdapters.Lock()
	defer activeAdapters.Unlock()
	if activeAdapters.unscoped == adapter {
		activeAdapters.unscoped = nil
	}
	for key, active := range activeAdapters.byHome {
		if active == adapter {
			delete(activeAdapters.byHome, key)
		}
	}
}

// Class attributes the Yuanbao adapter declares over the base defaults
func adapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.SplitsLongMessages = true
	return traits
}

// Media resolve concurrency of the extra clamped to its bounds, the default for a value that is not an integer
func resolveConcurrency(extra map[string]any) (int, error) {
	raw, configured := extra["media_resolve_concurrency"]
	if !configured {
		return defaultResolveConcurrency, nil
	}
	n, err := pyint.Of(raw)
	var conversion *pyint.Error
	switch {
	case errors.As(err, &conversion) && (conversion.Kind == "TypeError" || conversion.Kind == "ValueError"):
		return defaultResolveConcurrency, nil
	case errors.As(err, &conversion):
		return 0, &pyErr{conversion.Kind, conversion.Msg}
	case err != nil:
		return 0, err
	case n.Cmp(big.NewInt(minResolveConcurrency)) < 0:
		return minResolveConcurrency, nil
	case n.Cmp(big.NewInt(maxResolveConcurrency)) > 0:
		return maxResolveConcurrency, nil
	}
	return int(n.Int64()), nil
}

// Access policy and allowlist of DMs or groups from the extra, then the scoped env, pairing when neither names one
func policySetting(ctx context.Context, extra map[string]any, kind string) (string, []string, error) {
	prefix := "YUANBAO_" + pystr.Upper(kind)
	policy := extra[kind+"_policy"]
	if !pyrepr.Truthy(policy) {
		scoped, err := gateway.GetScopedSecret(ctx, prefix+"_POLICY", "", false)
		if err != nil {
			return "", nil, err
		}
		policy = scoped
	}
	policyText, err := strMethod(pyrepr.Or(policy, "pairing"), "strip")
	if err != nil {
		return "", nil, err
	}
	raw := extra[kind+"_allow_from"]
	if !pyrepr.Truthy(raw) {
		scoped, err := gateway.GetScopedSecret(ctx, prefix+"_ALLOW_FROM", "", false)
		if err != nil {
			return "", nil, err
		}
		raw = scoped
	}
	rawText, err := strMethod(raw, "split")
	if err != nil {
		return "", nil, err
	}
	allowFrom := []string{}
	for _, entry := range strings.Split(rawText, ",") {
		if entry = pystr.Strip(entry); entry != "" {
			allowFrom = append(allowFrom, entry)
		}
	}
	return pystr.Lower(pystr.Strip(policyText)), allowFrom, nil
}

// Adapter for a platform config reading credentials, endpoints, access policies and the home channel from extra then env
func NewYuanbaoAdapter(ctx context.Context, config *gateway.PlatformConfig) (*YuanbaoAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	a := &YuanbaoAdapter{memberCacheTTL: memberCacheTTLS, dedup: gateway.NewMessageDeduplicator(gateway.DefaultDedupMaxSize, dedupTTLSeconds),
		inboundTasks: map[*gateway.AdapterTask]struct{}{}, memberCache: map[string]memberCacheEntry{}, groupQueues: map[string]*groupQueue{},
		processingMsgIDs: pydict.New[string, string](), processingMsgTexts: map[string]string{}, msgContentCache: pydict.New[string, string]()}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformYuanbao, a)
	a.Traits = adapterTraits()
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	setting := func(key string, def any, method string) (string, error) {
		return strMethod(pyrepr.Or(extra[key], def), method)
	}
	var err error
	if a.appKey, err = setting("app_id", "", "strip"); err != nil {
		return nil, err
	}
	if a.appSecret, err = setting("app_secret", "", "strip"); err != nil {
		return nil, err
	}
	if botID := extra["bot_id"]; pyrepr.Truthy(botID) {
		a.botID = botID
	}
	if a.wsURL, err = setting("ws_url", defaultWSGatewayURL, "strip"); err != nil {
		return nil, err
	}
	if a.apiDomain, err = setting("api_domain", defaultBotAPIDomain, "rstrip"); err != nil {
		return nil, err
	}
	if a.routeEnv, err = setting("route_env", "", "strip"); err != nil {
		return nil, err
	}
	a.appKey, a.appSecret, a.wsURL, a.routeEnv = pystr.Strip(a.appKey), pystr.Strip(a.appSecret), pystr.Strip(a.wsURL), pystr.Strip(a.routeEnv)
	a.apiDomain = pystr.RstripChars(a.apiDomain, "/")
	if a.MediaResolveConcurrency, err = resolveConcurrency(extra); err != nil {
		return nil, err
	}
	a.connection = newConnectionManager(a)
	a.outbound = newOutboundManager(a)
	dmPolicy, dmAllowFrom, err := policySetting(ctx, extra, "dm")
	if err != nil {
		return nil, err
	}
	groupPolicy, groupAllowFrom, err := policySetting(ctx, extra, "group")
	if err != nil {
		return nil, err
	}
	if a.accessPolicy, err = NewAccessPolicy(dmPolicy, dmAllowFrom, groupPolicy, groupAllowFrom); err != nil {
		return nil, err
	}
	a.inboundPipeline = BuildInboundPipeline()
	existingHome, err := gateway.GetScopedSecret(ctx, "YUANBAO_HOME_CHANNEL", "", false)
	if err != nil {
		return nil, err
	}
	if existingHome == "" && config.HomeChannel != nil {
		existingHome = config.HomeChannel.ChatID
	}
	a.autoSethomeDone = existingHome != "" && !strings.HasPrefix(existingHome, "group:")
	return a, nil
}

// Bot account id, nil until the config or a sign token names one
func (a *YuanbaoAdapter) botIDValue() any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.botID
}

func (a *YuanbaoAdapter) setBotID(botID string) {
	a.mu.Lock()
	a.botID = botID
	a.mu.Unlock()
}

// Whether a DM already settled the home channel so no later conversation claims it
func (a *YuanbaoAdapter) autoSethomeDoneValue() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.autoSethomeDone
}

func (a *YuanbaoAdapter) setAutoSethomeDone(done bool) {
	a.mu.Lock()
	a.autoSethomeDone = done
	a.mu.Unlock()
}

// Text of a recently dispatched message by its id, empty when the cache no longer holds it
func (a *YuanbaoAdapter) cachedMsgContent(msgID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	text, _ := a.msgContentCache.Get(msgID)
	return text
}

// Session key and message id of every turn in progress, oldest first
func (a *YuanbaoAdapter) processingSnapshot() [][2]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	pairs := make([][2]string, 0, a.processingMsgIDs.Len())
	for sessionKey, msgID := range a.processingMsgIDs.All() {
		pairs = append(pairs, [2]string{sessionKey, msgID})
	}
	return pairs
}

// Text of the message a session is processing, empty when it processes none
func (a *YuanbaoAdapter) processingText(sessionKey string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.processingMsgTexts[sessionKey]
}

// Forgets the message a session was processing unless a newer message took its place
func (a *YuanbaoAdapter) clearProcessing(sessionKey, msgID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if current, tracked := a.processingMsgIDs.Get(sessionKey); tracked && current == msgID {
		a.processingMsgIDs.Delete(sessionKey)
		delete(a.processingMsgTexts, sessionKey)
	}
}

// Members cached for a group, the entry dropped once it outlived the cache window
func (a *YuanbaoAdapter) cachedMembers(groupCode string) []any {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, cached := a.memberCache[groupCode]
	if !cached {
		return nil
	}
	if wallClock()-entry.ts >= a.memberCacheTTL {
		delete(a.memberCache, groupCode)
		return nil
	}
	return entry.members
}

// Tracks a detached task for cancellation at shutdown until it finishes
func (a *YuanbaoAdapter) trackTask(task *gateway.AdapterTask) { a.TrackBackgroundTask(task) }

// Valid sign token of the app from the process wide cache
func (a *YuanbaoAdapter) getCachedToken(ctx context.Context) (map[string]any, error) {
	return signGetToken(ctx, a.appKey, a.appSecret, a.apiDomain, a.routeEnv)
}

// The adapter gates intake through its own access policy
func (a *YuanbaoAdapter) EnforcesOwnAccessPolicy() bool { return true }

// Whether the pairing store approved a sender, a package var the way hermes tests patch PairingStore
var pairingApprovedFn = func(ctx context.Context, sender string) (bool, error) {
	store, err := gateway.NewPairingStore(ctx, "")
	if err != nil {
		return false, err
	}
	return store.IsApproved(ctx, string(gateway.PlatformYuanbao), sender)
}

// Whether the sender may persist the home channel, by strict allowlist, open opt in or pairing approval
func (a *YuanbaoAdapter) senderMayDesignateHome(ctx context.Context, ic *InboundContext) (bool, error) {
	policy := a.accessPolicy
	sender := stripOrEmpty(ic.FromAccount)
	if sender == "" {
		return false, nil
	}
	if ic.ChatType == "dm" {
		allowed, err := policy.IsDMAllowed(ctx, sender)
		if err != nil || allowed {
			return allowed, err
		}
		if policy.DMPolicy == "pairing" {
			return pairingApprovedFn(ctx, sender)
		}
		return false, nil
	}
	groupCode := stripOrEmpty(ic.GroupCode)
	if ic.ChatType != "group" || groupCode == "" {
		return false, nil
	}
	if policy.GroupPolicy == "allowlist" {
		return policy.IsGroupAllowed(ctx, groupCode)
	}
	if policy.GroupPolicy != "open" {
		return false, nil
	}
	return policy.OpenDMOptedIn(ctx)
}

// Opens the signed connection and wires the plugin registered native handlers
func (a *YuanbaoAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	connected, err := a.connection.open(ctx)
	if err != nil || !connected {
		return false, err
	}
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Unregisters the adapter, releases its lock, closes the connection and cancels the inbound tasks
func (a *YuanbaoAdapter) Disconnect(ctx context.Context) error {
	clearActive(a)
	a.SetRunning(false)
	a.MarkDisconnected(ctx)
	a.ReleasePlatformLock()
	a.connection.close(ctx)
	a.outbound.close()
	a.mu.Lock()
	inbound := a.inboundTasks
	a.inboundTasks = map[*gateway.AdapterTask]struct{}{}
	clear(a.groupQueues)
	a.mu.Unlock()
	for task := range inbound {
		if !task.Done() {
			task.Cancel()
		}
	}
	slog.Info(fmt.Sprintf("[%s] Disconnected", a.Name()))
	return nil
}

// Sends text to a chat in ordered chunks
func (a *YuanbaoAdapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.outbound.sender.sendText(ctx, chatID, content, replyTo, "")
}

// Name and type of a chat from its id
func (a *YuanbaoAdapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	chatType := "dm"
	if strings.HasPrefix(chatID, "group:") {
		chatType = "group"
	}
	return map[string]any{"name": chatID, "type": chatType}, nil
}

// Starts the running heartbeat of the chat
func (a *YuanbaoAdapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.outbound.heartbeat.start(ctx, chatID)
	return nil
}

// Stops the running heartbeat without a finish, the send that follows emitting it after delivery
func (a *YuanbaoAdapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.outbound.heartbeat.stop(ctx, chatID)
	return nil
}

// Turn processing of the base adapter, a package var the way hermes tests patch the base method
var baseProcessMessageBackground = func(ctx context.Context, adapter *YuanbaoAdapter, event *gateway.MessageEvent, sessionKey string) error {
	return adapter.BasePlatformAdapter.ProcessMessageBackground(ctx, event, sessionKey)
}

// Runs the base turn processing under the slow response notifier and clears the recall tracking the turn still owns
func (a *YuanbaoAdapter) ProcessMessageBackground(ctx context.Context, event *gateway.MessageEvent, sessionKey string) error {
	chatID := event.Source.ChatID
	a.outbound.slowNotifier.start(ctx, chatID)
	defer func() {
		a.outbound.slowNotifier.cancel(chatID)
		if event.MessageID != nil && *event.MessageID != "" {
			a.clearProcessing(sessionKey, *event.MessageID)
		}
	}()
	return baseProcessMessageBackground(ctx, a, event, sessionKey)
}

// Sends an encoded group query over the socket, the empty answer for a bare response and nil on failure
func (a *YuanbaoAdapter) wsQuery(ctx context.Context, label, groupCode string, encoded []byte, decode func([]byte) *pyjson.Object, empty *pyjson.Object) (*pyjson.Object, error) {
	if a.connection.socket() == nil {
		return nil, nil
	}
	name := a.Name()
	result, err := func() (*pyjson.Object, error) {
		request, err := DecodeConnMsg(encoded)
		if err != nil {
			return nil, err
		}
		requestHead, err := dictOf(request.GetOr("head", nil))
		if err != nil {
			return nil, err
		}
		response, err := a.connection.sendBizRequest(ctx, encoded, pyrepr.Str(requestHead["msg_id"]), defaultSendTimeout)
		if err != nil {
			return nil, err
		}
		fields, err := dictOf(response)
		if err != nil {
			return nil, err
		}
		head, err := dictOf(dictGetOr(fields, "head", pyjson.NewObject()))
		if err != nil {
			return nil, err
		}
		if status := dictGetOr(head, "status", int64(0)); !equalsInt(status, 0) {
			slog.Warn(fmt.Sprintf("[%s] %s failed: status=%s", name, label, pyrepr.Str(status)))
			return nil, nil
		}
		bizData, isBytes := pyrepr.Or(dictGetOr(fields, "data", []byte{}), dictGetOr(fields, "body", []byte{})).([]byte)
		if isBytes && len(bizData) > 0 {
			return decode(bizData), nil
		}
		return empty, nil
	}()
	switch {
	case err == nil:
		return result, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case isTimeout(err):
		slog.Warn(fmt.Sprintf("[%s] %s timeout: group=%s", name, label, groupCode))
	default:
		slog.Warn(fmt.Sprintf("[%s] %s failed: %s", name, label, errText(err)))
	}
	return nil, nil
}

// Name, owner and member count of a group, nil on failure
func (a *YuanbaoAdapter) QueryGroupInfo(ctx context.Context, groupCode string) (*pyjson.Object, error) {
	encoded, err := EncodeQueryGroupInfo(groupCode)
	if err != nil {
		return nil, err
	}
	return a.wsQuery(ctx, "query_group_info", groupCode, encoded, DecodeQueryGroupInfoRsp, pyjson.ObjectOf("group_code", groupCode))
}

// Member list page of a group, nil on failure, its members cached for mention resolution
func (a *YuanbaoAdapter) GetGroupMemberList(ctx context.Context, groupCode string, offset, limit int64) (*pyjson.Object, error) {
	encoded, err := EncodeGetGroupMemberList(groupCode, offset, limit)
	if err != nil {
		return nil, err
	}
	result, err := a.wsQuery(ctx, "get_group_member_list", groupCode, encoded, DecodeGetGroupMemberListRsp,
		pyjson.ObjectOf("members", []any{}, "next_offset", int64(0), "is_complete", true))
	if err != nil || result == nil {
		return nil, err
	}
	if members, isList := result.GetOr("members", nil).([]any); isList && len(members) > 0 {
		a.mu.Lock()
		a.memberCache[groupCode] = memberCacheEntry{wallClock(), members}
		a.mu.Unlock()
	}
	return result, nil
}

// Sends a proactive direct message to a user the DM policy allows, long text cut at the DM cap
func (a *YuanbaoAdapter) SendDM(ctx context.Context, userID, text string, groupCode any) (gateway.SendResult, error) {
	allowed, err := a.accessPolicy.IsDMAllowed(ctx, userID)
	if err != nil {
		return gateway.SendResult{}, err
	}
	if !allowed {
		return gateway.SendResult{Success: false, Error: "DM access denied for this user"}, nil
	}
	if pystr.Len(text) > dmMaxChars {
		text = pystr.Slice(text, 0, dmMaxChars) + i18n.T(ctx, "platform.shared.truncated_suffix", "", nil)
	}
	return a.outbound.sender.sendText(ctx, "direct:"+userID, text, "", groupCode)
}

// Keyword arguments of a media send, the caller extras under the named ones
func mediaKwargs(extra map[string]any, named ...any) map[string]any {
	kwargs := make(map[string]any, len(extra)+len(named)/2)
	for key, value := range extra {
		kwargs[key] = value
	}
	for i := 0; i+1 < len(named); i += 2 {
		kwargs[named[i].(string)] = named[i+1]
	}
	return kwargs
}

// Downloads an image URL and sends it as an image message
func (a *YuanbaoAdapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return handleMediaSend(ctx, imageURLHandler{}, a, chatID, replyTo, caption, mediaKwargs(nil, "image_url", imageURL))
}

// Sends a local image file as an image message
func (a *YuanbaoAdapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return handleMediaSend(ctx, imageFileHandler{}, a, chatID, replyTo, caption, mediaKwargs(extra, "image_path", imagePath))
}

// Sends the named sticker, else the face of the index, else a random sticker
func (a *YuanbaoAdapter) SendSticker(ctx context.Context, chatID string, stickerName *string, faceIndex *int, replyTo string, extra map[string]any) (gateway.SendResult, error) {
	kwargs := mediaKwargs(extra)
	delete(kwargs, "sticker_name")
	delete(kwargs, "face_index")
	if stickerName != nil {
		kwargs["sticker_name"] = *stickerName
	}
	if faceIndex != nil {
		kwargs["face_index"] = *faceIndex
	}
	return handleMediaSend(ctx, stickerHandler{}, a, chatID, replyTo, "", kwargs)
}

// Sends a local file as a file message under the given name
func (a *YuanbaoAdapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	kwargs := mediaKwargs(extra, "file_path", filePath)
	delete(kwargs, "filename")
	if fileName != "" {
		kwargs["filename"] = fileName
	}
	return handleMediaSend(ctx, documentHandler{}, a, chatID, replyTo, caption, kwargs)
}
