package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

const (
	guiDesktopEntryName       = "ai.icebath.desktop"
	guiLegacyDesktopEntryName = "icebath.desktop"
)

// Python sys.platform of the running OS
func guiSysPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	}
	return runtime.GOOS
}

// Marked line printer of the uninstall screens
func guiLog(w io.Writer, mark, col, msg string) {
	fmt.Fprintf(w, "%s %s\n", cliui.Color(mark, col), msg)
}

// Path of an environment variable when set, else the fallback
func guiEnvDir(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// User home the way Path.home answers
func guiUserHome() string {
	h, err := pypath.PathHome()
	if err != nil {
		panic(err)
	}
	return h
}

// User data directory of the desktop app on this platform
func desktopUserdataDir() string {
	h := guiUserHome()
	switch guiSysPlatform() {
	case "darwin":
		return filepath.Join(h, "Library", "Application Support", "Icebath")
	case "win32":
		return filepath.Join(guiEnvDir("APPDATA", filepath.Join(h, "AppData", "Roaming")), "Icebath")
	}
	return filepath.Join(guiEnvDir("XDG_CONFIG_HOME", filepath.Join(h, ".config")), "Icebath")
}

// Desktop app the install placed under the home and the shortcuts it created
func installedGUIArtifacts(h string) []string {
	return append([]string{install.DesktopReleaseDir(install.InstallRootOf(h))}, install.DesktopShortcutPaths()...)
}

// Record of the installed app copies the update keeps current, under the default root
func desktopInstallRecordPath() string {
	return filepath.Join(home.Root(), "desktop-installed-apps.json")
}

// XDG data home, blank meaning unset
func guiXDGDataHome() string {
	if raw := os.Getenv("XDG_DATA_HOME"); raw != "" && pystr.Strip(raw) != "" {
		return pypath.Expanduser(raw)
	}
	return filepath.Join(guiUserHome(), ".local", "share")
}

// Linux desktop entry of the app id
func guiDesktopEntryPath() string {
	return filepath.Join(guiXDGDataHome(), "applications", guiDesktopEntryName)
}

// Standard install locations of the packaged desktop app on this OS, existing or not
func packagedGUIAppPaths() []string {
	h := guiUserHome()
	switch guiSysPlatform() {
	case "darwin":
		return []string{"/Applications/Icebath.app", filepath.Join(h, "Applications", "Icebath.app")}
	case "win32":
		local := guiEnvDir("LOCALAPPDATA", filepath.Join(h, "AppData", "Local"))
		out := []string{filepath.Join(local, "Programs", "Icebath"), filepath.Join(local, "icebath-desktop")}
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			out = append(out, filepath.Join(pf, "Icebath"))
		}
		return out
	}
	dataBase := guiEnvDir("XDG_DATA_HOME", filepath.Join(h, ".local", "share"))
	icons := filepath.Join(dataBase, "icons", "hicolor")
	out := []string{guiDesktopEntryPath(), filepath.Join(dataBase, "applications", guiLegacyDesktopEntryName),
		filepath.Join(dataBase, "applications", "Icebath.desktop")}
	for _, size := range []string{"scalable", "24x24", "32x32", "48x48", "256x256", "512x512", "1024x1024"} {
		out = append(out, filepath.Join(icons, size, "apps", "icebath.png"))
	}
	return out
}

// Whether a path exists the way Path.exists answers, symlinks followed
func guiExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Whether a usable agent install exists under the home, source or a venv being enough
func agentIsInstalled(h string) bool {
	for _, sub := range []string{"hermes_cli", "venv", ".venv"} {
		if st, err := os.Stat(filepath.Join(h, "icebath-agent", sub)); err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

// Whether any desktop GUI artifact or install record exists
func guiIsInstalled(h string) bool {
	paths := append(append(installedGUIArtifacts(h), packagedGUIAppPaths()...), desktopUserdataDir(), desktopInstallRecordPath())
	for _, p := range paths {
		if guiExists(p) {
			return true
		}
	}
	return false
}

// Existing paths of a list as a JSON list
func guiExistingList(paths []string) []any {
	out := []any{}
	for _, p := range paths {
		if guiExists(p) {
			out = append(out, p)
		}
	}
	return out
}

// JSON snapshot of what is installed for the desktop uninstall screen
func guiInstallSummary(h string) *pyjson.Object {
	userdata := desktopUserdataDir()
	steward, allowed := selfupdate.ClassifyInstall(filepath.Join(h, "icebath-agent"))
	return pyjson.ObjectOf("hermes_home", h, "agent_installed", agentIsInstalled(h), "gui_installed", guiIsInstalled(h),
		"source_built_artifacts", guiExistingList(installedGUIArtifacts(h)), "packaged_app_paths", guiExistingList(packagedGUIAppPaths()),
		"userdata_dir", userdata, "userdata_exists", guiExists(userdata), "platform", guiSysPlatform(),
		"steward", steward, "code_removal_allowed", allowed)
}

// Prints the install snapshot as one JSON line
func PrintGUIInstallSummary(ctx context.Context, w io.Writer) (int, error) {
	text, err := pyjson.Dumps(guiInstallSummary(home.Dir(ctx)))
	if err != nil {
		return 1, err
	}
	fmt.Fprintln(w, text)
	return 0, nil
}

// Removes a file, symlink or tree, warning and reporting false when it fails or nothing is there
func guiRemovePath(w io.Writer, path string) bool {
	err := func() error {
		if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return os.Remove(path)
		}
		st, err := os.Stat(path)
		if err != nil {
			return errNothingRemoved
		}
		if st.Mode().IsRegular() {
			return os.Remove(path)
		}
		if st.IsDir() {
			return os.RemoveAll(path)
		}
		return errNothingRemoved
	}()
	if errors.Is(err, errNothingRemoved) {
		return false
	}
	if err != nil {
		guiLog(w, "⚠", cliui.Yellow, fmt.Sprintf("Could not remove %s: %s", path, pyrepr.ExceptionMessage(err)))
		return false
	}
	return true
}

var errNothingRemoved = errors.New("nothing removed")

// Removes the desktop GUI artifacts, app copies and optionally its data, leaving the agent and user data
func uninstallGUI(w io.Writer, h string, removeUserdata bool) []string {
	var removed []string
	removeExisting := func(paths []string) bool {
		found := false
		for _, p := range paths {
			if !guiExists(p) {
				continue
			}
			found = true
			if guiRemovePath(w, p) {
				guiLog(w, "✓", cliui.Green, "Removed "+p)
				removed = append(removed, p)
			}
		}
		return found
	}
	guiLog(w, "→", cliui.Cyan, "Removing the installed desktop app and its shortcuts...")
	removeExisting(append(installedGUIArtifacts(h), desktopInstallRecordPath()))
	guiLog(w, "→", cliui.Cyan, "Removing installed desktop app...")
	if !removeExisting(packagedGUIAppPaths()) {
		guiLog(w, "→", cliui.Cyan, "No packaged desktop app found in standard locations")
	}
	if userdata := desktopUserdataDir(); removeUserdata && guiExists(userdata) {
		guiLog(w, "→", cliui.Cyan, "Removing desktop app data...")
		removeExisting([]string{userdata})
	}
	if len(removed) == 0 {
		guiLog(w, "→", cliui.Cyan, "No desktop GUI artifacts found to remove")
	}
	if strings.HasPrefix(guiSysPlatform(), "linux") {
		entry := guiDesktopEntryPath()
		for _, p := range removed {
			if p == entry {
				for _, tool := range refreshDesktopDatabases(filepath.Dir(entry)) {
					guiLog(w, "✓", cliui.Green, "Refreshed the application menu cache ("+tool+")")
				}
				break
			}
		}
	}
	return removed
}

// Three line framed heading of the uninstall screens
func guiPrintBox(w io.Writer, middle, col string) {
	fmt.Fprintln(w, cliui.Color("┌─────────────────────────────────────────────────────────┐", col, cliui.Bold))
	fmt.Fprintln(w, cliui.Color(middle, col, cliui.Bold))
	fmt.Fprintln(w, cliui.Color("└─────────────────────────────────────────────────────────┘", col, cliui.Bold))
}

// Asks the user to type yes, printing the cancel line otherwise unless the prompt was aborted
func guiConfirmYes(w io.Writer, text string) bool {
	con := console()
	prompter := &cliui.Console{In: con.In, Out: w, Getenv: con.Getenv, StdinTTY: con.StdinIsTTY, StdoutTTY: con.StdoutIsTTY}
	answer, err := prompter.Input("Type '" + cliui.Color("yes", cliui.Yellow) + "' " + text + ": ")
	if err != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Cancelled.")
		return false
	}
	confirm := pystr.Lower(pystr.Strip(answer))
	if confirm != "yes" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Uninstall cancelled.")
	}
	return confirm == "yes"
}

// Removes the Chat GUI only, leaving the agent and data, after a confirmation unless yes or dry run
func RunGUIUninstall(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	if refuseIfStewardOwned(w) {
		return 1, nil
	}
	h := home.Dir(ctx)
	summary := guiInstallSummary(h)
	fmt.Fprintln(w)
	guiPrintBox(w, "│         ☤ Icebath Chat GUI Uninstaller                 │", cliui.Magenta)
	fmt.Fprintln(w)
	if installed, _ := summary.GetOr("gui_installed", false).(bool); !installed {
		fmt.Fprintln(w, "No Icebath Chat GUI installation was found.")
		fmt.Fprintf(w, "  Checked: %s, and the standard app locations for this OS.\n", h)
		return 0, nil
	}
	fmt.Fprintln(w, cliui.Color("This removes the Chat GUI only. The Icebath agent stays installed.", cliui.Cyan))
	fmt.Fprintln(w)
	fmt.Fprintln(w, cliui.Color("Will remove:", cliui.Yellow, cliui.Bold))
	for _, key := range []string{"source_built_artifacts", "packaged_app_paths"} {
		list, _ := summary.GetOr(key, nil).([]any)
		for _, p := range list {
			fmt.Fprintf(w, "  • %s\n", pyrepr.Str(p))
		}
	}
	if record := desktopInstallRecordPath(); guiExists(record) {
		fmt.Fprintf(w, "  • %s  (desktop install record)\n", record)
	}
	if exists, _ := summary.GetOr("userdata_exists", false).(bool); exists {
		fmt.Fprintf(w, "  • %s  (desktop app data)\n", pyrepr.Str(summary.GetOr("userdata_dir", nil)))
	}
	fmt.Fprintln(w)
	if agentIsInstalled(h) {
		fmt.Fprintln(w, cliui.Color("Kept intact:", cliui.Green, cliui.Bold))
		fmt.Fprintf(w, "  • The Icebath agent at %s\n", filepath.Join(h, "icebath-agent"))
		fmt.Fprintf(w, "  • Your config, sessions, and secrets under %s\n", h)
		if shared := filepath.Join(h, "icebath-agent", "node_modules"); guiExists(shared) {
			fmt.Fprintf(w, "  • Shared workspace dependencies at %s\n", shared)
		}
		fmt.Fprintln(w)
	}
	if nsTruthy(ns, "dry_run") {
		fmt.Fprintln(w, "Dry run: no files or processes changed.")
		return 0, nil
	}
	if !nsTruthy(ns, "yes") && !guiConfirmYes(w, "to remove the Chat GUI") {
		return 0, nil
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, cliui.Color("Uninstalling Chat GUI...", cliui.Cyan, cliui.Bold))
	fmt.Fprintln(w)
	uninstallGUI(w, h, true)
	fmt.Fprintln(w)
	guiPrintBox(w, "│            ✓ Chat GUI Uninstalled!                      │", cliui.Green)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The Icebath agent is still installed. Run 'icebath' to use the CLI,")
	fmt.Fprintln(w, "or 'icebath uninstall' to remove the agent too.")
	fmt.Fprintln(w)
	return 0, nil
}

// Reindexes the menu caches with each tool that exists and names the ones that ran
func refreshDesktopDatabases(applicationsDir string) []string {
	run := func(argv ...string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		path, err := exec.LookPath(argv[0])
		return err == nil && exec.CommandContext(ctx, path, argv[1:]...).Run() == nil
	}
	var ran []string
	if run("update-desktop-database", applicationsDir) {
		ran = append(ran, "update-desktop-database")
	}
	for _, tool := range []string{"kbuildsycoca6", "kbuildsycoca5"} {
		if _, err := exec.LookPath(tool); err == nil {
			if run(tool, "--noincremental") {
				ran = append(ran, tool)
			}
			break
		}
	}
	return ran
}
