package dashboard

import (
	"errors"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

const managedFilesRootEnv = "ICEBATH_DASHBOARD_FILES_ROOT"

const hostedManagedFilesRoot = "/opt/data"

// Where managed file routes start, the root they are locked to and whether the path may change
type managedFilesPolicy struct {
	defaultPath   string
	lockedRoot    string
	canChangePath bool
}

// Expanded and resolved path, 404 for a missing path when it must exist and 400 for an unresolvable one
func canonicalPath(path string, requireExists bool) (string, error) {
	expanded := pypath.Expanduser(path)
	var resolved string
	var err error
	if requireExists {
		resolved, err = pypath.Realpath(expanded, pypath.StrictAll)
	} else {
		resolved, err = pypath.Resolve(expanded)
	}
	if err == nil {
		return resolved, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		if requireExists {
			return "", NewHTTPException(404, "Path not found")
		}
		return "", err
	}
	return "", NewHTTPException(400, "Invalid path")
}

// Managed root created when missing and resolved, 500 when it cannot serve as a directory
func ensureManagedRoot(rawPath string) (string, error) {
	root := pypath.Expanduser(rawPath)
	resolved, err := func() (string, error) {
		if err := os.MkdirAll(root, 0o777); err != nil {
			return "", err
		}
		return pypath.Resolve(root)
	}()
	if err != nil {
		return "", NewHTTPException(500, fmt.Sprintf("Managed files root is unavailable: %s", pypath.OSErrorText(err)))
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return "", NewHTTPException(500, "Managed files root is not a directory")
	}
	return resolved, nil
}

// Whether target is root or lies under it, a pure path comparison
func pathIsUnder(root, target string) bool {
	key := pypath.Normcase(pypath.PurePathStr(root))
	cur := pypath.PurePathStr(target)
	for {
		if pypath.Normcase(cur) == key {
			return true
		}
		parent := pypath.PathParent(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

// Stripped path text, 400 when it carries a NUL
func pathText(rawPath string) (string, error) {
	text := pystr.Strip(rawPath)
	for i := 0; i < len(text); i++ {
		if text[i] == 0 {
			return "", NewHTTPException(400, "Invalid path")
		}
	}
	return text, nil
}

// Whether an explicit home's root is the hosted container data dir
func defaultHermesRootIsOptData() bool {
	raw := pystr.Strip(os.Getenv("ICEBATH_HOME"))
	if raw == "" {
		return false
	}
	root, err := pypath.Resolve(pypath.Expanduser(home.Root()))
	if err != nil {
		if root, err = pypath.Resolve(pypath.Expanduser(raw)); err != nil {
			return false
		}
	}
	return pypath.Normcase(root) == pypath.Normcase(hostedManagedFilesRoot)
}

// Managed files policy, a forced or hosted root locking the routes and the home otherwise
func managedFilesPolicyOf(createRoot bool) (managedFilesPolicy, error) {
	if forced := pystr.Strip(os.Getenv(managedFilesRootEnv)); forced != "" {
		var root string
		var err error
		if createRoot {
			root, err = ensureManagedRoot(forced)
		} else {
			root, err = canonicalPath(forced, false)
		}
		if err != nil {
			return managedFilesPolicy{}, err
		}
		return managedFilesPolicy{defaultPath: root, lockedRoot: root, canChangePath: false}, nil
	}
	if defaultHermesRootIsOptData() {
		root := hostedManagedFilesRoot
		if createRoot {
			var err error
			if root, err = ensureManagedRoot(hostedManagedFilesRoot); err != nil {
				return managedFilesPolicy{}, err
			}
		}
		return managedFilesPolicy{defaultPath: root, lockedRoot: root, canChangePath: false}, nil
	}
	homeDir, err := canonicalPath("~", false)
	if err != nil {
		return managedFilesPolicy{}, err
	}
	return managedFilesPolicy{defaultPath: homeDir, canChangePath: true}, nil
}

// Whether a hosted read may reach target, inside the locked root and never a sensitive file
func hostedFSPathAllowed(root, target string) (bool, error) {
	resolved, err := canonicalPath(target, false)
	if err != nil {
		return false, err
	}
	return pathIsUnder(root, resolved) && !tuigateway.IsSensitivePath(target) && !tuigateway.IsSensitivePath(resolved), nil
}

// Locked root preview and git reads share, 403 for a target outside the managed read boundary
func hostedFSReadGuard(target string) (string, error) {
	policy, err := managedFilesPolicyOf(false)
	if err != nil {
		return "", err
	}
	if policy.lockedRoot == "" {
		return "", nil
	}
	allowed, err := hostedFSPathAllowed(policy.lockedRoot, target)
	if err != nil {
		return "", err
	}
	if !allowed {
		return "", NewHTTPException(403, "Path is outside the managed read boundary")
	}
	return policy.lockedRoot, nil
}
