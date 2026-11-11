package plugins

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// Names of the manifests a scan of a user plugins root returns, in scan order
func scannedNames(root string) []string {
	names := []string{}
	for _, mf := range ScanDirectory(root, "user", nil) {
		names = append(names, mf.Name)
	}
	return names
}

func TestScanSkipsDunderDirsAndStillLoadsRealPlugin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	mv2Write(t, filepath.Join(root, "demo", "plugin.yaml"), "name: demo\nversion: 0.1.0\ndescription: x\n")
	for _, dir := range []string{filepath.Join(root, "demo", "__pycache__"), filepath.Join(root, "__pycache__"), filepath.Join(root, "__MACOSX__")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := scannedNames(root); !reflect.DeepEqual(got, []string{"demo"}) {
		t.Fatalf("found %v", got)
	}
}

func TestScanSurvivesIterdirOserror(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	saved := pluginDirRead
	pluginDirRead = func(p string) ([]os.DirEntry, error) {
		return nil, &fs.PathError{Op: "open", Path: p, Err: syscall.EACCES}
	}
	t.Cleanup(func() { pluginDirRead = saved })
	logs := captureLogs(t)
	if got := scannedNames(root); len(got) != 0 {
		t.Fatalf("found %v", got)
	}
	if !strings.Contains(logs.String(), "Skipping unreadable plugin directory "+root) {
		t.Fatalf("unreadable root not reported: %s", logs.String())
	}
}

func TestScanSkipsChildWhoseIsDirRaises(t *testing.T) {
	root := filepath.Join(t.TempDir(), "plugins")
	mv2Write(t, filepath.Join(root, "demo", "plugin.yaml"), "name: demo\nversion: 0.1.0\ndescription: x\n")
	spooky := filepath.Join(root, "spooky")
	if err := os.MkdirAll(spooky, 0o755); err != nil {
		t.Fatal(err)
	}
	saved := pluginDirStat
	pluginDirStat = func(p string) (os.FileInfo, error) {
		if p == spooky {
			return nil, &fs.PathError{Op: "stat", Path: p, Err: syscall.EACCES}
		}
		return saved(p)
	}
	t.Cleanup(func() { pluginDirStat = saved })
	if got := scannedNames(root); !reflect.DeepEqual(got, []string{"demo"}) {
		t.Fatalf("found %v", got)
	}
}
