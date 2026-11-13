package backend

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"
)

const GatewayStopTimeout = 20 * time.Second

// Drains every gateway profile through the CLI on Windows before an uninstall deletes the installation
func StopGatewayBeforeUpdate(cliPath string) bool {
	return runGatewayLifecycleCommand(cliPath, []string{"gateway", "stop", "--all"}, runtime.GOOS == "windows")
}

// Runs a hidden gateway lifecycle command with stdio ignored, true only when it exits cleanly in time
func runGatewayLifecycleCommand(cliPath string, args []string, isWindows bool) bool {
	if !isWindows {
		return false
	}
	if _, err := os.Stat(cliPath); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), GatewayStopTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cliPath, args...)
	hideWindow(cmd)
	return cmd.Run() == nil
}

// Stops the app owned Windows backends before an update replaces application outputs
func (s *Service) StopBackendsForUpdate() {
	if runtime.GOOS != "windows" {
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.TeardownPrimary(false)
	}()
	go func() {
		defer wg.Done()
		s.StopAllPoolBackends()
	}()
	wg.Wait()
}
