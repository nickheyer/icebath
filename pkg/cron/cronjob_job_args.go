package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const threadHorizonMinutes = 60

// Resolver pieces the base_url guard reads, swapped by tests like patched runtime_provider attributes
var cronProviderResolver = struct {
	hostGated        func(ctx context.Context, h, baseURL string, ollama bool) []string
	hasNamed         func(ctx context.Context, h, name string) bool
	named            func(ctx context.Context, h, name string) (map[string]any, error)
	resolveRequested func(ctx context.Context, h, requested string) (string, error)
	registryURL      func(id string) (string, bool)
	registryURLs     func() []string
}{provider.HostGatedEnvKeyCandidates, provider.HasNamedCustomProvider, provider.NamedCustomProvider,
	provider.ResolveRequestedProvider, provider.RegistryInferenceBaseURL, provider.RegistryInferenceBaseURLs}

// True when the first fire of a one shot lands inside the live creating conversation window
func (s *Store) firstFireWithinThreadHorizon(schedule any) bool {
	var parsed *Schedule
	switch v := schedule.(type) {
	case nil:
		return false
	case *Schedule:
		parsed = v
	case map[string]any:
		if kind, _ := v["kind"].(string); kind != "once" || !pyrepr.Truthy(v["run_at"]) {
			return false
		}
		parsed = &Schedule{Kind: "once", RunAt: pyrepr.Str(v["run_at"])}
	case string:
		if v == "" {
			return false
		}
		p, err := s.parseSchedule(v)
		if err != nil {
			return false
		}
		parsed = p
	default:
		return false
	}
	if parsed == nil || parsed.Kind != "once" || parsed.RunAt == "" {
		return false
	}
	iso, err := ParseISO(parsed.RunAt)
	if err != nil {
		return false
	}
	now := s.clk.now()
	fireAt := iso.Time
	if !iso.Aware {
		fireAt = time.Date(fireAt.Year(), fireAt.Month(), fireAt.Day(), fireAt.Hour(), fireAt.Minute(), fireAt.Second(), fireAt.Nanosecond(), now.Location())
	}
	delta := fireAt.Sub(now)
	return delta >= 0 && delta <= threadHorizonMinutes*time.Minute
}

// Origin stamp from the creating session or nil when it cannot receive a fire time report
func (s *Store) originFromEnv(env *tools.Env, schedule any) map[string]any {
	platform := tools.SessionEnvValue(env, "ICEBATH_SESSION_PLATFORM")
	chatID := tools.SessionEnvValue(env, "ICEBATH_SESSION_CHAT_ID")
	if platform == "" || chatID == "" {
		return nil
	}
	if !tools.SessionAsyncDeliverySupported(env) {
		return nil
	}
	orNil := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	threadID := tools.SessionEnvValue(env, "ICEBATH_SESSION_THREAD_ID")
	if threadID != "" && platform == "slack" {
		messageID := tools.SessionEnvValue(env, "ICEBATH_SESSION_MESSAGE_ID")
		if messageID != "" && threadID == messageID {
			if s.firstFireWithinThreadHorizon(schedule) {
				s.log.Debug(fmt.Sprintf("Cron origin: keeping synthetic Slack thread_id=%s — first fire is within the conversation horizon", threadID))
			} else {
				s.log.Debug(fmt.Sprintf("Cron origin: dropping synthetic per-message Slack thread_id=%s (== creation message id)", threadID))
				threadID = ""
			}
		}
	}
	if threadID != "" {
		s.log.Debug(fmt.Sprintf("Cron origin captured thread_id=%s for %s:%s", threadID, platform, chatID))
	}
	return map[string]any{
		"platform": platform, "chat_id": chatID,
		"chat_name": orNil(tools.SessionEnvValue(env, "ICEBATH_SESSION_CHAT_NAME")), "thread_id": orNil(threadID),
		"user_id":  orNil(tools.SessionEnvValue(env, "ICEBATH_SESSION_USER_ID")),
		"scope_id": orNil(tools.SessionEnvValue(env, "ICEBATH_SESSION_SCOPE_ID")),
	}
}

// Create time notice for a job whose output never reaches the creating session, nil when it does
func (s *Store) localDeliveryNotice(ctx context.Context, env *tools.Env, job *Job, userDeliver *string) *string {
	if userDeliver != nil && strings.ToLower(strings.TrimSpace(*userDeliver)) == "local" {
		return nil
	}
	if targets := resolveDeliveryTargets(ctx, s, job, false); len(targets) > 0 {
		var fallback []Target
		for _, t := range targets {
			if t.ResolvedFrom == "origin_fallback" {
				fallback = append(fallback, t)
			}
		}
		if len(fallback) > 0 && tools.SessionEnvValue(env, "ICEBATH_SESSION_PLATFORM") != "" && !tools.SessionAsyncDeliverySupported(env) {
			msg := "Note: this stateless HTTP API session cannot receive cron delivery, so this " +
				fmt.Sprintf("job will report to the home channel %s:%s instead of back here.", fallback[0].Platform, fallback[0].ChatID)
			return &msg
		}
		return nil
	}
	msg := "This is a local-only cron job: its output is saved (view it with " +
		"cronjob(action='list')) but will NOT be delivered back into this " +
		"session — CLI/TUI and stateless HTTP API sessions have no live-delivery channel. To be " +
		"notified when it runs, recreate or update the job with deliver set to " +
		"a gateway-connected platform, e.g. deliver='telegram' or deliver='all'."
	return &msg
}

// Mode and delivery guidance echoed once in the create and update responses
func modeGuidanceNotes(job *Job, userDeliver *string) []string {
	var notes []string
	if nonEmpty(job.MonitorScript) != nil || nonEmpty(job.MonitorURL) != nil {
		notes = append(notes, "Monitor mode: the source runs first each tick and its output is "+
			"hashed as exact bytes — unchanged output suppresses the agent run "+
			"(silent no_change tick), changed output injects a MONITOR CHANGE "+
			"DETECTED diff into the prompt. The first tick always runs as "+
			"baseline. The source must emit STABLE output (no timestamps, no "+
			"random ordering) or every tick will look changed.")
	}
	if job.NoAgent {
		notes = append(notes, "no_agent mode: stdout is delivered verbatim; EMPTY stdout sends "+
			"nothing at all (watchdog pattern — script should stay quiet when "+
			"there is nothing to report). Non-zero exit or timeout sends an "+
			"error alert. prompt/skills are ignored.")
	}
	deliver := ""
	if userDeliver != nil {
		deliver = strings.ToLower(strings.TrimSpace(*userDeliver))
	}
	if deliver == "" {
		return notes
	}
	for _, p := range strings.Split(deliver, ",") {
		if p == "all" {
			notes = append(notes, "deliver='all' resolves at fire time and never includes "+
				"bot-chat targets — channels connected later are picked up "+
				"automatically.")
			break
		}
	}
	if strings.HasPrefix(deliver, "bot-chat:") {
		notes = append(notes, "Targeting another profile's Bot Chat costs that bot an agent turn per run.")
	}
	for _, target := range strings.Split(deliver, ",") {
		parts := strings.Split(strings.TrimSpace(target), ":")
		if len(parts) == 2 && parts[0] != "bot-chat" && parts[0] != "sms" && parts[1] != "" && !strings.HasPrefix(parts[1], "#") {
			notes = append(notes, fmt.Sprintf("deliver target '%s' has no :thread_id "+
				"segment — on thread/topic platforms the delivery lands in "+
				"the main chat, not a topic.", strings.TrimSpace(target)))
			break
		}
	}
	return notes
}

// Stored monitor script and URL pair for the model facing monitor field, nil leaves both unchanged
func splitMonitorArg(monitor, monitorScript, monitorURL any) (any, any) {
	if monitor == nil {
		return monitorScript, monitorURL
	}
	value := strings.TrimSpace(pyrepr.Str(monitor))
	if value == "" {
		return "", ""
	}
	low := strings.ToLower(value)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") {
		return "", value
	}
	return value, ""
}

// Human readable repeat budget of a job
func repeatDisplay(job *Job) string {
	if job.Repeat == nil || job.Repeat.Times == nil {
		return "forever"
	}
	times, completed := *job.Repeat.Times, job.Repeat.Completed
	if times == 1 {
		if completed == 0 {
			return "once"
		}
		return "1/1"
	}
	if completed != 0 {
		return fmt.Sprintf("%d/%d", completed, times)
	}
	return fmt.Sprintf("%d times", times)
}

// Stripped non empty string forms of a string or list in order
func cleanStrList(items any) []string {
	var raw []any
	switch v := items.(type) {
	case nil:
		return []string{}
	case string:
		raw = []any{v}
	case []string:
		for _, x := range v {
			raw = append(raw, x)
		}
	case []any:
		raw = v
	default:
		raw = []any{v}
	}
	out := []string{}
	for _, i := range raw {
		if s := strings.TrimSpace(pyrepr.Str(i)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Ordered unique skill names from the legacy single skill and the skills list
func canonicalSkills(skill, skills any) []string {
	var items []any
	switch v := skills.(type) {
	case nil:
		if pyrepr.Truthy(skill) {
			items = []any{skill}
		}
	case string:
		items = []any{v}
	case []string:
		for _, x := range v {
			items = append(items, x)
		}
	case []any:
		items = v
	default:
		items = []any{v}
	}
	cleaned := make([]any, 0, len(items))
	for _, item := range items {
		if !pyrepr.Truthy(item) {
			item = ""
		}
		cleaned = append(cleaned, item)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, s := range cleanStrList(cleaned) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Stripped string form of an optional value, nil for nil or empty
func normalizeOptionalJobValue(value any, stripTrailingSlash bool) *string {
	if value == nil {
		return nil
	}
	if p, ok := value.(*string); ok {
		if p == nil {
			return nil
		}
		value = *p
	}
	text := strings.TrimSpace(pyrepr.Str(value))
	if stripTrailingSlash {
		text = strings.TrimRight(text, "/")
	}
	if text == "" {
		return nil
	}
	return &text
}

// Canonical comma joined deliver string, nil for nil or empty
func normalizeDeliverParam(value any) *string {
	switch v := value.(type) {
	case nil:
		return nil
	case []any, []string:
		joined := strings.Join(cleanStrList(v), ",")
		if joined == "" {
			return nil
		}
		return &joined
	}
	text := strings.TrimSpace(pyrepr.Str(value))
	if text == "" {
		return nil
	}
	return &text
}

// Error text when a bot chat deliver element names a profile missing on this machine
func validateBotChatDeliver(deliver *string) string {
	if deliver == nil || *deliver == "" {
		return ""
	}
	for _, part := range strings.Split(*deliver, ",") {
		profileArg, _ := parseBotChatDeliverToken(strings.TrimSpace(part))
		if profileArg == "" {
			continue
		}
		canon, err := profiles.NormalizeProfileName(profileArg)
		if err != nil {
			return fmt.Sprintf("invalid bot-chat profile name '%s'", profileArg)
		}
		if !profiles.ProfileExists(canon) {
			return fmt.Sprintf("bot-chat delivery profile '%s' not found on this "+
				"gateway's machine. Bot Chat delivery is machine-local — use a "+
				"profile that exists here (icebath profile list), or omit the "+
				"name (deliver='bot-chat') for the job's own profile.", profileArg)
		}
	}
	return ""
}

// Concrete creator target for origin elements of a deliver made from inside a cron run
func resolveCronContextDeliver(env *tools.Env, deliver *string) *string {
	if !truthyText(tools.SessionEnvValue(env, "ICEBATH_CRON_SESSION")) {
		return deliver
	}
	creator := func() string {
		platform := strings.TrimSpace(tools.SessionEnvValue(env, "ICEBATH_CRON_AUTO_DELIVER_PLATFORM"))
		chatID := strings.TrimSpace(tools.SessionEnvValue(env, "ICEBATH_CRON_AUTO_DELIVER_CHAT_ID"))
		if platform == "" || chatID == "" {
			return "local"
		}
		if threadID := strings.TrimSpace(tools.SessionEnvValue(env, "ICEBATH_CRON_AUTO_DELIVER_THREAD_ID")); threadID != "" {
			return platform + ":" + chatID + ":" + threadID
		}
		return platform + ":" + chatID
	}
	if deliver == nil {
		t := creator()
		return &t
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range cleanStrList(strings.Split(*deliver, ",")) {
		if strings.ToLower(p) == "origin" {
			p = creator()
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	joined := strings.Join(out, ",")
	if joined == "" {
		return nil
	}
	return &joined
}

// Truthy string test of utils is_truthy_value
func truthyText(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Scheme, host and effective port equality with a configured endpoint
func sameOrigin(baseURL, configured string) bool {
	ws, wh, wp, err := pyurl.BaseURLOrigin(configured)
	if err != nil {
		return false
	}
	gs, gh, gp, err := pyurl.BaseURLOrigin(baseURL)
	if err != nil {
		return false
	}
	return wh != "" && gs == ws && gh == wh && gp == wp
}

// Refusal wording for a base_url override naming the violated endpoint rule
func baseURLRefused(bu, prov, why string) string {
	return fmt.Sprintf("base_url %s is not allowed for provider %s. %s", pyrepr.Repr(bu), pyrepr.Repr(prov), why)
}

// Refusal when a bare custom base_url would receive a host gated stored key off every configured origin
func (s *Store) customStoredKeyError(ctx context.Context, bu string) (string, error) {
	customWhy := "A stored API key matches its hostname, and a stored credential may only be sent to a " +
		"configured endpoint (same scheme, host and port); configure the endpoint as a custom " +
		"provider to use it."
	if _, _, _, err := pyurl.BaseURLOrigin(bu); err != nil {
		return baseURLRefused(bu, "custom", "It is not a valid URL."), nil
	}
	usable := false
	for _, key := range cronProviderResolver.hostGated(ctx, s.home, bu, true) {
		if provider.HasUsableSecret(key, 4) {
			usable = true
			break
		}
	}
	if !usable {
		return "", nil
	}
	cfg, err := s.loadConfig()
	if err != nil {
		return "", err
	}
	var configured []string
	if m, ok := cfg["model"].(map[string]any); ok {
		if u, ok := m["base_url"]; ok && pyrepr.Truthy(u) {
			configured = append(configured, pyrepr.Str(u))
		}
	}
	configured = append(configured, provider.GetSecretStr(ctx, "OPENAI_BASE_URL", ""), provider.OpenRouterBaseURL)
	configured = append(configured, cronProviderResolver.registryURLs()...)
	for _, entry := range provider.CompatibleCustomProviders(map[string]any(cfg)) {
		if u, ok := entry["base_url"]; ok && pyrepr.Truthy(u) {
			configured = append(configured, pyrepr.Str(u))
		}
	}
	for _, u := range configured {
		if u != "" && sameOrigin(bu, u) {
			return "", nil
		}
	}
	return baseURLRefused(bu, "custom", customWhy), nil
}

// Refusal when a provider and base_url pair could send a stored credential off its own origin
func (s *Store) validateCronBaseURL(ctx context.Context, prov, baseURL any) (string, error) {
	bu := normalizeOptionalJobValue(baseURL, true)
	if bu == nil {
		return "", nil
	}
	p := normalizeOptionalJobValue(prov, false)
	if p == nil {
		return "base_url override requires an explicit provider. Set provider to a " +
			"configured custom provider to use a custom endpoint.", nil
	}
	if strings.ToLower(*p) == "custom" {
		return s.customStoredKeyError(ctx, *bu)
	}
	if cronProviderResolver.hasNamed(ctx, s.home, *p) {
		cp, err := cronProviderResolver.named(ctx, s.home, *p)
		if err != nil {
			cp = nil
		}
		cfgURL := ""
		if u, ok := cp["base_url"]; ok && pyrepr.Truthy(u) {
			cfgURL = pyrepr.Str(u)
		}
		if cfgURL != "" && sameOrigin(*bu, cfgURL) {
			return "", nil
		}
		shown := cfgURL
		if shown == "" {
			shown = "unknown"
		}
		return baseURLRefused(*bu, *p, "A named custom provider's stored credential may only be sent to its own "+
			fmt.Sprintf("configured endpoint (%s): same scheme, host and port.", shown)), nil
	}
	resolved, err := cronProviderResolver.resolveRequested(ctx, s.home, *p)
	if err != nil {
		resolved = *p
	}
	if knownURL, ok := cronProviderResolver.registryURL(resolved); ok && knownURL != "" && sameOrigin(*bu, knownURL) {
		return "", nil
	}
	return baseURLRefused(*bu, *p, "A named provider's stored credential may only be sent to its own endpoint "+
		`(same scheme, host and port); use a configured custom provider (provider="custom") `+
		"for a custom base_url."), nil
}

// Refusal when a script path is absolute, escapes the scripts dir, or names no file
func (s *Store) validateCronScriptPath(script string) string {
	raw := strings.TrimSpace(script)
	if raw == "" {
		return ""
	}
	scriptsDir := filepath.Join(s.home, "scripts")
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "~") || (len(raw) >= 2 && raw[1] == ':') {
		return fmt.Sprintf("Script path must be relative to %s/. ", scriptsDir) +
			fmt.Sprintf("Got absolute or home-relative path: %s. ", pyrepr.Repr(raw)) +
			fmt.Sprintf("Place scripts in %s/ and use just the filename.", scriptsDir)
	}
	_ = os.MkdirAll(scriptsDir, 0o777)
	resolved := filepath.Join(scriptsDir, raw)
	if tools.ValidateWithinDir(resolved, scriptsDir) != "" {
		return fmt.Sprintf("Script path escapes the scripts directory via traversal: %s", pyrepr.Repr(raw))
	}
	if st, err := os.Stat(resolved); err != nil || !st.Mode().IsRegular() {
		return fmt.Sprintf("Script file not found: %s. ", resolved) + fmt.Sprintf("Create it in %s/ first.", scriptsDir)
	}
	return ""
}

// Context refs with self added for continuity true or removed for false, nil when empty
func applyContinuity(contextFrom any, continuity bool) []string {
	refs := cleanStrList(contextFrom)
	hasSelf := false
	for _, r := range refs {
		if strings.ToLower(r) == "self" {
			hasSelf = true
		}
	}
	if continuity && !hasSelf {
		refs = append(refs, "self")
	} else if !continuity && hasSelf {
		kept := []string{}
		for _, r := range refs {
			if strings.ToLower(r) != "self" {
				kept = append(kept, r)
			}
		}
		refs = kept
	}
	if len(refs) == 0 {
		return nil
	}
	return refs
}

// Error text when a non self context ref names a missing job
func (s *Store) validateContextFromRefs(refs []any) (string, error) {
	for _, ref := range refs {
		if str, ok := ref.(string); ok && strings.ToLower(strings.TrimSpace(str)) == "self" {
			continue
		}
		job, err := s.getJob(pyrepr.Str(ref))
		if err != nil {
			return "", err
		}
		if job == nil {
			return fmt.Sprintf("context_from job '%s' not found. ", pyrepr.Str(ref)) +
				"Use cronjob(action='list') to see available jobs.", nil
		}
	}
	return "", nil
}

// Model facing view of one job with secrets redacted from the last error
func (s *Store) formatJob(job *Job) *pyjson.Object {
	skills := canonicalSkills(strPtrAny(job.Skill), stringsAny(job.Skills))
	jobID := job.ID
	if jobID == "" {
		jobID = "unknown"
	}
	name := job.Name
	if name == "" {
		name = headRunes(job.Prompt, 50)
	}
	if name == "" && len(skills) > 0 {
		name = skills[0]
	}
	if name == "" {
		name = jobID
	}
	preview := job.Prompt
	if len([]rune(preview)) > 100 {
		preview = headRunes(preview, 100) + "..."
	}
	var skill any
	if len(skills) > 0 {
		skill = skills[0]
	}
	schedule := job.ScheduleDisplay
	if schedule == "" {
		schedule = "?"
	}
	var deliver any = "local"
	if job.Deliver != nil {
		deliver = *job.Deliver
	} else if jobHasKey(job, "deliver") {
		deliver = nil
	}
	var lastError any
	if nonEmpty(job.LastError) != nil {
		lastError = tools.RedactSensitiveTextWith(&tools.Env{Home: s.home}, *job.LastError, tools.RedactOptions{Force: true, URLCredentials: true})
	} else if job.LastError != nil {
		lastError = *job.LastError
	}
	out := pyjson.NewObject()
	out.Set("job_id", jobID)
	out.Set("name", name)
	out.Set("skill", skill)
	out.Set("skills", toAnyList(skills))
	out.Set("prompt_preview", preview)
	out.Set("model", strPtrAny(job.Model))
	out.Set("provider", strPtrAny(job.Provider))
	out.Set("pinned", job.Model != nil && strings.TrimSpace(*job.Model) != "")
	out.Set("base_url", strPtrAny(job.BaseURL))
	out.Set("schedule", schedule)
	out.Set("repeat", repeatDisplay(job))
	out.Set("deliver", deliver)
	out.Set("next_run_at", strPtrAny(job.NextRunAt))
	out.Set("last_run_at", strPtrAny(job.LastRunAt))
	out.Set("last_status", strPtrAny(job.LastStatus))
	out.Set("last_delivery_error", strPtrAny(job.LastDeliveryError))
	out.Set("last_delivery_unverified", jsonValue(job.LastDeliveryUnverified))
	out.Set("last_fire_error", jsonValue(job.LastFireError))
	out.Set("last_error", lastError)
	out.Set("enabled", job.Enabled)
	out.Set("state", effectiveJobState(job))
	out.Set("paused_at", strPtrAny(job.PausedAt))
	out.Set("paused_reason", strPtrAny(job.PausedReason))
	optional := []struct {
		key string
		v   any
	}{
		{"script", strPtrAny(job.Script)}, {"reasoning_effort", strPtrAny(job.ReasoningEffort)},
		{"monitor_script", strPtrAny(job.MonitorScript)}, {"monitor_url", strPtrAny(job.MonitorURL)},
		{"monitor_state", jsonValue(job.MonitorState)}, {"no_agent", job.NoAgent},
		{"enabled_toolsets", jsonValue(job.EnabledToolsets)}, {"workdir", strPtrAny(job.Workdir)},
		{"interpreter", strPtrAny(job.Interpreter)},
	}
	for _, o := range optional {
		if pyrepr.Truthy(o.v) {
			if o.key == "no_agent" {
				out.Set(o.key, true)
			} else {
				out.Set(o.key, o.v)
			}
		}
	}
	isSelf := func(r string) bool { return strings.ToLower(strings.TrimSpace(r)) == "self" || r == job.ID }
	var external []any
	continuity := false
	for _, r := range job.ContextFrom {
		if isSelf(r) {
			continuity = true
		} else {
			external = append(external, r)
		}
	}
	if continuity {
		out.Set("continuity", true)
	}
	if len(external) > 0 {
		out.Set("context_from", external)
	}
	if job.AttachToSession != nil {
		out.Set("attach_to_session", *job.AttachToSession)
	}
	return out
}

// Whether the stored record carried the key, so an explicit null differs from absent
func jobHasKey(job *Job, key string) bool {
	for _, k := range job.keyOrder {
		if k == key {
			return true
		}
	}
	return false
}

// Nil or the pointed string
func strPtrAny(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// Nil or the list as any values
func stringsAny(v []string) any {
	if v == nil {
		return nil
	}
	return toAnyList(v)
}

// String list as a list of any values
func toAnyList(v []string) []any {
	out := make([]any, len(v))
	for i, s := range v {
		out[i] = s
	}
	return out
}

// JSON value with Python key order of a typed field, nil when the field is nil
func jsonValue(v any) any {
	data, err := json.Marshal(v)
	if err != nil || string(data) == "null" {
		return nil
	}
	out, err := pyjson.LoadsBytes(data)
	if err != nil {
		return nil
	}
	return out
}

// Gateway running payload so the tool and the CLI agree on whether the scheduler is active
func (s *Store) gatewayLivenessNotice(ctx context.Context, plural bool) *pyjson.Object {
	out := pyjson.NewObject()
	gw, known := s.builtinGatewayLiveness(ctx)
	if !known {
		out.Set("gateway_running", nil)
		return out
	}
	if !gw {
		subject := "this job is saved"
		if plural {
			subject = "these jobs are saved"
		}
		out.Set("gateway_running", false)
		out.Set("warning", fmt.Sprintf("The Icebath gateway is not running — %s ", subject)+
			"but will NOT fire until the gateway is started "+
			"(icebath gateway install / icebath gateway start). "+
			"Tell the user the task is scheduled but not active yet.")
		return out
	}
	out.Set("gateway_running", true)
	return out
}
