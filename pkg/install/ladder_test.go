package install

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Points a ladder run at a scratch icebath home, user home and install dir with a bash login shell
func isolatedLadder(t *testing.T, repo, commit string) (*Runner, *bytes.Buffer, string, string) {
	t.Helper()
	homeDir, userHome := t.TempDir(), t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	t.Setenv("HOME", userHome)
	t.Setenv("SHELL", "/bin/bash")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("ICEBATH_INSTALL_DIR", "")
	t.Setenv("ICEBATH_INSTALL_CHANNEL", "")
	r, err := NewRunner(Options{RepoURL: repo, Commit: commit, NonInteractive: true, InstallMethod: MethodScript}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r.stdout, r.stderr = &out, &out
	r.sink = cliSink(&out, &out)
	return r, &out, homeDir, userHome
}

// Pending install receipt the run parked under the home metrics dir
func ladderReceipt(t *testing.T, homeDir string) map[string]any {
	t.Helper()
	found, _ := filepath.Glob(filepath.Join(homeDir, "telemetry", "shared_metrics", "pending_installs", "*.json"))
	if len(found) != 1 {
		t.Fatalf("install receipts %v", found)
	}
	raw, _ := os.ReadFile(found[0])
	var receipt map[string]any
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

// One unattended ladder against a loopback release installs, publishes and records the pinned commit
func TestInstallLadderInstallsAReleaseEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix layout")
	}
	tag, commit := "v1.4.2", strings.Repeat("ab", 20)
	goos, goarch := hostKey()
	f := newReleaseFixture(t, []fixtureRelease{{Tag: tag, Target: commit, Published: "2026-11-01T00:00:00Z",
		Assets: map[string][]byte{ReleaseArchiveName(tag, goos, goarch): fixtureArchive(t, tag)}}}, nil)
	r, out, homeDir, userHome := isolatedLadder(t, f.repo, commit)
	if code := r.runLadder(context.Background()); code != 0 {
		t.Fatalf("ladder exit %d\n%s", code, out)
	}
	installRoot := filepath.Join(homeDir, InstallDirName)
	if r.installDir != installRoot {
		t.Fatalf("install root %q", r.installDir)
	}
	for _, p := range []string{
		InstallBinaryPath(installRoot),
		filepath.Join(installRoot, InstallMethodFile),
		filepath.Join(installRoot, ".icebath", "bin", "icebath"),
		filepath.Join(userHome, ".local", "bin", "icebath"),
		filepath.Join(homeDir, ".env"),
		filepath.Join(homeDir, "skills"),
	} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("missing %s: %v\n%s", p, err, out)
		}
	}
	if got := InstalledRelease(context.Background(), InstallBinaryPath(installRoot)).Tag; got != tag {
		t.Fatalf("installed binary names %q", got)
	}
	if !strings.Contains(out.String(), "icebath fixture "+tag+" [pm install]") {
		t.Fatalf("runtimes stage did not run the installed pm install\n%s", out)
	}
	rc, _ := os.ReadFile(filepath.Join(userHome, ".bashrc"))
	if !strings.Contains(string(rc), ShellPathMarker) {
		t.Fatal("shell PATH line not wired")
	}
	var marker map[string]any
	raw, err := os.ReadFile(BootstrapMarkerPath(installRoot))
	if err != nil || json.Unmarshal(raw, &marker) != nil || marker["pinnedCommit"] != commit || marker["pinnedBranch"] != "main" {
		t.Fatalf("install receipt marker %q %v", raw, err)
	}
	if receipt := ladderReceipt(t, homeDir); receipt["outcome"] != "success" || receipt["failed_stage"] != "none" {
		t.Fatalf("receipt %v", receipt)
	}
}

// A digest mismatch fails the repository stage, keeps no binary and records the failure class
func TestInstallLadderRefusesABinaryWhoseChecksumDisagrees(t *testing.T) {
	goos, goarch := hostKey()
	commit := strings.Repeat("cd", 20)
	f := newReleaseFixture(t, []fixtureRelease{{Tag: "v1.0.0", Target: commit, Published: "2026-11-01T00:00:00Z",
		Assets: map[string][]byte{ReleaseArchiveName("v1.0.0", goos, goarch): []byte("release bytes")}}}, nil)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/"+ReleaseArchiveName("v1.0.0", goos, goarch)) {
			w.Write([]byte("tampered bytes"))
			return
		}
		f.serve(w, req)
	})
	r, out, homeDir, _ := isolatedLadder(t, f.repo, commit)
	if code := r.runLadder(context.Background()); code == 0 {
		t.Fatalf("tampered binary installed\n%s", out)
	}
	if !strings.Contains(out.String(), "digest mismatch") {
		t.Fatalf("output names no digest mismatch\n%s", out)
	}
	installRoot := filepath.Join(homeDir, InstallDirName)
	for _, p := range []string{InstallBinaryPath(installRoot), InstallBinaryPath(installRoot) + ".download"} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s left behind", p)
		}
	}
	receipt := ladderReceipt(t, homeDir)
	if receipt["outcome"] != "failed" || receipt["failed_stage"] != "repository" || receipt["failure_class"] != "download_digest_mismatch" {
		t.Fatalf("receipt %v", receipt)
	}
}
