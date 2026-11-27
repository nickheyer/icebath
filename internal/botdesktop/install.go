package botdesktop

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/proc"
)

// Exit code of an install on a host without root or sudo, the command to run as root said through the lines
const NoSudo = -2

// Exit code of an install the user gave no sudo password for
const Cancelled = -1

// Longest the package install may run
const installTimeout = 15 * time.Minute

// Refusal of an install while another holds the profile's slot
type InstallBusy struct{ Message string }

func (e *InstallBusy) Error() string { return e.Message }

// Takes the install slot of the profile, in this process and across processes, InstallBusy when another holds it
func (d *Desktop) Claim(cfg *config.Config) error {
	key := cfg.ProfileDir(cfg.Profile())
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.installing[key] != nil {
		return &InstallBusy{"an install is already running for this profile"}
	}
	if err := os.MkdirAll(StateDir(cfg), 0o700); err != nil {
		return err
	}
	release, ok, err := fsx.TryLock(filepath.Join(StateDir(cfg), "install.lock"))
	if err != nil {
		return err
	}
	if !ok {
		return &InstallBusy{"an install is already running for this profile (another process)"}
	}
	d.installing[key] = release
	return nil
}

func (d *Desktop) unclaim(cfg *config.Config) {
	key := cfg.ProfileDir(cfg.Profile())
	d.mu.Lock()
	release := d.installing[key]
	delete(d.installing, key)
	d.mu.Unlock()
	if release != nil {
		release()
	}
}

// Installs the screen's packages with the distribution's package manager under the profile's slot, claimed here unless the caller did,
// asking for the sudo password when sudo needs one and saying every line it prints; its exit code
func (d *Desktop) Install(ctx context.Context, cfg *config.Config, claimed bool, password func() (string, error), line func(string)) (int, error) {
	if !claimed {
		if err := d.Claim(cfg); err != nil {
			return 0, err
		}
	}
	defer d.unclaim(cfg)
	if !Supported() {
		return 0, errors.New("Bot Desktop runs on Linux gateway hosts only")
	}
	command := InstallCommand()
	if command == "" {
		return 0, errors.New("no supported package manager (apt-get, dnf, pacman) found on this host")
	}
	argv := strings.Fields(command)
	stdin := ""
	if argv[0] == "sudo" {
		if which("sudo") == "" {
			line("install needs root and this host has no sudo; run on the host as root: " + strings.TrimPrefix(command, "sudo "))
			return NoSudo, nil
		}
		check, cancel := context.WithTimeout(ctx, 3*time.Second)
		free := exec.CommandContext(check, "sudo", "-n", "true").Run() == nil
		cancel()
		if !free {
			given, err := password()
			if err != nil {
				return 0, err
			}
			if given == "" {
				line("install cancelled: no sudo password provided")
				return Cancelled, nil
			}
			argv = append([]string{"sudo", "-S", "-p", ""}, argv[1:]...)
			stdin = given + "\n"
		}
	}
	line("$ " + command)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C.UTF-8")
	cmd.SysProcAttr = sessionAttr()
	cmd.Stdin = strings.NewReader(stdin)
	cmd.WaitDelay = 10 * time.Second
	reader, writer := io.Pipe()
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	done := make(chan int, 1)
	go func() {
		err := cmd.Wait()
		writer.Close()
		done <- exitOf(cmd, err)
	}()
	read := make(chan struct{})
	go func() {
		defer close(read)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			line(strings.ToValidUTF8(scanner.Text(), "�"))
		}
	}()
	select {
	case code := <-done:
		<-read
		return code, nil
	case <-time.After(installTimeout):
	case <-ctx.Done():
	}
	// The whole session of the install goes, the package manager running as root included
	pid := cmd.Process.Pid
	code, exited := -9, false
	for _, round := range []struct {
		force bool
		grace time.Duration
	}{{false, 5 * time.Second}, {true, time.Second}} {
		terminal.SignalGroup(pid, round.force)
		if !exited {
			select {
			case code = <-done:
				exited = true
			case <-time.After(round.grace):
			}
		}
		if exited && terminal.GroupGone(pid) {
			break
		}
	}
	line(fmt.Sprintf("install timed out after %.0fs; the package manager may still be running as root", installTimeout.Seconds()))
	if exited {
		<-read
	}
	return code, nil
}

// Exit code of the install, the negated signal number when a signal ended it
func exitOf(cmd *exec.Cmd, err error) int {
	if err == nil {
		return 0
	}
	if _, exited := errors.AsType[*exec.ExitError](err); !exited {
		return 1
	}
	return proc.ExitCode(cmd.ProcessState)
}
