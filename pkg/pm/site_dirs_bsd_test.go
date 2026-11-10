//go:build darwin || freebsd

package pm

import (
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSiteDirSkipsAPthFileFlaggedHidden(t *testing.T) {
	base := resolvePath(t.TempDir())
	sdMkdirs(t, base, "site", "shown", "hidden")
	writeFile(t, filepath.Join(base, "site", "a.pth"), []byte(filepath.Join(base, "hidden")+"\n"), 0o666)
	writeFile(t, filepath.Join(base, "site", "b.pth"), []byte(filepath.Join(base, "shown")+"\n"), 0o666)
	if err := unix.Chflags(filepath.Join(base, "site", "a.pth"), 0x00008000); err != nil {
		t.Fatal(err)
	}
	got, err := addSiteDir(nil, filepath.Join(base, "site"), nil)
	if err != nil || !reflect.DeepEqual(sdRelative(base, got), []string{"/site", "/shown"}) {
		t.Fatalf("import path %v err %v", sdRelative(base, got), err)
	}
}
