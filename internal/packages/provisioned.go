package packages

import (
	"os"
	"path/filepath"
	"runtime"
)

// Command a person runs to provision an extra
func InstallHint(extra string) string { return installHint(extra) }

// Interpreter of a provisioned extra, false when the extra is unknown or not provisioned
func (h *Packages) ExtraPython(name string) (string, bool) {
	return h.extraPython(name)
}

// Why an extra can't be installed on this machine, empty when it can; false for an extra no feature declares
func (h *Packages) ExtraUnsupported(name string) (string, bool) {
	e, ok := h.extra(name)
	if !ok {
		return "", false
	}
	return e.Unsupported[CurrentTarget()], true
}

// Folder of a Python package installed in a provisioned extra's environment, such as the one a native library ships in; false when the
// extra is not provisioned or holds no such package
func (h *Packages) ExtraModule(extra, module string) (string, bool) {
	python, ok := h.extraPython(extra)
	if !ok {
		return "", false
	}
	venv := filepath.Dir(filepath.Dir(python))
	sites := []string{filepath.Join(venv, "Lib", "site-packages")}
	if runtime.GOOS != "windows" {
		sites, _ = filepath.Glob(filepath.Join(venv, "lib", "python*", "site-packages"))
	}
	for _, site := range sites {
		dir := filepath.Join(site, module)
		if info, err := os.Stat(filepath.Join(dir, "__init__.py")); err == nil && info.Mode().IsRegular() {
			return dir, true
		}
	}
	return "", false
}
