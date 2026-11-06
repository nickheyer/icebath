package profiles

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Directories bootstrapped inside every new profile
var ProfileDirs = []string{"memories", "sessions", "skills", "skins", "logs", "plans", "workspace", "cron", "home"}

// Files copied by a config clone when the source has them
var CloneConfigFiles = []string{"config.yaml", ".env", "SOUL.md"}

// Memory files copied by a config clone
var CloneSubdirFiles = []string{"memories/MEMORY.md", "memories/USER.md"}

// Runtime files stripped after a full clone so the copy never attaches to the source processes
var CloneAllStrip = []string{
	"gateway.pid", "gateway_state.json", "processes.json",
	"bot-desktop/launcher.pid", "bot-desktop/env", "bot-desktop/rfb.sock",
	"bot-desktop/lease.json", "bot-desktop/lease.lock",
	"bot-desktop/browser-profile/DevToolsActivePort",
	"bot-desktop/browser-profile/SingletonLock",
	"bot-desktop/browser-profile/SingletonCookie",
	"bot-desktop/browser-profile/SingletonSocket",
}

// Root entries of the default home never copied by a full clone
var CloneAllDefaultExcludeRoot = map[string]bool{
	"icebath-agent": true, ".worktrees": true, "profiles": true, "bin": true, "node_modules": true,
	".icebath-runtime": true, "node": true, "models": true, "runtimes": true,
}

// Per profile history never copied by a full clone from any source
var CloneAllHistoryExcludeRoot = map[string]bool{
	"state.db": true, "state.db-wal": true, "state.db-shm": true, "sessions": true, "backups": true,
	"state-snapshots": true, "checkpoints": true, "cron": true, "webapp": true,
}

// Marker file at a profile root opting out of bundled skill seeding
const NoBundledSkillsMarker = ".no-bundled-skills"

// Header seeded into the empty env file of a new profile
const PlaceholderEnv = "# Per-profile secrets for this icebath profile.\n" +
	"# API keys and tokens set here override the shell environment.\n" +
	"# Behavioral settings belong in config.yaml, not here.\n"

// Names that collide with the installation itself or a common system binary
var ReservedNames = map[string]bool{"icebath": true, "default": true, "test": true, "tmp": true, "root": true, "sudo": true}

// Top level icebath subcommands an alias must not shadow
var CLISubcommands = map[string]bool{
	"chat": true, "model": true, "gateway": true, "setup": true, "whatsapp": true, "login": true, "logout": true,
	"status": true, "cron": true, "doctor": true, "dump": true, "config": true, "pairing": true, "skills": true, "tools": true,
	"mcp": true, "connectors": true, "sessions": true, "insights": true, "version": true, "update": true, "uninstall": true,
	"profile": true, "plugins": true, "honcho": true, "acp": true,
}

const profileNameRule = "Use lowercase letters, numbers, '-' or '_', starting with a letter or number, up to 64 characters"

var suggestRe = regexp.MustCompile(`[^a-z0-9_-]+`)

// Invalid input, the Python ValueError of the profile helpers
type ValueError struct{ Msg string }

func (e *ValueError) Error() string { return e.Msg }

// Missing profile, matching fs.ErrNotExist like Python FileNotFoundError
type NotFoundError struct{ Msg string }

func (e *NotFoundError) Error() string        { return e.Msg }
func (e *NotFoundError) Is(target error) bool { return target == fs.ErrNotExist }

// Profile that already exists, matching fs.ErrExist like Python FileExistsError
type ExistsError struct{ Msg string }

func (e *ExistsError) Error() string        { return e.Msg }
func (e *ExistsError) Is(target error) bool { return target == fs.ErrExist }

// Root home owning the profile tree, the configured home itself in custom deployments
func DefaultHome() string { return home.Root() }

// Directory holding every named profile under the root home
func ProfilesRoot() string { return filepath.Join(DefaultHome(), home.ProfilesDir) }

// File recording the sticky active profile of the root home
func ActiveProfilePath() string { return filepath.Join(DefaultHome(), "active_profile") }

// Error for a profile a command needs but that was never created
func MissingProfileError(canon string) error {
	return &NotFoundError{fmt.Sprintf("Profile '%s' does not exist. Create it with: icebath profile create %s", canon, canon)}
}

// Error for a delete, rename or export naming no profile, likely a typo
func UnknownProfileError(canon string) error {
	return &NotFoundError{fmt.Sprintf("No profile named '%s'. See your profiles with: icebath profile list", canon)}
}

// Error for creating a profile whose name is taken
func ProfileExistsError(canon string) error {
	return &ExistsError{fmt.Sprintf("A profile named '%s' already exists. Switch to it with `icebath profile use %s`, "+
		"see all profiles with `icebath profile list`, or choose a different name.", canon, canon)}
}

// Valid profile id derived from a typed name, my-work when nothing usable remains
func SuggestProfileName(name string) string {
	candidate := strings.Trim(suggestRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-"), "-_")
	if r := []rune(candidate); len(r) > 64 {
		candidate = string(r[:64])
	}
	if home.ProfileIDRe.MatchString(candidate) {
		return candidate
	}
	return "my-work"
}

// Error for a name that fails the profile id grammar, with a suggested valid name
func InvalidProfileNameError(name string) error {
	s := SuggestProfileName(name)
	return &ValueError{fmt.Sprintf("%s is not a valid profile name. %s (for example: %s). Then run `icebath profile create %s`.",
		config.PyRepr(name), profileNameRule, s, s)}
}

// Canonical profile id, lowercase with default matched case insensitively
func NormalizeProfileName(name string) (string, error) {
	stripped := strings.TrimSpace(name)
	if stripped == "" {
		return "", &ValueError{"profile name cannot be empty"}
	}
	if casefold(stripped) == "default" {
		return "default", nil
	}
	return strings.ToLower(stripped), nil
}

// Canonical profile id of an untyped value, refusing anything but a string so numeric ids never become directories
func NormalizeProfileNameValue(name any) (string, error) {
	s, ok := name.(string)
	if !ok {
		return "", &ValueError{fmt.Sprintf("profile name must be a string, got %s: %s", config.PyTypeName(name), config.PyRepr(name))}
	}
	return NormalizeProfileName(s)
}

// Error unless name is a lowercase profile id outside the reserved names, default passing
func ValidateProfileName(name string) error {
	if name == "default" {
		return nil
	}
	if !home.ProfileIDRe.MatchString(name) {
		return InvalidProfileNameError(name)
	}
	if ReservedNames[name] {
		return &ValueError{fmt.Sprintf("Profile name %s is reserved — it collides with either "+
			"the icebath installation itself or a common system binary.  Pick a different name.", config.PyRepr(name))}
	}
	return nil
}

// Error unless name is safe as a wrapper file name under the wrapper directory
func ValidateAliasName(name string) error {
	if !home.ProfileIDRe.MatchString(name) {
		return &ValueError{fmt.Sprintf("Invalid alias name %s. %s.", config.PyRepr(name), profileNameRule)}
	}
	return nil
}

// Canonical id of name after normalizing and validating it
func CanonValid(name string) (string, error) {
	canon, err := NormalizeProfileName(name)
	if err != nil {
		return "", err
	}
	if err := ValidateProfileName(canon); err != nil {
		return "", err
	}
	return canon, nil
}

// Canonical id and directory of an existing profile, a not found error otherwise
func ExistingProfileDir(name string) (string, string, error) {
	canon, err := CanonValid(name)
	if err != nil {
		return "", "", err
	}
	dir, err := GetProfileDir(canon)
	if err != nil {
		return "", "", err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", "", UnknownProfileError(canon)
	}
	return canon, dir, nil
}

// Home directory of a profile name, the root home for default
func GetProfileDir(name string) (string, error) {
	canon, err := NormalizeProfileName(name)
	if err != nil {
		return "", err
	}
	if canon == "default" {
		return DefaultHome(), nil
	}
	if !home.ProfileIDRe.MatchString(canon) {
		return "", InvalidProfileNameError(canon)
	}
	return filepath.Join(ProfilesRoot(), canon), nil
}

// Reports whether a live profile exists under name, default always existing
func ProfileExists(name string) bool {
	canon, err := NormalizeProfileName(name)
	if err != nil {
		return false
	}
	dir, err := GetProfileDir(canon)
	if err != nil {
		return false
	}
	if canon == "default" {
		return true
	}
	return home.ProfileLive(dir)
}

// Reports whether name refers to the profile served from homeDir, the current home when empty
func ProfileMatchesHome(ctx context.Context, name, homeDir string) bool {
	target, err := GetProfileDir(name)
	if err != nil {
		return false
	}
	if homeDir == "" {
		homeDir = home.Dir(ctx)
	}
	target, err = pypath.PathExpanduser(target)
	if err != nil {
		return false
	}
	homeDir, err = pypath.PathExpanduser(homeDir)
	if err != nil {
		return false
	}
	return resolveLoose(target) == resolveLoose(homeDir)
}

// Sorted directories of named profiles carrying identity, skipping tombstoned ones when liveOnly
func IterNamedProfileDirs(liveOnly bool) ([]string, error) {
	root := ProfilesRoot()
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			continue
		}
		if e.Name() == "default" || !home.ProfileIDRe.MatchString(e.Name()) || !home.ProfileHasIdentity(p) {
			continue
		}
		if liveOnly && home.ProfileDeleted(p) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// Default plus every live named profile without reading any profile config
func ListProfileNames() []string {
	names := []string{"default"}
	dirs, err := IterNamedProfileDirs(true)
	if err != nil {
		return names
	}
	for _, d := range dirs {
		names = append(names, filepath.Base(d))
	}
	return names
}

// Absolute path with symlinks of its existing prefix resolved and the missing tail kept, like Path.resolve(strict=False)
func resolveLoose(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	cur, tail := abs, ""
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			if tail == "" {
				return r
			}
			return filepath.Join(r, tail)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return abs
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		if tail == "" {
			tail = filepath.Base(cur)
		} else {
			tail = filepath.Join(filepath.Base(cur), tail)
		}
		cur = parent
	}
}
