// Resolves the icebath home directory, its named profiles and the files inside them
package home

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/homeguard"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Environment variable naming the home directory of the running process
const EnvHome = "ICEBATH_HOME"

// Environment variable appending a suffix to the platform default directory name
const EnvSuffix = "ICEBATH_DATA_DIR_SUFFIX"

// Directory name under the user home on POSIX systems
const dirName = ".icebath"

// DirName is the dot directory name of a default home
const DirName = dirName

// Directory holding named profiles under the root home
const ProfilesDir = "profiles"

// Directory beside the profile directories holding deletion tombstones
const deletedDir = ".deleted"

// Profile id grammar every profile directory gate shares
var ProfileIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Files whose presence marks a directory as a real home
var homeMarkers = []string{"config.yaml", ".env", "state.db"}

// Files whose presence marks a profile directory as a profile and not a side effect
var identityMarkers = []string{"config.yaml", ".env", "SOUL.md", "profile.yaml", "auth.json", "state.db"}

type overrideKey struct{}

// Returns a context whose home resolution answers path instead of the process home
func WithOverride(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, overrideKey{}, path)
}

// Home override carried by ctx, empty when none was set
func Override(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(overrideKey{}).(string); ok {
		return v
	}
	return ""
}

// Expands a tilde and environment references in a home path
func expand(path string) string {
	return filepath.Clean(pathutil.ExpandUser(pathutil.ExpandEnv(path)))
}

// Platform default home with the data directory suffix applied
func platformDefault(environ func(string) string, userHome func() (string, error)) (string, error) {
	suffix := environ(EnvSuffix)
	if runtime.GOOS == "windows" {
		base := strings.TrimSpace(environ("LOCALAPPDATA"))
		if base == "" {
			h, err := userHome()
			if err != nil {
				return "", err
			}
			base = pathutil.Join(h, "AppData", "Local")
		}
		return pathutil.Join(base, "icebath"+suffix), nil
	}
	h, err := userHome()
	if err != nil {
		return "", err
	}
	return pathutil.Join(h, dirName+suffix), nil
}

// Patched pathlib Path.home lookup, nil while the real lookup serves
var userHomeLookup atomic.Pointer[func() (string, error)]

// User home like pathlib Path.home, the patched lookup when a test replaced it
func UserHome() (string, error) {
	if f := userHomeLookup.Load(); f != nil {
		return (*f)()
	}
	return pathutil.Home()
}

// Serves dir as the user home like a monkeypatched Path.home and returns the restore
func PatchUserHome(dir string) (restore func()) {
	lookup := func() (string, error) { return dir, nil }
	prev := userHomeLookup.Swap(&lookup)
	return func() { userHomeLookup.Store(prev) }
}

// Directory standing in for the parent of the native platform default home, nil while the real one serves
var platformDefaultParent atomic.Pointer[string]

// Serves the platform default home under dir keeping its name, as test isolation does, returning the restore
func PatchPlatformDefault(dir string) (restore func()) {
	prev := platformDefaultParent.Swap(&dir)
	return func() { platformDefaultParent.Store(prev) }
}

// Platform default home of the process, the Path.home RuntimeError when no user home is known
func platformDefaultHome() (string, error) {
	native, err := platformDefault(os.Getenv, UserHome)
	if parent := platformDefaultParent.Load(); parent != nil && err == nil && userHomeLookup.Load() == nil {
		return filepath.Join(*parent, filepath.Base(native)), nil
	}
	return native, err
}

// Platform default home of the running process, panicking with the RuntimeError Python raises without a user home
func PlatformDefault() string {
	p, err := platformDefaultHome()
	if err != nil {
		panic(err)
	}
	return p
}

// Expanded home variable of the process, empty when it is blank
func envHomePath() string {
	if v := strings.TrimSpace(os.Getenv(EnvHome)); v != "" {
		return expand(v)
	}
	return ""
}

// Home directory of the process, the Path.home RuntimeError when it falls back to an unknown user home
func processHome() (string, error) {
	if p := envHomePath(); p != "" {
		return p, nil
	}
	return platformDefaultHome()
}

// Home directory of the process from the environment variable or the platform default
func Process() string {
	if p := envHomePath(); p != "" {
		return p
	}
	return PlatformDefault()
}

// Home directory in effect, a context override winning over the process home
func Dir(ctx context.Context) string {
	if o := Override(ctx); o != "" {
		return expand(o)
	}
	if strings.TrimSpace(os.Getenv(EnvHome)) == "" {
		warnProfileFallbackOnce()
	}
	return Process()
}

// OS account home from the password database, the real home resolver where it cannot answer
func accountHome(ctx context.Context) string {
	if runtime.GOOS != "windows" {
		if u, err := homeguard.LookupID(strconv.Itoa(os.Getuid())); err == nil {
			return filepath.Clean(u.HomeDir)
		}
	}
	return filepath.Clean(RealHome(ctx, nil))
}

// Default home of the OS account, the process platform default when the user home resolves to the account home
func AccountDefault(ctx context.Context) string {
	account := accountHome(ctx)
	if sameResolvedPath(UserHome, account) {
		return PlatformDefault()
	}
	p, _ := platformDefault(os.Getenv, func() (string, error) { return account, nil })
	return p
}

// pathlib equality of two host paths, case insensitive on Windows
func pathEqual(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.ToLower(a) == strings.ToLower(b)
	}
	return a == b
}

// Reports whether the looked up home and path resolve to equal pathlib paths, false when either fails
func sameResolvedPath(lookup func() (string, error), path string) bool {
	h, err := lookup()
	if err != nil {
		return false
	}
	a, errA := resolve(h)
	b, errB := resolve(path)
	return errA == nil && errB == nil && pathEqual(a, b)
}

// Root that holds home, the platform default when home lives under it, the parent of a profiles directory, or home itself
func rootOf(native, home string) string {
	if home == "" {
		return native
	}
	rh, errH := resolve(home)
	rn, errN := resolve(native)
	if errH == nil && errN == nil {
		if _, under := pathutil.RelativeTo(rh, rn); under {
			return native
		}
	}
	if parent := filepath.Dir(filepath.Clean(home)); pathutil.Name(parent) == ProfilesDir {
		return filepath.Dir(filepath.Clean(parent))
	}
	return home
}

// Root home of the process, the Path.home RuntimeError when the platform default has no user home
func rootHome() (string, error) {
	native, err := platformDefaultHome()
	if err != nil {
		return "", err
	}
	return rootOf(native, envHomePath()), nil
}

// Root home that owns the profile tree, derived from the process home
func Root() string {
	r, err := rootHome()
	if err != nil {
		panic(err)
	}
	return r
}

// Root home that owns the given home
func RootOf(home string) string {
	native := PlatformDefault()
	if h := strings.TrimSpace(home); h != "" {
		return rootOf(native, expand(h))
	}
	return native
}

// Reports whether a pathlib is_dir check passes
func pathIsDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Reports whether dir is provably the profiles directory of a home
func isProfilesRoot(dir string) bool {
	root := filepath.Dir(filepath.Clean(dir))
	if pathutil.Name(root) == dirName {
		return true
	}
	if pathIsDir(pathutil.Join(dir, deletedDir)) {
		return true
	}
	for _, m := range homeMarkers {
		if pathExists(pathutil.Join(root, m)) {
			return true
		}
	}
	a, errA := resolve(root)
	b, errB := resolve(Root())
	return errA == nil && errB == nil && pathEqual(a, b)
}

// Profile home that path is or lies under, empty when path is not a named profile
func NamedProfileHome(path string) string {
	current := filepath.Clean(path)
	for _, c := range append([]string{current}, pathutil.Parents(current)...) {
		parent, name := filepath.Dir(filepath.Clean(c)), pathutil.Name(c)
		if pathutil.Name(parent) == ProfilesDir && !strings.HasPrefix(name, ".") && isProfilesRoot(parent) {
			return c
		}
		if name == dirName {
			return ""
		}
	}
	return ""
}

// Profile id owning path, default for the root home, empty when path is no home
func ProfileNameFor(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	current, err := pathutil.ExpandUserPath(path)
	if err != nil {
		panic(err)
	}
	root, err := rootHome()
	if err != nil {
		return ""
	}
	resolved, err := resolve(current)
	if err != nil {
		return ""
	}
	for _, c := range []string{current, resolved} {
		if pathEqual(c, root) {
			return "default"
		}
		if rr, err := resolve(root); err != nil {
			return ""
		} else if pathEqual(c, rr) {
			return "default"
		}
		if named := NamedProfileHome(c); named != "" {
			return pathutil.Name(named)
		}
		if name := pathutil.Name(c); pathutil.Name(filepath.Dir(filepath.Clean(c))) == ProfilesDir && !strings.HasPrefix(name, ".") {
			return name
		}
	}
	return ""
}

// pathlib Path.resolve of path, the ValueError of a null byte or an unencodable component returned
func resolve(path string) (string, error) { return pathutil.Resolve(path) }

// Home directory of the named profile under the root, the root for default
func ProfileHome(root, name string) string {
	if name == "" || name == "default" {
		return root
	}
	return filepath.Join(root, ProfilesDir, name)
}

// Tombstone file marking a profile as deleted
func TombstonePath(profileHome string) string {
	return pathutil.Join(filepath.Dir(filepath.Clean(profileHome)), deletedDir, pathutil.Name(profileHome))
}

// Reports whether the profile was deleted
func ProfileDeleted(profileHome string) bool {
	return pathExists(TombstonePath(profileHome))
}

// Reports whether a profile directory carries a file that identifies it as a profile
func ProfileHasIdentity(profileHome string) bool {
	for _, m := range identityMarkers {
		p := pathutil.Join(profileHome, m)
		if st, err := os.Lstat(p); err == nil && (st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0) {
			return true
		}
	}
	return false
}

// Reports whether the profile exists, has identity and is not deleted
func ProfileLive(profileHome string) bool {
	return pathIsDir(profileHome) && ProfileHasIdentity(profileHome) && !ProfileDeleted(profileHome)
}

// Writes the tombstone of a profile, its directory made like pathlib mkdir with parents
func MarkProfileDeleted(profileHome string) error {
	t := TombstonePath(profileHome)
	if err := pathMkdir(filepath.Dir(filepath.Clean(t)), 0o777, true, true); err != nil {
		return err
	}
	text := "deleted\n"
	if runtime.GOOS == "windows" {
		text = "deleted\r\n"
	}
	return os.WriteFile(t, []byte(text), 0o666)
}

// Unlinks the tombstone of a profile, a missing one ignored
func ClearProfileDeleted(profileHome string) error {
	err := os.Remove(TombstonePath(profileHome))
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Error of a path under a named profile that was deleted or never created
type ErrProfileMissing struct {
	Home string
}

func (e *ErrProfileMissing) Error() string {
	return fmt.Sprintf("Named profile home does not exist: %s. Create the profile explicitly before using it.", e.Home)
}

// Matches fs.ErrNotExist like the FileNotFoundError Python raises
func (e *ErrProfileMissing) Is(target error) bool { return target == fs.ErrNotExist }

// Homes whose data this process erased, so no later step of the same process sets them up again
var (
	erasedHomesMu sync.Mutex
	erasedHomes   []string
)

// Records that this process erased the home h, refusing every later directory it would create under h
func MarkHomeErased(h string) {
	erasedHomesMu.Lock()
	defer erasedHomesMu.Unlock()
	erasedHomes = append(erasedHomes, filepath.Clean(h))
}

// Home this process erased that path is or lies under, empty when none
func erasedHomeOf(path string) string {
	clean := filepath.Clean(path)
	erasedHomesMu.Lock()
	defer erasedHomesMu.Unlock()
	for _, h := range erasedHomes {
		if clean == h || strings.HasPrefix(clean, h+string(filepath.Separator)) {
			return h
		}
	}
	return ""
}

// Error of a path under a home this process erased
type ErrHomeErased struct {
	Home string
}

func (e *ErrHomeErased) Error() string {
	return fmt.Sprintf("Icebath home %s was erased by this process and is not set up again.", e.Home)
}

// Matches fs.ErrNotExist, the home data no longer exists
func (e *ErrHomeErased) Is(target error) bool { return target == fs.ErrNotExist }

// Fails when path lies under a home this process erased or under a deleted or missing named profile
func AssertProfileLive(path string) error {
	if erased := erasedHomeOf(path); erased != "" {
		return &ErrHomeErased{Home: erased}
	}
	h := NamedProfileHome(path)
	if h != "" && (ProfileDeleted(h) || !pathExists(h)) {
		return &ErrProfileMissing{Home: h}
	}
	return nil
}

// Creates path and its parents through MakeUserDir, refusing to materialize a dead profile
func Mkdir(ctx context.Context, path string) (string, error) {
	if err := AssertProfileLive(path); err != nil {
		return "", err
	}
	return MakeUserDir(ctx, path, 0o777, true, true)
}

// Names of every live named profile under root, sorted
func ListProfiles(root string) []string {
	entries, err := os.ReadDir(filepath.Join(root, ProfilesDir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || !ProfileIDRe.MatchString(e.Name()) {
			continue
		}
		if ProfileLive(filepath.Join(root, ProfilesDir, e.Name())) {
			out = append(out, e.Name())
		}
	}
	return out
}

// Path relative to the user home as a tilde posix path, the path itself when outside the user home
func Display(path string) string {
	p := filepath.Clean(path)
	h, err := UserHome()
	if err != nil {
		panic(err)
	}
	rel, ok := pathutil.RelativeTo(p, h)
	if !ok {
		return p
	}
	return "~/" + rel
}

// Registry write scope key, empty staying empty for the process global layer and anything else keyed like Key
func NormalizeScope(scope string) string {
	if scope == "" {
		return ""
	}
	return Key(scope)
}

var (
	keyMu    sync.Mutex
	keyCache = map[string]string{}
)

// Stable key of a home directory, the resolved path, cached once the directory exists
func Key(path string) string {
	raw := filepath.Clean(path)
	keyMu.Lock()
	defer keyMu.Unlock()
	if k, ok := keyCache[raw]; ok {
		return k
	}
	expanded, err := pathutil.ExpandUserPath(raw)
	if err != nil {
		panic(err)
	}
	resolved, err := strictResolve(expanded)
	if err != nil {
		lenient, rerr := resolve(expanded)
		if rerr != nil {
			panic(rerr)
		}
		return pathutil.FoldCase(lenient)
	}
	k := pathutil.FoldCase(resolved)
	keyCache[raw] = k
	return k
}

// Strict realpath a key must pass before it is remembered, swapped by tests counting filesystem resolves
var strictResolve = func(p string) (string, error) { return pathutil.ResolveExisting(p) }

// Forgets every cached home key
func ResetKeyCache() {
	keyMu.Lock()
	keyCache = map[string]string{}
	keyMu.Unlock()
}
