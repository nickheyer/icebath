//go:build windows

package install

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// Starts the child as a detached process with no console
func detachCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
}

// Clears the inherit flag of the installer's standard handles so the desktop never holds the caller's pipes
func prepareDetachedSpawn() {
	for _, kind := range []uint32{windows.STD_INPUT_HANDLE, windows.STD_OUTPUT_HANDLE, windows.STD_ERROR_HANDLE} {
		h, err := windows.GetStdHandle(kind)
		if err != nil || h == 0 || h == windows.InvalidHandle {
			continue
		}
		_ = windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, 0)
	}
}

// Runs update children without flashing a console behind the GUI
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

// Grants AppContainer read access on the app dir and creates Start menu and Desktop shortcuts
func (r *Runner) finishDesktopPlatform(ctx context.Context, exe string) {
	appDir := filepath.Dir(exe)
	if out, err := exec.CommandContext(ctx, "icacls", appDir, "/grant", "*S-1-15-2-2:(OI)(CI)(RX)", "/T", "/C", "/Q").CombinedOutput(); err == nil {
		r.logInfo("Granted AppContainer read access on " + appDir)
	} else {
		r.logWarn("icacls AppContainer grant failed for " + appDir + ": " + strings.TrimSpace(string(out)))
	}
	icon := exe + ",0"
	if _, err := os.Stat(filepath.Join(appDir, "resources", "icon.ico")); err == nil {
		icon = filepath.Join(appDir, "resources", "icon.ico") + ",0"
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := "$ErrorActionPreference='Stop'; $sh = New-Object -ComObject WScript.Shell; " +
		"foreach ($dir in @([Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('Desktop'))) { " +
		"if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }; " +
		"$l = $sh.CreateShortcut((Join-Path $dir 'Icebath.lnk')); $l.TargetPath = " + quote(exe) + "; " +
		"$l.WorkingDirectory = " + quote(appDir) + "; $l.IconLocation = " + quote(icon) + "; $l.Save() }"
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	hideConsole(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		r.logWarn("Could not create desktop shortcuts: " + strings.TrimSpace(string(out)))
		return
	}
	r.logSuccess("Created Start menu and Desktop shortcuts")
}

// Start menu and Desktop shortcuts the Windows desktop install creates
func shortcutPaths() []string {
	var out []string
	for _, id := range []*windows.KNOWNFOLDERID{windows.FOLDERID_Programs, windows.FOLDERID_Desktop} {
		if dir, err := windows.KnownFolderPath(id, 0); err == nil {
			out = append(out, filepath.Join(dir, "Icebath.lnk"))
		}
	}
	return out
}
