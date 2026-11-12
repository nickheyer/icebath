//go:build windows

package backend

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// Spawns command directly, or through cmd.exe with the verbatim command line when shell is set
func spawnCommand(command string, args []string, shell bool) *exec.Cmd {
	if !shell {
		cmd := exec.Command(command, args...)
		hideWindow(cmd)
		return cmd
	}
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	cmd := exec.Command(comspec)
	line := strings.TrimSpace(command + " " + strings.Join(args, " "))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000, CmdLine: `/d /s /c "` + line + `"`}
	return cmd
}

// Terminates the child like a default child kill on Windows
func terminateProcess(p *os.Process) {
	p.Kill()
}

// Canonical home a ticket names, every symlink and junction resolved
func ReservedHome(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// Mints a session ticket over the owner named pipe through the gateway control client
func MintLocalGatewayTicket(endpoint GatewayEndpoint, purpose string) (string, error) {
	if purpose == "" {
		purpose = "interactive"
	}
	client := configuredTicketClient()
	if client == nil {
		return "", errors.New("Gateway ticket client is not configured")
	}
	return client(endpoint, purpose)
}
