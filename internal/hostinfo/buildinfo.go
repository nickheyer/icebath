package hostinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
)

// Go release that built this binary without its go prefix and experiment marker, a development build keeping devel
func GoVersion() string {
	fields := strings.Fields(runtime.Version())
	if len(fields) == 0 {
		return ""
	}
	if fields[0] == "devel" && len(fields) > 1 {
		return "devel " + strings.TrimPrefix(fields[1], "go")
	}
	release, _, _ := strings.Cut(strings.TrimPrefix(fields[0], "go"), "-X:")
	return release
}

// Version of a module linked into this binary, the replacement version when replaced, false when not linked
func ModuleVersion(path string) (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, dep := range info.Deps {
		if dep.Path != path {
			continue
		}
		if dep.Replace != nil {
			return dep.Replace.Version, true
		}
		return dep.Version, true
	}
	return "", false
}
