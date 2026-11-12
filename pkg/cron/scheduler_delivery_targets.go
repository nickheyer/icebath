package cron

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	knownDeliveryPlatforms   = map[string]bool{"webhook": true, "wecom_callback": true}
	nonPushOriginPlatforms   = map[string]bool{"api_server": true}
	homeTargetPriority       = []string{"matrix", "telegram", "discord", "slack", "signal", "mattermost", "sms", "email", "dingtalk", "feishu", "wecom", "weixin", "bluebubbles", "qqbot", "whatsapp", "whatsapp_cloud"}
	legacyHomeTargetEnvVars  = map[string]string{"QQBOT_HOME_CHANNEL": "QQ_HOME_CHANNEL"}
	mirrorProvenanceRank     = map[string]int{"origin": 3, "origin_fallback": 2, "home": 2, "explicit": 1}
	threadReplyChatType      = map[string]string{"slack": "group", "matrix": "group", "telegram": "group"}
	cronDeliveryPluginScan   = func(ctx context.Context) { plugins.DiscoverPlugins(ctx, false) }
	cronDeliveryProfileNames = profiles.ListProfileNames
)

// Target is one resolved delivery destination, an empty ThreadID or ResolvedFrom is the Python None
type Target struct {
	Platform     string
	ChatID       string
	ThreadID     string
	ResolvedFrom string
}

// Mapping form of the target the way the Python dict prints and the shared route lookup reads it
func (t Target) dict() map[string]any {
	d := map[string]any{"platform": t.Platform, "chat_id": t.ChatID, "thread_id": nil}
	if t.ThreadID != "" {
		d["thread_id"] = t.ThreadID
	}
	if t.ResolvedFrom != "" {
		d["_resolved_from"] = t.ResolvedFrom
	}
	return d
}

// In channel when the relay logical sub block or the flat extra key asks for it, thread otherwise
func resolveCronSurfaceMode(pconfig *gateway.PlatformConfig, logicalPlatform string) string {
	if pconfig == nil {
		return "thread"
	}
	extra := pconfig.Extra
	sub, _ := extra[strings.ToLower(logicalPlatform)].(map[string]any)
	var raw any
	if sub != nil {
		raw = sub["cron_continuable_surface"]
	}
	if raw == nil {
		raw = extra["cron_continuable_surface"]
	}
	if raw != nil && strings.ToLower(pystr.Strip(pyrepr.Str(raw))) == "in_channel" {
		return "in_channel"
	}
	return "thread"
}

// Job origin with a platform and chat id, nil for a missing, malformed or non push origin
func resolveOrigin(job *Job) map[string]any {
	origin := job.Origin
	if origin == nil || !pyTruthy(origin["platform"]) || !pyTruthy(origin["chat_id"]) {
		return nil
	}
	if nonPushOriginPlatforms[strings.ToLower(pyrepr.Str(origin["platform"]))] {
		return nil
	}
	return origin
}

// Per job attach_to_session, then cron.mirror_delivery, then off
func (s *Store) cronMirrorDeliveryEnabled(job *Job, cfg config.Tree) bool {
	if job.AttachToSession != nil {
		return *job.AttachToSession
	}
	if cfg == nil {
		loaded, err := s.loadConfig()
		if err != nil {
			return false
		}
		cfg = loaded
	}
	cronCfg, present := cfg["cron"]
	if !present || !pyTruthy(cronCfg) {
		return false
	}
	m, ok := cronCfg.(map[string]any)
	if !ok {
		return false
	}
	return pyTruthy(m["mirror_delivery"])
}

// True when the target is the job origin conversation, a pinned origin thread must match
func targetMatchesOrigin(origin map[string]any, platform, chatID, threadID string) bool {
	if len(origin) == 0 || strings.ToLower(pyStrOrEmpty(origin["platform"])) != strings.ToLower(platform) || pyStrOrEmpty(origin["chat_id"]) != chatID {
		return false
	}
	originThread, ok := origin["thread_id"]
	return !ok || originThread == nil || pyrepr.Str(originThread) == threadID
}

// Whether a resolved target may receive the transcript mirror given its provenance
func targetMirrorEligible(job *Job, target Target, globalMirror bool, originMatch *bool) bool {
	var match bool
	if originMatch != nil {
		match = *originMatch
	} else {
		match = targetMatchesOrigin(resolveOrigin(job), target.Platform, target.ChatID, target.ThreadID)
	}
	if match {
		return true
	}
	switch target.ResolvedFrom {
	case "origin_fallback", "home":
		if job.AttachToSession != nil {
			return *job.AttachToSession
		}
		return globalMirror
	case "explicit":
		return job.AttachToSession != nil && *job.AttachToSession
	}
	return false
}

// DMs are always seedable, group keys need a real user id
func inchannelSeedAllowed(isDM bool, userID *string) bool {
	return isDM || (userID != nil && *userID != "")
}

// Fail closed forced secret redaction of anything a cron job emits outward
func (s *Store) redactCronPayload(text, what string) (out string) {
	if text == "" {
		return text
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.Warn(fmt.Sprintf("Failed to redact secrets from cron %s: %v", what, r))
			out = "[REDACTED - redaction failed]"
		}
	}()
	return tools.RedactSensitiveTextWith(&tools.Env{Home: s.home}, text, tools.RedactOptions{Force: true})
}

// Job name, or its id, redacted for outward facing text
func (s *Store) cronDisplayName(job *Job) string {
	name := job.Name
	if name == "" {
		name = jobIDOr(job, "cron")
	}
	return s.redactCronPayload(name, "job name")
}

func (s *Store) cronMirrorMessage(job *Job, text string) string {
	return fmt.Sprintf("[Cron delivery: %s]\n%s", s.cronDisplayName(job), text)
}

// Mirror of a delivery into the target chat session when enabled, every failure only logged
func (s *Store) maybeMirrorCronDelivery(ctx context.Context, job *Job, platform, chatID, mirrorText, threadID string, userID *string, enabled bool) {
	if !enabled {
		return
	}
	text := pystr.Strip(mirrorText)
	if text == "" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.Debug(fmt.Sprintf("Job '%s': delivery mirror failed for %s:%s: %v", jobIDOr(job, "?"), platform, chatID, r))
		}
	}()
	ok := gateway.MirrorToSession(ctx, platform, chatID, s.cronMirrorMessage(job, text), gateway.MirrorOptions{SourceLabel: "cron", ThreadID: optStr(threadID), UserID: userID, Role: "user"})
	if ok {
		s.log.Info(fmt.Sprintf("Job '%s': mirrored delivery into %s:%s session transcript", jobIDOr(job, "?"), platform, chatID))
	} else {
		s.log.Debug(fmt.Sprintf("Job '%s': delivery mirror skipped for %s:%s (no matching gateway session — cold start)", jobIDOr(job, "?"), platform, chatID))
	}
}

// Job id the way job.get("id", default) reads it
func jobIDOr(job *Job, def string) string {
	if job.ID == "" && !jobHasKey(job, "id") {
		return def
	}
	return job.ID
}

// Job name the way job.get("name", default) reads it
func jobNameOr(job *Job, def string) string {
	if job.Name == "" && !jobHasKey(job, "name") {
		return def
	}
	return job.Name
}

func optStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// Cron home channel env var a plugin platform entry registers, empty when none
func pluginCronEnvVar(ctx context.Context, platform string) (name string) {
	defer func() {
		if recover() != nil {
			name = ""
		}
	}()
	cronDeliveryPluginScan(ctx)
	if entry := gateway.Registry.Get(ctx, strings.ToLower(platform)); entry != nil && entry.CronDeliverEnvVar != "" {
		return entry.CronDeliverEnvVar
	}
	return ""
}

// Gateway config loader every cron path reads at call time, swapped by tests like a patched load_gateway_config
var loadGatewayConfig = gateway.LoadGatewayConfig

// Platform lookup the preflight and resolver read at call time, swapped by tests like a patched module attribute
var isKnownDeliveryPlatform = knownDeliveryPlatform

// True for a built in delivery platform or one a plugin registers a cron home env var for
func knownDeliveryPlatform(ctx context.Context, platform string) bool {
	name := strings.ToLower(platform)
	return knownDeliveryPlatforms[name] || pluginCronEnvVar(ctx, name) != ""
}

func resolveHomeEnvVar(ctx context.Context, platform string) string {
	return pluginCronEnvVar(ctx, strings.ToLower(platform))
}

// Home channel config.yaml records for the platform, nil when unavailable
func (s *Store) getConfigHomeChannel(ctx context.Context, platform string) (home *gateway.HomeChannel) {
	defer func() {
		if recover() != nil {
			home = nil
		}
	}()
	cfg, err := loadGatewayConfig(ctx)
	if err != nil {
		s.log.Debug(fmt.Sprintf("config home_channel lookup failed for platform %s", pyrepr.Repr(platform)))
		return nil
	}
	p, err := gateway.ParsePlatform(ctx, strings.ToLower(platform))
	if err != nil {
		s.log.Debug(fmt.Sprintf("config home_channel lookup failed for platform %s", pyrepr.Repr(platform)))
		return nil
	}
	return cfg.GetHomeChannel(p)
}

// Secret scope value of the env var plus suffix, then the legacy name with the same suffix
func homeEnvLookup(ctx context.Context, envVar, suffix string, strip bool) string {
	read := func(name string) string {
		v := provider.GetSecretStr(ctx, name, "")
		if strip {
			return pystr.Strip(v)
		}
		return v
	}
	value := read(envVar + suffix)
	if legacy, ok := legacyHomeTargetEnvVars[envVar]; value == "" && ok {
		value = read(legacy + suffix)
	}
	return value
}

func envHomeTargetChatID(ctx context.Context, platform string) string {
	if envVar := resolveHomeEnvVar(ctx, platform); envVar != "" {
		return homeEnvLookup(ctx, envVar, "", false)
	}
	return ""
}

// Home chat id from the env secret scope, then config.yaml home_channel
func (s *Store) getHomeTargetChatID(ctx context.Context, platform string) string {
	if v := envHomeTargetChatID(ctx, platform); v != "" {
		return v
	}
	if home := s.getConfigHomeChannel(ctx, platform); home != nil && home.ChatID != "" {
		return home.ChatID
	}
	return ""
}

// Home thread id from the telegram cron thread, the platform thread env, then config.yaml
func (s *Store) getHomeTargetThreadID(ctx context.Context, platform string) string {
	if strings.ToLower(platform) == "telegram" {
		if v := homeEnvLookup(ctx, "TELEGRAM_CRON_THREAD_ID", "", true); v != "" {
			return v
		}
	}
	if envVar := resolveHomeEnvVar(ctx, platform); envVar != "" {
		if v := homeEnvLookup(ctx, envVar, "_THREAD_ID", true); v != "" {
			return v
		}
	}
	if envHomeTargetChatID(ctx, platform) == "" {
		if home := s.getConfigHomeChannel(ctx, platform); home != nil && home.ThreadID != nil && *home.ThreadID != "" {
			return *home.ThreadID
		}
	}
	return ""
}

// Plugin platforms with a cron home env var, priority order first then the rest
func iterHomeTargetPlatforms(ctx context.Context) (out []string) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	cronDeliveryPluginScan(ctx)
	var named []string
	for _, e := range gateway.Registry.PluginEntries(ctx) {
		if e.CronDeliverEnvVar != "" {
			named = append(named, e.Name)
		}
	}
	for _, n := range homeTargetPriority {
		if containsString(named, n) {
			out = append(out, n)
		}
	}
	for _, n := range named {
		if !containsString(homeTargetPriority, n) {
			out = append(out, n)
		}
	}
	return out
}

// Relay fronted platforms when the relay itself is connected, empty otherwise
func (s *Store) relayFrontedConnectedPlatforms(ctx context.Context, connected map[string]bool) map[string]bool {
	if !connected["relay"] {
		return map[string]bool{}
	}
	fronted, err := gateway.RelayFrontedPlatformsChecked(ctx)
	if err != nil {
		s.log.Debug("relay fronted-platform lookup failed")
		return map[string]bool{}
	}
	return fronted
}

// Platforms a cron job can auto deliver to plus one Bot Chat entry per profile
func (s *Store) cronDeliveryTargets(ctx context.Context) []*pyjson.Object {
	var targets []*pyjson.Object
	connected := map[string]bool{}
	if cfg, err := loadGatewayConfig(ctx); err == nil {
		for _, p := range cfg.GetConnectedPlatforms(ctx) {
			connected[string(p)] = true
		}
		for p := range s.relayFrontedConnectedPlatforms(ctx, connected) {
			connected[p] = true
		}
	} else {
		s.log.Debug("cron_delivery_targets: gateway config unavailable")
	}
	for _, name := range iterHomeTargetPlatforms(ctx) {
		if !connected[name] || !isKnownDeliveryPlatform(ctx, name) {
			continue
		}
		o := pyjson.NewObject()
		o.Set("id", name)
		o.Set("name", pystr.Title(strings.ReplaceAll(name, "_", " ")))
		o.Set("home_target_set", s.getHomeTargetChatID(ctx, name) != "")
		var envVar any
		if v := resolveHomeEnvVar(ctx, name); v != "" {
			envVar = v
		}
		o.Set("home_env_var", envVar)
		targets = append(targets, o)
	}
	for _, profileName := range cronDeliveryProfileNames() {
		o := pyjson.NewObject()
		o.Set("id", BotChatPlatform+":"+profileName)
		o.Set("name", fmt.Sprintf("Bot Chat (%s)", profileName))
		o.Set("home_target_set", true)
		o.Set("home_env_var", nil)
		targets = append(targets, o)
	}
	return targets
}

// Slack origin thread stamped in the home chat is a session key, not a location
func (s *Store) originThreadIsStale(ctx context.Context, origin map[string]any) bool {
	if strings.ToLower(pyStrOrEmpty(origin["platform"])) != "slack" || !pyTruthy(origin["thread_id"]) {
		return false
	}
	homeChat := s.getHomeTargetChatID(ctx, "slack")
	return homeChat != "" && pyrepr.Str(origin["chat_id"]) == homeChat
}

// Thread a deliver=origin job uses with stale Slack stamps dropped
func (s *Store) originDeliveryThread(ctx context.Context, origin map[string]any) string {
	if s.originThreadIsStale(ctx, origin) {
		return s.getHomeTargetThreadID(ctx, "slack")
	}
	return optionalStr(origin["thread_id"])
}

// Text of an optional origin value, empty for None
func optionalStr(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

func (s *Store) homeTarget(ctx context.Context, platform, chatID, resolvedFrom string) Target {
	return Target{Platform: platform, ChatID: chatID, ThreadID: s.getHomeTargetThreadID(ctx, platform), ResolvedFrom: resolvedFrom}
}

// One concrete delivery target for a single deliver token, false when none resolves
func (s *Store) resolveSingleDeliveryTarget(ctx context.Context, job *Job, deliverValue string, fromBroadcast bool) (Target, bool) {
	origin := resolveOrigin(job)
	if deliverValue == "local" {
		return Target{}, false
	}
	if profile, ok := parseBotChatDeliverToken(deliverValue); ok {
		return s.resolveBotChatTarget(job, profile)
	}
	if deliverValue == "origin" {
		if origin != nil {
			return Target{Platform: pyrepr.Str(origin["platform"]), ChatID: pyrepr.Str(origin["chat_id"]), ThreadID: s.originDeliveryThread(ctx, origin), ResolvedFrom: "origin"}, true
		}
		for _, platform := range iterHomeTargetPlatforms(ctx) {
			if chatID := s.getHomeTargetChatID(ctx, platform); chatID != "" {
				name := jobNameOr(job, jobIDOr(job, "?"))
				s.log.Info(fmt.Sprintf("Job '%s' has deliver=origin but no origin; falling back to %s home channel", name, platform))
				return s.homeTarget(ctx, platform, chatID, "origin_fallback"), true
			}
		}
		return Target{}, false
	}
	if platform, rest, found := strings.Cut(deliverValue, ":"); found {
		key := strings.ToLower(platform)
		gateway.PrepareSendMessagePlatforms(ctx)
		chatID, threadID, resolutionErr := gateway.ResolveSendTarget(ctx, key, rest, true)
		if resolutionErr != "" {
			s.log.Warn(fmt.Sprintf("Invalid cron delivery target '%s': %s", deliverValue, resolutionErr))
			return Target{}, false
		}
		thread := ""
		if threadID != nil {
			thread = *threadID
		}
		if threadID == nil && key == "slack" && origin != nil && strings.ToLower(pyStrOrEmpty(origin["platform"])) == key && pyrepr.Str(origin["chat_id"]) == chatID && pyTruthy(origin["thread_id"]) && !s.originThreadIsStale(ctx, origin) {
			thread = pyrepr.Str(origin["thread_id"])
		}
		return Target{Platform: platform, ChatID: chatID, ThreadID: thread, ResolvedFrom: "explicit"}, true
	}
	platform := deliverValue
	provenance := "home"
	if fromBroadcast {
		provenance = ""
	}
	if origin != nil && pyStrOrEmpty(origin["platform"]) == platform {
		if chatID := s.getHomeTargetChatID(ctx, platform); chatID != "" {
			return s.homeTarget(ctx, platform, chatID, provenance), true
		}
		return Target{Platform: platform, ChatID: pyrepr.Str(origin["chat_id"]), ThreadID: optionalStr(origin["thread_id"])}, true
	}
	if !isKnownDeliveryPlatform(ctx, platform) {
		return Target{}, false
	}
	if chatID := s.getHomeTargetChatID(ctx, platform); chatID != "" {
		return s.homeTarget(ctx, platform, chatID, provenance), true
	}
	return Target{}, false
}

// Bot Chat target for the own profile or an existing named profile
func (s *Store) resolveBotChatTarget(job *Job, profileArg string) (t Target, ok bool) {
	if profileArg == "" {
		return Target{Platform: BotChatPlatform}, true
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.Warn(fmt.Sprintf("Job '%s': failed to resolve bot-chat profile '%s'", jobIDOr(job, "?"), profileArg))
			t, ok = Target{}, false
		}
	}()
	canon, err := profiles.NormalizeProfileName(profileArg)
	if err != nil {
		s.log.Warn(fmt.Sprintf("Job '%s': failed to resolve bot-chat profile '%s'", jobIDOr(job, "?"), profileArg))
		return Target{}, false
	}
	if !profiles.ProfileExists(canon) {
		s.log.Warn(fmt.Sprintf("Job '%s': bot-chat delivery profile '%s' not found on this machine — skipping target", jobIDOr(job, "?"), profileArg))
		return Target{}, false
	}
	return Target{Platform: BotChatPlatform, ChatID: canon}, true
}

// The all token expands to every home target platform with a chat id
func (s *Store) expandRoutingTokens(ctx context.Context, part string) []string {
	if !routingTokens[strings.ToLower(part)] {
		return []string{part}
	}
	var out []string
	for _, p := range iterHomeTargetPlatforms(ctx) {
		if s.getHomeTargetChatID(ctx, p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// Every concrete delivery target of the lane, deduplicated with the strongest provenance kept
func (s *Store) resolveDeliveryTargetsFor(ctx context.Context, job *Job, forFailure bool) []Target {
	deliver := normalizeDeliverValue(deliveryLaneValue(job, forFailure))
	if deliver == "local" {
		return nil
	}
	type key struct{ platform, chatID, threadID string }
	seen := map[key]int{}
	var targets []Target
	for _, raw := range strings.Split(deliver, ",") {
		raw = pystr.Strip(raw)
		if raw == "" {
			continue
		}
		fromBroadcast := routingTokens[strings.ToLower(raw)]
		for _, part := range s.expandRoutingTokens(ctx, raw) {
			target, ok := s.resolveSingleDeliveryTarget(ctx, job, part, fromBroadcast)
			if !ok {
				continue
			}
			k := key{strings.ToLower(target.Platform), target.ChatID, target.ThreadID}
			idx, kept := seen[k]
			if !kept {
				seen[k] = len(targets)
				targets = append(targets, target)
			} else if mirrorProvenanceRank[target.ResolvedFrom] > mirrorProvenanceRank[targets[idx].ResolvedFrom] {
				targets[idx].ResolvedFrom = target.ResolvedFrom
			}
		}
	}
	return targets
}

// Delivery targets of the job lane, the seam tests replace the way pytest patches the module attribute
var resolveDeliveryTargets = func(ctx context.Context, s *Store, job *Job, forFailure bool) []Target {
	return s.resolveDeliveryTargetsFor(ctx, job, forFailure)
}

// First delivery target of the success lane, nil when none resolves
func (s *Store) resolveDeliveryTarget(ctx context.Context, job *Job) *Target {
	targets := resolveDeliveryTargets(ctx, s, job, false)
	if len(targets) == 0 {
		return nil
	}
	return &targets[0]
}
