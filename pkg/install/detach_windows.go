//go:build windows

package install

import (
	"context"
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

// Creates Start menu and Desktop shortcuts to the installed desktop executable
func (r *Runner) finishDesktopPlatform(ctx context.Context, exe string) {
	appDir := filepath.Dir(exe)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := "$ErrorActionPreference='Stop'; $sh = New-Object -ComObject WScript.Shell; " +
		"foreach ($dir in @([Environment]::GetFolderPath('Programs'), [Environment]::GetFolderPath('Desktop'))) { " +
		"if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }; " +
		"$l = $sh.CreateShortcut((Join-Path $dir 'Icebath.lnk')); $l.TargetPath = " + quote(exe) + "; " +
		"$l.WorkingDirectory = " + quote(appDir) + "; $l.IconLocation = " + quote(exe+",0") + "; $l.Save() }"
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
