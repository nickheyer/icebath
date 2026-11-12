package dashboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

func prefChanges(pairs ...any) *pyjson.Object { return pyjson.ObjectOf(pairs...) }

func TestAProfileHomeAndTheRootHomeResolveTheSameStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom-root")
	if err := os.MkdirAll(filepath.Join(root, "profiles", "coder"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	fromRoot, err := uiPreferencesStorePath(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "profiles", "coder"))
	fromProfile, err := uiPreferencesStorePath(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := pypath.Resolve(root)
	if fromRoot != fromProfile || fromRoot != filepath.Join(want, "ui-preferences.json") {
		t.Fatalf("root %s profile %s", fromRoot, fromProfile)
	}
}

func TestACustomHomeUnderTheNativeHomeIsItsOwnRoot(t *testing.T) {
	tmp := t.TempDir()
	sandbox := filepath.Join(tmp, ".icebath", "sandbox")
	if err := os.MkdirAll(sandbox, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", sandbox)
	got, err := uiPreferencesStorePath(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := pypath.Resolve(sandbox)
	if got != filepath.Join(resolved, "ui-preferences.json") {
		t.Fatalf("got %s", got)
	}
}

func TestTheSeedImportOnlyAddsKeysAndNeverCreatesAnEmptyStore(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ui-preferences.json")
	empty, err := writePreferences(ctx, prefChanges(), true, path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := writePreferences(ctx, prefChanges("icebath.desktop.sessionListDensity", "detailed"), true, path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := writePreferences(ctx, prefChanges("icebath.desktop.sessionListDensity", "compact",
		"icebath-desktop-mode-v1", "dark"), true, path)
	if err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(empty, pyjson.ObjectOf("exists", false, "revision", 0, "values", pyjson.NewObject())) {
		t.Fatalf("empty %v", empty)
	}
	if !pyjson.Equal(first.Vals["values"], prefChanges("icebath.desktop.sessionListDensity", "detailed")) {
		t.Fatalf("first %v", first)
	}
	wantSecond := prefChanges("icebath.desktop.sessionListDensity", "detailed", "icebath-desktop-mode-v1", "dark")
	if !pyjson.Equal(second.Vals["values"], wantSecond) || second.Vals["generation"] != first.Vals["generation"] {
		t.Fatalf("second %v first %v", second, first)
	}
	raw, _ := os.ReadFile(path)
	doc, _ := pyjson.LoadsBytes(raw)
	if !pyjson.Equal(doc.(*pyjson.Object).Vals["values"], wantSecond) {
		t.Fatalf("on disk %s", raw)
	}
}

func TestAnUnreadableOrCorruptStoreIsNeverMergedOver(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if runtime.GOOS == "windows" {
		t.Skip("platforms linux, macos")
	}
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "ui-preferences.json")
	kept, err := writePreferences(ctx, prefChanges("icebath-desktop-theme-v2", "nous", "icebath.desktop.keybinds", "{}"), false, path)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(path); err == nil {
		f.Close()
		_ = os.Chmod(path, 0o600)
		t.Skip("running as a user that reads mode-000 files (root)")
	}
	_, err = writePreferences(ctx, prefChanges("icebath-desktop-mode-v1", "dark"), false, path)
	_ = os.Chmod(path, 0o600)
	var busy *preferencesBusy
	if !errors.As(err, &busy) {
		t.Fatalf("err = %v", err)
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, before) {
		t.Fatal("store changed")
	}
	torn := before[:len(before)-5]
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := readPreferences(ctx, path)
	if err != nil || snap.Vals["exists"] != false {
		t.Fatalf("snap %v err %v", snap, err)
	}
	after, err := writePreferences(ctx, prefChanges("icebath-desktop-mode-v1", "dark"), false, path)
	if err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "ui-preferences.json.corrupt-*"))
	if len(backups) != 1 {
		t.Fatalf("backups %v", backups)
	}
	if b, _ := os.ReadFile(backups[0]); !bytes.Equal(b, torn) {
		t.Fatalf("backup %q", b)
	}
	if !pyjson.Equal(after.Vals["values"], prefChanges("icebath-desktop-mode-v1", "dark")) || after.Vals["generation"] == kept.Vals["generation"] {
		t.Fatalf("after %v kept %v", after, kept)
	}
}

// Child process body of the concurrent writer case, a no-op unless the parent set the key
func TestUIPreferencesWriterChild(t *testing.T) {
	key, path := os.Getenv("UI_PREFS_CHILD_KEY"), os.Getenv("UI_PREFS_CHILD_PATH")
	if key == "" {
		return
	}
	if _, err := writePreferences(context.Background(), prefChanges(key, "1"), false, path); err != nil {
		t.Fatal(err)
	}
}

func TestWritesMergePerKeyAndConcurrentWritersKeepEachOthersKeys(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ui-preferences.json")
	if _, err := writePreferences(ctx, prefChanges("icebath-desktop-mode-v1", "dark", "icebath.desktop.panesFlipped", "true"), false, path); err != nil {
		t.Fatal(err)
	}
	var keys []string
	var writers []*exec.Cmd
	for n := range 8 {
		key := fmt.Sprintf("icebath.desktop.k%d", n)
		keys = append(keys, key)
		cmd := exec.Command(os.Args[0], "-test.run=^TestUIPreferencesWriterChild$", "-test.count=1")
		cmd.Env = append(os.Environ(), "UI_PREFS_CHILD_KEY="+key, "UI_PREFS_CHILD_PATH="+path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		writers = append(writers, cmd)
	}
	for i, w := range writers {
		if err := w.Wait(); err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	after, err := writePreferences(ctx, prefChanges("icebath.desktop.panesFlipped", nil), false, path)
	if err != nil {
		t.Fatal(err)
	}
	want := prefChanges("icebath-desktop-mode-v1", "dark")
	for _, k := range keys {
		want.Set(k, "1")
	}
	if !pyjson.Equal(after.Vals["values"], want) || !pyjson.Equal(after.Vals["revision"], 2+len(keys)) {
		t.Fatalf("after %v", after)
	}
	if _, err := os.Stat(path + ".lock"); err == nil {
		t.Fatal("lock left behind")
	}
}

func TestOnlyDesktopStringKeysAreStored(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	for _, changes := range []*pyjson.Object{prefChanges("hermes.agent.secret", "x"), prefChanges("icebath.desktop.n", int64(3)), prefChanges("", "x")} {
		path := filepath.Join(t.TempDir(), "ui-preferences.json")
		_, err := writePreferences(context.Background(), changes, false, path)
		var pe *preferencesError
		if !errors.As(err, &pe) {
			t.Fatalf("%v: err = %v", changes.Keys, err)
		}
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("%v: store created", changes.Keys)
		}
	}
}
