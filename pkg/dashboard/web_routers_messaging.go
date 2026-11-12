package dashboard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Display metadata of a messaging env var missing from the optional env table
type messagingEnvFallback struct {
	description, prompt, url string
	password, advanced       bool
}

// Labels for bridge toggles, Twilio, Email and other messaging env vars the optional env table lacks
var messagingEnvFallbacks = map[string]messagingEnvFallback{
	"SIGNAL_HTTP_URL":                 {description: "signal-cli REST API base URL, e.g. http://127.0.0.1:8080", prompt: "Signal bridge URL", url: "https://github.com/bbernhard/signal-cli-rest-api"},
	"SIGNAL_ACCOUNT":                  {description: "Signal account phone number registered with the bridge", prompt: "Signal account"},
	"SIGNAL_ALLOWED_USERS":            {description: "Comma-separated Signal users allowed to use the bot", prompt: "Allowed Signal users"},
	"WHATSAPP_ENABLED":                {description: "Enable the WhatsApp gateway adapter", prompt: "Enable WhatsApp", advanced: true},
	"WHATSAPP_MODE":                   {description: "WhatsApp bridge mode", prompt: "WhatsApp mode", advanced: true},
	"WHATSAPP_DM_POLICY":              {description: "How WhatsApp direct messages are authorized", prompt: "WhatsApp DM policy", advanced: true},
	"WHATSAPP_ALLOWED_USERS":          {description: "Comma-separated WhatsApp users allowed to use the bot", prompt: "Allowed WhatsApp users"},
	"EMAIL_ADDRESS":                   {description: "Email address to send and receive from", prompt: "Email address"},
	"EMAIL_PASSWORD":                  {description: "Email account password or app password", prompt: "Email password", password: true},
	"EMAIL_IMAP_HOST":                 {description: "IMAP server host (e.g. imap.gmail.com)", prompt: "IMAP host"},
	"EMAIL_SMTP_HOST":                 {description: "SMTP server host (e.g. smtp.gmail.com)", prompt: "SMTP host"},
	"TWILIO_ACCOUNT_SID":              {description: "Twilio Account SID", prompt: "Twilio Account SID", url: "https://www.twilio.com/console"},
	"TWILIO_AUTH_TOKEN":               {description: "Twilio Auth Token", prompt: "Twilio Auth Token", password: true},
	"WECOM_BOT_ID":                    {description: "WeCom group bot ID", prompt: "WeCom Bot ID"},
	"WECOM_SECRET":                    {description: "WeCom group bot secret", prompt: "WeCom Secret", password: true},
	"WECOM_CALLBACK_CORP_ID":          {description: "WeCom corp ID", prompt: "WeCom Corp ID"},
	"WECOM_CALLBACK_CORP_SECRET":      {description: "WeCom app corp secret", prompt: "WeCom Corp Secret", password: true},
	"WECOM_CALLBACK_AGENT_ID":         {description: "WeCom app agent ID", prompt: "WeCom Agent ID"},
	"WECOM_CALLBACK_TOKEN":            {description: "WeCom callback verification token", prompt: "WeCom Token"},
	"WECOM_CALLBACK_ENCODING_AES_KEY": {description: "WeCom callback AES encoding key", prompt: "WeCom AES Key", password: true},
	"WEIXIN_ACCOUNT_ID":               {description: "iLink Bot account ID obtained through QR login in icebath gateway setup", prompt: "iLink Bot account ID"},
	"WEIXIN_TOKEN":                    {description: "iLink Bot token obtained through QR login in icebath gateway setup", prompt: "iLink Bot token", password: true},
	"WEIXIN_BASE_URL":                 {description: "iLink API base URL saved by QR login (default: https://ilinkai.weixin.qq.com)", prompt: "iLink API base URL"},
	"FEISHU_APP_ID":                   {description: "Feishu / Lark app ID", prompt: "App ID"},
	"FEISHU_APP_SECRET":               {description: "Feishu / Lark app secret", prompt: "App secret", password: true},
	"FEISHU_ENCRYPT_KEY":              {description: "Feishu / Lark encrypt key", prompt: "Encrypt key", password: true},
	"FEISHU_VERIFICATION_TOKEN":       {description: "Feishu / Lark verification token", prompt: "Verification token", password: true},
	"DINGTALK_CLIENT_ID":              {description: "DingTalk client ID (App key)", prompt: "Client ID"},
	"DINGTALK_CLIENT_SECRET":          {description: "DingTalk client secret (App secret)", prompt: "Client secret", password: true},
}

// Write checks the registry entry declares, else those of the bundled platform's dashboard card
func messagingEnvValueRules(ctx context.Context, platformID string) map[string]gateway.EnvValueRule {
	if entry := gateway.Registry.Get(ctx, platformID); entry != nil && len(entry.EnvValueRules) > 0 {
		out := make(map[string]gateway.EnvValueRule, len(entry.EnvValueRules))
		for key, rule := range entry.EnvValueRules {
			out[key] = rule
		}
		return out
	}
	return gateway.BundledDashboardCardRules(platformID)
}

// 400 with the rule message when a non empty value fails its key's write check
func validateMessagingEnvValue(rules map[string]gateway.EnvValueRule, key, value string) error {
	if rule, ok := rules[key]; value != "" && ok && !rule.Check(value) {
		return NewHTTPException(400, rule.Message)
	}
	return nil
}

var allowlistKeyRE = regexp.MustCompile(`_ALLOWED(?:_[A-Z]+)?\n?\z`)

// Label, help and flags of a messaging env var, allowlists flagged as lists that carry their saved value
func messagingEnvInfo(key string) *pyjson.Object {
	description, prompt, help, url, isPassword, advanced := "", key, "", any(nil), false, false
	if info, ok := config.OptionalEnvVars.Get(key); ok {
		description, help, isPassword, advanced = info.Description, info.Help, info.Password, info.Advanced
		if info.Prompt != "" {
			prompt = info.Prompt
		}
		if info.URL != "" {
			url = info.URL
		}
	} else if info, ok := messagingEnvFallbacks[key]; ok {
		description, prompt, isPassword, advanced = info.description, info.prompt, info.password, info.advanced
		if info.url != "" {
			url = info.url
		}
	}
	return pyjson.ObjectOf(
		"description", description,
		"prompt", prompt,
		"help", help,
		"url", url,
		"is_password", isPassword,
		"is_list", !isPassword && allowlistKeyRE.MatchString(key),
		"advanced", advanced,
	)
}

// Catalog card of a platform, nil when the catalog has none
func catalogLookup(ctx context.Context, platformID string) *pyjson.Object {
	for _, entry := range messagingPlatformCatalog(ctx) {
		if entry.GetOr("id", nil) == platformID {
			return entry
		}
	}
	return nil
}

// Catalog card of a platform, 404 when unknown
func requirePlatform(ctx context.Context, platformID string) (*pyjson.Object, error) {
	entry := catalogLookup(ctx, platformID)
	if entry == nil {
		return nil, NewHTTPException(404, "Unknown messaging platform: "+platformID)
	}
	return entry, nil
}

// Strings of a catalog list field
func entryStrings(entry *pyjson.Object, key string) []string {
	items, _ := entry.GetOr(key, []any{}).([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, pyrepr.Str(item))
	}
	return out
}

// Mapping value of a config node, false for anything else
func configMapping(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// Home channel in its mapping key order with the optional ids only when set
func homeChannelObject(h *gateway.HomeChannel) *pyjson.Object {
	out := pyjson.ObjectOf("platform", string(h.Platform), "chat_id", h.ChatID, "name", h.Name)
	for _, field := range []struct {
		key   string
		value *string
	}{{"thread_id", h.ThreadID}, {"user_id", h.UserID}, {"scope_id", h.ScopeID}} {
		if field.value != nil && *field.value != "" {
			out.Set(field.key, *field.value)
		}
	}
	return out
}

// Enabled, configured and home channel of a platform, scoped reads judging only the profile's config, env and sources
func platformEnablement(ctx context.Context, platformID string, entry *pyjson.Object, envOnDisk map[string]string,
	scoped bool, sourceKeys map[string]bool) (enabled, configured bool, homeChannel any) {
	required := entryStrings(entry, "required_env")
	if scoped {
		configured = len(required) > 0
		for _, key := range required {
			if envOnDisk[key] == "" && !sourceKeys[key] {
				configured = false
			}
		}
		failed := func() (failed bool) {
			defer func() {
				if r := recover(); r != nil {
					failed = true
				}
			}()
			cfg, err := webLoadConfig(ctx)
			if err != nil {
				return true
			}
			platforms := map[string]any{}
			if raw := cfg["platforms"]; pyrepr.Truthy(raw) {
				m, ok := configMapping(raw)
				if !ok {
					return true
				}
				platforms = m
			}
			platCfg, ok := configMapping(platforms[platformID])
			if !ok {
				platCfg = map[string]any{}
			}
			hc := platCfg["home_channel"]
			statusKnown := false
			if len(required) == 0 {
				builtinEnabled, builtinConnected, known, err := gateway.BuiltinPlatformStatus(ctx, platformID, cfg)
				if err != nil {
					return true
				}
				if known {
					statusKnown = true
					enabled, configured = builtinEnabled, builtinConnected
				}
			}
			if !statusKnown {
				rawEnabled := platCfg["enabled"]
				enabled = rawEnabled != false && (pyrepr.Truthy(rawEnabled) || configured)
			}
			homeChannel = nil
			if _, isDict := configMapping(hc); isDict {
				homeChannel = configJSON(hc)
			}
			return false
		}()
		if failed {
			routersLog().Debug(fmt.Sprintf("Profile-scoped status for %s failed", platformID))
			enabled, homeChannel = false, nil
		}
		return enabled, configured, homeChannel
	}
	failed := func() (failed bool) {
		defer func() {
			if r := recover(); r != nil {
				failed = true
			}
		}()
		gatewayConfig, err := gateway.LoadGatewayConfig(ctx)
		if err != nil {
			return true
		}
		platform, err := gateway.ParsePlatform(ctx, platformID)
		if err != nil {
			return true
		}
		platformConfig := gatewayConfig.Platforms[platform]
		enabled = platformConfig != nil && platformConfig.Enabled
		configured = platformConfig != nil && gatewayConfig.IsPlatformConnected(ctx, platform, platformConfig)
		homeChannel = nil
		if platformConfig != nil && platformConfig.HomeChannel != nil {
			homeChannel = homeChannelObject(platformConfig.HomeChannel)
		}
		return false
	}()
	if failed {
		enabled, homeChannel = false, nil
		configured = true
		for _, key := range required {
			if envOnDisk[key] == "" && os.Getenv(key) == "" {
				configured = false
			}
		}
	}
	return enabled, configured, homeChannel
}

// Enablement and gateway status readers of the messaging routes, the module attributes hermes tests patch
var (
	messagingPlatformEnablement = platformEnablement
	messagingReadRuntimeStatus  = gateway.ReadRuntimeStatus
	messagingRuntimePidProbe    = gateway.GetRuntimeStatusRunningPid
	messagingPidProbe           = func(pidPath string) (int, bool, error) {
		return gateway.GetRunningPidCached(pidPath, true, time.Second)
	}
)

// Channels page card of one platform, its state judged by the same liveness ladder the status route uses
func messagingPlatformPayload(ctx context.Context, entry *pyjson.Object, envOnDisk map[string]string, runtime *pyjson.Object,
	scoped bool, profileHome string, sourceKeys map[string]bool) *pyjson.Object {
	platformID := entry.GetOr("id", "").(string)
	rt := runtime
	if rt == nil {
		rt = pyjson.NewObject()
	}
	runtimePlatform := pyjson.NewObject()
	if runtimePlatforms, ok := rt.GetOr("platforms", nil).(*pyjson.Object); ok {
		if own, ok := runtimePlatforms.GetOr(platformID, pyjson.NewObject()).(*pyjson.Object); ok {
			runtimePlatform = own
		}
	}
	opts := gateway.LivenessOptions{ProfileDir: profileHome, Runtime: runtime, RuntimeGiven: true, PidProbe: messagingPidProbe,
		RuntimeReader:   func(p string) (*pyjson.Object, error) { return messagingReadRuntimeStatus(p), nil },
		RuntimePidProbe: messagingRuntimePidProbe}
	if healthURL, _ := GatewayHealthURL(); healthURL != "" {
		opts.HealthProbe = func() (bool, *pyjson.Object, error) {
			alive, body := probeGatewayHealth(ctx)
			obj, _ := body.(*pyjson.Object)
			return alive, obj, nil
		}
	}
	gatewayRunning := gateway.ResolveGatewayLiveness(opts).Running
	if !gatewayRunning {
		runtimePlatform = pyjson.NewObject()
	}
	envValue := func(key string) string {
		if v := envOnDisk[key]; v != "" {
			return v
		}
		if scoped {
			return ""
		}
		return os.Getenv(key)
	}
	required := entryStrings(entry, "required_env")
	envVars := []any{}
	for _, key := range entryStrings(entry, "env_vars") {
		value, info := envValue(key), messagingEnvInfo(key)
		row := pyjson.ObjectOf("key", key, "required", slices.Contains(required, key), "is_set", value != "" || sourceKeys[key],
			"redacted_value", redactedCredentialPreview(value))
		if info.GetOr("is_list", false) == true {
			row.Set("value", value)
		} else {
			row.Set("value", nil)
		}
		for _, k := range info.Keys {
			row.Set(k, info.Vals[k])
		}
		envVars = append(envVars, row)
	}
	enabled, configured, homeChannel := messagingPlatformEnablement(ctx, platformID, entry, envOnDisk, scoped, sourceKeys)
	if gatewayRunning && pyrepr.Truthy(runtimePlatform.GetOr("mirrored_from", nil)) {
		enabled, configured = true, true
	}
	state := runtimePlatform.GetOr("state", nil)
	switch {
	case !enabled:
		state = "disabled"
	case !configured:
		state = "not_configured"
	case gatewayRunning && !pyrepr.Truthy(state):
		state = "pending_restart"
	case !gatewayRunning && !pyrepr.Truthy(state):
		state = "gateway_stopped"
		if gateway.RetainedGatewayState(rt) == "startup_failed" {
			state = "startup_failed"
		}
	}
	errorCode := runtimePlatform.GetOr("error_code", nil)
	errorMessage := runtimePlatform.GetOr("error_message", nil)
	if state == "startup_failed" {
		if !pyrepr.Truthy(errorCode) {
			errorCode = "startup_failed"
		}
		if !pyrepr.Truthy(errorMessage) {
			errorMessage = rt.GetOr("exit_reason", nil)
		}
	}
	var ingressURL any
	if gatewayRunning {
		ingressURL = runtimePlatform.GetOr("ingress_url", nil)
	}
	payload := pyjson.ObjectOf(
		"id", platformID, "name", entry.GetOr("name", nil), "description", entry.GetOr("description", nil),
		"docs_url", entry.GetOr("docs_url", nil), "enabled", enabled, "configured", configured,
		"gateway_running", gatewayRunning, "state", state, "error_code", errorCode,
		"error_message", errorMessage, "updated_at", runtimePlatform.GetOr("updated_at", nil),
		"home_channel", homeChannel, "env_vars", envVars,
		"ingress_url", ingressURL,
	)
	if platformID == "whatsapp" {
		whatsappMode := pystr.Strip(envValue("WHATSAPP_MODE"))
		if whatsappMode != "bot" && whatsappMode != "self-chat" {
			whatsappMode = ""
		}
		payload.Set("whatsapp_setup", pyjson.ObjectOf(
			"mode", whatsappMode,
			"allowed_users_set", pystr.Strip(envValue("WHATSAPP_ALLOWED_USERS")) != "",
			"home_channel_set", pyrepr.Truthy(homeChannel),
		))
	}
	return payload
}

// Cards of entries inside the profile scope, the gateway readers given the scoped dir explicitly
func platformPayloads(ctx context.Context, scopedDir string, entries []*pyjson.Object) ([]any, error) {
	envOnDisk := webLoadEnv(ctx)
	statusPath := ""
	if scopedDir != "" {
		statusPath = filepath.Join(scopedDir, "gateway_state.json")
	}
	runtime := messagingReadRuntimeStatus(statusPath)
	ownHome := scopedDir
	if ownHome == "" {
		ownHome = home.Process()
	}
	served, err := gateway.ServedProfileRuntime(ownHome, runtime, nil)
	if err != nil {
		return nil, err
	}
	if served != nil && served.Len() > 0 {
		runtime = served
	}
	sourceKeys := map[string]bool{}
	if scopedDir != "" {
		for key, value := range provider.SecretSourceValues(scopedDir) {
			if pystr.Strip(value) != "" {
				sourceKeys[key] = true
			}
		}
	}
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, messagingPlatformPayload(ctx, entry, envOnDisk, runtime, scopedDir != "", scopedDir, sourceKeys))
	}
	return out, nil
}

// Route error of an onboarding save, a ValueError becoming 400 with its text and anything else a logged 500 detail
func onboardingSaveErrors(err error, logMsg, detail string) error {
	if err == nil {
		return nil
	}
	if h, ok := asHTTPException(err); ok {
		return h
	}
	if pyrepr.ExceptionName(err) == "ValueError" {
		return NewHTTPException(400, pyrepr.ExceptionMessage(err))
	}
	routersLog().Error(logMsg, "exc_info", err)
	return NewHTTPException(500, detail)
}

// Reason enabling a shared listener platform on a multiplexed secondary is refused, empty when allowed
func multiplexPortBindingConflict(ctx context.Context, platformID, requestedProfile string) (string, error) {
	if !gateway.SharedListenerMirrorPlatforms[platformID] {
		return "", nil
	}
	requested := pystr.Strip(requestedProfile)
	target := requested
	if requested == "" || pystr.Lower(requested) == "current" {
		target = profiles.GetActiveProfileName(ctx)
	} else if _, err := resolveProfileDir(requested); err != nil {
		return "", err
	}
	if target == "default" || target == "custom" {
		return "", nil
	}
	multiplexes, err := gateway.DefaultGatewayMultiplexes(ctx, "")
	if err != nil {
		return "", err
	}
	if !multiplexes {
		return "", nil
	}
	return fmt.Sprintf("Cannot enable '%s' on profile '%s': gateway.multiplex_profiles is on and the "+
		"default profile's listener already serves it for every profile at /p/%s/. Configure it "+
		"on the default profile instead (disabling or clearing it here is still allowed).", platformID, target, target), nil
}

// Multiplexer hooks of the hot serve notice, the gateway CLI attributes hermes tests patch
var (
	messagingCurrentProfileName   = gateway.CurrentProfileName
	messagingProfileServedByMux   = gateway.NamedProfileServedByRunningMultiplexerFor
	messagingNotifyProfilesChange = gateway.NotifyMultiplexerProfilesChanged
)

// Whether a live multiplexer serves the written profile and was told to rebuild its adapters
func notifyMultiplexerHotServe(ctx context.Context, profile string) (bool, error) {
	name := pystr.Strip(profile)
	if name == "" {
		current, err := messagingCurrentProfileName(ctx)
		if err != nil {
			return false, err
		}
		name = current
	}
	if name == "" || name == "default" {
		return false, nil
	}
	served, err := messagingProfileServedByMux(ctx, &name)
	if err != nil {
		return false, err
	}
	if !served {
		return false, nil
	}
	_, answered := messagingNotifyProfilesChange(ctx, name, 8*time.Second)
	return answered, nil
}

// Allowed users and allow all env names of a platform, both empty without a per user authorization surface
func platformAuthzEnvNames(ctx context.Context, platformID string) (allowed, allowAll string) {
	defer func() { recover() }()
	entry := gateway.Registry.Get(ctx, platformID)
	if entry != nil && entry.TrustedInbound {
		return "", ""
	}
	allowed = truthyStr(gateway.AuthzField(platformID, "allowed_users_env", entry))
	allowAll = truthyStr(gateway.AuthzField(platformID, "allow_all_env", entry))
	return allowed, allowAll
}

// Lowercased dm_policy of the active profile's platform extra block, empty when unset or unreadable
func platformDMPolicy(ctx context.Context, platformID string) (policy string) {
	defer func() {
		if r := recover(); r != nil {
			policy = ""
		}
	}()
	cfg, err := webLoadConfig(ctx)
	if err != nil {
		return ""
	}
	platforms := map[string]any{}
	if raw := cfg["platforms"]; pyrepr.Truthy(raw) {
		m, ok := configMapping(raw)
		if !ok {
			return ""
		}
		platforms = m
	}
	platCfg, ok := configMapping(platforms[platformID])
	if !ok {
		return ""
	}
	extra := any(map[string]any{})
	if raw, held := platCfg["extra"]; held {
		extra = raw
	}
	extraMap, ok := configMapping(extra)
	if !ok {
		return ""
	}
	return pystr.Lower(pystr.Strip(truthyStr(extraMap["dm_policy"])))
}

// Number of non blank comma separated entries
func countCSV(value string) int {
	n := 0
	for _, part := range strings.Split(value, ",") {
		if pystr.Strip(part) != "" {
			n++
		}
	}
	return n
}

// Plural suffix for a count
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Who can reach a connected platform, open, allowlist, pairing or deny all, nil without an authorization surface
func messagingPlatformPosture(ctx context.Context, platformID string, entry *pyjson.Object, scoped bool, allowedEnv, allowAllEnv string) *pyjson.Object {
	if allowedEnv == "" {
		return nil
	}
	name := pyrepr.Str(entry.GetOr("name", nil))
	gateEnv := func(key string) string {
		if scoped {
			return webLoadEnv(ctx)[key]
		}
		return gateway.PlatformGateEnv(ctx, key, "")
	}
	truthy := map[string]bool{"true": true, "1": true, "yes": true}
	if allowAllEnv != "" && truthy[pystr.Lower(gateEnv(allowAllEnv))] {
		return pyjson.ObjectOf("posture", "open", "message", fmt.Sprintf("%s is open: %s is set.", name, allowAllEnv))
	}
	if truthy[pystr.Lower(gateEnv("GATEWAY_ALLOW_ALL_USERS"))] {
		return pyjson.ObjectOf("posture", "open", "message", "Gateway is open: GATEWAY_ALLOW_ALL_USERS is set.")
	}
	telegramOnly := func(key string) string {
		if platformID == "telegram" {
			return gateEnv(key)
		}
		return ""
	}
	allowlists := pyjson.NewObject()
	allowlists.Set(allowedEnv, gateEnv(allowedEnv))
	allowlists.Set("GATEWAY_ALLOWED_USERS", gateEnv("GATEWAY_ALLOWED_USERS"))
	allowlists.Set("TELEGRAM_GROUP_ALLOWED_USERS", telegramOnly("TELEGRAM_GROUP_ALLOWED_USERS"))
	allowlists.Set("TELEGRAM_GROUP_ALLOWED_CHATS", telegramOnly("TELEGRAM_GROUP_ALLOWED_CHATS"))
	for _, envName := range allowlists.Keys {
		if value := allowlists.Vals[envName].(string); pystr.Strip(value) != "" {
			n := countCSV(value)
			return pyjson.ObjectOf("posture", "allowlist",
				"message", fmt.Sprintf("Connected. %d user%s allowlisted via %s.", n, pluralS(n), envName))
		}
	}
	if platformDMPolicy(ctx, platformID) == "allowlist" {
		return pyjson.ObjectOf("posture", "allowlist", "message", "Connected. Access restricted by the configured allow_from policy.")
	}
	approved, pending := 0, 0
	func() {
		defer func() { recover() }()
		store, err := gateway.NewPairingStore(ctx, "")
		if err != nil {
			return
		}
		approvedRows, err := store.ListApproved(platformID)
		if err != nil {
			return
		}
		approved = len(approvedRows)
		pendingRows, err := store.ListPending(ctx, platformID)
		if err != nil {
			return
		}
		pending = len(pendingRows)
	}()
	if approved > 0 {
		return pyjson.ObjectOf("posture", "pairing", "message", fmt.Sprintf("Connected. %d user%s paired.", approved, pluralS(approved)))
	}
	base := fmt.Sprintf("%s is connected, but no users are authorized yet — set %s, approve a pairing request, "+
		"or enable pairing for new users.", name, allowedEnv)
	if pending > 0 {
		base += fmt.Sprintf(" %d pairing request%s pending approval.", pending, pluralS(pending))
	}
	return pyjson.ObjectOf("posture", "deny_all", "message", base)
}

// Channels page listing of the scoped profile with its env path and gateway start command
func getMessagingPlatforms(ctx context.Context, profile string) (*pyjson.Object, error) {
	sctx, scoped, release, err := profileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	defer release()
	subcommand, err := gatewaySubcommand(sctx, profile, "start")
	if err != nil {
		return nil, err
	}
	payloads, err := platformPayloads(sctx, scoped, messagingPlatformCatalog(sctx))
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf(
		"env_path", home.EnvPath(home.Dir(sctx)),
		"gateway_start_command", strings.Join(append([]string{"icebath"}, subcommand...), " "),
		"platforms", payloads,
	), nil
}

// Validates then applies a platform's env writes, clears and enabled flag, refusing a multiplex listener conflict first
func updateMessagingPlatform(ctx context.Context, platformID string, body *pyjson.Object, profile string) (*pyjson.Object, error) {
	targetProfile := firstProfile(body.GetOr("profile", nil), profile)
	type resolved struct {
		entry *pyjson.Object
		rules map[string]gateway.EnvValueRule
	}
	found, err := scopedToThread(ctx, targetProfile, func(sctx context.Context) (resolved, error) {
		entry, err := requirePlatform(sctx, platformID)
		if err != nil {
			return resolved{}, err
		}
		return resolved{entry, messagingEnvValueRules(sctx, platformID)}, nil
	})
	if err != nil {
		return nil, err
	}
	enabled := body.GetOr("enabled", nil)
	if enabled == true {
		conflict, err := multiplexPortBindingConflict(ctx, platformID, targetProfile)
		if err != nil {
			return nil, err
		}
		if conflict != "" {
			current := targetProfile
			if current == "" {
				current = "current"
			}
			routersLog().Info(fmt.Sprintf("Rejected messaging platform update: platform=%s profile=%s "+
				"(multiplex port-binding conflict)", platformID, current))
			return nil, NewHTTPException(409, conflict)
		}
	}
	allowedEnv := map[string]bool{}
	for _, key := range entryStrings(found.entry, "env_vars") {
		allowedEnv[key] = true
	}
	checkAllowed := func(key string) error {
		if !allowedEnv[key] {
			return NewHTTPException(400, fmt.Sprintf("%s is not configurable for %s", key, pyrepr.Str(found.entry.GetOr("name", nil))))
		}
		return nil
	}
	env, _ := body.GetOr("env", pyjson.NewObject()).(*pyjson.Object)
	if env == nil {
		env = pyjson.NewObject()
	}
	clearRaw, _ := body.GetOr("clear_env", []any{}).([]any)
	clearEnv := make([]string, 0, len(clearRaw))
	for _, key := range clearRaw {
		clearEnv = append(clearEnv, pyrepr.Str(key))
	}
	result, err := func() (*pyjson.Object, error) {
		_, err := scopedToThread(ctx, targetProfile, func(sctx context.Context) (struct{}, error) {
			updates := pyjson.NewObject()
			for _, key := range clearEnv {
				if err := checkAllowed(key); err != nil {
					return struct{}{}, err
				}
			}
			for _, key := range env.Keys {
				if err := checkAllowed(key); err != nil {
					return struct{}{}, err
				}
				trimmed := pystr.Strip(pyrepr.Str(env.Vals[key]))
				if trimmed == "" {
					continue
				}
				if isRedactedCredentialPreview(trimmed) {
					return struct{}{}, NewHTTPException(400, redactedCredentialWriteDetail)
				}
				if err := validateMessagingEnvValue(found.rules, key, trimmed); err != nil {
					return struct{}{}, err
				}
				updates.Set(key, trimmed)
			}
			h := home.Dir(sctx)
			for _, key := range clearEnv {
				if _, err := config.RemoveHomeEnvValue(sctx, h, key); err != nil {
					return struct{}{}, err
				}
			}
			for _, key := range updates.Keys {
				if err := config.SaveEnvValueContext(sctx, h, key, updates.Vals[key].(string)); err != nil {
					return struct{}{}, err
				}
			}
			if enabled != nil {
				if err := writePlatformEnabled(sctx, platformID, enabled == true); err != nil {
					return struct{}{}, err
				}
			}
			return struct{}{}, nil
		})
		if err != nil {
			return nil, err
		}
		current := targetProfile
		if current == "" {
			current = "current"
		}
		envKeys := append([]string(nil), env.Keys...)
		slices.Sort(envKeys)
		clearedKeys := append([]string(nil), clearEnv...)
		slices.Sort(clearedKeys)
		routersLog().Info(fmt.Sprintf("Messaging platform updated: platform=%s profile=%s enabled=%s env_keys=%s cleared_keys=%s",
			platformID, current, pyrepr.Str(enabled), pyrepr.Repr(stringsAny(envKeys)), pyrepr.Repr(stringsAny(clearedKeys))))
		hotServed, err := notifyMultiplexerHotServe(ctx, targetProfile)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("ok", true, "platform", platformID, "hot_served", hotServed), nil
	}()
	if err != nil {
		detail := "Internal server error"
		return nil, httpFailure(err, fmt.Sprintf("PUT /api/messaging/platforms/%s failed", platformID), 500, "", &detail)
	}
	return result, nil
}

// Connection check of one platform with the auth posture of a connected one
func testMessagingPlatform(ctx context.Context, platformID, profile string) (*pyjson.Object, error) {
	sctx, scoped, release, err := profileScope(ctx, profile)
	if err != nil {
		return nil, err
	}
	entry, payload, posture, err := func() (*pyjson.Object, *pyjson.Object, *pyjson.Object, error) {
		defer release()
		entry, err := requirePlatform(sctx, platformID)
		if err != nil {
			return nil, nil, nil, err
		}
		allowedEnv, allowAllEnv := platformAuthzEnvNames(sctx, platformID)
		payloads, err := platformPayloads(sctx, scoped, []*pyjson.Object{entry})
		if err != nil {
			return nil, nil, nil, err
		}
		payload := payloads[0].(*pyjson.Object)
		var posture *pyjson.Object
		if payload.GetOr("state", nil) == "connected" {
			posture = messagingPlatformPosture(sctx, platformID, entry, scoped != "", allowedEnv, allowAllEnv)
		}
		return entry, payload, posture, nil
	}()
	if err != nil {
		return nil, err
	}
	name := pyrepr.Str(entry.GetOr("name", nil))
	result := func(ok bool, message any) *pyjson.Object {
		return pyjson.ObjectOf("ok", ok, "state", payload.GetOr("state", nil), "message", message)
	}
	if !pyrepr.Truthy(payload.GetOr("enabled", nil)) {
		return result(false, name+" is disabled. Enable it, then restart the gateway."), nil
	}
	if !pyrepr.Truthy(payload.GetOr("configured", nil)) {
		var missing []string
		rows, _ := payload.GetOr("env_vars", []any{}).([]any)
		for _, raw := range rows {
			field := raw.(*pyjson.Object)
			if pyrepr.Truthy(field.GetOr("required", nil)) && !pyrepr.Truthy(field.GetOr("is_set", nil)) {
				missing = append(missing, pyrepr.Str(field.GetOr("key", nil)))
			}
		}
		if len(missing) > 0 {
			return result(false, "Missing required setup: "+strings.Join(missing, ", ")), nil
		}
		return result(false, "Platform setup is incomplete."), nil
	}
	if !pyrepr.Truthy(payload.GetOr("gateway_running", nil)) {
		return result(false, "Gateway is not running. Restart the gateway to connect this platform."), nil
	}
	if payload.GetOr("state", nil) == "connected" {
		if posture == nil {
			return result(true, name+" is connected."), nil
		}
		return pyjson.ObjectOf("ok", true, "state", payload.GetOr("state", nil), "message", posture.GetOr("message", nil),
			"auth_posture", posture.GetOr("posture", nil)), nil
	}
	if errorMessage := payload.GetOr("error_message", nil); pyrepr.Truthy(errorMessage) {
		return result(false, errorMessage), nil
	}
	return result(false, "Setup looks complete, but the gateway has not reported a connection yet. Restart the gateway."), nil
}

// Router of the WhatsApp and Telegram onboarding routes and the per platform enable, config and test routes
func messagingRouter() *APIRouter {
	r := NewAPIRouter("")
	optProfile := QueryParam("profile", optStr, nil)
	pairingPath := PathParam("pairing_id", Str)
	platformPath := PathParam("platform_id", Str)
	r.Post("/api/messaging/whatsapp/onboarding/start", &Endpoint{
		Name:   "start_whatsapp_onboarding",
		Params: []Param{optProfile},
		Body:   []BodyParam{Body("body", whatsAppOnboardingStartModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return startWhatsappOnboarding(ctx, c.Object("body"), c.Str("profile"))
		},
	})
	r.Get("/api/messaging/whatsapp/onboarding/{pairing_id}", &Endpoint{
		Name:   "get_whatsapp_onboarding_status",
		Params: []Param{pairingPath},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getWhatsappOnboardingStatus(c.Str("pairing_id"))
		},
	})
	r.Post("/api/messaging/whatsapp/onboarding/{pairing_id}/apply", &Endpoint{
		Name:   "apply_whatsapp_onboarding",
		Params: []Param{pairingPath, optProfile},
		Body:   []BodyParam{Body("body", whatsAppOnboardingApplyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return applyWhatsappOnboarding(ctx, c.Str("pairing_id"), c.Object("body"), c.Str("profile"))
		},
	})
	r.Delete("/api/messaging/whatsapp/onboarding/{pairing_id}", &Endpoint{
		Name:   "cancel_whatsapp_onboarding",
		Params: []Param{pairingPath},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return cancelWhatsappOnboarding(c.Str("pairing_id")), nil
		},
	})
	r.Post("/api/messaging/telegram/onboarding/start", &Endpoint{
		Name: "start_telegram_onboarding",
		Body: []BodyParam{Body("body", telegramOnboardingStartModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return startTelegramOnboarding(ctx, c.Object("body"))
		},
	})
	r.Get("/api/messaging/telegram/onboarding/{pairing_id}", &Endpoint{
		Name:   "get_telegram_onboarding_status",
		Params: []Param{pairingPath},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return getTelegramOnboardingStatus(ctx, c.Str("pairing_id"))
		},
	})
	r.Post("/api/messaging/telegram/onboarding/{pairing_id}/apply", &Endpoint{
		Name:   "apply_telegram_onboarding",
		Params: []Param{pairingPath, optProfile},
		Body:   []BodyParam{Body("body", telegramOnboardingApplyModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return applyTelegramOnboarding(ctx, c.Str("pairing_id"), c.Object("body"), c.Str("profile"))
		},
	})
	r.Delete("/api/messaging/telegram/onboarding/{pairing_id}", &Endpoint{
		Name:   "cancel_telegram_onboarding",
		Params: []Param{pairingPath},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return cancelTelegramOnboarding(c.Str("pairing_id")), nil
		},
	})
	r.Get("/api/messaging/platforms", &Endpoint{
		Name:   "get_messaging_platforms",
		Params: []Param{optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) { return getMessagingPlatforms(ctx, c.Str("profile")) },
	})
	r.Put("/api/messaging/platforms/{platform_id}", &Endpoint{
		Name:   "update_messaging_platform",
		Params: []Param{platformPath, optProfile},
		Body:   []BodyParam{Body("body", messagingPlatformUpdateModel)},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return updateMessagingPlatform(ctx, c.Str("platform_id"), c.Object("body"), c.Str("profile"))
		},
	})
	r.Post("/api/messaging/platforms/{platform_id}/test", &Endpoint{
		Name:   "test_messaging_platform",
		Params: []Param{platformPath, optProfile},
		Handle: func(ctx context.Context, c *Call) (any, error) {
			return testMessagingPlatform(ctx, c.Str("platform_id"), c.Str("profile"))
		},
	})
	return r
}
