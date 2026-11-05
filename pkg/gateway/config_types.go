package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Python exception class and message a gateway config read raises
type PyError struct {
	Kind string
	Msg  string
}

func (e *PyError) Error() string { return e.Msg }

// Python exception class of an error, empty for errors that carry none
func pyErrorKind(err error) string {
	switch e := err.(type) {
	case *PyError:
		return e.Kind
	case *pyint.Error:
		return e.Kind
	}
	return ""
}

// Python float of the value, the default for None or any value float rejects
func coerceFloat(value any, def float64) float64 {
	if value == nil {
		return def
	}
	f, err := pyint.Float(value)
	if err != nil {
		return def
	}
	return f
}

// Python int of the value, the default for None or any value int rejects
func coerceInt(value any, def int) *big.Int {
	if value == nil {
		return big.NewInt(int64(def))
	}
	n, err := pyint.Of(value)
	if err != nil {
		return big.NewInt(int64(def))
	}
	return n
}

// Positive int or nil, nil zero and negatives disable, malformed values warn and disable
func coerceOptionalPositiveInt(value any, key string) *big.Int {
	if value == nil {
		return nil
	}
	var parsed *big.Int
	var err error
	_, isBool := value.(bool)
	f, isFloat := value.(float64)
	switch {
	case isBool || (isFloat && (math.IsInf(f, 0) || math.IsNaN(f) || f != math.Trunc(f))):
		err = &PyError{"ValueError", pyrepr.Repr(value)}
	default:
		if s, ok := value.(string); ok {
			var good bool
			if parsed, good = pyint.Parse(pystr.Strip(s)); !good {
				err = &PyError{"ValueError", s}
			}
		} else {
			parsed, err = pyint.Of(value)
		}
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Ignoring invalid %s=%s (expected a positive integer; 0/null disables)", key, pyrepr.Repr(value)))
		return nil
	}
	if parsed.Sign() <= 0 {
		return nil
	}
	return parsed
}

// Largest watchdog interval systemd accepts
const systemdWatchdogMaxSeconds = 2_147_483_647

// Bounded positive watchdog interval in seconds, zero when unset or invalid
func CoerceSystemdWatchdogSeconds(value any, key string) int {
	if key == "" {
		key = "gateway.systemd_watchdog_seconds"
	}
	if value == nil {
		return 0
	}
	var parsed *big.Int
	if n, ok := pyrepr.Int(value); ok {
		parsed = n
	} else if s, ok := value.(string); ok {
		t := pystr.Strip(s)
		if t != "" && pystr.All(t, func(r rune) bool { return r < utf8.RuneSelf && pystr.IsDecimal(r) }) {
			parsed, _ = pyint.Parse(t)
		}
	}
	if parsed == nil {
		slog.Warn(fmt.Sprintf("Ignoring invalid %s (expected a positive integer)", key))
		return 0
	}
	if parsed.Sign() != 0 && (parsed.Sign() < 0 || parsed.Cmp(big.NewInt(systemdWatchdogMaxSeconds)) > 0) {
		slog.Warn(fmt.Sprintf("Ignoring invalid %s (expected an integer from 1 to %d)", key, systemdWatchdogMaxSeconds))
		return 0
	}
	return int(parsed.Int64())
}

// Env value through the active profile secret scope when one is installed, else the process env
func getenv(ctx context.Context, name string) (string, bool) {
	if provider.CurrentSecretScope(ctx) != nil {
		return provider.SecretScopeFrom(ctx).GetSecret(name)
	}
	return os.LookupEnv(name)
}

// Env value as a string with a default for an unset name
func getenvStr(ctx context.Context, name, def string) string {
	if v, ok := getenv(ctx, name); ok {
		return v
	}
	return def
}

// Messaging platform value, built in members plus plugin pseudo members created on demand
type Platform string

const (
	PlatformLocal          Platform = "local"
	PlatformTelegram       Platform = "telegram"
	PlatformDiscord        Platform = "discord"
	PlatformWhatsApp       Platform = "whatsapp"
	PlatformWhatsAppCloud  Platform = "whatsapp_cloud"
	PlatformSlack          Platform = "slack"
	PlatformSignal         Platform = "signal"
	PlatformMattermost     Platform = "mattermost"
	PlatformMatrix         Platform = "matrix"
	PlatformEmail          Platform = "email"
	PlatformSMS            Platform = "sms"
	PlatformDingTalk       Platform = "dingtalk"
	PlatformAPIServer      Platform = "api_server"
	PlatformWebhook        Platform = "webhook"
	PlatformMSGraphWebhook Platform = "msgraph_webhook"
	PlatformFeishu         Platform = "feishu"
	PlatformWeCom          Platform = "wecom"
	PlatformWeComCallback  Platform = "wecom_callback"
	PlatformWeixin         Platform = "weixin"
	PlatformBlueBubbles    Platform = "bluebubbles"
	PlatformQQBot          Platform = "qqbot"
	PlatformYuanbao        Platform = "yuanbao"
	PlatformRelay          Platform = "relay"
)

var builtinPlatforms = []Platform{
	PlatformLocal, PlatformTelegram, PlatformDiscord, PlatformWhatsApp, PlatformWhatsAppCloud, PlatformSlack,
	PlatformSignal, PlatformMattermost, PlatformMatrix, PlatformEmail, PlatformSMS, PlatformDingTalk,
	PlatformAPIServer, PlatformWebhook, PlatformMSGraphWebhook, PlatformFeishu, PlatformWeCom,
	PlatformWeComCallback, PlatformWeixin, PlatformBlueBubbles, PlatformQQBot, PlatformYuanbao, PlatformRelay,
}

// Built in platform values, snapshotted before any pseudo member exists
var BuiltinPlatformValues = func() map[string]bool {
	out := make(map[string]bool, len(builtinPlatforms))
	for _, p := range builtinPlatforms {
		out[string(p)] = true
	}
	return out
}()

var platformMembers struct {
	sync.Mutex
	pseudo         map[string]Platform
	memberMap      *pydict.Dict[string, Platform]
	scanned        bool
	bundledNames   map[string]bool
	bundledAliases map[string]string
}

// Built in members in declaration order, pseudo members never iterate
func PlatformMembers() []Platform { return append([]Platform(nil), builtinPlatforms...) }

// Values of the member map by name, pseudo members included in creation order as __members__ lists them
func PlatformMemberMap() []Platform {
	platformMembers.Lock()
	defer platformMembers.Unlock()
	if platformMembers.memberMap == nil {
		return PlatformMembers()
	}
	return platformMembers.memberMap.Values()
}

// Member name, the value upper cased with dashes and spaces as underscores
func (p Platform) Name() string {
	return strings.NewReplacer("-", "_", " ", "_").Replace(pystr.Upper(string(p)))
}

// Member registered under an exact value, built in or pseudo
func platformMember(value string) (Platform, bool) {
	if BuiltinPlatformValues[value] {
		return Platform(value), true
	}
	platformMembers.Lock()
	defer platformMembers.Unlock()
	p, ok := platformMembers.pseudo[value]
	return p, ok
}

// Creates and caches the pseudo member of a plugin platform value
func addPseudoPlatform(value string) Platform {
	platformMembers.Lock()
	defer platformMembers.Unlock()
	if platformMembers.pseudo == nil {
		platformMembers.pseudo = map[string]Platform{}
	}
	platformMembers.pseudo[value] = Platform(value)
	if platformMembers.memberMap == nil {
		platformMembers.memberMap = pydict.New[string, Platform]()
		for _, p := range builtinPlatforms {
			platformMembers.memberMap.Set(p.Name(), p)
		}
	}
	platformMembers.memberMap.Set(Platform(value).Name(), Platform(value))
	return Platform(value)
}

// Bundled platform directory names and manifest aliases, scanned once per process
func bundledPlatformScanCached() (map[string]bool, map[string]string) {
	platformMembers.Lock()
	defer platformMembers.Unlock()
	if !platformMembers.scanned {
		platformMembers.bundledNames, platformMembers.bundledAliases = scanBundledPluginPlatforms()
		platformMembers.scanned = true
	}
	return platformMembers.bundledNames, platformMembers.bundledAliases
}

// The platform for a value, accepting bundled or registered plugin names, ValueError for anything else
func ParsePlatform(ctx context.Context, value any) (Platform, error) {
	raw := value
	if p, ok := value.(Platform); ok {
		value = string(p)
	}
	invalid := &PyError{"ValueError", pyrepr.Repr(raw) + " is not a valid Platform"}
	s, ok := value.(string)
	if !ok {
		return "", invalid
	}
	if p, ok := platformMember(s); ok {
		return p, nil
	}
	if pystr.Strip(s) == "" {
		return "", invalid
	}
	s = pystr.Lower(pystr.Strip(s))
	if p, ok := platformMember(s); ok {
		return p, nil
	}
	names, aliases := bundledPlatformScanCached()
	registered := names[s]
	if !registered {
		if alias, ok := aliases[s]; ok {
			if p, ok := platformMember(alias); ok {
				return p, nil
			}
			return addPseudoPlatform(alias), nil
		}
		registered = Registry.IsRegistered(ctx, s)
	}
	if registered {
		return addPseudoPlatform(s), nil
	}
	return "", invalid
}

// Lower cased manifest name of a bundled platform plugin, empty when absent or unreadable
func bundledPlatformManifestName(pluginDir string) string {
	var manifest string
	for _, m := range []string{"plugin.yaml", "plugin.yml"} {
		if _, err := os.Stat(filepath.Join(pluginDir, m)); err == nil {
			manifest = filepath.Join(pluginDir, m)
			break
		}
	}
	if manifest == "" {
		return ""
	}
	raw, err := os.ReadFile(manifest)
	if err != nil || !utf8.Valid(raw) {
		return ""
	}
	data, err := config.FastSafeLoad(raw)
	if err != nil {
		return ""
	}
	if !pyrepr.Truthy(data) {
		data = map[string]any{}
	}
	m, ok := data.(map[string]any)
	if !ok {
		return pystr.Lower(pystr.Strip(pyrepr.Str(nil)))
	}
	return pystr.Lower(pystr.Strip(pyrepr.Str(m["name"])))
}

// Bundled platform plugin directories holding a manifest, plus manifest names that differ from their directory
func scanBundledPluginPlatforms() (map[string]bool, map[string]string) {
	platformsDir := filepath.Join(plugins.GetBundledPluginsDir(), "platforms")
	entries, err := os.ReadDir(platformsDir)
	if err != nil {
		return map[string]bool{}, map[string]string{}
	}
	var dirs []string
	for _, e := range entries {
		child := filepath.Join(platformsDir, e.Name())
		if fi, err := os.Stat(child); err != nil || !fi.IsDir() {
			continue
		}
		for _, m := range []string{"plugin.yaml", "plugin.yml"} {
			if _, err := os.Stat(filepath.Join(child, m)); err == nil {
				dirs = append(dirs, e.Name())
				break
			}
		}
	}
	names := map[string]bool{}
	for _, d := range dirs {
		names[pystr.Lower(d)] = true
	}
	aliases := map[string]string{}
	for _, d := range dirs {
		name := bundledPlatformManifestName(filepath.Join(platformsDir, d))
		if _, taken := aliases[name]; name != "" && !names[name] && !taken {
			aliases[name] = pystr.Lower(d)
		}
	}
	return names, aliases
}

// Default destination of a platform, thread routing to the topic where it was set
type HomeChannel struct {
	Platform Platform
	ChatID   string
	Name     any
	ThreadID *string
	UserID   *string
	ScopeID  *string
}

// Home channel with a Discord channel link reduced to its channel id
func NewHomeChannel(platform Platform, chatID string, name any) *HomeChannel {
	h := &HomeChannel{Platform: platform, ChatID: chatID, Name: name}
	h.PostInit()
	return h
}

// Normalizes a pasted Discord channel link to the bare channel id
func (h *HomeChannel) PostInit() {
	if h.Platform == PlatformDiscord {
		if id, ok := DiscordChannelIDFromLink(pystr.Strip(h.ChatID)); ok && id != "" {
			h.ChatID = id
		}
	}
}

// Mapping form with the optional ids only when set
func (h *HomeChannel) ToDict() map[string]any {
	out := map[string]any{"platform": string(h.Platform), "chat_id": h.ChatID, "name": h.Name}
	for k, v := range map[string]*string{"thread_id": h.ThreadID, "user_id": h.UserID, "scope_id": h.ScopeID} {
		if v != nil && *v != "" {
			out[k] = *v
		}
	}
	return out
}

// Home channel of a mapping, KeyError for a missing platform or chat id
func HomeChannelFromDict(ctx context.Context, data map[string]any) (*HomeChannel, error) {
	rawPlatform, ok := data["platform"]
	if !ok {
		return nil, &PyError{"KeyError", "'platform'"}
	}
	platform, err := ParsePlatform(ctx, rawPlatform)
	if err != nil {
		return nil, err
	}
	rawChat, ok := data["chat_id"]
	if !ok {
		return nil, &PyError{"KeyError", "'chat_id'"}
	}
	name, ok := data["name"]
	if !ok {
		name = "Home"
	}
	h := &HomeChannel{Platform: platform, ChatID: pyrepr.Str(rawChat), Name: name}
	for k, dst := range map[string]**string{"thread_id": &h.ThreadID, "user_id": &h.UserID, "scope_id": &h.ScopeID} {
		if v := data[k]; pyrepr.Truthy(v) {
			s := pyrepr.Str(v)
			*dst = &s
		}
	}
	h.PostInit()
	return h, nil
}

// Writes the home channel into config.yaml, enabling the platform only when asked and not yet set
func PersistHomeChannel(ctx context.Context, h *HomeChannel, enabledIfNew bool) error {
	dir := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, dir, config.EnvRefLookup)
	if err != nil {
		return err
	}
	doc := map[string]any(cfg)
	platformConfig := dictSlot(dictSlot(doc, "platforms"), string(h.Platform))
	if enabledIfNew {
		if _, ok := platformConfig["enabled"]; !ok {
			platformConfig["enabled"] = true
		}
	}
	platformConfig["home_channel"] = h.ToDict()
	return config.SaveConfig(ctx, dir, doc, config.SaveOptions{})
}

// Per channel reply policy, model, provider or system prompt override
type ChannelOverride struct {
	Model        any
	Provider     any
	SystemPrompt any
	AllowSilence any
}

// Mapping of the fields that are set
func (o *ChannelOverride) ToDict() map[string]any {
	out := map[string]any{}
	for k, v := range map[string]any{"model": o.Model, "provider": o.Provider, "system_prompt": o.SystemPrompt, "allow_silence": o.AllowSilence} {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

// Override of a mapping, every field nil for an empty one
func ChannelOverrideFromDict(data map[string]any) *ChannelOverride {
	if len(data) == 0 {
		return &ChannelOverride{}
	}
	return &ChannelOverride{Model: data["model"], Provider: data["provider"], SystemPrompt: data["system_prompt"], AllowSilence: data["allow_silence"]}
}

// Env var of each platform whose primary credential is the config token
var PlatformTokenEnvNames = map[Platform]string{
	PlatformTelegram:   "TELEGRAM_BOT_TOKEN",
	PlatformDiscord:    "DISCORD_BOT_TOKEN",
	PlatformSlack:      "SLACK_BOT_TOKEN",
	PlatformMattermost: "MATTERMOST_TOKEN",
	PlatformMatrix:     "MATRIX_ACCESS_TOKEN",
	PlatformWeixin:     "WEIXIN_TOKEN",
}

// Configuration of one messaging platform
type PlatformConfig struct {
	Enabled                    bool
	Token                      any
	APIKey                     any
	HomeChannel                *HomeChannel
	ReplyToMode                any
	GatewayRestartNotification bool
	TypingIndicator            bool
	TypingStatusText           any
	ChannelOverrides           map[string]*ChannelOverride
	Extra                      map[string]any
}

// Platform config with the dataclass defaults
func NewPlatformConfig() *PlatformConfig {
	return &PlatformConfig{ReplyToMode: "first", GatewayRestartNotification: true, TypingIndicator: true,
		ChannelOverrides: map[string]*ChannelOverride{}, Extra: map[string]any{}}
}

// Keys consumed by typed fields, the rest of a platform block is adapter config
var PlatformTypedKeys = map[string]bool{
	"enabled": true, "token": true, "api_key": true, "home_channel": true, "reply_to_mode": true,
	"channel_overrides": true, "extra": true, "gateway_restart_notification": true, "typing_indicator": true,
	"typing_status_text": true,
}

// Mapping form, token and api key only when truthy
func (c *PlatformConfig) ToDict() map[string]any {
	out := map[string]any{"enabled": c.Enabled, "extra": c.Extra, "reply_to_mode": c.ReplyToMode,
		"gateway_restart_notification": c.GatewayRestartNotification, "typing_indicator": c.TypingIndicator}
	if c.TypingStatusText != nil {
		out["typing_status_text"] = c.TypingStatusText
	}
	if pyrepr.Truthy(c.Token) {
		out["token"] = c.Token
	}
	if pyrepr.Truthy(c.APIKey) {
		out["api_key"] = c.APIKey
	}
	if c.HomeChannel != nil {
		out["home_channel"] = c.HomeChannel.ToDict()
	}
	if len(c.ChannelOverrides) > 0 {
		ov := make(map[string]any, len(c.ChannelOverrides))
		for cid, o := range c.ChannelOverrides {
			ov[cid] = o.ToDict()
		}
		out["channel_overrides"] = ov
	}
	return out
}

// Platform config of a block, non typed top level keys promoted into extra with explicit extra winning
func PlatformConfigFromDict(ctx context.Context, raw any) (*PlatformConfig, error) {
	data := coerceDict(raw)
	extra := map[string]any{}
	for k, v := range data {
		if !PlatformTypedKeys[k] {
			extra[k] = v
		}
	}
	explicitExtra, present := data["extra"]
	if !present {
		explicitExtra = map[string]any{}
	}
	for k, v := range coerceDict(explicitExtra) {
		extra[k] = v
	}
	toplevelOrExtra := func(key string) any {
		if v := data[key]; v != nil {
			return v
		}
		return extra[key]
	}
	overrides := map[string]*ChannelOverride{}
	if m, ok := data["channel_overrides"].(map[string]any); ok {
		for cid, ov := range m {
			if block, ok := ov.(map[string]any); ok {
				overrides[cid] = ChannelOverrideFromDict(block)
			}
		}
	}
	var homeCh *HomeChannel
	if block, ok := data["home_channel"].(map[string]any); ok {
		h, err := HomeChannelFromDict(ctx, block)
		if err != nil {
			return nil, err
		}
		homeCh = h
	}
	replyToMode, ok := data["reply_to_mode"]
	if !ok {
		replyToMode = "first"
	}
	return &PlatformConfig{
		Enabled:                    coerceBool(data["enabled"], false),
		Token:                      data["token"],
		APIKey:                     data["api_key"],
		HomeChannel:                homeCh,
		ReplyToMode:                replyToMode,
		GatewayRestartNotification: coerceBool(toplevelOrExtra("gateway_restart_notification"), true),
		TypingIndicator:            coerceBool(toplevelOrExtra("typing_indicator"), true),
		TypingStatusText:           toplevelOrExtra("typing_status_text"),
		ChannelOverrides:           overrides,
		Extra:                      extra,
	}, nil
}

// Streaming defaults shared with the stream consumer config
const (
	DefaultStreamingEditInterval    = 0.8
	DefaultStreamingBufferThreshold = 24
	DefaultStreamingCursor          = " ▉"
)

// Real time token streaming to messaging platforms
type StreamingConfig struct {
	Enabled                bool
	Transport              string
	EditInterval           float64
	BufferThreshold        *big.Int
	Cursor                 any
	FreshFinalAfterSeconds float64
}

// Streaming config with the dataclass defaults
func DefaultStreamingConfig() StreamingConfig {
	return StreamingConfig{Transport: "auto", EditInterval: DefaultStreamingEditInterval,
		BufferThreshold: big.NewInt(DefaultStreamingBufferThreshold), Cursor: DefaultStreamingCursor}
}

// The master switch, a transport of off also disabling
func (s StreamingConfig) GloballyEnabled() bool { return s.Enabled && s.Transport != "off" }

// Effective streaming for one platform, a per platform value only narrowing the global switch
func (s StreamingConfig) EnabledFor(platformOverride any) bool {
	return s.GloballyEnabled() && (platformOverride == nil || pyrepr.Truthy(platformOverride))
}

// Mapping of every field
func (s StreamingConfig) ToDict() map[string]any {
	return map[string]any{"enabled": s.Enabled, "transport": s.Transport, "edit_interval": s.EditInterval,
		"buffer_threshold": pyint.Native(s.BufferThreshold), "cursor": s.Cursor, "fresh_final_after_seconds": s.FreshFinalAfterSeconds}
}

// Streaming config of a mapping, mode as a transport alias that also implies enabled
func StreamingConfigFromDict(raw any) StreamingConfig {
	data, ok := raw.(map[string]any)
	if !ok || len(data) == 0 {
		return DefaultStreamingConfig()
	}
	rawTransport, rawMode := data["transport"], data["mode"]
	var enabled bool
	if _, ok := data["enabled"]; ok {
		enabled = coerceBool(data["enabled"], false)
	} else {
		enabled = rawMode != nil && normalizeTransportToken(rawMode) != "off"
	}
	transport := rawTransport
	if transport == nil {
		transport = rawMode
	}
	cursor, ok := data["cursor"]
	if !ok {
		cursor = DefaultStreamingCursor
	}
	return StreamingConfig{
		Enabled:                enabled,
		Transport:              normalizeTransportToken(transport),
		EditInterval:           coerceFloat(data["edit_interval"], DefaultStreamingEditInterval),
		BufferThreshold:        coerceInt(data["buffer_threshold"], DefaultStreamingBufferThreshold),
		Cursor:                 cursor,
		FreshFinalAfterSeconds: coerceFloat(data["fresh_final_after_seconds"], 0),
	}
}

// True when the API server key is strong enough for the adapter to start
func hasUsableAPIServerKey(key any) bool {
	if !pyrepr.Truthy(key) {
		return false
	}
	return provider.HasUsableSecret(key, 16)
}

// Core only connected checks for platforms without a plugin entry
var platformConnectedCheckers = map[Platform]func(cfg *PlatformConfig) bool{
	PlatformAPIServer: func(cfg *PlatformConfig) bool { return hasUsableAPIServerKey(cfg.Extra["key"]) },
	PlatformWebhook:   func(cfg *PlatformConfig) bool { return true },
	PlatformMSGraphWebhook: func(cfg *PlatformConfig) bool {
		v := cfg.Extra["client_state"]
		if !pyrepr.Truthy(v) {
			v = ""
		}
		return pystr.Strip(pyrepr.Str(v)) != ""
	},
	PlatformRelay: func(cfg *PlatformConfig) bool {
		return pyrepr.Truthy(cfg.Extra["relay_url"]) || pyrepr.Truthy(cfg.Extra["url"])
	},
}

// Registry entry of a platform after idempotent plugin discovery, nil when absent
func discoveredRegistryEntry(ctx context.Context, platform Platform) *PlatformEntry {
	plugins.DiscoverPlugins(ctx, false)
	return Registry.Get(ctx, string(platform))
}

// Top level boolean keys read verbatim with their defaults
var toplevelBoolDefaults = []struct {
	key string
	def bool
}{
	{"write_sessions_json", true}, {"always_log_local", true}, {"filter_silence_narration", true},
	{"group_sessions_per_user", true}, {"thread_sessions_per_user", false},
}

// Main gateway configuration, platform connections, session policies and delivery settings
type GatewayConfig struct {
	Platforms                    map[Platform]*PlatformConfig
	ResetTriggers                any
	QuickCommands                map[string]any
	SessionsDir                  string
	WriteSessionsJSON            bool
	AlwaysLogLocal               bool
	FilterSilenceNarration       bool
	STTEnabled                   bool
	STTEchoTranscripts           bool
	GroupSessionsPerUser         bool
	ThreadSessionsPerUser        bool
	MaxConcurrentSessions        *big.Int
	MultiplexProfiles            *bool
	RoomLinkURL                  *string
	SystemdWatchdogSeconds       int
	LoopWatchdog                 bool
	LoopWatchdogProbeIntervalS   float64
	LoopWatchdogProbeTimeoutS    float64
	LoopWatchdogMaxStrikes       int
	OnAllAdaptersDown            string
	UnauthorizedDMBehavior       string
	UnauthorizedDMDeclineMessage string
	Streaming                    StreamingConfig
	SessionStoreMaxAgeDays       *big.Int
	ProfileRoutes                []*ProfileRoute
}

// Gateway config with the dataclass defaults for the home in ctx
func DefaultGatewayConfig(ctx context.Context) *GatewayConfig {
	return &GatewayConfig{
		Platforms: map[Platform]*PlatformConfig{}, ResetTriggers: []any{"/new", "/reset"}, QuickCommands: map[string]any{},
		SessionsDir: filepath.Join(home.Dir(ctx), "sessions"), WriteSessionsJSON: true, AlwaysLogLocal: true,
		FilterSilenceNarration: true, STTEnabled: true, STTEchoTranscripts: true, GroupSessionsPerUser: true,
		LoopWatchdog: true, LoopWatchdogProbeIntervalS: DefaultLoopWatchdogIntervalS,
		LoopWatchdogProbeTimeoutS: DefaultLoopWatchdogTimeoutS, LoopWatchdogMaxStrikes: DefaultLoopWatchdogMaxStrikes,
		OnAllAdaptersDown: "exit", UnauthorizedDMBehavior: "pair", Streaming: DefaultStreamingConfig(),
		SessionStoreMaxAgeDays: big.NewInt(90), ProfileRoutes: []*ProfileRoute{},
	}
}

// Bounds the systemd watchdog interval as the dataclass post init does
func (c *GatewayConfig) PostInit() {
	c.SystemdWatchdogSeconds = CoerceSystemdWatchdogSeconds(c.SystemdWatchdogSeconds, "")
}

// Enabled and configured platforms sorted by value so the prompt block stays byte stable
func (c *GatewayConfig) GetConnectedPlatforms(ctx context.Context) []Platform {
	var connected []Platform
	for p, pc := range c.Platforms {
		if pc.Enabled && c.isPlatformConnected(ctx, p, pc) {
			connected = append(connected, p)
		}
	}
	sort.Slice(connected, func(i, j int) bool { return connected[i] < connected[j] })
	return connected
}

// Whether a platform has the credentials its core checker or plugin entry requires
func (c *GatewayConfig) isPlatformConnected(ctx context.Context, platform Platform, cfg *PlatformConfig) bool {
	if checker, ok := platformConnectedCheckers[platform]; ok {
		return pyrepr.Truthy(cfg.Token) || pyrepr.Truthy(cfg.APIKey) || checker(cfg)
	}
	entry := discoveredRegistryEntry(ctx, platform)
	if entry != nil && entry.ConfigConnectedFn != nil {
		return entry.ConfigConnectedFn(cfg)
	}
	if pyrepr.Truthy(cfg.Token) || pyrepr.Truthy(cfg.APIKey) {
		return true
	}
	if entry == nil {
		return false
	}
	check := entry.IsConnected
	if check == nil {
		check = entry.ValidateConfig
	}
	if check == nil {
		return true
	}
	ok, err := check(cfg)
	return err == nil && ok
}

// Home channel of a configured platform, nil otherwise
func (c *GatewayConfig) GetHomeChannel(platform Platform) *HomeChannel {
	if pc := c.Platforms[platform]; pc != nil {
		return pc.HomeChannel
	}
	return nil
}

// Mapping form of the whole config
func (c *GatewayConfig) ToDict() map[string]any {
	platforms := make(map[string]any, len(c.Platforms))
	for p, pc := range c.Platforms {
		platforms[string(p)] = pc.ToDict()
	}
	routes := make([]any, 0, len(c.ProfileRoutes))
	for _, r := range c.ProfileRoutes {
		routes = append(routes, r.ToDict())
	}
	optInt := func(p *big.Int) any {
		if p == nil {
			return nil
		}
		return pyint.Native(p)
	}
	optBool := func(p *bool) any {
		if p == nil {
			return nil
		}
		return *p
	}
	optStr := func(p *string) any {
		if p == nil {
			return nil
		}
		return *p
	}
	return map[string]any{
		"platforms": platforms, "reset_triggers": c.ResetTriggers, "quick_commands": c.QuickCommands,
		"sessions_dir": c.SessionsDir, "write_sessions_json": c.WriteSessionsJSON, "always_log_local": c.AlwaysLogLocal,
		"filter_silence_narration": c.FilterSilenceNarration, "stt_enabled": c.STTEnabled,
		"stt_echo_transcripts": c.STTEchoTranscripts, "group_sessions_per_user": c.GroupSessionsPerUser,
		"thread_sessions_per_user": c.ThreadSessionsPerUser, "max_concurrent_sessions": optInt(c.MaxConcurrentSessions),
		"multiplex_profiles": optBool(c.MultiplexProfiles), "on_all_adapters_down": c.OnAllAdaptersDown,
		"room_link_url": optStr(c.RoomLinkURL), "systemd_watchdog_seconds": c.SystemdWatchdogSeconds,
		"loop_watchdog": c.LoopWatchdog, "loop_watchdog_probe_interval_s": c.LoopWatchdogProbeIntervalS,
		"loop_watchdog_probe_timeout_s": c.LoopWatchdogProbeTimeoutS, "loop_watchdog_max_strikes": c.LoopWatchdogMaxStrikes,
		"unauthorized_dm_behavior": c.UnauthorizedDMBehavior, "unauthorized_dm_decline_message": c.UnauthorizedDMDeclineMessage,
		"streaming": c.Streaming.ToDict(), "session_store_max_age_days": pyint.Native(c.SessionStoreMaxAgeDays), "profile_routes": routes,
	}
}

// Gateway config of a mapping, top level keys winning over the nested gateway form
func GatewayConfigFromDict(ctx context.Context, raw any) (*GatewayConfig, error) {
	data := coerceDict(raw)
	nested := coerceDict(data["gateway"])
	pick := func(key string) any {
		if v, ok := data[key]; ok {
			return v
		}
		return nested[key]
	}
	keyLabel := func(key string) string {
		if _, ok := data[key]; ok {
			return key
		}
		return "gateway." + key
	}
	platformsRaw, ok := data["platforms"]
	if !ok {
		platformsRaw = map[string]any{}
	}
	platforms := map[Platform]*PlatformConfig{}
	for name, block := range coerceDict(platformsRaw) {
		if _, ok := block.(map[string]any); !ok {
			continue
		}
		p, err := ParsePlatform(ctx, name)
		if err == nil {
			var pc *PlatformConfig
			if pc, err = PlatformConfigFromDict(ctx, block); err == nil {
				platforms[p] = pc
				continue
			}
		}
		if pyErrorKind(err) != "ValueError" {
			return nil, err
		}
	}
	sttSetting := func(flat, nestedKey string) any {
		if v := data[flat]; v != nil {
			return v
		}
		return coerceDict(data["stt"])[nestedKey]
	}
	boundedFloat := func(key string, def, lo, hi float64) float64 {
		v := coerceFloat(pick(key), def)
		if !math.IsInf(v, 0) && !math.IsNaN(v) && lo <= v && v <= hi {
			return v
		}
		return def
	}
	maxStrikes := DefaultLoopWatchdogMaxStrikes
	if n := coerceInt(pick("loop_watchdog_max_strikes"), DefaultLoopWatchdogMaxStrikes); n.IsInt64() && n.Int64() >= 1 && n.Int64() <= 1000 {
		maxStrikes = int(n.Int64())
	}
	watchdog := CoerceSystemdWatchdogSeconds(pick("systemd_watchdog_seconds"), keyLabel("systemd_watchdog_seconds"))
	multiplexRaw := data["multiplex_profiles"]
	if multiplexRaw == nil {
		multiplexRaw = nested["multiplex_profiles"]
	}
	if env := envMultiplexProfilesOverride(); env != nil {
		multiplexRaw = *env
	}
	var multiplex *bool
	if multiplexRaw != nil {
		b := coerceBool(multiplexRaw, true)
		multiplex = &b
	}
	onDown := envOnAllAdaptersDownOverride()
	if onDown == "" {
		onDown = normalizeChoice(pick("on_all_adapters_down"), map[string]bool{"exit": true, "stay_alive": true}, "exit")
	}
	maxConcurrent := coerceOptionalPositiveInt(pick("max_concurrent_sessions"), keyLabel("max_concurrent_sessions"))
	maxAge := big.NewInt(90)
	ageRaw, ok := data["session_store_max_age_days"]
	if !ok {
		ageRaw = 90
	}
	if n, err := pyint.Of(ageRaw); err == nil {
		maxAge = n
		if n.Sign() < 0 {
			maxAge = big.NewInt(0)
		}
	} else if k := pyErrorKind(err); k != "TypeError" && k != "ValueError" {
		return nil, err
	}
	sessionsDir := filepath.Join(home.Dir(ctx), "sessions")
	if v, ok := data["sessions_dir"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return nil, &PyError{"TypeError", "argument should be a str or an os.PathLike object where __fspath__ returns a str, not " + pyrepr.Repr(pyrepr.TypeName(v))}
		}
		sessionsDir = pypath.PosixPath(s)
	}
	resetTriggers, ok := data["reset_triggers"]
	if !ok {
		resetTriggers = []any{"/new", "/reset"}
	}
	quickRaw, ok := data["quick_commands"]
	if !ok {
		quickRaw = map[string]any{}
	}
	var roomLink *string
	if s, ok := data["room_link_url"].(string); ok {
		roomLink = &s
	}
	declineRaw := data["unauthorized_dm_decline_message"]
	if !pyrepr.Truthy(declineRaw) {
		declineRaw = ""
	}
	streamingRaw, ok := data["streaming"]
	if !ok {
		streamingRaw = map[string]any{}
	}
	routesRaw := data["profile_routes"]
	if !pyrepr.Truthy(routesRaw) {
		routesRaw = []any{}
	}
	routes, err := ParseProfileRoutes(ctx, routesRaw)
	if err != nil {
		return nil, err
	}
	cfg := &GatewayConfig{
		Platforms: platforms, ResetTriggers: resetTriggers, QuickCommands: coerceDict(quickRaw), SessionsDir: sessionsDir,
		STTEnabled:                   coerceBool(sttSetting("stt_enabled", "enabled"), true),
		STTEchoTranscripts:           coerceBool(sttSetting("stt_echo_transcripts", "echo_transcripts"), true),
		MultiplexProfiles:            multiplex,
		RoomLinkURL:                  roomLink,
		SystemdWatchdogSeconds:       watchdog,
		LoopWatchdog:                 coerceBool(pick("loop_watchdog"), true),
		LoopWatchdogProbeIntervalS:   boundedFloat("loop_watchdog_probe_interval_s", DefaultLoopWatchdogIntervalS, 1, 3600),
		LoopWatchdogProbeTimeoutS:    boundedFloat("loop_watchdog_probe_timeout_s", DefaultLoopWatchdogTimeoutS, 1, 600),
		LoopWatchdogMaxStrikes:       maxStrikes,
		OnAllAdaptersDown:            onDown,
		MaxConcurrentSessions:        maxConcurrent,
		UnauthorizedDMBehavior:       normalizeChoice(data["unauthorized_dm_behavior"], UnauthorizedDMBehaviors, "pair"),
		UnauthorizedDMDeclineMessage: pystr.Strip(pyrepr.Str(declineRaw)),
		Streaming:                    StreamingConfigFromDict(streamingRaw),
		SessionStoreMaxAgeDays:       maxAge,
		ProfileRoutes:                routes,
	}
	for _, d := range toplevelBoolDefaults {
		v := coerceBool(data[d.key], d.def)
		switch d.key {
		case "write_sessions_json":
			cfg.WriteSessionsJSON = v
		case "always_log_local":
			cfg.AlwaysLogLocal = v
		case "filter_silence_narration":
			cfg.FilterSilenceNarration = v
		case "group_sessions_per_user":
			cfg.GroupSessionsPerUser = v
		case "thread_sessions_per_user":
			cfg.ThreadSessionsPerUser = v
		}
	}
	cfg.PostInit()
	return cfg, nil
}

// Normalized extra value of a platform when the key is present
func (c *GatewayConfig) extraChoice(platform Platform, key string, choices map[string]bool, def string) (string, bool) {
	if platform == "" {
		return "", false
	}
	pc := c.Platforms[platform]
	if pc == nil {
		return "", false
	}
	if _, ok := pc.Extra[key]; !ok {
		return "", false
	}
	return normalizeChoice(pc.Extra[key], choices, def), true
}

// Effective unauthorized DM behavior, email defaulting to ignore unless its own block opts in
func (c *GatewayConfig) GetUnauthorizedDMBehavior(platform Platform) string {
	if choice, ok := c.extraChoice(platform, "unauthorized_dm_behavior", UnauthorizedDMBehaviors, c.UnauthorizedDMBehavior); ok {
		return choice
	}
	if platform == PlatformEmail {
		return "ignore"
	}
	return c.UnauthorizedDMBehavior
}

// Effective notice delivery mode, public or private
func (c *GatewayConfig) GetNoticeDelivery(platform Platform) string {
	if choice, ok := c.extraChoice(platform, "notice_delivery", map[string]bool{"public": true, "private": true}, "public"); ok {
		return choice
	}
	return "public"
}

// Gateway config from env over config.yaml over legacy gateway.json over defaults
func LoadGatewayConfig(ctx context.Context) (*GatewayConfig, error) {
	h := home.Dir(ctx)
	gwData := LoadLegacyGatewayJSON(h)
	if err := LoadYAMLLayerAny(ctx, h, gwData); err != nil {
		slog.Warn(fmt.Sprintf("Failed to process config.yaml — falling back to .env / gateway.json values. Check %s for syntax errors. Error: %s",
			filepath.Join(h, "config.yaml"), err))
	}
	cfg, err := GatewayConfigFromDict(ctx, gwData)
	if err != nil {
		return nil, err
	}
	applyEnvOverrides(ctx, cfg, "")
	if err := validateGatewayConfig(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Warns on empty tokens and disables platforms whose token is a placeholder
func validateGatewayConfig(cfg *GatewayConfig) error {
	type tokenPlatform struct {
		platform Platform
		cfg      *PlatformConfig
		env      string
		token    string
	}
	var rows []tokenPlatform
	for p, pc := range cfg.Platforms {
		env, ok := PlatformTokenEnvNames[p]
		if !pc.Enabled || !ok || pc.Token == nil {
			continue
		}
		s, isStr := pc.Token.(string)
		if !isStr {
			return &PyError{"AttributeError", "'" + pyrepr.TypeName(pc.Token) + "' object has no attribute 'strip'"}
		}
		rows = append(rows, tokenPlatform{p, pc, env, s})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].platform < rows[j].platform })
	for _, r := range rows {
		if pystr.Strip(r.token) == "" {
			slog.Warn(fmt.Sprintf("%s is enabled but %s is empty. The adapter will likely fail to connect.", r.platform, r.env))
		}
	}
	for _, r := range rows {
		stripped := pystr.Strip(r.token)
		if stripped != "" && !provider.HasUsableSecret(r.token, 4) {
			head := []rune(stripped)
			if len(head) > 6 {
				head = head[:6]
			}
			slog.Error(fmt.Sprintf("%s is enabled but %s is set to a placeholder value ('%s'). Set a real bot token before starting the gateway. The adapter will NOT be started.",
				r.platform, r.env, string(head)+"..."))
			r.cfg.Enabled = false
		}
	}
	return nil
}
