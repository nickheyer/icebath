package pm

import (
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func TestSiteDirSkipsAPthFileWindowsMarksHidden(t *testing.T) {
	base := resolvePath(t.TempDir())
	sdMkdirs(t, base, "site", "shown", "hidden")
	writeFile(t, filepath.Join(base, "site", "a.pth"), []byte(filepath.Join(base, "hidden")+"\n"), 0o666)
	writeFile(t, filepath.Join(base, "site", "b.pth"), []byte(filepath.Join(base, "shown")+"\n"), 0o666)
	name, err := syscall.UTF16PtrFromString(filepath.Join(base, "site", "a.pth"))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetFileAttributes(name, syscall.FILE_ATTRIBUTE_HIDDEN); err != nil {
		t.Fatal(err)
	}
	got, err := addSiteDir(nil, filepath.Join(base, "site"), nil)
	if err != nil || !reflect.DeepEqual(sdRelative(base, got), []string{`\site`, `\shown`}) {
		t.Fatalf("import path %v err %v", sdRelative(base, got), err)
	}
}
