//go:build unix

package gateway

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestChoosingAPortKeepsTheCredentialFolderUserOnly(t *testing.T) {
	for _, existing := range []os.FileMode{0, 0o775} {
		t.Run(existing.String(), func(t *testing.T) {
			m := newAccessMachine(t)
			folder := filepath.Join(m.root, "webapp")
			if existing != 0 {
				if err := os.Mkdir(folder, existing); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(folder, existing); err != nil {
					t.Fatal(err)
				}
			}
			previous := syscall.Umask(0o002)
			_, _, _, _, err := WebappAccessPlan(context.Background(), "lan", "")
			syscall.Umask(previous)
			if err != nil {
				t.Fatal(err)
			}
			if st, err := os.Stat(folder); err != nil || st.Mode().Perm() != 0o700 {
				t.Fatalf("folder mode %v %v", st.Mode().Perm(), err)
			}
			if st, err := os.Stat(filepath.Join(folder, "port-claim.lock")); err != nil || st.Mode().Perm() != 0o600 {
				t.Fatalf("lock mode %v %v", st.Mode().Perm(), err)
			}
		})
	}
}
