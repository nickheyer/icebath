package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Windows OpenSSH has no Unix socket ControlMaster, so multiplexing is skipped there
var sshMultiplex = runtime.GOOS != "windows"

// Fails fast with a clear error when the ssh or scp client is unavailable
func ensureSSHAvailable() error {
	for _, tool := range []string{"ssh", "scp"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is not installed or not in PATH. Install OpenSSH client: apt install openssh-client", strings.ToUpper(tool))
		}
	}
	return nil
}

// Connection error for a failed sync step naming what to verify
func sshSyncError(reason, subject, what string) error {
	if what == "" {
		what = "the SSH connection"
	}
	return NewEnvironmentConnectionError(reason, fmt.Sprintf("%s failed — verify %s is healthy, then retry.", subject, what))
}

// sshEnv runs commands on a remote machine over SSH, one ssh process per call with ControlMaster reuse
type sshEnv struct {
	baseEnv
	host, user, keyPath string
	port                int
	controlDir          string
	controlSocket       string
	remoteHome          string
	remoteHomeDetected  bool
	syncManager         *FileSyncManager
	probeOnly           bool
}

// SSHOptions are the connection settings of one ssh environment
type SSHOptions struct {
	Host, User, Cwd, KeyPath string
	Port, Timeout            int
	ProbeOnly, SyncFiles     bool
	Persistent               bool
}

// NewSSHEnvironment connects, detects the remote home, seeds the remote icebath tree and starts the session
func NewSSHEnvironment(ctx context.Context, env *Env, o SSHOptions) (*sshEnv, error) {
	if o.Cwd == "" {
		o.Cwd = "~"
	}
	if o.Timeout == 0 {
		o.Timeout = 60
	}
	if o.Port == 0 {
		o.Port = 22
	}
	s := &sshEnv{host: o.Host, user: o.User, keyPath: o.KeyPath, port: o.Port, probeOnly: o.ProbeOnly}
	s.profileScopedPassthrough = true
	s.passwordlessSudoProbeSupported = true
	initBaseEnv(&s.baseEnv, s, env, o.Cwd, o.Timeout, nil, o.Persistent)
	s.controlDir = filepath.Join(os.TempDir(), "icebath-ssh")
	if err := os.MkdirAll(s.controlDir, 0o700); err != nil {
		return nil, err
	}
	socketKey := fmt.Sprintf("%s@%s:%d", o.User, o.Host, o.Port)
	if o.ProbeOnly {
		socketKey = socketKey + ":probe:" + s.sessionID
	}
	sum := sha256.Sum256([]byte(socketKey))
	s.controlSocket = filepath.Join(s.controlDir, hex.EncodeToString(sum[:])[:16]+".sock")
	if err := ensureSSHAvailable(); err != nil {
		return nil, err
	}
	if err := s.establishConnection(ctx); err != nil {
		return nil, err
	}
	if o.ProbeOnly {
		return s, nil
	}
	s.remoteHome = s.detectRemoteHome(ctx)
	if o.SyncFiles {
		s.ensureRemoteDirs(ctx)
		base := s.remoteHome + "/.icebath"
		s.syncManager = NewFileSyncManager(env, func() []syncFile { return iterSyncFiles(env, base) },
			func(h, r string) error { return s.scpUpload(ctx, h, r) }, func(paths []string) error { return s.sshDelete(ctx, paths) },
			func(files []syncFile) error { return s.sshBulkUpload(ctx, files) }, func(dest string) error { return s.sshBulkDownload(ctx, dest) })
		s.syncManager.Sync(true)
	}
	s.initSession(ctx)
	return s, nil
}

func (s *sshEnv) EnvType() string { return "ssh" }

// Python class name of the environment as hermes logs and records it
func (s *sshEnv) ClassName() string { return "SSHEnvironment" }

var _ interface{ ClassName() string } = (*sshEnv)(nil)

// Pushes changed host files into the sandbox through its sync manager, nothing when the backend has none
func (s *sshEnv) SyncFiles(force bool) {
	if s.syncManager != nil {
		s.syncManager.Sync(force)
	}
}

var _ SandboxSyncer = (*sshEnv)(nil)

// RemoteHome is the detected remote home and whether the probe found it
func (s *sshEnv) RemoteHome() (string, bool) { return s.remoteHome, s.remoteHomeDetected }

// ShellJoinedExec reports that OpenSSH joins the argv words for the remote login shell
func (s *sshEnv) ShellJoinedExec() bool { return true }

// ExecPrefix is the ssh argv that runs its remainder on the remote host
func (s *sshEnv) ExecPrefix(user string, interactive bool) ([]string, error) {
	return s.buildSSHCommand(nil, nil), nil
}

// One ControlMaster per SendEnv name set beside the plain target socket
func (s *sshEnv) controlSocketFor(sendEnv []string) string {
	if len(sendEnv) == 0 {
		return s.controlSocket
	}
	sum := sha256.Sum256([]byte(strings.Join(sendEnv, " ")))
	stem := strings.TrimSuffix(filepath.Base(s.controlSocket), ".sock")
	return filepath.Join(filepath.Dir(s.controlSocket), stem[:8]+hex.EncodeToString(sum[:])[:8]+".sock")
}

// The plain socket plus every SendEnv set sibling sharing the eight character target prefix
func (s *sshEnv) controlSockets() []string {
	out := []string{s.controlSocket}
	stem := strings.TrimSuffix(filepath.Base(s.controlSocket), ".sock")
	siblings, _ := filepath.Glob(filepath.Join(filepath.Dir(s.controlSocket), stem[:8]+"*.sock"))
	sort.Strings(siblings)
	for _, sib := range siblings {
		if sib != s.controlSocket {
			out = append(out, sib)
		}
	}
	return out
}

// Port and key flags shared by ssh and scp
func (s *sshEnv) targetFlags(portFlag string) []string {
	var flags []string
	if s.port != 22 {
		flags = append(flags, portFlag, strconv.Itoa(s.port))
	}
	if s.keyPath != "" {
		flags = append(flags, "-i", s.keyPath)
	}
	return flags
}

// Builds the ssh argv with multiplexing, batch mode, SendEnv names and the target
func (s *sshEnv) buildSSHCommand(extra []string, sendEnv []string) []string {
	names := append([]string(nil), sendEnv...)
	sort.Strings(names)
	cmd := []string{"ssh"}
	if sshMultiplex {
		cmd = append(cmd, "-o", "ControlPath="+s.controlSocketFor(names), "-o", "ControlMaster=auto", "-o", "ControlPersist=300")
	}
	cmd = append(cmd, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10")
	for _, name := range names {
		cmd = append(cmd, "-o", "SendEnv="+name)
	}
	cmd = append(cmd, s.targetFlags("-p")...)
	cmd = append(cmd, extra...)
	return append(cmd, s.user+"@"+s.host)
}

// Runs one remote shell command over the multiplexed connection capturing output
func (s *sshEnv) runSSH(ctx context.Context, remoteCmd string, timeout time.Duration) (*CaptureResult, error) {
	return runCapture(ctx, append(s.buildSSHCommand(nil, nil), remoteCmd), timeout, false, nil)
}

// Runs a remote command and turns a non zero exit into a sync error
func (s *sshEnv) runSSHChecked(ctx context.Context, remoteCmd string, timeout time.Duration, reason, subject string) error {
	res, err := s.runSSH(ctx, remoteCmd, timeout)
	if err != nil {
		return sshSyncError(reason+": "+err.Error(), subject, "")
	}
	if res.Code != 0 {
		return sshSyncError(reason+": "+strings.TrimSpace(res.Stderr), subject, "")
	}
	return nil
}

func (s *sshEnv) establishConnection(ctx context.Context) error {
	res, err := s.runSSH(ctx, "echo 'SSH connection established'", 15*time.Second)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return NewEnvironmentConnectionError(fmt.Sprintf("SSH connection to %s@%s timed out", s.user, s.host),
				fmt.Sprintf("Check that %s is up and reachable on port %d and that sshd is running, then retry.", s.host, s.port))
		}
		return NewEnvironmentConnectionError("SSH connection failed: "+err.Error(),
			fmt.Sprintf("Check that %s is up, sshd is running on port %d, and %s can log in with the configured key, then retry — the connection is re-established automatically.", s.host, s.port, s.user))
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return NewEnvironmentConnectionError("SSH connection failed: "+msg,
			fmt.Sprintf("Check that %s is up, sshd is running on port %d, and %s can log in with the configured key, then retry — the connection is re-established automatically.", s.host, s.port, s.user))
	}
	return nil
}

// Remote home from $HOME, the conventional path when the probe fails
func (s *sshEnv) detectRemoteHome(ctx context.Context) string {
	if res, err := s.runSSH(ctx, "echo $HOME", 10*time.Second); err == nil && res.Code == 0 && strings.TrimSpace(res.Stdout) != "" {
		envLog(s.env).Debug("SSH: remote home", "home", strings.TrimSpace(res.Stdout))
		s.remoteHomeDetected = true
		return strings.TrimSpace(res.Stdout)
	}
	if s.user == "root" {
		return "/root"
	}
	return "/home/" + s.user
}

// Creates the base icebath tree on the remote in one call
func (s *sshEnv) ensureRemoteDirs(ctx context.Context) {
	base := s.remoteHome + "/.icebath"
	s.runSSH(ctx, quotedMkdirCommand([]string{base, base + "/skills", base + "/credentials", base + "/cache"}), 10*time.Second)
}

// Uploads a single file via scp over the ControlMaster
func (s *sshEnv) scpUpload(ctx context.Context, hostPath, remotePath string) error {
	s.runSSH(ctx, "mkdir -p "+shQuote(path.Dir(remotePath)), 10*time.Second)
	cmd := []string{"scp"}
	if sshMultiplex {
		cmd = append(cmd, "-o", "ControlPath="+s.controlSocket)
	}
	cmd = append(cmd, s.targetFlags("-P")...)
	cmd = append(cmd, hostPath, fmt.Sprintf("%s@%s:%s", s.user, s.host, remotePath))
	res, err := runCapture(ctx, cmd, 30*time.Second, false, nil)
	if err != nil {
		return sshSyncError("scp failed: "+err.Error(), fmt.Sprintf("File sync to %s@%s", s.user, s.host), "")
	}
	if res.Code != 0 {
		return sshSyncError("scp failed: "+strings.TrimSpace(res.Stderr), fmt.Sprintf("File sync to %s@%s", s.user, s.host), "")
	}
	return nil
}

// Uploads many files in one tar over ssh stream after a single batched mkdir
func (s *sshEnv) sshBulkUpload(ctx context.Context, files []syncFile) error {
	if len(files) == 0 {
		return nil
	}
	base := s.remoteHome + "/.icebath"
	if parents := uniqueParentDirs(files); len(parents) > 0 {
		if err := s.runSSHChecked(ctx, quotedMkdirCommand(parents), 30*time.Second, "remote mkdir failed", "Remote directory setup on "+s.host); err != nil {
			return err
		}
	}
	staging, err := os.MkdirTemp("", "icebath-ssh-bulk-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	for _, f := range files {
		rel, err := relPosix(base, f.remote)
		if err != nil {
			return fmt.Errorf("remote path %s is not under sync base %s", pyRepr(f.remote), pyRepr(base))
		}
		if rel == "." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("remote path %s escapes sync base %s", pyRepr(f.remote), pyRepr(base))
		}
		staged := filepath.Join(staging, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
			return err
		}
		abs, _ := filepath.Abs(f.host)
		if err := os.Symlink(abs, staged); err != nil {
			if !symlinkPrivilegeMissing(err) {
				return err
			}
			if err := copyFileWithTimes(f.host, staged); err != nil {
				return err
			}
		}
	}
	sshCmd := append(s.buildSSHCommand(nil, nil), "tar xf - --no-overwrite-dir -C "+shQuote(base))
	tarCmd := exec.Command("tar", "-chf", "-", "-C", staging, ".")
	tarCmd.Stdin = nil
	tarStdout, err := tarCmd.StdoutPipe()
	if err != nil {
		return err
	}
	var tarStderr strings.Builder
	tarCmd.Stderr = &tarStderr
	sshProc := exec.Command(sshCmd[0], sshCmd[1:]...)
	sshProc.Stdin = tarStdout
	var sshStderr strings.Builder
	sshProc.Stderr = &sshStderr
	if err := tarCmd.Start(); err != nil {
		return err
	}
	if err := sshProc.Start(); err != nil {
		tarCmd.Process.Kill()
		tarCmd.Wait()
		return err
	}
	done := make(chan error, 1)
	go func() { done <- sshProc.Wait() }()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		sshProc.Process.Kill()
		tarCmd.Process.Kill()
		<-done
		tarCmd.Wait()
		return NewEnvironmentConnectionError("SSH bulk upload timed out", fmt.Sprintf("Bulk file sync to %s timed out — check the connection and retry.", s.host))
	}
	tarErr := tarCmd.Wait()
	if tarErr != nil {
		return fmt.Errorf("tar create failed (rc=%d): %s", exitCodeOf(tarCmd, tarErr), strings.TrimSpace(tarStderr.String()))
	}
	if code := sshProc.ProcessState.ExitCode(); code != 0 {
		return sshSyncError(fmt.Sprintf("tar extract over SSH failed (rc=%d): %s", code, strings.TrimSpace(sshStderr.String())), "File sync over SSH to "+s.host, "the connection")
	}
	envLog(s.env).Debug("SSH: bulk-uploaded files via tar pipe", "count", len(files))
	return nil
}

// POSIX relative path of target under base
func relPosix(base, target string) (string, error) {
	rel, err := filepath.Rel(filepath.FromSlash(base), filepath.FromSlash(target))
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// Downloads the remote icebath tree as a tar archive, tolerating only the socket ignored diagnostics
func (s *sshEnv) sshBulkDownload(ctx context.Context, dest string) error {
	relBase := strings.TrimLeft(s.remoteHome+"/.icebath", "/")
	sshCmd := append(s.buildSSHCommand(nil, nil), "tar cf - --exclude='*.sock' -C / "+shQuote(relBase))
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	runCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, sshCmd[0], sshCmd[1:]...)
	cmd.Stdout = out
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if runErr == nil {
		return nil
	}
	if runCtx.Err() != nil {
		return sshSyncError("SSH bulk download failed: timed out", "File sync from "+s.host, "")
	}
	code := exitCodeOf(cmd, runErr)
	text := strings.TrimSpace(stderr.String())
	var diagnostics []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			diagnostics = append(diagnostics, line)
		}
	}
	tolerated := code == 2 && len(diagnostics) > 0
	for _, line := range diagnostics {
		if !strings.HasSuffix(line, ": socket ignored") {
			tolerated = false
		}
	}
	if tolerated {
		return nil
	}
	return sshSyncError("SSH bulk download failed: "+text, "File sync from "+s.host, "")
}

func (s *sshEnv) sshDelete(ctx context.Context, remotePaths []string) error {
	return s.runSSHChecked(ctx, quotedRmCommand(remotePaths), 10*time.Second, "remote rm failed", "Remote file cleanup on "+s.host)
}

// Syncs changed host files before every command, rate limited inside the manager
func (s *sshEnv) beforeExecute(ctx context.Context) error {
	if s.syncManager != nil {
		s.syncManager.Sync(false)
	}
	return nil
}

// Forwards the passthrough allowlist by SendEnv names with values on the client env, unsetting scoped names remotely
func (s *sshEnv) runBash(ctx context.Context, cmd string, login bool, timeout int, stdinData *string) (ProcessHandle, error) {
	values, unsetNames, err := resolvePassthroughEnv(s.env, nil)
	if err != nil {
		return nil, err
	}
	argv := append(s.buildSSHCommand(nil, sortedNames(values)), bashArgv(shQuote(prependUnset(cmd, unsetNames)), login)...)
	clientEnv := clientEnvWith(values)
	return popenBash(argv, stdinData, func(c *exec.Cmd) {
		if clientEnv != nil {
			c.Env = envSlice(clientEnv)
		}
	})
}

// Cleanup syncs remote changes back then closes every control master
func (s *sshEnv) Cleanup(forceRemove bool) error {
	if s.syncManager != nil {
		envLog(s.env).Info("SSH: syncing files from sandbox...")
		s.syncManager.SyncBack(s.env.Home)
	}
	for _, socket := range s.controlSockets() {
		if _, err := os.Stat(socket); err != nil {
			continue
		}
		runCapture(context.Background(), []string{"ssh", "-o", "ControlPath=" + socket, "-O", "exit", s.user + "@" + s.host}, 5*time.Second, false, nil)
		os.Remove(socket)
	}
	return nil
}

// ---- provider -----------------------------------------------------------------------------------

type sshBackend struct{}

func (sshBackend) Name() string                           { return "ssh" }
func (sshBackend) DisplayName() string                    { return "SSH" }
func (sshBackend) Description() string                    { return "Run commands on a remote machine over SSH" }
func (sshBackend) EnvDescription() string                 { return "a remote host reached over SSH (likely Linux)" }
func (sshBackend) IsRemote() bool                         { return true }
func (sshBackend) IsContainer() bool                      { return false }
func (sshBackend) SkipContainerGuards() bool              { return false }
func (sshBackend) CachePathBase() string                  { return "~/.icebath" }
func (sshBackend) DefaultCwd() string                     { return "~" }
func (sshBackend) StripEnvKeys() []string                 { return nil }
func (sshBackend) SessionIsolatedWhenNonpersistent() bool { return false }
func (sshBackend) SupportsStreams() bool                  { return true }

// IsAvailable reports whether the ssh and scp clients are on PATH
func (sshBackend) IsAvailable() bool { return ensureSSHAvailable() == nil }

// CheckRequirements needs the host and user to be configured
func (sshBackend) CheckRequirements(env *Env, cfg map[string]any) error {
	if truthy(cfg["ssh_host"]) && truthy(cfg["ssh_user"]) {
		return nil
	}
	return errors.New("the SSH host and user are not configured (TERMINAL_SSH_HOST / TERMINAL_SSH_USER); run `icebath setup terminal` to enter them or pick the 'local' backend")
}

// Probe reports whether the configured host answers
func (sshBackend) Probe() (string, string) {
	if err := ensureSSHAvailable(); err != nil {
		return "unavailable", err.Error()
	}
	host := strings.TrimSpace(os.Getenv("TERMINAL_SSH_HOST"))
	if host == "" {
		return "needs_setup", "SSH host is not configured."
	}
	ok, detail := sshProbeConnection(host, os.Getenv("TERMINAL_SSH_USER"), os.Getenv("TERMINAL_SSH_PORT"), os.Getenv("TERMINAL_SSH_KEY"))
	if !ok {
		return "unavailable", detail
	}
	return "ready", "SSH connection to " + host
}

// Runs echo ok over a fresh batch mode connection with the doctor flags
func sshProbeConnection(host, user, port, key string) (bool, string) {
	cmd := []string{"ssh", "-o", "ConnectTimeout=5", "-o", "BatchMode=yes"}
	if port != "" {
		cmd = append(cmd, "-p", port)
	}
	if key != "" {
		cmd = append(cmd, "-i", expandTilde(key))
	}
	target := host
	if user != "" {
		target = user + "@" + host
	}
	cmd = append(cmd, target, "echo ok")
	res, err := runCapture(context.Background(), cmd, 15*time.Second, false, nil)
	if err != nil {
		return false, err.Error()
	}
	if res.Code != 0 {
		return false, strings.TrimSpace(res.Stderr)
	}
	return true, ""
}

// SetupInstructions explains the host, user, port and key settings
func (sshBackend) SetupInstructions(env *Env) []string {
	return []string{
		"Run commands on a remote machine via SSH.",
		"Set TERMINAL_SSH_HOST and TERMINAL_SSH_USER, with TERMINAL_SSH_PORT and TERMINAL_SSH_KEY when they differ from the defaults.",
		"The remote sshd must AcceptEnv the names in terminal.env_passthrough for secrets to reach the remote shell.",
	}
}

// DoctorChecks reports the configured host and a live connection probe
func (sshBackend) DoctorChecks() []DoctorCheck {
	host := os.Getenv("TERMINAL_SSH_HOST")
	if host == "" {
		return []DoctorCheck{{OK: false, Label: "SSH host not configured", Detail: "(needed for the 'ssh' terminal backend)"}}
	}
	ok, detail := sshProbeConnection(host, os.Getenv("TERMINAL_SSH_USER"), os.Getenv("TERMINAL_SSH_PORT"), os.Getenv("TERMINAL_SSH_KEY"))
	return []DoctorCheck{{OK: ok, Label: "SSH connection to " + host, Detail: detail}}
}

// CreateEnvironment builds the ssh environment from the shaped ssh_* keys
func (sshBackend) CreateEnvironment(ctx context.Context, env *Env, opts CreateOptions) (Environment, error) {
	cc := opts.ContainerConfig
	host, _ := cc["ssh_host"].(string)
	user, _ := cc["ssh_user"].(string)
	port, ok := pyIntValue(cc["ssh_port"])
	if !ok {
		port = 22
	}
	key, _ := cc["ssh_key"].(string)
	persistent := true
	if v, ok := cc["ssh_persistent"]; ok && v != nil {
		persistent = truthy(v)
	}
	return NewSSHEnvironment(ctx, env, SSHOptions{Host: host, User: user, Cwd: opts.Cwd, KeyPath: key, Port: port, Timeout: opts.Timeout,
		ProbeOnly: opts.ProbeOnly, SyncFiles: !opts.ProbeOnly, Persistent: persistent})
}
