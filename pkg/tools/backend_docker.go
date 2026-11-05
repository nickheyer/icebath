package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

var dockerSearchPaths = []string{"/usr/local/bin/docker", "/opt/homebrew/bin/docker", "/Applications/Docker.app/Contents/Resources/bin/docker"}

const (
	environmentLabelKey = "icebath-environment"
	workspaceLabelKey   = "icebath-workspace"
	egressLabelKey      = "icebath-egress"
	containerCA         = "/etc/ssl/certs/icebath-egress-ca.crt"
	nodeOptionsSentinel = "_ICEBATH_EGRESS_NODE_OPTIONS_APPEND"
	defaultPidsLimit    = "2048"
	defaultShmSize      = "1g"
	handbackRecord      = ".icebath-owner-handback"
	chownTimeout        = 10 * time.Second
)

var workspaceTaskRe = regexp.MustCompile(`:ws:([0-9a-f]{16})$`)
var labelValueBadRe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)
var volumeSpecRe = regexp.MustCompile(`^(.+):(/[^:]+)(?::[^:]*)?$`)
var hostCwdFallbackMounts = []string{"/host-cwd", "/host-cwd-2", "/host-cwd-3"}
var s6InitEntrypoints = map[string]bool{"/init": true, "/package/admin/s6-overlay/command/init": true}
var caModeFlags = map[string]bool{"--use-openssl-ca": true, "--use-bundled-ca": true}
var proxyControlEnv = map[string]bool{"HTTPS_PROXY": true, "https_proxy": true, "HTTP_PROXY": true, "http_proxy": true, "NO_PROXY": true, "no_proxy": true, "REQUESTS_CA_BUNDLE": true, "SSL_CERT_FILE": true, "CURL_CA_BUNDLE": true, "NODE_EXTRA_CA_CERTS": true}

var baseSecurityArgs = []string{"--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--cap-add", "CHOWN", "--cap-add", "FOWNER", "--tmpfs", "/tmp:rw,nosuid,size=512m", "--tmpfs", "/var/tmp:rw,noexec,nosuid,size=256m"}
var noNewPrivilegesArgs = []string{"--security-opt", "no-new-privileges"}
var runTmpfsNoexec = []string{"--tmpfs", "/run:rw,noexec,nosuid,size=64m"}
var runTmpfsExec = []string{"--tmpfs", "/run:rw,exec,nosuid,size=64m"}
var privdropCapArgs = []string{"--cap-add", "SETUID", "--cap-add", "SETGID"}

// EgressMapping is one provider credential the egress proxy swaps for a proxy token
type EgressMapping struct {
	RealEnvName   string
	ProxyToken    string
	AliasEnvNames []string
}

// EgressStatus is the live state of the host side egress proxy
type EgressStatus struct {
	Configured bool
	Pid        int
	Listening  bool
	TunnelPort int
	CACertPath string
}

// EgressProxySource reports the egress proxy state and token mappings, installed by the egress package
type EgressProxySource interface {
	Status(env *Env) EgressStatus
	Mappings(env *Env) []EgressMapping
}

// EgressProxy is the installed egress proxy source, nil meaning the proxy is unavailable
var EgressProxy EgressProxySource

var dockerExecutable struct {
	sync.Mutex
	path string
}

func isExecutableFile(p string) bool {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return st.Mode().Perm()&0o111 != 0
}

// FindDocker locates the docker or podman CLI once per process
func FindDocker(env *Env) string {
	dockerExecutable.Lock()
	defer dockerExecutable.Unlock()
	if dockerExecutable.path != "" {
		return dockerExecutable.path
	}
	found := ""
	if override := os.Getenv("ICEBATH_DOCKER_BINARY"); override != "" && isExecutableFile(override) {
		envLog(env).Info("Using ICEBATH_DOCKER_BINARY override", "path", override)
		found = override
	} else if p, err := exec.LookPath("docker"); err == nil {
		found = p
	} else if p, err := exec.LookPath("podman"); err == nil {
		envLog(env).Info("Using podman as container runtime", "path", p)
		found = p
	} else {
		for _, p := range dockerSearchPaths {
			if isExecutableFile(p) {
				envLog(env).Info("Found docker at non-PATH location", "path", p)
				found = p
				break
			}
		}
	}
	dockerExecutable.path = found
	return found
}

// DockerRuntimeName is the user facing runtime name for the CLI at a path
func DockerRuntimeName(executable string) string {
	if strings.Contains(strings.ToLower(filepath.Base(executable)), "podman") {
		return "Podman"
	}
	return "Docker"
}

// DockerRuntimeStartHint says how to bring the runtime back up
func DockerRuntimeStartHint(executable string) string {
	if DockerRuntimeName(executable) != "Podman" {
		return "start Docker and retry"
	}
	return "run `podman machine start` and retry"
}

// Lossy coercion into a docker label safe value
func sanitizeLabelValue(value string) string {
	if value == "" {
		return "unknown"
	}
	out := labelValueBadRe.ReplaceAllString(value, "_")
	if len(out) > 63 {
		out = out[:63]
	}
	if out == "" {
		return "unknown"
	}
	return out
}

// Profile label used for container reuse and orphan reaping
func dockerContainerIdentity(env *Env, sharedKey string) string {
	if sharedKey == "" {
		return sanitizeLabelValue(homeProfileName(env))
	}
	digest := sha256.Sum256([]byte(sharedKey))
	label := sanitizeLabelValue(sharedKey)
	if len(label) > 50 {
		label = label[:50]
	}
	return label + "-" + hex.EncodeToString(digest[:])[:12]
}

func splitVolumeSpec(spec string) (string, string, bool) {
	m := volumeSpecRe.FindStringSubmatch(strings.TrimSpace(spec))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// Reports whether a mount spec sources a per process temp directory
func isVolatileMountSpec(spec string) bool {
	if spec == "-v" || spec == "--mount" || !strings.Contains(spec, ":") {
		return false
	}
	source, _, ok := splitVolumeSpec(spec)
	if !ok {
		source = strings.SplitN(spec, ":", 2)[0]
	}
	if isWindowsDrivePath(source) {
		return false
	}
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(expandUser(source))
	if err != nil {
		return false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs == tempRoot || strings.HasPrefix(abs, tempRoot+string(filepath.Separator))
}

// Hash of the immutable configuration so reuse never attaches to stale mounts
func reuseEnvironmentFingerprint(image string, mountArgs []string, icebathHome string) string {
	normalizedHome, _ := filepath.Abs(expandUser(icebathHome))
	normalizedHome = normcaseHostPath(normalizedHome)
	canonical := make([]string, 0, len(mountArgs))
	for _, spec := range mountArgs {
		if isVolatileMountSpec(spec) {
			canonical = append(canonical, "<volatile-tempdir-mount>:"+strings.SplitN(spec, ":", 2)[1])
		} else {
			canonical = append(canonical, spec)
		}
	}
	payload, _ := json.Marshal(map[string]any{"icebath_home": normalizedHome, "image": image, "mount_args": canonical})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])[:24]
}

func normcaseHostPath(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(filepath.ToSlash(p))
	}
	return p
}

func dockerQuery(ctx context.Context, env *Env, argv []string, timeout time.Duration, fail string, nonzero string) *CaptureResult {
	res, err := runCapture(ctx, argv, timeout, false, nil)
	if err != nil {
		envLog(env).Debug(fail, "error", err)
		return nil
	}
	if nonzero != "" && res.Code != 0 {
		envLog(env).Debug(nonzero, "code", res.Code, "stderr", strings.TrimSpace(res.Stderr))
		return nil
	}
	return res
}

func containerFinishedAt(ctx context.Context, env *Env, docker, cid string) (time.Time, bool) {
	res := dockerQuery(ctx, env, []string{docker, "inspect", "--format", "{{.State.FinishedAt}}", cid}, 10*time.Second, "orphan reaper docker inspect failed", "")
	if res == nil || res.Code != 0 {
		return time.Time{}, false
	}
	raw := strings.TrimSpace(res.Stdout)
	if raw == "" || strings.HasPrefix(raw, "0001-01-01") {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		envLog(env).Debug("could not parse FinishedAt", "raw", raw, "error", err)
		return time.Time{}, false
	}
	return t, true
}

var workspaceDigestRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

func isIdleWorkspaceContainer(ctx context.Context, env *Env, docker, cid string) bool {
	res := dockerQuery(ctx, env, []string{docker, "inspect", "--format", `{{index .Config.Labels "` + workspaceLabelKey + `"}}`, cid}, 10*time.Second, "orphan reaper docker inspect failed", "")
	return res != nil && res.Code == 0 && workspaceDigestRe.MatchString(strings.TrimSpace(res.Stdout))
}

// Removes stale exited containers of the profile older than the age, returning the count removed
func dockerReapOrphanContainers(env *Env, maxAgeSeconds int, profileFilter string) (int, error) {
	ctx := context.Background()
	docker := FindDocker(env)
	if docker == "" {
		docker = "docker"
	}
	filters := []string{"--filter", "label=icebath-agent=1", "--filter", "status=exited"}
	if profileFilter != "" {
		filters = append(filters, "--filter", "label=icebath-profile="+sanitizeLabelValue(profileFilter))
	}
	argv := append([]string{docker, "ps", "-a"}, filters...)
	argv = append(argv, "--format", "{{.ID}}")
	listing := dockerQuery(ctx, env, argv, 15*time.Second, "orphan reaper docker ps failed", "orphan reaper docker ps returned nonzero")
	if listing == nil {
		return 0, nil
	}
	now := time.Now()
	removed := 0
	for _, line := range strings.Split(listing.Stdout, "\n") {
		cid := strings.TrimSpace(line)
		if cid == "" {
			continue
		}
		finished, ok := containerFinishedAt(ctx, env, docker, cid)
		if !ok {
			continue
		}
		age := now.Sub(finished).Seconds()
		if age < float64(maxAgeSeconds) {
			continue
		}
		stripSandboxPrivileges(ctx, env, docker, cid, nil)
		if isIdleWorkspaceContainer(ctx, env, docker, cid) {
			continue
		}
		res := dockerQuery(ctx, env, []string{docker, "rm", cid}, 30*time.Second, "orphan reaper docker rm failed", "")
		if res == nil {
			continue
		}
		if res.Code == 0 {
			removed++
			envLog(env).Info("Reaped orphan container", "id", cid[:min(12, len(cid))], "age_seconds", int(age))
		} else {
			envLog(env).Debug("docker rm failed", "id", cid[:min(12, len(cid))], "stderr", strings.TrimSpace(res.Stderr))
		}
	}
	return removed, nil
}

func extraArgsSetShmSize(extra []string) bool {
	for _, a := range extra {
		if a == "--shm-size" || strings.HasPrefix(a, "--shm-size=") {
			return true
		}
	}
	return false
}

// Network mode requested by docker_extra_args, ok false when no network flag is set
func extraArgsNetworkMode(extra []string) (string, bool) {
	for i, arg := range extra {
		for _, flag := range []string{"--network", "--net"} {
			if arg == flag {
				if i+1 < len(extra) {
					return extra[i+1], true
				}
				return "", true
			}
			if strings.HasPrefix(arg, flag+"=") {
				return strings.SplitN(arg, "=", 2)[1], true
			}
		}
	}
	return "", false
}

func buildSecurityArgs(runAsHostUser, runExec, snapCompat bool) []string {
	args := append([]string{}, baseSecurityArgs...)
	if !snapCompat {
		args = append(args, noNewPrivilegesArgs...)
	}
	if runExec {
		args = append(args, runTmpfsExec...)
	} else {
		args = append(args, runTmpfsNoexec...)
	}
	if !runAsHostUser {
		args = append(args, privdropCapArgs...)
	}
	return args
}

// Reports whether an image's entrypoint is the s6 overlay init
func imageUsesInitEntrypoint(ctx context.Context, env *Env, docker, image string) bool {
	res := dockerQuery(ctx, env, []string{docker, "image", "inspect", image, "--format", "{{json .Config.Entrypoint}}"}, 15*time.Second, "Docker: could not inspect entrypoint", "Docker: image inspect returned nonzero")
	if res == nil {
		return false
	}
	raw := strings.TrimSpace(res.Stdout)
	if raw == "" || raw == "null" {
		return false
	}
	var entrypoint any
	if err := json.Unmarshal([]byte(raw), &entrypoint); err != nil {
		return false
	}
	var first string
	switch v := entrypoint.(type) {
	case string:
		first = v
	case []any:
		if len(v) == 0 {
			return false
		}
		first = asStr(v[0])
	default:
		return false
	}
	return s6InitEntrypoints[strings.TrimSpace(first)]
}

var cgroupLimits struct {
	sync.Mutex
	known bool
	ok    bool
}

var storageOpt struct {
	sync.Mutex
	known bool
	ok    bool
}

// Probes once whether cgroup limit flags work here, caching only definitive answers
func cgroupLimitsAvailable(ctx context.Context, env *Env, image string) bool {
	cgroupLimits.Lock()
	if cgroupLimits.known {
		defer cgroupLimits.Unlock()
		return cgroupLimits.ok
	}
	cgroupLimits.Unlock()
	docker := FindDocker(env)
	if docker == "" || image == "" {
		return false
	}
	res, err := runCapture(ctx, []string{docker, "run", "--rm", "--cpus", "0.5", "--memory", "64m", "--pids-limit", "32", image, "sleep", "0"}, 60*time.Second, false, nil)
	if err != nil {
		envLog(env).Warn("Cgroup limit probe failed; containers run without CPU/memory/PID limits until a probe succeeds", "error", err)
		return false
	}
	cgroupLimits.Lock()
	defer cgroupLimits.Unlock()
	if res.Code == 0 {
		cgroupLimits.known, cgroupLimits.ok = true, true
		return true
	}
	stderr := strings.TrimSpace(res.Stderr)
	if !strings.Contains(strings.ToLower(stderr), "cgroup") {
		envLog(env).Warn("Cgroup limit probe could not determine support. Containers run without CPU/memory/PID limits until a probe succeeds.", "code", res.Code, "stderr", truncateRunes(stderr, 500))
		return false
	}
	cgroupLimits.known, cgroupLimits.ok = true, false
	envLog(env).Warn("Cgroup resource limits (--cpus/--memory/--pids-limit) not available in this environment. Containers will run without CPU, memory or PID limits. To enable, delegate the cpu, memory and pids cgroup controllers to this container.", "stderr", truncateRunes(stderr, 500))
	return false
}

func dockerUnavailable(env *Env, logMsg, errText, hint string) error {
	envLog(env).Error(logMsg)
	return NewEnvironmentConnectionError(errText, hint)
}

// Fails fast when the docker CLI or daemon is unusable
func ensureDockerAvailable(ctx context.Context, env *Env) error {
	docker := FindDocker(env)
	if docker == "" {
		return dockerUnavailable(env, "Docker backend selected but no docker executable was found in PATH or known install locations. Install Docker Desktop and ensure the CLI is available.",
			"Docker executable not found in PATH or known install locations. Install Docker and ensure the 'docker' command is available.",
			"Install Docker (or fix PATH) and retry, or run `icebath setup terminal` to switch to Local.")
	}
	res, err := runCapture(ctx, []string{docker, "version"}, 5*time.Second, false, nil)
	if err != nil {
		if errors.Is(err, ErrCaptureTimeout) {
			return dockerUnavailable(env, "Docker backend selected but '"+docker+" version' timed out. The Docker daemon may not be running.",
				"Docker daemon is not responding. Ensure Docker is running and try again.",
				"Start Docker (e.g. `systemctl start docker` or launch Docker Desktop), then retry — or run `icebath setup terminal` to switch to Local.")
		}
		return dockerUnavailable(env, "Docker backend selected but the resolved docker executable '"+docker+"' could not be executed.",
			"Docker executable could not be executed. Check your Docker installation.",
			"Repair the Docker installation and retry.")
	}
	if res.Code != 0 {
		return dockerUnavailable(env, fmt.Sprintf("Docker backend selected but '%s version' failed (exit code %d, stderr=%s)", docker, res.Code, strings.TrimSpace(res.Stderr)),
			"Docker command is available but 'docker version' failed. Check your Docker installation.",
			"Start Docker, or add your user to the docker group, then retry — or run `icebath setup terminal` to switch to Local.")
	}
	return nil
}

// Name only -e flags so values travel through the client env instead of the cmdline
func nameOnlyEnvArgs(names map[string]string) []string {
	keys := sortedNames(names)
	out := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		out = append(out, "-e", k)
	}
	return out
}

// Read only mount args for credential files, skill dirs, and cache dirs
func readonlySkillMountArgs(env *Env) []string {
	var args []string
	sources := []struct {
		mounts      []Mount
		expectsFile bool
		noun        string
	}{
		{GetCredentialFileMounts(env), true, "credential"},
		{GetSkillsDirectoryMount(env, DefaultContainerBase), false, "skills dir"},
		{GetCacheDirectoryMounts(env, DefaultContainerBase), false, "cache dir"},
	}
	for _, src := range sources {
		for _, m := range src.mounts {
			problem := ""
			if src.expectsFile {
				if isDir(m.HostPath) {
					problem = "source is a directory (likely Docker-in-Docker auto-creation)"
				} else if !isFile(m.HostPath) {
					problem = "source not found"
				}
			} else if !isDir(m.HostPath) {
				problem = "source is not a directory"
			}
			if problem != "" {
				envLog(env).Warn("Docker: skipping mount", "kind", strings.Fields(src.noun)[0], "problem", problem, "source", m.HostPath)
				continue
			}
			args = append(args, "-v", m.HostPath+":"+m.ContainerPath+":ro")
			envLog(env).Info("Docker: mounting", "kind", src.noun, "host", m.HostPath, "container", m.ContainerPath)
		}
	}
	return args
}

func resolveHostUserSpec() (string, bool) {
	if runtime.GOOS == "windows" {
		return "", false
	}
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), true
}

func hostUserArgs(env *Env, runAsHostUser bool) []string {
	if !runAsHostUser {
		return nil
	}
	if spec, ok := resolveHostUserSpec(); ok {
		envLog(env).Info("Docker: running container as host user", "user", spec)
		return []string{"--user", spec}
	}
	envLog(env).Warn("docker_run_as_host_user is enabled but this platform does not expose POSIX uid/gid; container will start as its image default user.")
	return nil
}

func containerMountTaken(volumeArgs []string, mount string) bool {
	target := strings.TrimRight(mount, "/")
	if target == "" {
		target = "/"
	}
	for _, arg := range volumeArgs {
		if _, c, ok := splitVolumeSpec(arg); ok {
			ct := strings.TrimRight(c, "/")
			if ct == "" {
				ct = "/"
			}
			if ct == target {
				return true
			}
		}
	}
	return false
}

func existingHostMount(volumeArgs []string, hostCwdAbs string) string {
	want := hostPathKey(hostCwdAbs)
	if want == "" {
		return ""
	}
	for _, arg := range volumeArgs {
		if h, c, ok := splitVolumeSpec(arg); ok && hostPathKey(h) == want {
			return c
		}
	}
	return ""
}

func freeHostCwdMount(volumeArgs []string) string {
	for _, c := range hostCwdFallbackMounts {
		if !containerMountTaken(volumeArgs, c) {
			return c
		}
	}
	return hostCwdFallbackMounts[len(hostCwdFallbackMounts)-1]
}

func absHostCwd(hostCwd string) string {
	expanded := expandUser(hostCwd)
	if isWindowsDrivePath(expanded) && runtime.GOOS != "windows" {
		return expanded
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return expanded
	}
	return abs
}

func normalizeForwardEnvNames(env *Env, raw any) []string {
	var out []string
	for _, item := range asList(raw) {
		s, ok := item.(string)
		if !ok {
			envLog(env).Warn("Ignoring non-string docker_forward_env entry", "entry", fmt.Sprint(item))
			continue
		}
		key := strings.TrimSpace(s)
		if key == "" {
			continue
		}
		if !shellEnvNameRe.MatchString(key) {
			envLog(env).Warn("Ignoring invalid docker_forward_env entry", "entry", s)
		} else if !inList(out, key) {
			out = append(out, key)
		}
	}
	return out
}

func normalizeEnvDict(env *Env, raw any) map[string]string {
	out := map[string]string{}
	if raw == nil {
		return out
	}
	m, ok := raw.(map[string]any)
	if !ok {
		envLog(env).Warn("docker_env is not a dict", "type", fmt.Sprintf("%T", raw))
		return out
	}
	for key, value := range m {
		k := strings.TrimSpace(key)
		if !shellEnvNameRe.MatchString(k) {
			envLog(env).Warn("Ignoring invalid docker_env key", "key", key)
			continue
		}
		switch v := value.(type) {
		case string:
			out[k] = v
		case bool:
			if v {
				out[k] = "True"
			} else {
				out[k] = "False"
			}
		case int, int64, float64, json.Number:
			out[k] = fmt.Sprint(v)
		default:
			envLog(env).Warn("Ignoring non-string docker_env value", "key", k, "type", fmt.Sprintf("%T", value))
		}
	}
	return out
}

// ---- egress plumbing ---------------------------------------------------------------------------

func egressProxyArgsForDocker(env *Env) ([]string, map[string]string, []string, error) {
	proxyCfg := cfgTree(env).Map("proxy")
	if !config.Truthy(proxyCfg["enabled"]) {
		return nil, map[string]string{}, nil, nil
	}
	enforce := true
	if v, ok := proxyCfg["enforce_on_docker"]; ok && v != nil {
		enforce = config.Truthy(v)
	}
	degraded := func(msg string) ([]string, map[string]string, []string, error) {
		if enforce {
			return nil, nil, nil, errors.New(msg)
		}
		envLog(env).Warn(msg + " — continuing without proxy (enforce_on_docker=false).")
		return nil, map[string]string{}, nil, nil
	}
	if EgressProxy == nil {
		return degraded("proxy.enabled is true but iron-proxy is not configured. Run `icebath egress setup` to mint tokens and write proxy.yaml.")
	}
	status := EgressProxy.Status(env)
	if !status.Configured {
		return degraded("proxy.enabled is true but iron-proxy is not configured. Run `icebath egress setup` to mint tokens and write proxy.yaml.")
	}
	if status.Pid == 0 || !status.Listening {
		return degraded(fmt.Sprintf("iron-proxy is enabled but not running on port %d. Start it with `icebath egress start`.", status.TunnelPort))
	}
	if status.CACertPath == "" || !isFile(status.CACertPath) {
		return degraded(fmt.Sprintf("iron-proxy CA cert vanished from %s. Re-run `icebath egress setup` to regenerate it.", status.CACertPath))
	}
	mappings := EgressProxy.Mappings(env)
	if len(mappings) == 0 {
		return degraded("iron-proxy is configured but mappings.json is empty or corrupt.  Re-run `icebath egress setup` to mint provider tokens before starting a sandbox.")
	}
	volumeArgs := []string{"-v", status.CACertPath + ":" + containerCA + ":ro"}
	proxyURL := fmt.Sprintf("http://host.docker.internal:%d", status.TunnelPort)
	plainURL := fmt.Sprintf("http://host.docker.internal:%d", status.TunnelPort+1)
	overrides := map[string]string{
		"HTTPS_PROXY": proxyURL, "https_proxy": proxyURL, "HTTP_PROXY": plainURL, "http_proxy": plainURL,
		"NO_PROXY": "127.0.0.1,localhost,::1", "no_proxy": "127.0.0.1,localhost,::1",
		"REQUESTS_CA_BUNDLE": containerCA, "SSL_CERT_FILE": containerCA, "CURL_CA_BUNDLE": containerCA, "NODE_EXTRA_CA_CERTS": containerCA,
		"ICEBATH_EGRESS_PROXY": "1", nodeOptionsSentinel: "--use-openssl-ca",
	}
	for _, m := range mappings {
		overrides[m.RealEnvName] = m.ProxyToken
		overrides["ICEBATH_PROXY_TOKEN_"+m.RealEnvName] = m.ProxyToken
		for _, alias := range m.AliasEnvNames {
			overrides[alias] = m.ProxyToken
		}
	}
	return volumeArgs, overrides, []string{"--add-host", "host.docker.internal:host-gateway"}, nil
}

func egressReuseFingerprint(volumeArgs []string, overrides map[string]string, hostArgs []string) string {
	if len(volumeArgs) == 0 && len(overrides) == 0 && len(hostArgs) == 0 {
		return "off"
	}
	payload, _ := json.Marshal(map[string]any{"env_overrides": overrides, "host_args": hostArgs, "volume_args": volumeArgs})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])[:24]
}

func egressEnforceOnDocker(env *Env) bool {
	proxyCfg := cfgTree(env).Map("proxy")
	if v, ok := proxyCfg["enforce_on_docker"]; ok && v != nil {
		return config.Truthy(v)
	}
	return true
}

func criticalEgressEnvNames(overrides map[string]string) map[string]bool {
	out := map[string]bool{"NODE_OPTIONS": true}
	for k := range proxyControlEnv {
		out[k] = true
	}
	for k := range overrides {
		out[k] = true
	}
	return out
}

var dockerRunBoolShorthands = "ditPq"

// Classifies an extra arg as a docker env or env-file flag with its inline value
func envFlagValue(arg string) (kind string, inline *string, ok bool) {
	if strings.HasPrefix(arg, "--") {
		flag, value, hasEq := strings.Cut(arg, "=")
		if flag == "--env" || flag == "--env-file" {
			if hasEq {
				return flag[2:], &value, true
			}
			return flag[2:], nil, true
		}
		return "", nil, false
	}
	if !strings.HasPrefix(arg, "-") {
		return "", nil, false
	}
	for pos, ch := range arg[1:] {
		if strings.ContainsRune(dockerRunBoolShorthands, ch) {
			continue
		}
		if ch == 'e' {
			rest := strings.TrimPrefix(arg[pos+2:], "=")
			if rest == "" {
				return "env", nil, true
			}
			return "env", &rest, true
		}
		return "", nil, false
	}
	return "", nil, false
}

func extraArgsEgressCollisions(extra []string, critical map[string]bool) []string {
	folded := map[string]bool{}
	for n := range critical {
		folded[strings.ToUpper(n)] = true
	}
	seen := map[string]bool{}
	for i := 0; i < len(extra); {
		arg := extra[i]
		if arg == "--" {
			break
		}
		kind, inline, ok := envFlagValue(arg)
		if ok {
			if kind == "env-file" {
				seen["--env-file"] = true
			} else {
				value := ""
				if inline != nil {
					value = *inline
				} else if i+1 < len(extra) {
					value = extra[i+1]
				}
				name := strings.SplitN(value, "=", 2)[0]
				if folded[strings.ToUpper(name)] {
					seen[name] = true
				}
			}
			if inline != nil {
				i++
			} else {
				i += 2
			}
			continue
		}
		if flag := strings.SplitN(arg, "=", 2)[0]; flag == "--network" || flag == "--net" {
			seen[arg] = true
		}
		i++
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func collisionGuard(env *Env, msg string, enforce bool, remedy, consequence string) error {
	state := "disabled"
	if enforce {
		state = "enabled"
	}
	msg = msg + "; enforce_on_docker is " + state + "."
	if enforce {
		return errors.New(msg + "  " + remedy)
	}
	envLog(env).Warn(msg + "  " + consequence)
	return nil
}

func checkForwardEnvCollisions(env *Env, forward []string, critical map[string]bool, enforce bool) error {
	folded := map[string]bool{}
	for k := range critical {
		folded[strings.ToUpper(k)] = true
	}
	var collisions []string
	for _, k := range forward {
		if folded[strings.ToUpper(k)] {
			collisions = append(collisions, k)
		}
	}
	if len(collisions) == 0 {
		return nil
	}
	sort.Strings(collisions)
	return collisionGuard(env, fmt.Sprintf("docker_forward_env would inject real egress-protected variables %s", pyList(collisions)), enforce,
		"Remove these names from docker_forward_env or disable enforce_on_docker to opt out of egress isolation.",
		"Explicit docker_forward_env values will override egress tokens.")
}

func checkDockerEnvCollisions(env *Env, userEnv, egressEnv map[string]string, enforce bool) error {
	providerKeys := map[string]bool{}
	if EgressProxy != nil {
		for _, m := range EgressProxy.Mappings(env) {
			providerKeys[m.RealEnvName] = true
		}
	}
	collides := func(k string) bool {
		uv, ok := userEnv[k]
		if !ok {
			return false
		}
		ev, inEgress := egressEnv[k]
		if providerKeys[k] {
			return !inEgress || uv != ev
		}
		return inEgress && uv != ev
	}
	var collisions []string
	for k := range proxyControlEnv {
		if collides(k) {
			collisions = append(collisions, k)
		}
	}
	for k := range providerKeys {
		if !proxyControlEnv[k] && collides(k) {
			collisions = append(collisions, k)
		}
	}
	if len(collisions) == 0 {
		return nil
	}
	sort.Strings(collisions)
	return collisionGuard(env, fmt.Sprintf("docker_env in config.yaml overrides egress-proxy variables %s", pyList(collisions)), enforce,
		"Remove these keys from docker_env or disable enforce_on_docker to opt out of egress isolation.",
		"Falling back to docker_env values; sandbox traffic will NOT route through the proxy.")
}

func checkExtraArgsCollisions(env *Env, extra []string, critical map[string]bool, enforce bool) error {
	collisions := extraArgsEgressCollisions(extra, critical)
	if len(collisions) == 0 {
		return nil
	}
	return collisionGuard(env, fmt.Sprintf("docker_extra_args would override egress-proxy controls %s", pyList(collisions)), enforce,
		"Remove these args or disable enforce_on_docker to opt out of egress isolation.",
		"Extra Docker args may bypass egress isolation.")
}

// Renders a string list the way Python prints one
func pyList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = pyRepr(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// Merges docker_env with the egress overrides and resolves the NODE_OPTIONS sentinel
func mergeEgressEnv(env *Env, userEnv, egressEnv map[string]string, enforce bool) map[string]string {
	merged := map[string]string{}
	if enforce && len(egressEnv) > 0 {
		for k, v := range userEnv {
			merged[k] = v
		}
		for k, v := range egressEnv {
			merged[k] = v
		}
	} else {
		for k, v := range egressEnv {
			merged[k] = v
		}
		for k, v := range userEnv {
			merged[k] = v
		}
	}
	rawAppend, ok := merged[nodeOptionsSentinel]
	delete(merged, nodeOptionsSentinel)
	if !ok || rawAppend == "" {
		return merged
	}
	appendToken := strings.TrimSpace(rawAppend)
	tokens := strings.Fields(merged["NODE_OPTIONS"])
	if caModeFlags[appendToken] {
		var dropped, kept []string
		for _, t := range tokens {
			if caModeFlags[t] && t != appendToken {
				dropped = append(dropped, t)
				continue
			}
			kept = append(kept, t)
		}
		if len(dropped) > 0 {
			envLog(env).Warn("Overriding conflicting NODE_OPTIONS CA-mode flag(s) with the egress-required flag to keep Node routed through the egress CA store.", "dropped", pyList(dropped), "required", appendToken)
		}
		tokens = kept
	}
	if !inList(tokens, appendToken) {
		tokens = append(tokens, appendToken)
	}
	joined := strings.TrimSpace(strings.Join(tokens, " "))
	if joined == "" {
		delete(merged, "NODE_OPTIONS")
	} else {
		merged["NODE_OPTIONS"] = joined
	}
	return merged
}

// ---- ownership handback ------------------------------------------------------------------------

var safeEnvArgs = []string{"-e", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "-e", "LD_PRELOAD=", "-e", "LD_LIBRARY_PATH=", "-w", "/"}
var throwawayConfinement = []string{"--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "DAC_OVERRIDE", "--cap-add", "FOWNER", "--security-opt", "no-new-privileges", "--pids-limit", "64", "--label", "icebath-handback=1"}

var remappingCache = struct {
	sync.Mutex
	byExe map[string]bool
}{byExe: map[string]bool{}}

var stripWarned = struct {
	sync.Mutex
	dirs map[string]bool
}{dirs: map[string]bool{}}

func daemonRemapsUsers(ctx context.Context, env *Env, docker string) bool {
	remappingCache.Lock()
	defer remappingCache.Unlock()
	if v, ok := remappingCache.byExe[docker]; ok {
		return v
	}
	options := ""
	res, err := runCapture(ctx, []string{docker, "info", "--format", "{{.SecurityOptions}}"}, 10*time.Second, false, nil)
	if err != nil {
		envLog(env).Debug("docker info for ownership handback failed", "error", err)
	} else if res.Code == 0 {
		options = strings.ToLower(res.Stdout)
	}
	v := strings.TrimSpace(options) == "" || strings.Contains(options, "rootless") || strings.Contains(options, "userns")
	remappingCache.byExe[docker] = v
	return v
}

func handbackIDs(ctx context.Context, env *Env, docker string) (int, int, bool) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		return 0, 0, false
	}
	if daemonRemapsUsers(ctx, env, docker) {
		return 0, 0, false
	}
	return os.Geteuid(), os.Getegid(), true
}

func recordPath(mounts [][2]string) string {
	if len(mounts) == 0 {
		return ""
	}
	return filepath.Join(filepath.Dir(mounts[0][0]), handbackRecord)
}

func dockerInspect(ctx context.Context, env *Env, docker, cid string) map[string]any {
	res, err := runCapture(ctx, []string{docker, "inspect", cid}, 10*time.Second, false, nil)
	if err != nil || res.Code != 0 {
		envLog(env).Debug("docker inspect failed", "id", cid[:min(12, len(cid))])
		return nil
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &list); err != nil || len(list) == 0 {
		return nil
	}
	return list[0]
}

func containerRunsAsRoot(info map[string]any) bool {
	user := asStr(asMap(info["Config"])["User"])
	first := strings.SplitN(user, ":", 2)[0]
	return first == "" || first == "root" || first == "0"
}

func containerMaySetFileCaps(info map[string]any) bool {
	host := asMap(info["HostConfig"])
	if truthy(host["Privileged"]) {
		return true
	}
	for _, c := range asList(host["CapAdd"]) {
		switch strings.TrimPrefix(strings.ToUpper(asStr(c)), "CAP_") {
		case "SETFCAP", "ALL":
			return true
		}
	}
	return false
}

func realpathOr(p, fallback string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return fallback
	}
	return r
}

func sandboxBinds(env *Env, info map[string]any) [][2]string {
	root := home.Root()
	roots := map[string]bool{realpathOr(filepath.Join(root, "sandboxes", "docker"), filepath.Join(root, "sandboxes", "docker")): true}
	for _, name := range home.ListProfiles(root) {
		p := filepath.Join(home.ProfileHome(root, name), "sandboxes", "docker")
		roots[realpathOr(p, p)] = true
	}
	if sd, err := SandboxDir(env); err == nil {
		p := filepath.Join(sd, "docker")
		roots[realpathOr(p, p)] = true
	}
	var binds [][2]string
	for _, m := range asList(info["Mounts"]) {
		mm := asMap(m)
		dst := asStr(mm["Destination"])
		if asStr(mm["Type"]) != "bind" || (dst != "/root" && dst != "/workspace") {
			continue
		}
		src := asStr(mm["Source"])
		if src == "" {
			src = "/nonexistent"
		}
		src = realpathOr(src, src)
		if isDir(src) && roots[filepath.Dir(filepath.Dir(src))] {
			binds = append(binds, [2]string{src, dst})
		}
	}
	return binds
}

func ownedFindTail(owner int) []string {
	return []string{"(", "-type", "f", "-o", "-type", "d", "-o", "-type", "l", ")", "-user", itoa(owner), "-print0"}
}

var findEscapeRe = regexp.MustCompile(`([\[\]*?\\])`)

// Builds the find argv applying tail to every entry under the sandbox mounts
func walkArgv(mounts [][2]string, mountPoints []string, tail []string) []string {
	targets := make([]string, 0, len(mounts))
	for _, m := range mounts {
		targets = append(targets, m[1])
	}
	pointSet := map[string]bool{}
	for _, p := range mountPoints {
		pointSet[strings.TrimRight(p, "/")] = true
	}
	points := make([]string, 0, len(pointSet))
	for p := range pointSet {
		points = append(points, p)
	}
	sort.Strings(points)
	var prune []string
	for _, point := range points {
		nested := false
		for _, t := range targets {
			if strings.HasPrefix(point, strings.TrimRight(t, "/")+"/") {
				nested = true
				break
			}
		}
		if !nested {
			continue
		}
		if len(prune) > 0 {
			prune = append(prune, "-o")
		}
		prune = append(prune, "-path", findEscapeRe.ReplaceAllString(point, `\$1`))
	}
	argv := append([]string{"/usr/bin/find", "-P"}, targets...)
	argv = append(argv, "-xdev")
	if len(prune) > 0 {
		argv = append(argv, "(")
		argv = append(argv, prune...)
		argv = append(argv, ")", "-prune", "-o")
	}
	return append(argv, tail...)
}

func mountPointsOf(info map[string]any) []string {
	var out []string
	for _, m := range asList(info["Mounts"]) {
		out = append(out, asStr(asMap(m)["Destination"]))
	}
	return out
}

func dockerIn(ctx context.Context, env *Env, docker string, target, argv []string, deadline time.Time, input []byte) (*CaptureResult, error) {
	remaining := time.Until(deadline)
	if remaining < time.Second {
		remaining = time.Second
	}
	full := append([]string{docker}, target...)
	full = append(full, argv...)
	runCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	cmd := exec.CommandContext(runCtx, full[0], full[1:]...)
	if input != nil {
		cmd.Stdin = strings.NewReader(string(input))
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	hideConsoleWindow(cmd)
	err := cmd.Run()
	if runCtx.Err() != nil {
		for i, t := range target {
			if t == "--name" && i+1 < len(target) {
				_, _ = runCapture(context.Background(), []string{docker, "rm", "-f", target[i+1]}, 10*time.Second, false, nil)
			}
		}
		return nil, ErrCaptureTimeout
	}
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, err
		}
		code = ee.ExitCode()
	}
	return &CaptureResult{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

func ownershipWalk(ctx context.Context, env *Env, docker string, target []string, mounts [][2]string, info map[string]any, owner int, deadline time.Time) ([]string, bool) {
	res, err := dockerIn(ctx, env, docker, target, walkArgv(mounts, mountPointsOf(info), ownedFindTail(owner)), deadline, nil)
	if err != nil || res.Code != 0 {
		if res != nil {
			envLog(env).Debug("ownership walk exited nonzero", "code", res.Code, "stderr", truncateRunes(strings.TrimSpace(res.Stderr), 500))
		}
		return nil, false
	}
	var paths []string
	for _, p := range strings.Split(res.Stdout, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, true
}

func ownershipChown(ctx context.Context, env *Env, docker string, target []string, paths []string, owner string, deadline time.Time) bool {
	var input strings.Builder
	for _, p := range paths {
		input.WriteString(p)
		input.WriteByte(0)
	}
	res, err := dockerIn(ctx, env, docker, target, []string{"/usr/bin/xargs", "-0", "-r", "/bin/chown", "-h", owner, "--"}, deadline, []byte(input.String()))
	if err != nil || res.Code != 0 {
		envLog(env).Debug("ownership chown failed", "owner", owner)
		return false
	}
	return true
}

func throwawayTarget(cid string, info map[string]any, volumes []string) []string {
	target := []string{"run", "--rm", "-i", "--pull", "never", "--network", "none", "--user", "0:0"}
	target = append(target, throwawayConfinement...)
	target = append(target, safeEnvArgs...)
	target = append(target, "--name", fmt.Sprintf("icebath-handback-%s-%s", cid[:min(12, len(cid))], strings.ReplaceAll(uuid.NewString(), "-", "")[:8]))
	target = append(target, volumes...)
	return append(target, "--entrypoint", "", asStr(info["Image"]))
}

const sameOwnerChown = `uids=$("$@" -type f -exec stat -c %u -- {} +) || exit; for u in $(printf "%s\n" "$uids" | sort -u); do "$@" -type f -user "$u" -exec chown -h "$u" {} + || exit; done`

// Strips setuid and setgid bits and file capabilities from a root container's sandbox binds
func stripSandboxPrivileges(ctx context.Context, env *Env, docker, cid string, info map[string]any) {
	if info == nil {
		if _, _, ok := handbackIDs(ctx, env, docker); ok {
			info = dockerInspect(ctx, env, docker, cid)
		}
	}
	if info == nil || !containerRunsAsRoot(info) {
		return
	}
	mounts := sandboxBinds(env, info)
	if len(mounts) == 0 {
		return
	}
	var volumes []string
	for _, m := range mounts {
		volumes = append(volumes, "--mount", "type=bind,src="+m[0]+",dst="+m[1])
	}
	target := throwawayTarget(cid, info, volumes)
	walk := append([]string{"find"}, walkArgv(mounts, mountPointsOf(info), nil)[1:]...)
	var argv []string
	if containerMaySetFileCaps(info) {
		argv = append([]string{"/bin/sh", "-c", sameOwnerChown, "sh"}, walk...)
	} else {
		argv = append(walk, "-type", "f", "-perm", "/6000", "-exec", "chmod", "ug-s", "{}", "+")
	}
	errText := ""
	res, err := dockerIn(ctx, env, docker, target, argv, time.Now().Add(chownTimeout), nil)
	if err != nil {
		errText = err.Error()
	} else if res.Code != 0 {
		errText = fmt.Sprintf("exit %d: %s", res.Code, truncateRunes(res.Stderr, 300))
	}
	if errText == "" {
		return
	}
	where := filepath.Dir(mounts[0][0])
	stripWarned.Lock()
	warned := stripWarned.dirs[where]
	stripWarned.dirs[where] = true
	stripWarned.Unlock()
	msg := "Could not strip setuid/setgid bits and file capabilities from Docker sandbox; files there may still run with elevated rights on the host"
	if warned {
		envLog(env).Debug(msg, "sandbox", where, "error", strings.TrimSpace(errText))
	} else {
		envLog(env).Warn(msg, "sandbox", where, "error", strings.TrimSpace(errText))
	}
}

// Gives a stopped root container's sandbox files to the host user and records which
func handBackSandboxFiles(ctx context.Context, env *Env, docker, cid string, mounts [][2]string) {
	record := recordPath(mounts)
	if record == "" {
		return
	}
	uid, gid, ok := handbackIDs(ctx, env, docker)
	if !ok {
		return
	}
	info := dockerInspect(ctx, env, docker, cid)
	if info == nil || !containerRunsAsRoot(info) {
		return
	}
	target := throwawayTarget(cid, info, []string{"--volumes-from", cid})
	deadline := time.Now().Add(chownTimeout)
	paths, walked := ownershipWalk(ctx, env, docker, target, mounts, info, 0, deadline)
	if walked && len(paths) > 0 {
		known := map[string]bool{}
		if data, err := os.ReadFile(record); err == nil {
			for _, p := range strings.Split(string(data), "\x00") {
				known[p] = true
			}
		}
		f, err := os.OpenFile(record, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			for _, p := range paths {
				if !known[p] {
					_, _ = f.WriteString(p + "\x00")
				}
			}
			_ = f.Close()
		}
		ownershipChown(ctx, env, docker, target, paths, fmt.Sprintf("%d:%d", uid, gid), deadline)
	}
	stripSandboxPrivileges(ctx, env, docker, cid, info)
}

// Returns recorded handed back paths to root inside a fresh root container, then drops the record
func reclaimSandboxFiles(ctx context.Context, env *Env, docker, cid string, mounts [][2]string) {
	record := recordPath(mounts)
	if record == "" || !isFile(record) {
		return
	}
	var info map[string]any
	if runtime.GOOS == "linux" {
		info = dockerInspect(ctx, env, docker, cid)
		if info == nil {
			return
		}
	}
	if info == nil || !containerRunsAsRoot(info) {
		_ = os.Remove(record)
		return
	}
	target := append([]string{"exec", "-i", "-u", "0:0"}, safeEnvArgs...)
	target = append(target, cid)
	deadline := time.Now().Add(chownTimeout)
	data, err := os.ReadFile(record)
	if err != nil {
		return
	}
	recorded := map[string]bool{}
	for _, p := range strings.Split(string(data), "\x00") {
		recorded[p] = true
	}
	owned, walked := ownershipWalk(ctx, env, docker, target, mounts, info, os.Geteuid(), deadline)
	if !walked {
		return
	}
	var paths []string
	for _, p := range owned {
		if recorded[p] {
			paths = append(paths, p)
		}
	}
	if len(paths) > 0 && !ownershipChown(ctx, env, docker, target, paths, "0:0", deadline) {
		return
	}
	_ = os.Remove(record)
}

// ---- teardown workers --------------------------------------------------------------------------

var teardownWorkers = struct {
	sync.Mutex
	pending map[chan struct{}]bool
}{pending: map[chan struct{}]bool{}}

func startTeardown(work func()) chan struct{} {
	done := make(chan struct{})
	teardownWorkers.Lock()
	teardownWorkers.pending[done] = true
	teardownWorkers.Unlock()
	go func() {
		defer func() {
			close(done)
			teardownWorkers.Lock()
			delete(teardownWorkers.pending, done)
			teardownWorkers.Unlock()
		}()
		work()
	}()
	return done
}

// WaitForAllBackendTeardowns joins every in flight teardown worker within the timeout
func WaitForAllBackendTeardowns(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		teardownWorkers.Lock()
		var first chan struct{}
		for ch := range teardownWorkers.pending {
			first = ch
			break
		}
		teardownWorkers.Unlock()
		if first == nil {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		select {
		case <-first:
		case <-time.After(remaining):
			return false
		}
	}
}

// ---- environment --------------------------------------------------------------------------------

// dockerEnv runs commands inside a hardened docker container
type dockerEnv struct {
	baseEnv
	dockerExe           string
	image               string
	imagePinned         bool
	s6Init              bool
	snapCompat          bool
	persistAcross       bool
	taskID              string
	forwardEnv          []string
	userEnv             map[string]string
	initUnsetNames      map[string]bool
	containerID         string
	initEnvValues       map[string]string
	initEnvArgs         []string
	runEnvValues        map[string]string
	workspaceDir        string
	homeDir             string
	sandboxMounts       [][2]string
	labels              map[string]string
	allRunArgs          []string
	cleanupDone         chan struct{}
	persistentFS        bool
	profileScopedForNet bool
}

func (d *dockerEnv) EnvType() string { return "docker" }

func (d *dockerEnv) additionalProfileScopedPassthroughNames() []string {
	return append([]string{}, d.forwardEnv...)
}

// DockerOptions are the resolved docker kwargs for one environment
type DockerOptions struct {
	Image                string
	Cwd                  string
	Timeout              int
	CPU                  float64
	Memory               int
	Disk                 int
	PersistentFilesystem bool
	TaskID               string
	Volumes              []string
	ForwardEnv           []string
	UserEnv              map[string]string
	Network              bool
	HostCwd              string
	AutoMountCwd         bool
	RunAsHostUser        bool
	ExtraArgs            []string
	PersistAcrossProcs   bool
	ShmSize              string
	SharedContainerKey   string
	SnapCompat           bool
	ImagePinned          bool
	SessionScoped        bool
}

func dockerStringList(env *Env, raw any, label string) []string {
	var out []string
	for _, v := range asList(raw) {
		s, ok := v.(string)
		if !ok {
			envLog(env).Warn("Ignoring non-string "+label+" entry", "entry", fmt.Sprint(v))
			continue
		}
		out = append(out, s)
	}
	return out
}

// Builds docker options from the shaped container config
func dockerOptionsFromConfig(env *Env, opts CreateOptions) DockerOptions {
	cc := opts.ContainerConfig
	toF := func(v any) float64 { return asFloat(v) }
	o := DockerOptions{
		Image: opts.Image, Cwd: opts.Cwd, Timeout: opts.Timeout, TaskID: opts.TaskID, HostCwd: opts.HostCwd,
		CPU: toF(cc["cpu"]), Memory: int(toF(cc["memory"])), Disk: int(toF(cc["disk"])),
		PersistentFilesystem: truthy(cc["persistent_filesystem"]),
		Volumes:              dockerStringList(env, cc["volumes"], "docker_volumes"),
		ForwardEnv:           normalizeForwardEnvNames(env, cc["forward_env"]),
		UserEnv:              normalizeEnvDict(env, cc["env"]),
		Network:              true, AutoMountCwd: truthy(cc["auto_mount_cwd"]), RunAsHostUser: truthy(cc["run_as_host_user"]),
		ExtraArgs:          dockerStringList(env, cc["extra_args"], "docker_extra_args"),
		PersistAcrossProcs: true, ShmSize: defaultShmSize, SharedContainerKey: asStr(cc["shared_container_key"]),
		SnapCompat: truthy(cc["snap_compat"]), ImagePinned: truthy(cc["image_pinned"]), SessionScoped: truthy(cc["session_scoped"]),
	}
	if v, ok := cc["network"]; ok && v != nil {
		o.Network = truthy(v)
	}
	if v, ok := cc["persist_across_processes"]; ok && v != nil {
		o.PersistAcrossProcs = truthy(v)
	}
	if v, ok := cc["shm_size"]; ok && v != nil {
		o.ShmSize = asStr(v)
	}
	if o.TaskID == "" {
		o.TaskID = "default"
	}
	if o.Timeout <= 0 {
		o.Timeout = 60
	}
	return o
}

// NewDockerEnvironment creates or reattaches the container and seeds the shell snapshot
func NewDockerEnvironment(ctx context.Context, env *Env, o DockerOptions) (*dockerEnv, error) {
	cwd := o.Cwd
	if cwd == "~" || cwd == "" {
		cwd = "/root"
	}
	d := &dockerEnv{image: o.Image, imagePinned: o.ImagePinned, snapCompat: o.SnapCompat, persistAcross: o.PersistAcrossProcs,
		taskID: o.TaskID, forwardEnv: o.ForwardEnv, userEnv: o.UserEnv, persistentFS: o.PersistentFilesystem}
	d.passwordlessSudoProbeSupported = true
	d.profileScopedPassthrough = true
	d.stdinMode = "pipe"
	initBaseEnv(&d.baseEnv, d, env, cwd, o.Timeout, nil, true)
	d.persistent = o.PersistentFilesystem
	d.sessionScoped = o.SessionScoped
	d.initUnsetNames = map[string]bool{}
	d.initEnvValues = map[string]string{}
	d.runEnvValues = map[string]string{}
	envLog(env).Info("DockerEnvironment volumes", "volumes", pyList(o.Volumes))
	if err := ensureDockerAvailable(ctx, env); err != nil {
		return nil, err
	}
	resourceArgs, err := d.resourceArgs(ctx, o)
	if err != nil {
		return nil, err
	}
	volumeArgs, writableArgs, err := d.mountArgs(env, o)
	if err != nil {
		return nil, err
	}
	if d.hostCwdMount != "" && cwdFollowsHostMount(cwd, d.hostCwdMount) {
		envLog(env).Info("Container cwd follows configured host workspace", "mount", d.hostCwdMount, "requested", cwd)
		cwd = d.hostCwdMount
		d.SetCwd(cwd)
	}
	volumeArgs = append(volumeArgs, readonlySkillMountArgs(env)...)
	egressLabel, egressVolumeArgs, egressHostArgs, envArgs, validatedExtra, err := d.egressAndEnvArgs(env, o.ExtraArgs)
	if err != nil {
		return nil, err
	}
	volumeArgs = append(volumeArgs, egressVolumeArgs...)
	userArgs := hostUserArgs(env, o.RunAsHostUser)
	d.dockerExe = FindDocker(env)
	if d.dockerExe == "" {
		d.dockerExe = "docker"
	}
	d.s6Init = imageUsesInitEntrypoint(ctx, env, d.dockerExe, o.Image)
	if d.s6Init {
		envLog(env).Info("Docker: image uses /init (s6-overlay) as entrypoint — skipping --init and mounting /run with exec.", "image", o.Image)
	}
	securityArgs := buildSecurityArgs(o.RunAsHostUser && len(userArgs) > 0, d.s6Init, o.SnapCompat)
	if o.SnapCompat {
		envLog(env).Warn("docker_snap_compat: running without --init and no-new-privileges (snap Docker under AppArmor)")
	}
	envLog(env).Info("Docker volume_args", "args", pyList(volumeArgs))
	all := append([]string{}, securityArgs...)
	all = append(all, userArgs...)
	all = append(all, writableArgs...)
	all = append(all, resourceArgs...)
	all = append(all, egressHostArgs...)
	all = append(all, volumeArgs...)
	all = append(all, envArgs...)
	all = append(all, validatedExtra...)
	envLog(env).Info("Docker run_args", "args", pyList(all))
	profileName := dockerContainerIdentity(env, o.SharedContainerKey)
	taskLabel := sanitizeLabelValue(o.TaskID)
	d.labels = map[string]string{"icebath-agent": "1", "icebath-task-id": taskLabel, "icebath-profile": profileName, egressLabelKey: egressLabel}
	if m := workspaceTaskRe.FindStringSubmatch(o.TaskID); m != nil {
		d.labels[workspaceLabelKey] = m[1]
	}
	if o.SharedContainerKey == "" {
		d.labels[environmentLabelKey] = reuseEnvironmentFingerprint(o.Image, append(append([]string{}, writableArgs...), volumeArgs...), env.Home)
	}
	d.allRunArgs = all
	reused := o.PersistAcrossProcs && d.attachExistingContainer(ctx, taskLabel, profileName, egressLabel, o.Network)
	if !reused {
		cid, err := d.dockerRun(ctx, cwd)
		if err != nil {
			return nil, err
		}
		d.containerID = cid
	} else {
		stripSandboxPrivileges(ctx, env, d.dockerExe, d.containerID, nil)
	}
	reclaimSandboxFiles(ctx, env, d.dockerExe, d.containerID, d.sandboxMounts)
	if err := d.buildInitEnvArgs(); err != nil {
		return nil, err
	}
	d.initSession(ctx)
	return d, nil
}

func (d *dockerEnv) egressAndEnvArgs(env *Env, extraArgs []string) (string, []string, []string, []string, []string, error) {
	egressVolumeArgs, overrides, egressHostArgs, err := egressProxyArgsForDocker(env)
	if err != nil {
		return "", nil, nil, nil, nil, err
	}
	label := egressReuseFingerprint(egressVolumeArgs, overrides, egressHostArgs)
	enforce := true
	if len(overrides) > 0 {
		enforce = egressEnforceOnDocker(env)
	}
	critical := criticalEgressEnvNames(overrides)
	if len(overrides) > 0 {
		if err := checkForwardEnvCollisions(env, d.forwardEnv, critical, enforce); err != nil {
			return "", nil, nil, nil, nil, err
		}
		if err := checkDockerEnvCollisions(env, d.userEnv, overrides, enforce); err != nil {
			return "", nil, nil, nil, nil, err
		}
	}
	merged := mergeEgressEnv(env, d.userEnv, overrides, enforce)
	d.runEnvValues = merged
	validated := append([]string{}, extraArgs...)
	if len(overrides) > 0 {
		if err := checkExtraArgsCollisions(env, validated, critical, enforce); err != nil {
			return "", nil, nil, nil, nil, err
		}
	}
	return label, egressVolumeArgs, egressHostArgs, nameOnlyEnvArgs(merged), validated, nil
}

func (d *dockerEnv) resourceArgs(ctx context.Context, o DockerOptions) ([]string, error) {
	var args []string
	if cgroupLimitsAvailable(ctx, d.env, o.Image) {
		if o.CPU > 0 {
			args = append(args, "--cpus", pyFloat(o.CPU))
		}
		if o.Memory > 0 {
			args = append(args, "--memory", fmt.Sprintf("%dm", o.Memory))
		}
		args = append(args, "--pids-limit", defaultPidsLimit)
	}
	shm := strings.TrimSpace(o.ShmSize)
	if shm != "" && shm != "0" && !extraArgsSetShmSize(o.ExtraArgs) {
		args = append(args, "--shm-size", shm)
	}
	if o.Disk > 0 && runtime.GOOS != "darwin" {
		if d.storageOptSupported(ctx) {
			args = append(args, "--storage-opt", fmt.Sprintf("size=%dm", o.Disk))
		} else {
			envLog(d.env).Warn("Docker storage driver does not support per-container disk limits (requires overlay2 on XFS with pquota). Container will run without disk quota.")
		}
	}
	if !o.Network {
		mode, set := extraArgsNetworkMode(o.ExtraArgs)
		if !set {
			args = append(args, "--network=none")
		} else if mode != "none" {
			return nil, fmt.Errorf("terminal.docker_network is false (air-gapped) but terminal.docker_extra_args requests --network=%s. Remove the --network entry from docker_extra_args, or set docker_network: true to opt into that network.", pyRepr(mode))
		}
	}
	return args, nil
}

// Formats a float the way Python str does for the docker cpus flag
func pyFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%.1f", f)
	}
	return fmt.Sprint(f)
}

func (d *dockerEnv) mountArgs(env *Env, o DockerOptions) ([]string, []string, error) {
	var volumeArgs []string
	for _, vol := range o.Volumes {
		vol = strings.TrimSpace(vol)
		if vol == "" {
			continue
		}
		if !strings.Contains(vol, ":") {
			envLog(env).Warn("Docker volume missing colon, skipping", "volume", vol)
			continue
		}
		volumeArgs = append(volumeArgs, "-v", vol)
	}
	workspaceExplicit := false
	for _, v := range volumeArgs {
		if strings.Contains(v, ":/workspace") {
			workspaceExplicit = true
		}
	}
	hostCwdAbs := ""
	if o.HostCwd != "" {
		hostCwdAbs = absHostCwd(o.HostCwd)
	}
	windowsCwd := isWindowsDrivePath(o.HostCwd) || isWindowsDrivePath(hostCwdAbs)
	hostDirExists := hostCwdAbs != "" && isDir(hostCwdAbs)
	shouldBind := hostDirExists && (o.AutoMountCwd || windowsCwd)
	if (o.AutoMountCwd || windowsCwd) && o.HostCwd != "" && !hostDirExists {
		envLog(env).Debug("Skipping docker cwd mount: host_cwd is not a valid directory", "host_cwd", o.HostCwd)
	}
	existing := ""
	if shouldBind {
		existing = existingHostMount(volumeArgs, hostCwdAbs)
	}
	bindTarget := ""
	switch {
	case existing != "":
		d.hostCwd, d.hostCwdMount = hostCwdAbs, existing
	case shouldBind:
		if workspaceExplicit {
			bindTarget = freeHostCwdMount(volumeArgs)
		} else {
			bindTarget = "/workspace"
		}
		d.hostCwd, d.hostCwdMount = hostCwdAbs, bindTarget
	default:
		d.hostCwd, d.hostCwdMount = "", ""
	}
	mountWorkspace := bindTarget != "/workspace" && !workspaceExplicit
	var writable []string
	if d.persistentFS {
		sd, err := SandboxDir(env)
		if err != nil {
			return nil, nil, err
		}
		sandbox := filepath.Join(sd, "docker", SanitizeTaskIDForPath(o.TaskID))
		d.homeDir = filepath.Join(sandbox, "home")
		if err := os.MkdirAll(d.homeDir, 0o755); err != nil {
			return nil, nil, err
		}
		d.sandboxMounts = [][2]string{{d.homeDir, "/root"}}
		if mountWorkspace {
			d.workspaceDir = filepath.Join(sandbox, "workspace")
			if err := os.MkdirAll(d.workspaceDir, 0o755); err != nil {
				return nil, nil, err
			}
			d.sandboxMounts = append(d.sandboxMounts, [2]string{d.workspaceDir, "/workspace"})
		}
		for _, m := range d.sandboxMounts {
			writable = append(writable, "-v", m[0]+":"+m[1])
		}
	} else {
		if mountWorkspace {
			writable = append(writable, "--tmpfs", "/workspace:rw,exec,size=10g")
		}
		writable = append(writable, "--tmpfs", "/home:rw,exec,size=1g", "--tmpfs", "/root:rw,exec,size=1g")
	}
	if bindTarget != "" {
		envLog(env).Info("Mounting configured host cwd", "target", bindTarget, "host", hostCwdAbs)
		volumeArgs = append([]string{"-v", hostCwdAbs + ":" + bindTarget}, volumeArgs...)
	} else if workspaceExplicit && existing == "" {
		envLog(env).Debug("Skipping docker cwd mount: /workspace already mounted by user config")
	}
	return volumeArgs, writable, nil
}

func (d *dockerEnv) attachExistingContainer(ctx context.Context, taskLabel, profileName, egressLabel string, network bool) bool {
	cid, state, ok := d.findReusableContainer(ctx, taskLabel, profileName, egressLabel)
	if !ok {
		return false
	}
	short := cid[:min(12, len(cid))]
	if actual, ok := d.containerImage(ctx, cid); ok && actual != d.image {
		switch {
		case !d.imagePinned:
			envLog(d.env).Warn("Existing container runs another image; the default docker_image changed. Keeping the existing sandbox — approve the switch with `icebath config set terminal.docker_image <image>` (files in /root and /workspace carry over) or pin the current image to stop this notice.",
				"container", short, "running", actual, "configured", d.image, "task", taskLabel, "profile", profileName)
		case !d.imageAvailableLocally(ctx):
			envLog(d.env).Warn("Existing container runs another image; docker_image could not be pulled — keeping the current sandbox until it can.",
				"container", short, "running", actual, "configured", d.image, "task", taskLabel, "profile", profileName)
		default:
			envLog(d.env).Warn("Existing container runs another image than docker_image — removing it and starting fresh.",
				"container", short, "running", actual, "configured", d.image, "task", taskLabel, "profile", profileName)
			if _, err := runCapture(ctx, []string{d.dockerExe, "rm", "-f", cid}, 30*time.Second, false, nil); err != nil {
				envLog(d.env).Warn("Failed to remove mismatched container", "container", short, "error", err)
			}
			return false
		}
	}
	if !network {
		mode, ok := d.containerNetworkMode(ctx, cid)
		if !ok || mode != "none" {
			if !ok {
				mode = "unknown"
			}
			envLog(d.env).Warn("Existing container NetworkMode mismatches docker_network=false air-gap — removing it and starting fresh.", "container", short, "mode", mode, "task", taskLabel, "profile", profileName)
			if _, err := runCapture(ctx, []string{d.dockerExe, "rm", "-f", cid}, 30*time.Second, false, nil); err != nil {
				envLog(d.env).Warn("Failed to remove mismatched container", "container", short, "error", err)
			}
			return false
		}
	}
	if state != "running" {
		if err := d.startContainer(ctx, cid); err != nil {
			envLog(d.env).Warn("Failed to start existing container — falling back to a fresh container.", "container", short, "state", state, "error", err)
			return false
		}
	}
	d.containerID = cid
	envLog(d.env).Info("Reusing container", "container", short, "task", taskLabel, "profile", profileName, "prior_state", state)
	return true
}

func (d *dockerEnv) imageAvailableLocally(ctx context.Context) bool {
	probe, err := runCapture(ctx, []string{d.dockerExe, "image", "inspect", d.image, "--format", "{{.Id}}"}, 30*time.Second, false, nil)
	if err == nil && probe.Code == 0 {
		return true
	}
	pull, err := runCapture(ctx, []string{d.dockerExe, "pull", d.image}, 900*time.Second, false, nil)
	if err != nil {
		envLog(d.env).Warn("Docker: could not pull image", "image", d.image, "error", err)
		return false
	}
	return pull.Code == 0
}

func (d *dockerEnv) startContainer(ctx context.Context, cid string) error {
	_, err := runCapture(ctx, []string{d.dockerExe, "start", cid}, 30*time.Second, true, nil)
	return err
}

func (d *dockerEnv) runCommand(name, workdir string) []string {
	argv := []string{d.dockerExe, "run", "-d"}
	if !d.s6Init && !d.snapCompat {
		argv = append(argv, "--init")
	}
	argv = append(argv, "--name", name)
	keys := make([]string, 0, len(d.labels))
	for k := range d.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "--label", k+"="+d.labels[k])
	}
	argv = append(argv, "-w", workdir)
	argv = append(argv, d.allRunArgs...)
	return append(argv, d.image, "sleep", "infinity")
}

func (d *dockerEnv) dockerRun(ctx context.Context, cwd string) (string, error) {
	name := "icebath-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	argv := d.runCommand(name, cwd)
	envLog(d.env).Debug("Starting container", "argv", shJoin(argv))
	res, err := runCapture(ctx, argv, 120*time.Second, true, clientEnvWith(d.runEnvValues))
	if err != nil {
		envLog(d.env).Warn("docker run failed, cleaning up orphaned container", "name", name, "error", err)
		_, _ = runCapture(context.Background(), []string{d.dockerExe, "rm", "-f", name}, 10*time.Second, false, nil)
		return "", err
	}
	cid := strings.TrimSpace(res.Stdout)
	envLog(d.env).Info("Started container", "name", name, "id", cid[:min(12, len(cid))])
	return cid, nil
}

func (d *dockerEnv) buildInitEnvArgs() error {
	passthrough, unset, err := resolvePassthroughEnv(d.env, d.forwardEnv)
	if err != nil {
		return err
	}
	execEnv := map[string]string{}
	for k, v := range d.userEnv {
		execEnv[k] = v
	}
	for k, v := range passthrough {
		execEnv[k] = v
	}
	for name := range unset {
		delete(execEnv, name)
	}
	d.initUnsetNames = unset
	d.initEnvValues = execEnv
	d.initEnvArgs = nameOnlyEnvArgs(execEnv)
	return nil
}

func (d *dockerEnv) runtimeEnvArgsWithUnsets() ([]string, map[string]bool, map[string]string, error) {
	passthrough, unset, err := resolvePassthroughEnv(d.env, d.forwardEnv)
	if err != nil {
		return nil, nil, nil, err
	}
	return nameOnlyEnvArgs(passthrough), unset, passthrough, nil
}

// Spawns bash inside the container with profile scoped values re injected per command
func (d *dockerEnv) runBash(ctx context.Context, cmd string, login bool, timeout int, stdinData *string) (ProcessHandle, error) {
	if d.containerID == "" {
		return nil, errors.New("Container not started")
	}
	argv := []string{d.dockerExe, "exec"}
	if stdinData != nil {
		argv = append(argv, "-i")
	}
	unset := map[string]bool{}
	envValues := map[string]string{}
	if login {
		argv = append(argv, d.initEnvArgs...)
		envValues = d.initEnvValues
		unset = d.initUnsetNames
	} else if d.profileScopedPassthrough {
		runtimeArgs, u, values, err := d.runtimeEnvArgsWithUnsets()
		if err != nil {
			return nil, err
		}
		argv = append(argv, runtimeArgs...)
		unset, envValues = u, values
	}
	argv = append(argv, d.containerID)
	argv = append(argv, bashArgv(prependUnset(cmd, unset), login)...)
	clientEnv := clientEnvWith(envValues)
	return popenBash(argv, stdinData, func(c *exec.Cmd) {
		if clientEnv != nil {
			c.Env = envSlice(clientEnv)
		}
	})
}

var noContainerPatterns = []string{"No such container", "is not running", "no such container"}

func (d *dockerEnv) isContainerGone(output string) bool {
	for _, p := range noContainerPatterns {
		if strings.Contains(output, p) {
			return true
		}
	}
	return false
}

func (d *dockerEnv) recreateContainer(ctx context.Context) bool {
	old := d.containerID
	envLog(d.env).Warn("Container appears to be gone — attempting recovery", "container", old[:min(12, len(old))])
	d.containerID = ""
	if cid, state, ok := d.findReusableContainer(ctx, d.labels["icebath-task-id"], d.labels["icebath-profile"], d.labels[egressLabelKey]); ok {
		short := cid[:min(12, len(cid))]
		if state == "running" {
			d.containerID = cid
			envLog(d.env).Info("Recovery: reusing running container", "container", short)
		} else if err := d.startContainer(ctx, cid); err == nil {
			d.containerID = cid
			envLog(d.env).Info("Recovery: restarted container", "container", short)
		} else {
			envLog(d.env).Warn("Recovery: failed to start container", "container", short, "error", err)
		}
	}
	if d.containerID == "" {
		if d.image == "" {
			envLog(d.env).Error("Recovery: no saved image name, cannot recreate container")
			return false
		}
		name := "icebath-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
		res, err := runCapture(ctx, d.runCommand(name, d.Cwd()), 120*time.Second, true, clientEnvWith(d.runEnvValues))
		if err != nil {
			envLog(d.env).Error("Recovery: failed to create new container", "error", err)
			return false
		}
		d.containerID = strings.TrimSpace(res.Stdout)
		envLog(d.env).Info("Recovery: created fresh container", "name", name, "container", d.containerID[:min(12, len(d.containerID))])
	}
	d.snapshotReady = false
	d.initSession(ctx)
	envLog(d.env).Info("Recovery successful — new container", "container", d.containerID[:min(12, len(d.containerID))])
	d.MarkRecreated()
	return true
}

// Execute runs a command, recreating a container removed out of band and retrying once
func (d *dockerEnv) Execute(ctx context.Context, command string, opts ExecOptions) (*ExecResult, error) {
	result, err := d.baseEnv.Execute(ctx, command, opts)
	if err != nil {
		return result, err
	}
	if result.ReturnCode != 0 && d.isContainerGone(result.Output) && d.persistAcross && d.recreateContainer(ctx) {
		return d.baseEnv.Execute(ctx, command, opts)
	}
	return result, nil
}

func (d *dockerEnv) storageOptSupported(ctx context.Context) bool {
	storageOpt.Lock()
	if storageOpt.known {
		defer storageOpt.Unlock()
		return storageOpt.ok
	}
	storageOpt.Unlock()
	docker := FindDocker(d.env)
	if docker == "" {
		docker = "docker"
	}
	res, err := runCapture(ctx, []string{docker, "info", "--format", "{{.Driver}}"}, 10*time.Second, false, nil)
	if err != nil || res.Code != 0 {
		return false
	}
	storageOpt.Lock()
	defer storageOpt.Unlock()
	if strings.ToLower(strings.TrimSpace(res.Stdout)) != "overlay2" {
		storageOpt.known, storageOpt.ok = true, false
		return false
	}
	probe, err := runCapture(ctx, []string{docker, "create", "--storage-opt", "size=1m", "hello-world"}, 15*time.Second, false, nil)
	if err != nil {
		return false
	}
	if probe.Code == 0 {
		storageOpt.known, storageOpt.ok = true, true
		if id := strings.TrimSpace(probe.Stdout); id != "" {
			_, _ = runCapture(ctx, []string{docker, "rm", id}, 5*time.Second, false, nil)
		}
	} else if strings.Contains(strings.ToLower(probe.Stderr), "storage") {
		storageOpt.known, storageOpt.ok = true, false
	}
	envLog(d.env).Debug("Docker --storage-opt support", "supported", storageOpt.ok)
	return storageOpt.ok
}

func (d *dockerEnv) containerImage(ctx context.Context, cid string) (string, bool) {
	res := dockerQuery(ctx, d.env, []string{d.dockerExe, "inspect", "--format", "{{.Config.Image}}", cid}, 10*time.Second, "docker inspect Image failed", "docker inspect Image returned nonzero")
	if res == nil {
		return "", false
	}
	s := strings.TrimSpace(res.Stdout)
	return s, s != ""
}

func (d *dockerEnv) containerNetworkMode(ctx context.Context, cid string) (string, bool) {
	res := dockerQuery(ctx, d.env, []string{d.dockerExe, "inspect", "--format", "{{.HostConfig.NetworkMode}}", cid}, 10*time.Second, "docker inspect NetworkMode failed", "docker inspect NetworkMode returned nonzero")
	if res == nil {
		return "", false
	}
	s := strings.TrimSpace(res.Stdout)
	return s, s != ""
}

func (d *dockerEnv) findReusableContainer(ctx context.Context, taskLabel, profileLabel, egressLabel string) (string, string, bool) {
	filters := []string{"--filter", "label=icebath-agent=1", "--filter", "label=icebath-task-id=" + taskLabel, "--filter", "label=icebath-profile=" + profileLabel, "--filter", "label=" + egressLabelKey + "=" + egressLabel}
	if envLabel := d.labels[environmentLabelKey]; envLabel != "" {
		filters = append(filters, "--filter", "label="+environmentLabelKey+"="+envLabel)
	}
	argv := append([]string{d.dockerExe, "ps", "-a"}, filters...)
	argv = append(argv, "--format", "{{.ID}}\t{{.State}}")
	res := dockerQuery(ctx, d.env, argv, 10*time.Second, "docker ps probe failed — will start a fresh container", "docker ps probe returned nonzero — will start a fresh container")
	if res == nil {
		return "", "", false
	}
	var firstID, firstState, runningID string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		cid, state := parts[0], strings.ToLower(strings.TrimSpace(parts[1]))
		if firstID == "" {
			firstID, firstState = cid, state
		}
		if state == "running" && runningID == "" {
			runningID = cid
		}
	}
	if runningID != "" {
		return runningID, "running", true
	}
	if firstID != "" {
		return firstID, firstState, true
	}
	return "", "", false
}

func (d *dockerEnv) removeBindDirs() {
	for _, dir := range []string{d.workspaceDir, d.homeDir} {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
	}
}

// Cleanup leaves a persistent container running or stops and removes it on a teardown worker
func (d *dockerEnv) Cleanup(forceRemove bool) error {
	cid := d.containerID
	if cid == "" {
		if !d.persistentFS {
			d.removeBindDirs()
		}
		return nil
	}
	docker := d.dockerExe
	env := d.env
	short := cid[:min(12, len(cid))]
	mounts := append([][2]string{}, d.sandboxMounts...)
	if !forceRemove && d.persistAcross {
		d.containerID = ""
		d.cleanupDone = startTeardown(func() { stripSandboxPrivileges(context.Background(), env, docker, cid, nil) })
		return nil
	}
	d.cleanupDone = startTeardown(func() {
		ctx := context.Background()
		for _, step := range []struct {
			argv []string
			fail string
		}{{[]string{"stop", "-t", "10"}, "docker stop timed out / failed"}, {[]string{"rm", "-f"}, "docker rm -f failed"}} {
			if step.argv[0] == "rm" && len(mounts) > 0 {
				handBackSandboxFiles(ctx, env, docker, cid, mounts)
			}
			argv := append([]string{docker}, step.argv...)
			argv = append(argv, cid)
			if _, err := runCapture(ctx, argv, 30*time.Second, false, nil); err != nil {
				envLog(env).Warn(step.fail, "container", short, "error", err)
			}
		}
	})
	d.containerID = ""
	if !d.persistentFS {
		d.removeBindDirs()
	}
	return nil
}

// StopIdle stops a per workspace persistent container without removing it
func (d *dockerEnv) StopIdle() error {
	cid := d.containerID
	if cid == "" {
		return nil
	}
	docker, env := d.dockerExe, d.env
	short := cid[:min(12, len(cid))]
	d.cleanupDone = startTeardown(func() {
		ctx := context.Background()
		if _, err := runCapture(ctx, []string{docker, "stop", "-t", "10", cid}, 30*time.Second, false, nil); err != nil {
			envLog(env).Warn("docker stop (idle workspace) failed", "container", short, "error", err)
		}
		stripSandboxPrivileges(ctx, env, docker, cid, nil)
	})
	d.containerID = ""
	return nil
}

// WaitForCleanup blocks for the teardown worker of this environment
func (d *dockerEnv) WaitForCleanup(timeout time.Duration) {
	if d.cleanupDone == nil {
		return
	}
	select {
	case <-d.cleanupDone:
	case <-time.After(timeout):
	}
}

// ExecPrefix is the local argv that runs its remainder inside the container
func (d *dockerEnv) ExecPrefix(user string, interactive bool) ([]string, error) {
	if d.containerID == "" {
		return nil, errors.New("Container not started")
	}
	argv := []string{d.dockerExe, "exec"}
	if interactive {
		argv = append(argv, "-i")
	}
	if user != "" {
		argv = append(argv, "-u", user)
	}
	return append(argv, d.containerID), nil
}

// ContainerID exposes the live container for sibling tools
func (d *dockerEnv) ContainerID() string { return d.containerID }

// ---- provider -----------------------------------------------------------------------------------

type dockerBackend struct{}

func (dockerBackend) Name() string        { return "docker" }
func (dockerBackend) DisplayName() string { return "Docker" }
func (dockerBackend) Description() string {
	return "Run commands in a hardened Docker container with persistent /root and /workspace sandboxes"
}
func (dockerBackend) EnvDescription() string                 { return "a Docker container (Linux)" }
func (dockerBackend) IsRemote() bool                         { return true }
func (dockerBackend) IsContainer() bool                      { return true }
func (dockerBackend) SkipContainerGuards() bool              { return false }
func (dockerBackend) CachePathBase() string                  { return DefaultContainerBase }
func (dockerBackend) DefaultCwd() string                     { return "" }
func (dockerBackend) StripEnvKeys() []string                 { return nil }
func (dockerBackend) SessionIsolatedWhenNonpersistent() bool { return true }
func (dockerBackend) SupportsStreams() bool                  { return true }

// IsAvailable reports whether a docker or podman CLI can be found
func (dockerBackend) IsAvailable() bool { return FindDocker(nil) != "" }

// CheckRequirements probes the CLI and daemon with the hermes wording
func (dockerBackend) CheckRequirements(env *Env, cfg map[string]any) error {
	docker := FindDocker(env)
	if docker == "" {
		return errors.New("Docker is not installed — no docker executable in PATH or the usual install locations")
	}
	res, err := runCapture(context.Background(), []string{docker, "version"}, 5*time.Second, false, nil)
	if err != nil {
		return fmt.Errorf("Docker is installed but the Docker daemon is not running (`%s version` failed: %v)", docker, err)
	}
	if res.Code != 0 {
		return fmt.Errorf("Docker is installed but the Docker daemon is not running (`%s version` exited with code %d)", docker, res.Code)
	}
	return nil
}

// Probe reports daemon reachability for the picker
func (b dockerBackend) Probe() (string, string) {
	docker := FindDocker(nil)
	if docker == "" {
		return "needs_setup", "Docker or Podman is not installed."
	}
	res, err := runCapture(context.Background(), []string{docker, "version"}, 5*time.Second, false, nil)
	if err != nil || res.Code != 0 {
		runtimeName := DockerRuntimeName(docker)
		if runtimeName == "Docker" {
			return "unavailable", "Docker daemon not running; " + DockerRuntimeStartHint(docker)
		}
		return "unavailable", runtimeName + " not reachable; " + DockerRuntimeStartHint(docker)
	}
	return "ready", DockerRuntimeName(docker) + " at " + docker
}

// SetupInstructions explains how to make the docker backend usable
func (dockerBackend) SetupInstructions(env *Env) []string {
	return []string{
		"Install Docker Desktop or Podman and make sure the CLI is on PATH.",
		"Start the daemon (Docker Desktop, `systemctl start docker`, or `podman machine start`).",
		"Set terminal.docker_image in config.yaml to choose the sandbox image.",
	}
}

// DoctorChecks reports the CLI and daemon rows
func (dockerBackend) DoctorChecks() []DoctorCheck {
	docker := FindDocker(nil)
	if docker == "" {
		return []DoctorCheck{{OK: false, Label: "Docker or Podman not installed", Detail: "(needed for the 'docker' terminal backend)"}}
	}
	runtimeName := DockerRuntimeName(docker)
	res, err := runCapture(context.Background(), []string{docker, "version"}, 10*time.Second, false, nil)
	if err != nil || res.Code != 0 {
		label := runtimeName + " daemon not running"
		if runtimeName != "Docker" {
			label = runtimeName + " not reachable"
		}
		return []DoctorCheck{{OK: false, Label: label, Detail: "(needed for the 'docker' terminal backend)"}}
	}
	detail := "(daemon running)"
	if runtimeName != "Docker" {
		detail = "(reachable)"
	}
	return []DoctorCheck{{OK: true, Label: runtimeName, Detail: detail}}
}

// CreateEnvironment builds the container environment from the shaped config
func (dockerBackend) CreateEnvironment(ctx context.Context, env *Env, opts CreateOptions) (Environment, error) {
	return NewDockerEnvironment(ctx, env, dockerOptionsFromConfig(env, opts))
}

func init() {
	registerBuiltinBackend(dockerBackend{})
}
