package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// TerminalPolicy is the complete TERMINAL_* policy of one profile, total by construction
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

// Error of a profile whose terminal policy could not be resolved
type TerminalPolicyUnavailable struct {
	Reason string
}

// Error names the unreadable policy source
func (e *TerminalPolicyUnavailable) Error() string {
	return "terminal policy unavailable for this profile: " + e.Reason
}

// Config key under terminal mapped to the env var the tool reads
var terminalConfigEnvMap = func() map[string]string {
	m := map[string]string{"backend": "TERMINAL_ENV"}
	for _, key := range []string{
		"modal_mode", "degraded_mode", "cwd", "temp_dir", "timeout", "lifetime_seconds",
		"docker_image", "docker_forward_env", "modal_image",
		"daytona_image", "vercel_runtime", "vercel_image", "ssh_host", "ssh_user", "ssh_port", "ssh_key",
		"container_cpu", "container_memory", "container_disk", "container_persistent",
		"docker_volumes", "docker_env", "docker_mount_cwd_to_workspace", "docker_network",
		"docker_extra_args", "docker_shm_size", "docker_run_as_host_user", "docker_snap_compat",
		"docker_persist_across_processes", "docker_shared_container_key",
		"docker_orphan_reaper", "sandbox_dir", "persistent_shell", "home_mode",
		"singularity_image", "wsl_distro", "wsl_user", "daemon_term_grace_seconds",
		"oneshot_completion_wait_seconds", "process_wait_auto_notify", "env_passthrough",
	} {
		m[key] = "TERMINAL_" + strings.ToUpper(key)
	}
	return m
}()

// Keys whose default lives in the tool rather than the config defaults
var toolLevelTerminalDefaults = map[string]any{
	"cwd": ".", "ssh_host": "", "ssh_user": "", "ssh_port": 22, "ssh_key": "",
	"docker_orphan_reaper": true, "docker_persist_across_processes": true,
	"sandbox_dir": "", "lifetime_seconds": 300, "docker_shared_container_key": "",
	"home_mode": "auto",
}

// Serialises a config value the way the env bridge does, lists and maps as JSON
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

// Writes a terminal config mapping into the policy
func applyTerminalMapping(policy TerminalPolicy, mapping map[string]any) {
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	backend := policy.Get("TERMINAL_ENV", "")
	if b, ok := mapping["backend"].(string); ok && b != "" {
		backend = b
	}
	for _, key := range keys {
		value := mapping[key]
		if value == nil || (key == "cwd" && cwdIsPlaceholder(value)) {
			continue
		}
		envVar, ok := terminalConfigEnvMap[key]
		if !ok {
			continue
		}
		if key == "cwd" {
			if s, ok := value.(string); ok && !isSSHRemoteTildeCwd(backend, strings.TrimSpace(s)) {
				value = expandUser(s)
			}
		}
		policy[envVar] = terminalEnvValue(value)
	}
}

// Code default terminal policy, what a profile bypassing session freezes
func defaultTerminalScope() TerminalPolicy {
	policy := TerminalPolicy{}
	merged := map[string]any{}
	for k, v := range toolLevelTerminalDefaults {
		merged[k] = v
	}
	for k, v := range config.AsMap(config.Defaults()["terminal"]) {
		merged[k] = v
	}
	applyTerminalMapping(policy, merged)
	return policy
}

// Reads the profile dotenv bytes, a seam for tests that deny the read
var readProfileEnvBytes = os.ReadFile

// Builds the complete TERMINAL_* policy of a home with the process environment as the overlay layer
func BuildTerminalScope(h string, secrets *config.Secrets) (TerminalPolicy, error) {
	envPath := home.EnvPath(h)
	if secrets != nil {
		envPath = secrets.Path()
	}
	return buildTerminalLayers(context.Background(), h, envPath, processTerminalEnv())
}

// Builds the complete TERMINAL_* policy of a profile home over a trusted launch overlay, never the process env
func BuildProfileTerminalScope(ctx context.Context, h string, envOverlay map[string]string) (TerminalPolicy, error) {
	policy, err := buildTerminalLayers(ctx, h, home.EnvPath(h), envOverlay)
	if err != nil {
		return nil, err
	}
	resolveScopeCwdPlaceholder(policy)
	return policy, nil
}

// TERMINAL_* assignments of the process environment
func processTerminalEnv() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "TERMINAL_") {
			out[k] = v
		}
	}
	return out
}

// Layers defaults, profile dotenv, overlay, profile config and managed config into one total policy
func buildTerminalLayers(ctx context.Context, h, envPath string, envOverlay map[string]string) (TerminalPolicy, error) {
	policy := defaultTerminalScope()
	defaultImage, hasDefaultImage := policy["TERMINAL_DOCKER_IMAGE"]
	imagePinned := false
	if _, err := os.Stat(envPath); err == nil {
		raw, err := readProfileEnvBytes(envPath)
		if err != nil && (!errors.Is(err, fs.ErrPermission) || config.PinnedTerminalBackend() == "") {
			return nil, &TerminalPolicyUnavailable{Reason: fmt.Sprintf("cannot read %s: %v", envPath, err)}
		}
		profileEnv := map[string]string{}
		if err == nil {
			profileEnv = config.ParseEnv(config.DecodeEnvBytes(raw))
		}
		for k, v := range profileEnv {
			if strings.HasPrefix(k, "TERMINAL_") {
				policy[k] = v
			}
		}
		_, imagePinned = profileEnv["TERMINAL_DOCKER_IMAGE"]
	}
	for k, v := range envOverlay {
		if strings.HasPrefix(k, "TERMINAL_") && k != "TERMINAL_DOCKER_IMAGE_PINNED" {
			policy[k] = v
		}
	}
	if v, ok := envOverlay["TERMINAL_DOCKER_IMAGE"]; ok && (!hasDefaultImage || v != defaultImage) {
		imagePinned = true
	}
	configPath := home.ConfigPath(h)
	hasConfig, err := config.ConfigExists(ctx, configPath)
	if err != nil {
		return nil, &TerminalPolicyUnavailable{Reason: fmt.Sprintf("cannot resolve terminal config in %s: %v", h, err)}
	}
	var rawTerminal map[string]any
	if hasConfig {
		raw, err := config.ReadConfigDocReadonly(ctx, configPath)
		if err != nil {
			return nil, &TerminalPolicyUnavailable{Reason: fmt.Sprintf("cannot parse %s: %v", configPath, err)}
		}
		if doc, ok := raw.(map[string]any); ok {
			rawTerminal, _ = doc["terminal"].(map[string]any)
		}
		if rawTerminal != nil {
			applyTerminalMapping(policy, rawTerminal)
			if _, ok := rawTerminal["docker_image"]; ok {
				imagePinned = true
			}
		}
	}
	if managed, ok := config.ApplyManagedOverlay(map[string]any{})["terminal"].(map[string]any); ok {
		base := rawTerminal
		if base == nil {
			base = map[string]any{}
		}
		applyTerminalMapping(policy, config.DeepMerge(base, managed))
		if managed["docker_image"] != nil {
			imagePinned = true
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
	if policy["TERMINAL_CWD"] != "" {
		return
	}
	homeFallback, _ := os.UserHomeDir()
	mount := strings.ToLower(strings.TrimSpace(policy.Get("TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE", "false")))
	resolved, ok := ResolvePlaceholderTerminalCwd("", policy["TERMINAL_ENV"], "", mount == "true" || mount == "1" || mount == "yes", homeFallback)
	if ok && resolved != "" {
		policy["TERMINAL_CWD"] = resolved
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
	slog.Default().Warn(fmt.Sprintf("terminal policy unavailable: %s", reason))
	return &TerminalScope{Refusal: &TerminalPolicyRefusal{Reason: reason}}
}

// Binds the profile policy of a home on the context, the refusal scope when it cannot be resolved
func WithProfileTerminalScope(ctx context.Context, h string, envOverlay map[string]string) context.Context {
	return SetTerminalScope(ctx, PrepareProfileTerminalScope(ctx, h, envOverlay))
}

// Binds a scope prepared earlier for a bounded turn, building the home policy when none was prepared
func WithPreparedTerminalScope(ctx context.Context, h string, prepared *TerminalScope) context.Context {
	if prepared != nil {
		return SetTerminalScope(ctx, prepared)
	}
	return WithProfileTerminalScope(ctx, h, nil)
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

// Cached policy with the inputs it was built from
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

// TERMINAL_* values present in the process environment, as one signature
func processTerminalEnvSig() string {
	var parts []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TERMINAL_") {
			parts = append(parts, kv)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
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
	var secrets *config.Secrets
	if env != nil {
		h, secrets = env.Home, env.Secrets
	}
	if secrets == nil && h != "" {
		secrets = config.NewSecrets(h)
	}
	configSig := fileSig(home.ConfigPath(h)) + "|" + fileSig(home.EnvPath(h))
	if md := config.ManagedDir(); md != "" {
		configSig += "|" + fileSig(filepath.Join(md, "config.yaml"))
	}
	envSig := processTerminalEnvSig()
	scopeCache.Lock()
	defer scopeCache.Unlock()
	if entry := scopeCache.entries[h]; entry != nil && entry.configSig == configSig && entry.envSig == envSig {
		return entry.policy, entry.err
	}
	policy, err := BuildTerminalScope(h, secrets)
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
	return sessionScope{envType: policy.Get("TERMINAL_ENV", "local"), persistent: policy.GetBool("TERMINAL_CONTAINER_PERSISTENT", "true")}
}
