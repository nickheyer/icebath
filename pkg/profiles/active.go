package profiles

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
)

// Sticky active profile name of root, the process root when empty, default when unset or unreadable
func GetActiveProfile(root string) string {
	path := ActiveProfilePath()
	if root != "" {
		path = filepath.Join(root, "active_profile")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "default"
	}
	data = bytes.TrimPrefix(data, []byte("\ufeff"))
	if !utf8.Valid(data) {
		return "default"
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return "default"
	}
	return name
}

// Sets the sticky active profile, default removing the file
func SetActiveProfile(name string) error {
	canon, err := CanonValid(name)
	if err != nil {
		return err
	}
	if canon != "default" && !ProfileExists(canon) {
		return MissingProfileError(canon)
	}
	path := ActiveProfilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	if canon == "default" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	tmp := strings.TrimSuffix(path, filepath.Ext(path)) + ".tmp"
	if err := os.WriteFile(tmp, []byte(canon+"\n"), 0o666); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Points the sticky active profile from old to new and prints message, swallowing every failure
func RetargetActiveProfile(old, new, message string) {
	if GetActiveProfile("") != old {
		return
	}
	if SetActiveProfile(new) == nil {
		fmt.Println(message)
	}
}

// Profile name of the current home, default for the root, the id under profiles, custom for any other path
func GetActiveProfileName(ctx context.Context) string {
	resolved := resolveLoose(home.Dir(ctx))
	if resolved == resolveLoose(DefaultHome()) {
		return "default"
	}
	root := resolveLoose(ProfilesRoot())
	if rel, err := filepath.Rel(root, resolved); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) == 1 && home.ProfileIDRe.MatchString(parts[0]) {
			return parts[0]
		}
	}
	return "custom"
}

// Profile the current task runs for, the home override first, then a launcher pinned name, then the process home
func CurrentProfileName(ctx context.Context) string {
	if home.Override(ctx) == "" {
		for _, env := range []string{"ICEBATH_PROFILE_NAME", "ICEBATH_PROFILE"} {
			if v := strings.TrimSpace(os.Getenv(env)); v != "" {
				return v
			}
		}
	}
	return GetActiveProfileName(ctx)
}

// Root named by an exported home, the grandparent of a profile shaped value, defaultRoot when unset
func ProfileRootForEnvHome(envHome, defaultRoot string) string {
	envHome = strings.TrimSpace(envHome)
	if envHome == "" {
		return defaultRoot
	}
	if filepath.Base(filepath.Dir(envHome)) == home.ProfilesDir {
		return filepath.Dir(filepath.Dir(envHome))
	}
	return envHome
}

// Home path of a profile keeping the configured spelling of the root, an error for a missing named profile
func ResolveProfileEnv(profileName string) (string, error) {
	canon, err := CanonValid(profileName)
	if err != nil {
		return "", err
	}
	root := ProfileRootForEnvHome(os.Getenv(home.EnvHome), DefaultHome())
	if canon == "default" {
		return root, nil
	}
	dir := filepath.Join(root, home.ProfilesDir, canon)
	if !home.ProfileLive(dir) {
		return "", MissingProfileError(canon)
	}
	return dir, nil
}
