package home

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Where connector tool results save the files they return, under each profile home
var ConnectorDownloadsSubdir = filepath.Join("downloads", "connectors")

// Request id of the synthetic response a stream cut short by the length limit leaves behind
const PartialStreamStubID = "partial-stream-stub"

// Provider finish reason for an output length stop
const FinishReasonLength = "length"

var envReference = regexp.MustCompile(`%([^%]+)%|\$\{([^}]+)\}|\$([A-Za-z0-9_-]+)`)

// Root another process started with environ resolves, empty when the answer depends on anything outside environ
func RootForEnvironment(environ map[string]string) string {
	keys := make([]string, 0, len(environ))
	for k := range environ {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	names := make(map[string]string, len(environ))
	for _, k := range keys {
		names[pystr.Upper(k)] = environ[k]
	}
	profileVar := "HOME"
	if runtime.GOOS == "windows" {
		profileVar = "USERPROFILE"
	}
	profile := pystr.Strip(names[profileVar])
	errNoProfile := errors.New("no user profile folder")
	userHome := func() (string, error) {
		if profile == "" {
			return "", errNoProfile
		}
		return profile, nil
	}
	native, err := platformDefault(func(k string) string { return names[k] }, userHome)
	if err != nil {
		return ""
	}
	raw := pystr.Strip(names[EnvHome])
	if raw == "" {
		return native
	}
	undefined := false
	expanded := replaceLastGroup(envReference, raw, func(name string) string {
		v, ok := names[pystr.Upper(name)]
		undefined = undefined || !ok
		return v
	})
	if undefined {
		return ""
	}
	if expanded == "~" || strings.HasPrefix(expanded, "~/") || strings.HasPrefix(expanded, `~\`) {
		if profile == "" {
			return ""
		}
		expanded = pypath.PathStr(profile) + expanded[1:]
	}
	if !pypath.IsAbs(expanded) {
		return ""
	}
	return rootOf(native, pypath.PathStr(expanded))
}

// Every match replaced by fn over the last group that took part
func replaceLastGroup(re *regexp.Regexp, s string, fn func(string) string) string {
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(s[last:loc[0]])
		for g := len(loc)/2 - 1; g >= 1; g-- {
			if loc[2*g] >= 0 {
				b.WriteString(fn(s[loc[2*g]:loc[2*g+1]]))
				break
			}
		}
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// Account lookup by name, swapped by tests
var lookupUser = user.Lookup

// Effective uid of the process, swapped by tests
var geteuid = os.Geteuid

// Invoking user's native home when this process is root under sudo, empty otherwise
func SudoInvokerDefaultHome() string {
	if runtime.GOOS == "windows" || geteuid() != 0 {
		return ""
	}
	sudoUser := pystr.Strip(os.Getenv("SUDO_USER"))
	if sudoUser == "" || sudoUser == "root" {
		return ""
	}
	u, err := lookupUser(sudoUser)
	if err != nil {
		return ""
	}
	return pypath.PathJoin(u.HomeDir, dirName)
}

var (
	profileFallbackWarned atomic.Bool
	fallbackWarnOut       io.Writer = os.Stderr
)

// Routes the profile fallback warning to w with its once latch rearmed, returning the function that restores both
func CaptureProfileFallbackWarning(w io.Writer) func() {
	prevOut, prevWarned := fallbackWarnOut, profileFallbackWarned.Load()
	fallbackWarnOut = w
	profileFallbackWarned.Store(false)
	return func() {
		fallbackWarnOut = prevOut
		profileFallbackWarned.Store(prevWarned)
	}
}

// Warns once on stderr when the home variable is unset while a non default profile is sticky active
func warnProfileFallbackOnce() {
	if profileFallbackWarned.Swap(true) {
		return
	}
	fallback := PlatformDefault()
	active := ""
	if data, err := os.ReadFile(pypath.PathJoin(fallback, "active_profile")); err == nil && utf8.Valid(data) {
		text := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
		active = pystr.Strip(text)
	}
	if active == "" || active == "default" {
		return
	}
	fmt.Fprintf(fallbackWarnOut, "[ICEBATH_HOME fallback] ICEBATH_HOME is unset but active profile is %s. "+
		"Falling back to %s, which is the DEFAULT profile — not %s. Any data this process writes will land "+
		"in the wrong profile. The subprocess spawner should pass ICEBATH_HOME explicitly (see issue #18594).\n",
		pyrepr.StrRepr(active), fallback, pyrepr.StrRepr(active))
}

var (
	pinMu      sync.Mutex
	pinnedHome string
	pinnedSet  bool
)

// Pins the home this process serves as its own profile for routed profile decisions, unpin clears it
func PinProcess(path string, unpin bool) {
	pinMu.Lock()
	defer pinMu.Unlock()
	pinnedHome, pinnedSet = path, !unpin
}

// Reports whether a host pinned the process home
func ProcessPinned() bool {
	pinMu.Lock()
	defer pinMu.Unlock()
	return pinnedSet
}

// Launch home for routed profile decisions, the expanded pinned home else the process home
func RoutingProcess() string {
	pinMu.Lock()
	pinned, set := pinnedHome, pinnedSet
	pinMu.Unlock()
	if set && pinned != "" {
		return expand(pinned)
	}
	return Process()
}

// Package manager relocatable directory from envVar, then def, then the subdir of the active home
func PackagedDir(ctx context.Context, envVar, def, subdir string) string {
	if override := pystr.Strip(os.Getenv(envVar)); override != "" {
		return pypath.PathStr(override)
	}
	if def != "" {
		return pypath.PathStr(def)
	}
	return pypath.PathJoin(Dir(ctx), subdir)
}

// Optional skills directory honoring package manager wrappers
func OptionalSkillsDir(ctx context.Context, def string) string {
	return PackagedDir(ctx, "ICEBATH_OPTIONAL_SKILLS", def, "optional-skills")
}

// Optional MCP catalog directory honoring package manager wrappers
func OptionalMCPsDir(ctx context.Context, def string) string {
	return PackagedDir(ctx, "ICEBATH_OPTIONAL_MCPS", def, "optional-mcps")
}

// Bundled skills directory honoring package manager wrappers
func BundledSkillsDir(ctx context.Context, def string) string {
	return PackagedDir(ctx, "ICEBATH_BUNDLED_SKILLS", def, "skills")
}

// Historical managed Node directories of a home, bin first on POSIX and the node dir first on Windows
func NodeDirs(ctx context.Context, h string) []string {
	if h == "" {
		h = Dir(ctx)
	}
	nodeDir := pypath.PathJoin(h, "node")
	if runtime.GOOS == "windows" {
		return []string{nodeDir, pypath.PathJoin(nodeDir, "bin")}
	}
	return []string{pypath.PathJoin(nodeDir, "bin"), nodeDir}
}

var windowsNodeShims = map[string][]string{
	"npm": {"npm.cmd", "npm.exe", "npm"}, "npx": {"npx.cmd", "npx.exe", "npx"}, "node": {"node.exe", "node"},
}

// File names a Node tool command may carry on this platform, the cmd shim first on Windows
func CandidateNodeCommandNames(command string) []string {
	base := pypath.PathName(command)
	if runtime.GOOS != "windows" || strings.Contains(base, ".") {
		return []string{base}
	}
	if names, ok := windowsNodeShims[pystr.Lower(base)]; ok {
		return append([]string(nil), names...)
	}
	return []string{base + ".cmd", base + ".exe", base}
}

// Reports whether path exists through any symlink and the process may execute it
func IsExecutableFile(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return executableByProcess(path)
}

// Reports whether a legacy managed Node tree with node, npm or npx sits in the home
func ManagedNodeTreePresent(ctx context.Context, h string) bool {
	for _, dir := range NodeDirs(ctx, h) {
		for _, c := range []string{"node", "npm", "npx"} {
			for _, name := range CandidateNodeCommandNames(c) {
				if st, err := os.Stat(pypath.PathJoin(dir, name)); err == nil && st.Mode().IsRegular() {
					return true
				}
			}
		}
	}
	return false
}

// Directory of the resolved running binary, the install tree secure parent hardening never chmods
func InstallRoot() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	r, err := resolve(exe)
	if err != nil {
		return ""
	}
	return pypath.PathParent(r)
}

// Module roots whose import failure means the shipped tree is inconsistent
var FirstPartyModuleRoots = map[string]bool{
	"agent": true, "acp_adapter": true, "cli": true, "cron": true, "gateway": true, "model_tools": true,
	"plugins": true, "providers": true, "tools": true, "toolsets": true, "run_agent": true,
	"tui_gateway": true, "utils": true,
}

// Reports whether the first dotted segment of name ships with the agent
func IsFirstPartyModule(name string) bool {
	root, _, _ := strings.Cut(name, ".")
	return root != "" && (FirstPartyModuleRoots[root] || strings.HasPrefix(root, "hermes_"))
}

// Python import failure a child runtime reported, ModuleNotFound marking the missing module subclass
type ImportError struct {
	Name           string
	Msg            string
	ModuleNotFound bool
}

func (e *ImportError) Error() string { return e.Msg }

// Recovery guidance lines when err is a first party import failure of a half updated tree, else none
func PartialUpdateHint(err error) []string {
	ie, ok := err.(*ImportError)
	if !ok || ie == nil || ie.ModuleNotFound || !IsFirstPartyModule(ie.Name) {
		return nil
	}
	return []string{
		"",
		"This looks like a partially-updated install: one module was refreshed and a related one was not.",
		"Re-run the update to bring the whole tree to the same version:",
		"    icebath update",
		"If that also fails, reinstall: https://hermes-agent.nousresearch.com",
	}
}

// Prints the error and its partial update guidance to w or stderr, true when guidance applied
func EmitPartialUpdateHint(err error, w io.Writer) bool {
	lines := PartialUpdateHint(err)
	if len(lines) == 0 {
		return false
	}
	if w == nil {
		w = os.Stderr
	}
	for _, line := range append([]string{"Error: " + err.Error()}, lines...) {
		fmt.Fprintln(w, line)
	}
	return true
}
