package updater

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/hardening"
)

// Staged updaters older than this refuse a claim pre-written for them
var markerSelfAdoptEpoch = time.Date(2026, time.July, 31, 0, 0, 0, 0, time.UTC)

// Staged setup app in ICEBATH_HOME on Windows, empty elsewhere or when it is absent
func resolveStagedUpdaterBinary(home string, isWindows bool) string {
	if !isWindows {
		return ""
	}
	candidate := filepath.Join(home, "hermes-setup.exe")
	if stagedFileExists(candidate) {
		return candidate
	}
	return ""
}

// Whether a staged updater is new enough to adopt a pre-written marker
func stagedUpdaterSupportsPrewrittenMarker(candidate string) bool {
	info, err := os.Stat(candidate)
	return err == nil && !info.ModTime().Before(markerSelfAdoptEpoch)
}

// Clears quarantine and ad hoc signs a macOS updater whose signature does not verify
func repairMacUpdaterHelper(updater string, isMac bool, log func(string)) {
	if !isMac || updater == "" {
		return
	}
	if err := exec.Command("/usr/bin/xattr", "-cr", updater).Run(); err != nil {
		log("[updates] macOS updater helper quarantine repair skipped: " + commandFailure("/usr/bin/xattr -cr "+updater, err))
	}
	if exec.Command("/usr/bin/codesign", "--verify", updater).Run() == nil {
		return
	}
	if err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", updater).Run(); err != nil {
		log("[updates] macOS updater helper signature repair skipped: " + commandFailure("/usr/bin/codesign --force --sign - "+updater, err))
		return
	}
	log("[updates] repaired macOS updater helper signature")
}

// Message of a failed synchronous command as execFileSync words it
func commandFailure(command string, err error) string {
	if _, ok := err.(*exec.ExitError); ok {
		return "Command failed: " + command
	}
	return hardening.NodeErr(err).Error()
}

// UpdaterChild is a spawned updater and the wait that reaps it
type UpdaterChild struct {
	cmd      *exec.Cmd
	startErr error
	done     chan struct{}
}

// PID of the spawned updater, zero when the spawn failed
func (c *UpdaterChild) PID() int {
	if c.startErr != nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// Spawns an updater with null stdio and a hidden console, detached when asked
func spawnUpdaterProcess(updater string, args []string, dir string, env []string, detached bool) *UpdaterChild {
	cmd := exec.Command(updater, args...)
	cmd.Dir, cmd.Env = dir, env
	detachUpdater(cmd, detached)
	child := &UpdaterChild{cmd: cmd, done: make(chan struct{})}
	if child.startErr = cmd.Start(); child.startErr != nil {
		close(child.done)
		return child
	}
	go func() {
		_ = cmd.Wait()
		close(child.done)
	}()
	return child
}

// UpdaterHandoffOutcome is whether a spawned updater survived its settle window
type UpdaterHandoffOutcome struct {
	OK      bool
	Reason  string
	Message string
}

// Waits out the settle window, failing on a spawn error or an early non zero or signal exit
func observeUpdaterHandoff(child *UpdaterChild, settle time.Duration) UpdaterHandoffOutcome {
	if child.startErr != nil {
		detail := hardening.NodeCode(child.startErr)
		if detail == "" {
			detail = child.startErr.Error()
		}
		return UpdaterHandoffOutcome{Reason: "spawn-error", Message: "updater spawn failed: " + detail}
	}
	timer := time.NewTimer(settle)
	defer timer.Stop()
	select {
	case <-timer.C:
		return UpdaterHandoffOutcome{OK: true}
	case <-child.done:
	}
	state := child.cmd.ProcessState
	if state == nil {
		return UpdaterHandoffOutcome{OK: true}
	}
	if signal := exitSignalName(state); signal != "" {
		return UpdaterHandoffOutcome{Reason: "early-exit", Message: fmt.Sprintf("updater died from signal %s before the settle window elapsed", signal)}
	}
	if code := state.ExitCode(); code != 0 {
		return UpdaterHandoffOutcome{Reason: "early-exit", Message: fmt.Sprintf("updater exited %d before the settle window elapsed", code)}
	}
	return UpdaterHandoffOutcome{OK: true}
}

// User facing text of an updater that could not start
func describeUpdaterHandoffFailure(outcome UpdaterHandoffOutcome) string {
	lead := "The updater couldn't start, so nothing was changed and Icebath keeps running as before. Try again; if it keeps failing, open the logs and send them to support."
	if outcome.Message != "" {
		return lead + "\n\nDetails: " + outcome.Message
	}
	return lead
}
