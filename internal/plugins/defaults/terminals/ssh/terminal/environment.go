package terminal

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	sshconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/ssh/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
)

// Windows OpenSSH has no Unix socket ControlMaster, so connections are not multiplexed there
var multiplex = runtime.GOOS != "windows"

// Commands run on a remote machine, one ssh process per call over a ControlMaster connection
type environment struct {
	*terminal.Shell
	runner *runner
}

// OpenSSH's argv that runs the rest on the remote machine
func (e *environment) ExecPrefix(string, bool) ([]string, error) { return e.runner.command(nil), nil }

// OpenSSH joins the argv words for the remote login shell
func (e *environment) ShellJoined() bool { return true }

// Pushes changed host files into the remote icebath folder now
func (e *environment) SyncFiles() {
	if e.runner.sync != nil {
		e.runner.sync.Sync(true)
	}
}

type runner struct {
	terminal.HostProcs
	spec   *terminal.Spec
	skills *skills.Registry
	shell  *terminal.Shell
	target
	// ControlMaster socket of the connection with no SendEnv names
	socket string
	// Remote home and its icebath folder the sandbox copies live in
	home string
	sync *terminal.FileSync
}

// Connects, finds the remote home, puts the profile's files there and takes the session's snapshot
func create(ctx context.Context, spec *terminal.Spec, skillset *skills.Registry) (terminal.Environment, error) {
	cfg := spec.Config
	t, err := targetOf(cfg)
	if err != nil {
		return nil, err
	}
	if t.host == "" || t.user == "" {
		return nil, errors.New("The SSH host and user are not set. Enter them in /settings under Terminal › SSH, or choose another place to run commands in /backends.")
	}
	if err := client(); err != nil {
		return nil, err
	}
	dir := filepath.Join(os.TempDir(), "icebath-ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key := fmt.Sprintf("%s:%d", t.login(), t.port)
	if spec.Probe {
		key += ":probe:" + rand.Text()
	}
	sum := sha256.Sum256([]byte(key))
	r := &runner{spec: spec, skills: skillset, target: t, socket: filepath.Join(dir, hex.EncodeToString(sum[:])[:16]+".sock")}
	if err := r.connect(ctx); err != nil {
		return nil, err
	}
	r.shell = terminal.NewShell(spec, r, terminal.ShellOptions{
		Backend:   "ssh",
		Cwd:       cmp.Or(spec.Cwd, "~"),
		Timeout:   spec.Timeout,
		Persist:   cfg.GetBool(sshconfig.Persistent),
		SudoProbe: true,
		Forwarded: r.forwarded,
	})
	env := &environment{Shell: r.shell, runner: r}
	if spec.Probe {
		return env, nil
	}
	home, detected := r.detectHome(ctx)
	r.home = home
	if detected {
		r.shell.StampHome(home)
	}
	base := r.base()
	r.capture(ctx, terminal.MkdirCommand([]string{base, base + "/skills", base + "/credentials", base + "/cache"}), 10*time.Second)
	r.sync = terminal.NewFileSync(cfg, func() []terminal.SyncFile {
		needs, _ := spec.Skills(skillset)
		return terminal.SyncFiles(cfg, base, needs)
	}, terminal.Transport{Delete: r.remove, BulkUpload: r.upload, BulkDownload: r.download})
	r.sync.Sync(true)
	r.shell.Init(ctx)
	return env, nil
}

// Remote folder the sandbox copies of the profile's files live in
func (r *runner) base() string { return r.home + "/.icebath" }

// Socket of the ControlMaster for the SendEnv names, one per name set beside the plain one
func (r *runner) socketFor(names []string) string {
	if len(names) == 0 {
		return r.socket
	}
	sum := sha256.Sum256([]byte(strings.Join(names, " ")))
	stem := strings.TrimSuffix(filepath.Base(r.socket), ".sock")
	return filepath.Join(filepath.Dir(r.socket), stem[:8]+hex.EncodeToString(sum[:])[:8]+".sock")
}

// The plain socket and every SendEnv sibling sharing its eight character prefix
func (r *runner) sockets() []string {
	out := []string{r.socket}
	stem := strings.TrimSuffix(filepath.Base(r.socket), ".sock")
	siblings, _ := filepath.Glob(filepath.Join(filepath.Dir(r.socket), stem[:8]+"*.sock"))
	slices.Sort(siblings)
	for _, sibling := range siblings {
		if sibling != r.socket {
			out = append(out, sibling)
		}
	}
	return out
}

// ssh argv with multiplexing, batch mode, the SendEnv names and the target
func (r *runner) command(sendEnv []string) []string {
	names := slices.Sorted(slices.Values(sendEnv))
	argv := []string{"ssh"}
	if multiplex {
		argv = append(argv, "-o", "ControlPath="+r.socketFor(names), "-o", "ControlMaster=auto", "-o", "ControlPersist=300")
	}
	argv = append(argv, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10")
	for _, name := range names {
		argv = append(argv, "-o", "SendEnv="+name)
	}
	if r.port != 22 {
		argv = append(argv, "-p", strconv.Itoa(r.port))
	}
	if r.key != "" {
		argv = append(argv, "-i", r.key)
	}
	return append(argv, r.login())
}

// Runs one remote shell command over the shared connection, capturing its output
func (r *runner) capture(ctx context.Context, remote string, timeout time.Duration) (*terminal.Captured, error) {
	return terminal.Capture(ctx, append(r.command(nil), remote), timeout, nil)
}

// Connection error of a failed step of the file sync, naming what to check
func syncError(reason, subject, what string) error {
	return terminal.NewConnectionError(reason, fmt.Sprintf("%s failed — verify %s is healthy, then retry.", subject, cmp.Or(what, "the SSH connection")))
}

// Runs a remote command, its failure a sync error
func (r *runner) checked(remote string, timeout time.Duration, reason, subject string) error {
	result, err := r.capture(context.Background(), remote, timeout)
	if err != nil {
		return syncError(reason+": "+err.Error(), subject, "")
	}
	if result.Code != 0 {
		return syncError(reason+": "+strings.TrimSpace(result.Stderr), subject, "")
	}
	return nil
}

// Opens the shared connection, failing with what to check
func (r *runner) connect(ctx context.Context) error {
	hint := fmt.Sprintf("Check that %s is up, sshd is running on port %d, and %s can log in with the configured key, then retry — the connection is re-established automatically.", r.host, r.port, r.user)
	result, err := r.capture(ctx, "echo 'SSH connection established'", 15*time.Second)
	if errors.Is(err, terminal.ErrTimedOut) {
		return terminal.NewConnectionError(fmt.Sprintf("SSH connection to %s timed out", r.login()),
			fmt.Sprintf("Check that %s is up and reachable on port %d and that sshd is running, then retry.", r.host, r.port))
	}
	if err != nil {
		return terminal.NewConnectionError("SSH connection failed: "+err.Error(), hint)
	}
	if result.Code != 0 {
		return terminal.NewConnectionError("SSH connection failed: "+cmp.Or(strings.TrimSpace(result.Stderr), strings.TrimSpace(result.Stdout)), hint)
	}
	return nil
}

// Remote home from $HOME and whether it was found, the usual place for the user when the probe fails
func (r *runner) detectHome(ctx context.Context) (string, bool) {
	if result, err := r.capture(ctx, "echo $HOME", 10*time.Second); err == nil && result.Code == 0 && strings.TrimSpace(result.Stdout) != "" {
		home := strings.TrimSpace(result.Stdout)
		slog.Debug("SSH: remote home", "home", home)
		return home, true
	}
	if r.user == "root" {
		return "/root", false
	}
	return "/home/" + r.user, false
}

// Variables handed each command and the names unset for it: those the profile passes through and those its skills need
func (r *runner) forward() (map[string]string, []string) {
	_, extra := r.spec.Skills(r.skills)
	return r.spec.Terminal.Forward(r.spec.Config, extra)
}

// Names of the variables handed each command, kept out of the snapshot
func (r *runner) forwarded() []string {
	values, unset := r.forward()
	return append(slices.Collect(maps.Keys(values)), unset...)
}

// Pushes changed host files before each command, at most every few seconds
func (r *runner) Prepare(context.Context) error {
	if r.sync != nil {
		r.sync.Sync(false)
	}
	return nil
}

// Runs bash on the remote machine, the variables handed by SendEnv from the client's environment and the scoped ones unset first; the
// ssh client gets a session of its own so it dies with icebath
func (r *runner) Run(_ context.Context, script string, login bool, _ time.Duration, stdin *string) (terminal.Proc, error) {
	values, unset := r.forward()
	shell := []string{"bash", "-c"}
	if login {
		shell = []string{"bash", "-l", "-c"}
	}
	argv := append(append(r.command(slices.Collect(maps.Keys(values))), shell...), terminal.Quote(terminal.Unset(script, unset)))
	cmd := exec.Command(argv[0], argv[1:]...)
	if len(values) > 0 {
		env := terminal.ProcessEnv()
		maps.Copy(env, values)
		cmd.Env = terminal.Environ(env)
	}
	terminal.NewSession(cmd)
	p, err := terminal.Start(cmd, stdin)
	if err != nil {
		return nil, err
	}
	p.Own(r.spec.Terminal.Supervisor)
	return p, nil
}

// Copies the remote changes back, then closes every ControlMaster of the target
func (r *runner) Cleanup(bool) error {
	if r.sync != nil {
		slog.Info("SSH: syncing files from sandbox...")
		r.sync.SyncBack()
	}
	for _, socket := range r.sockets() {
		if _, err := os.Stat(socket); err != nil {
			continue
		}
		terminal.Capture(context.Background(), []string{"ssh", "-o", "ControlPath=" + socket, "-O", "exit", r.login()}, 5*time.Second, nil)
		os.Remove(socket)
	}
	return nil
}
