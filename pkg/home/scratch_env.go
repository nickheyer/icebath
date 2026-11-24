package home

import (
	"strings"
	"unicode/utf8"

	"context"
	"os"
	"runtime"

	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Longest temp root that leaves room for Chrome and code kernel AF_UNIX socket names
const SocketTmpdirMaxLen = 50

// Temp root short enough for AF_UNIX sockets, the system root when the default one is too long
func SocketSafeTmpdir() (string, error) {
	if runtime.GOOS == "darwin" {
		return "/tmp", nil
	}
	candidate, err := pathutil.TempDir()
	if err != nil {
		return "", err
	}
	if st, serr := os.Stat("/tmp"); utf8.RuneCountInString(candidate) <= SocketTmpdirMaxLen || serr != nil || !st.IsDir() {
		return candidate, nil
	}
	return "/tmp", nil
}

// Points TMPDIR, TMP and TEMP in env at the scratch dir of its ICEBATH_HOME unless the user set them
func ApplyScratchTmpEnv(ctx context.Context, env map[string]string) bool {
	ours := env[ScratchDirMarkerEnv]
	for _, key := range ScratchTmpEnvVars {
		if v := strings.TrimSpace(env[key]); v != "" && v != ours {
			return false
		}
	}
	var base string
	if h := strings.TrimSpace(env["ICEBATH_HOME"]); h != "" {
		base = expand(h)
	} else {
		var err error
		if base, err = processHome(); err != nil {
			return false
		}
	}
	scratch := EnsureScratchDir(ctx, base)
	for _, key := range ScratchTmpEnvVars {
		env[key] = scratch
	}
	env[ScratchDirMarkerEnv] = scratch
	return true
}

// Boot hook pointing this process temp vars at its scratch dir, rerun after anything re-homes the process
func ExportScratchTmpEnv(ctx context.Context) bool {
	env := map[string]string{}
	for _, key := range append([]string{ScratchDirMarkerEnv, "ICEBATH_HOME"}, ScratchTmpEnvVars...) {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	if !ApplyScratchTmpEnv(ctx, env) {
		return false
	}
	for _, key := range append([]string{ScratchDirMarkerEnv}, ScratchTmpEnvVars...) {
		_ = os.Setenv(key, env[key])
	}
	return true
}
