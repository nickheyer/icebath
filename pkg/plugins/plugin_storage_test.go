package plugins

import (
	"context"
	"encoding/json"
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

const storageProbePlugin = `
import json, threading
from plugins.plugin_storage import plugin_data_dir, plugin_db
from plugins.plugin_utils import lazy_singleton, SingletonSlot
import tools.lazy_deps as lazy_deps

S = {"type": "object", "properties": {}}
built = []

@lazy_singleton
def client():
    built.append(1)
    return object()

def register(ctx):
    def probe(args, **kw):
        root = plugin_data_dir("probe-data")
        conn = plugin_db("probe-data")
        conn.execute("CREATE TABLE IF NOT EXISTS t (x)")
        conn.execute("INSERT INTO t VALUES (1)")
        conn.commit()
        mode = conn.execute("PRAGMA journal_mode").fetchone()[0]
        fk = conn.execute("PRAGMA foreign_keys").fetchone()[0]
        conn.close()
        errors = []
        for call in (lambda: plugin_db("probe-data", filename="../outside.db"), lambda: plugin_data_dir("../escape"),
                     lambda: lazy_deps.ensure("x"), lambda: lazy_deps.install_specs(["a==1"], dry_run=True)):
            try:
                call()
                errors.append(None)
            except Exception as exc:
                errors.append([type(exc).__name__, str(exc)])
        threads = [threading.Thread(target=client) for _ in range(8)]
        [t.start() for t in threads]
        [t.join() for t in threads]
        return json.dumps({"root": str(root), "mode": mode, "fk": fk, "errors": errors, "built": len(built)})
    ctx.register_tool(name="storage_probe", toolset="storageprobe", schema={"name": "storage_probe",
        "description": "d", "parameters": S}, handler=probe)
`

// A hosted plugin imports the storage, utils and lazy_deps SDK twins and gets the shared journal verdict
func TestLiveHostedPluginStorageTwins(t *testing.T) {
	installLiveServices(t)
	h, m := liveHome(t, liveInterpreters(t)["system"], map[string]string{"storageprobe": storageProbePlugin})
	if e := loadedError(m, "storageprobe"); e != "" {
		t.Fatalf("load error %q", e)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(dispatchLive(t, h, "storage_probe", map[string]any{})), &got); err != nil {
		t.Fatal(err)
	}
	want := "wal"
	if state.WALResetVulnerable(nil) {
		want = "delete"
	}
	errs, _ := got["errors"].([]any)
	if got["root"] != filepath.Join(h, "plugin-data", "probe-data") || got["mode"] != want || got["fk"] != float64(1) || got["built"] != float64(1) || len(errs) != 4 {
		t.Fatalf("probe %v", got)
	}
	wantErrs := []string{`["ValueError","invalid plugin db filename: '../outside.db'"]`, `["ValueError","invalid plugin name for storage: '../escape'"]`,
		`["ImportError","Dependencies are unknown to this old updater. Please relaunch Icebath."]`,
		`["ImportError","tools.lazy_deps.install_specs is retired; runtime dependency installation is unavailable."]`}
	for i, e := range errs {
		raw, _ := json.Marshal(e)
		if string(raw) != wantErrs[i] {
			t.Fatalf("error %d %s", i, raw)
		}
	}
}

// Ensure and InstallSpecs refuse with the retired installer ImportErrors outside an updater call stack
func TestLazyDepsRetiredInstaller(t *testing.T) {
	var hb *HostBuiltinError
	if err := Ensure("honcho", true); !errors.As(err, &hb) || hb.Type != "ImportError" || err.Error() != "Dependencies are unknown to this old updater. Please relaunch Icebath." {
		t.Fatalf("ensure %v", err)
	}
	if err := InstallSpecs(context.Background(), []string{"a==1"}, 300, nil, true); !errors.As(err, &hb) || hb.Type != "ImportError" ||
		err.Error() != "tools.lazy_deps.install_specs is retired; runtime dependency installation is unavailable." {
		t.Fatalf("install specs %v", err)
	}
}
