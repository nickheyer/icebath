package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Venv prefix holding pyvenv.cfg above or beside the resolved running binary, ok false outside any venv
var runningVenvPrefix = func() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	resolved, err := pypath.Resolve(exe)
	if err != nil {
		return "", false
	}
	exeDir := filepath.Dir(resolved)
	for _, prefix := range []string{filepath.Dir(exeDir), exeDir} {
		if isRegularFileAt(filepath.Join(prefix, "pyvenv.cfg")) {
			return prefix, true
		}
	}
	return "", false
}

// Site packages of the running venv computed once, empty when the runtime starts outside a venv
var runningSitePackages = sync.OnceValues(func() ([]string, error) {
	prefix, ok := runningVenvPrefix()
	if !ok {
		return nil, nil
	}
	return venvSitePackagesDirs(prefix)
})

// Directories site getsitepackages reports for a venv prefix, else the prefix site packages of the pinned interpreter
func venvSitePackagesDirs(prefix string) ([]string, error) {
	if runtime.GOOS == "windows" {
		return []string{prefix, filepath.Join(prefix, "Lib", "site-packages")}, nil
	}
	if site, err := pm.SitePackages(prefix); err == nil {
		return []string{site}, nil
	}
	major, minor, err := pm.PinnedPythonVersion()
	if err != nil {
		return nil, err
	}
	return []string{filepath.Join(prefix, "lib", "python"+major+"."+minor, "site-packages")}, nil
}

// Exact site packages owned by the runtime, the running venv plus a validated runtime venv not already listed
func icebathSitePackages(child map[string]string) ([]string, error) {
	running, err := runningSitePackages()
	if err != nil {
		return nil, err
	}
	result := slices.Clone(running)
	venv, ok, err := ValidatedRuntimeVenv(child)
	if err != nil || !ok {
		return result, err
	}
	site, err := pm.SitePackages(venv)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(result, func(existing string) bool { return SamePath(site, existing) }) {
		result = append(result, site)
	}
	return result, nil
}

// Strips owned PYTHONPATH entries before dropping the venv markers so a validated VIRTUAL_ENV still proves ownership
func stripIcebathOwnedPythonpathAndRuntimeMarkers(child map[string]string) error {
	if err := stripIcebathOwnedPythonpath(child); err != nil {
		return err
	}
	for _, m := range activeVenvMarkerVars {
		delete(child, m)
	}
	return nil
}

// Removes entries naming a repo root spelling or runtime site packages exactly, every other entry stays byte for byte
func stripIcebathOwnedPythonpath(child map[string]string) error {
	pp := child["PYTHONPATH"]
	if pp == "" {
		return nil
	}
	owned, err := icebathSitePackages(child)
	if err != nil {
		return err
	}
	owned = append(owned, runtimeRepoRootAliases()...)
	entries := strings.Split(pp, string(os.PathListSeparator))
	var stripped []string
	for _, e := range entries {
		if e != "" && slices.ContainsFunc(owned, func(p string) bool { return SamePath(e, p) }) {
			stripped = append(stripped, e)
		}
	}
	var kept []string
	for _, e := range entries {
		if !slices.Contains(stripped, e) {
			kept = append(kept, e)
		}
	}
	if len(kept) > 0 {
		child["PYTHONPATH"] = strings.Join(kept, string(os.PathListSeparator))
	} else {
		delete(child, "PYTHONPATH")
	}
	if len(stripped) > 0 {
		toolsLogger(nil).Debug(fmt.Sprintf("Stripped Icebath-owned entries from PYTHONPATH: %s", pyrepr.Repr(stripped)))
	}
	return nil
}
