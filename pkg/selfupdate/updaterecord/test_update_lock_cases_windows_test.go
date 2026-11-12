package updaterecord

import (
	"os"
	"testing"
)

func TestRepeatedWindowsProcessProbesBuildNoNewCtypesObjects(t *testing.T) {
	created, ok := windowsCreateFiletime(os.Getpid())
	parent, found, err := psutilParent(os.Getpid())
	if !ok || created == 0 || !found || parent == 0 || err != nil {
		t.Fatalf("created %d %v parent %d %v %v", created, ok, parent, found, err)
	}
	for range 20 {
		if again, ok := windowsCreateFiletime(os.Getpid()); !ok || again != created {
			t.Fatalf("creation time moved %d %d", again, created)
		}
		if again, found, err := psutilParent(os.Getpid()); !found || err != nil || again != parent {
			t.Fatalf("parent moved %d %d %v", again, parent, err)
		}
	}
}
