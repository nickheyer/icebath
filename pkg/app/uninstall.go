package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyplist"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

// Hidden entry the desktop app runs for a non interactive uninstall outside the tree it deletes
const UninstallModuleEntry = "__icebath_uninstall__"

// Platform remover of the gateway service and the warning label shown when it fails
type gatewayServiceRemover struct {
	remove func(ctx context.Context, w io.Writer) (bool, error)
	label  string
}

// Uninstall steps and probes the uninstall cases replace the way the Python cases monkeypatch them
var (
	uninstallNegativeNumber       = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)
	uninstallProjectRoot          = config.GetProjectRoot
	uninstallIsWindows            = func() bool { return runtime.GOOS == "windows" }
	uninstallIsDefaultHome        = isDefaultIcebathHome
	uninstallDiscoverProfiles     = discoverNamedProfiles
	uninstallPerform              = performUninstall
	uninstallGatewayServiceStep   = uninstallGatewayService
	uninstallShellConfigsStep     = removePathFromShellConfigs
	uninstallWrapperStep          = removeWrapperScript
	uninstallNodeSymlinksStep     = removeNodeSymlinks
	uninstallLegacyRuntimeStep    = removeLegacyRuntimeTrees
	uninstallDashboardJobsStep    = removeDashboardLaunchdJobs
	uninstallMacosCacheDirs       = macosCacheLeftoverDirs
	uninstallRmtreeIfExists       = rmtreeIfExists
	uninstallRmtreeStep           = rmtreeStep
	uninstallGUIStep              = uninstallGUI
	uninstallRmtree               = pyshutil.Rmtree
	uninstallNodeSymlinkDirs      = nodeSymlinkCandidateDirs
	uninstallFindGatewayPids      = func(ctx context.Context) ([]int, error) { return gateway.FindGatewayPIDsFor(ctx, nil, false) }
	uninstallKillGatewayProcesses = func(ctx context.Context, w io.Writer) (int, error) {
		return gateway.KillGatewayProcesses(ctx, w, false, nil, false)
	}
	uninstallRunCommand            = runUninstallCommand
	uninstallLaunchdPlistDirs      = gateway.LaunchdPlistDirs
	uninstallLaunchdGatewayPlist   = gateway.GetLaunchdPlistPath
	uninstallRunDataMode           = RunDataUninstall
	uninstallRunGUIMode            = RunGUIUninstall
	uninstallRunAgentMode          = RunUninstall
	uninstallGatewayServiceRemover = map[string]gatewayServiceRemover{
		"Linux":   {removeSystemdGateway, "Could not check systemd gateway services"},
		"Darwin":  {removeLaunchdGateway, "Could not remove launchd gateway service"},
		"Windows": {removeWindowsGateway, "Could not check Windows gateway service"},
	}
)

// Both gateway LaunchAgent naming schemes, the current label and the one older builds installed
var launchdGatewayPlistPatterns = []string{"ai.icebath.gateway*.plist", "io.nousresearch.icebath-agent.gateway*.plist"}

func uninstallInfo(w io.Writer, msg string)    { guiLog(w, "→", cliui.Cyan, msg) }
func uninstallSuccess(w io.Writer, msg string) { guiLog(w, "✓", cliui.Green, msg) }
func uninstallWarn(w io.Writer, msg string)    { guiLog(w, "⚠", cliui.Yellow, msg) }

// str of an exception an uninstall step raised, OSError text for filesystem failures
func uninstallErrText(err error) string {
	if pypath.IsOSError(err) {
		return pypath.OSErrorText(err)
	}
	return pyrepr.ExceptionMessage(err)
}

// Stripped lowercased answer to a prompt, false after printing Cancelled on an interrupt or EOF
func uninstallPrompt(w io.Writer, text string) (string, bool) {
	con := console()
	prompter := &cliui.Console{In: con.In, Out: w, Getenv: con.Getenv, StdinTTY: con.StdinIsTTY, StdoutTTY: con.StdoutIsTTY}
	answer, err := prompter.Input(text)
	if err != nil {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Cancelled.")
		return "", false
	}
	return pystr.Lower(pystr.Strip(answer)), true
}

// Prints the uninstall cancelled line after a blank line
func uninstallCancelled(w io.Writer) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Uninstall cancelled.")
}

// Runs remove per candidate collecting the ones it reports removed, failures becoming one warning each
func removeEach(w io.Writer, candidates []string, remove func(string) (bool, error)) []string {
	var removed []string
	for _, p := range candidates {
		ok, err := remove(p)
		if err != nil {
			uninstallWarn(w, fmt.Sprintf("Could not remove %s: %s", p, uninstallErrText(err)))
			continue
		}
		if ok {
			removed = append(removed, p)
		}
	}
	return removed
}

// Steward refusal text when this tree's code is not ours to remove, false for a git checkout
func codeRemovalRefusal() (string, bool) {
	steward := selfupdate.SealedSteward(uninstallProjectRoot())
	if steward == nil {
		return "", false
	}
	return selfupdate.StewardUninstallMessage(*steward, nil), true
}

// Prints the steward refusal and reports true when code removal is forbidden
func refuseIfStewardOwned(w io.Writer) bool {
	refusal, refused := codeRemovalRefusal()
	if !refused {
		return false
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, refusal)
	fmt.Fprintln(w)
	return true
}

// Shell rc files under the user home that might carry PATH entries
func findShellConfigs() []string {
	h := guiUserHome()
	var out []string
	for _, name := range []string{".bashrc", ".bash_profile", ".profile", ".zshrc", ".zprofile"} {
		if p := filepath.Join(h, name); guiExists(p) {
			out = append(out, p)
		}
	}
	return out
}

// Shell rc text with the icebath block comment, its PATH line and every icebath PATH line dropped
func stripShellConfigPath(content string) string {
	var lines []string
	skipNext := false
	for _, line := range strings.Split(content, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(line, "# Icebath Agent") || strings.Contains(line, "# icebath-agent") {
			skipNext = true
			continue
		}
		if skipNext && strings.Contains(lower, "icebath") && strings.Contains(line, "PATH") {
			skipNext = false
			continue
		}
		skipNext = false
		if strings.Contains(lower, "icebath") && (strings.Contains(line, "PATH=") || strings.Contains(lower, "path=")) {
			continue
		}
		lines = append(lines, line)
	}
	out := strings.Join(lines, "\n")
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return out
}

// Strips icebath PATH entries from each shell rc atomically, keeping its mode, owner and symlink
func removePathFromShellConfigs(ctx context.Context, w io.Writer) []string {
	var removedFrom []string
	for _, cfg := range findShellConfigs() {
		err := func() error {
			raw, err := os.ReadFile(cfg)
			if err != nil {
				return err
			}
			decoded, err := pystr.DecodeUTF8Sig(raw)
			if err != nil {
				return err
			}
			original := pystr.TranslateNewlines(decoded)
			updated := stripShellConfigPath(original)
			if updated == original {
				return nil
			}
			if err := pyfs.AtomicWriteTextWith(ctx, cfg, updated, pyfs.TextWrite{PreserveMode: true}); err != nil {
				return err
			}
			removedFrom = append(removedFrom, cfg)
			return nil
		}()
		if err != nil {
			uninstallWarn(w, fmt.Sprintf("Could not update %s: %s", cfg, uninstallErrText(err)))
		}
	}
	return removedFrom
}

// Removes each icebath wrapper script on PATH whose text proves it is ours
func removeWrapperScript(w io.Writer) []string {
	h := guiUserHome()
	local := filepath.Join(h, ".local", "bin")
	var removed []string
	for _, wrapper := range []string{filepath.Join(local, "icebath"), filepath.Join(local, "icebath-acp"), filepath.Join(local, "icebath-agent"),
		"/usr/local/bin/icebath", "/usr/local/bin/icebath-acp", "/usr/local/bin/icebath-agent"} {
		if !guiExists(wrapper) {
			continue
		}
		err := func() error {
			raw, err := os.ReadFile(wrapper)
			if err != nil {
				return err
			}
			content, err := pystr.DecodeUTF8Sig(raw)
			if err != nil {
				return err
			}
			if strings.Contains(content, "icebath_cli") || strings.Contains(content, "icebath-agent") {
				if err := pypath.Unlink(wrapper); err != nil {
					return err
				}
				removed = append(removed, wrapper)
			}
			return nil
		}()
		if err != nil {
			uninstallWarn(w, fmt.Sprintf("Could not remove %s: %s", wrapper, uninstallErrText(err)))
		}
	}
	return removed
}

// Directories the installer may have placed node, npm and npx symlinks in
func nodeSymlinkCandidateDirs() []string {
	dirs := []string{filepath.Join(guiUserHome(), ".local", "bin")}
	if guiSysPlatform() == "linux" {
		dirs = append(dirs, "/usr/local/bin")
	}
	return dirs
}

// Removes the node, npm and npx symlinks that still resolve into this home's node dir
func removeNodeSymlinks(w io.Writer, h string) []string {
	nodeDir, nodeErr := pypath.Resolve(filepath.Join(h, "node"))
	var candidates []string
	for _, name := range []string{"node", "npm", "npx"} {
		for _, dir := range uninstallNodeSymlinkDirs() {
			candidates = append(candidates, filepath.Join(dir, name))
		}
	}
	return removeEach(w, candidates, func(link string) (bool, error) {
		st, err := os.Lstat(link)
		if err != nil || st.Mode()&fs.ModeSymlink == 0 {
			return false, nil
		}
		if nodeErr != nil {
			return false, nodeErr
		}
		target, err := pypath.Resolve(link)
		if err != nil {
			return false, err
		}
		if !pathEq(target, nodeDir) && !pathUnder(target, nodeDir) {
			return false, nil
		}
		if err := pypath.Unlink(link); err != nil {
			return false, err
		}
		return true, nil
	})
}

// Stops and removes the gateway service of this platform and kills standalone gateway processes
func uninstallGatewayService(ctx context.Context, w io.Writer) bool {
	stopped := false
	func() {
		pids, err := uninstallFindGatewayPids(ctx)
		if err != nil {
			uninstallWarn(w, "Could not check for gateway processes: "+uninstallErrText(err))
			return
		}
		killed := 0
		if len(pids) > 0 {
			if killed, err = uninstallKillGatewayProcesses(ctx, w); err != nil {
				uninstallWarn(w, "Could not check for gateway processes: "+uninstallErrText(err))
				return
			}
		}
		if killed > 0 {
			uninstallSuccess(w, fmt.Sprintf("Killed %d running gateway process(es)", killed))
			stopped = true
		}
	}()
	if os.Getenv("TERMUX_VERSION") != "" || strings.Contains(os.Getenv("PREFIX"), "com.termux/files/usr") {
		return stopped
	}
	remover, ok := uninstallGatewayServiceRemover[pyplatform.System()]
	if ok && remover.remove != nil {
		removed, err := remover.remove(ctx, w)
		if err != nil {
			uninstallWarn(w, remover.label+": "+uninstallErrText(err))
		} else {
			stopped = removed || stopped
		}
	}
	return stopped
}

// subprocess.run with captured output and no check, the OSError or TimeoutExpired it raises returned
func runUninstallCommand(argv []string, timeout time.Duration) error {
	_, err := pysubprocess.RunWith(context.Background(), argv, pysubprocess.Options{Timeout: timeout})
	return err
}

// Removes the user and system systemd gateway units this home owns
func removeSystemdGateway(ctx context.Context, w io.Writer) (bool, error) {
	svcName, err := gateway.GetServiceName(ctx)
	if err != nil {
		return false, err
	}
	removedAny := false
	for _, scope := range []struct {
		system bool
		name   string
	}{{false, "user"}, {true, "system"}} {
		unitPath, err := gateway.GetSystemdUnitPath(ctx, scope.system)
		if err != nil {
			return removedAny, err
		}
		if !guiExists(unitPath) {
			continue
		}
		belongs, err := gateway.SystemdUnitBelongsToCurrentHome(ctx, scope.system)
		if err != nil {
			return removedAny, err
		}
		if !belongs {
			continue
		}
		if scope.system && os.Geteuid() != 0 {
			uninstallWarn(w, fmt.Sprintf("System gateway service exists at %s but needs sudo to remove", unitPath))
			continue
		}
		err = func() error {
			cmd := gateway.SystemctlCmd(scope.system)
			for _, verb := range []string{"stop", "disable"} {
				if err := uninstallRunCommand(append(slices.Clone(cmd), verb, svcName), 0); err != nil {
					return err
				}
			}
			if err := pypath.Unlink(unitPath); err != nil {
				return err
			}
			if err := uninstallRunCommand(append(slices.Clone(cmd), "daemon-reload"), 0); err != nil {
				return err
			}
			uninstallSuccess(w, fmt.Sprintf("Removed %s gateway service (%s)", scope.name, unitPath))
			removedAny = true
			return nil
		}()
		if err != nil {
			uninstallWarn(w, fmt.Sprintf("Could not remove %s gateway service: %s", scope.name, uninstallErrText(err)))
		}
	}
	return removedAny, nil
}

// Every gateway LaunchAgent plist of both label schemes under the real account home, sorted
func launchdGatewayPlists(ctx context.Context) ([]string, error) {
	plist, err := uninstallLaunchdGatewayPlist(ctx)
	if err != nil {
		return nil, err
	}
	agents := pypath.PathParent(plist)
	seen := map[string]bool{}
	var out []string
	for _, pattern := range launchdGatewayPlistPatterns {
		matches, err := pypath.Glob(agents, pattern)
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	slices.SortFunc(out, pathCompare)
	return out, nil
}

// Boots out and deletes every gateway LaunchAgent so launchd never respawns a removed gateway
func removeLaunchdGateway(ctx context.Context, w io.Writer) (bool, error) {
	plists, err := launchdGatewayPlists(ctx)
	if err != nil {
		return false, err
	}
	uid := os.Getuid()
	for _, plist := range plists {
		label := pypath.PathStem(plist)
		for _, domain := range []string{fmt.Sprintf("gui/%d", uid), fmt.Sprintf("user/%d", uid)} {
			if err := uninstallRunCommand([]string{"launchctl", "bootout", domain + "/" + label}, 0); err != nil {
				return false, err
			}
		}
		if err := uninstallRunCommand([]string{"launchctl", "unload", plist}, 0); err != nil {
			return false, err
		}
		if err := pypath.Unlink(plist); err != nil {
			return false, err
		}
		uninstallSuccess(w, fmt.Sprintf("Removed macOS gateway service (%s)", plist))
	}
	return len(plists) > 0, nil
}

// Removes the Windows Scheduled Task and Startup entry of the gateway when any is present
func removeWindowsGateway(ctx context.Context, w io.Writer) (bool, error) {
	present := false
	for _, probe := range []func(context.Context) (bool, error){gateway.WindowsIsInstalled, gateway.WindowsIsTaskRegistered, gateway.WindowsStartupEntryInstalled} {
		found, err := probe(ctx)
		if err != nil {
			return false, err
		}
		if found {
			present = true
			break
		}
	}
	if !present {
		return false, nil
	}
	if err := gateway.WindowsStop(ctx); err != nil {
		uninstallWarn(w, "Could not stop Windows gateway cleanly: "+uninstallErrText(err))
	}
	if err := gateway.WindowsUninstall(ctx); err != nil {
		uninstallWarn(w, "Could not fully uninstall Windows gateway: "+uninstallErrText(err))
		return false, nil
	}
	uninstallSuccess(w, "Removed Windows gateway (Scheduled Task + Startup entry)")
	return true, nil
}

// Prefixes of the icebath owned User PATH entries, the managed bin only when it is about to be deleted
func icebathPathMarkers(h string, includeManagedBin bool) []string {
	root := strings.TrimRight(h, `\/`)
	subs := []string{"icebath-agent", "git", "node", "venv"}
	if includeManagedBin {
		subs = append(subs, "bin")
	}
	out := make([]string, len(subs))
	for i, sub := range subs {
		out[i] = root + `\` + sub
	}
	return out
}

// Splits a User PATH value into the kept entries and the icebath owned ones by marker prefix
func splitUserPath(pathValue string, markers []string) (kept, removed []string) {
	lowered := make([]string, len(markers))
	for i, m := range markers {
		lowered[i] = pystr.Lower(m)
	}
	for _, entry := range strings.Split(pathValue, ";") {
		if entry == "" {
			continue
		}
		probe := pystr.Lower(strings.TrimRight(entry, `\/`))
		if slices.ContainsFunc(lowered, func(m string) bool { return strings.HasPrefix(probe, m) }) {
			removed = append(removed, entry)
		} else {
			kept = append(kept, entry)
		}
	}
	return kept, removed
}

// Deletes the PortableGit, Node and gateway service dirs the Windows installer created under the home
func removePortableToolingWindows(w io.Writer, h string) []string {
	var targets []string
	for _, sub := range []string{"git", "node", "gateway-service"} {
		if t := filepath.Join(h, sub); guiExists(t) {
			targets = append(targets, t)
		}
	}
	return removeEach(w, targets, func(t string) (bool, error) {
		if err := uninstallRmtree(t); err != nil {
			return false, err
		}
		return true, nil
	})
}

// XDG cache dir always removable and XDG data dir removable only on a full uninstall, else preserved
func xdgLeftoverPaths(fullUninstall bool) (removable, preserved []string) {
	h := guiUserHome()
	cache := filepath.Join(h, ".cache", "icebath")
	data := filepath.Join(h, ".local", "share", "icebath")
	if guiExists(cache) {
		removable = append(removable, cache)
	}
	if guiExists(data) && fullUninstall {
		removable = append(removable, data)
	}
	if guiExists(data) && !fullUninstall {
		preserved = append(preserved, data)
	}
	return removable, preserved
}

// Removes icebath named macOS Library entries and the XDG leftovers outside the home and checkout
func removeDesktopAppLeftovers(w io.Writer, fullUninstall bool) []string {
	var targets []string
	if guiSysPlatform() == "darwin" {
		library := filepath.Join(guiUserHome(), "Library")
		for _, parent := range []string{"Caches", "Logs", "WebKit", "HTTPStorages", "Saved Application State"} {
			parentDir := filepath.Join(library, parent)
			if st, err := os.Stat(parentDir); err != nil || !st.IsDir() {
				continue
			}
			entries, err := os.ReadDir(parentDir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if strings.Contains(strings.ToLower(e.Name()), "icebath") {
					targets = append(targets, filepath.Join(parentDir, e.Name()))
				}
			}
		}
	}
	removable, _ := xdgLeftoverPaths(fullUninstall)
	targets = append(targets, removable...)
	var existing []string
	for _, t := range targets {
		if guiExists(t) {
			existing = append(existing, t)
		}
	}
	return removeEach(w, existing, removePathOrTree)
}

// Unlinks a file or symlink or removes a tree, false on any OSError
func removePathOrTree(p string) (bool, error) {
	if st, err := os.Lstat(p); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		return pypath.Unlink(p) == nil, nil
	}
	if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
		return pypath.Unlink(p) == nil, nil
	}
	return uninstallRmtree(p) == nil, nil
}

// Deletes the node tree and uv binaries a pre split install left in the home, never other bin files
func removeLegacyRuntimeTrees(w io.Writer, h string) []string {
	var removed []string
	nodeTree := filepath.Join(h, "node")
	if st, err := os.Stat(nodeTree); err == nil && st.IsDir() {
		if err := uninstallRmtree(nodeTree); err != nil {
			uninstallWarn(w, fmt.Sprintf("Could not remove %s: %s", nodeTree, uninstallErrText(err)))
		} else {
			removed = append(removed, nodeTree)
		}
	}
	for _, uvName := range []string{"uv", "uv.exe"} {
		uvBinary := filepath.Join(h, "bin", uvName)
		if st, err := os.Stat(uvBinary); err == nil && st.Mode().IsRegular() {
			if err := pypath.Unlink(uvBinary); err != nil {
				uninstallWarn(w, fmt.Sprintf("Could not remove %s: %s", uvBinary, uninstallErrText(err)))
			} else {
				removed = append(removed, uvBinary)
			}
		}
	}
	return removed
}

// Default root of the process home, the error Python raises when it cannot be located
func defaultIcebathRoot() (root string, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, isErr := r.(error); isErr {
				err = e
				return
			}
			err = fmt.Errorf("%v", r)
		}
	}()
	return home.Root(), nil
}

// Deletes every launcher in the managed bin dir on Windows, renaming aside one that is locked
func removeWindowsBinLaunchers(w io.Writer, windows bool) []string {
	if !windows {
		return nil
	}
	root, err := defaultIcebathRoot()
	if err != nil {
		uninstallWarn(w, "Could not locate the managed binary dir: "+uninstallErrText(err))
		return nil
	}
	binDir := filepath.Join(root, "bin")
	if st, err := os.Stat(binDir); err != nil || !st.IsDir() {
		return nil
	}
	launchers, err := sortedChildren(binDir)
	if err != nil {
		return nil
	}
	var removed []string
	for _, launcher := range launchers {
		if st, err := os.Stat(launcher); err != nil || !st.Mode().IsRegular() {
			continue
		}
		if err := pypath.Unlink(launcher); err == nil {
			removed = append(removed, launcher)
			continue
		}
		aside := filepath.Join(binDir, fmt.Sprintf("%s.uninstalled.%d", filepath.Base(launcher), os.Getpid()))
		if err := os.Rename(launcher, aside); err != nil {
			uninstallWarn(w, fmt.Sprintf("Could not remove %s: %s", launcher, uninstallErrText(err)))
			continue
		}
		removed = append(removed, launcher)
	}
	_ = os.Remove(binDir)
	return removed
}

// Whether the home is the default non profile root, false when either cannot be resolved
func isDefaultIcebathHome(h string) bool {
	resolved, err := pypath.Resolve(h)
	if err != nil {
		return false
	}
	root, err := defaultIcebathRoot()
	if err != nil {
		return false
	}
	resolvedRoot, err := pypath.Resolve(root)
	return err == nil && pathEq(resolved, resolvedRoot)
}

// Every non default profile, none with a warning when the profiles cannot be listed
func discoverNamedProfiles(ctx context.Context, w io.Writer) []manage.ProfileInfo {
	all, err := manage.ListProfiles(ctx, false)
	if err != nil {
		uninstallWarn(w, "Could not enumerate profiles: "+uninstallErrText(err))
		return nil
	}
	var named []manage.ProfileInfo
	for _, p := range all {
		if !p.IsDefault {
			named = append(named, p)
		}
	}
	return named
}

// Stops a named profile's gateway, removes its alias wrapper and wipes its home
func uninstallProfile(ctx context.Context, w io.Writer, profile manage.ProfileInfo) {
	name := profile.Name
	uninstallInfo(w, fmt.Sprintf("Uninstalling profile '%s'...", name))
	exe, exeErr := os.Executable()
	for _, subcmd := range []string{"stop", "uninstall"} {
		err := exeErr
		if err == nil {
			_, err = pysubprocess.RunWith(ctx, []string{exe, "--profile", name, "gateway", subcmd}, pysubprocess.Options{Timeout: 60 * time.Second})
		}
		var timeout *pysubprocess.TimeoutExpired
		switch {
		case errors.As(err, &timeout):
			uninstallWarn(w, fmt.Sprintf("  Gateway %s timed out for '%s'", subcmd, name))
		case err != nil:
			uninstallWarn(w, fmt.Sprintf("  Could not run gateway %s for '%s': %s", subcmd, name, uninstallErrText(err)))
		}
	}
	if alias := profile.AliasPath; alias != "" && guiExists(alias) {
		if err := pypath.Unlink(alias); err != nil {
			uninstallWarn(w, fmt.Sprintf("  Could not remove alias %s: %s", alias, uninstallErrText(err)))
		} else {
			uninstallSuccess(w, "  Removed alias "+alias)
		}
	}
	uninstallRmtreeStep(w, profile.Path, "  ", false)
}

// Erases this home's data while the installed runtimes and sibling profiles stay, exit 1 when it cannot
func RunDataUninstall(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	h := home.Dir(ctx)
	userdata := desktopUserdataDir()
	if raw := ns["desktop_userdata"]; pyrepr.Truthy(raw) {
		userdata = pypath.PathStr(pyrepr.Str(raw))
	}
	plan, err := planDataRemoval(ctx, h, uninstallProjectRoot(), &userdata)
	if err != nil {
		uninstallWarn(w, dataErrText(err))
		return 1, nil
	}
	if len(plan.Remove) == 0 {
		fmt.Fprintf(w, "No Icebath user data found in %s.\n", h)
		return 0, nil
	}
	fmt.Fprintln(w, "Data-only removal: installed code and other profiles stay intact.")
	fmt.Fprintln(w, "Will remove:")
	for _, p := range plan.Remove {
		fmt.Fprintf(w, "  %s\n", p)
	}
	if len(plan.Keep) > 0 {
		fmt.Fprintln(w, "Kept intact:")
		for _, p := range plan.Keep {
			fmt.Fprintf(w, "  %s\n", p)
		}
	}
	if nsTruthy(ns, "dry_run") {
		fmt.Fprintln(w, "Dry run: no data or processes changed.")
		return 0, nil
	}
	if !nsTruthy(ns, "yes") && !guiConfirmYes(w, "to remove this home's data") {
		return 0, nil
	}
	var removed []string
	var failed []dataRemovalFailure
	if err := quiescentHome(plan.Home, func() error {
		removed, failed = removeData(plan)
		return nil
	}); err != nil {
		uninstallWarn(w, "No data removed: "+dataErrText(err))
		return 1, nil
	}
	for _, p := range removed {
		uninstallSuccess(w, "Removed "+p)
	}
	if len(failed) > 0 {
		fmt.Fprintln(w, "Icebath data was only partially removed. Surviving targets:")
		for _, f := range failed {
			uninstallWarn(w, f.Path+": "+f.Reason)
		}
		return 1, nil
	}
	uninstallSuccess(w, "Icebath data removed.")
	return 0, nil
}

// Interactive or yes mode removal of the agent code, keeping or wiping the data, offering named profiles too
func RunUninstall(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	if refuseIfStewardOwned(w) {
		return 1, nil
	}
	projectRoot := uninstallProjectRoot()
	h := home.Dir(ctx)
	fullFlag := nsTruthy(ns, "full")
	if nsTruthy(ns, "dry_run") {
		printUninstallDryRun(ctx, w, projectRoot, h, fullFlag)
		return 0, nil
	}
	var named []manage.ProfileInfo
	if uninstallIsDefaultHome(h) {
		named = uninstallDiscoverProfiles(ctx, w)
	}
	if nsTruthy(ns, "yes") {
		uninstallPerform(ctx, w, projectRoot, h, fullFlag, false, named)
		return 0, nil
	}
	fmt.Fprintln(w)
	guiPrintBox(w, "│            ☤ Icebath Agent Uninstaller                 │", cliui.Magenta)
	fmt.Fprintln(w)
	fmt.Fprintln(w, cliui.Color("Current Installation:", cliui.Cyan, cliui.Bold))
	fmt.Fprintf(w, "  Code:    %s\n", projectRoot)
	fmt.Fprintf(w, "  Config:  %s\n", filepath.Join(h, "config.yaml"))
	fmt.Fprintf(w, "  Secrets: %s\n", filepath.Join(h, ".env"))
	fmt.Fprintf(w, "  Data:    %s, %s, %s\n", filepath.Join(h, "cron"), filepath.Join(h, "sessions"), filepath.Join(h, "logs"))
	if userdata := desktopUserdataDir(); guiExists(userdata) {
		fmt.Fprintf(w, "  Desktop: %s  (app data - kept by 'Keep data', removed by 'Full uninstall')\n", userdata)
	}
	fmt.Fprintln(w)
	if len(named) > 0 {
		fmt.Fprintln(w, cliui.Color("Other profiles detected:", cliui.Cyan, cliui.Bold))
		for _, p := range named {
			running := ""
			if p.GatewayRunning {
				running = " (gateway running)"
			}
			fmt.Fprintf(w, "  • %s%s: %s\n", p.Name, running, p.Path)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, cliui.Color("Uninstall Options:", cliui.Yellow, cliui.Bold))
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  1) "+cliui.Color("Keep data", cliui.Green)+" - Remove code only, keep configs/sessions/logs")
	fmt.Fprintln(w, "     (Recommended - you can reinstall later with your settings intact)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  2) "+cliui.Color("Full uninstall", cliui.Red)+" - Remove everything including all data")
	fmt.Fprintln(w, "     (Warning: This deletes all configs, sessions, and logs permanently)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  3) "+cliui.Color("Cancel", cliui.Cyan)+" - Don't uninstall")
	fmt.Fprintln(w)
	choice, answered := uninstallPrompt(w, cliui.Color("Select option [1/2/3]: ", cliui.Bold))
	if !answered {
		return 0, nil
	}
	switch choice {
	case "3", "c", "cancel", "q", "quit", "n", "no":
		uninstallCancelled(w)
		return 0, nil
	}
	fullUninstall := choice == "2"
	removeProfiles := false
	nProfiles := len(named)
	names := make([]string, nProfiles)
	for i, p := range named {
		names[i] = p.Name
	}
	profileNames := strings.Join(names, ", ")
	if fullUninstall && nProfiles > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, cliui.Color("Other profiles will NOT be removed by default.", cliui.Yellow))
		fmt.Fprintf(w, "Found %d named profile(s): %s\n", nProfiles, profileNames)
		fmt.Fprintln(w)
		resp, ok := uninstallPrompt(w, cliui.Color(fmt.Sprintf("Also stop and remove these %d profile(s)? [y/N]: ", nProfiles), cliui.Bold))
		if !ok {
			return 0, nil
		}
		removeProfiles = resp == "y" || resp == "yes"
	}
	fmt.Fprintln(w)
	if fullUninstall {
		fmt.Fprintln(w, cliui.Color("⚠️  WARNING: This will permanently delete ALL Icebath data!", cliui.Red, cliui.Bold))
		fmt.Fprintln(w, cliui.Color("   Including: configs, API keys, sessions, scheduled jobs, logs", cliui.Red))
		if removeProfiles {
			fmt.Fprintln(w, cliui.Color(fmt.Sprintf("   Plus %d profile(s): %s", nProfiles, profileNames), cliui.Red))
		}
	} else {
		fmt.Fprintln(w, "This will remove the Icebath code but keep your configuration and data.")
	}
	fmt.Fprintln(w)
	if !guiConfirmYes(w, "to confirm") {
		return 0, nil
	}
	uninstallPerform(ctx, w, projectRoot, h, fullUninstall, removeProfiles, named)
	return 0, nil
}

// Prints the uninstall plan without stopping services or deleting files
func printUninstallDryRun(ctx context.Context, w io.Writer, projectRoot, h string, fullUninstall bool) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, cliui.Color("Dry run: no files, services, or environment entries will be changed.", cliui.Cyan, cliui.Bold))
	fmt.Fprintln(w)
	fmt.Fprintln(w, cliui.Color("Would inspect/remove:", cliui.Yellow, cliui.Bold))
	fmt.Fprintln(w, "  • Gateway services and standalone gateway processes")
	fmt.Fprintln(w, "  • Icebath PATH entries from shell configs / Windows User PATH")
	fmt.Fprintln(w, "  • Icebath wrapper scripts and Icebath-managed node/npm/npx symlinks")
	fmt.Fprintln(w, "  • Desktop Chat GUI artifacts")
	fmt.Fprintf(w, "  • Code checkout: %s\n", projectRoot)
	userdata := desktopUserdataDir()
	if !fullUninstall {
		fmt.Fprintf(w, "  • Keep Icebath config/data: %s\n", h)
		if guiExists(userdata) {
			fmt.Fprintf(w, "  • Keep desktop app data: %s\n", userdata)
		}
	} else {
		fmt.Fprintf(w, "  • Icebath config/data: %s\n", h)
		if guiSysPlatform() == "darwin" {
			fmt.Fprintln(w, "  • macOS: dashboard/serve launchd jobs, Electron + setup caches")
		}
		if guiExists(userdata) {
			fmt.Fprintf(w, "  • Desktop app data: %s\n", userdata)
		}
		var profiles []manage.ProfileInfo
		if uninstallIsDefaultHome(h) {
			profiles = uninstallDiscoverProfiles(ctx, w)
		}
		if len(profiles) > 0 {
			fmt.Fprintln(w, "  • Named profiles (interactive uninstall asks before removing):")
			for _, p := range profiles {
				fmt.Fprintf(w, "    - %s: %s\n", p.Name, p.Path)
			}
		}
	}
	fmt.Fprintln(w)
}

// Announces label, runs remove and logs one success line per removed item or the none message
func removeStep(w io.Writer, label string, remove func() []string, successFmt, noneMsg string) {
	uninstallInfo(w, label)
	removed := remove()
	for _, item := range removed {
		uninstallSuccess(w, strings.Replace(successFmt, "{}", item, 1))
	}
	if len(removed) == 0 {
		uninstallInfo(w, noneMsg)
	}
}

// Removes an existing tree with the shared success line, a warning when it cannot
func rmtreeStep(w io.Writer, p, indent string, fully bool) {
	if !guiExists(p) {
		return
	}
	if err := uninstallRmtree(p); err != nil {
		adverb := ""
		if fully {
			adverb = "fully "
		}
		uninstallWarn(w, fmt.Sprintf("%sCould not %sremove %s: %s", indent, adverb, p, uninstallErrText(err)))
		if fully {
			uninstallInfo(w, "You may need to manually remove it")
		}
		return
	}
	uninstallSuccess(w, indent+"Removed "+p)
}

// Electron and setup cache dirs macOS writes outside the home that the desktop data removal never reaches
func macosCacheLeftoverDirs() []string {
	caches := filepath.Join(guiUserHome(), "Library", "Caches")
	return []string{filepath.Join(caches, "Icebath"), filepath.Join(caches, "ai.icebath"),
		filepath.Join(caches, "icebath-setup"), filepath.Join(caches, "ai.icebath.setup")}
}

// Removes an existing tree reporting true, false when nothing is there
func rmtreeIfExists(p string) (bool, error) {
	if !guiExists(p) {
		return false, nil
	}
	if err := uninstallRmtree(p); err != nil {
		return false, err
	}
	return true, nil
}

// Boots out and deletes every macOS launchd job that runs a dashboard or serve backend
func removeDashboardLaunchdJobs(w io.Writer) []string {
	if guiSysPlatform() != "darwin" {
		return nil
	}
	uid := os.Getuid()
	var removed []string
	for _, entry := range uninstallLaunchdPlistDirs() {
		kind, dir := entry[0], entry[1]
		plists, err := pypath.Glob(dir, "*.plist")
		if err != nil {
			continue
		}
		slices.SortFunc(plists, pathCompare)
		for _, plistPath := range plists {
			raw, err := os.ReadFile(plistPath)
			if err != nil {
				continue
			}
			doc, err := pyplist.Load(raw)
			if err != nil {
				continue
			}
			data, isDict := doc.(*pyplist.Dict)
			if !isDict || data == nil {
				continue
			}
			label := ""
			if v, _ := data.Get("Label"); pyrepr.Truthy(v) {
				label = pystr.Strip(pyrepr.Str(v))
			}
			rawArgs, _ := data.Get("ProgramArguments")
			args, isList := rawArgs.([]any)
			if label == "" || !isList || len(args) == 0 {
				continue
			}
			argv := make([]string, len(args))
			for i, a := range args {
				argv[i] = pyrepr.Str(a)
			}
			if _, _, _, ok := gateway.ParseDashboardRuntime(pyshlex.Join(argv)); !ok {
				continue
			}
			domains := []string{fmt.Sprintf("gui/%d", uid), fmt.Sprintf("user/%d", uid)}
			if kind == "daemon" {
				domains = []string{"system"}
			}
			for _, domain := range domains {
				if err := uninstallRunCommand([]string{"launchctl", "bootout", domain + "/" + label}, 90*time.Second); err != nil {
					uninstallWarn(w, fmt.Sprintf("Could not boot out %s/%s: %s", domain, label, uninstallErrText(err)))
				}
			}
			if err := pypath.Unlink(plistPath); err != nil {
				uninstallWarn(w, fmt.Sprintf("Could not remove %s: %s", plistPath, uninstallErrText(err)))
				continue
			}
			removed = append(removed, plistPath)
		}
	}
	return removed
}

// Reinstall command per platform, keyed by whether this is Windows
var uninstallReinstallHint = map[bool]string{
	true:  "  iex (irm https://hermes-agent.nousresearch.com/install.ps1)",
	false: "  curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash",
}

// Shell reload advice lines and their colors per platform, keyed by whether this is Windows
var uninstallReloadHint = map[bool][][2]string{
	true: {{"Open a new terminal (PowerShell / Windows Terminal) to pick up", cliui.Yellow},
		{"the updated User PATH and environment variables.", cliui.Yellow}},
	false: {{"Reload your shell to complete the process:", cliui.Yellow}, {"  source ~/.bashrc  # or ~/.zshrc", ""}},
}

// Shared uninstall steps of the interactive and yes paths, from the gateway through the code to the data
func performUninstall(ctx context.Context, w io.Writer, projectRoot, h string, fullUninstall, removeProfiles bool, named []manage.ProfileInfo) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, cliui.Color("Uninstalling...", cliui.Cyan, cliui.Bold))
	fmt.Fprintln(w)
	uninstallInfo(w, "Checking for running gateway...")
	if !uninstallGatewayServiceStep(ctx, w) {
		uninstallInfo(w, "No gateway service or processes found")
	}
	windows := uninstallIsWindows()
	sweepManagedBin := windows && fullUninstall && uninstallIsDefaultHome(h)
	steps := []struct {
		onThisPlatform bool
		label          string
		remove         func() []string
		successFmt     string
		noneMsg        string
	}{
		{true, "Removing PATH entries from shell configs...", func() []string { return uninstallShellConfigsStep(ctx, w) },
			"Updated {}", "No PATH entries found to remove in shell rc files"},
		{windows, "Removing PATH entries from Windows User environment...",
			func() []string { return removePathFromWindowsRegistry(w, pypath.Expandvars(h), sweepManagedBin) },
			"Removed from User PATH: {}", "No Icebath-owned PATH entries in User environment"},
		{windows, "Removing ICEBATH_HOME / ICEBATH_GIT_BASH_PATH User env vars...", func() []string { return removeIcebathEnvVarsWindows(w) },
			"Removed User env var: {}", "No Icebath-set User env vars to remove"},
		{true, "Removing icebath command...", func() []string { return uninstallWrapperStep(w) }, "Removed {}", "No wrapper script found"},
		{windows, "Removing Windows icebath launchers...", func() []string { return removeWindowsBinLaunchers(w, uninstallIsWindows()) },
			"Removed {}", "No Windows icebath launchers found"},
		{true, "Removing Icebath-managed node/npm/npx symlinks...", func() []string { return uninstallNodeSymlinksStep(w, h) },
			"Removed {}", "No Icebath-managed node/npm/npx symlinks found"},
	}
	for _, step := range steps {
		if step.onThisPlatform {
			removeStep(w, step.label, step.remove, step.successFmt, step.noneMsg)
		}
	}
	uninstallInfo(w, "Removing desktop Chat GUI artifacts...")
	if len(uninstallGUIStep(w, h, fullUninstall)) == 0 {
		uninstallInfo(w, "No desktop GUI artifacts found")
	}
	uninstallInfo(w, "Removing app caches and leftovers outside the install...")
	if leftovers := removeDesktopAppLeftovers(w, fullUninstall); len(leftovers) > 0 {
		for _, p := range leftovers {
			uninstallSuccess(w, "Removed "+p)
		}
	} else {
		uninstallInfo(w, "No app caches or leftovers found")
	}
	uninstallInfo(w, "Removing installation directory...")
	uninstallRmtreeStep(w, projectRoot, "", true)
	if windows {
		removeStep(w, "Removing Windows installer artifacts (PortableGit, Node, gateway-service)...",
			func() []string { return removePortableToolingWindows(w, h) }, "Removed {}", "No Windows installer artifacts to remove")
	}
	uninstallInfo(w, "Removing managed runtime trees...")
	if runtimes := uninstallLegacyRuntimeStep(w, h); len(runtimes) > 0 {
		for _, p := range runtimes {
			uninstallSuccess(w, "Removed "+p)
		}
	} else {
		uninstallInfo(w, "No legacy runtime trees to remove")
	}
	if fullUninstall {
		if removeProfiles {
			for _, prof := range named {
				uninstallProfile(ctx, w, prof)
			}
		}
		if guiSysPlatform() == "darwin" {
			removeStep(w, "Removing dashboard/serve launchd jobs...", func() []string { return uninstallDashboardJobsStep(w) },
				"Removed {}", "No dashboard/serve launchd jobs found")
		}
		if guiSysPlatform() == "darwin" {
			removeStep(w, "Removing Electron and setup caches...",
				func() []string { return removeEach(w, uninstallMacosCacheDirs(), uninstallRmtreeIfExists) },
				"Removed {}", "No Electron or setup caches found")
		}
		uninstallInfo(w, "Removing configuration and data...")
		uninstallRmtreeStep(w, h, "", true)
	} else {
		uninstallInfo(w, "Keeping configuration and data in "+h)
	}
	fmt.Fprintln(w)
	guiPrintBox(w, "│              ✓ Uninstall Complete!                      │", cliui.Green)
	fmt.Fprintln(w)
	if !fullUninstall {
		fmt.Fprintln(w, cliui.Color("Your configuration and data have been preserved:", cliui.Cyan))
		fmt.Fprintf(w, "  %s/\n", h)
		if userdata := desktopUserdataDir(); guiExists(userdata) {
			fmt.Fprintf(w, "  %s  (desktop app data)\n", userdata)
		}
		_, preserved := xdgLeftoverPaths(false)
		for _, p := range preserved {
			fmt.Fprintf(w, "  %s/\n", p)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "To reinstall later with your existing settings:")
		fmt.Fprintln(w, cliui.Color(uninstallReinstallHint[windows], cliui.Dim))
		fmt.Fprintln(w)
	}
	for _, hint := range uninstallReloadHint[windows] {
		if hint[1] != "" {
			fmt.Fprintln(w, cliui.Color(hint[0], hint[1]))
		} else {
			fmt.Fprintln(w, hint[0])
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Thank you for using Icebath Agent! ☤")
	fmt.Fprintln(w)
}

// Dispatches icebath uninstall to the summary, data, GUI or agent path, gating on a terminal only when it prompts
func CmdUninstall(ctx context.Context, ns map[string]any, w io.Writer, requireTTY func(commandName string)) (int, error) {
	if nsTruthy(ns, "gui_summary") {
		return PrintGUIInstallSummary(ctx, w)
	}
	prompting := !nsTruthy(ns, "yes") && !nsTruthy(ns, "dry_run")
	switch {
	case nsTruthy(ns, "data"):
		if prompting {
			requireTTY("uninstall --data")
		}
		return uninstallRunDataMode(ctx, ns, w)
	case nsTruthy(ns, "gui"):
		if prompting {
			requireTTY("uninstall --gui")
		}
		return uninstallRunGUIMode(ctx, ns, w)
	}
	if prompting {
		requireTTY("uninstall")
	}
	return uninstallRunAgentMode(ctx, ns, w)
}

// Namespace the module entry hands the run functions for one mode, always non interactive
func uninstallModeNamespace(mode string, desktopUserdata any) map[string]any {
	return map[string]any{"gui": mode == "gui", "gui_summary": false, "full": mode == "full", "data": mode == "data",
		"yes": true, "desktop_userdata": desktopUserdata}
}

// Argparse of the uninstall module entry, the mode, the desktop data override and an exit code when rejected
func parseUninstallModuleArgs(argv []string, stdout, stderr io.Writer) (string, any, int, bool) {
	const prog = "icebath " + UninstallModuleEntry
	const usage = "usage: " + prog + " [-h] --mode {gui,lite,full,data}\n"
	fail := func(msg string) (string, any, int, bool) {
		fmt.Fprintf(stderr, "%s%s: error: %s\n", usage, prog, msg)
		return "", nil, 2, true
	}
	mode := ""
	var desktopUserdata any
	modes := []string{"gui", "lite", "full", "data"}
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		name, value, hasValue := strings.Cut(arg, "=")
		if !strings.HasPrefix(arg, "--") || len(name) < 3 {
			if arg == "-h" {
				name = "--help"
			} else {
				return fail("unrecognized arguments: " + strings.Join(argv[i:], " "))
			}
		}
		var matched []string
		for _, option := range []string{"--help", "--mode", "--desktop-userdata"} {
			if option == name {
				matched = []string{option}
				break
			}
			if strings.HasPrefix(option, name) {
				matched = append(matched, option)
			}
		}
		if len(matched) > 1 {
			return fail(fmt.Sprintf("ambiguous option: %s could match %s", name, strings.Join(matched, ", ")))
		}
		if len(matched) == 0 {
			return fail("unrecognized arguments: " + strings.Join(argv[i:], " "))
		}
		option := matched[0]
		if option == "--help" {
			fmt.Fprint(stdout, usage+"\noptions:\n  -h, --help            show this help message and exit\n"+
				"  --mode {gui,lite,full,data}\n                        gui = Chat GUI only; lite = GUI + agent, keep data;\n"+
				"                        full = everything; data = user data only, keep code\n")
			return "", nil, 0, true
		}
		if !hasValue {
			if i+1 >= len(argv) || uninstallModuleOptionLike(argv[i+1]) {
				return fail("argument " + option + ": expected one argument")
			}
			i++
			value = argv[i]
		}
		if option == "--mode" {
			if !slices.Contains(modes, value) {
				return fail(fmt.Sprintf("argument --mode: invalid choice: '%s' (choose from gui, lite, full, data)", value))
			}
			mode = value
			continue
		}
		desktopUserdata = pypath.PathStr(value)
	}
	if mode == "" {
		return fail("the following arguments are required: --mode")
	}
	if desktopUserdata != nil && mode != "data" {
		return fail("--desktop-userdata requires --mode data")
	}
	return mode, desktopUserdata, 0, false
}

// Whether argparse reads the word as an option, negative numbers and words with a space reading as values
func uninstallModuleOptionLike(word string) bool {
	if !strings.HasPrefix(word, "-") || word == "-" {
		return false
	}
	name, _, _ := strings.Cut(word, "=")
	for _, option := range []string{"-h", "--help", "--mode", "--desktop-userdata"} {
		if name == option || (strings.HasPrefix(name, "--") && len(name) > 2 && strings.HasPrefix(option, name)) {
			return true
		}
	}
	return !uninstallNegativeNumber.MatchString(word) && !strings.Contains(word, " ")
}

// Runs one uninstall mode non interactively for the desktop app, the exit code the run ends with
func UninstallModuleMain(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	mode, desktopUserdata, code, done := parseUninstallModuleArgs(argv, stdout, stderr)
	if done {
		return code
	}
	ns := uninstallModeNamespace(mode, desktopUserdata)
	run := uninstallRunAgentMode
	switch {
	case ns["data"] == true:
		run = uninstallRunDataMode
	case ns["gui"] == true:
		run = uninstallRunGUIMode
	}
	code, err := run(ctx, ns, stdout)
	if err != nil {
		fmt.Fprintln(stderr, uninstallErrText(err))
		return 1
	}
	return code
}

// Runs the uninstall module entry and exits when this process was started as one
func DispatchUninstallModuleEntry(ctx context.Context) {
	if len(os.Args) > 1 && os.Args[1] == UninstallModuleEntry {
		os.Exit(UninstallModuleMain(ctx, os.Args[2:], os.Stdout, os.Stderr))
	}
}
