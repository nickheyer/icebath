package env

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
)

const VercelSandboxDefaultCwd = "/vercel/sandbox"

type EnvConfig struct {
	Kind                         string
	ModalMode                    string
	DockerImage                  string
	DockerImagePinned            bool
	DockerForwardEnv             any
	ModalImage                   string
	DaytonaImage                 string
	VercelRuntime                string
	VercelImage                  string
	Cwd                          string
	HostCwd                      string
	DockerMountCwdToWorkspace    bool
	Timeout                      int
	LifetimeSeconds              int
	SSHHost                      string
	SSHUser                      string
	SSHPort                      int
	SSHKey                       string
	SSHPersistent                bool
	LocalPersistent              bool
	ContainerCPU                 float64
	ContainerMemory              int
	ContainerDisk                int
	ContainerPersistent          bool
	DockerVolumes                any
	DockerEnv                    any
	DockerRunAsHostUser          bool
	DockerSnapCompat             bool
	DockerNetwork                bool
	DockerExtraArgs              any
	DockerShmSize                string
	DockerPersistAcrossProcesses bool
	DockerSharedContainerKey     string
	DockerOrphanReaper           bool
}

func dockerVolumeUsesHostPath(v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	vol := strings.TrimSpace(s)
	if vol == "" {
		return false
	}
	for _, p := range []string{"/", "~", "./", "../"} {
		if strings.HasPrefix(vol, p) {
			return true
		}
	}
	return len(vol) >= 3 && vol[1] == ':' && (vol[2] == '/' || vol[2] == '\\')
}

func (c EnvConfig) DockerHostAccess() bool {
	if c.Kind != "docker" {
		return false
	}
	if c.HostCwd != "" && c.DockerMountCwdToWorkspace {
		return true
	}
	switch vols := c.DockerVolumes.(type) {
	case []any:
		for _, v := range vols {
			if dockerVolumeUsesHostPath(v) {
				return true
			}
		}
	case map[string]any:
		for k := range vols {
			if dockerVolumeUsesHostPath(k) {
				return true
			}
		}
	case string:
		return strings.ContainsAny(vols, "/~")
	}
	return false
}

var containerBackends = map[string]bool{"docker": true, "modal": true, "daytona": true, "vercel_sandbox": true}

func (s *Settings) pluginFlags(ctx context.Context, kind string) (ProviderFlags, bool) {
	if kind == "" || IsBuiltinBackend(kind) {
		return ProviderFlags{}, false
	}
	flags, ok, err := s.providers.Flags(ctx, kind, "")
	if err != nil || !ok {
		return ProviderFlags{}, false
	}
	return flags, true
}

func (s *Settings) IsContainerBackend(ctx context.Context, kind string) bool {
	if containerBackends[kind] {
		return true
	}
	flags, _ := s.pluginFlags(ctx, kind)
	return flags.Container
}

func (s *Settings) IsRemoteBackend(ctx context.Context, kind string) bool {
	if remoteBackends[kind] {
		return true
	}
	if kind == "" || kind == "local" {
		return false
	}
	flags, _ := s.pluginFlags(ctx, kind)
	return flags.Remote
}

var remoteBackends = map[string]bool{"docker": true, "modal": true, "daytona": true, "ssh": true, "managed_modal": true, "vercel_sandbox": true}

func (s *Settings) EnvConfig(ctx context.Context) (EnvConfig, error) {
	s.ensureBridged(ctx)
	var c EnvConfig
	var err error
	str := func(name, def string) string {
		if err != nil {
			return ""
		}
		var v string
		v, err = s.tenv(ctx, name, def)
		return v
	}
	flag := func(name, def string) bool {
		if err != nil {
			return false
		}
		var v bool
		v, err = s.tenvBool(ctx, name, def)
		return v
	}
	integer := func(name, def string) int {
		if err != nil {
			return 0
		}
		var v int
		v, err = s.parseInt(ctx, name, def)
		return v
	}
	payload := func(name, def string) any {
		if err != nil {
			return nil
		}
		var raw string
		if raw, err = s.rawSetting(ctx, name, def); err != nil {
			return nil
		}
		var v any
		if jerr := json.Unmarshal([]byte(raw), &v); jerr != nil {
			err = &InvalidValueError{Name: name, Raw: raw, Expected: "valid JSON"}
			return nil
		}
		return v
	}
	c.Kind = str("TERMINAL_ENV", "local")
	c.DockerMountCwdToWorkspace = flag("TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE", "false")
	if err != nil {
		return EnvConfig{}, err
	}
	c.ContainerCPU, c.ContainerMemory, c.ContainerDisk = 1, 5120, 51200
	if s.IsContainerBackend(ctx, c.Kind) {
		if c.ContainerCPU, err = s.parseFloat(ctx, "TERMINAL_CONTAINER_CPU", "1"); err != nil {
			return EnvConfig{}, err
		}
		c.ContainerMemory = integer("TERMINAL_CONTAINER_MEMORY", "5120")
		c.ContainerDisk = integer("TERMINAL_CONTAINER_DISK", "51200")
	}
	c.DockerForwardEnv, c.DockerVolumes, c.DockerEnv, c.DockerExtraArgs, c.DockerShmSize = []any{}, []any{}, map[string]any{}, []any{}, "1g"
	if c.Kind == "docker" {
		c.DockerForwardEnv = payload("TERMINAL_DOCKER_FORWARD_ENV", "[]")
		c.DockerVolumes = payload("TERMINAL_DOCKER_VOLUMES", "[]")
		c.DockerEnv = payload("TERMINAL_DOCKER_ENV", "{}")
		c.DockerExtraArgs = payload("TERMINAL_DOCKER_EXTRA_ARGS", "[]")
		c.DockerShmSize = str("TERMINAL_DOCKER_SHM_SIZE", "1g")
	}
	if err != nil {
		return EnvConfig{}, err
	}
	if c.Cwd, c.HostCwd, err = s.resolveConfigCwd(ctx, c.Kind, c.DockerMountCwdToWorkspace); err != nil {
		return EnvConfig{}, err
	}
	c.ModalMode = CoerceModalMode(str("TERMINAL_MODAL_MODE", "auto"))
	c.DockerImage = str("TERMINAL_DOCKER_IMAGE", config.DefaultSandboxImage)
	c.DockerImagePinned = str("TERMINAL_DOCKER_IMAGE_PINNED", "0") == "1"
	c.ModalImage = str("TERMINAL_MODAL_IMAGE", config.DefaultSandboxImage)
	c.DaytonaImage = str("TERMINAL_DAYTONA_IMAGE", config.DefaultSandboxImage)
	c.VercelRuntime = strings.TrimSpace(str("TERMINAL_VERCEL_RUNTIME", ""))
	c.VercelImage = strings.TrimSpace(str("TERMINAL_VERCEL_IMAGE", ""))
	c.Timeout = integer("TERMINAL_TIMEOUT", "180")
	c.LifetimeSeconds = integer("TERMINAL_LIFETIME_SECONDS", "300")
	c.SSHHost = str("TERMINAL_SSH_HOST", "")
	c.SSHUser = str("TERMINAL_SSH_USER", "")
	c.SSHPort = integer("TERMINAL_SSH_PORT", "22")
	c.SSHKey = str("TERMINAL_SSH_KEY", "")
	c.SSHPersistent = flag("TERMINAL_SSH_PERSISTENT", str("TERMINAL_PERSISTENT_SHELL", "true"))
	c.LocalPersistent = flag("TERMINAL_LOCAL_PERSISTENT", "true")
	c.ContainerPersistent = flag("TERMINAL_CONTAINER_PERSISTENT", "true")
	c.DockerRunAsHostUser = flag("TERMINAL_DOCKER_RUN_AS_HOST_USER", "false")
	c.DockerSnapCompat = flag("TERMINAL_DOCKER_SNAP_COMPAT", "false")
	c.DockerNetwork = flag("TERMINAL_DOCKER_NETWORK", "true")
	c.DockerPersistAcrossProcesses = flag("TERMINAL_DOCKER_PERSIST_ACROSS_PROCESSES", "true")
	c.DockerSharedContainerKey = strings.TrimSpace(str("TERMINAL_DOCKER_SHARED_CONTAINER_KEY", ""))
	c.DockerOrphanReaper = flag("TERMINAL_DOCKER_ORPHAN_REAPER", "true")
	if err != nil {
		return EnvConfig{}, err
	}
	return c, nil
}

func (s *Settings) backendDefaultCwd(ctx context.Context, kind string) (string, error) {
	switch kind {
	case "local":
		return s.safeGetwd(ctx)
	case "ssh":
		return "~", nil
	case "vercel_sandbox":
		return VercelSandboxDefaultCwd, nil
	}
	flags, _ := s.pluginFlags(ctx, kind)
	if d := flags.DefaultCwd; d == "~" || strings.HasPrefix(d, "/") {
		return d, nil
	}
	return "/root", nil
}

func isSSHRemoteTildeCwd(backend, cwd string) bool {
	return strings.ToLower(strings.TrimSpace(backend)) == "ssh" && (cwd == "~" || strings.HasPrefix(cwd, "~/"))
}

func hostAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}

func (s *Settings) resolveConfigCwd(ctx context.Context, kind string, mountDockerCwd bool) (string, string, error) {
	def, err := s.backendDefaultCwd(ctx, kind)
	if err != nil {
		return "", "", err
	}
	cwd, err := s.tenv(ctx, "TERMINAL_CWD", def)
	if err != nil {
		return "", "", err
	}
	if cwd != "" && cwd != def && !isSSHRemoteTildeCwd(kind, cwd) {
		cwd = home.ExpandUser(cwd)
	}
	hostCwd := ""
	switch {
	case kind == "docker" && mountDockerCwd:
		raw, err := s.tenv(ctx, "TERMINAL_CWD", "")
		if err != nil {
			return "", "", err
		}
		if raw == "" {
			if raw, err = s.safeGetwd(ctx); err != nil {
				return "", "", err
			}
		}
		candidate := hostAbs(home.ExpandUser(raw))
		if isHostCwd(candidate) || (filepath.IsAbs(candidate) && isDir(candidate) && !hasContainerPrefix(candidate)) {
			hostCwd, cwd = candidate, "/workspace"
		}
	case kind == "docker" && IsWindowsDrivePath(cwd):
		candidate := home.ExpandUser(cwd)
		if runtime.GOOS == "windows" {
			candidate = hostAbs(candidate)
		}
		if isDir(candidate) {
			hostCwd, cwd = candidate, candidate
		} else {
			slog.Info(fmt.Sprintf("Ignoring TERMINAL_CWD=%s for %s backend (host/relative path won't work in sandbox). Using %s instead.", quotedPath(cwd), kind, quotedPath(def)))
			cwd = def
		}
	case s.IsContainerBackend(ctx, kind) && cwd != "" && IsUnusableContainerCwd(cwd, "") && cwd != def:
		slog.Info(fmt.Sprintf("Ignoring TERMINAL_CWD=%s for %s backend (host/relative path won't work in sandbox). Using %s instead.", quotedPath(cwd), kind, quotedPath(def)))
		cwd = def
	}
	return cwd, hostCwd, nil
}

func hasContainerPrefix(p string) bool {
	return strings.HasPrefix(p, "/workspace") || strings.HasPrefix(p, "/root")
}

var windowsDriveRe = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

func isHostCwd(p string) bool {
	return strings.HasPrefix(p, "/Users/") || strings.HasPrefix(p, "/home/") || windowsDriveRe.MatchString(p)
}

func IsWindowsDrivePath(p string) bool {
	return p != "" && windowsDriveRe.MatchString(p)
}

func hostPathKey(p string) string {
	text := strings.ReplaceAll(p, "\\", "/")
	if len(text) >= 2 && text[1] == ':' {
		text = strings.ToLower(text[:1]) + text[1:]
	}
	if len(text) > 3 {
		text = strings.TrimRight(text, "/")
	}
	return text
}

func TranslateMountedHostPath(p, hostRoot, containerRoot string) (string, bool) {
	if p == "" || hostRoot == "" || containerRoot == "" {
		return "", false
	}
	key, root := hostPathKey(p), hostPathKey(hostRoot)
	if key == "" || root == "" {
		return "", false
	}
	mount := strings.TrimRight(containerRoot, "/")
	if mount == "" {
		mount = "/"
	}
	if key == root {
		return mount, true
	}
	prefix := root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	return mount + "/" + key[len(prefix):], true
}

func cwdFollowsHostMount(cwd, mount string) bool {
	if mount == "" || cwd == "" || cwd == mount {
		return false
	}
	if IsUnusableContainerCwd(cwd, "") {
		return true
	}
	return cwd == "/workspace" && mount != "/workspace"
}

func isMountedHostCwd(cwd, mountedHost string) bool {
	if cwd == "" || mountedHost == "" {
		return false
	}
	return filepath.Clean(hostAbs(home.ExpandUser(cwd))) == filepath.Clean(hostAbs(home.ExpandUser(mountedHost)))
}

func IsUnusableContainerCwd(cwd, mountedHost string) bool {
	if cwd == "" {
		return false
	}
	if isMountedHostCwd(cwd, mountedHost) {
		return true
	}
	return isHostCwd(cwd) || !hostIsAbs(cwd)
}

func hostIsAbs(p string) bool {
	if runtime.GOOS == "windows" {
		return filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\")
	}
	return filepath.IsAbs(p)
}

func (s *Settings) CoerceSSHRemoteCwd(ctx context.Context, cwd, kind string) string {
	if strings.ToLower(strings.TrimSpace(kind)) != "ssh" {
		return cwd
	}
	text := strings.TrimSpace(cwd)
	if text == "" {
		return "~"
	}
	if strings.HasPrefix(text, "~") {
		return text
	}
	sub := s.homes.SubprocessHome(ctx, nil)
	if sub == "" || !path.IsAbs(text) || path.Clean(sub) == path.Clean(home.RealHome(ctx, nil)) {
		return text
	}
	rel, ok := posixRel(path.Clean(sub), path.Clean(text))
	if !ok {
		return text
	}
	if rel == "." {
		return "~"
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return text
	}
	return "~/" + rel
}

func posixRel(base, target string) (string, bool) {
	if !path.IsAbs(base) || !path.IsAbs(target) {
		return "", false
	}
	split := func(p string) []string {
		if p == "/" {
			return nil
		}
		return strings.Split(strings.TrimPrefix(p, "/"), "/")
	}
	b, t := split(base), split(target)
	i := 0
	for i < len(b) && i < len(t) && b[i] == t[i] {
		i++
	}
	parts := make([]string, 0, len(b)-i+len(t)-i)
	for range b[i:] {
		parts = append(parts, "..")
	}
	parts = append(parts, t[i:]...)
	if len(parts) == 0 {
		return ".", true
	}
	return strings.Join(parts, "/"), true
}

func PluginSandboxHome(h string) (string, bool) {
	if h != "" && path.IsAbs(h) && h != "/" {
		return h, true
	}
	return "", false
}

func InSandboxHome(cwd, sandboxHome string) bool {
	h, ok := PluginSandboxHome(sandboxHome)
	if !ok || !path.IsAbs(cwd) {
		return false
	}
	h, cwd = path.Clean(h), path.Clean(cwd)
	return cwd == h || strings.HasPrefix(cwd, h+"/")
}

const defaultModalMode = "auto"

func CoerceModalMode(value string) string {
	mode := strings.ToLower(strings.TrimSpace(value))
	if mode == "" {
		mode = defaultModalMode
	}
	switch mode {
	case "auto", "direct", "managed":
		return mode
	}
	return defaultModalMode
}

type modalState struct {
	RequestedMode      string
	HasDirect          bool
	ManagedReady       bool
	ManagedModeBlocked bool
	Selected           string
}

func resolveModalState(mode string, hasDirect, managedReady, managedEnabled bool) modalState {
	requested := CoerceModalMode(mode)
	managedOK := managedEnabled && managedReady
	selected := ""
	switch requested {
	case "managed":
		if managedOK {
			selected = "managed"
		}
	case "direct":
		if hasDirect {
			selected = "direct"
		}
	default:
		if managedOK {
			selected = "managed"
		} else if hasDirect {
			selected = "direct"
		}
	}
	return modalState{RequestedMode: requested, HasDirect: hasDirect, ManagedReady: managedReady,
		ManagedModeBlocked: requested == "managed" && !managedEnabled, Selected: selected}
}
