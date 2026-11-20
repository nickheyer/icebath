package app

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/selfupdate"
)

func TestDoctorAndSetupAreSilentOffMacos(t *testing.T) {
	w, _ := newQuickRun(t, "", nil)
	if err := os.MkdirAll(filepath.Join(w.tmp, "Library", "Application Support", "com.apple.TCC"), 0o755); err != nil {
		t.Fatal(err)
	}
	var doctor bytes.Buffer
	if err := selfupdate.CheckMacosFullDiskAccess(&doctor); err != nil {
		t.Fatal(err)
	}
	if err := printMacosFDATip(); err != nil {
		t.Fatal(err)
	}
	if doctor.Len() != 0 || w.out.Len() != 0 {
		t.Fatalf("doctor %q setup %q", doctor.String(), w.out.String())
	}
}
