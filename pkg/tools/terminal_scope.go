package tools

import (
	"encoding/json"
	"fmt"
	"os"
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

// Builds the complete TERMINAL_* policy of a home from defaults, dotenv, process env and config
func BuildTerminalScope(h string, secrets *config.Secrets) (TerminalPolicy, error) {
	policy := defaultTerminalScope()
	defaultImage := policy["TERMINAL_DOCKER_IMAGE"]
	imagePinned := false
	if secrets != nil {
		if _, err := os.Stat(secrets.Path()); err == nil {
			if _, err := os.ReadFile(secrets.Path()); err != nil {
				return nil, &TerminalPolicyUnavailable{Reason: fmt.Sprintf("cannot read %s: %v", secrets.Path(), err)}
			}
			file := secrets.File()
			for k, v := range file {
				if strings.HasPrefix(k, "TERMINAL_") {
					policy[k] = v
				}
			}
			if _, ok := file["TERMINAL_DOCKER_IMAGE"]; ok {
				imagePinned = true
			}
		}
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(k, "TERMINAL_") || k == "TERMINAL_DOCKER_IMAGE_PINNED" {
			continue
		}
		policy[k] = v
		if k == "TERMINAL_DOCKER_IMAGE" && v != defaultImage {
			imagePinned = true
		}
	}
	configPath := home.ConfigPath(h)
	if _, err := os.Stat(configPath); err == nil {
		raw, err := config.ReadDocument(configPath)
		if err != nil {
			return nil, &TerminalPolicyUnavailable{Reason: fmt.Sprintf("cannot parse %s: %v", configPath, err)}
		}
		if terminal, ok := raw["terminal"].(map[string]any); ok {
			applyTerminalMapping(policy, terminal)
			if _, ok := terminal["docker_image"]; ok {
				imagePinned = true
			}
		}
	}
	if imagePinned {
		policy["TERMINAL_DOCKER_IMAGE_PINNED"] = "1"
	} else {
		policy["TERMINAL_DOCKER_IMAGE_PINNED"] = "0"
	}
	return policy, nil
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

// Policy of the session home, rebuilt only when its inputs changed
func terminalPolicy(env *Env) (TerminalPolicy, error) {
	h := ""
	var secrets *config.Secrets
	if env != nil {
		h, secrets = env.Home, env.Secrets
	}
	if secrets == nil && h != "" {
		secrets = config.NewSecrets(h)
	}
	configSig := fileSig(home.ConfigPath(h)) + "|" + fileSig(home.EnvPath(h))
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
