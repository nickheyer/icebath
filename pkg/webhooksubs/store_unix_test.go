//go:build !windows

package webhooksubs

import (
	"os"
	"strings"
	"syscall"
	"testing"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func TestSaveCreatesSecretFileOwnerOnlyUnderPermissiveUmask(t *testing.T) {
	h := storeHome(t)
	old := syscall.Umask(0o022)
	err := Mutate(h, func(s *jsonutil.Object) error {
		s.Set("demo", route("secret", "TOPSECRET", "prompt", "x"))
		return nil
	})
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(Path(h))
	data, _ := os.ReadFile(Path(h))
	if st.Mode().Perm() != 0o600 || !strings.Contains(string(data), "TOPSECRET") {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}
