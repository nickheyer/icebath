package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	previousAppKept       = "  ↩ The previous desktop app was left untouched and still works."
	desktopStagingPrefix  = ".staging-"
	desktopPreviousSuffix = ".previous"
)

var (
	htmlTagWithURL = regexp.MustCompile(`(?i)<(?:script|link)\b[^>]*\b(?:src|href)=["']([^"']+)["'][^>]*>`)
	moduleTag      = regexp.MustCompile(`(?i)\btype=["']module["']|\brel=["']modulepreload["']`)
	absoluteURL    = regexp.MustCompile(`(?i)^[a-z]+:|^//`)
	peMachineNames = map[uint16]string{peMachineI386: "x86 (32-bit)", peMachineAMD64: "x64 (AMD64)", peMachineARM64: "ARM64"}
)

// Pause between promotion rename attempts, the sleep hermes tests record
var desktopSleep = time.Sleep

// Backoff of a promotion rename riding out a transient scanner file lock
var desktopSwapRenameRetryDelays = []time.Duration{500 * time.Millisecond, time.Second, time.Second, time.Second}

// Rename of the stage and swap promotion, the os rename hermes tests replace
var desktopRename = os.Rename

// Renderer dist a launch loads, the source dist or the packaged unpacked copy, empty without one
var rendererBundleDir = func(desktopDir string, sourceMode bool) string {
	if sourceMode {
		return filepath.Join(desktopDir, "dist")
	}
	resources := packagedResourcesDir(desktopDir)
	if resources == "" {
		return ""
	}
	return filepath.Join(resources, "app.asar.unpacked", "dist")
}

// Packaged app resources dir holding the renderer bundle and baked install stamp, empty without a package
func packagedResourcesDir(desktopDir string) string {
	exe, ok := desktopPackagedExecutable(desktopDir)
	if !ok {
		return ""
	}
	if desktopGOOS() == "darwin" {
		return filepath.Join(filepath.Dir(filepath.Dir(exe)), "Resources")
	}
	return filepath.Join(filepath.Dir(exe), "resources")
}

// True when index.html names module chunks of this bundle that are missing
func rendererBundleTorn(distDir string) bool {
	raw, err := os.ReadFile(filepath.Join(distDir, "index.html"))
	if err != nil {
		return false
	}
	html := strings.TrimPrefix(pystr.DecodeUTF8Replace(raw), "\ufeff")
	for _, match := range htmlTagWithURL.FindAllStringSubmatch(html, -1) {
		href := match[1]
		if !moduleTag.MatchString(match[0]) || absoluteURL.MatchString(href) {
			continue
		}
		rel, _, _ := strings.Cut(href, "?")
		rel, _, _ = strings.Cut(rel, "#")
		rel = strings.TrimLeft(rel, "./")
		if rel != "" && !desktopPathExists(filepath.Join(distDir, filepath.FromSlash(rel))) {
			return true
		}
	}
	return false
}

// True when the packaged node-pty carries no native binary for this OS
func packagedNodePtyMissing(distDir string) bool {
	root := filepath.Join(distDir, "node_modules", "node-pty")
	if info, err := os.Stat(filepath.Join(root, "package.json")); err != nil || !info.Mode().IsRegular() {
		return false
	}
	dirs := []string{filepath.Join(root, "build", "Release")}
	prebuilds, _ := filepath.Glob(filepath.Join(root, "prebuilds", pySysPlatform()+"-*"))
	dirs = append(dirs, prebuilds...)
	for _, d := range dirs {
		if !desktopIsDir(d) {
			continue
		}
		found := false
		_ = filepath.WalkDir(d, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && strings.HasSuffix(entry.Name(), ".node") && path != d {
				found = true
				return filepath.SkipAll
			}
			return nil
		})
		if found {
			return false
		}
	}
	return true
}

// Python sys.platform of this OS
func pySysPlatform() string {
	switch desktopGOOS() {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	case "linux":
		return "linux"
	}
	return desktopGOOS()
}

// True when the desktop output is stale, missing, torn or built in the other mode
var desktopBuildNeeded = func(ctx context.Context, desktopDir, projectRoot string, sourceMode bool) bool {
	if sourceMode {
		if !desktopDistExists(desktopDir) {
			return true
		}
	} else if _, ok := desktopPackagedExecutable(desktopDir); !ok {
		return true
	}
	distDir := rendererBundleDir(desktopDir, sourceMode)
	if distDir != "" && rendererBundleTorn(distDir) {
		fmt.Fprintf(desktopOut, "  ⚠ A previous update left the desktop bundle incomplete (%s); rebuilding it\n", distDir)
		return true
	}
	if !sourceMode && distDir != "" && packagedNodePtyMissing(distDir) {
		fmt.Fprintln(desktopOut, "  ⚠ The packaged desktop app has no node-pty native binary; rebuilding it")
		return true
	}
	return distDir == "" || !sourceProductCurrent(ctx, projectRoot, "desktop", distDir)
}

// True when the packaged app was built from HEAD and none of its inputs changed since
func packagedDesktopCurrentForHead(ctx context.Context, desktopDir, projectRoot string) bool {
	resources := packagedResourcesDir(desktopDir)
	if resources == "" {
		return false
	}
	commit, ok := pm.ReadInstallStamp(resources).GetOr("commit", nil).(string)
	if !ok || commit != updaterecord.RunGitQueryText(projectRoot, "rev-parse", "HEAD") {
		return false
	}
	return !desktopBuildNeeded(ctx, desktopDir, projectRoot, false)
}

// Rename retrying a transient permission refusal with bounded backoff, the last refusal returned
func renameRidingOutFileLock(src, dst string) error {
	total := len(desktopSwapRenameRetryDelays) + 1
	for attempt, delay := range desktopSwapRenameRetryDelays {
		err := desktopRename(src, dst)
		if err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrPermission) {
			return err
		}
		slog.Warn(fmt.Sprintf("desktop promotion rename %s -> %s hit a file lock (attempt %d/%d), retrying in %.1fs: %s",
			filepath.Base(src), filepath.Base(dst), attempt+1, total, delay.Seconds(), pypath.OSErrorText(err)))
		desktopSleep(delay)
	}
	return desktopRename(src, dst)
}

// Fresh staging dir beside release, sweeping leftover staging dirs first
func desktopStagingDir(desktopDir string) string {
	stale, _ := filepath.Glob(filepath.Join(desktopDir, desktopStagingPrefix+"*"))
	for _, dir := range stale {
		_ = pyfs.RmTree(dir, nil)
	}
	return filepath.Join(desktopDir, fmt.Sprintf("%s%d-%d", desktopStagingPrefix, os.Getpid(), time.Now().Unix()))
}

// Dir directly under the release dir that holds the executable
func desktopUnpackedRoot(exe, releaseDir string) (string, error) {
	unpacked := exe
	for filepath.Dir(unpacked) != releaseDir {
		if filepath.Dir(unpacked) == unpacked {
			return "", desktopPyError{"ValueError", fmt.Sprintf("%s is not under %s", exe, releaseDir)}
		}
		unpacked = filepath.Dir(unpacked)
	}
	return unpacked, nil
}

// Restores an app an interrupted swap moved aside, else removes the leftover aside copy
func recoverInterruptedSwap(live, aside string) {
	if !desktopPathExists(live) && desktopIsDir(aside) {
		if err := desktopRename(aside, live); err != nil {
			slog.Warn(fmt.Sprintf("could not restore %s from %s: %s", live, aside, pypath.OSErrorText(err)))
			return
		}
		slog.Warn(fmt.Sprintf("restored %s from an interrupted desktop app swap", live))
		return
	}
	_ = pyfs.RmTree(aside, nil)
}

// First swap rename, on Windows naming cwd holders and retrying once after consent closed them
func moveLiveAppAside(liveRoot, previous string, heldBy *[]releaseHolder, consent holderConsent) error {
	err := renameRidingOutFileLock(liveRoot, previous)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrPermission) || desktopGOOS() != "windows" {
		return err
	}
	holders, closed := releaseAfterRefusedRename(liveRoot, consent)
	if len(closed) == 0 {
		if heldBy != nil {
			*heldBy = append(*heldBy, holders...)
		}
		return err
	}
	if err := renameRidingOutFileLock(liveRoot, previous); err != nil {
		if errors.Is(err, fs.ErrPermission) && heldBy != nil {
			*heldBy = append(*heldBy, findReleaseCwdHolders(liveRoot)...)
		}
		return err
	}
	return nil
}

// Promotes a verified staged pack over release by two renames, the live exe or empty with the live app kept
func swapStagedDesktopApp(desktopDir, stagingDir string, heldBy *[]releaseHolder) string {
	releaseDir := filepath.Join(desktopDir, "release")
	consent := takeConsent(releaseDir, pystr.Strip(os.Getenv("ICEBATH_UPDATE_CORRELATION_ID")))
	stagedExe, ok := desktopPackagedExecutableIn(stagingDir)
	if !ok {
		_ = pyfs.RmTree(stagingDir, nil)
		return ""
	}
	defer func() { _ = pyfs.RmTree(stagingDir, nil) }()
	live, err := func() (string, error) {
		stagedRoot, err := desktopUnpackedRoot(stagedExe, stagingDir)
		if err != nil {
			return "", err
		}
		liveRoot := filepath.Join(releaseDir, filepath.Base(stagedRoot))
		previous := filepath.Join(releaseDir, filepath.Base(stagedRoot)+desktopPreviousSuffix)
		if err := os.MkdirAll(releaseDir, 0o777); err != nil {
			return "", err
		}
		recoverInterruptedSwap(liveRoot, previous)
		movedAside := desktopPathExists(liveRoot)
		if movedAside {
			if stopped := stopDesktopProcessesLockingBuild(desktopDir, true); len(stopped) > 0 {
				slog.Info(fmt.Sprintf("stopped desktop processes before staged app promotion: %v", stopped))
			}
			if err := moveLiveAppAside(liveRoot, previous, heldBy, consent); err != nil {
				return "", err
			}
		}
		if err := renameRidingOutFileLock(stagedRoot, liveRoot); err != nil {
			if movedAside {
				if rerr := renameRidingOutFileLock(previous, liveRoot); rerr != nil {
					return "", rerr
				}
			}
			return "", err
		}
		if movedAside {
			_ = pyfs.RmTree(previous, nil)
		}
		rel, err := filepath.Rel(stagedRoot, stagedExe)
		if err != nil {
			return "", err
		}
		return filepath.Join(liveRoot, rel), nil
	}()
	if err != nil {
		slog.Warn("desktop stage-and-swap failed, live app kept: " + desktopErrText(err))
		return ""
	}
	return live
}

// Removes a staging dir and everything in it
func discardDesktopStaging(stagingDir string) { _ = pyfs.RmTree(stagingDir, nil) }

// Why an executable cannot run on this Windows host, empty when it parses as a loadable PE
func desktopExeIntegrityError(path string) string {
	machine, err := parsePEMachine(path)
	if err != nil {
		return err.Error()
	}
	if !expectedWindowsPEMachines()[machine] {
		got, ok := peMachineNames[machine]
		if !ok {
			got = fmt.Sprintf("unknown machine 0x%04X", machine)
		}
		return fmt.Sprintf("architecture mismatch: built a %s executable but this is a %s Windows host", got, windowsNativeMachine())
	}
	return ""
}

// Installed Electron package dir, workspace local when present else the root hoist
func electronDir(projectRoot string) string {
	local := filepath.Join(projectRoot, "apps", "desktop", "node_modules", "electron")
	if desktopPathExists(local) {
		return local
	}
	return filepath.Join(projectRoot, "node_modules", "electron")
}

// True when the process executable lives inside the release dir
func runsFrom(pid int, releaseDir string) bool {
	exe, err := proc.Exe(pid)
	if err != nil || exe == "" {
		return false
	}
	resolved, err := pypath.Resolve(exe)
	return err == nil && pathUnder(resolved, releaseDir)
}

// True when path lies strictly below dir
func pathUnder(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Pid of a desktop from this build's release tree that is an ancestor of this process, zero when none
func desktopAncestorIn(desktopDir string) int {
	releaseDir, err := pypath.Resolve(filepath.Join(desktopDir, "release"))
	if err != nil {
		return 0
	}
	ancestors, err := proc.Parents(os.Getpid())
	if err != nil {
		return 0
	}
	for _, pid := range ancestors {
		if runsFrom(pid, releaseDir) {
			return pid
		}
	}
	return 0
}

// Terminates desktop processes running from this build's release tree, sparing ancestor desktops and their trees
var stopDesktopProcessesLockingBuild = func(desktopDir string, alsoPosix bool) []int {
	if desktopGOOS() != "windows" && !alsoPosix {
		return nil
	}
	releaseDir, err := pypath.Resolve(filepath.Join(desktopDir, "release"))
	if err != nil || !desktopIsDir(releaseDir) {
		return nil
	}
	me := os.Getpid()
	spared := map[int]bool{}
	ancestors, err := proc.Parents(me)
	if err != nil {
		ancestors = nil
	}
	for _, parent := range ancestors {
		spared[parent] = true
		if !runsFrom(parent, releaseDir) {
			continue
		}
		if table, err := proc.Snapshot(); err == nil {
			for _, child := range table.Descendants(parent) {
				spared[child] = true
			}
		}
	}
	pids, err := proc.PIDs()
	if err != nil {
		return nil
	}
	var victims []int
	for _, pid := range pids {
		if pid == me || spared[pid] {
			continue
		}
		exe, err := proc.Exe(pid)
		if err != nil || exe == "" {
			continue
		}
		resolved, err := pypath.Resolve(exe)
		if err != nil {
			continue
		}
		if pathUnder(resolved, releaseDir) {
			victims = append(victims, pid)
		}
	}
	var stopped []int
	for _, pid := range victims {
		if proc.Terminate(pid) == nil {
			stopped = append(stopped, pid)
		}
	}
	if len(stopped) > 0 {
		alive := waitPIDsGone(victims, 5*time.Second)
		var killed []int
		for _, pid := range alive {
			if proc.KillPID(pid) == nil {
				killed = append(killed, pid)
			}
		}
		if len(killed) > 0 {
			waitPIDsGone(killed, 5*time.Second)
		}
	}
	return stopped
}

// Signs and verifies the staged app before swapping it over the live one, the live executable
func promoteStagedDesktopApp(desktopDir, stagingDir string, integrityCheck func(string) string) (string, error) {
	stagedExe, staged := desktopPackagedExecutableIn(stagingDir)
	if !desktopMacosRelaunchableFixup(desktopDir, nil, stagingDir) {
		discardDesktopStaging(stagingDir)
		fmt.Fprintln(desktopOut, "✗ The rebuilt desktop app could not be signed with a stable identity; not promoting it.")
		return "", desktopPyError{"RuntimeError", "Desktop signing refused the staged build. " + previousAppKept}
	}
	if integrityCheck == nil && desktopGOOS() == "windows" {
		integrityCheck = desktopExeIntegrityError
	}
	reason := ""
	if staged && integrityCheck != nil {
		reason = integrityCheck(stagedExe)
	}
	if !staged || reason != "" {
		discardDesktopStaging(stagingDir)
		if !staged {
			fmt.Fprintf(desktopOut, "✗ Desktop build produced no launchable app in %s\n", stagingDir)
		} else {
			fmt.Fprintf(desktopOut, "✗ The built %s failed its integrity check: %s\n    at: %s\n", filepath.Base(stagedExe), reason, stagedExe)
		}
		return "", desktopPyError{"RuntimeError", "Desktop build produced no launchable app. " + previousAppKept}
	}
	var heldBy []releaseHolder
	packaged := swapStagedDesktopApp(desktopDir, stagingDir, &heldBy)
	if packaged == "" && len(heldBy) > 0 {
		fmt.Fprintln(desktopOut, describeBlock(heldBy, filepath.Join(desktopDir, "release")))
		return "", desktopPyError{"RuntimeError", fmt.Sprintf("Could not publish the desktop build: %s is using its folder. %s", holderNames(heldBy), previousAppKept)}
	}
	if packaged == "" {
		fmt.Fprintf(desktopOut, "✗ Could not install the rebuilt desktop app into %s\n", filepath.Join(desktopDir, "release"))
		return "", desktopPyError{"RuntimeError", "Could not publish the desktop build. " + previousAppKept}
	}
	return packaged, nil
}

// Prints the npm ignore-scripts remedy when a build failed for a missing esbuild binary
func diagnoseEsbuildIgnoreScripts(output string) {
	if !(strings.Contains(output, "@esbuild/") && strings.Contains(output, "could not be found")) && !strings.Contains(output, "ignore-scripts") {
		return
	}
	fmt.Fprintln(desktopOut, "  ⚠ This looks like esbuild's native binary is missing — commonly caused by")
	fmt.Fprintln(desktopOut, "    `ignore-scripts=true` in your npm config, which skips esbuild's postinstall")
	fmt.Fprintln(desktopOut, "    that stages the @esbuild/<platform> package.")
	fmt.Fprintln(desktopOut, "    Fix: run `npm config get ignore-scripts` — if true, either set it to false")
	fmt.Fprintln(desktopOut, "    (`npm config set ignore-scripts false`), then reinstall: `npm ci` in the repo root,")
	fmt.Fprintln(desktopOut, "    or stage the binary directly: `node node_modules/esbuild/install.js` in apps/desktop.")
}

// Builds prepared desktop sources then publishes the verified staged app, the packaged executable
func BuildPreparedDesktop(ctx context.Context, desktopDir string, sourceMode bool, npm string, env map[string]string, icons string) (string, error) {
	if !sourceMode && desktopGOOS() == "windows" {
		if ancestor := desktopAncestorIn(desktopDir); ancestor != 0 {
			fmt.Fprintf(desktopOut, "  ⚠ Skipped rebuilding the desktop app: this update is running inside it (pid %d),\n", ancestor)
			fmt.Fprintln(desktopOut, "    and Windows locks a running app's files. Quit Icebath Desktop and run `icebath desktop`")
			fmt.Fprintln(desktopOut, "    from a terminal, or use Update now in Settings → About, to rebuild and reopen it.")
			return "", nil
		}
	}
	label := "packaged app"
	if sourceMode {
		label = "source build"
	}
	buildEnv := make(map[string]string, len(env))
	for k, v := range env {
		buildEnv[k] = v
	}
	if desktopGOOS() == "windows" {
		runner, err := sourceEnsure(ctx, "git", pm.EnsureOptions{BaseEnv: buildEnv})
		if err != nil {
			return "", err
		}
		buildEnv = runner.Env
	}
	if forceAdhocMacosSigning(buildEnv, sourceMode) {
		fmt.Fprintln(desktopOut, "  → No Developer ID configured; ad-hoc signing this local rebuild (CSC_IDENTITY_AUTO_DISCOVERY=false)")
	}
	buildCmd := []string{npm, "run", "build", "--"}
	if icons != "" {
		buildCmd = append(buildCmd, "--icons", icons)
	}
	stagingDir := ""
	if !sourceMode {
		stagingDir = desktopStagingDir(desktopDir)
		if stopped := stopDesktopProcessesLockingBuild(desktopDir, false); len(stopped) > 0 {
			parts := make([]string, len(stopped))
			for i, pid := range stopped {
				parts[i] = fmt.Sprint(pid)
			}
			fmt.Fprintf(desktopOut, "  ⚠ Stopped running desktop app to free the build output (pid %s)\n", strings.Join(parts, ", "))
		}
		defer discardDesktopStaging(stagingDir)
	}
	projectRoot := filepath.Dir(filepath.Dir(desktopDir))
	diagnose := func(err error) error {
		var called *pm.CalledProcessError
		if errors.As(err, &called) {
			diagnoseEsbuildIgnoreScripts(called.Output)
		}
		return err
	}
	if npm == "" {
		return "", sourceNoneArgvError{}
	}
	if _, err := RunSourceInCustody(ctx, projectRoot, buildCmd, "Building desktop "+label, containedRunOptions{Dir: desktopDir, Env: buildEnv}); err != nil {
		return "", diagnose(err)
	}
	if stagingDir == "" {
		return "", nil
	}
	productSteps.publishStage("Packaging the desktop app")
	if _, err := RunSourceInCustody(ctx, projectRoot, []string{npm, "run", "builder", "--", "--dir", "--publish", "never",
		"-c.directories.output=" + stagingDir}, "Packaging the desktop app", containedRunOptions{Dir: desktopDir, Env: buildEnv}); err != nil {
		return "", diagnose(err)
	}
	return promoteStagedDesktopApp(desktopDir, stagingDir, nil)
}
