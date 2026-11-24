package home

import (
	"context"
	"errors"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
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
	if path := pathutil.Join(h, ".managed"); !set {
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

// ICEBATH_UID and ICEBATH_GID as integers, nil when unset, not a whole number or on Windows, a number beyond int64 kept as the bound of its sign
func resolveIcebathUIDGID() (*int64, *int64) {
	if runtime.GOOS == "windows" {
		return nil, nil
	}
	envInt := func(name string) *int64 {
		text := strings.TrimSpace(os.Getenv(name))
		if text == "" {
			return nil
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil
		}
		return &n
	}
	return envInt("ICEBATH_UID"), envInt("ICEBATH_GID")
}

// The uid_t chown accepts for v, minus one leaving the id alone, panicking with an OverflowError for anything else outside uid_t
func chownID(v *int64, kind string) int {
	switch {
	case v == nil || *v == -1:
		return -1
	case *v == math.MaxInt64:
		panic(&errorutil.Error{Kind: "OverflowError", Msg: kind + " is greater than maximum"})
	case *v < 0 || *v > math.MaxUint32:
		panic(&errorutil.Error{Kind: "OverflowError", Msg: kind + " is less than minimum"})
	}
	return int(*v)
}

// Chowns path to ICEBATH_UID and ICEBATH_GID when either is set, failures ignored
func chownToIcebathUID(path string) {
	uid, gid := resolveIcebathUIDGID()
	if uid == nil && gid == nil {
		return
	}
	u := chownID(uid, "uid")
	g := chownID(gid, "gid")
	_ = chownPath(path, u, g)
}

// Owner only directory unless the home is managed, a container keeps its modes, or ICEBATH_HOME_MODE names one
func ApplySecureDirPolicy(ctx context.Context, path, h string) {
	if ManagedSystem(ctx, h) != "" {
		return
	}
	explicit := strings.TrimSpace(os.Getenv("ICEBATH_HOME_MODE"))
	if ContainerOrChmodSkipped() && explicit == "" {
		chownToIcebathUID(path)
		return
	}
	text := explicit
	if text == "" {
		text = "700"
	}
	mode := int64(0o700)
	n, err := strconv.ParseInt(strings.TrimPrefix(strings.ToLower(text), "0o"), 8, 64)
	if err == nil || errors.Is(err, strconv.ErrRange) {
		if n > math.MaxInt32 || n < math.MinInt32 {
			panic(&errorutil.Error{Kind: "OverflowError", Msg: "Python int too large to convert to C int"})
		}
		mode = n
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
