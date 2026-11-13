package home

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
)

// Directory beside the profile directories holding deletion tombstones
const deletedDir = ".deleted"

// Name of the profile the root home itself serves
const DefaultProfile = "default"

// Canonical named profile id grammar every profile directory gate shares
var profileID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Files that mark a directory as a real home, a stray directory with a profiles segment carrying none
var homeMarkers = [...]Entry{Config, EnvFile, StateDB}

// Files that identify a directory under profiles as a profile rather than a runtime side effect
var identityMarkers = [...]Entry{Config, EnvFile, Soul, ProfileManifest, AuthFile, StateDB}

// Reports whether name fits the named profile id grammar
func ValidProfileID(name string) bool { return profileID.MatchString(name) }

// Files whose presence marks a profile directory as a profile
func ProfileIdentityMarkers() []Entry { return slices.Clone(identityMarkers[:]) }

// Home of the profile name under root, the root itself for the default profile
func ProfileHome(root, name string) string {
	if name == "" || name == DefaultProfile {
		return root
	}
	return filepath.Join(root, profilesDir, name)
}

// Reports whether something is at p, following symlinks
func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Reports whether p is a directory, following symlinks
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// Reports whether dir is provably the profiles directory of a home
func isProfilesRoot(dir string) (bool, error) {
	root := filepath.Dir(dir)
	if filepath.Base(root) == ".icebath" {
		return true, nil
	}
	if isDir(filepath.Join(dir, deletedDir)) {
		return true, nil
	}
	for _, m := range homeMarkers {
		if exists(m.In(root)) {
			return true, nil
		}
	}
	def, err := DefaultRoot()
	if errors.Is(err, ErrNoUserHome) {
		return false, err
	}
	if err != nil {
		return false, nil
	}
	realRoot, err := resolvePath(root)
	if err != nil {
		return false, nil
	}
	realDef, err := resolvePath(def)
	return err == nil && samePath(realRoot, realDef), nil
}

// Named profile home that path is or lies under, empty when path belongs to no named profile
func NamedProfileHome(path string) (string, error) {
	for c := filepath.Clean(path); ; {
		parent, name := filepath.Dir(c), filepath.Base(c)
		if filepath.Base(parent) == profilesDir && !strings.HasPrefix(name, ".") {
			ok, err := isProfilesRoot(parent)
			if err != nil {
				return "", err
			}
			if ok {
				return c, nil
			}
		}
		if name == ".icebath" || parent == c {
			return "", nil
		}
		c = parent
	}
}

// Profile id owning path, the default profile for the root, false when path is no profile home
func ProfileName(path string) (string, bool) {
	if strings.TrimSpace(path) == "" {
		return "", false
	}
	current := filepath.Clean(ExpandUser(path))
	root, err := DefaultRoot()
	if err != nil {
		return "", false
	}
	realRoot, err := resolvePath(root)
	if err != nil {
		return "", false
	}
	realCurrent, err := resolvePath(current)
	if err != nil {
		return "", false
	}
	for _, c := range []string{current, realCurrent} {
		if samePath(c, root) || samePath(c, realRoot) {
			return DefaultProfile, true
		}
		named, err := NamedProfileHome(c)
		if err != nil {
			return "", false
		}
		if named != "" {
			return filepath.Base(named), true
		}
		if name := filepath.Base(c); filepath.Base(filepath.Dir(c)) == profilesDir && !strings.HasPrefix(name, ".") {
			return name, true
		}
	}
	return "", false
}

// Selector with a trailing space pinning copied commands to the named profile of home, empty otherwise
func ProfileSelector(home string) string {
	if name, ok := ProfileName(home); ok && name != DefaultProfile {
		return "-p " + name + " "
	}
	return ""
}

// Tombstone beside the profile directory that marks it deleted
func TombstonePath(profileHome string) string {
	return filepath.Join(filepath.Dir(profileHome), deletedDir, filepath.Base(profileHome))
}

// Reports whether the profile carries a tombstone
func ProfileDeleted(profileHome string) bool { return exists(TombstonePath(profileHome)) }

// Reports whether a marker at p is a regular file or any symlink, a dangling link still claiming identity
func markerPresent(p string) bool {
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return true
	}
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// Reports whether a profile directory carries any identity marker
func ProfileHasIdentity(profileHome string) bool {
	for _, m := range identityMarkers {
		if markerPresent(m.In(profileHome)) {
			return true
		}
	}
	return false
}

// Reports identity a host changes posture for, an empty regular env file alone not counting
func ProfileHasServableIdentity(profileHome string) bool {
	for _, m := range identityMarkers {
		p := m.In(profileHome)
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return true
		}
		fi, err := os.Stat(p)
		if err == nil && fi.Mode().IsRegular() && (m != EnvFile || fi.Size() > 0) {
			return true
		}
	}
	return false
}

// Reports whether the profile is an existing directory with identity and no tombstone
func ProfileLive(profileHome string) bool {
	return isDir(profileHome) && ProfileHasIdentity(profileHome) && !ProfileDeleted(profileHome)
}

// Writes the tombstone of a profile, creating the tombstone directory
func MarkProfileDeleted(ctx context.Context, profileHome string) error {
	t := TombstonePath(profileHome)
	if err := makeDir(filepath.Dir(t), 0o777, true); err != nil {
		return err
	}
	return fileutil.WriteFile(ctx, t, []byte("deleted"+lineEnding))
}

// Removes the tombstone of a profile, a missing one ignored
func ClearProfileDeleted(profileHome string) error {
	err := os.Remove(TombstonePath(profileHome))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Error for a path under a named profile that was deleted or never created
type ProfileMissingError struct {
	Home string
}

// Named home and the remedy of creating the profile
func (e *ProfileMissingError) Error() string {
	return fmt.Sprintf("Named profile home does not exist: %s. Create the profile explicitly before using it.", e.Home)
}

// Matches fs.ErrNotExist
func (e *ProfileMissingError) Unwrap() error { return fs.ErrNotExist }

// Refuses a path that lies under a deleted or missing named profile
func AssertProfileLive(path string) error {
	h, err := NamedProfileHome(path)
	if err != nil {
		return err
	}
	if h != "" && (ProfileDeleted(h) || !exists(h)) {
		return &ProfileMissingError{Home: h}
	}
	return nil
}
