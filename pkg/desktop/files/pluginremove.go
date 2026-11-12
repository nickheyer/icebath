package files

import (
	"os"
	"path/filepath"
	"strings"
)

func isSingleSegment(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `\/`)
}

// Deletes one standalone desktop plugin folder by name under the app root, never a package's desktop half
func RemoveDesktopPlugin(appRoot string, rawName any) *OrderedObject {
	out, _ := WithDesktopPluginsRoot(appRoot, func() (*OrderedObject, error) {
		return removeFolder(appRoot, rawName), nil
	})
	return out
}

func removeFolder(appRoot string, rawName any) *OrderedObject {
	root := resolvePath(appRoot)
	raw := jsStringOrEmpty(rawName)
	exists := false
	if isSingleSegment(raw) {
		_, err := os.Lstat(filepath.Join(root, raw))
		exists = err == nil
	}
	name := raw
	if !exists {
		name = StripLikePython(raw)
	}
	if !isSingleSegment(name) {
		return NewOrderedObject("ok", false, "error", "invalid plugin folder name")
	}
	target := resolveAgainst(root, name)
	if rel, err := filepath.Rel(root, target); err != nil || rel != name {
		return NewOrderedObject("ok", false, "error", name+" is not inside the desktop-plugins folder")
	}
	st, err := os.Lstat(target)
	if err != nil {
		return NewOrderedObject("ok", false, "error", name+" is not installed")
	}
	if !st.IsDir() && !isLinkMode(st.Mode()) {
		return NewOrderedObject("ok", false, "error", name+" is not a plugin folder")
	}
	if st.IsDir() && !isLinkMode(st.Mode()) {
		if _, err := os.Stat(filepath.Join(target, PackageMarker)); err == nil {
			return NewOrderedObject("ok", false, "error", name+" is the desktop half of an agent plugin \u2014 uninstall that plugin instead")
		}
	}
	if err := removeAll(target); err != nil {
		return NewOrderedObject("ok", false, "error", nodeFsError(err, "rm", target).Error())
	}
	return NewOrderedObject("ok", true, "path", target)
}
