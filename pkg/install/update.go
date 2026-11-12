package install

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	desktopExitWait = 20 * time.Second
	desktopExitPoll = 500 * time.Millisecond
	stdoutTailLines = 40
	// How long a finished child's pipes may stay open under a surviving descendant
	DrainGrace = 20 * time.Second
)

// Synthetic update manifest, the desktop refresh and the macOS app swap each listed only when they run
func UpdateStages(includeDesktop, includeInstall bool) []StageInfo {
	stage := func(name, title string) StageInfo {
		return StageInfo{Name: name, Title: title, Category: "update"}
	}
	stages := []StageInfo{stage("handoff", "Preparing to update"), stage("update", "Downloading the latest version")}
	if includeDesktop {
		stages = append(stages, stage("desktop", "Updating the desktop app"))
	}
	if includeInstall {
		stages = append(stages, stage("install", "Installing the update"))
	}
	return stages
}

// Desktop executables an update replaces, the files a still running desktop holds open
func InstallLockProbePaths(installRoot string) []string {
	return desktopCandidates(DesktopReleaseDir(installRoot), runtime.GOOS)
}

// Reports a file another process holds so it cannot be opened for writing
func IsLocked(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return true
	}
	f.Close()
	return false
}

// Probe paths that are locked now
func LockedPaths(paths []string) []string {
	var out []string
	for _, p := range paths {
		if IsLocked(p) {
			out = append(out, p)
		}
	}
	return out
}

// Waits for the old desktop to release the files the update replaces, refusing after the exit wait
func waitForInstallLocksFree(ctx context.Context, installRoot string, emit func(Event), stage string) error {
	targets := InstallLockProbePaths(installRoot)
	deadline := time.Now().Add(desktopExitWait)
	emitLog(emit, stage, Stdout, "[handoff] waiting for Icebath to exit…")
	for {
		locked := LockedPaths(targets)
		if len(locked) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Desktop application files are still locked: %s. Close the other Icebath window and retry.", strings.Join(locked, ", "))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(desktopExitPoll):
		}
	}
}

// Message of an exit 2 refusal, the child's own refusal block when it printed one
func ConcurrentUpdateMessage(stdoutTail []string) string {
	for i := len(stdoutTail) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimLeft(stdoutTail[i], " \t"), "✗") {
			return strings.TrimSpace(strings.Join(stdoutTail[i:], "\n"))
		}
	}
	return "Icebath is still running. Close all Icebath windows and try the update again."
}

// Environment of update children, naming our pid so the child runs under this updater's marker claim
func UpdateChildEnv(installRoot string) []string {
	return []string{
		"ICEBATH_HOME=" + Home(),
		updaterecord.HandoffPIDEnv + "=" + itoa(os.Getpid()),
		"ICEBATH_INSTALL_ROOT=" + installRoot,
	}
}

// Command the install publishes for itself, the checkout launcher then the binary then a verified user bin convenience
func ResolveInstalledCommand(ctx context.Context, installRoot string) string {
	names := []string{"icebath"}
	if runtime.GOOS == "windows" {
		names = []string{"icebath.exe", "icebath.cmd"}
	}
	for _, name := range names {
		if p := filepath.Join(installRoot, ".icebath", "bin", name); isFile(p) {
			return p
		}
	}
	if p := InstallBinaryPath(installRoot); isFile(p) {
		return p
	}
	dirs := []string{filepath.Join(Home(), "bin")}
	if userHome, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(userHome, ".local", "bin"))
	}
	dirs = append(dirs, filepath.Join(filepath.Dir(installRoot), "bin"))
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		dirs = append(dirs, filepath.Join(local, "icebath", "bin"))
	}
	for _, dir := range dirs {
		for _, name := range names {
			if p := filepath.Join(dir, name); isFile(p) && launcherTargetsInstallation(ctx, p, installRoot) {
				return p
			}
		}
	}
	return ""
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Reports a launcher whose --version names root as its install directory
func launcherTargetsInstallation(ctx context.Context, launcher, root string) bool {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, launcher, "--version")
	cmd.Dir = root
	hideConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if reported, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "Install directory: "); ok {
			a, errA := filepath.EvalSymlinks(strings.TrimSpace(reported))
			b, errB := filepath.EvalSymlinks(root)
			return errA == nil && errB == nil && a == b
		}
	}
	return false
}

func emitLog(emit func(Event), stage string, stream LogStream, line string) {
	if stage != "" {
		slog.Info(line, "target", "bootstrap.log", "stage", stage)
	} else {
		slog.Info(line, "target", "bootstrap.log")
	}
	emit(LogEvent(stage, line, stream))
}

func emitStage(emit func(Event), name string, state StageState, durationMs *int64, errText *string) {
	attrs := []any{"stage", name, "state", string(state)}
	if durationMs != nil {
		attrs = append(attrs, "duration_ms", *durationMs)
	}
	if errText != nil {
		attrs = append(attrs, "error", *errText)
	}
	slog.Info("update stage", attrs...)
	emit(StageEvent(name, state, durationMs, nil, errText))
}

func since(t time.Time) *int64 { return int64Ptr(time.Since(t).Milliseconds()) }

type cmdResult struct {
	exitCode   int
	stdoutTail []string
}

// Streams each output line as a log event, abandoning pipes a descendant holds past the drain grace
func runStreamed(ctx context.Context, emit func(Event), program string, args []string, dir string, env []string, stage string) (cmdResult, error) {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = DrainGrace
	hideConsole(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return cmdResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return cmdResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return cmdResult{}, fmt.Errorf("spawning %s %v: %w", program, args, err)
	}
	var tail []string
	var tailMu sync.Mutex
	var wg sync.WaitGroup
	pump := func(r io.Reader, stream LogStream) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.ToValidUTF8(strings.TrimRight(sc.Text(), "\r"), "�")
			emitLog(emit, stage, stream, line)
			if stream == Stdout {
				tailMu.Lock()
				if len(tail) == stdoutTailLines {
					tail = tail[1:]
				}
				tail = append(tail, line)
				tailMu.Unlock()
			}
		}
	}
	wg.Add(2)
	go pump(stdout, Stdout)
	go pump(stderr, Stderr)
	waitErr := cmd.Wait()
	wg.Wait()
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		note := fmt.Sprintf("%s exited but a surviving descendant still holds its stdout/stderr; gave up on the last %ds of output (#90455)", program, int(DrainGrace.Seconds()))
		slog.Warn(note)
		emitLog(emit, stage, Stderr, note)
	}
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return cmdResult{exitCode: code, stdoutTail: tail}, nil
}

// Update argv, event sink and the exit the host performs once the fresh desktop is launched
type UpdateHost struct {
	Args []string
	Emit func(Event)
	Exit func()
}

// Hand off update of an existing install, marker held throughout and the fresh desktop relaunched at the end
func RunUpdate(ctx context.Context, host UpdateHost) error {
	emit := func(e Event) { host.Emit(e.SanitizedForUI()) }
	failed := func(stage, msg string) error {
		emit(FailedEvent(stage, msg))
		return errors.New(msg)
	}
	installRoot := InstallRootOf(Home())
	guard, refusal := AcquireUpdateGuard(ctx, UpdateMarkerPath(), installRoot)
	if guard == nil {
		return failed("", refusal)
	}
	defer guard.Complete(ctx)
	target := UpdateTargetFromArgs(host.Args)
	targetApp := ""
	if runtime.GOOS == "darwin" {
		targetApp = TargetAppFromArgs(host.Args)
	}
	icebath := ResolveInstalledCommand(ctx, installRoot)
	if icebath == "" {
		return failed("", fmt.Sprintf("Could not find the icebath CLI under %s. Is Icebath installed? Re-run the installer to repair the install.", installRoot))
	}
	desktop := DesktopProductPresent(installRoot)
	emit(ManifestEvent(UpdateStages(desktop, targetApp != ""), nil))

	started := time.Now()
	emitStage(emit, "handoff", StateRunning, nil, nil)
	if err := waitForInstallLocksFree(ctx, installRoot, emit, "handoff"); err != nil {
		emitStage(emit, "handoff", StateFailed, since(started), strPtr(err.Error()))
		return failed("handoff", err.Error())
	}
	emitStage(emit, "handoff", StateSucceeded, since(started), nil)

	emitLog(emit, "update", Stdout, "[update] updating against "+strings.Join(target, " "))
	env := UpdateChildEnv(installRoot)
	args := append([]string{"update", "--yes", "--gateway"}, target...)
	emitStage(emit, "update", StateRunning, nil, nil)
	started = time.Now()
	res, err := runStreamed(ctx, emit, icebath, args, installRoot, env, "update")
	if err != nil {
		return failed("update", err.Error())
	}
	if ShouldHealSelfMarkerRefusal(ctx, res.exitCode, UpdateMarkerPath(), installRoot) {
		emitLog(emit, "update", Stdout, "[update] child refused over this updater's own marker (handoff not recognized); clearing the claim and retrying once…")
		guard.Complete(ctx)
		if res, err = runStreamed(ctx, emit, icebath, args, installRoot, env, "update"); err != nil {
			return failed("update", err.Error())
		}
	}
	switch res.exitCode {
	case 0:
		emitStage(emit, "update", StateSucceeded, since(started), nil)
	case updaterecord.UpdateExitConcurrent:
		msg := ConcurrentUpdateMessage(res.stdoutTail)
		emitStage(emit, "update", StateFailed, since(started), strPtr(msg))
		return failed("update", msg)
	default:
		msg := fmt.Sprintf("icebath update failed (exit %d). See %s for details.", res.exitCode, filepath.Join(Home(), "logs", "update.log"))
		emitStage(emit, "update", StateFailed, since(started), strPtr(msg))
		return failed("update", msg)
	}

	if desktop {
		started = time.Now()
		emitStage(emit, "desktop", StateRunning, nil, nil)
		if err := refreshDesktop(ctx, installRoot, emit); err != nil {
			emitStage(emit, "desktop", StateFailed, since(started), strPtr(err.Error()))
			return failed("desktop", err.Error())
		}
		emitStage(emit, "desktop", StateSucceeded, since(started), nil)
	}

	launchTarget := ""
	if targetApp != "" {
		started = time.Now()
		emitStage(emit, "install", StateRunning, nil, nil)
		installed, err := installMacOSAppUpdate(ctx, installRoot, targetApp, emit)
		if err != nil {
			emitStage(emit, "install", StateFailed, since(started), strPtr(err.Error()))
			return failed("install", err.Error())
		}
		emitStage(emit, "install", StateSucceeded, since(started), nil)
		launchTarget = installed
	}

	emit(CompleteEvent(installRoot, nil))
	guard.Complete(ctx)
	if launchTarget != "" {
		if err := OpenMacOSAppDetached(launchTarget); err != nil {
			emitLog(emit, "", Stderr, fmt.Sprintf("[update] could not auto-launch desktop: launching %s: %v. Launch Icebath manually.", launchTarget, err))
		} else {
			time.Sleep(150 * time.Millisecond)
		}
	} else if err := LaunchDesktop(installRoot); err != nil {
		emitLog(emit, "", Stdout, fmt.Sprintf("[update] could not auto-launch desktop: %v. Launch Icebath manually.", err))
	}
	if host.Exit != nil {
		host.Exit()
	}
	return nil
}

// Replaces the installed desktop app with the one of the release the updated binary names
func refreshDesktop(ctx context.Context, installRoot string, emit func(Event)) error {
	tag := InstalledRelease(ctx, InstallBinaryPath(installRoot)).Tag
	if tag == "" {
		return fmt.Errorf("the updated icebath binary names no release to fetch the desktop app from")
	}
	r, err := NewRunner(Options{NonInteractive: true, Dir: installRoot}, func(e Event) {
		if e.Type == EventLog {
			e.Stage = strPtr("desktop")
		}
		emit(e)
	})
	if err != nil {
		return err
	}
	r.stage = "desktop"
	repo, err := ParseRepo(r.opts.RepoURL)
	if err != nil {
		return err
	}
	return r.installDesktopRelease(ctx, targetForTag(ctx, repo, tag))
}

// Copies the installed app bundle over the macOS launch target with a rollback swap, clearing quarantine
func installMacOSAppUpdate(ctx context.Context, installRoot, targetApp string, emit func(Event)) (string, error) {
	if filepath.Ext(targetApp) != ".app" {
		return "", fmt.Errorf("refusing to install update into non-app path: %s", targetApp)
	}
	app := ResolveDesktopApp(installRoot)
	if app == "" {
		return "", fmt.Errorf("desktop update succeeded but no Icebath.app was found under %s", DesktopReleaseDir(installRoot))
	}
	if samePath(app, targetApp) {
		emitLog(emit, "install", Stdout, "[update] updated app is already the launch target: "+targetApp)
		return targetApp, nil
	}
	emitLog(emit, "install", Stdout, fmt.Sprintf("[update] installing updated app %s -> %s", app, targetApp))
	if err := os.MkdirAll(filepath.Dir(targetApp), 0o777); err != nil {
		return "", err
	}
	tmp, old := targetApp+".icebath-update-new", targetApp+".icebath-update-old"
	os.RemoveAll(tmp)
	os.RemoveAll(old)
	ditto := exec.CommandContext(ctx, "/usr/bin/ditto", app, tmp)
	ditto.Dir = Home()
	if err := ditto.Run(); err != nil {
		return "", fmt.Errorf("ditto failed while copying updated app into %s: %v", tmp, err)
	}
	if err := SwapInNewBundle(tmp, targetApp, old); err != nil {
		return "", err
	}
	xattr := exec.CommandContext(ctx, "/usr/bin/xattr", "-dr", "com.apple.quarantine", targetApp)
	xattr.Dir = Home()
	_ = xattr.Run()
	return targetApp, nil
}
