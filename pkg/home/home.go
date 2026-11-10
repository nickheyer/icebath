// Resolves the icebath home directory, its named profiles and the files inside them
package home

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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
	return pypath.PathStr(pypath.Expanduser(pypath.Expandvars(path)))
}

// Platform default home with the data directory suffix applied
func platformDefault(environ func(string) string, userHome func() (string, error)) (string, error) {
	suffix := environ(EnvSuffix)
	if runtime.GOOS == "windows" {
		base := pystr.Strip(environ("LOCALAPPDATA"))
		if base == "" {
			h, err := userHome()
			if err != nil {
				return "", err
			}
			base = pypath.PathJoin(h, "AppData", "Local")
		}
		return pypath.PathJoin(base, "icebath"+suffix), nil
	}
	h, err := userHome()
	if err != nil {
		return "", err
	}
	return pypath.PathJoin(h, dirName+suffix), nil
}

// Patched pathlib Path.home lookup, nil while the real lookup serves
var userHomeLookup atomic.Pointer[func() (string, error)]

// User home like pathlib Path.home, the patched lookup when a test replaced it
func UserHome() (string, error) {
	if f := userHomeLookup.Load(); f != nil {
		return (*f)()
	}
	return pypath.PathHome()
}

// Serves dir as the user home like a monkeypatched Path.home and returns the restore
func PatchUserHome(dir string) (restore func()) {
	lookup := func() (string, error) { return dir, nil }
	prev := userHomeLookup.Swap(&lookup)
	return func() { userHomeLookup.Store(prev) }
}

// Platform default home of the process, the Path.home RuntimeError when no user home is known
func platformDefaultHome() (string, error) { return platformDefault(os.Getenv, UserHome) }

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
	if v := pystr.Strip(os.Getenv(EnvHome)); v != "" {
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
	if pystr.Strip(os.Getenv(EnvHome)) == "" {
		warnProfileFallbackOnce()
	}
	return Process()
}

// OS account home from the password database, the real home resolver where it cannot answer
func accountHome() string {
	if runtime.GOOS != "windows" {
		if u, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil {
			return pypath.PathStr(u.HomeDir)
		}
	}
	return pypath.PathStr(RealHome(context.Background(), nil))
}

// Default home of the OS account, the process platform default when the user home resolves to the account home
func AccountDefault() string {
	account := accountHome()
	if sameResolvedPath(UserHome, account) {
		return PlatformDefault()
	}
	p, _ := platformDefault(os.Getenv, func() (string, error) { return account, nil })
	return p
}

// pathlib equality of two host paths, case insensitive on Windows
func pathEqual(a, b string) bool {
	a, b = pypath.PathStr(a), pypath.PathStr(b)
	if runtime.GOOS == "windows" {
		return pystr.Lower(a) == pystr.Lower(b)
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
		if _, under := pypath.PathRelativeTo(rh, rn); under {
			return native
		}
	}
	if parent := pypath.PathParent(home); pypath.PathName(parent) == ProfilesDir {
		return pypath.PathParent(parent)
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
	if h := pystr.Strip(home); h != "" {
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
	root := pypath.PathParent(dir)
	if pypath.PathName(root) == dirName {
		return true
	}
	if pathIsDir(pypath.PathJoin(dir, deletedDir)) {
		return true
	}
	for _, m := range homeMarkers {
		if pathExists(pypath.PathJoin(root, m)) {
			return true
		}
	}
	a, errA := resolve(root)
	b, errB := resolve(Root())
	return errA == nil && errB == nil && pathEqual(a, b)
}

// Profile home that path is or lies under, empty when path is not a named profile
func NamedProfileHome(path string) string {
	current := pypath.PathStr(path)
	for _, c := range append([]string{current}, pypath.PathParents(current)...) {
		parent, name := pypath.PathParent(c), pypath.PathName(c)
		if pypath.PathName(parent) == ProfilesDir && !strings.HasPrefix(name, ".") && isProfilesRoot(parent) {
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
	if pystr.Strip(path) == "" {
		return ""
	}
	current, err := pypath.PathExpanduser(path)
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
			return pypath.PathName(named)
		}
		if name := pypath.PathName(c); pypath.PathName(pypath.PathParent(c)) == ProfilesDir && !strings.HasPrefix(name, ".") {
			return name
		}
	}
	return ""
}

// pathlib Path.resolve of path, the ValueError of a null byte or an unencodable component returned
func resolve(path string) (string, error) { return pypath.Resolve(path) }

// Home directory of the named profile under the root, the root for default
func ProfileHome(root, name string) string {
	if name == "" || name == "default" {
		return root
	}
	return filepath.Join(root, ProfilesDir, name)
}

// Tombstone file marking a profile as deleted
func TombstonePath(profileHome string) string {
	return pypath.PathJoin(pypath.PathParent(profileHome), deletedDir, pypath.PathName(profileHome))
}

// Reports whether the profile was deleted
func ProfileDeleted(profileHome string) bool {
	return pathExists(TombstonePath(profileHome))
}

// Reports whether a profile directory carries a file that identifies it as a profile
func ProfileHasIdentity(profileHome string) bool {
	for _, m := range identityMarkers {
		p := pypath.PathJoin(profileHome, m)
		if st, err := os.Lstat(p); err == nil && (st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0) {
			return true
		}
	}
	return false
}

// Reports identity a host changes posture for, where an empty regular .env alone does not count
func ProfileHasServableIdentity(profileHome string) bool {
	for _, m := range identityMarkers {
		p := pypath.PathJoin(profileHome, m)
		if lst, err := os.Lstat(p); err == nil && lst.Mode()&os.ModeSymlink != 0 {
			return true
		}
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if st.Mode().IsRegular() && (m != ".env" || st.Size() > 0) {
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
	if err := pathMkdir(pypath.PathParent(t), 0o777, true, true); err != nil {
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
	err := pypath.Unlink(TombstonePath(profileHome))
	if err != nil && pypath.OSErrorClass(err) == "FileNotFoundError" {
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

// Fails when path lies under a deleted or missing named profile
func AssertProfileLive(path string) error {
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
	p := pypath.PathStr(path)
	h, err := UserHome()
	if err != nil {
		panic(err)
	}
	rel, ok := pypath.PathRelativeTo(p, h)
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
	raw := pypath.PathStr(path)
	keyMu.Lock()
	defer keyMu.Unlock()
	if k, ok := keyCache[raw]; ok {
		return k
	}
	expanded, err := pypath.PathExpanduser(raw)
	if err != nil {
		panic(err)
	}
	resolved, err := strictResolve(expanded)
	if err != nil {
		lenient, rerr := resolve(expanded)
		if rerr != nil {
			panic(rerr)
		}
		return normcase(lenient)
	}
	k := normcase(resolved)
	keyCache[raw] = k
	return k
}

// Strict realpath a key must pass before it is remembered, swapped by tests counting filesystem resolves
var strictResolve = func(p string) (string, error) { return pypath.Realpath(p, pypath.StrictAll) }

// Lower cases a path on case insensitive platforms
func normcase(p string) string { return pypath.Normcase(p) }

// Forgets every cached home key
func ResetKeyCache() {
	keyMu.Lock()
	keyCache = map[string]string{}
	keyMu.Unlock()
}
