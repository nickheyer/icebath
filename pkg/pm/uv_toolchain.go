package pm

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Toolchain lookup the Python environment engine calls, replaceable by tests the way Python patches _toolchain
var uvToolchain = Toolchain

// Replaces the toolchain lookup the Python environment engine calls and returns the previous one
func SwapToolchain(fn func(context.Context, bool, bool) (string, string, bool, error)) func(context.Context, bool, bool) (string, string, bool, error) {
	prev := uvToolchain
	uvToolchain = fn
	return prev
}

// Pinned uv and Python binaries without host discovery, a read only probe reporting found false instead of installing
func Toolchain(ctx context.Context, realize, explicit bool) (uv, python string, found bool, err error) {
	if realize {
		if _, err := ensure(ctx, "uv", EnsureOptions{Explicit: explicit}); err != nil {
			return "", "", false, err
		}
	}
	lockfile := currentLockfile()
	target, err := CurrentTarget()
	if err != nil {
		return "", "", false, err
	}
	binaries := map[string]string{}
	for _, name := range []string{"uv", "python"} {
		p, err := GetPackage(name)
		if err != nil {
			return "", "", false, err
		}
		loc, err := InstalledLocation(ctx, p, lockfile, target, LocationOptions{})
		if err != nil {
			return "", "", false, err
		}
		if loc == nil {
			if !realize {
				return "", "", false, nil
			}
			return "", "", false, NewInstallError(name, "pinned tool is unavailable", "run `icebath pm install`")
		}
		facts, store := loc.Facts, loc.Store
		if name == "python" && strings.HasPrefix(target, "win32") {
			sealed, err := Sealed(ctx)
			if err != nil {
				return "", "", false, err
			}
			if sealed {
				copied, err := WritableCopy(ctx, name, WritableCopyOptions{NoRealize: !realize, Explicit: explicit,
					Why: "a writable Python is required for bundled builds"})
				if err != nil {
					return "", "", false, err
				}
				if copied == nil {
					return "", "", false, nil
				}
				facts, store = copied.Facts, copied.Store
			}
		}
		fact, _ := facts.Get(name)
		binary := p.Binary(store.Entry(pyrepr.Str(fact["entry"])), target)
		if binary == "" || !isRegularFile(binary) {
			if !realize {
				return "", "", false, nil
			}
			return "", "", false, NewInstallError(name, "installed binary is missing", "run `icebath pm install`")
		}
		binaries[name] = binary
	}
	return Native(binaries["uv"]), Native(binaries["python"]), true, nil
}
