package home

import (
	"os"
	"runtime"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Longest temp root that leaves room for Chrome and code kernel AF_UNIX socket names
const SocketTmpdirMaxLen = 50

// Temp root short enough for AF_UNIX sockets, the system root when the default one is too long
func SocketSafeTmpdir() (string, error) {
	if runtime.GOOS == "darwin" {
		return "/tmp", nil
	}
	candidate, err := pypath.Gettempdir()
	if err != nil {
		return "", err
	}
	if st, serr := os.Stat("/tmp"); len(candidate) <= SocketTmpdirMaxLen || serr != nil || !st.IsDir() {
		return candidate, nil
	}
	return "/tmp", nil
}

// Points TMPDIR, TMP and TEMP in env at the scratch dir of its ICEBATH_HOME unless the user set them
func ApplyScratchTmpEnv(env map[string]string) bool {
	ours := env[ScratchDirMarkerEnv]
	for _, key := range ScratchTmpEnvVars {
		if v := pystr.Strip(env[key]); v != "" && v != ours {
			return false
		}
	}
	base := Process()
	if h := pystr.Strip(env["ICEBATH_HOME"]); h != "" {
		base = expand(h)
	}
	if base == "" {
		return false
	}
	scratch, err := EnsureScratchDir(base)
	if err != nil {
		return false
	}
	for _, key := range ScratchTmpEnvVars {
		env[key] = scratch
	}
	env[ScratchDirMarkerEnv] = scratch
	return true
}

// Boot hook pointing this process temp vars at its scratch dir, rerun after anything re-homes the process
func ExportScratchTmpEnv() bool {
	env := map[string]string{}
	for _, key := range append([]string{ScratchDirMarkerEnv, "ICEBATH_HOME"}, ScratchTmpEnvVars...) {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	if !ApplyScratchTmpEnv(env) {
		return false
	}
	for _, key := range append([]string{ScratchDirMarkerEnv}, ScratchTmpEnvVars...) {
		_ = os.Setenv(key, env[key])
	}
	return true
}
