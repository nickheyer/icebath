package tuiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Monotonic desktop and backend contract version the desktop compares against its required one
const desktopBackendContract = 8

// Stand in terminal cwd values resolved to the home dir at runtime, never an explicit workspace
var cwdPlaceholders = map[string]bool{".": true, "auto": true, "cwd": true}

// CLI banner facts the session info reports, assigned by the command wiring from the app package
var Banner struct {
	ReleaseDate     string
	AvailableSkills func(ctx context.Context) *pyjson.Object
	UpdateResult    func(timeout time.Duration) *int64
}

// Launch home captured at start, the launch state db handle and the homes served besides the launch one
var (
	hermesHome         = home.Process()
	hermesHomeAtImport = hermesHome
	dbMu               sync.Mutex
	sharedDB           *state.DB
	dbError            string
	servedProfileMu    sync.Mutex
	servedProfileHomes = map[string]bool{}
)

// Logger every server module writes through
func logger() *slog.Logger { return slog.Default().With("logger", "tui_gateway.server") }

// Launch profile home the gateway serves at call time
func LaunchHome() string { return launchHome() }

// Named profile home on this host, empty for the launch profile, an error naming an unknown profile
func ProfileHome(profile string) (string, error) { return profileHome(profile) }

// Launch profile home at call time, the swapped copy when a harness changed it else the live process home
func launchHome() string {
	if hermesHome != hermesHomeAtImport {
		return hermesHome
	}
	return home.Process()
}

// state.db of the launch profile
func launchStateDBPath() string { return filepath.Join(launchHome(), "state.db") }

// Shared launch session store opened on first use, nil with the error recorded when it cannot open
func getDB(ctx context.Context) *state.DB {
	dbMu.Lock()
	defer dbMu.Unlock()
	if sharedDB == nil {
		db, err := state.Acquire(ctx, launchStateDBPath())
		if err != nil {
			dbError = err.Error()
			logger().Warn(fmt.Sprintf("TUI session store unavailable — continuing without state.db features: %s", err))
			return nil
		}
		sharedDB, dbError = db, ""
	}
	return sharedDB
}

// Effective user config of the active profile without the defaults merge
func loadCfg(ctx context.Context) map[string]any { return tuigateway.LoadCfg(ctx) }

// display section of the behavioral config, empty when absent or malformed
func displayCfg(ctx context.Context) map[string]any {
	display, _ := loadCfg(ctx)["display"].(map[string]any)
	if display == nil {
		return map[string]any{}
	}
	return display
}

// Explicit profile param naming no live profile on this host, answered as JSON RPC 4064
type ProfileUnavailableError struct{ Msg string }

func (e *ProfileUnavailableError) Error() string { return e.Msg }

// Python exception class name the error stands for
func (e *ProfileUnavailableError) PyTypeName() string { return "ProfileUnavailableError" }

// Older payload basenames of the default home mapped to default unless a real profile carries that name
func canonicalProfileRequest(name string) string {
	folded := strings.ToLower(name)
	if folded == ".icebath" || folded == "icebath" {
		normalized, err := profiles.NormalizeProfileName(name)
		if err != nil || !isDir(filepath.Join(profiles.ProfilesRoot(), normalized)) {
			return "default"
		}
	}
	return name
}

// Named profile home on this host, empty for the launch profile, a ProfileUnavailableError for an unknown one
func profileHome(profile string) (string, error) {
	name := canonicalProfileRequest(strings.TrimSpace(profile))
	if name == "" {
		return "", nil
	}
	dir, err := profiles.GetProfileDir(name)
	if err != nil || dir == "" || !isDir(dir) {
		return "", &ProfileUnavailableError{fmt.Sprintf("Profile '%s' does not exist.", name)}
	}
	if resolveLax(dir) == resolveLax(hermesHome) {
		return "", nil
	}
	servedProfileMu.Lock()
	defer servedProfileMu.Unlock()
	if !servedProfileHomes[dir] {
		provider.ActivateMultiProfileHosting()
	}
	servedProfileHomes[dir] = true
	return dir, nil
}

// Profile name for session payloads, the requested real non launch profile else the launch one
func responseProfileName(ctx context.Context, profile string) string {
	name := canonicalProfileRequest(strings.TrimSpace(profile))
	if name == "" {
		return currentProfileName(ctx)
	}
	h, err := profileHome(name)
	if err != nil || h == "" {
		return currentProfileName(ctx)
	}
	return name
}

// Absolute existing terminal cwd of a config mapping, empty for stand in, missing or invalid paths
func configuredCwdFromCfg(cfg map[string]any) string {
	terminalCfg, _ := cfg["terminal"].(map[string]any)
	raw := ""
	if terminalCfg != nil {
		raw = strings.TrimSpace(strOr(terminalCfg["cwd"]))
	}
	if raw == "" || cwdPlaceholders[raw] {
		return ""
	}
	resolved, err := pypath.Abspath(pypath.Expanduser(raw))
	if err != nil || !isDir(resolved) {
		return ""
	}
	return resolved
}

// Terminal cwd from the config.yaml of a profile home, empty when unset, invalid or unreadable
func profileConfiguredCwd(ctx context.Context, profileHome string) string {
	if profileHome == "" {
		return ""
	}
	p := filepath.Join(profileHome, "config.yaml")
	exists, err := config.ConfigExists(ctx, p)
	if err != nil {
		var unavailable *config.ConfigBackendUnavailable
		if errors.As(err, &unavailable) {
			logger().Warn(fmt.Sprintf("terminal.cwd of %s unavailable: %s", profileHome, unavailable.SystemExitCode()))
		}
		return ""
	}
	if !exists {
		return ""
	}
	cfg, err := config.LoadUserEffective(p, config.EnvRefLookupContext(ctx))
	if err != nil {
		var unavailable *config.ConfigBackendUnavailable
		if errors.As(err, &unavailable) {
			logger().Warn(fmt.Sprintf("terminal.cwd of %s unavailable: %s", profileHome, unavailable.SystemExitCode()))
		}
		return ""
	}
	return configuredCwdFromCfg(cfg)
}

// Launch profile terminal cwd read by path, never through the active home override
func launchConfiguredCwd(ctx context.Context) string {
	return profileConfiguredCwd(ctx, hermesHome)
}

// Fallback cwd with no explicit, stored or profile cwd, the configured terminal cwd else the process cwd
func defaultSessionCwd(ctx context.Context) string {
	if cwd := launchConfiguredCwd(ctx); cwd != "" {
		return cwd
	}
	if cwd := os.Getenv("TERMINAL_CWD"); cwd != "" {
		return cwd
	}
	cwd, _ := os.Getwd()
	return cwd
}

// Active profile name, default when none is active or the lookup fails
func currentProfileName(ctx context.Context) (name string) {
	defer func() {
		if recover() != nil {
			name = "default"
		}
	}()
	if active := profiles.GetActiveProfileName(ctx); active != "" {
		return active
	}
	return "default"
}

// Token counters, context occupancy, cache hit share, rolling latency and plan gauge of a live agent
func getUsage(ctx context.Context, a *agent.AIAgent) map[string]any {
	or := func(v, fallback int) int {
		if v != 0 {
			return v
		}
		return fallback
	}
	usage := map[string]any{
		"model":     a.Model(),
		"input":     or(a.SessionInputTokens(), a.SessionPromptTokens()),
		"output":    or(a.SessionOutputTokens(), a.SessionCompletionTokens()),
		"reasoning": a.SessionReasoningTokens(), "prompt": a.SessionPromptTokens(),
		"completion": a.SessionCompletionTokens(), "total": a.SessionTotalTokens(),
		"calls": a.SessionAPICalls(),
	}
	if comp := a.ContextCompressor(); comp != nil {
		if fields, ok := agent.ContextUsageFieldsOf(comp); ok {
			usage["context_used"], usage["context_max"] = fields.ContextUsed, fields.ContextMax
			usage["context_percent"], usage["context_source"] = fields.ContextPercent, fields.ContextSource
			usage["context_estimated"] = fields.ContextEstimated
		}
		usage["compressions"] = comp.Base().CompressionCount
	}
	promptTotal, cacheRead := a.SessionPromptTokens(), a.SessionCacheReadTokens()
	if promptTotal > 0 && cacheRead > 0 {
		usage["cache_hit_pct"] = max(0, min(100, int(math.RoundToEven(float64(cacheRead)/float64(promptTotal)*100))))
	}
	latencies, outputs := a.APILatencyHistory(), a.APIOutputHistory()
	if n := min(len(latencies), len(outputs)); n > 0 {
		totalLatency, totalOutput := 0.0, 0
		for _, v := range latencies[len(latencies)-n:] {
			totalLatency += v
		}
		for _, v := range outputs[len(outputs)-n:] {
			totalOutput += v
		}
		readouts := map[string]float64{"avg_latency_s": totalLatency / float64(n)}
		if totalLatency > 0 {
			readouts["avg_tps"] = float64(totalOutput) / totalLatency
		}
		for key, value := range readouts {
			if !math.IsNaN(value) && value > 0 && value < 1e6 {
				usage[key] = pyint.Round(value, 1)
			}
		}
	}
	if capture := a.CreditsCapture(); capture != nil {
		if gauge := gateway.PlanGauge(capture.Credits()); gauge != nil {
			usage["plan_gauge"] = gauge
		}
	}
	usage["active_subagents"] = tools.AsyncDelegationActiveCount()
	if provider.IsTruthyValue(os.Getenv("ICEBATH_DEV_CREDITS")) {
		if capture := a.CreditsCapture(); capture != nil {
			if spent, ok := capture.CreditsSpentMicros(); ok {
				usage["dev_credits_spent_micros"] = spent
			}
		}
	}
	return usage
}

// Missing key warning for the agent provider, empty when a key is configured
func probeCredentials(a *agent.AIAgent) string {
	if a.APIKey() == "" {
		return fmt.Sprintf("No API key configured for provider '%s'. First message will fail.", agent.SessionRuntimeView(a).Provider())
	}
	return ""
}

// Usage of the live agent, else the compute host mirror copy, else empty
func sessionUsageSnapshot(ctx context.Context, session *tuigateway.Session) map[string]any {
	mirrorUsage, isDict := metadataMirror(session)["usage"].(map[string]any)
	if a := sessionAgent(session); a != nil && !(truthy(session.Get("_compute_host_active")) && isDict) {
		return getUsage(ctx, a)
	}
	out := map[string]any{}
	for key, value := range mirrorUsage {
		out[key] = value
	}
	return out
}

// Named project owning the cwd in the profile projects db, nil when none or on failure
func projectInfoForCwd(ctx context.Context, cwd string) any {
	if strings.TrimSpace(cwd) == "" {
		return nil
	}
	db, err := tools.OpenProjectsDB(ctx, &tools.Env{Home: home.Dir(ctx)})
	if err != nil {
		logger().Debug("failed to resolve project for cwd", "error", err)
		return nil
	}
	defer func() { _ = db.Close() }()
	project, err := db.ProjectForPath(cwd, false)
	if err != nil {
		logger().Debug("failed to resolve project for cwd", "error", err)
		return nil
	}
	if project == nil {
		return nil
	}
	var primary any
	if project.PrimaryPath != nil {
		primary = *project.PrimaryPath
	}
	return map[string]any{"id": project.ID, "slug": project.Slug, "name": project.Name, "primary_path": primary}
}

// Epoch seconds the running turn started, nil when idle
func turnStartedAt(session *tuigateway.Session) any {
	inflight, _ := session.Get("inflight_turn").(map[string]any)
	if inflight == nil || !truthy(inflight["started_at"]) {
		return nil
	}
	started, err := pyint.Float(inflight["started_at"])
	if err != nil {
		return nil
	}
	return started
}

// Whether a priority tier reaches the session route, the base URL known only while no switch is pending
func fastTierApplies(view agent.SessionRuntime, model, providerName string, routeKnown bool, tier string) (applies bool) {
	defer func() {
		if recover() != nil {
			applies = false
		}
	}()
	baseURL := ""
	if routeKnown && view != nil {
		if view.APIMode() == "anthropic_messages" {
			baseURL = view.AnthropicBaseURL()
		}
		if baseURL == "" {
			baseURL = view.BaseURL()
		}
	}
	return provider.ResolveFastModeOverrides(model, providerName, baseURL, tier) != nil
}

// Session info payload of the status bar and desktop, model route, approvals, workspace, usage and inventory
func sessionInfo(ctx context.Context, a *agent.AIAgent, session *tuigateway.Session) map[string]any {
	if session == nil {
		for _, live := range tuigateway.LiveSessions() {
			if sessionAgent(live.Session) == a {
				session = live.Session
				break
			}
		}
	}
	view := agent.SessionRuntimeView(a)
	mirror := metadataMirror(session)
	cwd := displaySessionCwd(ctx, session)
	sessionKey := strOr(session.Get("session_key"))
	if sessionKey == "" && a != nil {
		sessionKey = a.SessionID()
	}
	defaultPersonality := pyrepr.Or(displayCfg(ctx)["personality"], "")
	personality, present := session.Lookup("personality")
	if !present {
		personality = defaultPersonality
	}
	reasoningEffort := ""
	if view != nil {
		if rc := view.ReasoningConfig(); rc != nil {
			if enabled, ok := rc["enabled"].(bool); ok && !enabled {
				reasoningEffort = "none"
			} else {
				reasoningEffort = strOr(rc["effort"])
			}
		}
	}
	var serviceTier any = ""
	if a != nil && a.ServiceTier() != "" {
		serviceTier = a.ServiceTier()
	} else if truthy(mirror["service_tier"]) {
		serviceTier = mirror["service_tier"]
	}
	yolo, approvalMode := func() (yolo bool, mode string) {
		defer func() {
			if recover() != nil {
				yolo, mode = false, "manual"
			}
		}()
		sessionYolo := sessionKey != "" && tools.IsSessionYoloEnabled(sessionKey)
		mode = loadApprovalMode(ctx)
		return tools.YoloModeFrozen() || sessionYolo || mode == "off", mode
	}()
	pendingSwitch, _ := session.Get("pending_model_switch").(map[string]any)
	pendingModel := strings.TrimSpace(strOr(pendingSwitch["display_model"]))
	pendingProvider := strings.TrimSpace(strOr(pendingSwitch["display_provider"]))
	providerValue, mirrorHasProvider := mirror["provider"]
	if !mirrorHasProvider {
		providerValue = ""
		if view != nil {
			providerValue = view.Provider()
		}
	}
	if providerValue == "custom" && !mirrorHasProvider && a != nil {
		providerValue = scopedSessionProvider(ctx, a, session)
	}
	var model any = pendingModel
	if pendingModel == "" {
		if mirrored, ok := mirror["model"]; ok {
			model = mirrored
		} else if view != nil {
			model = view.Model()
		} else {
			model = ""
		}
	}
	var reportedProvider any = pendingProvider
	if pendingProvider == "" {
		reportedProvider = providerValue
	}
	apiMode := ""
	if view != nil {
		apiMode = view.APIMode()
	}
	reasoningEffortWire := ""
	if reasoningEffort != "" && reasoningEffort != "none" {
		reasoningEffortWire = provider.ClampEffort(reasoningEffort, provider.RouteSupportedEfforts(strOr(reportedProvider), strOr(model), apiMode))
	}
	tierText := strOr(serviceTier)
	info := map[string]any{
		"model":                 model,
		"provider":              reportedProvider,
		"reasoning_effort":      reasoningEffort,
		"reasoning_effort_wire": reasoningEffortWire,
		"service_tier":          serviceTier,
		"fast":                  provider.StaticTiers[tierText] && fastTierApplies(view, strOr(model), strOr(reportedProvider), pendingProvider == "", tierText),
		"yolo":                  yolo,
		"approval_mode":         approvalMode,
		"tools":                 copyDict(mirror["tools"]),
		"skills":                copyDict(mirror["skills"]),
		"cwd":                   cwd,
		"branch":                tuigateway.Branch(cwd),
		"project":               projectInfoForCwd(ctx, cwd),
		"terminal_backend":      effectiveTerminalBackend(ctx),
		"personality":           strOr(personality),
		"running":               truthy(session.Get("running")),
		"turn_started_at":       turnStartedAt(session),
		"title":                 "",
		"stored_session_id":     sessionKey,
		"desktop_contract":      desktopBackendContract,
		"version":               "",
		"release_date":          "",
		"update_behind":         nil,
		"update_command":        "",
		"usage":                 sessionUsageSnapshot(ctx, session),
	}
	if sessionKey != "" {
		info["title"] = sessionLiveTitle(ctx, session, sessionKey)
	}
	if name := home.ProfileNameFor(strOr(session.Get("profile_home"))); name != "" {
		info["profile_name"] = name
	} else {
		info["profile_name"] = currentProfileName(ctx)
	}
	if versionInfo, err := updaterecord.GetVersionInfo(); err == nil {
		info["version"], info["release_date"] = versionInfo.BaseVersion, Banner.ReleaseDate
	}
	liveAgent := a != nil && !truthy(session.Get("_compute_host_active"))
	if liveAgent {
		suppress(func() {
			grouped := map[string]any{}
			info["tools"] = grouped
			scope := tools.ScopeKey(home.Dir(ctx))
			for _, def := range a.ToolDefs() {
				function, _ := def["function"].(map[string]any)
				name := function["name"].(string)
				toolset := tools.Default.ToolsetForTool(ctx, scope, name)
				if toolset == "" {
					toolset = "other"
				}
				names, _ := grouped[toolset].([]any)
				grouped[toolset] = append(names, name)
			}
		})
		suppress(func() { info["skills"] = Banner.AvailableSkills(ctx) })
	}
	info["mcp_servers"] = []any{}
	suppress(func() { info["mcp_servers"] = mcpStatusRows(ctx) })
	suppress(func() {
		if prompt, ok := mirror["system_prompt"]; ok {
			info["system_prompt"] = prompt
		} else if a != nil {
			info["system_prompt"] = a.CachedSystemPrompt()
		} else {
			info["system_prompt"] = ""
		}
	})
	suppress(func() {
		if behind := Banner.UpdateResult(500 * time.Millisecond); behind != nil {
			info["update_behind"] = *behind
		} else {
			info["update_behind"] = nil
		}
		command, err := config.RecommendedUpdateCommand(ctx, home.Dir(ctx))
		if err != nil {
			panic(err)
		}
		info["update_command"] = command
	})
	if liveAgent {
		if warning := probeCredentials(a); warning != "" {
			info["credential_warning"] = warning
		}
	}
	return info
}

// MCP server status rows as the JSON objects the client reads
func mcpStatusRows(ctx context.Context) []any {
	env := tools.ToolEnvFrom(ctx)
	if env == nil {
		env = &tools.Env{Home: home.Dir(ctx)}
	}
	raw, err := json.Marshal(mcp.Status(ctx, env, nil, true))
	if err != nil {
		panic(err)
	}
	rows := []any{}
	if err := json.Unmarshal(raw, &rows); err != nil {
		panic(err)
	}
	return rows
}

// Emits session info for a session holding an agent or a compute host mirror, swallowing any failure
func emitSessionInfoForSession(ctx context.Context, sid string, session *tuigateway.Session) {
	if sessionAgent(session) != nil || len(metadataMirror(session)) > 0 {
		suppress(func() { tuigateway.Emit(ctx, "session.info", sid, sessionInfo(ctx, sessionAgent(session), session)) })
	}
}

// Re-emits session info to every live session of this process, for approvals writers outside config.set
func BroadcastSessionInfo(ctx context.Context) {
	for _, live := range tuigateway.LiveSessions() {
		emitSessionInfoForSession(ctx, live.SID, live.Session)
	}
}

// Live agent a session record holds, nil before its build
func sessionAgent(session *tuigateway.Session) *agent.AIAgent {
	a, _ := session.Get("agent").(*agent.AIAgent)
	return a
}

// Runs a body and swallows any panic it raises
func suppress(body func()) {
	defer func() { _ = recover() }()
	body()
}

// Python truthiness of a slot value
func truthy(v any) bool { return pyrepr.Truthy(v) }

// str of a value with None and falsy values read as the empty string
func strOr(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// Shallow copy of a dict value, empty for anything else
func copyDict(v any) map[string]any {
	out := map[string]any{}
	if source, ok := v.(map[string]any); ok {
		for key, value := range source {
			out[key] = value
		}
	}
	return out
}

// Whether a path names an existing directory
func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Lenient pathlib resolve, the absolute path when a component cannot be resolved
func resolveLax(p string) string {
	if resolved, err := pypath.Resolve(p); err == nil {
		return resolved
	}
	abs, _ := filepath.Abs(p)
	return abs
}

// Session storage unavailable error with the user facing gloss, action and raw cause details
func dbUnavailableError(ctx context.Context, rid any, code int) map[string]any {
	dbMu.Lock()
	cause := dbError
	dbMu.Unlock()
	failure := state.DescribeStorageFailure(ctx, cause)
	return rpcErr(rid, code, fmt.Sprintf("Session storage is unavailable: %s. %s", failure.Gloss, failure.Action),
		map[string]any{"code": failure.Code, "cause": failure.Cause, "details": state.StorageFailureDetails(cause, 200)})
}

// Model and provider a live session runs, from its pending switch, mirror, agent or composer pick, else the profile default
func liveSessionIdentity(ctx context.Context, session *tuigateway.Session) (string, string) {
	pending, _ := session.Get("pending_model_switch").(map[string]any)
	mirror := metadataMirror(session)
	a := sessionAgent(session)
	view := agent.SessionRuntimeView(a)
	override, _ := session.Get("model_override").(map[string]any)
	model := strings.TrimSpace(strOr(pending["display_model"]))
	if model == "" {
		model = strOr(mirror["model"])
	}
	if model == "" && view != nil {
		model = view.Model()
	}
	if model == "" {
		model = strOr(override["model"])
	}
	agentProvider := ""
	if view != nil {
		agentProvider = view.Provider()
	}
	if agentProvider == "custom" {
		agentProvider = scopedSessionProvider(ctx, a, session)
	}
	providerName := strings.TrimSpace(strOr(pending["display_provider"]))
	if providerName == "" {
		providerName = strOr(mirror["provider"])
	}
	if providerName == "" {
		providerName = agentProvider
	}
	if providerName == "" {
		providerName = strOr(override["provider"])
	}
	defaultModel, defaultProvider := "", ""
	if model == "" {
		defaultModel, defaultProvider = sessionDefaultRoute(ctx, session)
	}
	if model == "" {
		model = defaultModel
	}
	if providerName == "" {
		providerName = defaultProvider
	}
	return model, providerName
}

// Busy input mode of the display config, interrupt unless queue or steer is configured
func loadBusyInputMode(ctx context.Context) string {
	raw := strings.ToLower(strings.TrimSpace(strOr(displayCfg(ctx)["busy_input_mode"])))
	if raw == "queue" || raw == "steer" || raw == "interrupt" {
		return raw
	}
	return "interrupt"
}

// Whether interim assistant commentary reaches the client, display interim_assistant_messages defaulting on
func loadInterimAssistantMessages(ctx context.Context) bool {
	raw, present := displayCfg(ctx)["interim_assistant_messages"]
	if !present {
		raw = true
	}
	return isTruthyValue(raw)
}

// Wraps a handler in the runtime scope of the params profile, else the named live session's, else the ticket's
func profileScoped(handler Handler) Handler {
	return func(ctx context.Context, rid any, params map[string]any) map[string]any {
		scopeHome := ""
		if strings.TrimSpace(strOr(params["profile"])) != "" {
			h, err := profileHome(strOr(params["profile"]))
			if err != nil {
				panic(err)
			}
			scopeHome = h
		} else if session := tuigateway.SessionRecord(strOr(params["session_id"])); session != nil {
			scopeHome = strOr(session.Get("profile_home"))
		} else {
			scopeHome = connectionProfileHome(ctx)
		}
		return handler(profileRuntimeScope(ctx, scopeHome, true), rid, params)
	}
}
