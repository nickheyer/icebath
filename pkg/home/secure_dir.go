package home

import (
	"context"
	"math"
	"math/big"
	"os"
	"runtime"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Uncached container probe, cached container fact and chown call, swapped by tests that simulate a container or record ownership
var (
	detectContainer = hostinfo.DetectContainer
	isContainer     = hostinfo.IsContainer
	chownPath       = os.Chown
)

// Package manager owning the install from ICEBATH_MANAGED or the home marker, empty when unmanaged
func ManagedSystem(ctx context.Context, h string) string {
	marker := pystr.Lower(pystr.Strip(os.Getenv("ICEBATH_MANAGED")))
	set := marker != ""
	if h == "" {
		h = Dir(ctx)
	}
	if path := pypath.PathJoin(h, ".managed"); !set {
		if pathExists(path) {
			set = true
			if b, err := os.ReadFile(path); err == nil {
				marker = pystr.Lower(pystr.Strip(pystr.TranslateNewlines(pystr.DecodeUTF8Replace(b))))
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

// ICEBATH_UID and ICEBATH_GID as Python ints, nil when unset, invalid or on Windows
func resolveIcebathUIDGID() (*big.Int, *big.Int) {
	if runtime.GOOS == "windows" {
		return nil, nil
	}
	envInt := func(name string) *big.Int {
		text := pystr.Strip(os.Getenv(name))
		if text == "" {
			return nil
		}
		n, ok := pyint.Parse(text)
		if !ok {
			return nil
		}
		return n
	}
	return envInt("ICEBATH_UID"), envInt("ICEBATH_GID")
}

// The uid_t os.chown accepts for v, panicking with the OverflowError CPython raises for anything else but minus one
func chownID(v *big.Int, kind string) int {
	if v == nil {
		return -1
	}
	if !v.IsInt64() {
		if v.Sign() < 0 {
			panic(&pyint.Error{Kind: "OverflowError", Msg: kind + " is less than minimum"})
		}
		panic(&pyint.Error{Kind: "OverflowError", Msg: kind + " is greater than maximum"})
	}
	n := v.Int64()
	if n == -1 {
		return -1
	}
	if n < 0 || n > math.MaxUint32 {
		panic(&pyint.Error{Kind: "OverflowError", Msg: kind + " is less than minimum"})
	}
	return int(n)
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
	explicit := pystr.Strip(os.Getenv("ICEBATH_HOME_MODE"))
	if ContainerOrChmodSkipped() && explicit == "" {
		chownToIcebathUID(path)
		return
	}
	text := explicit
	if text == "" {
		text = "700"
	}
	mode := int64(0o700)
	if n, ok := pyint.ParseBase(text, 8); ok {
		if !n.IsInt64() || n.Int64() > math.MaxInt32 || n.Int64() < math.MinInt32 {
			panic(&pyint.Error{Kind: "OverflowError", Msg: "Python int too large to convert to C int"})
		}
		mode = n.Int64()
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
