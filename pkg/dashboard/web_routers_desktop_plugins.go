package dashboard

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/desktopplugins"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var desktopPluginReadableFiles = map[string]bool{"plugin.js": true, ".hermes-package.json": true}

const pluginSourceMaxBytes = 16 * 1024 * 1024

// Pure path parts of p, empty and dot components dropped as pathlib does
func purePathParts(p string) []string {
	var parts []string
	for _, part := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
		if part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

// Path under the desktop plugins root and its parts, judged as written with at most a folder and a file
func desktopPluginInsideRoot(raw string) (string, []string, error) {
	root, err := desktopplugins.Root()
	if err != nil {
		return "", nil, err
	}
	rawParts, rootParts := purePathParts(raw), purePathParts(root)
	notPath := NewHTTPException(404, "Not a desktop plugin path")
	if filepath.IsAbs(raw) != filepath.IsAbs(root) || len(rawParts) < len(rootParts) || !slices.Equal(rawParts[:len(rootParts)], rootParts) {
		return "", nil, notPath
	}
	parts := rawParts[len(rootParts):]
	if len(parts) > 2 || slices.Contains(parts, "..") {
		return "", nil, notPath
	}
	return filepath.Join(append([]string{root}, parts...)...), parts, nil
}

// Entries of the root or one plugin folder, sorted by name
func desktopPluginList(raw string) (*pyjson.Object, error) {
	target, parts, err := desktopPluginInsideRoot(raw)
	if err != nil {
		return nil, err
	}
	if st, serr := os.Stat(target); len(parts) > 1 || serr != nil || !st.IsDir() {
		return nil, NewHTTPException(404, "Not a desktop plugin folder")
	}
	children, err := os.ReadDir(target)
	if err != nil {
		return nil, err
	}
	entries := []any{}
	for _, child := range children {
		p := filepath.Join(target, child.Name())
		st, serr := os.Stat(p)
		entries = append(entries, pyjson.ObjectOf("name", child.Name(), "path", p, "isDirectory", serr == nil && st.IsDir()))
	}
	return pyjson.ObjectOf("entries", entries), nil
}

// Entry file of a plugin folder, a linked folder followed and a linked or swapped file refused
func desktopPluginRead(raw string) (*pyjson.Object, error) {
	target, parts, err := desktopPluginInsideRoot(raw)
	if err != nil {
		return nil, err
	}
	notFound := NewHTTPException(404, "Not a desktop plugin file")
	if len(parts) != 2 || !desktopPluginReadableFiles[parts[1]] {
		return nil, notFound
	}
	parent, err := pypath.Realpath(filepath.Dir(target), pypath.StrictAll)
	if err != nil {
		return nil, notFound
	}
	entry := filepath.Join(parent, parts[1])
	if tuigateway.IsSensitivePath(entry) {
		return nil, notFound
	}
	if st, err := os.Lstat(entry); err != nil || !st.Mode().IsRegular() {
		return nil, notFound
	}
	handle, err := os.OpenFile(entry, desktopPluginOpenFlags, 0)
	if err != nil {
		return nil, notFound
	}
	defer handle.Close()
	st, err := handle.Stat()
	if err != nil {
		return nil, err
	}
	here, err := os.Lstat(entry)
	if err != nil {
		return nil, notFound
	}
	resolved, err := pypath.Realpath(entry, pypath.StrictAll)
	if err != nil {
		return nil, notFound
	}
	if !(resolved == entry && st.Mode().IsRegular() && os.SameFile(st, here)) {
		return nil, notFound
	}
	if st.Size() > pluginSourceMaxBytes {
		return nil, NewHTTPException(413, "Plugin source exceeds 16 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(handle, pluginSourceMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > pluginSourceMaxBytes {
		return nil, NewHTTPException(413, "Plugin source exceeds 16 MiB")
	}
	text, err := pycodecs.DecodeReplace(data, "utf-8-sig")
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("byteSize", st.Size(), "path", target, "text", text, "truncated", false), nil
}

// Root after copying unified packages' desktop halves into it
func desktopPluginsRootReconciled() (string, error) {
	root, err := desktopplugins.Root()
	if err != nil {
		return "", err
	}
	if _, err := desktopplugins.ReconcileDesktopHalves(home.Root(), root); err != nil {
		return "", err
	}
	return root, nil
}

// Router of the host's desktop plugin folder for the Webapp loader
func desktopPluginsRouter() *APIRouter {
	r := NewAPIRouter("")
	r.Get("/api/desktop-plugins/root", &Endpoint{
		Name: "desktop_plugins_root_route",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			root, err := desktopPluginsRootReconciled()
			if err != nil {
				return nil, err
			}
			return pyjson.ObjectOf("root", root), nil
		},
	})
	r.Post("/api/desktop-plugins/reconcile", &Endpoint{
		Name: "reconcile_desktop_plugins",
		Doc: "Electron's ``reconcileDesktopPluginsReport``: re-run the copy after an install, update or removal, and\n" +
			"    name each package whose desktop half it refused and no later profile published, so the install tab does not\n" +
			"    report it as installed.",
		Handle: func(ctx context.Context, c *Call) (any, error) {
			root, err := desktopplugins.Root()
			if err != nil {
				return nil, err
			}
			return desktopplugins.ReconcileDesktopHalvesReport(home.Root(), root)
		},
	})
	r.Get("/api/desktop-plugins/list", &Endpoint{
		Name:   "list_desktop_plugins",
		Params: []Param{RequiredQuery("path", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return desktopPluginList(c.Str("path")) },
	})
	r.Get("/api/desktop-plugins/read", &Endpoint{
		Name:   "read_desktop_plugin_file",
		Params: []Param{RequiredQuery("path", Str)},
		Handle: func(ctx context.Context, c *Call) (any, error) { return desktopPluginRead(c.Str("path")) },
	})
	return r
}
