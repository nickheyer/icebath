package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const uiPrefsChildEnv = "ICEBATH_UI_PREFS_CHILD"

func prefValues(o *pyjson.Object) map[string]any {
	out := map[string]any{}
	for _, k := range o.Keys {
		out[k] = o.Vals[k]
	}
	return out
}

func storedPrefValues(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := pyjson.Loads(string(data))
	if err != nil {
		t.Fatal(err)
	}
	values, _ := doc.(*pyjson.Object).Get("values")
	return prefValues(values.(*pyjson.Object))
}

func TestAProfileHomeAndTheRootHomeResolveTheSameStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom-root")
	os.MkdirAll(filepath.Join(root, "profiles", "coder"), 0o755)
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	t.Setenv("ICEBATH_HOME", root)
	fromRoot := UIPreferencesStorePath(context.Background(), "")
	t.Setenv("ICEBATH_HOME", filepath.Join(root, "profiles", "coder"))
	fromProfile := UIPreferencesStorePath(context.Background(), "")
	if want := filepath.Join(resolvedRoot, UIPreferencesStoreName); fromRoot != want || fromProfile != want {
		t.Fatal(fromRoot, fromProfile)
	}
}

func TestACustomHomeUnderTheNativeHomeIsItsOwnRoot(t *testing.T) {
	tmp := t.TempDir()
	sandbox := filepath.Join(tmp, ".icebath", "sandbox")
	os.MkdirAll(sandbox, 0o755)
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", sandbox)
	resolved, _ := filepath.EvalSymlinks(sandbox)
	if got := UIPreferencesStorePath(context.Background(), ""); got != filepath.Join(resolved, UIPreferencesStoreName) {
		t.Fatal(got)
	}
}

func TestTheSeedImportOnlyAddsKeysAndNeverCreatesAnEmptyStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), UIPreferencesStoreName)
	empty, err := WritePreferences(ctx, nil, true, path)
	if err != nil {
		t.Fatal(err)
	}
	first, err := WritePreferences(ctx, []UIPreferenceChange{{"icebath.desktop.sessionListDensity", "detailed"}}, true, path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := WritePreferences(ctx, []UIPreferenceChange{{"icebath.desktop.sessionListDensity", "compact"}, {"icebath-desktop-mode-v1", "dark"}}, true, path)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := pyjson.Dumps(empty.Dict()); text != `{"exists": false, "revision": 0, "values": {}}` {
		t.Fatal(text)
	}
	if !reflect.DeepEqual(prefValues(first.Values), map[string]any{"icebath.desktop.sessionListDensity": "detailed"}) {
		t.Fatal(first.Values)
	}
	want := map[string]any{"icebath.desktop.sessionListDensity": "detailed", "icebath-desktop-mode-v1": "dark"}
	if !reflect.DeepEqual(prefValues(second.Values), want) || second.Generation != first.Generation {
		t.Fatal(second)
	}
	if got := storedPrefValues(t, path); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestAnUnreadableOrCorruptStoreIsNeverMergedOver(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode 000 files are a posix permission")
	}
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, UIPreferencesStoreName)
	kept, err := WritePreferences(ctx, []UIPreferenceChange{{"icebath-desktop-theme-v2", "nous"}, {"icebath.desktop.keybinds", "{}"}}, false, path)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	os.Chmod(path, 0)
	if f, err := os.Open(path); err == nil {
		f.Close()
		os.Chmod(path, 0o600)
		t.Skip("running as a user that reads mode-000 files (root)")
	}
	_, err = WritePreferences(ctx, []UIPreferenceChange{{"icebath-desktop-mode-v1", "dark"}}, false, path)
	os.Chmod(path, 0o600)
	var busy *PreferencesBusy
	if !errors.As(err, &busy) {
		t.Fatalf("unreadable store must refuse, got %v", err)
	}
	if now, _ := os.ReadFile(path); string(now) != string(before) {
		t.Fatal("unreadable store changed")
	}
	torn := before[:len(before)-5]
	os.WriteFile(path, torn, 0o600)
	if snap, err := ReadPreferences(ctx, path); err != nil || snap.Exists {
		t.Fatal(snap, err)
	}
	after, err := WritePreferences(ctx, []UIPreferenceChange{{"icebath-desktop-mode-v1", "dark"}}, false, path)
	if err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, UIPreferencesStoreName+".corrupt-*"))
	if len(backups) != 1 {
		t.Fatal(backups)
	}
	if data, _ := os.ReadFile(backups[0]); string(data) != string(torn) {
		t.Fatal(string(data))
	}
	if !reflect.DeepEqual(prefValues(after.Values), map[string]any{"icebath-desktop-mode-v1": "dark"}) || after.Generation == kept.Generation {
		t.Fatal(after)
	}
}

// Runs a child preferences writer when the concurrent writers test spawns this binary
func TestMain(m *testing.M) {
	if spec := os.Getenv(uiPrefsChildEnv); spec != "" {
		dir, key := filepath.Dir(spec), filepath.Base(spec)
		if _, err := WritePreferences(context.Background(), []UIPreferenceChange{{key, "1"}}, false, filepath.Join(dir, UIPreferencesStoreName)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestWritesMergePerKeyAndConcurrentWritersKeepEachOthersKeys(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, UIPreferencesStoreName)
	if _, err := WritePreferences(ctx, []UIPreferenceChange{{"icebath-desktop-mode-v1", "dark"}, {"icebath.desktop.panesFlipped", "true"}}, false, path); err != nil {
		t.Fatal(err)
	}
	var keys []string
	var procs []*exec.Cmd
	for n := 0; n < 8; n++ {
		key := fmt.Sprintf("icebath.desktop.k%d", n)
		keys = append(keys, key)
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), uiPrefsChildEnv+"="+filepath.Join(dir, key))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, cmd)
	}
	for _, cmd := range procs {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	after, err := WritePreferences(ctx, []UIPreferenceChange{{"icebath.desktop.panesFlipped", nil}}, false, path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"icebath-desktop-mode-v1": "dark"}
	for _, k := range keys {
		want[k] = "1"
	}
	if !reflect.DeepEqual(prefValues(after.Values), want) || after.Revision != int64(2+len(keys)) {
		t.Fatal(prefValues(after.Values), after.Revision)
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("lock left behind")
	}
}

func TestOnlyDesktopStringKeysAreStored(t *testing.T) {
	for _, change := range []UIPreferenceChange{{"icebath.agent.secret", "x"}, {"icebath.desktop.n", 3}, {"", "x"}} {
		path := filepath.Join(t.TempDir(), UIPreferencesStoreName)
		_, err := WritePreferences(context.Background(), []UIPreferenceChange{change}, false, path)
		var refused *PreferencesError
		if !errors.As(err, &refused) {
			t.Fatalf("%v: %v", change, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("refused write created the store")
		}
	}
}
