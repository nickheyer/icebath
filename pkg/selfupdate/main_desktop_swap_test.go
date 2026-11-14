package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Rename refusal standing in for a Windows sharing violation
type mdsDenied struct{ text string }

func (e mdsDenied) Error() string        { return e.text }
func (e mdsDenied) Is(target error) bool { return target == fs.ErrPermission }

func mdsPackagedExeRel() string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join("mac-arm64", "Icebath.app", "Contents", "MacOS", "Icebath")
	case "windows":
		return filepath.Join("win-unpacked", "Icebath.exe")
	}
	return filepath.Join("linux-unpacked", "icebath")
}

// Staged app over a live one with the stop and the backoff sleep replaced
func mdsStagedOverLive(t *testing.T) (string, string, string, *[]time.Duration) {
	desktopDir := filepath.Join(t.TempDir(), "apps", "desktop")
	liveExe := filepath.Join(desktopDir, "release", mdsPackagedExeRel())
	sbWrite(t, liveExe, "old")
	staging := desktopStagingDir(desktopDir)
	sbWrite(t, filepath.Join(staging, mdsPackagedExeRel()), "new")
	prevStop, prevSleep, prevRename := stopDesktopProcessesLockingBuild, desktopSleep, desktopRename
	stopDesktopProcessesLockingBuild = func(string, bool) []int { return nil }
	slept := &[]time.Duration{}
	desktopSleep = func(d time.Duration) { *slept = append(*slept, d) }
	t.Cleanup(func() {
		stopDesktopProcessesLockingBuild, desktopSleep, desktopRename = prevStop, prevSleep, prevRename
	})
	return desktopDir, staging, liveExe, slept
}

// Captures warnings of the default logger for one test
func mdsLogs(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestSwapRetriesTransientPermissionErrorThenPromotes(t *testing.T) {
	desktopDir, staging, liveExe, slept := mdsStagedOverLive(t)
	mdsLogs(t)
	locked := 0
	desktopRename = func(src, dst string) error {
		if dst == filepath.Dir(liveExe) && locked < 2 {
			locked++
			return mdsDenied{"[Errno 32] being used by another process"}
		}
		return os.Rename(src, dst)
	}
	promoted := swapStagedDesktopApp(desktopDir, staging, nil)
	if promoted != liveExe {
		t.Fatalf("promoted %q", promoted)
	}
	if raw, _ := os.ReadFile(liveExe); string(raw) != "new" {
		t.Fatalf("live %q", raw)
	}
	if len(*slept) != 2 {
		t.Fatalf("slept %v", *slept)
	}
	if _, err := os.Stat(staging); err == nil {
		t.Fatal("staging left")
	}
}

func TestSwapGivesUpAfterBoundedRetriesAndKeepsLiveApp(t *testing.T) {
	desktopDir, staging, liveExe, slept := mdsStagedOverLive(t)
	logs := mdsLogs(t)
	attempts := 0
	stagedRoot := filepath.Join(staging, strings.Split(mdsPackagedExeRel(), string(filepath.Separator))[0])
	desktopRename = func(src, dst string) error {
		if src == stagedRoot {
			attempts++
			return mdsDenied{"[Errno 5] Access is denied"}
		}
		return os.Rename(src, dst)
	}
	if got := swapStagedDesktopApp(desktopDir, staging, nil); got != "" {
		t.Fatalf("promoted %q", got)
	}
	if attempts != len(desktopSwapRenameRetryDelays)+1 || len(*slept) != len(desktopSwapRenameRetryDelays) {
		t.Fatalf("attempts %d slept %v", attempts, *slept)
	}
	if raw, _ := os.ReadFile(liveExe); string(raw) != "old" {
		t.Fatalf("live %q", raw)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(liveExe)), filepath.Base(filepath.Dir(liveExe))+".previous")); err == nil {
		t.Fatal("previous left behind")
	}
	if !strings.Contains(logs.String(), "Access is denied") || !strings.Contains(logs.String(), "live app kept") {
		t.Fatalf("logs %s", logs.String())
	}
}

func TestSwapAfterACrashBetweenRenamesKeepsThePreviousApp(t *testing.T) {
	desktopDir, staging, liveExe, _ := mdsStagedOverLive(t)
	liveRoot := liveExe
	for filepath.Base(filepath.Dir(liveRoot)) != "release" {
		liveRoot = filepath.Dir(liveRoot)
	}
	if err := os.Rename(liveRoot, liveRoot+desktopPreviousSuffix); err != nil {
		t.Fatal(err)
	}
	desktopRename = func(src, dst string) error {
		if filepath.Dir(src) == staging {
			return errors.New("[Errno 5] disk error")
		}
		return os.Rename(src, dst)
	}
	mdsLogs(t)
	if got := swapStagedDesktopApp(desktopDir, staging, nil); got != "" {
		t.Fatalf("promoted %q", got)
	}
	if raw, _ := os.ReadFile(liveExe); string(raw) != "old" {
		t.Fatalf("live %q", raw)
	}
}

func mdsHintPrinted(t *testing.T, output string) bool {
	buf := sbCapture(t)
	diagnoseEsbuildIgnoreScripts(output)
	return strings.Contains(buf.String(), "esbuild")
}

func TestDiagnoseTriggersOnMissingEsbuildBinary(t *testing.T) {
	if !mdsHintPrinted(t, "Error: The package \"@esbuild/darwin-arm64\" could not be found, and is needed by esbuild.") {
		t.Fatal("no hint")
	}
}

func TestDiagnoseTriggersOnIgnoreScriptsMention(t *testing.T) {
	if !mdsHintPrinted(t, "npm error code ERESOLVE\nnpm warn config ignore-scripts=true") {
		t.Fatal("no hint")
	}
}

func TestDiagnoseSilentOnUnrelatedFailure(t *testing.T) {
	if mdsHintPrinted(t, "vite build failed: unexpected token in index.html") {
		t.Fatal("hint printed")
	}
}

func TestDiagnoseSilentOnNoneOutput(t *testing.T) {
	if mdsHintPrinted(t, "") {
		t.Fatal("hint printed")
	}
}

// Packaged app whose node-pty has no native binary yet, receipt current
func mdsPackaged(t *testing.T) (string, string, string) {
	root := t.TempDir()
	desktop := filepath.Join(root, "apps", "desktop")
	dist := filepath.Join(desktop, "release", "fixture", "resources", "app.asar.unpacked", "dist")
	pty := filepath.Join(dist, "node_modules", "node-pty")
	sbWrite(t, filepath.Join(pty, "package.json"), `{"main": "lib/index.js"}`)
	sbWrite(t, filepath.Join(dist, "index.html"), "<html></html>")
	sbWrite(t, filepath.Join(root, ".gitignore"), "apps/desktop/release/\n")
	prevExe, prevDir, prevCurrent := desktopPackagedExecutable, rendererBundleDir, sourceProductCurrent
	desktopPackagedExecutable = func(string) (string, bool) { return filepath.Join(dist, "Icebath"), true }
	rendererBundleDir = func(string, bool) string { return dist }
	sourceProductCurrent = func(context.Context, string, string, string) bool { return true }
	t.Cleanup(func() {
		desktopPackagedExecutable, rendererBundleDir, sourceProductCurrent = prevExe, prevDir, prevCurrent
	})
	sbCapture(t)
	return root, desktop, pty
}

func TestCurrentStampDoesNotHideMissingNativeBinary(t *testing.T) {
	other := "win32"
	if pySysPlatform() == "win32" {
		other = "linux"
	}
	for _, stale := range []string{"prebuilds/" + pySysPlatform() + "-x64/", "prebuilds/" + other + "-x64/pty.node"} {
		t.Run(stale, func(t *testing.T) {
			root, desktop, pty := mdsPackaged(t)
			path := filepath.Join(pty, filepath.FromSlash(stale))
			if strings.HasSuffix(stale, "/") {
				_ = os.MkdirAll(path, 0o755)
			} else {
				sbWrite(t, path, "MZ")
			}
			if !desktopBuildNeeded(context.Background(), desktop, root, false) {
				t.Fatal("a missing native binary was hidden")
			}
		})
	}
}

func TestPackageWithNativeBinaryStaysCurrent(t *testing.T) {
	for _, location := range []string{"build/Release/pty.node", "prebuilds/" + pySysPlatform() + "-arm64/pty.node"} {
		t.Run(location, func(t *testing.T) {
			root, desktop, pty := mdsPackaged(t)
			sbWrite(t, filepath.Join(pty, filepath.FromSlash(location)), "\x7fELF")
			if desktopBuildNeeded(context.Background(), desktop, root, false) {
				t.Fatal("a package with its native binary was rebuilt")
			}
		})
	}
}
