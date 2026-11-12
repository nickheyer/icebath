package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Release dir the desktop app unpacks into, the one `icebath desktop` launches from
func DesktopReleaseDir(installRoot string) string {
	return filepath.Join(installRoot, "apps", "desktop", "release")
}

// Unpacked app dir name of a platform, x64 bare and every other arch suffixed
func DesktopPlatformDir(goos, goarch string) string {
	switch goos {
	case "windows":
		if goarch == "arm64" {
			return "win-arm64-unpacked"
		}
		return "win-unpacked"
	case "darwin":
		if goarch == "arm64" {
			return "mac-arm64"
		}
		return "mac"
	}
	if goarch == "arm64" {
		return "linux-arm64-unpacked"
	}
	return "linux-unpacked"
}

// Desktop executable candidates under a release dir for goos
func desktopCandidates(release, goos string) []string {
	switch goos {
	case "windows":
		return []string{filepath.Join(release, "win-unpacked", "Icebath.exe"), filepath.Join(release, "win-arm64-unpacked", "Icebath.exe")}
	case "darwin":
		return []string{filepath.Join(release, "mac", "Icebath.app", "Contents", "MacOS", "Icebath"), filepath.Join(release, "mac-arm64", "Icebath.app", "Contents", "MacOS", "Icebath")}
	}
	return []string{filepath.Join(release, "linux-unpacked", "icebath"), filepath.Join(release, "linux-arm64-unpacked", "icebath")}
}

// Installed desktop executable of a tree, empty when none exists
func ResolveDesktopExe(installRoot string) string {
	for _, p := range desktopCandidates(DesktopReleaseDir(installRoot), runtime.GOOS) {
		if _, err := os.Stat(p); err == nil {
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

// Reports an install already carrying a desktop app on any platform layout
func DesktopProductPresent(installRoot string) bool {
	release := DesktopReleaseDir(installRoot)
	for _, d := range []string{"linux-unpacked", "linux-arm64-unpacked", "mac", "mac-arm64", "win-unpacked", "win-ia32-unpacked", "win-arm64-unpacked"} {
		if st, err := os.Stat(filepath.Join(release, d)); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

// Command that starts the desktop detached with null stdio, through LaunchServices for a macOS bundle
func desktopLaunchCommand(exe, installRoot string) *exec.Cmd {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		if app := appBundleForExe(exe); app != "" {
			cmd = exec.Command("/usr/bin/open", app)
			cmd.Dir = Home()
		}
	}
	if cmd == nil {
		cmd = exec.Command(exe)
		cmd.Dir = filepath.Dir(exe)
		if cmd.Dir == "" {
			cmd.Dir = installRoot
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	detachCommand(cmd)
	return cmd
}

// Starts the installed desktop detached, an error when none is installed or the spawn fails
func SpawnInstalledDesktop(installRoot string) error {
	exe := ResolveDesktopExe(installRoot)
	if exe == "" {
		return fmt.Errorf("no built Icebath desktop app")
	}
	cmd := desktopLaunchCommand(exe, installRoot)
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
		return fmt.Errorf("Couldn't find a built Icebath desktop at %s. The desktop build step may have been skipped or failed. Run `icebath desktop` from a terminal to build and launch it.", DesktopReleaseDir(installRoot))
	}
	cmd := desktopLaunchCommand(exe, installRoot)
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

// Downloads the desktop app of the installed binary's release and swaps it into the release dir
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

// Fetches, verifies and unpacks one release's desktop app over the installed one
func (r *Runner) installDesktopRelease(ctx context.Context, t *ReleaseTarget) error {
	goos, goarch := hostKey()
	name := DesktopAssetName(goos, goarch)
	sum, err := t.ExpectedSHA256(ctx, name)
	if err != nil {
		return fail("desktop app download failed: "+err.Error(), "products_build_failed")
	}
	release := DesktopReleaseDir(r.installDir)
	archive := filepath.Join(r.home, "bootstrap-cache", name)
	if err := r.download(ctx, fmt.Sprintf("Downloading the Icebath desktop app %s", t.Tag), t.AssetURL(name), archive, sum); err != nil {
		return err
	}
	defer os.Remove(archive)
	platformDir := DesktopPlatformDir(goos, goarch)
	staged := filepath.Join(release, "."+platformDir+".new")
	old := filepath.Join(release, "."+platformDir+".old")
	os.RemoveAll(old)
	if err := pm.Extract(archive, staged); err != nil {
		os.RemoveAll(staged)
		return fail(fmt.Sprintf("could not unpack %s: %v", name, err), "products_build_failed")
	}
	if err := SwapInNewBundle(staged, filepath.Join(release, platformDir), old); err != nil {
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

// Moves a staged tree to target, parking the existing one at old and never leaving target missing on failure
func SwapInNewBundle(tmp, target, old string) error {
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
