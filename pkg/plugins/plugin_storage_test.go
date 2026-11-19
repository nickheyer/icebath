package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
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
func TestPluginDBJournalModeAndFilenames(t *testing.T) {
	ctx, h := storageCtx(t)
	conn, err := PluginDB(ctx, "board", "data.db")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"CREATE TABLE t (x)", "INSERT INTO t VALUES (1)"} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var mode string
	var fk int
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	want := "wal"
	if state.WALResetVulnerable(nil) {
		want = "delete"
	}
	_ = conn.Close()
	if mode != want || fk != 1 {
		t.Fatalf("journal %q foreign keys %d", mode, fk)
	}
	if _, err := os.Stat(filepath.Join(h, "plugin-data", "board", "data.db")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../outside.db", "", ".", "sub/x.db", "x.db/"} {
		if _, err := PluginDB(ctx, "board", bad); err == nil || !strings.HasPrefix(err.Error(), "invalid plugin db filename: ") {
			t.Fatalf("%q accepted %v", bad, err)
		}
	}
	if purePathName("a/.") != "a" || purePathName("..") != ".." || purePathName("/") != "" {
		t.Fatal("pathlib name parity")
	}
}
