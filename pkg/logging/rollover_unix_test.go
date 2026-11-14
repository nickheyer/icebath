//go:build linux || darwin

package logging

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type chownCall struct {
	path     string
	uid, gid int
}

// Seeds path at mode 0640 and rolls it over once through a handler keeping one backup
func rolledHandler(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	h, err := newManagedRotatingFileHandler(context.Background(), path, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	if err := h.doRollover(); err != nil {
		t.Fatal(err)
	}
}

func asRoot(t *testing.T, fn func(string, int, int) error) {
	prevEuid, prevChown := geteuid, chown
	geteuid = func() int { return 0 }
	chown = fn
	t.Cleanup(func() { geteuid, chown = prevEuid, prevChown })
}

func TestRolloverHandsTheNewFileBackToThePreviousOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	var calls []chownCall
	asRoot(t, func(p string, uid, gid int) error {
		calls = append(calls, chownCall{p, uid, gid})
		return nil
	})
	if err := os.WriteFile(path, []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sys := before.Sys().(*syscall.Stat_t)
	rolledHandler(t, path)
	if len(calls) != 1 || calls[0] != (chownCall{path, int(sys.Uid), int(sys.Gid)}) {
		t.Fatalf("chown calls %v", calls)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
}

func TestRolloverSurvivesARefusedChown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	asRoot(t, func(string, int, int) error { return errors.New("operation not permitted") })
	rolledHandler(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
