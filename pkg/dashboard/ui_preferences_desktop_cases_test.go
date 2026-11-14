package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
	"github.com/nickheyer/icebath/pkg/desktop/storage"
)

// Store file the desktop storage service opens for this ICEBATH_HOME and home
func electronStorePath(home, hermesHome string) string {
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	root := backend.ResolveDesktopHermesHome(backend.HomeInputs{
		Home:     home,
		Env:      map[string]string{"ICEBATH_HOME": hermesHome},
		Platform: platform,
		DirectoryExists: func(p string) bool {
			st, err := os.Stat(p)
			return err == nil && st.IsDir()
		},
		ReadWindowsHome: func() string { return "" },
	})
	return storage.UIPreferencesPath(storage.UIPreferencesRoot(root))
}

// Store file the Webapp server resolves with ICEBATH_HOME and HOME set as given
func webappStorePath(t *testing.T, home, hermesHome string) string {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("ICEBATH_HOME", hermesHome)
	p, err := uiPreferencesStorePath(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNativeDesktopAndTheWebappServerResolveTheSameStoreFromAProfileHome(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "custom-root")
	profileHome := filepath.Join(root, "profiles", "coder")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	electronFile := electronStorePath(home, profileHome)
	pythonFile := webappStorePath(t, home, profileHome)
	if electronFile != filepath.Join(realPath(t, root), "ui-preferences.json") || pythonFile != electronFile {
		t.Fatalf("desktop %s webapp %s", electronFile, pythonFile)
	}
}

func TestASymlinkedProfileDirectoryStillResolvesItsRootTheSameStoreOnBothSides(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinked profile directories are a POSIX layout")
	}
	home := t.TempDir()
	root := filepath.Join(home, ".icebath")
	elsewhere := filepath.Join(home, "elsewhere", "coder")
	if err := os.MkdirAll(filepath.Join(root, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	profileHome := filepath.Join(root, "profiles", "coder")
	if err := os.Symlink(elsewhere, profileHome); err != nil {
		t.Fatal(err)
	}
	pythonFile := webappStorePath(t, home, profileHome)
	electronFile := electronStorePath(home, profileHome)
	if electronFile != filepath.Join(realPath(t, root), "ui-preferences.json") || pythonFile != electronFile {
		t.Fatalf("desktop %s webapp %s", electronFile, pythonFile)
	}
}

func TestACustomHomeInsideTheDefaultHomeResolvesTheSameStoreOnBothSides(t *testing.T) {
	home := t.TempDir()
	sandbox := filepath.Join(home, ".icebath", "sandbox")
	if err := os.MkdirAll(sandbox, 0o755); err != nil {
		t.Fatal(err)
	}
	electronFile := electronStorePath(home, sandbox)
	pythonFile := webappStorePath(t, home, sandbox)
	if electronFile != filepath.Join(realPath(t, sandbox), "ui-preferences.json") {
		t.Fatalf("desktop %s", electronFile)
	}
	if realPath(t, filepath.Dir(pythonFile)) != realPath(t, filepath.Dir(electronFile)) {
		t.Fatalf("desktop %s webapp %s", electronFile, pythonFile)
	}
}

// Child process body of the concurrent Webapp writer, a no-op unless the parent named the store
func TestUIPreferencesDesktopWebappWriterChild(t *testing.T) {
	path := os.Getenv("UI_PREFS_WEBAPP_PATH")
	if path == "" {
		return
	}
	for n := range 25 {
		if _, err := writePreferences(context.Background(), prefChanges(fmt.Sprintf("icebath.desktop.py%d", n), "web"), false, path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentNativeAndWebappWritersKeepEveryKeyTheOtherWrote(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	file := storage.UIPreferencesPath(root)
	cmd := exec.Command(os.Args[0], "-test.run=^TestUIPreferencesDesktopWebappWriterChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "UI_PREFS_WEBAPP_PATH="+file)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for n := range 25 {
		values := storage.NewObject()
		values.Set(fmt.Sprintf("icebath.desktop.native%d", n), "native")
		if _, err := storage.WriteUIPreferences(file, storage.UIPreferencesWrite{Values: values}); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("webapp writer: %v", err)
	}
	snap, err := storage.ReadUIPreferences(file)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Values.Len() != 50 || snap.Revision != 50 {
		t.Fatalf("keys %d revision %v", snap.Values.Len(), snap.Revision)
	}
	for n := range 25 {
		if v, _ := snap.Values.Get(fmt.Sprintf("icebath.desktop.native%d", n)); v != "native" {
			t.Fatalf("native%d is %v", n, v)
		}
		if v, _ := snap.Values.Get(fmt.Sprintf("icebath.desktop.py%d", n)); v != "web" {
			t.Fatalf("py%d is %v", n, v)
		}
	}
	if _, err := os.Stat(file + ".lock"); err == nil {
		t.Fatal("lock left behind")
	}
}

func TestNeitherWriterMergesOverAStoreItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("mode 0 does not hide a file from Windows or root")
	}
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	file := storage.UIPreferencesPath(home)
	values := storage.NewObject()
	values.Set("icebath-desktop-theme-v2", "nous")
	values.Set("icebath.desktop.keybinds", "{}")
	if _, err := storage.WriteUIPreferences(file, storage.UIPreferencesWrite{Values: values}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(file, 0o600)
	dark := storage.NewObject()
	dark.Set("icebath-desktop-mode-v1", "dark")
	if _, err := storage.WriteUIPreferences(file, storage.UIPreferencesWrite{Values: dark}); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("desktop write gave %v", err)
	}
	_, err = writePreferences(context.Background(), prefChanges("icebath-desktop-mode-v1", "dark"), false, "")
	var busy *preferencesBusy
	if !errors.As(err, &busy) || !strings.HasPrefix(busy.Error(), "preferences store is unreadable") {
		t.Fatalf("webapp write gave %v", err)
	}
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(file)
	if err != nil || string(after) != string(before) {
		t.Fatalf("store changed %q %v", after, err)
	}
}
