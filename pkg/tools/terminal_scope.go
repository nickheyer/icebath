package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
)

// TerminalPolicy is the frozen terminal settings of one profile keyed by their policy names, total by construction
type TerminalPolicy map[string]string

// Get reads a policy value, the default when the key is absent
func (p TerminalPolicy) Get(name, def string) string {
	if v, ok := p[name]; ok {
		return v
	}
	return def
}

// GetBool reads a policy value as true, 1 or yes
func (p TerminalPolicy) GetBool(name, def string) bool {
	switch strings.ToLower(p.Get(name, def)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// Value of the terminal setting key in the policy, its registry default when the policy lacks it
func (p TerminalPolicy) Setting(key string) string {
	if v, ok := p[terminalConfigEnvMap[key]]; ok {
		return v
	}
	return terminalDefault(key)
}

// Terminal setting key of the policy as true, 1 or yes
func (p TerminalPolicy) SettingBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(p.Setting(key))) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// Policy text of a terminal setting's registry default, empty when unset by default
func terminalDefault(key string) string {
	e := settings.Find("terminal." + key)
	if e == nil {
		return ""
	}
	v := canonicalTerminalValue(key, e.DefaultValue())
	if key == "cwd" && cwdIsPlaceholder(v) {
		return ""
	}
	return terminalEnvValue(v)
}

// Error of a profile whose terminal policy could not be resolved
type TerminalPolicyUnavailable struct {
	Reason string
}

// Error names the unreadable policy source
func (e *TerminalPolicyUnavailable) Error() string {
	return "terminal policy unavailable for this profile: " + e.Reason
}

// Config key under terminal mapped to its name in the terminal policy
var terminalConfigEnvMap = func() map[string]string {
	m := map[string]string{"backend": "TERMINAL_ENV"}
	for _, key := range []string{
		"degraded_mode", "cwd", "temp_dir", "timeout", "lifetime_seconds",
		"docker_image", "docker_forward_env", "modal_image",
		"daytona_image", "vercel_runtime", "vercel_image", "ssh_host", "ssh_user", "ssh_port", "ssh_key",
		"container_cpu", "container_memory", "container_disk", "container_persistent",
		"docker_volumes", "docker_env", "docker_mount_cwd_to_workspace", "docker_network",
		"docker_extra_args", "docker_shm_size", "docker_run_as_host_user", "docker_snap_compat",
		"docker_persist_across_processes", "docker_shared_container_key",
		"docker_orphan_reaper", "sandbox_dir", "persistent_shell", "local_persistent", "home_mode",
		"singularity_image", "daemon_term_grace_seconds",
		"oneshot_completion_wait_seconds", "process_wait_auto_notify", "env_passthrough",
	} {
		m[key] = "TERMINAL_" + strings.ToUpper(key)
	}
	return m
}()

// Policy name of the launch working folder a launcher hands its backend process
const launchCwdName = "TERMINAL_CWD"

// Serialises a setting value for the policy, lists and maps as JSON
func terminalEnvValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case []any, map[string]any:
		b, _ := json.Marshal(t)
		return string(b)
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		return fmt.Sprint(t)
	}
}

// Reports whether a cwd value is a placeholder resolved per surface rather than a policy value
func cwdIsPlaceholder(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	switch strings.TrimSpace(s) {
	case ".", "auto", "cwd":
		return true
	}
	return false
}

// Whether the backend needs the remote shell to expand a tilde cwd itself
func isSSHRemoteTildeCwd(backend, cwd string) bool {
	return strings.ToLower(strings.TrimSpace(backend)) == "ssh" && (cwd == "~" || strings.HasPrefix(cwd, "~/"))
}

// Terminal config keys in a stable order
func terminalConfigKeys() []string {
	keys := make([]string, 0, len(terminalConfigEnvMap))
	for k := range terminalConfigEnvMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Canonical stored form of a terminal setting's value, the value as given when it does not fit
func canonicalTerminalValue(key string, v any) any {
	if v == nil {
		return nil
	}
	if e := settings.Find("terminal." + key); e != nil {
		if c, err := e.Coerce(v); err == nil {
			return c
		}
	}
	return v
}

// Stores one terminal value in the policy, skipping unset values and placeholder working folders
func setTerminalValue(policy TerminalPolicy, key string, v any) {
	if v == nil || (key == "cwd" && cwdIsPlaceholder(v)) {
		return
	}
	if key == "cwd" {
		if s, ok := v.(string); ok && !isSSHRemoteTildeCwd(policy[terminalConfigEnvMap["backend"]], strings.TrimSpace(s)) {
			v = expandUser(s)
		}
	}
	policy[terminalConfigEnvMap[key]] = terminalEnvValue(v)
}

// Registry default terminal policy, what a profile bypassing session freezes
func defaultTerminalScope() TerminalPolicy {
	policy := TerminalPolicy{}
	for _, key := range terminalConfigKeys() {
		if e := settings.Find("terminal." + key); e != nil {
			setTerminalValue(policy, key, canonicalTerminalValue(key, e.DefaultValue()))
		}
	}
	return policy
}

// DefaultTerminalScope is the terminal policy of the registry defaults with no profile config folded in
func DefaultTerminalScope() TerminalPolicy { return defaultTerminalScope() }

// Builds the complete terminal policy of a home, the launch working folder of this process applying where the profile sets none
func BuildTerminalScope(h string) (TerminalPolicy, error) {
	overlay := map[string]string{}
	if cwd, ok := os.LookupEnv(launchCwdName); ok {
		overlay[launchCwdName] = cwd
	}
	return buildTerminalLayers(context.Background(), h, overlay, true)
}

// Builds the complete terminal policy of a profile home over a trusted launch overlay, never the process env
func BuildProfileTerminalScope(ctx context.Context, h string, envOverlay map[string]string) (TerminalPolicy, error) {
	policy, err := buildTerminalLayers(ctx, h, envOverlay, false)
	if err != nil {
		return nil, err
	}
	resolveScopeCwdPlaceholder(policy)
	return policy, nil
}

// Settings of a home (defaults, config.yaml, managed pins) over a launch overlay that only fills what the profile leaves unset; a scoped build refuses a config that does not parse
func buildTerminalLayers(ctx context.Context, h string, envOverlay map[string]string, unscoped bool) (TerminalPolicy, error) {
	configPath := home.ConfigPath(h)
	hasConfig, err := config.ConfigExists(ctx, configPath)
	if err != nil {
		return nil, &TerminalPolicyUnavailable{Reason: fmt.Sprintf("cannot resolve terminal config in %s: %v", h, err)}
	}
	if hasConfig {
		if _, err := config.ReadConfigDocReadonly(ctx, configPath); err != nil && !(unscoped && config.IsYAMLError(err)) {
			return nil, &TerminalPolicyUnavailable{Reason: fmt.Sprintf("cannot parse %s: %v", configPath, err)}
		}
	}
	sctx := home.WithOverride(ctx, h)
	policy := TerminalPolicy{}
	imagePinned := false
	keys := terminalConfigKeys()
	for _, key := range keys {
		if key == "cwd" {
			continue
		}
		v, err := settings.Resolve(sctx, "terminal."+key)
		if err != nil {
			return nil, fmt.Errorf("terminal.%s: %w", key, err)
		}
		name := terminalConfigEnvMap[key]
		if over, ok := envOverlay[name]; ok && v.Source == settings.SourceDefault {
			policy[name] = over
			if key == "docker_image" && over != terminalEnvValue(canonicalTerminalValue(key, v.Value)) {
				imagePinned = true
			}
			continue
		}
		if key == "docker_image" && v.Source != settings.SourceDefault && v.Value != nil {
			imagePinned = true
		}
		setTerminalValue(policy, key, canonicalTerminalValue(key, v.Value))
	}
	cwd, err := settings.Resolve(sctx, settings.WorkingFolder.Key)
	if err != nil {
		return nil, err
	}
	if over, ok := envOverlay[launchCwdName]; ok && (cwd.Source == settings.SourceDefault || cwdIsPlaceholder(cwd.Value)) {
		policy[launchCwdName] = over
	} else {
		setTerminalValue(policy, "cwd", cwd.Value)
	}
	for name, v := range envOverlay {
		if _, set := policy[name]; !set && strings.HasPrefix(name, "TERMINAL_") && name != "TERMINAL_DOCKER_IMAGE_PINNED" && !slices.Contains(keys, strings.ToLower(strings.TrimPrefix(name, "TERMINAL_"))) {
			policy[name] = v
		}
	}
	if imagePinned {
		policy["TERMINAL_DOCKER_IMAGE_PINNED"] = "1"
	} else {
		policy["TERMINAL_DOCKER_IMAGE_PINNED"] = "0"
	}
	return policy, nil
}

// Gives a policy without an explicit cwd the TERMINAL_CWD a standalone gateway resolves at startup
func resolveScopeCwdPlaceholder(policy TerminalPolicy) {
	if policy[launchCwdName] != "" {
		return
	}
	homeFallback, _ := os.UserHomeDir()
	resolved, ok := ResolvePlaceholderTerminalCwd("", policy.Setting("backend"), "", policy.SettingBool("docker_mount_cwd_to_workspace"), homeFallback)
	if ok && resolved != "" {
		policy[launchCwdName] = resolved
	}
}

// Placeholder values of terminal.cwd that carry no directory choice
var cwdPlaceholders = map[string]bool{".": true, "auto": true, "cwd": true}

// TERMINAL_CWD for a configured cwd and backend, false when the sandbox default applies
func ResolvePlaceholderTerminalCwd(configuredCwd, terminalBackend, messagingCwd string, dockerMountCwdToWorkspace bool, homeFallback string) (string, bool) {
	if configuredCwd != "" && !cwdPlaceholders[configuredCwd] {
		return configuredCwd, true
	}
	backend := terminalBackend
	if backend == "" {
		backend = "local"
	}
	backend = strings.ToLower(strings.TrimSpace(backend))
	messaging := strings.TrimSpace(messagingCwd)
	if backend == "local" {
		if messaging != "" {
			return messaging, true
		}
		return homeFallback, true
	}
	if backend == "docker" && dockerMountCwdToWorkspace && messaging != "" && !cwdPlaceholders[messaging] {
		return messaging, true
	}
	return "", false
}

// Refusal scope installed when a profile terminal policy cannot be resolved
type TerminalPolicyRefusal struct {
	Reason string
}

// Terminal scope bound to a turn, a complete policy or a refusal
type TerminalScope struct {
	Policy  TerminalPolicy
	Refusal *TerminalPolicyRefusal
}

// Context key of the bound terminal scope
type terminalScopeKey struct{}

// Binds scope as the context terminal policy, nil unbinds it
func SetTerminalScope(ctx context.Context, scope *TerminalScope) context.Context {
	return context.WithValue(ctx, terminalScopeKey{}, scope)
}

// Active terminal scope of the context, nil when none is bound
func GetTerminalScope(ctx context.Context) *TerminalScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(terminalScopeKey{}).(*TerminalScope)
	return scope
}

// Error when the context scope is a refusal so execution fails closed
func EnforceNoRefusal(ctx context.Context) error {
	return GetTerminalScope(ctx).refusalError()
}

// Unavailable error of a refusal scope, nil for a policy scope or no scope
func (s *TerminalScope) refusalError() error {
	if s != nil && s.Refusal != nil {
		return &TerminalPolicyUnavailable{Reason: s.Refusal.Reason}
	}
	return nil
}

// Profile policy scope of a home, the refusal scope when the policy cannot be resolved
func PrepareProfileTerminalScope(ctx context.Context, h string, envOverlay map[string]string) *TerminalScope {
	policy, err := BuildProfileTerminalScope(ctx, h, envOverlay)
	if err == nil {
		return &TerminalScope{Policy: policy}
	}
	reason := err.Error()
	var unavailable *TerminalPolicyUnavailable
	if errors.As(err, &unavailable) {
		reason = unavailable.Reason
	}
	pkgLog().Warn(fmt.Sprintf("terminal policy unavailable: %s", reason))
	return &TerminalScope{Refusal: &TerminalPolicyRefusal{Reason: reason}}
}

// Binds the profile policy of a home on the context, the refusal scope when it cannot be resolved
func WithProfileTerminalScope(ctx context.Context, h string, envOverlay map[string]string) context.Context {
	return SetTerminalScope(ctx, PrepareProfileTerminalScope(ctx, h, envOverlay))
}

// Env of a call carrying the context terminal scope, the same env when none is bound
func EnvInTerminalScope(ctx context.Context, env *Env) *Env {
	scope := GetTerminalScope(ctx)
	if scope == nil || (env != nil && env.TerminalScope == scope) {
		return env
	}
	scoped := &Env{}
	if env != nil {
		*scoped = *env
	}
	scoped.TerminalScope = scope
	return scoped
}

// Copy of env re-homed at h with its config, secrets, the context terminal scope and shared session values
func EnvForProfileHome(ctx context.Context, env *Env, h string) *Env {
	scoped := &Env{}
	if env != nil {
		*scoped = *env
	}
	scoped.Home, scoped.profileHome = h, h
	scoped.Secrets = config.NewSecrets(h)
	if tree, err := config.LoadReadonly(ctx, h, scoped.Secrets.Lookup()); err == nil {
		scoped.Config = tree
	} else {
		scoped.Config = nil
	}
	scoped.TerminalScope = GetTerminalScope(ctx)
	return scoped
}

// Cached policy with the config and launch folder it was built from
type scopeCacheEntry struct {
	policy    TerminalPolicy
	configSig string
	envSig    string
	err       error
}

// Policies per home, rebuilt when the config or dotenv file changes
var scopeCache = struct {
	sync.Mutex
	entries map[string]*scopeCacheEntry
}{entries: map[string]*scopeCacheEntry{}}

// Change signature of a file from its size and modification time
func fileSig(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
}

// Policy bound to the env scope, else the session home policy rebuilt only when its inputs changed
func terminalPolicy(env *Env) (TerminalPolicy, error) {
	if env != nil && env.TerminalScope != nil {
		if err := env.TerminalScope.refusalError(); err != nil {
			return nil, err
		}
		return env.TerminalScope.Policy, nil
	}
	h := ""
	if env != nil {
		h = env.Home
	}
	configSig := fileSig(home.ConfigPath(h))
	if md := config.ManagedDir(); md != "" {
		configSig += "|" + fileSig(filepath.Join(md, "config.yaml"))
	}
	envSig := os.Getenv(launchCwdName)
	scopeCache.Lock()
	defer scopeCache.Unlock()
	if entry := scopeCache.entries[h]; entry != nil && entry.configSig == configSig && entry.envSig == envSig {
		return entry.policy, entry.err
	}
	policy, err := BuildTerminalScope(h)
	scopeCache.entries[h] = &scopeCacheEntry{policy: policy, configSig: configSig, envSig: envSig, err: err}
	return policy, err
}

// ResetTerminalScopeCache forgets every cached terminal policy
func ResetTerminalScopeCache() {
	scopeCache.Lock()
	defer scopeCache.Unlock()
	scopeCache.entries = map[string]*scopeCacheEntry{}
}

// Backend identity and scoping predicates for one call
type sessionScope struct {
	envType    string
	persistent bool
}

// Reports whether a non persistent sandbox gets a per session identity
func (s sessionScope) sessionIsolated() bool {
	if s.envType != "docker" && !providerFlagBool(s.envType, func(p BackendProvider) bool { return p.SessionIsolatedWhenNonpersistent() }) {
		return false
	}
	return !s.persistent
}

// Reports whether the docker workspace mount and session scoped teardown apply
func (s sessionScope) dockerSessionIsolated() bool {
	return s.envType == "docker" && s.sessionIsolated()
}

// Reports whether one persistent docker container per profile is shared by every session
func (s sessionScope) dockerProfileScoped() bool {
	return s.envType == "docker" && s.persistent
}

// Snapshot of the backend scope for this call
func currentSessionScope(policy TerminalPolicy) sessionScope {
	return sessionScope{envType: policy.Setting("backend"), persistent: policy.SettingBool("container_persistent")}
}
