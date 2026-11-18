// Serves every go test binary a private user home under the temp dir in place of the developer's real one
package homeguard

import (
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Name prefix of the private homes, followed by the pid namespace and the pid of the owning test binary
const privatePrefix = "icebath-test-home-"

// Variable handing the developer cache directory down to test binaries a test binary starts
const sharedCacheEnv = "HOMEGUARD_SHARED_CACHE"

// Variable handing the temp dir of the first test binary down to test binaries it starts under another temp dir
const tempRootEnv = "HOMEGUARD_TEMP_ROOT"

// Directories of the go toolchain a test spawned go command keeps using after the home moves
var toolchainDirs = []struct {
	name string
	base func() (string, error)
	rel  []string
}{
	{"GOCACHE", os.UserCacheDir, []string{"go-build"}},
	{"GOPATH", os.UserHomeDir, []string{"go"}},
	{"GOENV", os.UserConfigDir, []string{"go", "env"}},
}

// Base directory variables that follow the private home once removed
var xdgHomes = []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"}

// Search path variables whose entries under the developer home are dropped
var xdgSearchPaths = []string{"XDG_CONFIG_DIRS", "XDG_DATA_DIRS"}

// Agent homes under the developer home a pointer variable must not keep, icebath and the agent installed beside it
var productDirs = []string{".icebath", ".hermes"}

var guard struct {
	active                bool
	home, original, cache string
}

func init() {
	if testing.Testing() {
		isolate()
	}
}

// Private home of this test binary, empty outside go test binaries
func Home() string {
	if !guard.active {
		return ""
	}
	return guard.home
}

// Home the developer launched the test binary with, empty outside go test binaries
func Original() string {
	if !guard.active {
		return ""
	}
	return guard.original
}

// Developer cache directory where test suites keep pinned downloads between runs, empty outside go test binaries
func SharedCache() string {
	if !guard.active {
		return ""
	}
	return guard.cache
}

// Current account with its home swapped for the private home inside a test binary
func Current() (*user.User, error) {
	return swap(user.Current())
}

// Account of uid with its home swapped for the private home when uid is this process inside a test binary
func LookupID(uid string) (*user.User, error) {
	return swap(user.LookupId(uid))
}

// Account named name with its home swapped for the private home when it is this process inside a test binary
func Lookup(name string) (*user.User, error) {
	return swap(user.Lookup(name))
}

// Passwd home of uid, the private home when uid is this process inside a test binary
func AccountDir(uid int, dir string) string {
	if guard.active && uid == os.Getuid() {
		return guard.home
	}
	return dir
}

// Copy of an account whose home is the private home when the account is this process inside a test binary
func swap(u *user.User, err error) (*user.User, error) {
	if err != nil || u == nil || !guard.active || u.Uid != strconv.Itoa(os.Getuid()) {
		return u, err
	}
	out := *u
	out.HomeDir = guard.home
	return &out, nil
}

// Points HOME and every home derived variable at a private path unless a test already gave this binary a temp home
func isolate() {
	tmp := tempBase()
	original := userHomeEnv()
	if os.Getenv(tempRootEnv) == "" {
		os.Setenv(tempRootEnv, tmp)
	}
	guard.cache = os.Getenv(sharedCacheEnv)
	if guard.cache == "" {
		guard.cache, _ = os.UserCacheDir()
		os.Setenv(sharedCacheEnv, guard.cache)
	}
	if original != "" && underTemp(resolve(original)) {
		guard.active, guard.home, guard.original = true, original, original
		return
	}
	if original == "" {
		if u, err := user.Current(); err == nil {
			original = u.HomeDir
		}
	}
	reapStale(tmp)
	private := filepath.Join(tmp, privatePrefix+pidNamespace()+"-"+strconv.Itoa(os.Getpid()))
	os.RemoveAll(private)
	pinToolchain()
	setHome(private)
	for _, name := range xdgHomes {
		os.Unsetenv(name)
	}
	for _, name := range xdgSearchPaths {
		dropUnder(name, original)
	}
	os.Setenv("XDG_RUNTIME_DIR", filepath.Join(private, ".run"))
	os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
	dropProductPointers(original)
	guard.active, guard.home, guard.original = true, private, original
}

// Directory test temp dirs are made in, GOTMPDIR as testing TempDir honours it or the system temp dir
func tempBase() string {
	if dir := os.Getenv("GOTMPDIR"); dir != "" {
		return resolve(dir)
	}
	return resolve(os.TempDir())
}

// Whether path lies in the system temp dir, GOTMPDIR or the inherited temp root, a root holding the account home never counting
func underTemp(path string) bool {
	account := ""
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		account = resolve(u.HomeDir)
	}
	roots := []string{resolve(os.TempDir()), tempBase()}
	if inherited := os.Getenv(tempRootEnv); inherited != "" {
		roots = append(roots, resolve(inherited))
	}
	for _, root := range roots {
		if within(path, root) && (account == "" || !within(account, root)) {
			return true
		}
	}
	return false
}

// Developer home from the environment the way os.UserHomeDir reads it
func userHomeEnv() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("USERPROFILE")
	}
	return os.Getenv("HOME")
}

// Home variables of the platform pointed at the private home
func setHome(private string) {
	os.Setenv("HOME", private)
	if runtime.GOOS == "windows" {
		os.Setenv("USERPROFILE", private)
		os.Setenv("LOCALAPPDATA", filepath.Join(private, "AppData", "Local"))
		os.Setenv("APPDATA", filepath.Join(private, "AppData", "Roaming"))
		os.Unsetenv("HOMEDRIVE")
		os.Unsetenv("HOMEPATH")
	}
}

// Fixes the go toolchain directories at their developer values so a spawned go command reuses its caches
func pinToolchain() {
	for _, dir := range toolchainDirs {
		if os.Getenv(dir.name) != "" {
			continue
		}
		if base, err := dir.base(); err == nil && base != "" {
			os.Setenv(dir.name, filepath.Join(append([]string{base}, dir.rel...)...))
		}
	}
}

// Removes the entries of a path list variable that lie under root, unsetting it when none remain
func dropUnder(name, root string) {
	value, ok := os.LookupEnv(name)
	if !ok || root == "" {
		return
	}
	var kept []string
	for _, entry := range filepath.SplitList(value) {
		if entry != "" && !within(resolve(entry), resolve(root)) {
			kept = append(kept, entry)
		}
	}
	if len(kept) == 0 {
		os.Unsetenv(name)
		return
	}
	os.Setenv(name, strings.Join(kept, string(os.PathListSeparator)))
}

// Unsets every ICEBATH_ variable naming the developer home or a product home inside it
func dropProductPointers(original string) {
	if original == "" {
		return
	}
	root := resolve(original)
	for _, entry := range os.Environ() {
		name, value, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(name, "ICEBATH_") || value == "" {
			continue
		}
		for _, part := range filepath.SplitList(value) {
			if pointsIntoProductHome(resolve(expandTilde(part, original)), root) {
				os.Unsetenv(name)
				break
			}
		}
	}
}

// Whether path is the developer home itself or lies in a product home directory beneath it
func pointsIntoProductHome(path, root string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return false
	}
	first := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	for _, dir := range productDirs {
		if first == dir || strings.HasPrefix(first, dir) {
			return true
		}
	}
	return false
}

// Path with a leading tilde replaced by home
func expandTilde(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return filepath.Join(home, path[2:])
	}
	return path
}

// Removes the private homes of exited test binaries of this pid namespace
func reapStale(tmp string) {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	own := pidNamespace() + "-"
	for _, e := range entries {
		rest, ok := strings.CutPrefix(e.Name(), privatePrefix+own)
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(rest)
		if err != nil || pid <= 0 || pid == os.Getpid() || alive(pid) {
			continue
		}
		os.RemoveAll(filepath.Join(tmp, e.Name()))
	}
}

// Absolute path with symlinks resolved as far as they exist
func resolve(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	dir, base := filepath.Split(abs)
	if dir == abs || dir == "" {
		return abs
	}
	return filepath.Join(resolve(filepath.Clean(dir)), base)
}

// Whether path equals root or lies beneath it
func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
