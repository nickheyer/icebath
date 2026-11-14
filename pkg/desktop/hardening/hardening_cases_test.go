package hardening

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

func modeOf(t *testing.T, p string) fs.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

type fakeFS struct {
	stat     FileStat
	chmodded []string
}

func (f *fakeFS) Lstat(string) (FileStat, error) { return f.stat, nil }
func (f *fakeFS) Chmod(p string, _ fs.FileMode) error {
	f.chmodded = append(f.chmodded, p)
	return nil
}
func (f *fakeFS) RemoveForce(string) error                    { return nil }
func (f *fakeFS) WriteFile(string, []byte, fs.FileMode) error { return nil }
func (f *fakeFS) Rename(string, string) error                 { return nil }

func TestWriteSecretFileAtomicCannotBeRedirectedThroughASymlinkPlantedAtTheTempPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "connection.json")
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	victimMode := modeOf(t, victim)
	if err := os.Symlink(victim, target+".tmp"); err != nil {
		if os.IsPermission(err) {
			t.Skip("creating a file symlink requires host permission")
		}
		t.Fatal(err)
	}
	if err := WriteSecretFileAtomic(target, []byte("tok-live-42"), SecretFileOptions{}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(victim); string(raw) != "original" || modeOf(t, victim) != victimMode {
		t.Fatalf("victim %q %v", raw, modeOf(t, victim))
	}
	if raw, _ := os.ReadFile(target); string(raw) != "tok-live-42" {
		t.Fatalf("target %q", raw)
	}
	if st, _ := os.Lstat(target); st.Mode()&fs.ModeSymlink != 0 {
		t.Fatal("target is the planted link")
	}
	if runtime.GOOS != "windows" && modeOf(t, target) != SecretFileMode {
		t.Fatalf("mode %v", modeOf(t, target))
	}
}

func TestTightenSecretFileModeTightensAPreExistingWorldReadableConfigInPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	target := filepath.Join(t.TempDir(), "connection.json")
	legacy := `{"mode":"remote","remote":{"url":"https://gw.example.com","authMode":"token","token":{"encoding":"safeStorage","value":"BLOB"}}}`
	if err := os.WriteFile(target, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if modeOf(t, target) != 0o644 || !TightenSecretFileMode(target, SecretFileOptions{}) || modeOf(t, target) != SecretFileMode {
		t.Fatalf("mode %v", modeOf(t, target))
	}
	if raw, _ := os.ReadFile(target); string(raw) != legacy {
		t.Fatalf("contents %q", raw)
	}
}

func TestTightenSecretFileModeLeavesANonSafeStorageTokenPayloadReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	target := filepath.Join(t.TempDir(), "connection.json")
	plain := `{"mode":"remote","remote":{"url":"https://gw.example.com","authMode":"token","token":{"encoding":"plain","value":"tok-live-42"}}}`
	if err := os.WriteFile(target, []byte(plain), 0o644); err != nil {
		t.Fatal(err)
	}
	TightenSecretFileMode(target, SecretFileOptions{})
	var doc struct {
		Remote struct{ Token struct{ Value string } }
	}
	raw, _ := os.ReadFile(target)
	if json.Unmarshal(raw, &doc) != nil || doc.Remote.Token.Value != "tok-live-42" || modeOf(t, target) != SecretFileMode {
		t.Fatalf("contents %q mode %v", raw, modeOf(t, target))
	}
}

func TestTightenSecretFileModeIsIdempotentAndNeverThrowsOnAnUnusablePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "connection.json")
	if err := WriteSecretFileAtomic(target, []byte("{}"), SecretFileOptions{}); err != nil {
		t.Fatal(err)
	}
	if !TightenSecretFileMode(target, SecretFileOptions{}) || !TightenSecretFileMode(target, SecretFileOptions{}) || modeOf(t, target) != SecretFileMode {
		t.Fatal("not idempotent")
	}
	if TightenSecretFileMode(filepath.Join(dir, "absent.json"), SecretFileOptions{}) {
		t.Fatal("absent path tightened")
	}
}

func TestTightenSecretFileModeNeverChangesASymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "connection.json")
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("not mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	victimMode := modeOf(t, victim)
	if err := os.Symlink(victim, target); err != nil {
		if os.IsPermission(err) {
			t.Skip("creating a file symlink requires host permission")
		}
		t.Fatal(err)
	}
	if TightenSecretFileMode(target, SecretFileOptions{}) != (runtime.GOOS == "windows") || modeOf(t, victim) != victimMode {
		t.Fatalf("victim mode %v", modeOf(t, victim))
	}
}

func TestTightenSecretFileModeOnlyTouchesARegularFileTheCurrentUserOwns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ownership")
	}
	uid, _ := processUID()
	var chmodded []string
	try := func(stat FileStat) bool {
		f := &fakeFS{stat: stat}
		ok := TightenSecretFileMode("/x/connection.json", SecretFileOptions{FS: f, Platform: "linux"})
		chmodded = append(chmodded, f.chmodded...)
		return ok
	}
	if try(FileStat{IsFile: false, Mode: 0o644, UID: 0, HasOwnerUID: true}) {
		t.Fatal("non file tightened")
	}
	if try(FileStat{IsFile: true, Mode: 0o644, UID: uid + 1, HasOwnerUID: true}) {
		t.Fatal("a file owned by another user is left alone")
	}
	if len(chmodded) != 0 {
		t.Fatalf("chmodded %v", chmodded)
	}
	if !try(FileStat{IsFile: true, Mode: 0o644, UID: uid, HasOwnerUID: true}) || !reflect.DeepEqual(chmodded, []string{"/x/connection.json"}) {
		t.Fatalf("chmodded %v", chmodded)
	}
}

func TestTightenSecretFileModeLeavesWindowsAloneRatherThanFlippingTheReadOnlyBit(t *testing.T) {
	uid, _ := processUID()
	f := &fakeFS{stat: FileStat{IsFile: true, Mode: 0o644, UID: uid, HasOwnerUID: true}}
	if !TightenSecretFileMode(`C:\Users\me\connection.json`, SecretFileOptions{FS: f, Platform: "win32"}) || len(f.chmodded) != 0 {
		t.Fatalf("chmodded %v", f.chmodded)
	}
	if !TightenSecretFileMode("/home/me/connection.json", SecretFileOptions{FS: f, Platform: "linux", UID: func() (int, bool) { return uid, true }}) || len(f.chmodded) != 1 {
		t.Fatalf("chmodded %v", f.chmodded)
	}
}
