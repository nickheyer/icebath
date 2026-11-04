// Resolves the icebath home directory, its named profiles and the files inside them
package home

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// Environment variable naming the home directory of the running process
const EnvHome = "ICEBATH_HOME"

// Environment variable appending a suffix to the platform default directory name
const EnvSuffix = "ICEBATH_DATA_DIR_SUFFIX"

// Directory name under the user home on POSIX systems
const dirName = ".icebath"

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
	path = os.ExpandEnv(path)
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if u, err := os.UserHomeDir(); err == nil {
			path = u + path[1:]
		}
	}
	return filepath.Clean(path)
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
			base = filepath.Join(h, "AppData", "Local")
		}
		return filepath.Join(base, "icebath"+suffix), nil
	}
	h, err := userHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, dirName+suffix), nil
}

// Platform default home of the running process
func PlatformDefault() string {
	p, err := platformDefault(os.Getenv, os.UserHomeDir)
	if err != nil {
		return filepath.Join(".", dirName)
	}
	return p
}

// Home directory of the process from the environment variable or the platform default
func Process() string {
	if v := strings.TrimSpace(os.Getenv(EnvHome)); v != "" {
		return expand(v)
	}
	return PlatformDefault()
}

// Home directory in effect, a context override winning over the process home
func Dir(ctx context.Context) string {
	if o := Override(ctx); o != "" {
		return expand(o)
	}
	return Process()
}

// Default home of the OS account rather than of a process whose HOME points elsewhere
func AccountDefault() string {
	if runtime.GOOS != "windows" {
		if u, err := user.Current(); err == nil && u.HomeDir != "" {
			if p, err := platformDefault(os.Getenv, func() (string, error) { return u.HomeDir, nil }); err == nil {
				return p
			}
		}
	}
	return PlatformDefault()
}

// Root that holds home, the platform default when home lives under it, the parent of a profiles directory, or home itself
func rootOf(native, home string) string {
	if home == "" {
		return native
	}
	rel, err := filepath.Rel(native, home)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return native
	}
	if filepath.Base(filepath.Dir(home)) == ProfilesDir {
		return filepath.Dir(filepath.Dir(home))
	}
	return home
}

// Root home that owns the profile tree, derived from the process home
func Root() string {
	return rootOf(PlatformDefault(), Process())
}

// Root home that owns the given home
func RootOf(home string) string {
	return rootOf(PlatformDefault(), expand(home))
}

// Reports whether dir is provably the profiles directory of a home
func isProfilesRoot(dir string) bool {
	root := filepath.Dir(dir)
	if filepath.Base(root) == dirName {
		return true
	}
	if st, err := os.Stat(filepath.Join(dir, deletedDir)); err == nil && st.IsDir() {
		return true
	}
	for _, m := range homeMarkers {
		if _, err := os.Stat(filepath.Join(root, m)); err == nil {
			return true
		}
	}
	a, err1 := filepath.Abs(root)
	b, err2 := filepath.Abs(Root())
	return err1 == nil && err2 == nil && a == b
}

// Profile home that path is or lies under, empty when path is not a named profile
func NamedProfileHome(path string) string {
	cur := filepath.Clean(path)
	for {
		parent := filepath.Dir(cur)
		if filepath.Base(parent) == ProfilesDir && !strings.HasPrefix(filepath.Base(cur), ".") && isProfilesRoot(parent) {
			return cur
		}
		if filepath.Base(cur) == dirName {
			return ""
		}
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// Profile id owning path, default for the root home, empty when path is no home
func ProfileNameFor(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	cur := expand(path)
	root := Root()
	for _, c := range []string{cur, resolve(cur)} {
		if c == root || c == resolve(root) {
			return "default"
		}
		if named := NamedProfileHome(c); named != "" {
			return filepath.Base(named)
		}
		if filepath.Base(filepath.Dir(c)) == ProfilesDir && !strings.HasPrefix(filepath.Base(c), ".") {
			return filepath.Base(c)
		}
	}
	return ""
}

// Path with every symlink resolved, the cleaned path when resolution fails
func resolve(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		if a, err := filepath.Abs(r); err == nil {
			return a
		}
		return r
	}
	if a, err := filepath.Abs(path); err == nil {
		return a
	}
	return filepath.Clean(path)
}

// Home directory of the named profile under the root, the root for default
func ProfileHome(root, name string) string {
	if name == "" || name == "default" {
		return root
	}
	return filepath.Join(root, ProfilesDir, name)
}

// Tombstone file marking a profile as deleted
func TombstonePath(profileHome string) string {
	return filepath.Join(filepath.Dir(profileHome), deletedDir, filepath.Base(profileHome))
}

// Reports whether the profile was deleted
func ProfileDeleted(profileHome string) bool {
	_, err := os.Stat(TombstonePath(profileHome))
	return err == nil
}

// Reports whether a profile directory carries a file that identifies it as a profile
func ProfileHasIdentity(profileHome string) bool {
	for _, m := range identityMarkers {
		p := filepath.Join(profileHome, m)
		if st, err := os.Lstat(p); err == nil && (st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0) {
			return true
		}
	}
	return false
}

// Reports whether the profile exists, has identity and is not deleted
func ProfileLive(profileHome string) bool {
	st, err := os.Stat(profileHome)
	return err == nil && st.IsDir() && ProfileHasIdentity(profileHome) && !ProfileDeleted(profileHome)
}

// Writes the tombstone of a profile
func MarkProfileDeleted(profileHome string) error {
	t := TombstonePath(profileHome)
	if err := os.MkdirAll(filepath.Dir(t), 0o700); err != nil {
		return err
	}
	return os.WriteFile(t, []byte("deleted\n"), 0o600)
}

// Removes the tombstone of a profile
func ClearProfileDeleted(profileHome string) error {
	err := os.Remove(TombstonePath(profileHome))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Error of a path under a named profile that was deleted or never created
type ErrProfileMissing struct {
	Home string
}

func (e *ErrProfileMissing) Error() string {
	return fmt.Sprintf("named profile home does not exist: %s, create the profile explicitly before using it", e.Home)
}

// Fails when path lies under a deleted or missing named profile
func AssertProfileLive(path string) error {
	h := NamedProfileHome(path)
	if h == "" {
		return nil
	}
	if ProfileDeleted(h) {
		return &ErrProfileMissing{Home: h}
	}
	if _, err := os.Stat(h); err != nil {
		return &ErrProfileMissing{Home: h}
	}
	return nil
}

// Creates path and its parents with owner only permissions, refusing to materialize a dead profile
func Mkdir(path string) (string, error) {
	if err := AssertProfileLive(path); err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	return path, nil
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

// Path with the user home shortened to a tilde, for text shown to people
func Display(path string) string {
	u, err := os.UserHomeDir()
	if err != nil || u == "" {
		return path
	}
	if path == u {
		return "~"
	}
	if strings.HasPrefix(path, u+string(filepath.Separator)) {
		return "~" + path[len(u):]
	}
	return path
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
	expanded := expand(raw)
	if _, err := os.Stat(expanded); err != nil {
		return normcase(resolve(expanded))
	}
	k := normcase(resolve(expanded))
	keyCache[raw] = k
	return k
}

// Lower cases a path on case insensitive platforms
func normcase(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// Forgets every cached home key
func ResetKeyCache() {
	keyMu.Lock()
	keyCache = map[string]string{}
	keyMu.Unlock()
}
