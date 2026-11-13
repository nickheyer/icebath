package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Stable and canary release tags a binary may carry
var releaseTagRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(\+canary\.\d{8}T\d{6}Z)?$`)

const (
	// Executable of the Wails desktop app
	desktopExecutable = "icebath-desktop"
	// Bundle the macOS desktop artifact carries
	desktopAppBundle = "Icebath.app"
)

// File name of the desktop executable on goos
func DesktopExecutableName(goos string) string {
	if goos == "windows" {
		return desktopExecutable + ".exe"
	}
	return desktopExecutable
}

// Release asset of the desktop app, the bare executable on Linux and Windows and the gzipped app bundle on macOS
func DesktopAssetName(goos, goarch string) string {
	name := desktopExecutable + "-" + goos + "-" + goarch
	switch goos {
	case "windows":
		return name + ".exe"
	case "darwin":
		return name + ".tar.gz"
	}
	return name
}

// Directory the release desktop app installs into under an install tree
func DesktopReleaseDir(installRoot string) string {
	return filepath.Join(installRoot, "desktop")
}

// Desktop executable a release dir holds on goos, inside the app bundle on macOS
func desktopCandidates(release, goos string) []string {
	return []string{DesktopExecutableIn(release, goos)}
}

// Desktop executable the release layout puts in dir on goos, inside the app bundle on macOS
func DesktopExecutableIn(dir, goos string) string {
	if goos == "darwin" {
		return filepath.Join(dir, desktopAppBundle, "Contents", "MacOS", desktopExecutable)
	}
	return filepath.Join(dir, DesktopExecutableName(goos))
}

// Release tag the running binary was stamped with or installed at, empty for a development build
func RunningReleaseTag() string {
	if v := updaterecord.BuildVersion(); releaseTagRE.MatchString(v) {
		return v
	}
	if info, ok := debug.ReadBuildInfo(); ok && stableTagRE.MatchString(info.Main.Version) {
		return info.Main.Version
	}
	return ""
}

// Installed desktop executable of a tree, empty when none exists
func ResolveDesktopExe(installRoot string) string {
	for _, p := range desktopCandidates(DesktopReleaseDir(installRoot), runtime.GOOS) {
		if isFile(p) {
			return p
		}
	}
	return ""
}

// The .app bundle holding a macOS desktop executable, empty when the layout is not a bundle
func appBundleForExe(exe string) string {
	app := filepath.Dir(filepath.Dir(filepath.Dir(exe)))
	if filepath.Ext(app) != ".app" {
		return ""
	}
	if st, err := os.Stat(app); err != nil || !st.IsDir() {
		return ""
	}
	return app
}

// Relaunch target of an install, the .app bundle on macOS and the executable elsewhere
func ResolveDesktopApp(installRoot string) string {
	exe := ResolveDesktopExe(installRoot)
	if exe == "" {
		return ""
	}
	if runtime.GOOS == "darwin" {
		return appBundleForExe(exe)
	}
	return exe
}

// Reports a completed install with a launchable desktop app
func IsInstalled(installRoot string) bool {
	if _, err := os.Stat(BootstrapMarkerPath(installRoot)); err != nil {
		return false
	}
	return ResolveDesktopExe(installRoot) != ""
}

// Reports an install already carrying the desktop app
func DesktopProductPresent(installRoot string) bool {
	return ResolveDesktopExe(installRoot) != ""
}

// Command that starts a desktop executable detached with null stdio, through LaunchServices for a macOS bundle
func DesktopLaunchCommand(exe string, args []string) *exec.Cmd {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		if app := appBundleForExe(exe); app != "" {
			openArgs := []string{app}
			if len(args) > 0 {
				openArgs = append([]string{"-n", app, "--args"}, args...)
			}
			cmd = exec.Command("/usr/bin/open", openArgs...)
			cmd.Dir = Home()
		}
	}
	if cmd == nil {
		cmd = exec.Command(exe, args...)
		cmd.Dir = filepath.Dir(exe)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	detachCommand(cmd)
	return cmd
}

// Starts the installed desktop detached, an error when none is installed or the spawn fails
func SpawnInstalledDesktop(installRoot string) error {
	exe := ResolveDesktopExe(installRoot)
	if exe == "" {
		return fmt.Errorf("no Icebath desktop app is installed under %s", DesktopReleaseDir(installRoot))
	}
	cmd := DesktopLaunchCommand(exe, nil)
	prepareDetachedSpawn()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Launches the installed desktop for the UI, naming the expected location when none is installed
func LaunchDesktop(installRoot string) error {
	exe := ResolveDesktopExe(installRoot)
	if exe == "" {
		return fmt.Errorf("Couldn't find the Icebath desktop app under %s. Run `icebath desktop` from a terminal to install and launch it.", DesktopReleaseDir(installRoot))
	}
	cmd := DesktopLaunchCommand(exe, nil)
	prepareDetachedSpawn()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to launch %s: %v", exe, err)
	}
	_ = cmd.Process.Release()
	time.Sleep(150 * time.Millisecond)
	return nil
}

// Opens a macOS app bundle detached through LaunchServices
func OpenMacOSAppDetached(app string) error {
	cmd := exec.Command("/usr/bin/open", app)
	cmd.Dir = Home()
	detachCommand(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// Start menu and Desktop shortcuts the desktop install creates, none outside Windows
func DesktopShortcutPaths() []string { return shortcutPaths() }

// Icebath source checkout holding the running binary or the working directory, empty when neither lies in one
func SourceCheckoutRoot() string {
	var starts []string
	if exe, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		starts = append(starts, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	for _, dir := range starts {
		for {
			if isIcebathCheckout(dir) {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ""
}

// Reports a directory whose go.mod declares the icebath module
func isIcebathCheckout(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	first, _, _ := strings.Cut(string(raw), "\n")
	fields := strings.Fields(first)
	return len(fields) == 2 && fields[0] == "module" && fields[1] == "github.com/nickheyer/icebath"
}

// Desktop executable make desktop leaves in a checkout, empty when it was not built
func CheckoutDesktopExe(checkout string) string {
	exe := filepath.Join(checkout, "apps", "desktop", DesktopExecutableName(runtime.GOOS))
	if isFile(exe) {
		return exe
	}
	return ""
}

// Installs the desktop artifact of tag, the newest stable release when tag is empty, and returns its executable
func InstallDesktop(ctx context.Context, tag string, out io.Writer) (string, error) {
	installRoot := InstallRootOf(Home())
	r, err := NewRunner(Options{NonInteractive: true, Dir: installRoot}, func(e Event) {
		if e.Type == EventLog {
			fmt.Fprintln(out, e.Line)
		}
	})
	if err != nil {
		return "", err
	}
	r.stage = "desktop"
	repo, err := ParseRepo(r.opts.RepoURL)
	if err != nil {
		return "", err
	}
	var target *ReleaseTarget
	if tag == "" {
		if target, err = LatestDesktopRelease(ctx, repo, "stable"); err != nil {
			return "", err
		}
	} else {
		target = targetForTag(ctx, repo, tag)
	}
	if err := r.installDesktopRelease(ctx, target); err != nil {
		return "", err
	}
	return ResolveDesktopExe(installRoot), nil
}

// Downloads the desktop app of the installed binary's release and places it in the release dir
func (r *Runner) installDesktop(ctx context.Context) error {
	tag := InstalledRelease(ctx, InstallBinaryPath(r.installDir)).Tag
	if tag == "" {
		tag = r.opts.Version
	}
	if tag == "" {
		return fail("the installed icebath binary names no release to fetch the desktop app from", "products_build_failed")
	}
	repo, err := ParseRepo(r.opts.RepoURL)
	if err != nil {
		return fail(err.Error(), "products_build_failed")
	}
	return r.installDesktopRelease(ctx, targetForTag(ctx, repo, tag))
}

// Fetches and verifies one release's desktop app and swaps it over the installed one
func (r *Runner) installDesktopRelease(ctx context.Context, t *ReleaseTarget) error {
	goos, goarch := hostKey()
	name := DesktopAssetName(goos, goarch)
	sum, err := t.ExpectedSHA256(ctx, name)
	if err != nil {
		return fail("desktop app download failed: "+err.Error(), "products_build_failed")
	}
	release := DesktopReleaseDir(r.installDir)
	staged := filepath.Join(release, "."+name+".download")
	if err := r.download(ctx, fmt.Sprintf("Downloading the Icebath desktop app %s", t.Tag), t.AssetURL(name), staged, sum); err != nil {
		os.Remove(staged)
		return err
	}
	if goos == "darwin" {
		err = placeDesktopBundle(staged, release)
	} else {
		err = placeDesktopExecutable(staged, filepath.Join(release, DesktopExecutableName(goos)))
	}
	if err != nil {
		return fail(err.Error(), "products_build_failed")
	}
	exe := ResolveDesktopExe(r.installDir)
	if exe == "" {
		return fail(fmt.Sprintf("desktop install produced no Icebath app under %s", release), "products_build_failed")
	}
	r.logSuccess("Desktop ready: " + exe)
	r.finishDesktopPlatform(ctx, exe)
	return nil
}

// Marks a downloaded desktop executable runnable and renames it over the installed one
func placeDesktopExecutable(staged, dest string) error {
	if err := os.Chmod(staged, 0o755); err != nil {
		os.Remove(staged)
		return fmt.Errorf("cannot mark %s executable: %v", staged, err)
	}
	if err := replaceFile(staged, dest); err != nil {
		return fmt.Errorf("cannot place %s: %v", dest, err)
	}
	return nil
}

// Unpacks a downloaded app bundle archive and swaps its bundle over the installed one
func placeDesktopBundle(archive, release string) error {
	defer os.Remove(archive)
	unpacked := filepath.Join(release, "."+desktopAppBundle+".new")
	defer os.RemoveAll(unpacked)
	if err := pm.Extract(archive, unpacked); err != nil {
		return fmt.Errorf("could not unpack %s: %v", filepath.Base(archive), err)
	}
	bundle := filepath.Join(unpacked, desktopAppBundle)
	if st, err := os.Stat(filepath.Join(bundle, "Contents", "MacOS", desktopExecutable)); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("%s holds no %s with Contents/MacOS/%s", filepath.Base(archive), desktopAppBundle, desktopExecutable)
	}
	target := filepath.Join(release, desktopAppBundle)
	return SwapInNewBundle(bundle, target, filepath.Join(release, "."+desktopAppBundle+".old"))
}

// Moves a staged tree to target, parking the existing one at old and never leaving target missing on failure
func SwapInNewBundle(tmp, target, old string) error {
	os.RemoveAll(old)
	movedOld := false
	if _, err := os.Lstat(target); err == nil {
		if err := os.Rename(target, old); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("could not move existing app aside at %s (leaving it in place): %v", target, err)
		}
		movedOld = true
	}
	if err := os.Rename(tmp, target); err != nil {
		if movedOld {
			_ = os.Rename(old, target)
		}
		os.RemoveAll(tmp)
		return fmt.Errorf("installing updated app at %s: %v", target, err)
	}
	os.RemoveAll(old)
	return nil
}
