package pypath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

type notFound struct{}

func (notFound) Error() string        { return "gone" }
func (notFound) Is(target error) bool { return target == fs.ErrNotExist }

func TestIsOSError(t *testing.T) {
	_, statErr := os.Stat(filepath.Join(t.TempDir(), "missing"))
	_, lookErr := exec.LookPath("icebath-no-such-binary-anywhere")
	cases := []struct {
		err  error
		want bool
	}{
		{statErr, true},
		{fmt.Errorf("wrapped: %w", statErr), true},
		{&os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.EXDEV}, true},
		{os.NewSyscallError("flock", syscall.EWOULDBLOCK), true},
		{syscall.EACCES, true},
		{lookErr, true},
		{notFound{}, true},
		{fs.ErrExist, true},
		{fs.ErrPermission, true},
		{errors.New("plain"), false},
		{nil, false},
	}
	for i, c := range cases {
		if got := IsOSError(c.err); got != c.want {
			t.Errorf("case %d %v: got %v want %v", i, c.err, got, c.want)
		}
	}
}
