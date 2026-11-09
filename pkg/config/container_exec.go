package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	xterm "github.com/charmbracelet/x/term"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

// Seams of exec_in_container the tests replace, each defaulting to the real process operation
var (
	containerWhich      = exec.LookPath
	containerProbeRun   = runContainerProbe
	containerStdinIsTTY = func() bool { return xterm.IsTerminal(os.Stdin.Fd()) }
	containerExecvp     = execContainerCommand
	containerStderr     io.Writer
)

// Writer for the container routing errors, os.Stderr unless a test swapped it
func containerErr() io.Writer {
	if containerStderr != nil {
		return containerStderr
	}
	return os.Stderr
}

// Exit status of a 15 second container inspect probe, timedOut set when the daemon never answered
func runContainerProbe(argv []string) (returncode int, timedOut bool, err error) {
	done, err := pysubprocess.RunWith(context.Background(), argv, pysubprocess.Options{Timeout: 15 * time.Second})
	var expired *pysubprocess.TimeoutExpired
	if errors.As(err, &expired) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	return done.Returncode, false, nil
}

// Container inspect probe, exit 1 with the daemon message when it times out
func probeContainer(argv []string, backend string, viaSudo bool) (int, bool) {
	code, timedOut, err := containerProbeRun(argv)
	if err != nil {
		panic(err)
	}
	if timedOut {
		label := backend
		if viaSudo {
			label = "sudo " + backend
		}
		fmt.Fprintf(containerErr(), "Error: timed out waiting for %s to respond.\nThe %s daemon may be unresponsive or starting up.\n", label, backend)
		return 1, true
	}
	return code, false
}

// ExecInContainer replaces this process with the same command inside the managed container, hermes_cli/main_container.py exec_in_container
func ExecInContainer(info *ContainerExecInfo, cliArgs []string) (code int, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				code, err = 1, e
				return
			}
			panic(r)
		}
	}()
	backend, containerName := info.Backend, info.ContainerName
	runtime, lookErr := containerWhich(backend)
	if lookErr != nil || runtime == "" {
		fmt.Fprintf(containerErr(), "Error: %s not found on PATH. Cannot route to container.\n", backend)
		return 1, nil
	}
	inspectCmd := []string{runtime, "inspect", "--format", "ok", containerName}
	cmdPrefix := []string{runtime}
	if rc, exited := probeContainer(inspectCmd, backend, false); exited {
		return rc, nil
	} else if rc != 0 {
		sudoPath, sudoErr := containerWhich("sudo")
		if sudoErr != nil || sudoPath == "" {
			fmt.Fprintf(containerErr(), "Error: container '%s' not found via %s.\nThe container may be running under root. Try: sudo icebath %s\n",
				containerName, backend, strings.Join(cliArgs, " "))
			return 1, nil
		}
		cmdPrefix = []string{sudoPath, "-n", runtime}
		if rc, exited := probeContainer(append(append([]string{}, cmdPrefix[:2]...), inspectCmd...), backend, true); exited {
			return rc, nil
		} else if rc != 0 {
			user := os.Getenv("USER")
			if user == "" {
				user = "your-user"
			}
			fmt.Fprintf(containerErr(), "Error: container '%s' not found via %s.\n\n"+
				"The container is likely running as root. Your user cannot see it\n"+
				"because %s uses per-user namespaces. Grant passwordless\n"+
				"sudo for %s — the -n (non-interactive) flag is required\n"+
				"because a password prompt would hang or break piped commands.\n\n"+
				"On NixOS:\n\n"+
				"  security.sudo.extraRules = [{\n"+
				"    users = [ \"%s\" ];\n"+
				"    commands = [{ command = \"%s\"; options = [ \"NOPASSWD\" ]; }];\n"+
				"  }];\n\n"+
				"Or run: sudo icebath %s\n", containerName, backend, backend, backend, user, runtime, strings.Join(cliArgs, " "))
			return 1, nil
		}
	}
	var envFlags []string
	for _, name := range []string{"TERM", "COLORTERM", "LANG", "LC_ALL"} {
		if val := os.Getenv(name); val != "" {
			envFlags = append(envFlags, "-e", name+"="+val)
		}
	}
	tty := "-i"
	if containerStdinIsTTY() {
		tty = "-it"
	}
	execCmd := append([]string{}, cmdPrefix...)
	execCmd = append(execCmd, "exec", tty, "-u", info.ExecUser)
	execCmd = append(execCmd, envFlags...)
	execCmd = append(execCmd, containerName, info.IcebathBin)
	execCmd = append(execCmd, cliArgs...)
	return containerExecvp(execCmd)
}
