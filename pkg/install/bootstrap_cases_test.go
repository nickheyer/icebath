package install

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Fake installed desktop at the platform's release layout, returning the app bundle or executable
func makeReleaseTree(t *testing.T, installRoot string) string {
	t.Helper()
	release := DesktopReleaseDir(installRoot)
	switch runtime.GOOS {
	case "darwin":
		macos := filepath.Join(release, "mac-arm64", "Icebath.app", "Contents", "MacOS")
		os.MkdirAll(macos, 0o755)
		os.WriteFile(filepath.Join(macos, "Icebath"), []byte("#!/bin/sh\n"), 0o755)
		return filepath.Dir(filepath.Dir(macos))
	case "windows":
		dir := filepath.Join(release, "win-unpacked")
		os.MkdirAll(dir, 0o755)
		exe := filepath.Join(dir, "Icebath.exe")
		os.WriteFile(exe, []byte("desktop"), 0o755)
		return exe
	}
	dir := filepath.Join(release, "linux-unpacked")
	os.MkdirAll(dir, 0o755)
	exe := filepath.Join(dir, "icebath")
	os.WriteFile(exe, []byte("desktop"), 0o755)
	return exe
}

func TestResolveDesktopAppFindsBuiltBundle(t *testing.T) {
	root := t.TempDir()
	expected := makeReleaseTree(t, root)
	resolved := ResolveDesktopApp(root)
	if resolved != expected {
		t.Fatalf("resolved %q want %q", resolved, expected)
	}
	if runtime.GOOS == "darwin" && filepath.Ext(resolved) != ".app" {
		t.Fatal("relaunch target must be a .app bundle on macOS")
	}
}

func TestResolveDesktopExeFindsArm64LinuxBuild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux release layout")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "apps/desktop/release/linux-arm64-unpacked")
	os.MkdirAll(dir, 0o755)
	exe := filepath.Join(dir, "icebath")
	os.WriteFile(exe, []byte("desktop"), 0o755)
	if got := ResolveDesktopExe(root); got != exe {
		t.Fatalf("got %q want %q", got, exe)
	}
}

func TestResolveDesktopAppIsNoneWithoutABuild(t *testing.T) {
	if got := ResolveDesktopApp(t.TempDir()); got != "" {
		t.Fatalf("resolved %q with nothing installed", got)
	}
}

func TestBootstrapCompleteMarkerUsesDesktopCompatibleSchema(t *testing.T) {
	root := t.TempDir()
	marker, err := WriteBootstrapMarker(root, Pin{Commit: strPtr("abcdef1234567890"), Branch: strPtr("main")})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(root, BootstrapMarkerName))
	if string(raw) != string(marker) {
		t.Fatalf("returned marker differs from disk:\n%s\n%s", marker, raw)
	}
	var disk map[string]any
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if disk["schemaVersion"] != float64(1) || disk["pinnedCommit"] != "abcdef1234567890" || disk["pinnedBranch"] != "main" {
		t.Fatalf("marker %v", disk)
	}
	if _, ok := disk["completedAtUnix"]; ok {
		t.Fatal("receipt must use the shared completedAt field")
	}
	if got := ISO8601UTC(time.UnixMilli(1_709_251_199_123)); got != "2024-02-29T23:59:59.123Z" {
		t.Fatalf("iso8601 %q", got)
	}
	completed, _ := disk["completedAt"].(string)
	if len(completed) != 24 || !strings.HasSuffix(completed, "Z") {
		t.Fatalf("completedAt must be ISO-8601 UTC: %q", completed)
	}
}

func TestBranchPinMarkerResolvesCommitFromThePriorReceipt(t *testing.T) {
	root := t.TempDir()
	head := "0123456789abcdef0123456789abcdef01234567"
	receipt := "\ufeff{\"schemaVersion\":1,\"pinnedCommit\":\"" + head + "\",\"pinnedBranch\":\"main\",\"completedAt\":\"2026-01-01T00:00:00.000Z\"}"
	os.WriteFile(filepath.Join(root, BootstrapMarkerName), []byte(receipt), 0o644)
	marker, err := WriteBootstrapMarker(root, Pin{Branch: strPtr("main")})
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]any
	json.Unmarshal(marker, &disk)
	if disk["pinnedCommit"] != head {
		t.Fatalf("pinnedCommit %v want %s", disk["pinnedCommit"], head)
	}
}

func TestBootstrapCompleteMarkerIsPublishedAtomically(t *testing.T) {
	root := t.TempDir()
	makeReleaseTree(t, root)
	if _, err := WriteBootstrapMarker(root, Pin{Commit: strPtr("abcdef1234567890"), Branch: strPtr("main")}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(root, BootstrapMarkerName)); err != nil || !st.Mode().IsRegular() {
		t.Fatal("final marker must exist after atomic publish")
	}
	if _, err := os.Stat(filepath.Join(root, BootstrapMarkerName+".tmp")); err == nil {
		t.Fatal("temp sibling must not remain after atomic publish")
	}
	if !IsInstalled(root) {
		t.Fatal("atomically published marker must enable the installer fast path")
	}
}

func TestIsInstalledTreatsMarkerExistenceAsSufficient(t *testing.T) {
	root := t.TempDir()
	makeReleaseTree(t, root)
	os.WriteFile(filepath.Join(root, BootstrapMarkerName), nil, 0o644)
	if !IsInstalled(root) {
		t.Fatal("empty marker content still counts as installed")
	}
}

func TestMarkerWriteFailureLeavesNoFinalMarker(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "not-a-dir")
	os.WriteFile(notADir, []byte("not a directory"), 0o644)
	_, err := WriteBootstrapMarker(notADir, Pin{Commit: strPtr("abcdef1234567890"), Branch: strPtr("main")})
	if err == nil || !strings.Contains(err.Error(), "bootstrap marker") {
		t.Fatalf("error should mention the marker path: %v", err)
	}
	for _, name := range []string{BootstrapMarkerName, BootstrapMarkerName + ".tmp"} {
		if _, err := os.Stat(filepath.Join(notADir, name)); err == nil {
			t.Fatalf("%s left behind", name)
		}
	}
}

// One scripted install for the UI against a loopback release, asserting the event sequence and the installed layout
func TestBootstrapInstallsAReleaseEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix layout")
	}
	tag := "v1.4.2"
	goos, goarch := runtime.GOOS, runtime.GOARCH
	platformDir := DesktopPlatformDir(goos, goarch)
	exeName := platformDir + "/icebath"
	if goos == "darwin" {
		exeName = "Icebath.app/Contents/MacOS/Icebath"
	}
	assets := map[string][]byte{
		BinaryAssetName(goos, goarch):  fixtureBinary(t, tag),
		DesktopAssetName(goos, goarch): desktopArchive(t, strings.TrimPrefix(exeName, platformDir+"/")),
	}
	f := newReleaseFixture(t, []fixtureRelease{{Tag: tag, Target: "main", Assets: assets, Published: "2026-11-01T00:00:00Z"}}, nil)
	homeDir, userHome := isolatedInstall(t, f.repo)
	var events []Event
	installRoot, err := Bootstrap(context.Background(), BootstrapArgs{}, func(e Event) { events = append(events, e) })
	if err != nil {
		for _, e := range events {
			data, _ := json.Marshal(e)
			t.Log(string(data))
		}
		t.Fatal(err)
	}
	if installRoot != filepath.Join(homeDir, InstallDirName) {
		t.Fatalf("install root %q", installRoot)
	}
	var seq []string
	for _, e := range events {
		switch e.Type {
		case EventManifest:
			seq = append(seq, "manifest")
		case EventStage:
			seq = append(seq, e.Name+":"+string(e.State))
		case EventComplete, EventFailed:
			seq = append(seq, string(e.Type))
		}
	}
	want := "manifest prerequisites:running prerequisites:succeeded repository:running repository:succeeded config:running config:succeeded products:running products:succeeded setup:running setup:skipped gateway:running gateway:skipped complete:running complete:succeeded complete"
	if strings.Join(seq, " ") != want {
		t.Fatalf("event sequence\n got %s\nwant %s", strings.Join(seq, " "), want)
	}
	for _, p := range []string{
		InstallBinaryPath(installRoot),
		filepath.Join(installRoot, ".icebath", "bin", "icebath"),
		filepath.Join(userHome, ".local", "bin", "icebath"),
		filepath.Join(DesktopReleaseDir(installRoot), platformDir),
		filepath.Join(installRoot, BootstrapMarkerName),
		filepath.Join(homeDir, ".env"),
		filepath.Join(homeDir, "skills"),
	} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("missing %s: %v", p, err)
		}
	}
	if ResolveDesktopExe(installRoot) == "" {
		t.Fatal("desktop app not installed")
	}
	if got := InstalledRelease(context.Background(), InstallBinaryPath(installRoot)).Tag; got != tag {
		t.Fatalf("installed binary names %q", got)
	}
	rc, _ := os.ReadFile(filepath.Join(userHome, ".bashrc"))
	if !strings.Contains(string(rc), ShellPathMarker) {
		t.Fatal("shell PATH line not wired")
	}
	last := events[len(events)-1]
	if last.Type != EventComplete || !json.Valid(last.Marker) {
		t.Fatalf("last event %+v", last)
	}
}

// A digest mismatch fails the repository stage with the failure frame and a failed event
func TestBootstrapRefusesABinaryWhoseChecksumDisagrees(t *testing.T) {
	goos, goarch := runtime.GOOS, runtime.GOARCH
	good := []byte("release bytes")
	f := newReleaseFixture(t, []fixtureRelease{{Tag: "v1.0.0", Target: "main", Assets: map[string][]byte{BinaryAssetName(goos, goarch): good}, Published: "2026-11-01T00:00:00Z"}}, nil)
	isolatedInstall(t, f.repo)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/"+BinaryAssetName(goos, goarch)) {
			w.Write([]byte("tampered bytes"))
			return
		}
		f.serve(w, r)
	})
	var events []Event
	_, err := Bootstrap(context.Background(), BootstrapArgs{}, func(e Event) { events = append(events, e) })
	if err == nil {
		t.Fatal("tampered binary installed")
	}
	last := events[len(events)-1]
	if last.Type != EventFailed || last.Stage == nil || *last.Stage != "repository" {
		t.Fatalf("last event %+v", last)
	}
}

// An installer run from an AppImage stages the AppImage file, the self contained artefact
func TestCopySelfToHomeStagesTheAppImageItRunsFrom(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	image := filepath.Join(t.TempDir(), "icebath-setup-x86_64.AppImage")
	os.WriteFile(image, []byte("appimage bytes"), 0o755)
	t.Setenv("APPIMAGE", image)
	if err := CopySelfToHome(); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(InstallerDest()); string(data) != "appimage bytes" {
		t.Fatalf("staged %q", data)
	}
}
