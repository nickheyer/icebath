package dashboard

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"runtime"
	"slices"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Whether the outer launcher or image updates this dashboard so it must not offer icebath update
func dashboardLocalUpdateManagedExternally(ctx context.Context) bool {
	if defaultHermesRootIsOptData() {
		return true
	}
	if !hostinfo.IsContainer() {
		return false
	}
	if method, err := config.DetectInstallMethod(ctx, home.Dir(ctx), ""); err == nil && method == "git" {
		return false
	}
	return true
}

// Policy, resolved target and display path of a managed path, 400 403 or 404 when the policy refuses it
func resolveManagedPath(ctx context.Context, rawPath *string, forWrite, createRoot, forAgentRead bool) (managedFilesPolicy, string, string, error) {
	policy, err := managedFilesPolicyOf(createRoot)
	if err != nil {
		return policy, "", "", err
	}
	raw := ""
	if rawPath != nil {
		raw = *rawPath
	}
	text, err := pathText(raw)
	if err != nil {
		return policy, "", "", err
	}
	root := policy.lockedRoot
	if forAgentRead && text != "" && !hasFilePrefix(text) && (runtime.GOOS == "windows" || !pyfs.Access(text, osReadable)) {
		translated, ok, err := agentContainerPath(ctx, text, "", "", false, "")
		if err != nil {
			return policy, "", "", err
		}
		if ok {
			text = translated
		}
	}
	var candidate string
	switch {
	case root != "" && (text == "" || text == "." || text == "/"):
		candidate = root
	case text == "":
		candidate = policy.defaultPath
	default:
		if candidate, err = pypath.PathExpanduser(text); err != nil {
			return policy, "", "", err
		}
		if root != "" && !pypath.IsAbs(candidate) {
			if hasDotDotPart(candidate) {
				return policy, "", "", NewHTTPException(400, "Path cannot contain '..'")
			}
			candidate = pypath.PathJoin(root, candidate)
		} else if !pypath.IsAbs(candidate) {
			return policy, "", "", NewHTTPException(400, "Path must be absolute")
		}
	}
	if hasDotDotPart(candidate) {
		return policy, "", "", NewHTTPException(400, "Path cannot contain '..'")
	}
	var resolved string
	exists := false
	if forWrite {
		fi, err := pypath.PathlibStat(candidate)
		if err != nil {
			return policy, "", "", err
		}
		exists = fi != nil
	}
	if forWrite && !exists {
		parent, err := canonicalPath(pypath.PathParent(candidate), false)
		if err != nil {
			return policy, "", "", err
		}
		resolved = pypath.PathJoin(parent, pypath.PathName(candidate))
	} else if resolved, err = canonicalPath(candidate, !forWrite); err != nil {
		return policy, "", "", err
	}
	if root != "" && !pathIsUnder(root, resolved) {
		return policy, "", "", NewHTTPException(403, "Path outside managed files root")
	}
	return policy, resolved, resolved, nil
}

// Whether text starts with the file scheme exactly as written
func hasFilePrefix(text string) bool { return len(text) >= 5 && text[:5] == "file:" }

// Whether any pathlib part of p is a parent reference
func hasDotDotPart(p string) bool {
	_, parts := pypath.PathParts(p)
	return slices.Contains(parts, "..")
}

// Root, locked root and path mobility of a managed files policy as response fields
func managedResponseMeta(policy managedFilesPolicy) *pyjson.Object {
	o := pyjson.NewObject()
	var locked any
	if policy.lockedRoot != "" {
		locked = policy.lockedRoot
	}
	o.Set("root", locked)
	o.Set("locked_root", locked)
	o.Set("can_change_path", policy.canChangePath)
	return o
}

// Listing entry of a managed path, nil for a vanished file when skipping, a stand in for a broken link
func managedFileEntry(policy managedFilesPolicy, target string, skipMissing bool) (*pyjson.Object, error) {
	resolved, err := pypath.Resolve(target)
	if err != nil {
		return nil, NewHTTPException(400, "Invalid path")
	}
	name := pypath.PathName(target)
	if name == "" {
		if name = pypath.PathName(resolved); name == "" {
			name = resolved
		}
	}
	var st os.FileInfo
	if lfi, lerr := os.Lstat(target); lerr == nil && lfi.Mode()&fs.ModeSymlink != 0 {
		fi, serr := os.Stat(resolved)
		switch {
		case serr == nil:
			st = fi
		case errors.Is(serr, fs.ErrNotExist):
			if policy.lockedRoot != "" {
				parent, perr := pypath.Resolve(pypath.PathParent(target))
				if perr != nil {
					return nil, perr
				}
				if !pathIsUnder(policy.lockedRoot, pypath.PathJoin(parent, pypath.PathName(target))) {
					return nil, NewHTTPException(403, "Path outside managed files root")
				}
			}
			o := pyjson.NewObject()
			o.Set("name", name)
			o.Set("path", pypath.PathStr(target))
			o.Set("is_directory", false)
			o.Set("broken_link", true)
			o.Set("size", nil)
			o.Set("mtime", nil)
			o.Set("mime_type", nil)
			return o, nil
		}
	}
	if policy.lockedRoot != "" && !pathIsUnder(policy.lockedRoot, resolved) {
		return nil, NewHTTPException(403, "Path outside managed files root")
	}
	if st == nil {
		if st, err = os.Stat(resolved); err != nil {
			if errors.Is(err, fs.ErrNotExist) && skipMissing {
				return nil, nil
			}
			return nil, NewHTTPException(500, "Could not stat path: "+pypath.OSErrorText(err))
		}
	}
	o := pyjson.NewObject()
	o.Set("name", name)
	o.Set("path", resolved)
	o.Set("is_directory", st.IsDir())
	if st.IsDir() {
		o.Set("size", nil)
	} else {
		o.Set("size", st.Size())
	}
	o.Set("mtime", float64(st.ModTime().UnixNano())/1e9)
	if st.IsDir() {
		o.Set("mime_type", nil)
	} else {
		mime, _, _ := pymimetypes.GuessType(pypath.PathName(resolved), true)
		if mime == "" {
			mime = "application/octet-stream"
		}
		o.Set("mime_type", mime)
	}
	return o, nil
}
