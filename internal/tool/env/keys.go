package env

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
)

type TaskOverrides map[string]string

func (o TaskOverrides) clone() TaskOverrides {
	out := make(TaskOverrides, len(o))
	for k, v := range o {
		out[k] = v
	}
	return out
}

func isIsolationOverrideKey(key string) bool {
	return key == "env_type" || strings.HasSuffix(key, "_image")
}

type sessionScope struct {
	kind       string
	persistent bool
	isolated   bool
}

func (s sessionScope) dockerSessionIsolated() bool { return s.kind == "docker" && s.isolated }

func (s sessionScope) dockerProfileScoped() bool { return s.kind == "docker" && s.persistent }

func (m *Manager) sessionScope(ctx context.Context) (sessionScope, error) {
	m.settings.ensureBridged(ctx)
	kind, err := m.settings.tenv(ctx, "TERMINAL_ENV", "local")
	if err != nil {
		return sessionScope{}, err
	}
	persistent, err := m.settings.tenvBool(ctx, "TERMINAL_CONTAINER_PERSISTENT", "true")
	if err != nil {
		return sessionScope{}, err
	}
	sc := sessionScope{kind: kind, persistent: persistent}
	if kind == "docker" {
		sc.isolated = !persistent
	} else if flags, ok := m.settings.pluginFlags(ctx, kind); ok && flags.SessionIsolatedWhenNonpersistent {
		sc.isolated = !persistent
	}
	return sc, nil
}

func (m *Manager) sessionEnv(ctx context.Context, name string) string {
	vars, _ := m.host.policy.session.Vars(ctx)
	for _, v := range vars {
		if v.Name == name && v.Bound {
			return v.Value
		}
	}
	return os.Getenv(name)
}

func (m *Manager) containerProfile(ctx context.Context) string {
	override := home.Override(ctx)
	session := m.sessionEnv(ctx, "ICEBATH_SESSION_PROFILE")
	bound := ""
	if override != "" {
		bound, _ = home.ProfileName(override)
	}
	if bound != "" {
		if (session != "" && bound != session) || (session == "" && RoutedHome(override)) {
			return bound
		}
	}
	if session != "" {
		return session
	}
	return m.settings.HomeProfileName(ctx)
}

func routedHomeTaskKey(ctx context.Context, profileScoped bool) (string, bool) {
	override := home.Override(ctx)
	if override == "" || !RoutedHome(override) {
		return "", false
	}
	if profileScoped {
		if name, ok := home.ProfileName(override); ok && name != "" {
			return ProfileContainerKey(name), true
		}
	}
	if real, err := resolveLoose(override); err == nil {
		return "home:" + real, true
	}
	return "home:" + override, true
}

func (m *Manager) sessionlessContainerKey(ctx context.Context, profileScoped bool) string {
	if routed, ok := routedHomeTaskKey(ctx, profileScoped); ok {
		return routed
	}
	if profileScoped {
		return ProfileContainerKey(m.settings.HomeProfileName(ctx))
	}
	return "default"
}

func qualifyTaskKey(ctx context.Context, key string) string {
	if scope, ok := routedHomeTaskKey(ctx, true); ok {
		return scope + ":" + key
	}
	return key
}

func orDefault(key string) string {
	if key == "" {
		return "default"
	}
	return key
}

func (m *Manager) hasIsolationOverrides(ctx context.Context, taskID string) bool {
	if taskID == "" {
		return false
	}
	m.ovMu.Lock()
	defer m.ovMu.Unlock()
	for k := range m.overrides[qualifyTaskKey(ctx, taskID)] {
		if isIsolationOverrideKey(k) {
			return true
		}
	}
	return false
}

func (m *Manager) resolveAlias(taskID string) string {
	m.aliasMu.Lock()
	defer m.aliasMu.Unlock()
	seen := map[string]bool{}
	key := taskID
	for {
		next, ok := m.aliases[key]
		if !ok || seen[key] {
			return key
		}
		seen[key] = true
		key = next
	}
}

func (m *Manager) ContainerKey(ctx context.Context, taskID string) (string, error) {
	if taskID != "" && m.hasIsolationOverrides(ctx, taskID) {
		return qualifyTaskKey(ctx, taskID), nil
	}
	sc, err := m.sessionScope(ctx)
	if err != nil {
		return "", err
	}
	if taskID != "" && sc.isolated {
		return qualifyTaskKey(ctx, m.resolveAlias(taskID)), nil
	}
	sessionKey := m.sessionEnv(ctx, "ICEBATH_SESSION_KEY")
	shared := ""
	if sc.dockerProfileScoped() {
		v, err := m.settings.tenv(ctx, "TERMINAL_DOCKER_SHARED_CONTAINER_KEY", "")
		if err != nil {
			return "", err
		}
		shared = strings.TrimSpace(v)
	}
	if shared != "" {
		return "shared:" + shared, nil
	}
	if sessionKey == "" {
		return m.sessionlessContainerKey(ctx, sc.dockerProfileScoped()), nil
	}
	if !sc.dockerProfileScoped() {
		return qualifyTaskKey(ctx, "session:"+sessionKey), nil
	}
	return m.workspaceContainerKey(ctx, ProfileContainerKey(m.containerProfile(ctx)), taskID)
}

func WorkspaceKeyFor(cwd string) string {
	resolved, err := resolveLoose(home.ExpandUser(cwd))
	if err != nil {
		resolved = hostAbs(home.ExpandUser(cwd))
	}
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:])[:16]
}

func (m *Manager) workspaceContainerKey(ctx context.Context, base, taskID string) (string, error) {
	if taskID == "" {
		return base, nil
	}
	if strings.HasPrefix(taskID, base+":ws:") {
		return taskID, nil
	}
	mount, err := m.settings.tenvBool(ctx, "TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE", "false")
	if err != nil {
		return "", err
	}
	if !mount {
		return base, nil
	}
	keys := []string{taskID}
	if alias := m.resolveAlias(taskID); alias != taskID {
		keys = append(keys, alias)
	}
	for _, key := range keys {
		m.ovMu.Lock()
		ov := m.overrides[qualifyTaskKey(ctx, key)]
		m.ovMu.Unlock()
		if len(ov) == 0 || ov["cwd_source"] != "session" {
			continue
		}
		digest, cwd := ov["workspace_key"], ov["cwd"]
		if digest == "" && strings.TrimSpace(cwd) != "" && !strings.Contains(cwd, "\x00") {
			digest = WorkspaceKeyFor(cwd)
		}
		if digest != "" {
			return base + ":ws:" + digest, nil
		}
	}
	return base, nil
}

func isWorkspaceKey(key string) bool {
	return strings.Contains(key, ":ws:")
}

func (m *Manager) ResolveTaskOverrides(ctx context.Context, taskID string) (TaskOverrides, error) {
	raw := orDefault(taskID)
	m.ovMu.Lock()
	ov := m.overrides[qualifyTaskKey(ctx, raw)]
	m.ovMu.Unlock()
	if len(ov) > 0 {
		return ov.clone(), nil
	}
	key, err := m.ContainerKey(ctx, raw)
	if err != nil {
		return nil, err
	}
	m.ovMu.Lock()
	ov = m.overrides[key]
	m.ovMu.Unlock()
	if len(ov) > 0 {
		return ov.clone(), nil
	}
	return TaskOverrides{}, nil
}

func (m *Manager) RecordSessionCwd(ctx context.Context, sessionKey, cwd string) {
	if strings.TrimSpace(cwd) == "" {
		return
	}
	key := qualifyTaskKey(ctx, orDefault(sessionKey))
	m.cwdMu.Lock()
	m.sessionCwd[key] = cwd
	m.cwdMu.Unlock()
}

func (m *Manager) SessionCwd(ctx context.Context, sessionKey string) (string, bool) {
	m.cwdMu.Lock()
	defer m.cwdMu.Unlock()
	v, ok := m.sessionCwd[qualifyTaskKey(ctx, orDefault(sessionKey))]
	return v, ok
}

func (m *Manager) ClearSessionCwd(ctx context.Context, sessionKey string) {
	m.cwdMu.Lock()
	delete(m.sessionCwd, qualifyTaskKey(ctx, sessionKey))
	m.cwdMu.Unlock()
}

func (m *Manager) RegisterTaskOverrides(ctx context.Context, taskID string, overrides TaskOverrides) error {
	m.ovMu.Lock()
	m.overrides[qualifyTaskKey(ctx, taskID)] = overrides.clone()
	m.ovMu.Unlock()
	cwd := overrides["cwd"]
	if strings.TrimSpace(cwd) == "" {
		return nil
	}
	m.RecordSessionCwd(ctx, taskID, cwd)
	containerID, err := m.ContainerKey(ctx, taskID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	sh := m.active[taskID]
	if sh == nil {
		sh = m.active[containerID]
	}
	m.mu.Unlock()
	if sh == nil {
		return nil
	}
	if sanitized, ok := m.sanitizeCwdForLiveEnv(ctx, sh, cwd); ok {
		sh.SetCwd(sanitized)
	}
	return nil
}

func (m *Manager) sanitizeCwdForLiveEnv(ctx context.Context, sh *Shell, newCwd string) (string, bool) {
	kind := sh.EnvType()
	newCwd = m.settings.CoerceSSHRemoteCwd(ctx, newCwd, kind)
	if kind == "" || !m.settings.IsContainerBackend(ctx, kind) {
		return newCwd, true
	}
	mounted, mount, _ := HostMount(sh)
	if !IsUnusableContainerCwd(newCwd, mounted) {
		return newCwd, true
	}
	if mounted != "" {
		if mount == "" {
			mount = "/workspace"
		}
		if isMountedHostCwd(newCwd, mounted) {
			return mount, true
		}
		if translated, ok := TranslateMountedHostPath(newCwd, mounted, mount); ok {
			return translated, true
		}
	}
	return "", false
}

func (m *Manager) ClearTaskOverrides(ctx context.Context, taskID string) {
	m.ovMu.Lock()
	delete(m.overrides, qualifyTaskKey(ctx, taskID))
	m.ovMu.Unlock()
	m.ClearSessionCwd(ctx, taskID)
	m.aliasMu.Lock()
	delete(m.aliases, taskID)
	m.aliasMu.Unlock()
}

func (m *Manager) RegisterContainerAlias(childTaskID, parentTaskID string) {
	if childTaskID == "" {
		return
	}
	m.aliasMu.Lock()
	m.aliases[childTaskID] = orDefault(parentTaskID)
	m.aliasMu.Unlock()
}

func selectImage(kind string, overrides TaskOverrides, cfg EnvConfig) string {
	var configured string
	switch kind {
	case "docker":
		configured = cfg.DockerImage
	case "modal":
		configured = cfg.ModalImage
	case "daytona":
		configured = cfg.DaytonaImage
	default:
		return overrides[kind+"_image"]
	}
	if v := overrides[kind+"_image"]; v != "" {
		return v
	}
	return configured
}

func (m *Manager) resolveTaskHostCwd(ctx context.Context, cfg EnvConfig, taskID string) (string, error) {
	if cfg.Kind != "docker" || !cfg.DockerMountCwdToWorkspace {
		return "", nil
	}
	sc, err := m.sessionScope(ctx)
	if err != nil {
		return "", err
	}
	if !sc.dockerSessionIsolated() {
		return cfg.HostCwd, nil
	}
	key, err := m.ContainerKey(ctx, taskID)
	if err != nil {
		return "", err
	}
	if key == "default" {
		return cfg.HostCwd, nil
	}
	ov, err := m.ResolveTaskOverrides(ctx, taskID)
	if err != nil {
		return "", err
	}
	candidate := ov["cwd"]
	if ov["cwd_source"] == "process" || strings.TrimSpace(candidate) == "" {
		return "", nil
	}
	candidate = hostAbs(home.ExpandUser(candidate))
	if !isDir(candidate) || hasContainerPrefix(candidate) {
		return "", nil
	}
	return candidate, nil
}

type Placement struct {
	Config  EnvConfig
	Kind    string
	Key     string
	Image   string
	Cwd     string
	HostCwd string
}

func (m *Manager) Place(ctx context.Context, taskID string, hostLocal bool) (Placement, error) {
	cfg, err := m.settings.EnvConfig(ctx)
	if err != nil {
		return Placement{}, err
	}
	kind := cfg.Kind
	if hostLocal {
		kind = "local"
	} else if err := EnforceNoRefusal(ctx); err != nil {
		return Placement{}, err
	}
	key, err := m.ContainerKey(ctx, taskID)
	if err != nil {
		return Placement{}, err
	}
	if hostLocal {
		key = "host-local-" + key
	}
	ov, err := m.ResolveTaskOverrides(ctx, taskID)
	if err != nil {
		return Placement{}, err
	}
	cwd := ov["cwd"]
	if cwd == "" {
		cwd, _ = m.SessionCwd(ctx, taskID)
	}
	if cwd == "" {
		cwd = cfg.Cwd
	}
	cwd = m.settings.CoerceSSHRemoteCwd(ctx, cwd, kind)
	hostCwd, err := m.resolveTaskHostCwd(ctx, cfg, taskID)
	if err != nil {
		return Placement{}, err
	}
	if m.settings.IsContainerBackend(ctx, kind) && IsUnusableContainerCwd(cwd, hostCwd) {
		remapped := cfg.Cwd
		if hostCwd != "" {
			remapped = "/workspace"
		}
		if cwd != remapped {
			slog.Info(fmt.Sprintf("Remapping host/relative cwd override %s for %s backend (won't exist in sandbox). Using %s instead.", quotedPath(cwd), kind, quotedPath(remapped)))
		}
		cwd = remapped
	}
	return Placement{Config: cfg, Kind: kind, Key: key, Image: selectImage(kind, ov, cfg), Cwd: cwd, HostCwd: hostCwd}, nil
}
