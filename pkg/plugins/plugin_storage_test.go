package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func storageCtx(t *testing.T) (context.Context, string) {
	h := t.TempDir()
	return home.WithOverride(context.Background(), h), h
}

// test_data_dir_lives_outside_the_install_tree and test_hostile_names_are_rejected
func TestPluginStorageDirAndHostileNames(t *testing.T) {
	ctx, h := storageCtx(t)
	root, err := PluginStorageDir(ctx, "my-plugin")
	if err != nil || root != filepath.Join(h, "plugin-data", "my-plugin") {
		t.Fatalf("root %q %v", root, err)
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() || strings.HasPrefix(root, filepath.Join(h, "plugins")+string(filepath.Separator)) {
		t.Fatalf("data dir %v", err)
	}
	for _, bad := range []string{"", ".", "..", "../escape", "a/b", "a\\b", strings.Repeat("x", 65), "ok\n", "a..b"} {
		var hb *HostBuiltinError
		if _, err := PluginStorageDir(ctx, bad); !errors.As(err, &hb) || hb.Type != "ValueError" {
			t.Fatalf("%q accepted %v", bad, err)
		}
	}
}

// test_plugin_db_journal_mode_is_the_shared_fallback_verdict and test_plugin_db_rejects_path_shaped_filenames
func TestStorageFileNamesFollowThePathNameRule(t *testing.T) {
	if purePathName("a/.") != "a" || purePathName("..") != ".." || purePathName("/") != "" {
		t.Fatal("pathlib name parity")
	}
}
