package botdesktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Exit code of an install on an unprivileged host without sudo, the command went to the line sink
const NoSudo = -2

// Refusal raised while another install holds the slot of the profile
type InstallBusy struct{ Msg string }

func (e *InstallBusy) Error() string { return e.Msg }

var (
	installLock      sync.Mutex
	installRunning   = map[string]struct{}{}
	installSlotFiles = map[string]*os.File{}
	termGraceSeconds = 5.0
	installExec      = exec.Command
	installRun       = runInstall
	sudoNopasswd     = func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "sudo", "-n", "true").Run() == nil
	}
)

// Opens the lock file with a non blocking exclusive flock, nil when another holder has it
func tryFlock(path string) (*os.File, error) {
	if err := flockAvailable(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return nil, err
	}
	fh, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	if err := flockExclusiveNonBlocking(fh); err != nil {
		fh.Close()
		return nil, nil
	}
	return fh, nil
}

// Slot key of the profile the env stands for
func installKey(env *tools.Env) string { return home.Key(envHome(env)) }

// Returns InstallBusy when this process or another one holds the install slot of the profile
func AssertNotRunning(env *tools.Env) error {
	installLock.Lock()
	_, busy := installRunning[installKey(env)]
	installLock.Unlock()
	if busy {
		return &InstallBusy{Msg: "an install is already running for this profile"}
	}
	fh, err := tryFlock(filepath.Join(StateDir(env), "install.lock"))
	if err != nil {
		return err
	}
	if fh == nil {
		return &InstallBusy{Msg: "an install is already running for this profile (another process)"}
	}
	fh.Close()
	return nil
}

// Atomically takes the install slot of the profile and returns its key, InstallBusy when taken
func Claim(env *tools.Env) (string, error) {
	key := installKey(env)
	installLock.Lock()
	defer installLock.Unlock()
	if _, busy := installRunning[key]; busy {
		return "", &InstallBusy{Msg: "an install is already running for this profile"}
	}
	fh, err := tryFlock(filepath.Join(StateDir(env), "install.lock"))
	if err != nil {
		return "", err
	}
	if fh == nil {
		return "", &InstallBusy{Msg: "an install is already running for this profile (another process)"}
	}
	installRunning[key] = struct{}{}
	installSlotFiles[key] = fh
	return key, nil
}

// Frees the install slot under key and drops its file lock
func Release(key string) {
	installLock.Lock()
	delete(installRunning, key)
	fh := installSlotFiles[key]
	delete(installSlotFiles, key)
	installLock.Unlock()
	if fh != nil {
		fh.Close()
	}
}

// Runs the distro package install with the default fifteen minute timeout and returns its exit code
func InstallPackages(env *tools.Env, askPassword func() (string, error), onLine func(string)) (int, error) {
	return InstallPackagesWith(env, askPassword, onLine, 900.0, false)
}

// Runs the package install under the profile slot, released on return even when the caller claimed it
func InstallPackagesWith(env *tools.Env, askPassword func() (string, error), onLine func(string), timeoutSeconds float64, claimed bool) (int, error) {
	key, held := "", false
	if claimed {
		key, held = installKey(env), true
	}
	defer func() {
		if held {
			Release(key)
		}
	}()
	if !IsSupportedHost() {
		return 0, errors.New("Bot Desktop runs on Linux gateway hosts only")
	}
	cmd := InstallCommand()
	if cmd == "" {
		return 0, errors.New("no supported package manager (apt-get, dnf, pacman) found on this host")
	}
	if !held {
		k, err := Claim(env)
		if err != nil {
			return 0, err
		}
		key, held = k, true
	}
	return installRun(cmd, askPassword, onLine, timeoutSeconds)
}

// Child of one install with its exit code published once the leader is reaped
type installProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	code int
}

// Exit code of the reaped leader, false while it still runs
func (p *installProc) poll() (int, bool) {
	select {
	case <-p.done:
		return p.code, true
	default:
		return 0, false
	}
}

// Waits up to the given seconds for the leader to exit and reports whether it did
func (p *installProc) waitFor(seconds float64) bool {
	t := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer t.Stop()
	select {
	case <-p.done:
		return true
	case <-t.C:
		return false
	}
}

// Spawns the install command, feeds the sudo password on stdin, streams output lines and returns the exit code
func runInstall(cmd string, askPassword func() (string, error), onLine func(string), timeoutSeconds float64) (int, error) {
	argv, err := pyshlex.SplitPOSIX(cmd, false)
	if err != nil {
		return 0, err
	}
	stdinPayload, hasPayload := "", false
	if argv[0] == "sudo" {
		if whichBinary("sudo") == "" {
			onLine("install needs root and this host has no sudo; run on the host as root: " + cmd[min(len("sudo "), len(cmd)):])
			return NoSudo, nil
		}
		if !sudoNopasswd() {
			password, err := askPassword()
			if err != nil {
				return 0, err
			}
			if password == "" {
				onLine("install cancelled: no sudo password provided")
				return -1, nil
			}
			argv = append([]string{"sudo", "-S", "-p", ""}, argv[1:]...)
			stdinPayload, hasPayload = password+"\n", true
		}
	}
	onLine("$ " + cmd)
	c := installExec(argv[0], argv[1:]...)
	c.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C.UTF-8")
	c.SysProcAttr = newSessionAttr()
	stdin, err := c.StdinPipe()
	if err != nil {
		return 0, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	c.Stdout, c.Stderr = outW, outW
	if err := c.Start(); err != nil {
		outR.Close()
		outW.Close()
		return 0, err
	}
	outW.Close()
	proc := &installProc{cmd: c, done: make(chan struct{})}
	go func() {
		proc.code = installReturncode(c.Wait())
		close(proc.done)
	}()
	if hasPayload {
		stdin.Write([]byte(stdinPayload))
	}
	stdin.Close()
	defer func() {
		outR.Close()
		if _, exited := proc.poll(); !exited {
			killInstallGroup(proc)
		}
	}()
	deadline := time.Now().Add(time.Duration(timeoutSeconds * float64(time.Second)))
	if drainUntil(outR, onLine, deadline) {
		<-proc.done
		return proc.code, nil
	}
	killInstallGroup(proc)
	onLine(fmt.Sprintf("install timed out after %.0fs; the package manager may still be running as root", timeoutSeconds))
	if code, exited := proc.poll(); exited {
		return code, nil
	}
	return -9, nil
}

// Popen returncode of the reaped leader, the negated signal number when a signal killed it
func installReturncode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return pysubprocess.Returncode(exit)
	}
	return 0
}

// Streams output lines to the sink until end of file, false when the deadline passes first
func drainUntil(out *os.File, onLine func(string), deadline time.Time) bool {
	type read struct {
		chunk []byte
		eof   bool
	}
	reads, stop := make(chan read), make(chan struct{})
	defer close(stop)
	go func() {
		for {
			chunk := make([]byte, 65536)
			n, _ := out.Read(chunk)
			select {
			case reads <- read{chunk: chunk[:n], eof: n == 0}:
			case <-stop:
				return
			}
			if n == 0 {
				return
			}
		}
	}()
	var buf []byte
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		t := time.NewTimer(remaining)
		select {
		case <-t.C:
			return false
		case r := <-reads:
			t.Stop()
			if r.eof {
				if len(buf) > 0 {
					onLine(pystr.DecodeUTF8Replace(buf))
				}
				return true
			}
			parts := bytes.Split(append(buf, r.chunk...), []byte("\n"))
			for _, line := range parts[:len(parts)-1] {
				onLine(pystr.DecodeUTF8Replace(line))
			}
			buf = append([]byte(nil), parts[len(parts)-1]...)
		}
	}
}

// Terminates the whole session group of the install, SIGKILL after the grace when anything survives
func killInstallGroup(proc *installProc) {
	pid := proc.cmd.Process.Pid
	groupGone := func() bool { return isESRCH(killpg(pid, 0)) }
	for _, round := range []struct {
		sig   syscall.Signal
		grace float64
	}{{syscall.SIGTERM, termGraceSeconds}, {syscall.SIGKILL, 1.0}} {
		killpg(pid, round.sig)
		proc.waitFor(round.grace)
		deadline := time.Now().Add(time.Duration(round.grace * float64(time.Second)))
		for time.Now().Before(deadline) && !groupGone() {
			time.Sleep(50 * time.Millisecond)
		}
		if groupGone() {
			return
		}
	}
}
