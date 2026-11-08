package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pypwd"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Raised when the account a system service should run as cannot be chosen or found
type ServiceUserError struct{ Msg string }

func (e *ServiceUserError) Error() string { return e.Msg }

var (
	serviceDefProjectRoot = pm.RepoRoot
	serviceDefUserHome    = pypath.PathHome
	serviceDefChown       = os.Chown
	launchdPlistPathRe    = regexp.MustCompile(`(?s)(<key>PATH</key>\s*<string>)(.*?)(</string>)`)
)

// Directory test that counts every stat failure as absent
func isDirSafe(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Existing node_modules bin and home node bin directories a service definition puts on PATH
func buildServicePathDirs(ctx context.Context, projectRoot string) []string {
	if projectRoot == "" {
		projectRoot = serviceDefProjectRoot()
	}
	h := home.Dir(ctx)
	var candidates []string
	for _, extra := range []string{filepath.Join(projectRoot, "node_modules", ".bin"), filepath.Join(h, "node", "bin"), filepath.Join(h, "node_modules", ".bin")} {
		if isDirSafe(extra) {
			candidates = append(candidates, extra)
		}
	}
	return candidates
}

// Resolved home when it is a directory, else the project root, so a service never starts in a vanished checkout
func stableServiceWorkingDir(ctx context.Context) string {
	if h := home.Dir(ctx); h != "" && isDirSafe(h) {
		if resolved, err := pypath.Resolve(h); err == nil {
			return resolved
		}
	}
	return serviceDefProjectRoot()
}

// Node, npm and npx PATH directories the home's pm installed state records and that exist on disk
func pmManagedNodeDirs(h string) ([]string, error) {
	store := filepath.Join(h, "tools")
	facts, err := pm.NewFacts(filepath.Join(store, "facts.json"), false)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, name := range []string{"node", "npm", "npx"} {
		raw := facts.EnvFor(name, store)["PATH"]
		var values []string
		switch v := raw.(type) {
		case []string:
			values = v
		case []any:
			for _, item := range v {
				s, ok := item.(string)
				if !ok {
					return nil, fmt.Errorf("PATH entry %v of %s is not a string", item, name)
				}
				values = append(values, s)
			}
		}
		for _, value := range values {
			if value != "" && isDirSafe(value) {
				dirs = append(dirs, value)
			}
		}
	}
	return dirs, nil
}

// Entries plus the pm recorded node dirs, or the legacy home node tree only when pm recorded none
func appendNodeDirForService(ctx context.Context, entries []string, hermesRoot string) []string {
	h := hermesRoot
	if h == "" {
		h = home.Dir(ctx)
	}
	managed, err := pmManagedNodeDirs(h)
	if err != nil {
		managed = nil
	}
	for _, entry := range managed {
		if !slices.Contains(entries, entry) {
			entries = append(entries, entry)
		}
	}
	if len(managed) > 0 || !home.ManagedNodeTreePresent(h) {
		return entries
	}
	for _, dir := range home.NodeDirs(h) {
		if isDirSafe(dir) && !slices.Contains(entries, dir) {
			entries = append(entries, dir)
		}
	}
	return entries
}

// Account name, group, home dir and uid a system service runs as, refusing implicit root
func systemServiceIdentity(runAsUser string) (string, string, string, int, error) {
	username := runAsUser
	for _, key := range []string{"SUDO_USER", "USER", "LOGNAME"} {
		if username != "" {
			break
		}
		username = os.Getenv(key)
	}
	if username == "" {
		var err error
		if username, err = pypwd.Getuser(); err != nil {
			return "", "", "", 0, err
		}
	}
	username = pystr.Strip(username)
	if username == "" {
		return "", "", "", 0, &ServiceUserError{"Could not determine which user the gateway service should run as"}
	}
	if username == "root" && runAsUser == "" {
		return "", "", "", 0, &ServiceUserError{"Refusing to install the gateway system service as root; pass --run-as-user root to override (e.g. in LXC containers)"}
	}
	if username == "root" {
		fmt.Fprintln(lifecycleOut, cliui.Color("⚠ Installing gateway service to run as root.", cliui.Yellow))
		fmt.Fprintln(lifecycleOut, cliui.Color("    This is fine for LXC/container environments but not recommended on bare-metal hosts.", cliui.Dim))
	}
	info, err := pypwd.Getpwnam(username)
	var keyErr *pypwd.KeyError
	if errors.As(err, &keyErr) {
		return "", "", "", 0, &ServiceUserError{"Unknown user: " + username}
	}
	if err != nil {
		return "", "", "", 0, err
	}
	group, err := pypwd.Getgrgid(info.GID)
	if err != nil {
		return "", "", "", 0, err
	}
	return username, group.Name, info.Dir, info.UID, nil
}

// Path with the caller home prefix swapped for the target home, other paths expanded and kept
func remapPathForUser(p, targetHomeDir string) (string, error) {
	current, err := serviceDefUserHome()
	if err != nil {
		return "", err
	}
	expanded, err := pypath.PathExpanduser(p)
	if err != nil {
		return "", err
	}
	if rel, ok := pypath.PathRelativeTo(expanded, current); ok {
		return pypath.PathJoin(targetHomeDir, rel), nil
	}
	return pypath.PathStr(expanded), nil
}

// Caller home in effect remapped under the target account's default home, a custom home kept as is
func hermesHomeForTargetUser(ctx context.Context, targetHomeDir string) (string, error) {
	current := home.Dir(ctx)
	if raw := pystr.Strip(os.Getenv("ICEBATH_HOME")); raw != "" {
		expanded, err := pypath.PathExpanduser(raw)
		if err != nil {
			return "", err
		}
		current = expanded
	}
	userHome, err := serviceDefUserHome()
	if err != nil {
		return "", err
	}
	if rel, ok := pypath.PathRelativeTo(current, pypath.PathJoin(userHome, ".icebath")); ok {
		return pypath.PathJoin(pypath.PathJoin(targetHomeDir, ".icebath"), rel), nil
	}
	return pypath.PathStr(current), nil
}

// Publishes the install launchers a service definition references, owned by the service account for a system unit
func prepareServiceLauncher(ctx context.Context, system bool, runAsUser string) error {
	root, h := serviceDefProjectRoot(), home.Dir(ctx)
	owned, uid, username := false, 0, ""
	if system {
		name, _, homeDir, id, err := systemServiceIdentity(runAsUser)
		if err != nil {
			return err
		}
		if root, err = remapPathForUser(root, homeDir); err != nil {
			return err
		}
		if h, err = hermesHomeForTargetUser(ctx, homeDir); err != nil {
			return err
		}
		owned, uid, username = true, id, name
	}
	octx := home.WithOverride(ctx, h)
	if pm.ResolveInstallBinary(root) == "" {
		return nil
	}
	local := filepath.Join(root, ".icebath", "bin")
	paths, err := pm.EnsureInstallLaunchers(octx, root, local)
	if err != nil {
		return err
	}
	if len(paths) != pm.EntryPointCount() {
		return errors.New("Could not publish the gateway installation launcher")
	}
	if !owned {
		return nil
	}
	info, err := pypwd.Getpwnam(username)
	if err != nil {
		return err
	}
	for _, p := range append([]string{filepath.Dir(local), local}, paths...) {
		if err := serviceDefChown(p, uid, info.GID); err != nil {
			return err
		}
	}
	return nil
}

// Definition text trimmed with every line right stripped so whitespace drift never reads as stale
func normalizeServiceDefinition(text string) string {
	lines := pystr.Splitlines(pystr.Strip(text), false)
	for i, line := range lines {
		lines[i] = pystr.Rstrip(line)
	}
	return strings.Join(lines, "\n")
}

// Normalized plist with the PATH payload masked since the invoking shell decides it
func normalizeLaunchdPlistForComparison(text string) string {
	return launchdPlistPathRe.ReplaceAllString(normalizeServiceDefinition(text), "${1}__ICEBATH_PATH__${3}")
}
