package app

import (
	"errors"
	"reflect"
	"testing"
)

// Clears every variable the Ctrl Enter probe reads and hides the kernel release files
func localLinuxCtrlEnterEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range []string{"SSH_CONNECTION", "SSH_CLIENT", "SSH_TTY", "WT_SESSION", "GHOSTTY_RESOURCES_DIR", "GHOSTTY_BIN_DIR", "TERM", "TERM_PROGRAM", "WSL_DISTRO_NAME"} {
		t.Setenv(name, env[name])
	}
	prevRead, prevOS := ctrlEnterReadFile, ctrlEnterGOOS
	ctrlEnterReadFile = func(string) ([]byte, error) { return nil, errors.New("no /proc") }
	ctrlEnterGOOS = "linux"
	t.Cleanup(func() { ctrlEnterReadFile, ctrlEnterGOOS = prevRead, prevOS })
}

func hasKeys(bound [][]string, keys ...string) bool {
	for _, b := range bound {
		if reflect.DeepEqual(b, keys) {
			return true
		}
	}
	return false
}

// Environment lookup over a fixed map
func envOf(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}
