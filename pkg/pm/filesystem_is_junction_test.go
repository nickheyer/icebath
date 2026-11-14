package pm

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMissingPathIsNotAJunction(t *testing.T) {
	tmp := t.TempDir()
	for _, path := range []string{filepath.Join(tmp, "absent"), filepath.Join(tmp, "absent", "deeper"), tmp} {
		if IsJunction(path) {
			t.Fatalf("%s reported as a junction", path)
		}
	}
}

func TestRealJunctionAndPlainDirectory(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("platforms windows")
	}
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target")
	if err := os.Mkdir(target, 0o777); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(tmp, "junction")
	tdlMklinkJunction(t, junction, target)
	defer os.Remove(junction)
	if !IsJunction(junction) {
		t.Fatal("junction not recognised")
	}
	if IsJunction(target) {
		t.Fatal("plain directory reported as a junction")
	}
}
