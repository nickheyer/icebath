package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Higher package collaborators the top level wiring installs so pkg/plugins never imports them
type Services struct {
	InvokeLifecycleHook               func(ctx context.Context, hookName string, kwargs map[string]any) []any
	RequestToolApproval               func(ctx context.Context, toolName, message, ruleKey, turnID, toolCallID, sessionID string) (map[string]any, error)
	FailoverReason                    func(value any) (any, bool)
	ClearDashboardAuthProviders       func()
	MemoryProviderDir                 func(h, name string) (string, bool)
	HostObject                        func(kind string, obj any) (name string, ok bool)
	BindProfileSecretScope            func(ctx context.Context, home string) context.Context
	GetSecret                         func(ctx context.Context, name string) (string, bool, error)
	PluginLLM                         PluginLLMClient
	SubagentLifecycle                 SubagentDelegation
	DelegationBackends                func(c *Context) any
	CurrentCronExecution              func(ctx context.Context) any
	CallMCPTool                       func(ctx context.Context, server, tool string, timeout float64, args map[string]any) (string, error)
	RedeemSideQuestion                func(ctx context.Context, token, prompt any, pluginID, h string, timeout any) (*string, error)
	BoundSessionKey                   func(ctx context.Context) string
	ResolveBuiltinCommand             func(name string) bool
	DispatchTool                      func(ctx context.Context, scope, toolName string, args, kwargs map[string]any) (string, error)
	RegisterContextReference          func(provider any) error
	DashboardAuth                     DashboardAuthRegistry
	Platforms                         PlatformRegistry
	ProviderRegistries                map[string]ScopedRegistry
	BuiltinAuxTaskKeys                func() []string
	RegisterRedactionPatterns         func(patterns any, source string) (int, error)
	PluginSDK                         func(ctx context.Context, home, op string, args []any, kwargs map[string]any) (any, error)
	BuildPluginBlueprint              func(pluginName, key string, spec BlueprintSpec) (string, any, error)
	SecretSources                     SecretSources
	ReRegisterOutboundWebhooks        func() error
	PlatformDeclarations              PlatformDeclarations
	OAuthSurface                      func(manifest map[string]any) map[string]bool
	OAuthAdmissionProblems            func(manifest map[string]any, pluginDir string) []string
	OAuthAdmissionWarnings            func(manifest map[string]any, pluginDir string) []string
	PortableServerAvailability        func(ctx context.Context, decl AgentPluginServerDeclaration) (PortableAvailability, error)
	MaskedSecretPrompt                func(prompt string) (string, error)
	ServerAvailabilityReason          func(ctx context.Context, declaration AgentPluginServerDeclaration, appTitle string) (reason string, offerable bool, err error)
	DefinesMemoryProvider             func(pluginDir string) bool
	PrepareMemoryProviderDependencies func(ctx context.Context, name string) error
	RequiresAuthNotice                func(manifest map[string]any) string
	DeclaresModelProviderKind         func(pluginDir string) bool
	PluginOAuthKey                    func(pluginDir string) string
	ForgetPluginGrants                func(plugin string) error
	ReloadGatewayPlugins              func(ctx context.Context, target, profileHome string) (map[string]any, error)
	HostRendezvous                    HostRendezvous
	GatewayRunningPid                 func(pidPath string, cleanupStale bool) (int, bool, error)
	InstalledGatewayServiceKind       func(ctx context.Context) (string, bool)
	RefreshPluginSessions             func(ctx context.Context, home, pluginKey, note string) error
	MCP                               MCPConnector
	BroadcastGlobalEvent              func(name string, payload map[string]any)
	ActiveMemoryProvider              func(ctx context.Context, h string) string
	PruneInactiveMemoryProviderSkills func(ctx context.Context, h, active string)
	LoadMemoryProvider                func(ctx context.Context, h, name string)
	GatewayLoop                       func(ctx context.Context) (runner any, onLoop bool)
	BoundRoute                        func(ctx context.Context) any
	ResolvePluginMessageTarget        func(ctx context.Context, runner any, sessionKey string, route any, pluginHome, pluginID string) (target any, refusal map[string]any, err error)
	PostPluginMessage                 func(ctx context.Context, target any, text, pluginID string) map[string]any
	EditPluginMessage                 func(ctx context.Context, target, messageID any, text, pluginID string) map[string]any
	ActiveSessionPolicy               func(ctx context.Context) (platform, cwd string, ok bool)
	GatewayRunnerRef                  func() PlatformActionRunner
	ParseServerLiveness               func(raw any) (ServerLiveness, error)
	DiscoverMemoryProviders           func(ctx context.Context) [][2]string
	OutboundWebhookTargets            func(cfg map[string]any) []OutboundWebhookTarget
}

// Host rendezvous record of a running dashboard or Desktop backend, Record carried back to the rendezvous
type HostRecord struct {
	Role   string
	Port   int
	Record any
}

// Host rendezvous reads the activation path makes to find the running serve backend
type HostRendezvous interface {
	ReadRecord(role string) (*HostRecord, error)
	RecordTokenIsConsistent(record *HostRecord) bool
	ReadToken(role string) (string, error)
	DialHost(record *HostRecord) string
}

// MCP configuration, registration and naming the activation path reaches through the wiring
type MCPConnector interface {
	LoadConfig(ctx context.Context) (map[string]any, error)
	FilterSuspicious(servers map[string]any) map[string]any
	RegisterServers(ctx context.Context, servers map[string]any) ([]string, error)
	RegisteredToolNames(ctx context.Context, server string) ([]string, error)
	UtilityKinds() []string
	Status(ctx context.Context) ([]map[string]any, error)
	SanitizeNameComponent(value string) string
	ToolNamePrefix() string
}

var services atomic.Pointer[Services]

// Installs the process wide collaborator set used by every plugin manager
func InstallServices(s *Services) { services.Store(s) }

// Installed collaborator set, nil before the wiring runs
func installedServices() *Services { return services.Load() }

// Error naming a collaborator the wiring never installed
type ServiceMissingError struct{ Name string }

func (e *ServiceMissingError) Error() string {
	return "plugin service " + e.Name + " is not installed"
}

var (
	backgroundDiscoveryMu   sync.Mutex
	backgroundDiscoveryDone chan struct{}
)

type discoveryWorkerKey struct{}

// Discovers and loads every plugin of the active profile, joining an in flight background scan first
func DiscoverPlugins(ctx context.Context, force bool) {
	if config.SafeWorkerEnabled() {
		return
	}
	joinBackgroundDiscovery(ctx)
	GetPluginManager(ctx).DiscoverAndLoad(ctx, force)
}

// Starts plugin discovery on a background goroutine unless done or already in flight
func StartBackgroundPluginDiscovery(ctx context.Context) {
	if config.SafeWorkerEnabled() {
		return
	}
	m := GetPluginManager(ctx)
	m.mu.RLock()
	discovered := m.discovered
	m.mu.RUnlock()
	if discovered {
		return
	}
	backgroundDiscoveryMu.Lock()
	defer backgroundDiscoveryMu.Unlock()
	if backgroundDiscoveryDone != nil {
		select {
		case <-backgroundDiscoveryDone:
		default:
			return
		}
	}
	done := make(chan struct{})
	backgroundDiscoveryDone = done
	workerCtx := context.WithValue(context.WithoutCancel(ctx), discoveryWorkerKey{}, true)
	go func() {
		defer close(done)
		_, err := guardedValue(func() (any, error) {
			m.DiscoverAndLoad(workerCtx, false)
			persistPluginToolsetKeys(workerCtx)
			return nil, nil
		})
		if err != nil {
			logWarn("background plugin discovery failed: " + err.Error())
		}
	}()
}

// Reports whether a background discovery goroutine is still running
func backgroundDiscoveryInFlight() bool {
	backgroundDiscoveryMu.Lock()
	done := backgroundDiscoveryDone
	backgroundDiscoveryMu.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// Waits up to thirty seconds for an in flight background discovery unless called from inside it
func joinBackgroundDiscovery(ctx context.Context) {
	backgroundDiscoveryMu.Lock()
	done := backgroundDiscoveryDone
	backgroundDiscoveryMu.Unlock()
	if done == nil || ctx.Value(discoveryWorkerKey{}) != nil || inPluginLoadWorker(ctx) {
		return
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

// Path of the persisted plugin toolset key cache
func pluginToolsetKeysCachePath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "cache", "plugin_toolset_keys.json")
}

// Persists the plugin toolset keys and portable MCP server names for the next launch
func persistPluginToolsetKeys(ctx context.Context) {
	keys := []string{}
	seen := map[string]bool{}
	for _, ts := range GetPluginToolsets(ctx) {
		if !seen[ts.Key] {
			seen[ts.Key] = true
			keys = append(keys, ts.Key)
		}
	}
	sort.Strings(keys)
	portable := sortedKeys(GetPluginManager(ctx).GetPortableMCPServers())
	data, err := json.Marshal(map[string]any{"toolset_keys": keys, "portable_mcp": portable})
	if err == nil {
		path := pluginToolsetKeysCachePath(ctx)
		if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			err = atomicWrite(path, data, 0o600)
		}
	}
	if err != nil {
		logDebug("plugin toolset key persist failed: " + err.Error())
	}
}

// Live set once discovery finished, the persisted launch set while a background scan runs, else discovers
func nowaitPluginSet(ctx context.Context, field string, live func(*Manager) map[string]bool) map[string]bool {
	m := GetPluginManager(ctx)
	inFlight := backgroundDiscoveryInFlight()
	m.mu.RLock()
	discovered := m.discovered
	m.mu.RUnlock()
	if discovered && !inFlight {
		return live(m)
	}
	if inFlight {
		if raw, err := os.ReadFile(pluginToolsetKeysCachePath(ctx)); err == nil {
			var blob map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(string(raw), "\ufeff")), &blob) == nil {
				if values, ok := blob[field].([]any); ok {
					out := map[string]bool{}
					valid := true
					for _, v := range values {
						s, isStr := v.(string)
						valid = valid && isStr
						out[s] = true
					}
					if valid {
						return out
					}
				}
			}
		}
	}
	DiscoverPlugins(ctx, false)
	return live(m)
}

// Plugin toolset keys without blocking on an in flight background discovery
func GetPluginToolsetKeysNowait(ctx context.Context) map[string]bool {
	return nowaitPluginSet(ctx, "toolset_keys", func(*Manager) map[string]bool {
		out := map[string]bool{}
		for _, ts := range GetPluginToolsets(ctx) {
			out[ts.Key] = true
		}
		return out
	})
}

// Portable MCP server names without blocking on an in flight background discovery
func GetPortableMCPServerNamesNowait(ctx context.Context) map[string]bool {
	return nowaitPluginSet(ctx, "portable_mcp", func(m *Manager) map[string]bool {
		return toSet(sortedKeys(m.GetPortableMCPServers()))
	})
}

// Whether config enables a portable package with MCP servers, scanned on a fresh manager
func HasEnabledAgentPluginMCP(ctx context.Context, rawConfig map[string]any) bool {
	return NewManager(pluginHomeKey(ctx)).HasEnabledPortableMCP(rawConfig)
}

// Active profile's manager after idempotent or forced discovery
func ensurePluginsDiscovered(ctx context.Context, force bool) *Manager {
	m := GetPluginManager(ctx)
	m.DiscoverAndLoad(ctx, force)
	return m
}

// Plugin registered context engine after discovery, nil when none
func GetPluginContextEngine(ctx context.Context) any {
	return ensurePluginsDiscovered(ctx, false).ContextEngine()
}

// Handler of a plugin slash command after discovery
func GetPluginCommandHandler(ctx context.Context, name string) (PluginCommandHandler, bool) {
	entry, ok := ensurePluginsDiscovered(ctx, false).PluginCommands()[name]
	if !ok {
		return nil, false
	}
	return entry.Handler, true
}

// Plugin slash commands by name after discovery
func GetPluginCommands(ctx context.Context) map[string]*PluginCommand {
	return ensurePluginsDiscovered(ctx, false).PluginCommands()
}

// Plugin auxiliary task registrations sorted by key after discovery
func GetPluginAuxiliaryTasks(ctx context.Context) []map[string]any {
	tasks := ensurePluginsDiscovered(ctx, false).AuxiliaryTasks()
	out := make([]map[string]any, 0, len(tasks))
	for _, k := range sortedKeys(tasks) {
		out = append(out, tasks[k])
	}
	return out
}

// One plugin toolset row of the tools picker
type PluginToolset struct {
	Key, Label, Description string
}

// Python str.title over ASCII and Unicode letters
func pyTitle(s string) string {
	var b strings.Builder
	prevLetter := false
	for _, r := range s {
		if unicode.IsLetter(r) {
			if prevLetter {
				b.WriteRune(unicode.ToLower(r))
			} else {
				b.WriteRune(unicode.ToTitle(r))
			}
			prevLetter = true
			continue
		}
		prevLetter = false
		b.WriteRune(r)
	}
	return b.String()
}

// Plugin toolsets as key, label and description rows for the tools picker
func GetPluginToolsets(ctx context.Context) []PluginToolset {
	m := GetPluginManager(ctx)
	m.mu.RLock()
	toolNames := sortedSet(m.pluginToolNames)
	plugins := make([]*LoadedPlugin, 0, len(m.plugins))
	for _, k := range sortedKeys(m.plugins) {
		plugins = append(plugins, m.plugins[k])
	}
	m.mu.RUnlock()
	if len(toolNames) == 0 {
		return []PluginToolset{}
	}
	toolsetTools := map[string][]string{}
	for _, name := range toolNames {
		if d := tools.Default.Lookup(ctx, m.home, name); d != nil {
			toolsetTools[d.Toolset] = append(toolsetTools[d.Toolset], d.Name)
		}
	}
	toolsetPlugin := map[string]*LoadedPlugin{}
	for _, loaded := range plugins {
		for _, name := range loaded.ToolsRegistered {
			if d := tools.Default.Lookup(ctx, m.home, name); d != nil {
				if _, ok := toolsetTools[d.Toolset]; ok {
					if _, set := toolsetPlugin[d.Toolset]; !set {
						toolsetPlugin[d.Toolset] = loaded
					}
				}
			}
		}
	}
	out := []PluginToolset{}
	for _, key := range sortedKeys(toolsetTools) {
		desc := ""
		if p := toolsetPlugin[key]; p != nil {
			desc = p.Manifest.Description
		}
		if desc == "" {
			names := append([]string(nil), toolsetTools[key]...)
			sort.Strings(names)
			desc = strings.Join(names, ", ")
		}
		out = append(out, PluginToolset{Key: key, Label: "🔌 " + pyTitle(strings.ReplaceAll(key, "_", " ")), Description: desc})
	}
	return out
}

type toolWhitelistKey struct{}

// Allowed tool set and deny message format bound to a context
type toolWhitelist struct {
	allowed map[string]bool
	format  string
}

// Restricts the tools pre_tool_call admits for work under the returned context
func WithToolWhitelist(ctx context.Context, allowed map[string]bool, denyFmt string) context.Context {
	if denyFmt == "" {
		denyFmt = "Tool '{tool_name}' denied: not in this thread's tool whitelist"
	}
	return context.WithValue(ctx, toolWhitelistKey{}, &toolWhitelist{allowed: allowed, format: denyFmt})
}

// Lifts any tool whitelist for work under the returned context
func ClearToolWhitelist(ctx context.Context) context.Context {
	return context.WithValue(ctx, toolWhitelistKey{}, (*toolWhitelist)(nil))
}

// Resolved pre_tool_call outcome
type PreToolCallDirective struct {
	Action, Message, RuleKey string
	ModifiedArgs             map[string]any
}

// Observability ids forwarded to pre_tool_call hooks
type PreToolCallIDs struct {
	TaskID, SessionID, ToolCallID, TurnID, APIRequestID string
	MiddlewareTrace                                     []map[string]any
}

// Collects pre_tool_call directives with block over approve over none and merges modify args
func preToolCallDirectiveDetails(ctx context.Context, toolName string, args map[string]any, ids PreToolCallIDs) (PreToolCallDirective, error) {
	if wl, _ := ctx.Value(toolWhitelistKey{}).(*toolWhitelist); wl != nil && wl.allowed != nil && !wl.allowed[toolName] {
		return PreToolCallDirective{Action: "block", Message: strings.ReplaceAll(wl.format, "{tool_name}", toolName)}, nil
	}
	s := installedServices()
	if s == nil || s.InvokeLifecycleHook == nil {
		return PreToolCallDirective{}, &ServiceMissingError{Name: "InvokeLifecycleHook"}
	}
	if args == nil {
		args = map[string]any{}
	}
	trace := make([]any, 0, len(ids.MiddlewareTrace))
	for _, t := range ids.MiddlewareTrace {
		trace = append(trace, t)
	}
	results := s.InvokeLifecycleHook(ctx, "pre_tool_call", map[string]any{"tool_name": toolName, "args": args,
		"task_id": ids.TaskID, "session_id": ids.SessionID, "tool_call_id": ids.ToolCallID, "turn_id": ids.TurnID,
		"api_request_id": ids.APIRequestID, "middleware_trace": trace})
	var modified map[string]any
	var approve *PreToolCallDirective
	for _, raw := range results {
		result, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		action := result["action"]
		if action == "modify" {
			if partial, ok := result["args"].(map[string]any); ok && len(partial) > 0 {
				base := modified
				if base == nil {
					base = args
				}
				merged := copyKwargs(base)
				for k, v := range partial {
					merged[k] = v
				}
				modified = merged
			}
			continue
		}
		if action != "block" && action != "approve" {
			continue
		}
		message, _ := result["message"].(string)
		if action == "block" {
			if message == "" {
				continue
			}
			return PreToolCallDirective{Action: "block", Message: message, ModifiedArgs: modified}, nil
		}
		if approve == nil {
			ruleKey, _ := result["rule_key"].(string)
			approve = &PreToolCallDirective{Action: "approve", Message: message, RuleKey: strings.TrimSpace(ruleKey)}
		}
	}
	if approve != nil {
		approve.ModifiedArgs = modified
		return *approve, nil
	}
	return PreToolCallDirective{ModifiedArgs: modified}, nil
}

// Back compatible directive and message pair of the pre_tool_call hooks
func GetPreToolCallDirective(ctx context.Context, toolName string, args map[string]any, ids PreToolCallIDs) (string, string, error) {
	d, err := preToolCallDirectiveDetails(ctx, toolName, args, ids)
	return d.Action, d.Message, err
}

// Block message of the pre_tool_call hooks, empty when nothing blocks
func GetPreToolCallBlockMessage(ctx context.Context, toolName string, args map[string]any, ids PreToolCallIDs) (string, error) {
	action, message, err := GetPreToolCallDirective(ctx, toolName, args, ids)
	if action != "block" {
		message = ""
	}
	return message, err
}

// Final block message of the pre_tool_call hooks after the approval gate, empty to proceed
func ResolvePreToolBlock(ctx context.Context, toolName string, args map[string]any, ids PreToolCallIDs) (string, error) {
	msg, _, err := DispatchPreToolCallHooks(ctx, toolName, args, ids)
	return msg, err
}

// Fail closed approval logic turning a directive into a block message, empty to proceed
func resolveBlockFromDetails(ctx context.Context, d PreToolCallDirective, toolName string, ids PreToolCallIDs) string {
	if d.Action == "block" {
		return d.Message
	}
	if d.Action != "approve" {
		return ""
	}
	ruleKey := d.RuleKey
	if ruleKey == "" {
		ruleKey = toolName
	}
	s := installedServices()
	if s == nil || s.RequestToolApproval == nil {
		return "BLOCKED: plugin approval gate failed for " + toolName
	}
	result, err := guardedValue(func() (any, error) {
		return s.RequestToolApproval(ctx, toolName, d.Message, ruleKey, ids.TurnID, ids.ToolCallID, ids.SessionID)
	})
	decision, _ := result.(map[string]any)
	if err != nil {
		return "BLOCKED: plugin approval gate failed for " + toolName
	}
	if !config.PyTruthy(decision["approved"]) {
		if msg := decision["message"]; config.PyTruthy(msg) {
			return fmt.Sprint(msg)
		}
		return "BLOCKED: plugin approval required for " + toolName
	}
	return ""
}

// Invokes pre_tool_call hooks once returning the resolved block message and the merged modify args
func DispatchPreToolCallHooks(ctx context.Context, toolName string, args map[string]any, ids PreToolCallIDs) (string, map[string]any, error) {
	d, err := preToolCallDirectiveDetails(ctx, toolName, args, ids)
	if err != nil {
		return "", nil, err
	}
	return resolveBlockFromDetails(ctx, d, toolName, ids), d.ModifiedArgs, nil
}

// Scope signals forwarded to pre_verify hooks
type PreVerifyInput struct {
	SessionID, Platform, Model, FinalResponse string
	Coding                                    bool
	Attempt                                   int
	ChangedPaths                              []string
}

// First non empty continue or block message of the pre_verify hooks, empty to let the turn finish
func GetPreVerifyContinueMessage(ctx context.Context, in PreVerifyInput) string {
	paths := make([]any, 0, len(in.ChangedPaths))
	for _, p := range in.ChangedPaths {
		paths = append(paths, p)
	}
	results := InvokeHook(ctx, "pre_verify", map[string]any{"session_id": in.SessionID, "platform": in.Platform,
		"model": in.Model, "coding": in.Coding, "attempt": in.Attempt, "final_response": in.FinalResponse, "changed_paths": paths})
	for _, raw := range results {
		result, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		actionVal := result["action"]
		if !config.PyTruthy(actionVal) {
			actionVal = result["decision"]
		}
		action := ""
		if config.PyTruthy(actionVal) {
			action = strings.ToLower(strings.TrimSpace(fmt.Sprint(actionVal)))
		}
		messageVal := result["message"]
		if !config.PyTruthy(messageVal) {
			messageVal = result["reason"]
		}
		message, _ := messageVal.(string)
		if (action == "continue" || action == "block") && strings.TrimSpace(message) != "" {
			return strings.TrimSpace(message)
		}
	}
	return ""
}

// Error facts offered to transform_api_error_classification hooks
type ErrorClassificationInput struct {
	Provider, Model, ErrorType, ErrorCode, ErrorMessage string
	StatusCode                                          *int
	ErrorBody                                           map[string]any
	Err                                                 error
	ApproxTokens, ContextLength, NumMessages            int
}

// First valid plugin error classification in registration order, sanitized, nil when none
func GetPluginErrorClassification(ctx context.Context, in ErrorClassificationInput) (map[string]any, error) {
	s := installedServices()
	if s == nil || s.FailoverReason == nil {
		return nil, &ServiceMissingError{Name: "FailoverReason"}
	}
	body := in.ErrorBody
	if body == nil {
		body = map[string]any{}
	}
	var status any
	if in.StatusCode != nil {
		status = *in.StatusCode
	}
	var errValue any
	if in.Err != nil {
		errValue = in.Err
	}
	results := InvokeHook(ctx, "transform_api_error_classification", map[string]any{"provider": in.Provider,
		"model": in.Model, "status_code": status, "error_type": in.ErrorType, "error_code": in.ErrorCode,
		"error_message": in.ErrorMessage, "error_body": body, "error": errValue, "approx_tokens": in.ApproxTokens,
		"context_length": in.ContextLength, "num_messages": in.NumMessages})
	type valid struct {
		result map[string]any
		reason any
	}
	var picks []valid
	for _, raw := range results {
		result, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		reason := result["reason"]
		if str, isStr := reason.(string); isStr {
			reason = strings.ToLower(strings.TrimSpace(str))
		}
		if r, ok := s.FailoverReason(reason); ok {
			picks = append(picks, valid{result, r})
		}
	}
	if len(picks) == 0 {
		return nil, nil
	}
	result := picks[0].result
	winner := map[string]any{"reason": picks[0].reason}
	for _, key := range []string{"retryable", "should_compress", "should_rotate_credential", "should_fallback"} {
		if v, ok := result[key]; ok {
			winner[key] = config.PyTruthy(v)
		}
	}
	if msg, ok := result["message"].(string); ok && strings.TrimSpace(msg) != "" {
		trimmed := []rune(strings.TrimSpace(msg))
		if len(trimmed) > 500 {
			trimmed = trimmed[:500]
		}
		winner["message"] = string(trimmed)
	}
	if ec, ok := result["error_context"].(map[string]any); ok {
		winner["error_context"] = ec
	}
	if len(picks) > 1 {
		logWarn(fmt.Sprintf("transform_api_error_classification: skipped %d valid classification(s) after the first result in registration order won (run-all-then-pick-first)", len(picks)-1))
	}
	return winner, nil
}
