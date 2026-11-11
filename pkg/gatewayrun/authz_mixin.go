package gatewayrun

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

var (
	groupChatTypes  = map[string]bool{"group": true, "forum": true, "channel": true}
	groupForumTypes = map[string]bool{"group": true, "forum": true}
	groupUserEnv    = map[gateway.Platform]string{gateway.PlatformTelegram: "TELEGRAM_GROUP_ALLOWED_USERS"}
	botGuardInitMu  sync.Mutex
)

// Scoped gate environment value, empty when unset
func authEnv(ctx context.Context, name string) string {
	return gateway.PlatformGateEnv(ctx, name, "")
}

// Whether a scoped gate environment value is a true token
func envTruthy(ctx context.Context, name string) bool {
	switch pystr.Lower(authEnv(ctx, name)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// Authorization field of a platform entry as text, empty when it is unset
func authzFieldText(platform gateway.Platform, field string, entry *gateway.PlatformEntry) string {
	value := gateway.AuthzField(string(platform), field, entry)
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pyrepr.Str(value)
}

// Group chat allowlist environment name of a platform, empty when it declares none
func groupChatEnv(ctx context.Context, platform gateway.Platform, entry *gateway.PlatformEntry) string {
	if platform == "" {
		return ""
	}
	if entry == nil {
		entry = registryEntry(ctx, platform)
	}
	return authzFieldText(platform, "group_allowed_chats_env", entry)
}

// Platform registry entry of a plugin platform after an idempotent discovery, nil when none is registered
func registryEntry(ctx context.Context, platform gateway.Platform) *gateway.PlatformEntry {
	if platform == "" {
		return nil
	}
	func() {
		defer func() { _ = recover() }()
		plugins.DiscoverPlugins(ctx, false)
	}()
	return gateway.Registry.Get(ctx, string(platform))
}

// Allowlist entries of a YAML list, a JSON list literal or a comma separated scalar
func coerceAllowSet(raw any) map[string]bool {
	out := map[string]bool{}
	if raw == nil {
		return out
	}
	raw = gateway.DecodeJSONListLiteral(raw)
	if items, ok := raw.([]any); ok {
		for _, item := range items {
			if part := pystr.Strip(pyrepr.Str(item)); part != "" {
				out[part] = true
			}
		}
		return out
	}
	for _, part := range strings.Split(pyrepr.Str(raw), ",") {
		if part = pystr.Strip(part); part != "" {
			out[part] = true
		}
	}
	return out
}

// Whether the allowlist is a wildcard or holds the candidate
func allows(allowed map[string]bool, candidate string) bool {
	return allowed["*"] || allowed[candidate]
}

// Config extra of the adapter, empty without one
func adapterConfigExtra(adapter gateway.PlatformAdapter) map[string]any {
	if adapter == nil || adapter.Base() == nil || adapter.Base().Config == nil || adapter.Base().Config.Extra == nil {
		return map[string]any{}
	}
	return adapter.Base().Config.Extra
}

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var bech32Generator = [5]uint32{0x3B6A57B2, 0x26508E6D, 0x1EA119FA, 0x3D4233DD, 0x2A1462B3}

// Bech32 checksum polymod of the values
func bech32Polymod(values []int) uint32 {
	chk := uint32(1)
	for _, value := range values {
		top := chk >> 25
		chk = (chk&0x1FFFFFF)<<5 ^ uint32(value)
		for i, gen := range bech32Generator {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen
			}
		}
	}
	return chk
}

// Human readable part expanded for the bech32 checksum
func bech32HRPExpand(hrp string) []int {
	out := make([]int, 0, 2*len(hrp)+1)
	for _, c := range hrp {
		out = append(out, int(c)>>5)
	}
	out = append(out, 0)
	for _, c := range hrp {
		out = append(out, int(c)&31)
	}
	return out
}

// Regroups bit groups between widths, nil on an out of range value or bad padding
func convertBits(data []int, fromBits, toBits uint, pad bool) []int {
	acc, bits := 0, uint(0)
	var out []int
	maxv := (1 << toBits) - 1
	for _, value := range data {
		if value < 0 || value>>fromBits != 0 {
			return nil
		}
		acc = acc<<fromBits | value
		bits += fromBits
		for bits >= toBits {
			bits -= toBits
			out = append(out, (acc>>bits)&maxv)
		}
	}
	if pad && bits > 0 {
		out = append(out, (acc<<(toBits-bits))&maxv)
	} else if !pad && (bits >= fromBits || (acc<<(toBits-bits))&maxv != 0) {
		return nil
	}
	if out == nil {
		out = []int{}
	}
	return out
}

// Hex public key of an npub1 bech32 string, empty when it is not a valid npub
func npubToHex(npub string) string {
	npub = pystr.Lower(pystr.Strip(npub))
	if !strings.HasPrefix(npub, "npub1") {
		return ""
	}
	var data []int
	for _, c := range npub[len("npub1"):] {
		idx := strings.IndexRune(bech32Charset, c)
		if idx < 0 {
			return ""
		}
		data = append(data, idx)
	}
	if bech32Polymod(append(bech32HRPExpand("npub"), data...)) != 1 || len(data) < 6 {
		return ""
	}
	decoded := convertBits(data[:len(data)-6], 5, 8, false)
	if decoded == nil || len(decoded) != 32 {
		return ""
	}
	raw := make([]byte, len(decoded))
	for i, v := range decoded {
		raw[i] = byte(v)
	}
	return hex.EncodeToString(raw)
}

// Allowlist with the hex form of every valid npub1 entry added
func normalizeNostrAllowEntries(entries map[string]bool) map[string]bool {
	out := map[string]bool{}
	for entry := range entries {
		out[entry] = true
		if strings.HasPrefix(pystr.Lower(entry), "npub1") {
			if h := npubToHex(entry); h != "" {
				out[h] = true
			}
		}
	}
	return out
}

// Whether the user id under any platform alias is in the allowlist
func principalMatchesAllowlist(ctx context.Context, source *gateway.SessionSource, userID string, allowed map[string]bool) (bool, error) {
	check := map[string]bool{userID: true}
	if source.Platform == gateway.PlatformWhatsApp || source.Platform == gateway.PlatformWhatsAppCloud {
		expanded := map[string]bool{}
		for entry := range allowed {
			aliases, err := gateway.ExpandWhatsAppAliases(ctx, entry)
			if err != nil {
				return false, err
			}
			for alias := range aliases {
				expanded[alias] = true
			}
		}
		if len(expanded) > 0 {
			allowed = expanded
		}
		aliases, err := gateway.ExpandWhatsAppAliases(ctx, userID)
		if err != nil {
			return false, err
		}
		for alias := range aliases {
			check[alias] = true
		}
		if normalized := gateway.NormalizeWhatsAppIdentifier(userID); normalized != "" {
			check[normalized] = true
		}
	}
	if source.Platform == "buzz" {
		allowed = normalizeNostrAllowEntries(allowed)
		if strings.HasPrefix(userID, "npub") {
			if h := npubToHex(userID); h != "" {
				check[h] = true
			}
		}
	}
	for id := range check {
		if allowed[id] {
			return true, nil
		}
	}
	return false, nil
}

// Adapter declared boolean of the authorization adapter, false when unknown
func (r *GatewayRunner) adapterFlag(ctx context.Context, platform gateway.Platform, name string, profile *string) bool {
	if platform == "" {
		return false
	}
	adapter := r.authorizationAdapter(ctx, platform, profile)
	if adapter == nil {
		return false
	}
	switch name {
	case "authorization_is_upstream":
		return adapter.AuthorizationIsUpstream()
	case "enforces_own_access_policy":
		return adapter.EnforcesOwnAccessPolicy()
	}
	return pyrepr.Truthy(adapterAttr(adapter, name))
}

// Platform config extra, empty when the platform has no config
func (r *GatewayRunner) configExtra(platform gateway.Platform) map[string]any {
	if r.config == nil {
		return map[string]any{}
	}
	cfg := r.config.Platforms[platform]
	if cfg == nil || cfg.Extra == nil {
		return map[string]any{}
	}
	return cfg.Extra
}

// Live adapter's resolved attribute, else the config extra key for a runner with no adapter
func (r *GatewayRunner) adapterSetting(ctx context.Context, platform gateway.Platform, attr, extraKey string, profile *string) any {
	var value any
	if adapter := r.authorizationAdapter(ctx, platform, profile); adapter != nil {
		value = adapterAttr(adapter, attr)
	}
	if value == nil {
		value = r.configExtra(platform)[extraKey]
	}
	return value
}

// Lowercased effective dm or group policy, empty when unknown
func (r *GatewayRunner) adapterPolicy(ctx context.Context, platform gateway.Platform, kind string, profile *string) string {
	if platform == "" {
		return ""
	}
	value := r.adapterSetting(ctx, platform, "_"+kind+"_policy", kind+"_policy", profile)
	if !pyrepr.Truthy(value) {
		return ""
	}
	return pystr.Lower(pystr.Strip(pyrepr.Str(value)))
}

// Whether a per group sender allowlist already gated the message at the adapter
func (r *GatewayRunner) adapterGroupHasSenderAllowlist(ctx context.Context, platform gateway.Platform, chatID string, profile *string) bool {
	if platform == "" || chatID == "" {
		return false
	}
	groups, ok := r.adapterSetting(ctx, platform, "_groups", "groups", profile).(map[string]any)
	if !ok {
		return false
	}
	groupCfg, ok := groups[chatID].(map[string]any)
	if !ok {
		lowered := pystr.Lower(chatID)
		groupCfg, ok = nil, false
		for key, value := range groups {
			if cfg, isCfg := value.(map[string]any); isCfg && pystr.Lower(key) == lowered {
				groupCfg, ok = cfg, true
				break
			}
		}
		if !ok {
			groupCfg, ok = groups["*"].(map[string]any)
		}
	}
	if !ok {
		return false
	}
	senderAllow := groupCfg["allow_from"]
	if !pyrepr.Truthy(senderAllow) {
		senderAllow = groupCfg["allowFrom"]
	}
	switch v := senderAllow.(type) {
	case string:
		return pystr.Strip(v) != ""
	case []any:
		for _, item := range v {
			if pystr.Strip(pyrepr.Str(item)) != "" {
				return true
			}
		}
	}
	return false
}

// Config extra of the adapter that answers the source
func (r *GatewayRunner) adapterExtraForSource(ctx context.Context, source *gateway.SessionSource) (map[string]any, error) {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return nil, err
	}
	return adapterConfigExtra(adapter), nil
}

// Adapter rechecking a DM sender against its live allowlist
type dmAllowedAdapter interface {
	IsDMAllowed(userID string) bool
}

// Own policy adapter verdict when no environment allowlist exists, nil for no verdict
func (r *GatewayRunner) ownPolicyAuthorizes(ctx context.Context, source *gateway.SessionSource, userID string, isGroup bool, adapterProfile *string) *bool {
	yes := true
	if isGroup && r.adapterGroupHasSenderAllowlist(ctx, source.Platform, source.ChatID, adapterProfile) {
		return &yes
	}
	kind := "dm"
	if isGroup {
		kind = "group"
	}
	if r.adapterPolicy(ctx, source.Platform, kind, adapterProfile) != "allowlist" {
		return nil
	}
	if !isGroup {
		if checker, ok := r.authorizationAdapter(ctx, source.Platform, adapterProfile).(dmAllowedAdapter); ok {
			verdict := checker.IsDMAllowed(userID)
			return &verdict
		}
	}
	return &yes
}

// Adapter normalising allowlist entries and ids to one spelling
type userIDNormalizer interface {
	NormalizeUserID(entry string) string
}

// Whether the answering adapter's config extra allowlist admits the user
func (r *GatewayRunner) adapterExtraAllowlistAuthorizes(ctx context.Context, source *gateway.SessionSource, userID string, isGroup bool) (bool, error) {
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil || adapter == nil {
		return false, err
	}
	extra := adapterConfigExtra(adapter)
	groupKey := "group_allow_from"
	if covers, ok := adapterAttr(adapter, "allow_from_covers_groups").(bool); ok && covers {
		groupKey = "allow_from"
	}
	key := "allow_from"
	if isGroup {
		key = groupKey
	}
	adapterAllow := extra[key]
	if !pyrepr.Truthy(adapterAllow) {
		if authzFieldText(source.Platform, "allowed_users_env", registryEntry(ctx, source.Platform)) != "" {
			adapterAllow = extra["allowed_users"]
		}
	}
	if !pyrepr.Truthy(adapterAllow) {
		return false, nil
	}
	allowed := coerceAllowSet(adapterAllow)
	if normalizer, ok := adapter.(userIDNormalizer); ok {
		normalized := map[string]bool{}
		for entry := range allowed {
			if n := normalizer.NormalizeUserID(entry); n != "" {
				normalized[n] = true
			} else {
				normalized[entry] = true
			}
		}
		allowed = normalized
	}
	return allows(allowed, userID), nil
}

// Adapter holding ids it resolved from username shaped allowlist entries at connect time
type resolvedAllowlistAdapter interface {
	ResolvedAllowlistUserIDs() (any, error)
}

// Ids the answering adapter resolved from username shaped allowlist entries
func (r *GatewayRunner) adapterResolvedAllowlistIDs(ctx context.Context, source *gateway.SessionSource) map[string]bool {
	out := map[string]bool{}
	adapter, err := r.deliveryAdapterFor(ctx, source)
	if err != nil {
		return out
	}
	resolver, ok := adapter.(resolvedAllowlistAdapter)
	if !ok {
		return out
	}
	resolved, err := resolver.ResolvedAllowlistUserIDs()
	if err != nil {
		return out
	}
	var items []any
	switch v := resolved.(type) {
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	case map[string]bool:
		for s := range v {
			items = append(items, s)
		}
	default:
		return out
	}
	for _, item := range items {
		switch item.(type) {
		case string, int, int64:
			if s := pystr.Strip(pyrepr.Str(item)); s != "" {
				out[s] = true
			}
		}
	}
	return out
}

// Grants that need no user id, trusted upstream delegation, chat scoped group allowlists and admitted bots
func (r *GatewayRunner) chatScopedGrant(ctx context.Context, source *gateway.SessionSource, adapterProfile *string, isGroup, allowDelegation bool) bool {
	if allowDelegation && (source.DeliveredViaUpstreamRelay || r.adapterFlag(ctx, source.Platform, "authorization_is_upstream", adapterProfile)) {
		return true
	}
	if isGroup && source.ChatID != "" {
		if env := groupChatEnv(ctx, source.Platform, nil); env != "" && allows(coerceAllowSet(authEnv(ctx, env)), source.ChatID) {
			return true
		}
		if extra, err := r.adapterExtraForSource(ctx, source); err == nil {
			if groupAllowed := extra["group_allowed_chats"]; pyrepr.Truthy(groupAllowed) && allows(coerceAllowSet(groupAllowed), source.ChatID) {
				return true
			}
		}
	}
	if source.IsBot {
		func() {
			defer func() { _ = recover() }()
			plugins.DiscoverPlugins(ctx, false)
		}()
		allowBotsVar := authzFieldText(source.Platform, "allow_bots_env", registryEntry(ctx, source.Platform))
		if allowBotsVar != "" {
			extra, err := r.adapterExtraForSource(ctx, source)
			if err != nil {
				extra = map[string]any{}
			}
			mode, err := gateway.ExtraOrSecret(ctx, extra, "allow_bots", allowBotsVar, "none", false)
			if err == nil {
				switch pystr.Strip(pystr.Lower(pyrepr.Str(mode))) {
				case "mentions", "all":
					return true
				}
			}
		}
	}
	return false
}

// Chat ids misfiled in TELEGRAM_GROUP_ALLOWED_USERS honored as chat ids with a one time warning
func (r *GatewayRunner) legacyTelegramChatGrant(source *gateway.SessionSource, groupUserAllowlist string) bool {
	legacy := map[string]bool{}
	for _, value := range strings.Split(groupUserAllowlist, ",") {
		if strings.HasPrefix(pystr.Strip(value), "-") {
			legacy[pystr.Strip(value)] = true
		}
	}
	if len(legacy) == 0 {
		return false
	}
	if !r.warnedTelegramGroupUsersLegacy {
		ids := make([]string, 0, len(legacy))
		for id := range legacy {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		runLog().Warn(fmt.Sprintf("TELEGRAM_GROUP_ALLOWED_USERS contains chat-ID-shaped values "+
			"(%s). Treating them as chat IDs for backward compatibility. "+
			"Move chat IDs to TELEGRAM_GROUP_ALLOWED_CHATS — the _USERS var "+
			"is now for sender user IDs.", strings.Join(ids, ",")))
		r.warnedTelegramGroupUsersLegacy = true
	}
	return legacy[source.ChatID]
}

// Bot loop guard of the runner, built once on first use
func (r *GatewayRunner) botLoopGuardInstance() *gateway.BotLoopGuard {
	botGuardInitMu.Lock()
	defer botGuardInitMu.Unlock()
	if r.botLoopGuard == nil {
		r.botLoopGuard = gateway.NewBotLoopGuard(nil, nil)
	}
	return r.botLoopGuard
}

// One loop budget per conversation of the transport profile, platform and chat
func (r *GatewayRunner) botLoopGuardConversation(source *gateway.SessionSource) [3]string {
	profile := ""
	if p := r.adapterProfileForSource(source); p != nil {
		profile = *p
	}
	return [3]string{profile, string(source.Platform), source.ChatID}
}

// Counts one authorized bot authored inbound message, false when it trips the budget or the chat cools down
func (r *GatewayRunner) admitBotMessage(ctx context.Context, source *gateway.SessionSource) bool {
	if !source.IsBot {
		return true
	}
	allowed, state := r.botLoopGuardInstance().Admit(ctx, r.botLoopGuardConversation(source))
	if state == "tripped" {
		userID := "None"
		if source.UserID != nil {
			userID = *source.UserID
		}
		slog.Default().With("logger", "gateway.authz_mixin").Warn(fmt.Sprintf("Bot loop guard is dropping bot messages in %s chat %s: bot %s sent one message too many "+
			"for the window, cooling down (gateway.bot_loop_guard in config.yaml).", source.Platform, source.ChatID, userID))
	}
	return allowed
}

// Whether a user may use the bot, a bot admitted by the allowlists still refused while its chat cools down
func (r *GatewayRunner) isUserAuthorized(ctx context.Context, source *gateway.SessionSource, allowDelegation bool) (bool, error) {
	authorized, err := r.principalAuthorized(ctx, source, allowDelegation)
	if err != nil || !authorized {
		return false, err
	}
	if !source.IsBot {
		return true, nil
	}
	return !r.botLoopGuardInstance().Blocked(ctx, r.botLoopGuardConversation(source)), nil
}

// Allowlist verdict alone before the bot loop guard
func (r *GatewayRunner) principalAuthorized(ctx context.Context, source *gateway.SessionSource, allowDelegation bool) (bool, error) {
	local, err := gateway.AuthorizeLocalSource(r, source)
	if err != nil {
		return false, err
	}
	if local != nil {
		return *local, nil
	}
	if source.Platform == gateway.PlatformWebhook {
		return true, nil
	}
	if entry := registryEntry(ctx, source.Platform); entry != nil && entry.TrustedInbound {
		return true, nil
	}
	adapterProfile := r.adapterProfileForSource(source)
	isGroup := groupChatTypes[source.ChatType]
	isGroupOrForum := groupForumTypes[source.ChatType]
	if r.chatScopedGrant(ctx, source, adapterProfile, isGroup, allowDelegation) {
		return true, nil
	}
	if source.UserID == nil || *source.UserID == "" {
		return false, nil
	}
	userID := *source.UserID
	entry := registryEntry(ctx, source.Platform)
	platformAllowEnv := authzFieldText(source.Platform, "allowed_users_env", entry)
	platformAllowAllVar := authzFieldText(source.Platform, "allow_all_env", entry)
	if platformAllowAllVar != "" && envTruthy(ctx, platformAllowAllVar) {
		return true, nil
	}
	if allowDelegation && source.RoleAuthorized {
		return true, nil
	}
	if store := r.pairingStoreFor(source); store != nil {
		approved, err := store.IsApproved(ctx, string(source.Platform), userID)
		if err != nil {
			return false, err
		}
		if approved {
			return true, nil
		}
	}
	platformAllowlist := authEnv(ctx, platformAllowEnv)
	groupUserAllowlist, groupChatAllowlist := "", ""
	if isGroupOrForum {
		groupUserAllowlist = authEnv(ctx, groupUserEnv[source.Platform])
		groupChatAllowlist = authEnv(ctx, groupChatEnv(ctx, source.Platform, entry))
	}
	globalAllowlist := authEnv(ctx, "GATEWAY_ALLOWED_USERS")
	if platformAllowlist == "" && groupUserAllowlist == "" && groupChatAllowlist == "" && globalAllowlist == "" {
		if allowDelegation && r.adapterFlag(ctx, source.Platform, "enforces_own_access_policy", adapterProfile) {
			if verdict := r.ownPolicyAuthorizes(ctx, source, userID, isGroup, adapterProfile); verdict != nil {
				return *verdict, nil
			}
		}
		granted, err := r.adapterExtraAllowlistAuthorizes(ctx, source, userID, isGroup)
		if err != nil {
			return false, err
		}
		if granted {
			return true, nil
		}
		return envTruthy(ctx, "GATEWAY_ALLOW_ALL_USERS"), nil
	}
	if isGroupOrForum && source.ChatID != "" {
		if groupChatAllowlist != "" && allows(coerceAllowSet(groupChatAllowlist), source.ChatID) {
			return true, nil
		}
		if source.Platform == gateway.PlatformTelegram && groupUserAllowlist != "" && r.legacyTelegramChatGrant(source, groupUserAllowlist) {
			return true, nil
		}
	}
	allowedIDs := coerceAllowSet(platformAllowlist)
	for id := range coerceAllowSet(groupUserAllowlist) {
		allowedIDs[id] = true
	}
	for id := range coerceAllowSet(globalAllowlist) {
		allowedIDs[id] = true
	}
	if platformAllowlist != "" {
		for id := range r.adapterResolvedAllowlistIDs(ctx, source) {
			allowedIDs[id] = true
		}
	}
	if allowedIDs["*"] {
		return true, nil
	}
	return principalMatchesAllowlist(ctx, source, userID, allowedIDs)
}

// How unauthorized DMs are handled for a platform, pair, ignore or decline
func (r *GatewayRunner) getUnauthorizedDMBehavior(ctx context.Context, platform gateway.Platform, profile *string) string {
	if r.config != nil && platform != "" {
		if _, ok := r.configExtra(platform)["unauthorized_dm_behavior"]; ok {
			return r.config.GetUnauthorizedDMBehavior(platform)
		}
	}
	if platform == gateway.PlatformEmail {
		return "ignore"
	}
	if r.config != nil && r.config.UnauthorizedDMBehavior != "pair" {
		return r.config.UnauthorizedDMBehavior
	}
	allowlistKeys := []string{"GATEWAY_ALLOWED_USERS"}
	if platform != "" {
		dmPolicy := r.adapterPolicy(ctx, platform, "dm", profile)
		if dmPolicy == "" {
			if raw := r.configExtra(platform)["dm_policy"]; pyrepr.Truthy(raw) {
				dmPolicy = pystr.Lower(pystr.Strip(pyrepr.Str(raw)))
			}
		}
		if dmPolicy == "pairing" {
			return "pair"
		}
		if dmPolicy == "allowlist" || dmPolicy == "disabled" {
			return "ignore"
		}
		entry := registryEntry(ctx, platform)
		if pyrepr.Truthy(gateway.AuthzField(string(platform), "allowlist_suppresses_pairing", entry)) {
			allowlistKeys = append([]string{authzFieldText(platform, "allowed_users_env", entry), groupUserEnv[platform],
				groupChatEnv(ctx, platform, entry)}, allowlistKeys...)
		}
	}
	for _, key := range allowlistKeys {
		if key != "" && pystr.Strip(authEnv(ctx, key)) != "" {
			return "ignore"
		}
	}
	return "pair"
}
