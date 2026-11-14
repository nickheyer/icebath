package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Points the home, the host state directory and the checkout at this test's own directories
func uzIsolate(t *testing.T, root string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(state, ".icebath"))
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(state, "gateway-locks"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(state, "xdg"))
	if err := os.MkdirAll(filepath.Join(state, ".icebath"), 0o755); err != nil {
		t.Fatal(err)
	}
	ucPatchRoot(t, root)
	beginUpdateAttempt()
	t.Cleanup(beginUpdateAttempt)
}

func TestZipSwapKeepsEveryNestedBuildOutputAndTheGuardAdmitsThem(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "install")
	outputs := map[string]string{
		"apps/desktop/release/win-unpacked/Hermes.exe": "MZbuilt",
		"apps/desktop/node_modules/electron/index.js":  "electron",
		"apps/desktop/dist/index.html":                 "live renderer",
		"cmd/icebath/web_dist/index.html":              "<dashboard>",
	}
	for rel, data := range outputs {
		ugWrite(t, filepath.Join(root, filepath.FromSlash(rel)), data)
	}
	if err := os.Mkdir(filepath.Join(root, "cmd", "icebath", "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	ugWrite(t, filepath.Join(root, "cmd", "icebath", "x.py"), "old")

	extracted := filepath.Join(tmp, "extracted")
	ugWrite(t, filepath.Join(extracted, "apps", "desktop", "dist", "index.html"), "shipped renderer")
	ugWrite(t, filepath.Join(extracted, "cmd", "icebath", "x.py"), "new")

	staged, err := stageEntries(&bytes.Buffer{}, extracted, []string{"apps", "cmd"}, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitStagedReplacements(staged, nil, "", nil); err != nil {
		t.Fatal(err)
	}

	if got := ugRead(t, filepath.Join(root, "cmd", "icebath", "x.py")); got != "new" {
		t.Fatalf("x.py = %q", got)
	}
	for rel, data := range outputs {
		if strings.HasPrefix(rel, "apps/desktop/dist/") {
			continue
		}
		if got := ugRead(t, filepath.Join(root, filepath.FromSlash(rel))); got != data {
			t.Fatalf("%s = %q, want %q", rel, got, data)
		}
	}
	if got := ugRead(t, filepath.Join(root, "apps", "desktop", "dist", "index.html")); got != "shipped renderer" {
		t.Fatalf("dist/index.html = %q", got)
	}
	if statExists(filepath.Join(root, "cmd", "icebath", "__pycache__")) {
		t.Fatal("__pycache__ survived the swap")
	}

	for _, line := range []string{"!! apps/desktop/release/", "!! apps/desktop/dist/", "!! apps/desktop/node_modules/",
		"!! apps/desktop/build/", "!! cmd/icebath/web_dist/", "!! cmd/icebath/__pycache__/",
		"!! __pycache__/",
		"!! scripts/whatsapp-bridge/node_modules/", "!! web/node_modules/", "!! tests-js/node_modules/"} {
		if !isZipPreservedEntryStatusLine(line, nil) {
			t.Fatalf("not admitted: %s", line)
		}
	}
	for _, line := range []string{"!! apps/desktop/notes.local", "?? apps/desktop/release/", "!! cmd/icebath/web_dist_backup/",
		"R  src/x -> apps/desktop/release/x"} {
		if isZipPreservedEntryStatusLine(line, nil) {
			t.Fatalf("admitted: %s", line)
		}
	}
}

// Serves the archive at zipPath for every download and skips the completion tail for one test
func uzServeArchive(t *testing.T, zipPath string, downloadedTo *string) {
	t.Helper()
	savedRetrieve, savedComplete := zipURLRetrieve, zipCompleteSourceUpdate
	zipURLRetrieve = func(_ context.Context, _, dest string) error {
		if downloadedTo != nil {
			*downloadedTo = filepath.Dir(dest)
		}
		raw, err := os.ReadFile(zipPath)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, raw, 0o644)
	}
	zipCompleteSourceUpdate = func(context.Context, io.Writer, map[string]any) error { return nil }
	t.Cleanup(func() { zipURLRetrieve, zipCompleteSourceUpdate = savedRetrieve, savedComplete })
}

// Writes a ZIP with the given members, a symlink member when its mode says so, and an archive comment
func uzBuildZip(t *testing.T, zipPath, comment string, members map[string]string, symlink bool) {
	t.Helper()
	handle, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(handle)
	for name, body := range members {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		if symlink {
			header.SetMode(os.ModeSymlink | 0o777)
		}
		member, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if comment != "" {
		if err := writer.SetComment(comment); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateViaZipRejectsSymlinkMember(t *testing.T) {
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "evil.zip")
	uzBuildZip(t, zipPath, "", map[string]string{"icebath-agent-main/evil-link": "/etc/passwd"}, true)
	fakeRoot := filepath.Join(tmp, "install_dir")
	if err := os.Mkdir(fakeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	uzIsolate(t, fakeRoot)
	downloadedTo := ""
	uzServeArchive(t, zipPath, &downloadedTo)

	_, err := updateViaZip(context.Background(), &bytes.Buffer{}, map[string]any{}, false, "", "", map[string]any{})
	var exit *pm.SystemExit
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("err = %v, want exit status 1", err)
	}
	if downloadedTo == "" {
		t.Fatal("ZIP validation must run before refusal")
	}
	if lexists(filepath.Join(downloadedTo, "icebath-agent-main", "evil-link")) {
		t.Fatal("symlink member should never be materialized")
	}
}

func TestUpdateViaZipAcceptsNormalMember(t *testing.T) {
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "normal.zip")
	uzBuildZip(t, zipPath, "8192da90e0afb20010a1c2f5da83db305d05ac5a", map[string]string{"icebath-agent-main/README.md": "ok\n"}, false)
	fakeRoot := filepath.Join(tmp, "install_dir")
	if err := os.Mkdir(fakeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	uzIsolate(t, fakeRoot)
	uzServeArchive(t, zipPath, nil)

	var out bytes.Buffer
	if _, err := updateViaZip(context.Background(), &out, map[string]any{}, false, "", "", map[string]any{}); err != nil && !isUpdateExit(err) {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "symlink member") {
		t.Fatalf("output names a symlink member:\n%s", out.String())
	}
	if got := ugRead(t, filepath.Join(fakeRoot, "README.md")); got != "ok\n" {
		t.Fatalf("README.md = %q", got)
	}
}
