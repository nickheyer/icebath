package proc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExeNamesThisProcessAndRefusesAnAbsentPid(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Exe(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	same := func(a, b string) bool {
		ra, errA := filepath.EvalSymlinks(a)
		rb, errB := filepath.EvalSymlinks(b)
		return errA == nil && errB == nil && ra == rb
	}
	if !same(got, self) {
		t.Fatalf("Exe(self) = %q, want %q", got, self)
	}
	if _, err := Exe(1 << 30); err == nil {
		t.Fatal("Exe of an absent pid reported no error")
	}
}
