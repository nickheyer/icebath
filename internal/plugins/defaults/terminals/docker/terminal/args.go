package terminal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	dockerconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/docker/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	pidsLimit = "2048"
	// Where /workspace's place goes when a volume of the settings takes /workspace itself
	hostCwdMounts = 3
)

// Every container drops all capabilities but those a root shell needs for its own files, and gets small private temp folders
var baseSecurity = []string{"--cap-drop", "ALL", "--cap-add", "DAC_OVERRIDE", "--cap-add", "CHOWN", "--cap-add", "FOWNER",
	"--tmpfs", "/tmp:rw,nosuid,size=512m", "--tmpfs", "/var/tmp:rw,noexec,nosuid,size=256m"}

// Security flags of a container: no new privileges unless snap Docker refuses them, /run executable for an s6 image, and the capabilities
// dropping privileges takes unless the container runs as the host user already
func securityArgs(asHostUser, runExec, snap bool) []string {
	args := append([]string{}, baseSecurity...)
	if !snap {
		args = append(args, "--security-opt", "no-new-privileges")
	}
	if runExec {
		args = append(args, "--tmpfs", "/run:rw,exec,nosuid,size=64m")
	} else {
		args = append(args, "--tmpfs", "/run:rw,noexec,nosuid,size=64m")
	}
	if !asHostUser {
		args = append(args, "--cap-add", "SETUID", "--cap-add", "SETGID")
	}
	return args
}

// --user of the host's uid and gid when the container runs as the host user
func hostUserArgs(asHostUser bool) []string {
	if !asHostUser {
		return nil
	}
	if runtime.GOOS == "windows" {
		slog.Warn(dockerconfig.RunAsHostUser + " is on but this platform does not expose POSIX uid/gid; the container starts as its image's default user.")
		return nil
	}
	spec := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	slog.Info("Docker: running container as host user", "user", spec)
	return []string{"--user", spec}
}

// Whether the extra flags set the shared memory size themselves
func setsShm(extra []string) bool {
	for _, arg := range extra {
		if arg == "--shm-size" || strings.HasPrefix(arg, "--shm-size=") {
			return true
		}
	}
	return false
}

// Network mode the extra flags ask for, false when they name none
func networkMode(extra []string) (string, bool) {
	for i, arg := range extra {
		for _, flag := range []string{"--network", "--net"} {
			if arg == flag {
				if i+1 < len(extra) {
					return extra[i+1], true
				}
				return "", true
			}
			if value, ok := strings.CutPrefix(arg, flag+"="); ok {
				return value, true
			}
		}
	}
	return "", false
}

// Answers of checks of what the daemon supports, kept once they are definite
type probe struct {
	known, ok bool
}

// CPU, memory and process limits where cgroups allow them, the shared memory size, the disk quota where the storage driver takes one,
// and no network for an air-gapped sandbox
func (r *registrant) resourceArgs(ctx context.Context, docker string, o *options) ([]string, error) {
	var args []string
	if r.cgroupLimits(ctx, docker, o.image) {
		if o.cpu > 0 {
			args = append(args, "--cpus", strconv.FormatFloat(o.cpu, 'f', -1, 64))
		}
		if o.memory > 0 {
			args = append(args, "--memory", fmt.Sprintf("%dm", o.memory))
		}
		args = append(args, "--pids-limit", pidsLimit)
	}
	if o.shm != "" && o.shm != "0" && !setsShm(o.extra) {
		args = append(args, "--shm-size", o.shm)
	}
	if o.disk > 0 && runtime.GOOS != "darwin" {
		if r.storageOpt(ctx, docker) {
			args = append(args, "--storage-opt", fmt.Sprintf("size=%dm", o.disk))
		} else {
			slog.Warn("Docker storage driver does not support per-container disk limits (requires overlay2 on XFS with pquota). Container will run without disk quota.")
		}
	}
	if !o.network {
		switch mode, set := networkMode(o.extra); {
		case !set:
			args = append(args, "--network=none")
		case mode != "none":
			return nil, fmt.Errorf("Network access is off on the Docker page of /sandbox, but Extra flags there ask for --network=%s. Remove "+
				"that flag from Extra flags, or turn on Network access to use that network.", strconv.Quote(mode))
		}
	}
	return args, nil
}

// Whether cgroup limit flags work here, probed until a probe gives a definite answer
func (r *registrant) cgroupLimits(ctx context.Context, docker, image string) bool {
	r.mu.Lock()
	known := r.cgroups
	r.mu.Unlock()
	if known.known {
		return known.ok
	}
	if docker == "" || image == "" {
		return false
	}
	captured, err := terminal.Capture(ctx, []string{docker, "run", "--rm", "--cpus", "0.5", "--memory", "64m", "--pids-limit", "32", image, "sleep", "0"}, 60*time.Second, nil)
	if err != nil {
		slog.Warn("Cgroup limit probe failed; containers run without CPU/memory/PID limits until a probe succeeds", "error", err)
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if captured.Code == 0 {
		r.cgroups = probe{known: true, ok: true}
		return true
	}
	stderr := strings.TrimSpace(captured.Stderr)
	if !strings.Contains(strings.ToLower(stderr), "cgroup") {
		slog.Warn("Cgroup limit probe could not determine support. Containers run without CPU/memory/PID limits until a probe succeeds.", "code", captured.Code,
			"stderr", strs.Head(stderr, 500))
		return false
	}
	r.cgroups = probe{known: true}
	slog.Warn("Cgroup resource limits (--cpus/--memory/--pids-limit) not available in this environment. Containers will run without CPU, memory or "+
		"PID limits. To enable, delegate the cpu, memory and pids cgroup controllers to this container.", "stderr", strs.Head(stderr, 500))
	return false
}

// Whether the storage driver takes a per container disk quota, overlay2 on XFS with pquota
func (r *registrant) storageOpt(ctx context.Context, docker string) bool {
	r.mu.Lock()
	known := r.storage
	r.mu.Unlock()
	if known.known {
		return known.ok
	}
	captured, err := terminal.Capture(ctx, []string{docker, "info", "--format", "{{.Driver}}"}, 10*time.Second, nil)
	if err != nil || captured.Code != 0 {
		return false
	}
	if strings.ToLower(strings.TrimSpace(captured.Stdout)) != "overlay2" {
		r.mu.Lock()
		r.storage = probe{known: true}
		r.mu.Unlock()
		return false
	}
	created, err := terminal.Capture(ctx, []string{docker, "create", "--storage-opt", "size=1m", "hello-world"}, 15*time.Second, nil)
	if err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if created.Code == 0 {
		r.storage = probe{known: true, ok: true}
		if id := strings.TrimSpace(created.Stdout); id != "" {
			terminal.Capture(ctx, []string{docker, "rm", id}, 5*time.Second, nil)
		}
	} else if strings.Contains(strings.ToLower(created.Stderr), "storage") {
		r.storage = probe{known: true}
	}
	slog.Debug("Docker --storage-opt support", "supported", r.storage.ok)
	return r.storage.ok
}

// Entry points of the s6 overlay's init, which takes the place of --init
var s6Init = map[string]bool{"/init": true, "/package/admin/s6-overlay/command/init": true}

// Whether the image's entry point is the s6 overlay's init
func initEntrypoint(ctx context.Context, docker, image string) bool {
	captured := query(ctx, []string{docker, "image", "inspect", image, "--format", "{{json .Config.Entrypoint}}"}, 15*time.Second,
		"Docker: could not inspect entrypoint", "Docker: image inspect returned nonzero")
	if captured == nil {
		return false
	}
	raw := strings.TrimSpace(captured.Stdout)
	if raw == "" || raw == "null" {
		return false
	}
	var entrypoint any
	if err := json.Unmarshal([]byte(raw), &entrypoint); err != nil {
		return false
	}
	switch v := entrypoint.(type) {
	case string:
		return s6Init[strings.TrimSpace(v)]
	case []any:
		if len(v) > 0 {
			first, _ := v[0].(string)
			return s6Init[strings.TrimSpace(first)]
		}
	}
	return false
}

// A volume spec's host and container paths
var volumeSpec = regexp.MustCompile(`^(.+):(/[^:]+)(?::[^:]*)?$`)

func splitVolume(spec string) (string, string, bool) {
	m := volumeSpec.FindStringSubmatch(strings.TrimSpace(spec))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// Volumes of the settings as -v flags, those without a colon left out
func volumeArgs(volumes []string) []string {
	var args []string
	for _, volume := range volumes {
		if volume = strings.TrimSpace(volume); volume == "" {
			continue
		}
		if !strings.Contains(volume, ":") {
			slog.Warn("Docker volume missing colon, skipping", "volume", volume)
			continue
		}
		args = append(args, "-v", volume)
	}
	return args
}

// Whether the volume flags mount something at the container path
func mountTaken(args []string, target string) bool {
	target = cmpRoot(strings.TrimRight(target, "/"))
	for _, arg := range args {
		if _, container, ok := splitVolume(arg); ok && cmpRoot(strings.TrimRight(container, "/")) == target {
			return true
		}
	}
	return false
}

func cmpRoot(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// Where the volumes already mount the host folder, empty when they don't
func existingMount(args []string, host string) string {
	want := terminal.HostKey(host)
	if want == "" {
		return ""
	}
	for _, arg := range args {
		if h, container, ok := splitVolume(arg); ok && terminal.HostKey(h) == want {
			return container
		}
	}
	return ""
}

// Whether the volumes mount something at /workspace themselves
func workspaceTaken(args []string) bool {
	for _, arg := range args {
		if strings.Contains(arg, ":/workspace") {
			return true
		}
	}
	return false
}

// Where the host folder goes in the container: where a volume mounts it already, else /workspace, else a free /host-cwd when a volume
// takes /workspace; existing tells the first
func bindTarget(args []string, host string) (target string, existing bool) {
	if mount := existingMount(args, host); mount != "" {
		return mount, true
	}
	if !workspaceTaken(args) {
		return "/workspace", false
	}
	for i := range hostCwdMounts {
		candidate := "/host-cwd"
		if i > 0 {
			candidate = fmt.Sprintf("/host-cwd-%d", i+1)
		}
		if !mountTaken(args, candidate) {
			return candidate, false
		}
	}
	return fmt.Sprintf("/host-cwd-%d", hostCwdMounts), false
}

// Windows drive paths
var windowsDrive = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// Absolute host path of a folder, a Windows drive path left as it is on another system
func absHost(dir string) string {
	expanded := fsx.ExpandHome(dir)
	if windowsDrive.MatchString(expanded) && runtime.GOOS != "windows" {
		return expanded
	}
	if abs, err := filepath.Abs(expanded); err == nil {
		return abs
	}
	return expanded
}

// Host folder the sandbox of a session working in dir mounts and where it sees it: the session's folder when the working folder is
// mounted, or always for a Windows drive folder, which can't be a container's own
func mount(cfg *config.Config, dir string) (string, string) {
	if dir == "" {
		return "", ""
	}
	host := absHost(dir)
	if !cfg.GetBool(dockerconfig.MountCwd) && !windowsDrive.MatchString(dir) && !windowsDrive.MatchString(host) {
		return "", ""
	}
	if st, err := os.Stat(host); err != nil || !st.IsDir() {
		slog.Debug("Skipping docker cwd mount: host_cwd is not a valid directory", "host_cwd", dir)
		return "", ""
	}
	if strings.HasPrefix(host, "/workspace") || strings.HasPrefix(host, "/root") {
		return "", ""
	}
	volumes, err := list(cfg, dockerconfig.Volumes)
	if err != nil {
		slog.Warn("Docker volumes unreadable; mounting the working folder at /workspace", "error", err)
	}
	target, _ := bindTarget(volumeArgs(volumes), host)
	return host, target
}

// Whether the settings hand the sandbox host folders: the session's folder, or a volume of a host path
func exposed(cfg *config.Config) bool {
	if cfg.GetBool(dockerconfig.MountCwd) {
		return true
	}
	volumes, _ := list(cfg, dockerconfig.Volumes)
	for _, volume := range volumes {
		if hostVolume(volume) {
			return true
		}
	}
	return false
}

// Whether a volume spec mounts a path of the host rather than a named volume
func hostVolume(spec string) bool {
	volume := strings.TrimSpace(spec)
	for _, prefix := range []string{"/", "~", "./", "../"} {
		if strings.HasPrefix(volume, prefix) {
			return true
		}
	}
	return len(volume) >= 3 && volume[1] == ':' && (volume[2] == '/' || volume[2] == '\\')
}

// Whether a working folder stands for the mounted host folder rather than a folder of the container
func followsMount(cwd, mount string) bool {
	if mount == "" || cwd == "" || cwd == mount {
		return false
	}
	if unusable(cwd) {
		return true
	}
	return cwd == "/workspace" && mount != "/workspace"
}

// Whether a folder is a host user folder, a Windows drive folder or relative, so no folder of a container
func unusable(cwd string) bool {
	return strings.HasPrefix(cwd, "/Users/") || strings.HasPrefix(cwd, "/home/") || windowsDrive.MatchString(cwd) || !strings.HasPrefix(cwd, "/")
}

// Whether a mount spec's source is a per process temp folder, which changes every run
func volatile(spec string) bool {
	if spec == "-v" || spec == "--mount" || !strings.Contains(spec, ":") {
		return false
	}
	source, _, ok := splitVolume(spec)
	if !ok {
		source, _, _ = strings.Cut(spec, ":")
	}
	if windowsDrive.MatchString(source) {
		return false
	}
	temp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(fsx.ExpandHome(source))
	if err != nil {
		return false
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs == temp || strings.HasPrefix(abs, temp+string(filepath.Separator))
}

// Digest of what a container can't change once made, its image and mounts, so reuse never attaches to one with stale mounts
func fingerprint(cfg *config.Config, image string, mounts []string) string {
	home, _ := filepath.Abs(cfg.ProfileDir(cfg.Profile()))
	if runtime.GOOS == "windows" {
		home = strings.ToLower(filepath.ToSlash(home))
	}
	canonical := make([]string, 0, len(mounts))
	for _, spec := range mounts {
		if volatile(spec) {
			_, rest, _ := strings.Cut(spec, ":")
			canonical = append(canonical, "<volatile-tempdir-mount>:"+rest)
		} else {
			canonical = append(canonical, spec)
		}
	}
	payload, _ := json.Marshal(map[string]any{"icebath_home": home, "image": image, "mount_args": canonical})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])[:24]
}

// -e flags naming the variables only, so their values travel through the docker client's environment rather than its command line
func nameOnly(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	args := make([]string, 0, 2*len(names))
	for _, name := range names {
		args = append(args, "-e", name)
	}
	return args
}

// Environment of a docker client carrying the values: this process's own with them on top, nil to inherit it when there are none
func client(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	env := terminal.ProcessEnv()
	for name, value := range values {
		env[name] = value
	}
	return terminal.Environ(env)
}
