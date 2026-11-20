//go:build unix

package tools

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestConnectCreatesStateDb0o600UnderPermissiveUmask(t *testing.T) {
	env := asyncTestEnv(t)
	old := syscall.Umask(0o022)
	conn, err := asyncConnect(context.Background(), env)
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	st, err := os.Stat(filepath.Join(env.Home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("state.db mode %o", st.Mode().Perm())
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if side, err := os.Stat(filepath.Join(env.Home, "state.db"+suffix)); err == nil && side.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %o", suffix, side.Mode().Perm())
		}
	}
}
