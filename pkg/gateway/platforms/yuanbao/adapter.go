package yuanbao

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	defaultWSURL              = "wss://bot-wss.yuanbao.tencent.com/wss/connection"
	defaultAPIDomain          = "https://bot.yuanbao.tencent.com"
	maxTextChunk              = 4000
	dmMaxChars                = 10000
	memberCacheTTL            = 300 * time.Second
	dedupTTLSeconds           = 300
	defaultResolveConcurrency = 6
	maxResolveConcurrency     = 12
	contentCacheSize          = 200
)

var errNotConnected = errors.New("not connected")

// Cached member list of one group with its fetch time
type memberCacheEntry struct {
	at      time.Time
	members []groupMember
}

// Yuanbao bot adapter over a persistent signed websocket
type Adapter struct {
	*gateway.BasePlatformAdapter
	*gateway.OwnAccessPolicyMixin

	appKey, appSecret, wsURL, apiDomain, routeEnv string
	resolveConcurrency                            int
	dedup                                         *gateway.MessageDeduplicator
	conn                                          *connection
	out                                           *outbound

	mu              sync.Mutex
	botID           string
	runCtx          context.Context
	runCancel       context.CancelFunc
	closing         bool
	activeKey       string
	autoSethomeDone bool
	memberCache     map[string]memberCacheEntry
	processingIDs   map[string]string
	processingTexts map[string]string
	contentKeys     []string
	contentCache    map[string]string
	groupQueues     map[string]*groupQueue
	tasks           map[*gateway.AdapterTask]struct{}
}

var (
	_ gateway.PlatformAdapter  = (*Adapter)(nil)
	_ gateway.AttrAdapter      = (*Adapter)(nil)
	_ gateway.DMAllowedAdapter = (*Adapter)(nil)
	_ tools.YuanbaoAdapter     = (*Adapter)(nil)
)

func adapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.SplitsLongMessages = true
	return traits
}

// Trimmed text of a config extra value
func extraString(extra map[string]any, key string) string {
	switch v := extra[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// Allowlist entries of a comma separated string or a YAML list
func allowEntries(raw any) map[string]bool {
	out := map[string]bool{}
	var parts []string
	switch v := raw.(type) {
	case string:
		parts = strings.Split(v, ",")
	case []any:
		for _, item := range v {
			parts = append(parts, fmt.Sprint(item))
		}
	case []string:
		parts = v
	}
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	return out
}

// Media resolve concurrency from extra clamped to one through twelve
func resolveConcurrency(raw any) int {
	n := defaultResolveConcurrency
	switch v := raw.(type) {
	case int:
		n = v
	case int64:
		n = int(v)
	case float64:
		n = int(v)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			n = parsed
		}
	}
	return max(1, min(maxResolveConcurrency, n))
}

// Adapter configured from extra and the profile scoped YUANBAO env
func NewAdapter(ctx context.Context, config *gateway.PlatformConfig) (*Adapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	policy, err := gateway.NewOwnAccessPolicyMixin("YuanbaoAdapter", "YUANBAO")
	if err != nil {
		return nil, err
	}
	a := &Adapter{OwnAccessPolicyMixin: policy, appKey: extraString(extra, "app_id"), appSecret: extraString(extra, "app_secret"),
		botID: extraString(extra, "bot_id"), wsURL: extraString(extra, "ws_url"), routeEnv: extraString(extra, "route_env"),
		apiDomain: strings.TrimRight(extraString(extra, "api_domain"), "/"), resolveConcurrency: resolveConcurrency(extra["media_resolve_concurrency"]),
		dedup: gateway.NewMessageDeduplicator(gateway.DefaultDedupMaxSize, dedupTTLSeconds), memberCache: map[string]memberCacheEntry{},
		processingIDs: map[string]string{}, processingTexts: map[string]string{}, contentCache: map[string]string{},
		groupQueues: map[string]*groupQueue{}, tasks: map[*gateway.AdapterTask]struct{}{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.PlatformYuanbao, a)
	a.Traits = adapterTraits()
	if a.wsURL == "" {
		a.wsURL = defaultWSURL
	}
	if a.apiDomain == "" {
		a.apiDomain = defaultAPIDomain
	}
	for _, kind := range []string{"dm", "group"} {
		upper := strings.ToUpper(kind)
		mode := extraString(extra, kind+"_policy")
		if mode == "" {
			if mode, err = gateway.GetScopedSecret(ctx, "YUANBAO_"+upper+"_POLICY", "", false); err != nil {
				return nil, err
			}
		}
		mode = strings.ToLower(strings.TrimSpace(mode))
		if mode == "" {
			mode = "pairing"
		}
		raw := extra[kind+"_allow_from"]
		if allowEntriesEmpty(raw) {
			envRaw, err := gateway.GetScopedSecret(ctx, "YUANBAO_"+upper+"_ALLOW_FROM", "", false)
			if err != nil {
				return nil, err
			}
			raw = envRaw
		}
		if kind == "dm" {
			a.DMPolicy, a.AllowFrom = mode, allowEntries(raw)
		} else {
			a.GroupPolicy, a.GroupAllowFrom = mode, allowEntries(raw)
		}
	}
	existingHome, err := gateway.GetScopedSecret(ctx, "YUANBAO_HOME_CHANNEL", "", false)
	if err != nil {
		return nil, err
	}
	if existingHome == "" && config.HomeChannel != nil {
		existingHome = config.HomeChannel.ChatID
	}
	a.autoSethomeDone = existingHome != "" && !strings.HasPrefix(existingHome, "group:")
	a.conn = newConnection(a)
	a.out = newOutbound(a)
	return a, nil
}

// Whether an extra allowlist value carries no entries
func allowEntriesEmpty(raw any) bool { return len(allowEntries(raw)) == 0 }

// Bot account id, configured or adopted from the sign token
func (a *Adapter) currentBotID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.botID
}

func (a *Adapter) setBotID(id string) {
	a.mu.Lock()
	a.botID = id
	a.mu.Unlock()
}

// Context every background goroutine of the current connection runs under
func (a *Adapter) backgroundCtx() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.runCtx == nil {
		return context.Background()
	}
	return a.runCtx
}

func (a *Adapter) isClosing() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closing
}

// Runs fn as a tracked task cancelled at disconnect
func (a *Adapter) spawn(fn func(ctx context.Context) error) {
	task := gateway.SpawnAdapterTask(a.backgroundCtx(), fn)
	a.mu.Lock()
	a.tasks[task] = struct{}{}
	a.mu.Unlock()
	task.AddDoneCallback(func() {
		a.mu.Lock()
		delete(a.tasks, task)
		a.mu.Unlock()
	})
}

// Python attributes the runner reads for policy and credential claims
func (a *Adapter) AdapterAttr(name string) any {
	switch name {
	case "_bot_id":
		if id := a.currentBotID(); id != "" {
			return id
		}
		return nil
	}
	return a.OwnAccessPolicyMixin.AdapterAttr(name)
}

// The adapter gates DM and group intake itself
func (a *Adapter) EnforcesOwnAccessPolicy() bool { return true }

// Group intake, an open group still requiring the allow all users opt in
func (a *Adapter) IsGroupAllowed(ctx context.Context, chatID, senderID string) (bool, error) {
	if a.GroupPolicy == "open" {
		return a.OpenDMOptedIn(ctx)
	}
	return a.OwnAccessPolicyMixin.IsGroupAllowed(ctx, strings.TrimSpace(chatID), senderID)
}

// Signs in, binds the websocket and starts the heartbeat and receive loops
func (a *Adapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.appKey == "" || a.appSecret == "" {
		msg := "Yuanbao startup failed: YUANBAO_APP_ID and YUANBAO_APP_SECRET are required"
		slog.Error("[Yuanbao] " + msg)
		a.SetFatalError(ctx, "yuanbao_missing_credentials", msg, false)
		return false, nil
	}
	if a.conn.connected() {
		return true, nil
	}
	acquired, err := a.AcquirePlatformLock(ctx, "yuanbao-app-key", a.appKey, "Yuanbao app key")
	if err != nil || !acquired {
		return false, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a.mu.Lock()
	a.runCtx, a.runCancel, a.closing = runCtx, cancel, false
	a.activeKey = home.Key(home.Dir(ctx))
	a.mu.Unlock()
	slog.Info(fmt.Sprintf("[Yuanbao] Fetching sign token from %s", a.apiDomain))
	if err := a.conn.open(runCtx); err != nil {
		slog.Error(fmt.Sprintf("[Yuanbao] connect failed: %v", err))
		cancel()
		a.ReleasePlatformLock()
		return false, nil
	}
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Publishes the bound connection, starts its loops and registers the adapter for tools and standalone sends
func (a *Adapter) markBound(ctx context.Context) {
	a.MarkConnected(ctx, "")
	a.conn.startLoops()
	a.mu.Lock()
	key := a.activeKey
	a.mu.Unlock()
	setActiveAdapter(key, a)
	slog.Info(fmt.Sprintf("[Yuanbao] Connected. connectId=%s botId=%s", a.conn.currentConnectID(), a.currentBotID()))
}

// Stops every loop and task and closes the websocket
func (a *Adapter) Disconnect(ctx context.Context) error {
	clearActiveAdapter(a)
	a.mu.Lock()
	a.closing = true
	cancel := a.runCancel
	tasks := make([]*gateway.AdapterTask, 0, len(a.tasks))
	for t := range a.tasks {
		tasks = append(tasks, t)
	}
	a.tasks = map[*gateway.AdapterTask]struct{}{}
	a.groupQueues = map[string]*groupQueue{}
	a.mu.Unlock()
	a.MarkDisconnected(ctx)
	a.ReleasePlatformLock()
	a.conn.close()
	a.out.close()
	for _, t := range tasks {
		t.Cancel()
	}
	if cancel != nil {
		cancel()
	}
	slog.Info("[Yuanbao] Disconnected")
	return nil
}

// Text reply chunked at 4000 characters, mentions resolved in groups
func (a *Adapter) Send(ctx context.Context, chatID, content, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.out.sendText(ctx, chatID, content, replyTo, ""), nil
}

func (a *Adapter) GetChatInfo(ctx context.Context, chatID string) (map[string]any, error) {
	kind := "dm"
	if strings.HasPrefix(chatID, "group:") {
		kind = "group"
	}
	return map[string]any{"name": chatID, "type": kind}, nil
}

// Starts or renews the RUNNING reply heartbeat
func (a *Adapter) SendTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.out.startHeartbeat(chatID)
	return nil
}

// Stops the RUNNING heartbeat without FINISH, the text send emitting FINISH after delivery
func (a *Adapter) StopTyping(ctx context.Context, chatID string, metadata map[string]any) error {
	a.out.stopHeartbeat(chatID, false)
	return nil
}

// Agent turn wrapped with the slow response notice and recall tracking cleanup
func (a *Adapter) ProcessMessageBackground(ctx context.Context, event *gateway.MessageEvent, sessionKey string) error {
	chatID := ""
	if event.Source != nil {
		chatID = event.Source.ChatID
	}
	a.out.startSlowNotice(chatID)
	defer func() {
		a.out.cancelSlowNotice(chatID)
		if event.MessageID == nil || *event.MessageID == "" {
			return
		}
		a.mu.Lock()
		if a.processingIDs[sessionKey] == *event.MessageID {
			delete(a.processingIDs, sessionKey)
			delete(a.processingTexts, sessionKey)
		}
		a.mu.Unlock()
	}()
	return a.BasePlatformAdapter.ProcessMessageBackground(ctx, event, sessionKey)
}

// Image fetched from a URL, uploaded to COS and sent as an image element
func (a *Adapter) SendImage(ctx context.Context, chatID, imageURL, caption, replyTo string, metadata map[string]any) (gateway.SendResult, error) {
	return a.out.sendMedia(ctx, chatID, mediaSource{imageURL: imageURL}, caption, replyTo, ""), nil
}

// Local image uploaded to COS and sent as an image element
func (a *Adapter) SendImageFile(ctx context.Context, chatID, imagePath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.out.sendMedia(ctx, chatID, mediaSource{path: imagePath, image: true}, caption, replyTo, ""), nil
}

// Local file uploaded to COS and sent as a file element
func (a *Adapter) SendDocument(ctx context.Context, chatID, filePath, caption, fileName, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.out.sendMedia(ctx, chatID, mediaSource{path: filePath, fileName: fileName}, caption, replyTo, ""), nil
}

// Video file delivered as a downloadable file element
func (a *Adapter) SendVideo(ctx context.Context, chatID, videoPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.out.sendMedia(ctx, chatID, mediaSource{path: videoPath}, caption, replyTo, ""), nil
}

// Audio file delivered as a downloadable file element
func (a *Adapter) SendVoice(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (gateway.SendResult, error) {
	return a.out.sendMedia(ctx, chatID, mediaSource{path: audioPath}, caption, replyTo, ""), nil
}

// Group profile fetched over the websocket
func (a *Adapter) queryGroupInfo(ctx context.Context, groupCode string) (groupInfo, error) {
	frame, reqID := encodeQueryGroupInfo(groupCode)
	data, err := a.conn.bizQuery(ctx, "query_group_info", groupCode, frame, reqID)
	if err != nil || len(data) == 0 {
		return groupInfo{}, err
	}
	status, info, err := decodeQueryGroupInfoRsp(data)
	if err != nil {
		return groupInfo{}, fmt.Errorf("query_group_info response: %w", err)
	}
	if status.Code != 0 {
		return groupInfo{}, fmt.Errorf("query_group_info failed: code=%d message=%s", status.Code, status.Message)
	}
	return info, nil
}

// Group members fetched over the websocket and cached for mention resolution
func (a *Adapter) groupMembers(ctx context.Context, groupCode string) ([]groupMember, error) {
	frame, reqID := encodeGetGroupMemberList(groupCode)
	data, err := a.conn.bizQuery(ctx, "get_group_member_list", groupCode, frame, reqID)
	if err != nil || len(data) == 0 {
		return nil, err
	}
	status, members, err := decodeGroupMemberListRsp(data)
	if err != nil {
		return nil, fmt.Errorf("get_group_member_list response: %w", err)
	}
	if status.Code != 0 {
		return nil, fmt.Errorf("get_group_member_list failed: code=%d message=%s", status.Code, status.Message)
	}
	if len(members) > 0 {
		a.mu.Lock()
		a.memberCache[groupCode] = memberCacheEntry{at: time.Now(), members: members}
		a.mu.Unlock()
	}
	return members, nil
}

// Unexpired cached members of a group, nil once stale
func (a *Adapter) cachedMembers(groupCode string) []groupMember {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.memberCache[groupCode]
	if ok && time.Since(entry.at) >= memberCacheTTL {
		delete(a.memberCache, groupCode)
		return nil
	}
	return entry.members
}

// Error unless the DM access policy allows the user
func (a *Adapter) checkDMAllowed(ctx context.Context, userID string) error {
	allowed, err := a.IsDMAllowed(ctx, strings.TrimSpace(userID))
	if err != nil {
		return err
	}
	if !allowed {
		return errors.New("DM access denied for this user")
	}
	return nil
}

// Proactive DM capped at 10000 characters, group code marking a DM started from a group
func (a *Adapter) sendDM(ctx context.Context, userID, text, groupCode string) gateway.SendResult {
	if err := a.checkDMAllowed(ctx, userID); err != nil {
		return gateway.SendResult{Error: err.Error()}
	}
	if r := []rune(text); len(r) > dmMaxChars {
		text = string(r[:dmMaxChars]) + i18n.T(ctx, "platform.shared.truncated_suffix", "", nil)
	}
	return a.out.sendText(ctx, "direct:"+userID, text, "", groupCode)
}

// Message id text of a send result, empty when the platform returned none
func resultMessageID(r gateway.SendResult) string {
	if s, ok := r.MessageID.(string); ok {
		return s
	}
	return ""
}

// Error of a failed send result
func resultError(r gateway.SendResult) error {
	if r.Success {
		return nil
	}
	if s, ok := r.Error.(string); ok && s != "" {
		return errors.New(s)
	}
	return errors.New("send failed")
}

// Group profile for the yb_query_group_info tool
func (a *Adapter) QueryGroupInfo(ctx context.Context, groupCode string) (tools.YuanbaoGroupInfo, error) {
	info, err := a.queryGroupInfo(ctx, groupCode)
	if err != nil {
		return tools.YuanbaoGroupInfo{}, err
	}
	return tools.YuanbaoGroupInfo{Name: info.Name, OwnerID: info.OwnerID, OwnerNickname: info.OwnerNickname, MemberCount: info.MemberCount}, nil
}

// Group members for the member query and DM tools
func (a *Adapter) GroupMembers(ctx context.Context, groupCode string) ([]tools.YuanbaoMember, error) {
	members, err := a.groupMembers(ctx, groupCode)
	if err != nil {
		return nil, err
	}
	out := make([]tools.YuanbaoMember, 0, len(members))
	for _, m := range members {
		out = append(out, tools.YuanbaoMember{UserID: m.UserID, Nickname: m.Nickname, UserType: m.UserType})
	}
	return out, nil
}

// Text DM for the yb_send_dm tool
func (a *Adapter) SendDM(ctx context.Context, userID, text, groupCode string) (string, error) {
	r := a.sendDM(ctx, userID, text, groupCode)
	return resultMessageID(r), resultError(r)
}

// Image or file DM for the yb_send_dm tool, images by extension
func (a *Adapter) SendDMFile(ctx context.Context, userID, path, groupCode string) (string, error) {
	if err := a.checkDMAllowed(ctx, userID); err != nil {
		return "", err
	}
	r := a.out.sendMedia(ctx, "direct:"+userID, mediaSource{path: path, image: sendsAsImage(path)}, "", "", groupCode)
	return resultMessageID(r), resultError(r)
}

// Builtin sticker for the yb_send_sticker tool
func (a *Adapter) SendSticker(ctx context.Context, chatID string, s tools.YuanbaoSticker, replyTo string) (string, error) {
	found, ok := stickerByID(s.ID)
	if !ok {
		return "", fmt.Errorf("sticker not found: %s", s.ID)
	}
	r := a.out.sendMedia(ctx, chatID, mediaSource{sticker: &found}, "", replyTo, "")
	return resultMessageID(r), resultError(r)
}

var imageSendExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true}

// Whether a local file goes out as an image element
func sendsAsImage(path string) bool { return imageSendExts[fileExt(path)] }

// Text first and then each media file on the live adapter, the send_message and cron entry point
func (a *Adapter) sendDirect(ctx context.Context, chatID, message string, media []gateway.MediaRef) map[string]any {
	var last *gateway.SendResult
	if strings.TrimSpace(message) != "" {
		r, _ := a.Send(ctx, chatID, message, "", nil)
		if !r.Success {
			return map[string]any{"error": "Yuanbao send failed: " + resultError(r).Error()}
		}
		last = &r
	}
	for _, m := range media {
		r := a.out.sendMedia(ctx, chatID, mediaSource{path: m.Path, image: sendsAsImage(m.Path)}, "", "", "")
		if !r.Success {
			return map[string]any{"error": "Yuanbao media send failed: " + resultError(r).Error()}
		}
		last = &r
	}
	if last == nil {
		return map[string]any{"error": "No deliverable text or media remained after processing"}
	}
	return map[string]any{"success": true, "platform": "yuanbao", "chat_id": chatID, "message_id": last.MessageID}
}

// Remembers recent inbound texts by message id for recall redaction and quote media lookup
func (a *Adapter) rememberContent(msgID, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.contentCache[msgID]; !ok {
		a.contentKeys = append(a.contentKeys, msgID)
	}
	a.contentCache[msgID] = text
	for len(a.contentKeys) > contentCacheSize {
		delete(a.contentCache, a.contentKeys[0])
		a.contentKeys = a.contentKeys[1:]
	}
}

func (a *Adapter) rememberedContent(msgID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.contentCache[msgID]
}

// Live adapters by profile home key for tools and standalone sends
var activeAdapters = struct {
	mu sync.Mutex
	m  map[string]*Adapter
}{m: map[string]*Adapter{}}

func setActiveAdapter(key string, a *Adapter) {
	activeAdapters.mu.Lock()
	activeAdapters.m[key] = a
	activeAdapters.mu.Unlock()
}

func clearActiveAdapter(a *Adapter) {
	activeAdapters.mu.Lock()
	defer activeAdapters.mu.Unlock()
	for k, v := range activeAdapters.m {
		if v == a {
			delete(activeAdapters.m, k)
		}
	}
}

// Live adapter serving a profile home, nil when none is connected there
func activeAdapter(profileHome string) *Adapter {
	key := home.Key(profileHome)
	activeAdapters.mu.Lock()
	defer activeAdapters.mu.Unlock()
	return activeAdapters.m[key]
}
