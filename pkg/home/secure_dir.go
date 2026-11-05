package home

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Container probe and chown call, swapped by tests that simulate a container or record ownership changes
var (
	detectContainer = hostinfo.IsContainer
	chownPath       = os.Chown
)

// Package manager owning the install from ICEBATH_MANAGED or the home marker, empty when unmanaged
func ManagedSystem(h string) string {
	if h == "" {
		h = Process()
	}
	marker := strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_MANAGED")))
	set := marker != ""
	if !set {
		path := filepath.Join(h, ".managed")
		if _, err := os.Stat(path); err == nil {
			set = true
			if b, err := os.ReadFile(path); err == nil {
				marker = strings.ToLower(strings.TrimSpace(pystr.DecodeUTF8Replace(b)))
			}
		}
	}
	switch marker {
	case "brew", "homebrew", "false", "0", "no", "off":
		return ""
	case "", "true", "1", "yes":
		if !set {
			return ""
		}
		return "nixos"
	}
	return marker
}

// Operator chmod skip or container signal that leaves directory modes to the deployment
func containerOrChmodSkipped() bool {
	if os.Getenv("ICEBATH_CONTAINER") != "" || os.Getenv("ICEBATH_SKIP_CHMOD") != "" {
		return true
	}
	return detectContainer()
}

// ICEBATH_UID and ICEBATH_GID as ints, minus one when unset, invalid or on Windows
func resolveIcebathUIDGID() (int, int) {
	if runtime.GOOS == "windows" {
		return -1, -1
	}
	envInt := func(name string) int {
		n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
		if err != nil {
			return -1
		}
		return n
	}
	return envInt("ICEBATH_UID"), envInt("ICEBATH_GID")
}

// Chowns path to ICEBATH_UID and ICEBATH_GID when either is set, failures ignored
func chownToIcebathUID(path string) {
	uid, gid := resolveIcebathUIDGID()
	if uid < 0 && gid < 0 {
		return
	}
	_ = chownPath(path, uid, gid)
}

// Octal mode text like int(text, 8), with sign, 0o prefix and digit underscores accepted
func parseOctalMode(text string) (int64, bool) {
	sign, digits := "", text
	if strings.HasPrefix(digits, "+") || strings.HasPrefix(digits, "-") {
		sign, digits = digits[:1], digits[1:]
	}
	if !strings.HasPrefix(strings.ToLower(digits), "0o") {
		digits = "0o" + digits
	}
	n, err := strconv.ParseInt(sign+digits, 0, 64)
	return n, err == nil
}

// Owner only directory unless the home is managed, a container keeps its modes, or ICEBATH_HOME_MODE names one
func ApplySecureDirPolicy(path, h string) {
	if ManagedSystem(h) != "" {
		return
	}
	explicit := strings.TrimSpace(os.Getenv("ICEBATH_HOME_MODE"))
	if containerOrChmodSkipped() && explicit == "" {
		chownToIcebathUID(path)
		return
	}
	text := explicit
	if text == "" {
		text = "700"
	}
	mode, ok := parseOctalMode(text)
	if !ok {
		mode = 0o700
	}
	fm := os.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		fm |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		fm |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		fm |= os.ModeSticky
	}
	_ = os.Chmod(path, fm)
	chownToIcebathUID(path)
}
