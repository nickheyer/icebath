package process

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

const isLinux = true

func secureRuntimeDir(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !filepath.IsAbs(path) || !info.IsDir() {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid() && info.Mode().Perm()&0o022 == 0
}

func userBusEnv(env map[string]string) map[string]string {
	runtimeDir := env[runtimeDirVar]
	if runtimeDir == "" || !secureRuntimeDir(runtimeDir) {
		runtimeDir = "/run/user/" + strconv.Itoa(os.Getuid())
		if !secureRuntimeDir(runtimeDir) {
			return env
		}
	}
	bus := filepath.Join(runtimeDir, "bus")
	info, err := os.Lstat(bus)
	if err != nil || info.Mode().Type() != fs.ModeSocket {
		return env
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return env
	}
	env[runtimeDirVar] = filepath.Clean(runtimeDir)
	env[busAddressVar] = "unix:path=" + bus
	return env
}
