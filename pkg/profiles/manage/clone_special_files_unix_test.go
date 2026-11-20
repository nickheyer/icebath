//go:build unix

package manage

import (
	"path/filepath"
	"syscall"
	"testing"
)

func TestCloneAllSkipsSpecialFiles(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(def, "config.yaml"), "model: test")
	browser := filepath.Join(def, "home", ".agent-browser")
	writeFile(t, filepath.Join(browser, "state.json"), "{}")
	if err := syscall.Mkfifo(filepath.Join(browser, "control"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := CreateProfile(t.Context(), "coder", CreateOptions{CloneAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(dir, "home", ".agent-browser", "state.json")) {
		t.Fatal("state.json was not cloned")
	}
	if pathExists(filepath.Join(dir, "home", ".agent-browser", "control")) {
		t.Fatal("fifo was cloned")
	}
}
