package home

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Uncached container probe, cached container fact and chown call, swapped by tests that simulate a container or record ownership
var (
	detectContainer = hostinfo.DetectContainer
	isContainer     = hostinfo.IsContainer
	chownPath       = os.Chown
)

// Package manager owning the install from ICEBATH_MANAGED or the home marker, empty when unmanaged
func ManagedSystem(ctx context.Context, h string) string {
	marker := strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_MANAGED")))
	set := marker != ""
	if h == "" {
		h = Dir(ctx)
	}
	if path := pathutil.JoinKeepDotDot(h, ".managed"); !set {
		if pathExists(path) {
			set = true
			if b, err := os.ReadFile(path); err == nil {
				marker = strings.ToLower(strings.TrimSpace(textutil.NormalizeNewlines(strings.ToValidUTF8(string(b), "\uFFFD"))))
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
func ContainerOrChmodSkipped() bool {
	if os.Getenv("ICEBATH_CONTAINER") != "" || os.Getenv("ICEBATH_SKIP_CHMOD") != "" {
		return true
	}
	return detectContainer()
}

// ICEBATH_UID and ICEBATH_GID as the ids chown takes, -1 leaving an id unchanged, nil when unset, on Windows, or set to anything but an id from -1 to 4294967295 (warned once)
func resolveIcebathUIDGID() (*int64, *int64) {
	if runtime.GOOS == "windows" {
		return nil, nil
	}
	envID := func(name string) *int64 {
		raw := os.Getenv(name)
		text := strings.TrimSpace(raw)
		if text == "" {
			return nil
		}
		n, err := strconv.ParseInt(text, 10, 64)
		switch {
		case err != nil && !errors.Is(err, strconv.ErrRange):
			warnIgnoredEnv(name, raw, "is not a whole number, leaving that id unchanged")
			return nil
		case err != nil || n < -1 || n > math.MaxUint32:
			warnIgnoredEnv(name, raw, "is outside the ids from -1 to 4294967295, leaving that id unchanged")
			return nil
		}
		return &n
	}
	return envID("ICEBATH_UID"), envID("ICEBATH_GID")
}

// The id chown takes for v, minus one leaving the id unchanged
func chownID(v *int64) int {
	if v == nil {
		return -1
	}
	return int(*v)
}

// Chowns path to ICEBATH_UID and ICEBATH_GID when either is set, failures ignored
func chownToIcebathUID(path string) {
	uid, gid := resolveIcebathUIDGID()
	if uid == nil && gid == nil {
		return
	}
	_ = chownPath(path, chownID(uid), chownID(gid))
}

// Environment settings already warned about, by name and value
var ignoredEnvWarned = &sync.Map{}

// Logs once per variable and value that an environment setting is ignored and why
func warnIgnoredEnv(name, value, reason string) {
	if _, seen := ignoredEnvWarned.LoadOrStore(name+"="+value, true); !seen {
		pkgLog().Warn(fmt.Sprintf("%s=%s %s", name, strconv.Quote(value), reason))
	}
}

// Mode bits ICEBATH_HOME_MODE names in octal, an optional 0o prefix allowed, 700 for unset and for a value that is no octal number within 32 bits (warned once)
func homeModeBits(explicit string) uint64 {
	text := strings.TrimSpace(explicit)
	if text == "" {
		return 0o700
	}
	mode, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(text), "0o"), 8, 32)
	switch {
	case errors.Is(err, strconv.ErrRange):
		warnIgnoredEnv("ICEBATH_HOME_MODE", explicit, "is beyond the largest file mode, using 700")
		return 0o700
	case err != nil:
		warnIgnoredEnv("ICEBATH_HOME_MODE", explicit, "is not an octal file mode, using 700")
		return 0o700
	}
	return mode
}

// Owner only directory unless the home is managed, a container keeps its modes, or ICEBATH_HOME_MODE names one
func ApplySecureDirPolicy(ctx context.Context, path, h string) {
	if ManagedSystem(ctx, h) != "" {
		return
	}
	explicit := os.Getenv("ICEBATH_HOME_MODE")
	if ContainerOrChmodSkipped() && strings.TrimSpace(explicit) == "" {
		chownToIcebathUID(path)
		return
	}
	mode := homeModeBits(explicit)
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
