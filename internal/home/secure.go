package home

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/hostinfo"
)

// Package manager named by a bare true value or an empty marker, the shape only the NixOS module wrote
const legacyManagedSystem = "nixos"

// Marker values that mean managed without naming the manager, and those that mean unmanaged
var (
	managedTrue  = map[string]bool{"true": true, "1": true, "yes": true}
	managedFalse = map[string]bool{"false": true, "0": true, "no": true, "off": true, "brew": true, "homebrew": true}
)

// Package manager owning the install from ICEBATH_MANAGED or the home marker file, empty when unmanaged
func ManagedSystem(home string) string {
	marker := strings.ToLower(envValue("ICEBATH_MANAGED"))
	set := marker != ""
	if p := ManagedMarker.In(home); !set && exists(p) {
		set = true
		if data, err := os.ReadFile(p); err == nil {
			marker = strings.ToLower(strings.TrimSpace(strings.ToValidUTF8(string(data), "�")))
		} else {
			marker = ""
		}
	}
	switch {
	case !set || managedFalse[marker]:
		return ""
	case marker == "" || managedTrue[marker]:
		return legacyManagedSystem
	}
	return marker
}

// Reports an operator chmod skip or container signal that leaves directory modes to the deployment
func containerOrChmodSkipped() bool {
	if os.Getenv("ICEBATH_CONTAINER") != "" || os.Getenv("ICEBATH_SKIP_CHMOD") != "" {
		return true
	}
	return hostinfo.DetectContainer().Inside()
}

// Go mode of twelve POSIX permission bits
func posixMode(bits int64) fs.FileMode {
	m := fs.FileMode(bits & 0o777)
	if bits&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if bits&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if bits&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// Owner-only directory unless the install is managed, a container keeps its modes, or ICEBATH_HOME_MODE names one
func ApplySecureDirPolicy(path, home string) {
	if ManagedSystem(home) != "" {
		return
	}
	explicit := envValue("ICEBATH_HOME_MODE")
	if explicit == "" && containerOrChmodSkipped() {
		chownToConfigured(path)
		return
	}
	text := explicit
	if text == "" {
		text = "700"
	}
	mode, ok := parseInt(text, 8)
	if !ok {
		mode = 0o700
	}
	_ = os.Chmod(path, posixMode(mode))
	chownToConfigured(path)
}

// Directory holding the resolved running binary, the install tree no credential hardening may chmod
func InstallRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	real, err := resolvePath(exe)
	if err != nil {
		return "", err
	}
	return filepath.Dir(real), nil
}

// Restricts the resolved parent of path to 0700, never the filesystem root, a top level directory or the install tree
func SecureParentDir(path string) error {
	parent, err := resolvePath(filepath.Dir(path))
	if err != nil {
		return err
	}
	vol := filepath.VolumeName(parent)
	depth := 1 + len(strings.FieldsFunc(parent[len(vol):], func(r rune) bool { return r < 0x80 && os.IsPathSeparator(uint8(r)) }))
	if depth < 3 {
		return nil
	}
	root, err := InstallRoot()
	if err != nil {
		return fmt.Errorf("locate the install directory: %w", err)
	}
	if _, inside := within(parent, root); inside {
		slog.Warn(fmt.Sprintf("Not restricting permissions on %s: it is inside the icebath install directory (%s). "+
			"Credential files are normally stored under the icebath home directory instead.", parent, root))
		return nil
	}
	_ = os.Chmod(parent, 0o700)
	return nil
}

// Owner-only 0600 for an existing file unless the install is managed or a container leaves modes to the deployment
func ApplySecureFilePolicy(path, home string) {
	if ManagedSystem(home) != "" || containerOrChmodSkipped() {
		return
	}
	if exists(path) {
		_ = os.Chmod(path, 0o600)
	}
}
