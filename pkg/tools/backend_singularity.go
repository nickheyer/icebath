//go:build !icebath_core

package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	fsutil "github.com/nickheyer/icebath/pkg/utils/fs"
)

const singularityInstallURL = "https://apptainer.org/docs/admin/main/installation.html"

// Name of the apptainer or singularity CLI on PATH with apptainer preferred, empty when neither is installed
func findSingularityExecutable() string {
	for _, exe := range []string{"apptainer", "singularity"} {
		if _, err := exec.LookPath(exe); err == nil {
			return exe
		}
	}
	return ""
}

// Python returncode of a finished capture, minus the signal number for a child a signal killed
func singularityReturncode(res *CaptureResult, state *os.ProcessState) int {
	if state != nil {
		if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return -int(ws.Signal())
		}
	}
	return res.Code
}

// Text of the Python TimeoutExpired raised when argv outlives its timeout
func singularityTimeoutText(argv []string, seconds int) string {
	return fmt.Sprintf("Command '%s' timed out after %d seconds", anyutil.Quote(argv), seconds)
}

// Image from the Apptainer image setting, else the default sandbox image as a docker reference
func singularityConfiguredImage(ctx context.Context, env *Env) (string, error) {
	if text := strings.TrimSpace(settings.TerminalSingularityImage.Get(envHomeCtx(ctx, env))); text != "" {
		return text, nil
	}
	return "docker://" + defaultSandboxImage, nil
}

// Bare registry reference as a docker URL, schemes, sif files and existing paths kept as given
func asApptainerRef(image string) string {
	if strings.Contains(image, "://") || strings.HasSuffix(image, ".sif") {
		return image
	}
	if _, err := os.Stat(image); err == nil {
		return image
	}
	return "docker://" + image
}

// Image of a new instance, the per task override first then the configured image
func resolveSingularityImage(ctx context.Context, env *Env, image string) (string, error) {
	if image = strings.TrimSpace(image); image != "" {
		return asApptainerRef(image), nil
	}
	configured, err := singularityConfiguredImage(ctx, env)
	if err != nil {
		return "", err
	}
	return asApptainerRef(configured), nil
}

// CLI the instance runs through, an error naming both binaries when neither is installed
func requireSingularityExecutable() (string, error) {
	if exe := findSingularityExecutable(); exe != "" {
		return exe, nil
	}
	return "", errors.New("Neither 'apptainer' nor 'singularity' was found in PATH. " +
		"Install Apptainer (https://apptainer.org/docs/admin/main/installation.html) " +
		"or Singularity and ensure the CLI is available.")
}

// Resolves the CLI and confirms its version subcommand answers within ten seconds
func ensureSingularityAvailable(ctx context.Context) (string, error) {
	exe, err := requireSingularityExecutable()
	if err != nil {
		return "", err
	}
	res, state, err := runCaptureState(ctx, []string{exe, "version"}, 10*time.Second, false, nil)
	switch {
	case errors.Is(err, ErrCaptureTimeout):
		return "", fmt.Errorf("'%s version' timed out.", exe)
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("Singularity backend selected but '%s' could not be executed.", exe)
	case err != nil:
		return "", err
	}
	if code := singularityReturncode(res, state); code != 0 {
		return "", fmt.Errorf("'%s version' failed (exit code %d): %s", exe, code, runePrefix(strings.TrimSpace(res.Stderr), 200))
	}
	return exe, nil
}

// Scratch root of overlays and the SIF cache, TERMINAL_SCRATCH_DIR, else a writable /scratch, else sandboxes/singularity
func singularityScratchDir(env *Env) (string, error) {
	scratchPath := strings.TrimSpace(settings.TerminalScratchDir.Get(envHomeCtx(context.Background(), env)))
	if scratchPath == "" {
		sandbox, err := SandboxDir(env)
		if err != nil {
			return "", err
		}
		scratchPath = filepath.Join(sandbox, "singularity")
		if _, err := os.Stat("/scratch"); err == nil && fsutil.Access("/scratch", 2) {
			user, ok := os.LookupEnv("USER")
			if !ok {
				user = "icebath"
			}
			scratchPath = filepath.Join("/scratch", user, "icebath-agent")
			if err := os.MkdirAll(scratchPath, 0o755); err != nil {
				return "", err
			}
			envLog(env).Info(fmt.Sprintf("Using /scratch for sandboxes: %s", scratchPath))
		}
	}
	if err := os.MkdirAll(scratchPath, 0o755); err != nil {
		return "", err
	}
	return scratchPath, nil
}

// SIF cache directory, APPTAINER_CACHEDIR else the scratch .apptainer directory
func apptainerCacheDir(env *Env) (string, error) {
	cachePath := os.Getenv("APPTAINER_CACHEDIR")
	if cachePath == "" {
		scratch, err := singularityScratchDir(env)
		if err != nil {
			return "", err
		}
		cachePath = filepath.Join(scratch, ".apptainer")
	}
	if err := os.MkdirAll(cachePath, 0o755); err != nil {
		return "", err
	}
	return cachePath, nil
}

// Every icebath prefixed entry of a scratch directory, overlays included
func singularityScratchPaths(scratch string) []string {
	paths, _ := filepath.Glob(filepath.Join(scratch, "icebath-*"))
	return paths
}

const singularityDiskUsageCacheTTL = 300 * time.Second

// Last scratch usage verdict and when it was taken, unset until the first successful scan
var singularityDiskUsage struct {
	sync.Mutex
	stamp  time.Time
	set    bool
	result bool
}

// Python percent f formatting of a float, inf and nan spelled the Python way
func pyPercentF(f float64, prec int) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return strconv.FormatFloat(f, 'f', prec, 64)
}

// Warning threshold in GB from terminal.disk_warning_gb
func singularityDiskWarningThresholdGB(env *Env) float64 {
	return settings.TerminalDiskWarningGb.Get(envHomeCtx(context.Background(), env))
}

// Bytes of every regular file below root, symlinks followed for files the way Path.rglob and is_file see them
func singularityTreeBytes(root string) int64 {
	var total int64
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return 0
	}
	walkRoot := root + string(os.PathSeparator)
	_ = filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == walkRoot {
			return nil
		}
		if st, statErr := os.Stat(p); statErr == nil && st.Mode().IsRegular() {
			total += st.Size()
		}
		return nil
	})
	return total
}

// Reports scratch usage above the threshold, cached for five minutes and never cached after a failed scan
func checkSingularityDiskUsageWarning(env *Env) bool {
	singularityDiskUsage.Lock()
	if singularityDiskUsage.set && time.Since(singularityDiskUsage.stamp) < singularityDiskUsageCacheTTL {
		result := singularityDiskUsage.result
		singularityDiskUsage.Unlock()
		return result
	}
	singularityDiskUsage.Unlock()
	scratch, err := singularityScratchDir(env)
	if err != nil {
		envLog(env).Debug("Disk usage warning check failed", "error", err)
		return false
	}
	var totalBytes int64
	for _, p := range singularityScratchPaths(scratch) {
		totalBytes += singularityTreeBytes(p)
	}
	totalGB := float64(totalBytes) / math.Pow(1024, 3)
	threshold := singularityDiskWarningThresholdGB(env)
	exceeded := totalGB > threshold
	if exceeded {
		envLog(env).Warn(fmt.Sprintf("Singularity scratch disk usage (%sGB) exceeds threshold (%sGB).", pyPercentF(totalGB, 1), pyPercentF(threshold, 0)))
	}
	singularityDiskUsage.Lock()
	singularityDiskUsage.stamp, singularityDiskUsage.set, singularityDiskUsage.result = time.Now(), true, exceeded
	singularityDiskUsage.Unlock()
	return exceeded
}

// Scratch directories a Singularity instance of this process used, swept once at process exit
var singularityExitSweep struct {
	sync.Mutex
	dirs []string
}

// Queues a scratch directory for the exit sweep once
func registerSingularityExitSweep(scratch string) {
	singularityExitSweep.Lock()
	defer singularityExitSweep.Unlock()
	for _, d := range singularityExitSweep.dirs {
		if d == scratch {
			return
		}
	}
	singularityExitSweep.dirs = append(singularityExitSweep.dirs, scratch)
}

// Removes every icebath prefixed entry of a scratch directory, errors ignored like rmtree with ignore_errors
func sweepSingularityScratchDir(scratch string) {
	for _, p := range singularityScratchPaths(scratch) {
		_ = os.RemoveAll(p)
		envLog(nil).Info(fmt.Sprintf("Removed orphaned: %s", p))
	}
}

// Removes the orphaned scratch entries of every Singularity scratch directory this process used
func sweepSingularityScratchAtExit() {
	singularityExitSweep.Lock()
	dirs := append([]string(nil), singularityExitSweep.dirs...)
	singularityExitSweep.Unlock()
	for _, d := range dirs {
		sweepSingularityScratchDir(d)
	}
}

var singularitySIFBuildMu sync.Mutex

// Cached SIF built once from a docker URL, the URL itself when the build fails or times out
func singularityGetOrBuildSIF(ctx context.Context, env *Env, image, executable string) (string, error) {
	if strings.HasSuffix(image, ".sif") {
		if _, err := os.Stat(image); err == nil {
			return image, nil
		}
	}
	if !strings.HasPrefix(image, "docker://") {
		return image, nil
	}
	if IsDefaultSandboxImage(strings.TrimPrefix(image, "docker://")) {
		return apptainerSandboxSIF(ctx, env, executable)
	}
	imageName := strings.NewReplacer("/", "-", ":", "-").Replace(strings.ReplaceAll(image, "docker://", ""))
	cacheDir, err := apptainerCacheDir(env)
	if err != nil {
		return "", err
	}
	sifPath := filepath.Join(cacheDir, imageName+".sif")
	if _, err := os.Stat(sifPath); err == nil {
		return sifPath, nil
	}
	singularitySIFBuildMu.Lock()
	defer singularitySIFBuildMu.Unlock()
	if _, err := os.Stat(sifPath); err == nil {
		return sifPath, nil
	}
	log := envLog(env)
	log.Info("Building SIF image (one-time setup)...")
	log.Info(fmt.Sprintf("  Source: %s", image))
	log.Info(fmt.Sprintf("  Target: %s", sifPath))
	tmpDir := filepath.Join(cacheDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}
	buildEnv := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			buildEnv[k] = v
		}
	}
	buildEnv["APPTAINER_TMPDIR"] = tmpDir
	buildEnv["APPTAINER_CACHEDIR"] = cacheDir
	res, err := runCapture(ctx, []string{executable, "build", sifPath, image}, 600*time.Second, false, buildEnv)
	switch {
	case errors.Is(err, ErrCaptureTimeout):
		log.Warn("SIF build timed out, falling back to docker:// URL")
		if _, statErr := os.Stat(sifPath); statErr == nil {
			if rmErr := os.Remove(sifPath); rmErr != nil {
				return "", rmErr
			}
		}
		return image, nil
	case err != nil:
		log.Warn(fmt.Sprintf("SIF build error: %s, falling back to docker:// URL", err))
		return image, nil
	}
	if res.Code != 0 {
		log.Warn("SIF build failed, falling back to docker:// URL")
		log.Warn(fmt.Sprintf("  Error: %s", runePrefix(res.Stderr, 500)))
		return image, nil
	}
	log.Info("SIF image built successfully")
	return sifPath, nil
}

// Hardened Singularity or Apptainer instance with resource limits and an optional persistent overlay
type singularityEnv struct {
	baseEnv
	executable      string
	image           string
	instanceID      string
	instanceStarted atomic.Bool
	persistentValue any
	overlayDir      string
	cpu             any
	memory          any
}

func (s *singularityEnv) EnvType() string { return "singularity" }

// Python class name of the environment as logs and records name it
func (s *singularityEnv) ClassName() string { return "SingularityEnvironment" }

var _ interface{ ClassName() string } = (*singularityEnv)(nil)

// Inputs of one Singularity instance
type singularityOptions struct {
	Image      string
	Cwd        string
	Timeout    int
	CPU        any
	Memory     any
	Persistent any
	TaskID     string
}

// Starts the instance with its overlay or tmpfs and captures the session snapshot
func newSingularityEnvironment(ctx context.Context, env *Env, o singularityOptions) (*singularityEnv, error) {
	s := &singularityEnv{persistentValue: o.Persistent, cpu: o.CPU, memory: o.Memory}
	s.passwordlessSudoProbeSupported = true
	initBaseEnv(&s.baseEnv, s, env, o.Cwd, o.Timeout, nil, true)
	s.persistent = truthy(o.Persistent)
	exe, err := ensureSingularityAvailable(ctx)
	if err != nil {
		return nil, err
	}
	s.executable = exe
	if s.image, err = singularityGetOrBuildSIF(ctx, env, o.Image, exe); err != nil {
		return nil, err
	}
	s.instanceID = "icebath_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	scratch, scratchErr := singularityScratchDir(env)
	if scratchErr == nil {
		registerSingularityExitSweep(scratch)
	} else {
		envLog(env).Debug("Singularity scratch directory unavailable for the exit sweep", "error", scratchErr)
	}
	if s.persistent {
		if scratchErr != nil {
			return nil, scratchErr
		}
		s.overlayDir = filepath.Join(scratch, "icebath-overlays", "overlay-"+SanitizeTaskIDForPath(o.TaskID))
		if err := os.MkdirAll(s.overlayDir, 0o755); err != nil {
			return nil, err
		}
	}
	if err := s.startInstance(ctx); err != nil {
		return nil, err
	}
	s.initSession(ctx)
	return s, nil
}

// Whether a limit is above zero and its text, an error for a value that is not a number
func singularityLimit(v any) (bool, string, error) {
	if b, ok := v.(bool); ok {
		return b, strconv.FormatBool(b), nil
	}
	if n, ok := anyutil.Int(v); ok {
		return n > 0, strconv.FormatInt(n, 10), nil
	}
	if f, ok := v.(float64); ok {
		return f > 0, anyutil.String(f), nil
	}
	return false, "", fmt.Errorf("a Singularity limit is %s, expected a number", errchain.Kind(v))
}

// Read only bind args for registered credential files and skills directories
func (s *singularityEnv) credentialBindArgs() (args []string) {
	defer func() {
		if r := recover(); r != nil {
			envLog(s.env).Debug(fmt.Sprintf("Singularity: could not load credential/skills mounts: %v", r))
		}
	}()
	mounts := append(GetCredentialFileMounts(s.env), GetSkillsDirectoryMount(s.env, DefaultContainerBase)...)
	for _, m := range mounts {
		args = append(args, "--bind", m.HostPath+":"+m.ContainerPath+":ro")
	}
	return args
}

// Starts the named instance with containall, no home, the overlay or a writable tmpfs, mounts and limits
func (s *singularityEnv) startInstance(ctx context.Context) error {
	argv := []string{s.executable, "instance", "start", "--containall", "--no-home"}
	if s.persistent && s.overlayDir != "" {
		argv = append(argv, "--overlay", s.overlayDir)
	} else {
		argv = append(argv, "--writable-tmpfs")
	}
	argv = append(argv, s.credentialBindArgs()...)
	positive, text, err := singularityLimit(s.memory)
	if err != nil {
		return err
	}
	if positive {
		argv = append(argv, "--memory", text+"M")
	}
	if positive, text, err = singularityLimit(s.cpu); err != nil {
		return err
	}
	if positive {
		argv = append(argv, "--cpus", text)
	}
	argv = append(argv, s.image, s.instanceID)
	res, err := runCapture(ctx, argv, 120*time.Second, false, nil)
	switch {
	case errors.Is(err, ErrCaptureTimeout):
		return errors.New("Instance start timed out")
	case err != nil:
		return err
	}
	if res.Code != 0 {
		return fmt.Errorf("Failed to start instance: %s", res.Stderr)
	}
	s.instanceStarted.Store(true)
	envLog(s.env).Info(fmt.Sprintf("Singularity instance %s started (persistent=%s)", s.instanceID, anyutil.String(s.persistentValue)))
	return nil
}

// Spawns bash inside the running instance from the root dir, never the host cwd the container lacks
func (s *singularityEnv) runBash(ctx context.Context, cmd string, login bool, timeout int, stdinData *string) (ProcessHandle, error) {
	if !s.instanceStarted.Load() {
		return nil, errors.New("Singularity instance not started")
	}
	argv := append([]string{s.executable, "exec", "--pwd", "/", "instance://" + s.instanceID}, bashArgv(cmd, login)...)
	return popenBash(argv, stdinData, nil)
}

// ExecPrefix is the local argv running its remainder inside the instance as the caller, nil before start
func (s *singularityEnv) ExecPrefix(user string, interactive bool) ([]string, error) {
	if !s.instanceStarted.Load() {
		return nil, nil
	}
	return []string{s.executable, "exec", "--pwd", "/", "instance://" + s.instanceID}, nil
}

// Cleanup stops the instance, a persistent overlay surviving until the exit sweep
func (s *singularityEnv) Cleanup(forceRemove bool) error {
	if !s.instanceStarted.Load() {
		return nil
	}
	argv := []string{s.executable, "instance", "stop", s.instanceID}
	_, err := runCapture(context.Background(), argv, 30*time.Second, false, nil)
	switch {
	case errors.Is(err, ErrCaptureTimeout):
		envLog(s.env).Warn(fmt.Sprintf("Failed to stop Singularity instance %s: %s", s.instanceID, singularityTimeoutText(argv, 30)))
	case err != nil:
		envLog(s.env).Warn(fmt.Sprintf("Failed to stop Singularity instance %s: %s", s.instanceID, err))
	default:
		envLog(s.env).Info(fmt.Sprintf("Singularity instance %s stopped", s.instanceID))
	}
	s.instanceStarted.Store(false)
	return nil
}

// ---- provider -----------------------------------------------------------------------------------

// Singularity or Apptainer backend, a rootless HPC friendly container on this machine
type singularityBackend struct{}

func (singularityBackend) Name() string        { return "singularity" }
func (singularityBackend) DisplayName() string { return "Singularity/Apptainer" }
func (singularityBackend) Description() string {
	return "HPC-friendly rootless container (Apptainer or Singularity)."
}
func (singularityBackend) EnvDescription() string                 { return "a Singularity container (Linux)" }
func (singularityBackend) IsRemote() bool                         { return true }
func (singularityBackend) IsContainer() bool                      { return true }
func (b singularityBackend) SkipContainerGuards() bool            { return b.IsContainer() }
func (singularityBackend) CachePathBase() string                  { return "" }
func (singularityBackend) DefaultCwd() string                     { return "" }
func (singularityBackend) StripEnvKeys() []string                 { return nil }
func (singularityBackend) SessionIsolatedWhenNonpersistent() bool { return false }
func (singularityBackend) SupportsStreams() bool                  { return true }

// IsAvailable reports whether apptainer or singularity is on PATH
func (singularityBackend) IsAvailable() bool { return findSingularityExecutable() != "" }

// CheckRequirements logs why the CLI is missing or broken and reports the requirements unmet
func (singularityBackend) CheckRequirements(env *Env, cfg map[string]any) error {
	exe := findSingularityExecutable()
	if exe == "" {
		envLog(env).Error(fmt.Sprintf("Singularity backend selected but neither 'apptainer' nor 'singularity' is on PATH. "+
			"Install Apptainer: %s", singularityInstallURL))
		return ErrBackendRequirementsNotMet
	}
	argv := []string{exe, "--version"}
	res, state, err := runCaptureState(context.Background(), argv, 5*time.Second, false, nil)
	if err != nil {
		cause := err.Error()
		if errors.Is(err, ErrCaptureTimeout) {
			cause = singularityTimeoutText(argv, 5)
		}
		envLog(env).Error(fmt.Sprintf("Apptainer/Singularity is installed but `%s --version` failed (%s), "+
			"so the install looks broken", exe, cause))
		return ErrBackendRequirementsNotMet
	}
	if code := singularityReturncode(res, state); code != 0 {
		envLog(env).Error(fmt.Sprintf("Apptainer/Singularity is installed but `%s --version` exited with code %d, "+
			"so the install looks broken", exe, code))
		return ErrBackendRequirementsNotMet
	}
	return nil
}

// SetupInstructions names the CLI found or where to install it and the image in use
func (singularityBackend) SetupInstructions(env *Env) []string {
	lines := []string{fmt.Sprintf("Singularity/Apptainer not found in PATH! Install: %s", singularityInstallURL)}
	if exe := findSingularityExecutable(); exe != "" {
		lines[0] = fmt.Sprintf("Found %s.", exe)
	}
	image, err := singularityConfiguredImage(WithEnvHome(context.Background(), env), env)
	if err != nil {
		panic(err)
	}
	return append(lines, fmt.Sprintf("Image: %s (change it with `icebath config set terminal.singularity_image <image>`).", image))
}

// DoctorChecks reports the CLI found or where to install it
func (singularityBackend) DoctorChecks() []DoctorCheck {
	if exe := findSingularityExecutable(); exe != "" {
		return []DoctorCheck{{OK: true, Label: "Singularity/Apptainer", Detail: fmt.Sprintf("(%s found)", exe)}}
	}
	return []DoctorCheck{{OK: false, Label: "Singularity/Apptainer", Detail: fmt.Sprintf("(not found on PATH; install: %s)", singularityInstallURL)}}
}

// CreateEnvironment checks scratch usage and starts an instance with the resolved image and container limits
func (singularityBackend) CreateEnvironment(ctx context.Context, env *Env, opts CreateOptions) (Environment, error) {
	cc := opts.ContainerConfig
	get := func(key string, def any) any {
		if v, ok := cc[key]; ok {
			return v
		}
		return def
	}
	checkSingularityDiskUsageWarning(env)
	image, err := resolveSingularityImage(WithEnvHome(ctx, env), env, opts.Image)
	if err != nil {
		return nil, err
	}
	e, err := newSingularityEnvironment(ctx, env, singularityOptions{
		Image: image, Cwd: opts.Cwd, Timeout: opts.Timeout, CPU: get("container_cpu", 1), Memory: get("container_memory", 5120),
		Persistent: get("container_persistent", true), TaskID: opts.TaskID,
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

func init() {
	if err := RegisterTerminalBackend(singularityBackend{}); err != nil {
		panic(err)
	}
	addBackendExitSweep(sweepSingularityScratchAtExit)
}
