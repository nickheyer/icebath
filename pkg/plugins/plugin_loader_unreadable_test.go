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

// Two plugin children with __init__.py under a temp root, the first denied and the second readable
func unreadableRoot(t *testing.T) (root, denied, good string) {
	root = t.TempDir()
	denied, good = filepath.Join(root, "denied"), filepath.Join(root, "good")
	for _, dir := range []string{denied, good} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "__init__.py"), "")
	}
	return root, denied, good
}

// test_plugin_loader_unreadable.py test_iter_plugin_dirs_skips_unreadable_child over the Go loader
func TestIterPluginDirsSkipsUnreadableChild(t *testing.T) {
	root, denied, good := unreadableRoot(t)
	saved := pluginDirStat
	pluginDirStat = func(p string) (os.FileInfo, error) {
		if p == denied || filepath.Dir(p) == denied {
			return nil, &fs.PathError{Op: "stat", Path: p, Err: syscall.EACCES}
		}
		return saved(p)
	}
	t.Cleanup(func() { pluginDirStat = saved })
	logs := captureLogs(t)
	got, err := IterPluginDirs(root)
	if err != nil || !reflect.DeepEqual(got, []string{good}) {
		t.Fatalf("IterPluginDirs = %v, %v", got, err)
	}
	if want := "Skipping unreadable plugin directory " + denied + ": [Errno 13] Permission denied: '" + denied + "'"; !strings.Contains(logs.String(), want) {
		t.Fatalf("warning %q missing from %s", want, logs)
	}
	pluginDirStat = func(p string) (os.FileInfo, error) {
		if p == root {
			return nil, &fs.PathError{Op: "stat", Path: p, Err: syscall.EACCES}
		}
		return saved(p)
	}
	if got, err := IterPluginDirs(root); err == nil || got != nil {
		t.Fatalf("denied root = %v, %v", got, err)
	}
	if got, err := IterPluginDirs(filepath.Join(root, "missing")); err != nil || got != nil {
		t.Fatalf("missing root = %v, %v", got, err)
	}
}

const unreadableDriver = `
import io, logging, sys
from pathlib import Path
from plugins import plugin_loader

tmp = Path(sys.argv[1])
denied = tmp / "denied"
real_exists, real_is_dir = Path.exists, Path.is_dir

def refuse(method):
    def probe(self, *args, **kwargs):
        if self.parent == denied or self == denied:
            raise PermissionError(13, "Permission denied", str(self))
        return method(self, *args, **kwargs)
    return probe

Path.exists, Path.is_dir = refuse(real_exists), refuse(real_is_dir)
stream = io.StringIO()
handler = logging.StreamHandler(stream)
logging.getLogger(plugin_loader._log.name).addHandler(handler)
got = plugin_loader.iter_plugin_dirs(tmp)
assert got == [tmp / "good"], got
want = "Skipping unreadable plugin directory %s: [Errno 13] Permission denied: '%s'" % (denied, denied)
assert want in stream.getvalue(), stream.getvalue()
print("ok")
`
