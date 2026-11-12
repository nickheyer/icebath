package selfupdate

import (
	"context"
	"os"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// PM node tool lookup, swapped by tests where hermes patches find_node_executable
var findNodeExecutable = pm.FindNodeExecutable

// Whether npmPath names a Windows npm shim by WSL drive interop, an exe cmd or bat suffix, or a backslash
func isWindowsNpmPath(npmPath string) bool {
	low := pystr.Lower(npmPath)
	mount := ""
	if strings.HasPrefix(low, "/mnt/") {
		mount = strings.SplitN(low, "/", 4)[2]
	}
	r, size := utf8.DecodeRuneInString(mount)
	driveMount := size > 0 && size == len(mount) && pystr.IsAlpha(r)
	return strings.HasSuffix(low, ".exe") || strings.HasSuffix(low, ".cmd") || strings.HasSuffix(low, ".bat") ||
		driveMount || strings.Contains(npmPath, "\\")
}

// PM npm path, empty when PM has none or when a POSIX host would get a Windows shim, never a PATH rescan
func resolveNodeRuntimeNpm(ctx context.Context) (string, error) {
	npm, err := findNodeExecutable(ctx, "npm")
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" || npm == "" {
		return npm, nil
	}
	if isWindowsNpmPath(npm) {
		return "", nil
	}
	return npm, nil
}

// IsTermuxEnv reports a Termux environment from env, or from the process environment when env is empty
func IsTermuxEnv(env map[string]string) bool {
	get := os.Getenv
	if len(env) > 0 {
		get = func(key string) string { return env[key] }
	}
	prefix := get("PREFIX")
	return get("TERMUX_VERSION") != "" || strings.Contains(prefix, "com.termux/files/usr") || strings.HasPrefix(prefix, "/data/data/com.termux/")
}
