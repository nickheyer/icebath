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
	"time"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
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
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
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

func TestAnUndecodableStoreRaisesThePythonDecodeError(t *testing.T) {
	path := filepath.Join(t.TempDir(), UIPreferencesStoreName)
	os.WriteFile(path, []byte("\xef\xbb\xbf{\"values\": \"\xff\"}"), 0o600)
	_, err := ReadPreferences(context.Background(), path)
	var busy *PreferencesBusy
	if err == nil || errors.As(err, &busy) || err.Error() != "'utf-8' codec can't decode byte 0xff in position 12: invalid start byte" {
		t.Fatalf("read %v", err)
	}
	if _, err := WritePreferences(context.Background(), []UIPreferenceChange{{Key: UIPreferenceKeyPrefixes[0] + "theme", Value: "dark"}}, false, path); err == nil ||
		err.Error() != "'utf-8' codec can't decode byte 0xff in position 12: invalid start byte" {
		t.Fatalf("write %v", err)
	}
	if _, err := os.Stat(path + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the lock is released when the read raises, stat %v", err)
	}
}

func TestACorruptStoreIsCopiedAsideWithItsModeAndBothTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), UIPreferencesStoreName)
	os.WriteFile(path, []byte("{not json"), 0o640)
	accessed, modified := time.Now().Add(time.Hour).Truncate(time.Second), time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, accessed, modified); err != nil {
		t.Fatal(err)
	}
	key := UIPreferenceKeyPrefixes[0] + "theme"
	if _, err := WritePreferences(context.Background(), []UIPreferenceChange{{Key: key, Value: "dark"}}, false, path); err != nil {
		t.Fatal(err)
	}
	asides, _ := filepath.Glob(path + ".corrupt-*")
	if len(asides) != 1 {
		t.Fatalf("asides %v", asides)
	}
	data, _ := os.ReadFile(asides[0])
	fi, err := os.Stat(asides[0])
	if err != nil || string(data) != "{not json" || !fi.ModTime().Equal(modified) {
		t.Fatalf("aside %q %v %v", data, fi, err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640 {
		t.Fatalf("aside mode %v", fi.Mode().Perm())
	}
	if got := fileAccessTime(t, asides[0]); !got.Equal(accessed) {
		t.Fatalf("aside access time %v, the source had %v", got, accessed)
	}
	if !reflect.DeepEqual(storedPrefValues(t, path), map[string]any{key: "dark"}) {
		t.Fatal("the store was replaced after the copy")
	}
}

func TestStaleLockChecksFollowThePythonErrorClasses(t *testing.T) {
	dir := t.TempDir()
	if err := breakPreferencesLockIfStale(filepath.Join(dir, "absent.lock")); err != nil {
		t.Fatalf("a missing lock is no failure, got %v", err)
	}
	blocker := filepath.Join(dir, "file")
	os.WriteFile(blocker, nil, 0o600)
	if err := breakPreferencesLockIfStale(filepath.Join(blocker, "x.lock")); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a stat failure other than a missing lock is returned, got %v", err)
	}
	old := time.Now().Add(-2 * uiPreferencesLockStale)
	stale := filepath.Join(dir, "stale.lock")
	os.MkdirAll(filepath.Join(stale, "owner"), 0o700)
	os.Chtimes(stale, old, old)
	if err := breakPreferencesLockIfStale(stale); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(stale, "owner")); err != nil || !fi.IsDir() {
		t.Fatalf("an owner that is a directory cannot be unlinked, so the lock stays: %v", err)
	}
	dead := filepath.Join(dir, "dead.lock")
	os.MkdirAll(dead, 0o700)
	os.WriteFile(filepath.Join(dead, "owner"), []byte("1-aa"), 0o600)
	os.Chtimes(dead, old, old)
	if err := breakPreferencesLockIfStale(dead); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a dead writer's lock is removed, stat %v", err)
	}
	fresh := filepath.Join(dir, "fresh.lock")
	os.MkdirAll(fresh, 0o700)
	os.WriteFile(filepath.Join(fresh, "owner"), []byte("2-bb"), 0o600)
	if err := breakPreferencesLockIfStale(fresh); err != nil {
		t.Fatal(err)
	}
	if owns, err := ownsPreferencesLock(fresh, "2-bb"); !owns || err != nil {
		t.Fatalf("a fresh lock stays with its owner, got %v %v", owns, err)
	}
	if owns, err := ownsPreferencesLock(fresh, "other"); owns || err != nil {
		t.Fatalf("another token does not own it, got %v %v", owns, err)
	}
	os.WriteFile(filepath.Join(fresh, "owner"), []byte("\xff"), 0o600)
	if owns, err := ownsPreferencesLock(fresh, "2-bb"); owns || err == nil ||
		err.Error() != "'utf-8' codec can't decode byte 0xff in position 0: invalid start byte" {
		t.Fatalf("an undecodable owner file raises, got %v %v", owns, err)
	}
	if owns, err := ownsPreferencesLock(filepath.Join(dir, "absent.lock"), "x"); owns || err != nil {
		t.Fatalf("an unreadable owner file is not ours, got %v %v", owns, err)
	}
}

func TestStorePathKeepsPathlibSemanticsForProfilesAndLinks(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real-root")
	os.MkdirAll(filepath.Join(real, "Profiles", "coder"), 0o755)
	t.Setenv("ICEBATH_HOME", filepath.Join(real, "Profiles", "coder"))
	resolved, _ := filepath.EvalSymlinks(real)
	if got := UIPreferencesStorePath(context.Background(), ""); got != filepath.Join(resolved, UIPreferencesStoreName) {
		t.Fatalf("a profiles directory in another case still names its root, got %s", got)
	}
	if got := UIPreferencesStorePath(context.Background(), filepath.FromSlash("given/../root")); got != filepath.FromSlash("given/../root/"+UIPreferencesStoreName) {
		t.Fatalf("a given root is joined as it stands, got %s", got)
	}
}
