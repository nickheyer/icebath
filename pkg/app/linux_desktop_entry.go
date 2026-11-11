package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pypil"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

const (
	// Identity the packaged app claims for its window, carried by the entry file name
	desktopAppID = "com.nousresearch.icebath"
	// App id entry file name
	desktopEntryFileName = desktopAppID + ".desktop"
	// Entry name the app id entry replaced, kept as a hidden alias for old pins
	legacyDesktopEntryFileName = "icebath.desktop"
	// Variable an XDG startup notification sets on an app grid launch
	shellLaunchEnvVar = "DESKTOP_STARTUP_ID"
	// Variable naming the pipe Electron writes the reveal byte into
	readyFDEnvVar = "ICEBATH_DESKTOP_READY_FD"
	// Child fd the ready pipe lands on as the first extra file of the launch
	readyChildFD = 3
	// Byte the window reveal writes, any other byte is the wake up of finish
	revealByte = 'r'
)

var (
	desktopShellNames      = []string{"bash", "sh", "dash", "zsh", "ksh"}
	desktopRootTerminators = []string{"\"", "'", " ", "\n", "\t", "\r", "$", "\x00", "/"}
	icebathCodeMarkers     = []string{"icebath_cli", "workspace/icebath_cli"}
	desktopAppEntries      = []string{"apps/desktop/package.json", "workspace/apps/desktop/package.json"}
	hicolorIndexedSizes    = []int{16, 22, 24, 32, 36, 48, 64, 72, 96, 128, 192, 256, 512}
	hicolorInstallSizes    = []int{24, 32, 48, 256}
	pythonInterpreterName  = regexp.MustCompile(`^python[23]?(\d+)?(\.\d+)?$`)
	embeddedLauncherPath   = regexp.MustCompile(`["'\s](/[^\s"'$;)\\]+)`)
	cliImportProbeMu       sync.Mutex
	cliImportProbeCache    = map[string]bool{}
)

// Process facts and steps the desktop entry installer reads
type ldeSteps struct {
	executable           func() string
	geteuid              func() int
	resolveBin           func() (string, bool)
	runQuiet             func(cmd []string, timeout time.Duration, cwd string) (bool, bool)
	canImportCLI         func(interpreter string) bool
	refreshDatabases     func(applicationsDir string) []string
	installIcon          func(icon string) bool
	managementEnabled    func() bool
	copyfile             func(src, dst string) error
	installDesktopEntry  func(projectRoot string) (string, error)
	sleep                func(time.Duration)
	moduleLexicalRoot    func() string
	installStateEnvsRoot func(checkoutRoot string) string
	resolveExecCommand   func(projectRoot string) string
}

// Steps the desktop entry installer runs, assigned at package init
var lde ldeSteps

func init() {
	lde = ldeSteps{
		executable:          runningExecutable,
		geteuid:             os.Geteuid,
		resolveBin:          resolveHermesBin,
		runQuiet:            runQuiet,
		canImportCLI:        canImportIcebathCLI,
		refreshDatabases:    refreshDesktopDatabases,
		installIcon:         installIconToHicolor,
		managementEnabled:   launcherEntryManagementEnabled,
		copyfile:            pyshutil.CopyFile,
		installDesktopEntry: installDesktopEntry,
		sleep:               time.Sleep,
		moduleLexicalRoot:   executableLexicalRoot,
		installStateEnvsRoot: func(root string) string {
			return filepath.Join(pm.InstallStateDir(context.Background(), root), "environments")
		},
		resolveExecCommand: resolveExecCommand,
	}
}

// Absolute path of the running executable, the sys.executable of this process
func runningExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if abs, err := filepath.Abs(exe); err == nil {
		return abs
	}
	return exe
}

// Checkout the running executable sits in, lexical so installer written shim text still matches
func executableLexicalRoot() string { return filepath.Dir(lde.executable()) }

// True where XDG desktop entries exist, Linux and the BSDs
func desktopEntrySupported() bool {
	p := guiPlatform()
	return strings.HasPrefix(p, "linux") || strings.HasPrefix(p, "freebsd") || strings.HasPrefix(p, "openbsd") || strings.HasPrefix(p, "netbsd")
}

// XDG data home, the explicit variable when set else the share dir under home
func xdgDataHome() string {
	if raw := os.Getenv("XDG_DATA_HOME"); raw != "" && pystr.Strip(raw) != "" {
		return pypath.Expanduser(raw)
	}
	return filepath.Join(gui.homeDir(), ".local", "share")
}

// Path of the app id desktop entry
func desktopEntryPath() string {
	return filepath.Join(xdgDataHome(), "applications", desktopEntryFileName)
}

// App icon shipped in the checkout
func desktopIconPath(projectRoot string) string {
	return filepath.Join(projectRoot, "apps", "desktop", "assets", "icon.png")
}

// Interpreter path for the persisted entry, lexical inside a venv so its site packages stay reachable
func runningInterpreter() string {
	lexical := lde.executable()
	for base := filepath.Dir(lexical); ; base = filepath.Dir(base) {
		if st, err := os.Stat(filepath.Join(base, "pyvenv.cfg")); err == nil && st.Mode().IsRegular() {
			return lexical
		}
		if filepath.Dir(base) == base {
			break
		}
	}
	if resolved, err := pypath.Resolve(lexical); err == nil {
		return resolved
	}
	return lexical
}

// Whether the interpreter can start the CLI unaided, assumed when it cannot be probed and cached otherwise
func canImportIcebathCLI(interpreter string) bool {
	cliImportProbeMu.Lock()
	cached, ok := cliImportProbeCache[interpreter]
	cliImportProbeMu.Unlock()
	if ok {
		return cached
	}
	root, _ := filepath.Abs(string(os.PathSeparator))
	result, ran := lde.runQuiet([]string{interpreter, "-I", "-c", "import icebath_cli.main"}, 15*time.Second, root)
	if !ran {
		return true
	}
	cliImportProbeMu.Lock()
	cliImportProbeCache[interpreter] = result
	cliImportProbeMu.Unlock()
	return result
}

// Running executable, which starts the CLI by definition
func runningInterpreterFallback() string { return lde.executable() }

// Absolute Exec command line of the desktop launcher, the real launcher else the module form
func resolveExecCommand(projectRoot string) string {
	binPath := resolveHermesBinForDesktopEntry(lde.resolveBin, projectRoot)
	interpreter := runningInterpreter()
	if !lde.canImportCLI(interpreter) {
		interpreter = runningInterpreterFallback()
	}
	argv := append([]string{interpreter, "-m", "icebath_cli.main"}, desktopArgvTail(projectRoot)...)
	if binPath != "" {
		resolved, err := pypath.Resolve(binPath)
		if err != nil {
			resolved = binPath
		}
		var prefix []string
		if needsInterpreter(resolved) {
			prefix = []string{interpreter}
		}
		argv = append(append(prefix, resolved), desktopArgvTail(projectRoot)...)
	}
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = quoteExecArg(a)
	}
	return strings.Join(quoted, " ")
}

// Desktop subcommand arguments of a persisted launcher, skipping the build once a packaged app exists
func desktopArgvTail(projectRoot string) []string {
	if projectRoot == "" {
		return []string{"desktop"}
	}
	release := filepath.Join(projectRoot, "apps", "desktop", "release")
	for _, dist := range []string{"linux-unpacked", "linux-arm64-unpacked"} {
		for _, name := range []string{"icebath", "Icebath"} {
			if _, err := os.Stat(filepath.Join(release, dist, name)); err == nil {
				return []string{"desktop", "--skip-build"}
			}
		}
	}
	return []string{"desktop"}
}

// A python interpreter binary inside a bin or Scripts dir, never a launcher
func isInterpreterPath(candidate string) bool {
	return pythonInterpreterName.MatchString(strings.ToLower(pypath.PathName(candidate))) &&
		slices.Contains([]string{"bin", "scripts"}, pypath.PathName(pypath.PathParent(candidate)))
}

// Whether a path equals root or lies below it
func pathAtOrUnder(path, root string) bool {
	return path == root || slices.Contains(pypath.PathParents(path), root)
}

// Whether a launcher candidate is a launch context artifact of the checkout rather than a durable launcher
func insideCheckout(candidate, checkoutRoot, originalArgv0 string) bool {
	path, err := pypath.Resolve(candidate)
	if err != nil {
		return false
	}
	roots := []string{checkoutRoot}
	if resolvedRoot, err := pypath.Resolve(checkoutRoot); err == nil && resolvedRoot != checkoutRoot {
		roots = append(roots, resolvedRoot)
	}
	for _, root := range roots {
		if pathAtOrUnder(path, root) {
			return true
		}
	}
	a, errA := os.Stat(path)
	b, errB := os.Stat(originalArgv0)
	if errA != nil || errB != nil {
		return false
	}
	return os.SameFile(a, b) && isInterpreterPath(path)
}

// True when the candidate is this checkout's managed environment console script
func isThisCheckoutManagedCLI(candidate, checkoutRoot string) bool {
	path, err := pypath.Resolve(candidate)
	if err != nil {
		return false
	}
	generations, err := pypath.Resolve(lde.installStateEnvsRoot(checkoutRoot))
	if err != nil {
		return false
	}
	return pathAtOrUnder(path, generations)
}

// Whether a candidate is a durable launcher that can serve the desktop for this checkout
func durableDesktopLauncher(candidate, checkoutRoot, moduleLexicalRoot, originalArgv0 string) bool {
	if insideCheckout(candidate, checkoutRoot, originalArgv0) || isThisCheckoutManagedCLI(candidate, checkoutRoot) ||
		isThisCheckoutManagedCLI(candidate, moduleLexicalRoot) {
		return false
	}
	verdict, known := canServeDesktop(candidate, 2)
	return !known || verdict
}

// Launcher for the persisted entry, skipping checkout internal and desktop less candidates before the known wrappers
func resolveHermesBinForDesktopEntry(resolveFn func() (string, bool), checkoutRoot string) string {
	if checkoutRoot == "" {
		checkoutRoot = lde.moduleLexicalRoot()
	}
	if abs, err := filepath.Abs(checkoutRoot); err == nil {
		checkoutRoot = abs
	}
	moduleLexicalRoot := lde.moduleLexicalRoot()
	argv := relaunchArgv()
	originalArgv0 := ""
	if len(argv) > 0 {
		originalArgv0 = argv[0]
	}
	primary, primaryOK := resolveFn()
	if primaryOK && primary != "" && durableDesktopLauncher(primary, checkoutRoot, moduleLexicalRoot, originalArgv0) {
		return primary
	}
	rerouted, reroutedOK := "", false
	if len(argv) > 0 {
		argv[0] = ""
		rerouted, reroutedOK = resolveFn()
		argv[0] = originalArgv0
	} else {
		rerouted, reroutedOK = resolveFn()
	}
	if primaryOK && primary != "" && reroutedOK && durableDesktopLauncher(rerouted, checkoutRoot, moduleLexicalRoot, originalArgv0) {
		return rerouted
	}
	for _, candidate := range knownWrapperCandidates() {
		if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() && accessExecutable(candidate) &&
			wrapperShebangSafe(candidate) &&
			(wrapperTargetsCheckout(candidate, checkoutRoot) || wrapperTargetsCheckout(candidate, moduleLexicalRoot)) {
			return candidate
		}
	}
	return ""
}

// True for an ELF or PE header
func isNativeBinary(head []byte) bool {
	return bytes.HasPrefix(head, []byte("\x7fELF")) || bytes.HasPrefix(head, []byte("MZ"))
}

// First line of a file head decoded with replacement
func headFirstLine(head []byte) string {
	lines := pystr.Splitlines(pystr.DecodeUTF8Replace(head), false)
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// Whether an executable wrapper can run in the desktop environment, native and shell launchers by construction
func wrapperShebangSafe(wrapper string) bool {
	head, ok := readHead(wrapper, 4096)
	if !ok {
		return false
	}
	if isNativeBinary(head) {
		return true
	}
	if !bytes.HasPrefix(head, []byte("#!")) {
		return false
	}
	shebang := headFirstLine(head)
	tokens := shebangTokens(shebang)
	if len(tokens) == 0 {
		return false
	}
	interp := pypath.PathName(tokens[0])
	if interp == "env" {
		target := ""
		for _, t := range tokens[1:] {
			if !strings.HasPrefix(t, "-") {
				target = t
				break
			}
		}
		return slices.Contains(desktopShellNames, pypath.PathName(target)) || !shebangEscapesRunningEnv(shebang)
	}
	if slices.Contains(desktopShellNames, interp) {
		return true
	}
	if !strings.Contains(strings.ToLower(interp), "python") {
		return false
	}
	return !shebangEscapesRunningEnv(shebang)
}

// Whether a launcher script launches this checkout, the root ending at a boundary or continuing into the tree
func wrapperTargetsCheckout(wrapper, checkoutRoot string) bool {
	head, ok := readHead(wrapper, 4096)
	if !ok {
		return false
	}
	if isNativeBinary(head) {
		return true
	}
	text := pystr.DecodeUTF8Replace(head)
	lexicalRoot, err := filepath.Abs(checkoutRoot)
	if err != nil {
		lexicalRoot = checkoutRoot
	}
	roots := []string{checkoutRoot, lexicalRoot}
	if resolved, err := pypath.Resolve(lexicalRoot); err == nil {
		roots = append(roots, resolved)
	}
	stripped := strings.TrimRight(text, "\r\n")
	for _, root := range roots {
		if strings.HasSuffix(stripped, root) {
			return true
		}
		for _, t := range desktopRootTerminators {
			if strings.Contains(text, root+t) {
				return true
			}
		}
	}
	return false
}

// Install tree a bin launcher belongs to, the venv parent for a venv bin dir
func launcherTree(path string) string {
	parent := filepath.Dir(path)
	if !slices.Contains([]string{"bin", "Scripts", "scripts"}, filepath.Base(parent)) {
		return parent
	}
	tree := filepath.Dir(parent)
	if slices.Contains([]string{"venv", ".venv", "env"}, filepath.Base(tree)) {
		return filepath.Dir(tree)
	}
	return tree
}

// Desktop capability of a tree, false only for a code tree without the desktop app, unknown otherwise
func treeDesktopState(tree string) (bool, bool) {
	for _, entry := range desktopAppEntries {
		if st, err := os.Stat(filepath.Join(tree, entry)); err == nil && st.Mode().IsRegular() {
			return true, true
		}
	}
	if selfupdate.IsBundledPayload(tree) {
		return true, true
	}
	for _, marker := range icebathCodeMarkers {
		if st, err := os.Stat(filepath.Join(tree, marker)); err == nil && st.IsDir() {
			return false, true
		}
	}
	return false, false
}

// First absolute path to an existing bin launcher embedded in a shell wrapper
func embeddedLauncherTarget(wrapper string) string {
	head, ok := readHead(wrapper, 4096)
	if !ok {
		return ""
	}
	for _, m := range embeddedLauncherPath.FindAllStringSubmatch(pystr.DecodeUTF8Replace(head), -1) {
		if strings.Contains(m[1], "/bin/") {
			if st, err := os.Stat(m[1]); err == nil && st.Mode().IsRegular() {
				return m[1]
			}
		}
	}
	return ""
}

// Desktop capability of a launcher path, shell wrappers followed to their target, unknown for unfamiliar shapes
func canServeDesktop(candidate string, depth int) (bool, bool) {
	if st, err := os.Stat(candidate); err != nil || !st.Mode().IsRegular() {
		return false, false
	}
	head, ok := readHead(candidate, 256)
	if !ok || !bytes.HasPrefix(head, []byte("#!")) {
		return false, false
	}
	tokens := shebangTokens(headFirstLine(head))
	if depth > 0 && len(tokens) > 0 {
		name := pypath.PathName(tokens[0])
		if slices.Contains(desktopShellNames, name) || name == "env" {
			if target := embeddedLauncherTarget(candidate); target != "" {
				if verdict, known := canServeDesktop(target, depth-1); known {
					return verdict, true
				}
			}
		}
	}
	return treeDesktopState(launcherTree(candidate))
}

// Desktop capability of a rendered Exec line judged by its launcher, unknown for the module form
func persistedExecServesDesktop(execCommand string) (bool, bool) {
	tokens, err := pyshlex.SplitPOSIX(execCommand, false)
	if err != nil || len(tokens) == 0 {
		return false, false
	}
	candidate := tokens[0]
	if isInterpreterPath(candidate) {
		if len(tokens) > 1 && tokens[1] != "-m" {
			candidate = tokens[1]
		} else {
			return false, false
		}
	}
	return canServeDesktop(candidate, 2)
}

// Durable installed launcher locations, most likely first
func knownWrapperCandidates() []string {
	var candidates []string
	if prefix := os.Getenv("PREFIX"); prefix != "" {
		candidates = append(candidates, filepath.Join(prefix, "bin", "icebath"))
	}
	if lde.geteuid() == 0 {
		candidates = append(candidates, "/usr/local/bin/icebath")
	}
	return append(candidates, filepath.Join(gui.homeDir(), ".local", "bin", "icebath"))
}

// Quotes an Exec argument that carries a character the desktop entry spec reserves
func quoteExecArg(arg string) string {
	if !strings.ContainsAny(arg, " \t\n\"'\\><~|&;$*?#()`") {
		return arg
	}
	escaped := strings.ReplaceAll(strings.ReplaceAll(arg, "\\", "\\\\"), "\"", "\\\"")
	return "\"" + escaped + "\""
}

// App id entry text whose identity lives in the file name and StartupWMClass
func renderDesktopEntry(execCommand, icon string) string {
	return "[Desktop Entry]\n" +
		"Type=Application\n" +
		"Name=Icebath\n" +
		"GenericName=Icebath Desktop\n" +
		"Comment=Launch Icebath Desktop\n" +
		"Exec=" + execCommand + "\n" +
		"Icon=" + icon + "\n" +
		"Terminal=false\n" +
		"Categories=Utility;\n" +
		"StartupNotify=true\n" +
		"StartupWMClass=" + desktopAppID + "\n"
}

// App id entry hidden from the app grid for the legacy file name
func renderLegacyAliasEntry(execCommand, icon string) string {
	return renderDesktopEntry(execCommand, icon) + "NoDisplay=true\n"
}

// Reindexes the menu caches with each tool that exists and names the ones that ran
func refreshDesktopDatabases(applicationsDir string) []string {
	var ran []string
	if updateDB := gui.which("update-desktop-database"); updateDB != "" {
		if ok, _ := lde.runQuiet([]string{updateDB, applicationsDir}, 60*time.Second, ""); ok {
			ran = append(ran, "update-desktop-database")
		}
	}
	for _, tool := range []string{"kbuildsycoca6", "kbuildsycoca5"} {
		if resolved := gui.which(tool); resolved != "" {
			if ok, _ := lde.runQuiet([]string{resolved, "--noincremental"}, 60*time.Second, ""); ok {
				ran = append(ran, tool)
			}
			break
		}
	}
	return ran
}

// Exit status success of a silenced command and whether it could run at all
func runQuiet(cmd []string, timeout time.Duration, cwd string) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = cwd
	err := c.Run()
	if ctx.Err() != nil {
		return false, false
	}
	if err == nil {
		return true, true
	}
	if _, ok := err.(*exec.ExitError); ok {
		return false, true
	}
	return false, false
}

// Width and height from a PNG IHDR, false when unreadable
func pngDimensions(raw []byte) (int, int, bool) {
	if len(raw) >= 24 && bytes.Equal(raw[:8], []byte("\x89PNG\r\n\x1a\n")) && bytes.Equal(raw[12:16], []byte("IHDR")) {
		return int(binary.BigEndian.Uint32(raw[16:20])), int(binary.BigEndian.Uint32(raw[20:24])), true
	}
	return 0, 0, false
}

// Fixed size hicolor dir the theme indexes for an icon, never scalable
func hicolorSubdir(width, height int, known bool) string {
	if !known {
		return "256x256"
	}
	if slices.Contains(hicolorIndexedSizes, width) && width == height {
		return fmt.Sprintf("%dx%d", width, width)
	}
	if width != height || width <= 0 || width > 256 {
		return "256x256"
	}
	nearest := hicolorIndexedSizes[0]
	for _, size := range hicolorIndexedSizes {
		if abs(size-width) < abs(nearest-width) {
			nearest = size
		}
	}
	return fmt.Sprintf("%dx%d", nearest, nearest)
}

// Absolute value of an int
func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// Installed icon path for a hicolor size dir
func hicolorIconDest(subdir string) string {
	return filepath.Join(xdgDataHome(), "icons", "hicolor", subdir, "apps", "icebath.png")
}

// Drops a leftover PNG from the scalable dir, true when one was removed
func removeStaleScalableIcon() bool {
	stale := hicolorIconDest("scalable")
	st, err := os.Stat(stale)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	return os.Remove(stale) == nil
}

// Reindexes the user hicolor tree with the first cache tool present
func refreshHicolorCache() {
	hicolor := filepath.Join(xdgDataHome(), "icons", "hicolor")
	for _, tool := range []string{"gtk-update-icon-cache", "gtk4-update-icon-cache"} {
		if resolved := gui.which(tool); resolved != "" {
			lde.runQuiet([]string{resolved, "-f", "-t", hicolor}, 60*time.Second, "")
			return
		}
	}
}

// Lanczos resized PNGs of the icon for each panel size, nil when it does not decode
func resizedHicolorPNGs(raw []byte) map[string][]byte {
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	rgba := pypil.ToNRGBA(img)
	out := map[string][]byte{}
	for _, size := range hicolorInstallSizes {
		var buf bytes.Buffer
		if err := png.Encode(&buf, pypil.ResizeLanczos(rgba, size, size)); err != nil {
			return nil
		}
		out[fmt.Sprintf("%dx%d", size, size)] = buf.Bytes()
	}
	return out
}

// Writes the icons keyed by hicolor size dir in size order, true when any file changed
func writeHicolorPNGs(files map[string][]byte) (bool, error) {
	wrote := false
	for _, size := range hicolorInstallSizes {
		subdir := fmt.Sprintf("%dx%d", size, size)
		data, ok := files[subdir]
		if !ok {
			continue
		}
		dest := hicolorIconDest(subdir)
		if current, err := os.ReadFile(dest); err == nil && bytes.Equal(current, data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
			return wrote, err
		}
		if err := os.WriteFile(dest, data, 0o666); err != nil {
			return wrote, err
		}
		wrote = true
	}
	return wrote, nil
}

// Installs the app icon into the user hicolor tree so the themed icon name resolves, false on a filesystem error
func installIconToHicolor(icon string) bool {
	raw, err := os.ReadFile(icon)
	if err != nil {
		return false
	}
	wrote := false
	if resized := resizedHicolorPNGs(raw); resized != nil {
		if wrote, err = writeHicolorPNGs(resized); err != nil {
			return false
		}
	} else {
		w, h, known := pngDimensions(raw)
		dest := hicolorIconDest(hicolorSubdir(w, h, known))
		current, readErr := os.ReadFile(dest)
		wrote = !(readErr == nil && bytes.Equal(current, raw))
		if wrote {
			if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
				return false
			}
			if err := lde.copyfile(icon, dest); err != nil {
				return false
			}
		}
	}
	if removeStaleScalableIcon() || wrote {
		refreshHicolorCache()
	}
	return true
}

// Whether config allows rewriting an existing launcher entry, any config error reading as enabled
func launcherEntryManagementEnabled() bool {
	ctx := context.Background()
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		return true
	}
	desktopCfg, _ := cfg["desktop"].(map[string]any)
	raw, present := desktopCfg["manage_launcher_entry"]
	if !present {
		return true
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		return !slices.Contains([]string{"false", "0", "no", "off"}, pystr.Lower(pystr.Strip(v)))
	}
	return true
}

// Rewrites a legacy entry that still names this app as a hidden alias, true when it was rewritten
func aliasLegacyDesktopEntry(applicationsDir, execCommand, icon string) (bool, error) {
	legacy := filepath.Join(applicationsDir, legacyDesktopEntryFileName)
	raw, err := os.ReadFile(legacy)
	if err != nil {
		return false, nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return false, err
	}
	names := false
	for _, line := range pystr.Splitlines(text, false) {
		if pystr.Strip(line) == "Name=Icebath" {
			names = true
			break
		}
	}
	if !names {
		return false, nil
	}
	alias := renderLegacyAliasEntry(execCommand, icon)
	if text == alias {
		return false, nil
	}
	if err := pyfs.AtomicWriteText(context.Background(), legacy, alias, ".tmp_"); err != nil {
		if pypath.IsOSError(err) {
			return false, nil
		}
		return false, err
	}
	return os.Chmod(legacy, 0o755) == nil, nil
}

// Writes the app id entry, empty off Linux, after a failed write or for an Exec unable to serve
func installDesktopEntry(projectRoot string) (string, error) {
	if !desktopEntrySupported() {
		return "", nil
	}
	entryPath := desktopEntryPath()
	manageEnabled := lde.managementEnabled()
	if st, err := os.Stat(entryPath); err == nil && st.Mode().IsRegular() && !manageEnabled {
		return entryPath, nil
	}
	execCommand := lde.resolveExecCommand(projectRoot)
	if verdict, known := persistedExecServesDesktop(execCommand); known && !verdict {
		return "", nil
	}
	icon := desktopIconPath(projectRoot)
	iconIsFile := false
	if st, err := os.Stat(icon); err == nil && st.Mode().IsRegular() {
		iconIsFile = true
	}
	iconValue := "icebath"
	if iconIsFile {
		iconValue = icon
		if lde.installIcon(icon) {
			iconValue = "icebath"
		}
	}
	contents := renderDesktopEntry(execCommand, iconValue)
	if err := os.MkdirAll(filepath.Dir(entryPath), 0o777); err != nil {
		return "", nil
	}
	unchanged := false
	if st, err := os.Stat(entryPath); err == nil && st.Mode().IsRegular() {
		raw, err := os.ReadFile(entryPath)
		if err != nil {
			return "", nil
		}
		text, err := pystr.DecodeUTF8Sig(raw)
		if err != nil {
			return "", err
		}
		unchanged = text == contents
	}
	if !unchanged {
		if err := pyfs.AtomicWriteText(context.Background(), entryPath, contents, ".tmp_"); err != nil {
			if pypath.IsOSError(err) {
				return "", nil
			}
			return "", err
		}
		if err := os.Chmod(entryPath, 0o755); err != nil {
			return "", nil
		}
	}
	aliased := false
	if manageEnabled {
		var err error
		if aliased, err = aliasLegacyDesktopEntry(filepath.Dir(entryPath), execCommand, iconValue); err != nil {
			return "", err
		}
	}
	if aliased || !unchanged {
		lde.refreshDatabases(filepath.Dir(entryPath))
	}
	return entryPath, nil
}

// True when this process was started from the app grid through an XDG startup notification
func launchedFromShell(environ map[string]string) bool {
	if environ == nil {
		return os.Getenv(shellLaunchEnvVar) != ""
	}
	return environ[shellLaunchEnvVar] != ""
}

// Installs the entry once Electron reports its window on screen, never while the shell still starts the app
type deferredDesktopEntryInstall struct {
	projectRoot string
	install     func(string) (string, error)
	settle      time.Duration
	out         io.Writer
	read        *os.File
	write       *os.File
	done        chan struct{}
}

// Pipe and waiter of a deferred entry install for the checkout, the waiter not yet started
func newDeferredDesktopEntryInstall(w io.Writer, projectRoot string, install func(string) (string, error), settle time.Duration) (*deferredDesktopEntryInstall, error) {
	if install == nil {
		install = lde.installDesktopEntry
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &deferredDesktopEntryInstall{projectRoot: projectRoot, install: install, settle: settle, out: w, read: read, write: write, done: make(chan struct{})}, nil
}

// Child environment naming the ready pipe fd the launch hands Electron
func (d *deferredDesktopEntryInstall) childEnv(env map[string]string) map[string]string {
	env[readyFDEnvVar] = strconv.Itoa(readyChildFD)
	return env
}

// Files the launch passes to the child, the ready pipe write end first
func (d *deferredDesktopEntryInstall) passFds() []*os.File { return []*os.File{d.write} }

// Starts the waiter for the reveal byte
func (d *deferredDesktopEntryInstall) start() { go d.waitForReveal() }

// Installs the entry after the reveal byte and its settle delay, skipping on any other wake up
func (d *deferredDesktopEntryInstall) waitForReveal() {
	defer close(d.done)
	data := make([]byte, 1)
	n, err := d.read.Read(data)
	if err != nil || n != 1 || data[0] != revealByte {
		return
	}
	if d.settle > 0 {
		lde.sleep(d.settle)
	}
	entry, err := d.install(d.projectRoot)
	if err != nil {
		fmt.Fprintf(d.out, "⚠ Could not install the desktop launcher entry: %s\n", err.Error())
	} else if entry != "" {
		fmt.Fprintf(d.out, "✓ Desktop launcher entry installed: %s\n", entry)
	}
}

// Wakes a still waiting reader, lets a triggered install finish within fifteen seconds and closes the pipe
func (d *deferredDesktopEntryInstall) finish() {
	_, _ = d.write.Write([]byte("x"))
	select {
	case <-d.done:
	case <-time.After(15 * time.Second):
	}
	_ = d.read.Close()
	_ = d.write.Close()
}
