package backup

import (
	"context"
	"path/filepath"
	"testing"
)

// The exported page restore refuses a source that fails its integrity check like the package one
func TestSafeRestoreDBRefusesAnInvalidSource(t *testing.T) {
	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "snap.db"), filepath.Join(tmp, "projects.db")
	writeText(t, src, "not a database at all")
	makeMarkerDB(t, dst, "live-old")
	if ok, err := SafeRestoreDB(context.Background(), src, dst); ok || err != nil {
		t.Fatalf("restore %v %v", ok, err)
	}
	if readMarker(t, dst) != "live-old" {
		t.Fatal("destination touched")
	}
}
