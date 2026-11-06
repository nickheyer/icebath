//go:build windows

package manage

import (
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"golang.org/x/sys/windows"
)

func TestCloneKeepsRealNtfsJunction(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def, external := homeWithLinkedSkill(t, tmp)
	if err := ntfsCreateJunction(filepath.Join(external, "foo"), filepath.Join(def, "skills", "foo")); err != nil {
		t.Fatal(err)
	}
	clone, err := CreateProfile(t.Context(), "clone", CreateOptions{CloneConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	foo := filepath.Join(clone, "skills", "foo")
	if pyfs.ReparseTag(foo) != windows.IO_REPARSE_TAG_MOUNT_POINT {
		t.Fatal("foo is not a junction")
	}
	got, _ := filepath.EvalSymlinks(foo)
	want, _ := filepath.EvalSymlinks(filepath.Join(external, "foo"))
	if got != want {
		t.Fatalf("foo resolves to %s, want %s", got, want)
	}
	if n := skillCandidateCount("foo", []string{filepath.Join(clone, "skills"), external}); n != 1 {
		t.Fatalf("foo has %d candidates", n)
	}
}
