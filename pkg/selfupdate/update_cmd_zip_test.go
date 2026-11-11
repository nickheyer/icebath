package selfupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
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

// Set of names for the shipped argument of the dirty tree guard
func uzSet(names ...string) map[string]bool {
	set := map[string]bool{}
	for _, name := range names {
		set[name] = true
	}
	return set
}

// Whether the dirty tree guard refuses root, failing the test when git cannot answer
func uzBlocked(t *testing.T, root string, shipped map[string]bool) bool {
	t.Helper()
	_, blocked, err := zipOverlayBlockReason(context.Background(), root, nil, shipped)
	if err != nil {
		t.Fatal(err)
	}
	return blocked
}

func TestZipSwapKeepsEveryNestedBuildOutputAndTheGuardAdmitsThem(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "install")
	outputs := map[string]string{
		"apps/desktop/release/win-unpacked/Hermes.exe": "MZbuilt",
		"apps/desktop/node_modules/electron/index.js":  "electron",
		"apps/desktop/dist/index.html":                 "live renderer",
		"icebath_cli/web_dist/index.html":              "<dashboard>",
	}
	for rel, data := range outputs {
		ugWrite(t, filepath.Join(root, filepath.FromSlash(rel)), data)
	}
	if err := os.Mkdir(filepath.Join(root, "icebath_cli", "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	ugWrite(t, filepath.Join(root, "icebath_cli", "x.py"), "old")

	extracted := filepath.Join(tmp, "extracted")
	ugWrite(t, filepath.Join(extracted, "apps", "desktop", "dist", "index.html"), "shipped renderer")
	ugWrite(t, filepath.Join(extracted, "icebath_cli", "x.py"), "new")

	staged, err := stageEntries(&bytes.Buffer{}, extracted, []string{"apps", "icebath_cli"}, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitStagedReplacements(staged, nil, "", nil); err != nil {
		t.Fatal(err)
	}

	if got := ugRead(t, filepath.Join(root, "icebath_cli", "x.py")); got != "new" {
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
	if statExists(filepath.Join(root, "icebath_cli", "__pycache__")) {
		t.Fatal("__pycache__ survived the swap")
	}

	for _, line := range []string{"!! apps/desktop/release/", "!! apps/desktop/dist/", "!! apps/desktop/node_modules/",
		"!! apps/desktop/build/", "!! icebath_cli/web_dist/", "!! icebath_cli/__pycache__/",
		"!! __pycache__/", "!! ui-tui/dist/", "!! ui-tui/packages/hermes-ink/dist/",
		"!! scripts/whatsapp-bridge/node_modules/", "!! web/node_modules/", "!! tests-js/node_modules/"} {
		if !isZipPreservedEntryStatusLine(line, nil) {
			t.Fatalf("not admitted: %s", line)
		}
	}
	for _, line := range []string{"!! apps/desktop/notes.local", "?? apps/desktop/release/", "!! icebath_cli/web_dist_backup/",
		"R  src/x -> apps/desktop/release/x"} {
		if isZipPreservedEntryStatusLine(line, nil) {
			t.Fatalf("admitted: %s", line)
		}
	}
}

func TestGuardAdmitsARealInstallsIgnoredSetAndBlocksOnlyWhatTheSwapDestroys(t *testing.T) {
	ugEnv(t)
	root := filepath.Join(t.TempDir(), "install")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ugGit(t, root, "init", "-q", root)
	ignore, err := os.ReadFile(filepath.Join("..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), ignore, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tracked := range []string{"apps/desktop/package.json", "icebath_cli/main.py", "scripts/whatsapp-bridge/index.js",
		"ui-tui/package.json", "web/package.json", "tests-js/a.test.ts", "run_agent.py"} {
		ugWrite(t, filepath.Join(root, filepath.FromSlash(tracked)), "src")
	}
	ugGit(t, root, "add", "-A")
	ugGit(t, root, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init")
	for _, ignored := range []string{".bytecode-fingerprint", ".icebath-bootstrap-complete", ".install_method",
		"icebath_agent.egg-info/PKG-INFO", "icebath_cli/__pycache__/main.pyc", "__pycache__/x.pyc",
		"apps/desktop/release/win-unpacked/Hermes.exe", "apps/desktop/dist/index.html",
		"apps/desktop/build/icon.ico", "apps/desktop/node_modules/electron/index.js",
		"icebath_cli/web_dist/index.html", "ui-tui/dist/entry.js", "ui-tui/node_modules/x/index.js",
		"ui-tui/packages/hermes-ink/dist/index.js", "web/node_modules/x/index.js",
		"tests-js/node_modules/x/index.js", "scripts/whatsapp-bridge/node_modules/x/index.js",
		"venv/lib.py", "node_modules/x/index.js", ".env"} {
		ugWrite(t, filepath.Join(root, filepath.FromSlash(ignored)), "artifact")
	}
	status := ugGit(t, root, "status", "--porcelain", "-uall", "--ignored=matching")
	if strings.Count(status, "!!") < 19 || strings.Contains(status, "??") {
		t.Fatalf("the fixture is not all ignored:\n%s", status)
	}

	if uzBlocked(t, root, nil) {
		t.Fatal("the guard refused an install that only carries ignored artifacts")
	}
	shipped := []string{"apps", "icebath_cli", "scripts", "ui-tui", "web", "tests-js", "run_agent.py"}
	if uzBlocked(t, root, uzSet(shipped...)) {
		t.Fatal("the guard refused with the real shipped set")
	}
	if !uzBlocked(t, root, uzSet(append(shipped, "icebath_agent.egg-info")...)) {
		t.Fatal("the guard admitted an ignored root entry the ZIP ships")
	}
	ugWrite(t, filepath.Join(root, "apps", "desktop", ".env"), "mine")
	if !uzBlocked(t, root, nil) {
		t.Fatal("the guard admitted ignored user data under a shipped directory")
	}
}

// A clean install with the tracked ignore rules, one tracked source file and both built renderers
func uzCleanInstall(t *testing.T) string {
	t.Helper()
	ugEnv(t)
	root := filepath.Join(t.TempDir(), "install")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ugGit(t, root, "init", "-q", root)
	ignore, err := os.ReadFile(filepath.Join("..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), ignore, 0o644); err != nil {
		t.Fatal(err)
	}
	ugWrite(t, filepath.Join(root, "apps", "desktop", "package.json"), "{\"name\":\"source\"}\n")
	ugGit(t, root, "add", "-A")
	ugGit(t, root, "commit", "-qm", "init")
	if status := ugGit(t, root, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("status = %q", status)
	}
	for _, dist := range []string{"dist", "dist-webapp"} {
		ugWrite(t, filepath.Join(root, "apps", "desktop", dist, "index.html"), "retained "+dist)
	}
	return root
}

func TestCleanWebappOutputIsAdmittedButDirtySourceStillRefuses(t *testing.T) {
	root := uzCleanInstall(t)
	if status := ugGit(t, root, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("status = %q", status)
	}
	if ignored := ugGit(t, root, "check-ignore", filepath.Join(root, "apps", "desktop", "dist-webapp", "index.html")); strings.TrimSpace(ignored) == "" {
		t.Fatal("the webapp output is not ignored")
	}
	for _, shipped := range []map[string]bool{nil, uzSet("apps", ".gitignore")} {
		if uzBlocked(t, root, shipped) {
			t.Fatalf("refused a clean install (shipped %v)", shipped)
		}
	}
	source := filepath.Join(root, "apps", "desktop", "package.json")
	ugWrite(t, source, ugRead(t, source)+"\n")
	for _, shipped := range []map[string]bool{nil, uzSet("apps", ".gitignore")} {
		if !uzBlocked(t, root, shipped) {
			t.Fatalf("admitted a dirty source file (shipped %v)", shipped)
		}
	}
}

func TestLocalZipGraftsBothRenderersAndPreswapGuardKeepsDirtySource(t *testing.T) {
	root := uzCleanInstall(t)
	tmp := t.TempDir()
	uzIsolate(t, root)
	ctx := context.Background()
	source := filepath.Join(root, "apps", "desktop", "package.json")
	original := ugRead(t, source)
	outputs := map[string]string{}
	for _, path := range []string{filepath.Join(root, "apps", "desktop", "dist", "index.html"),
		filepath.Join(root, "apps", "desktop", "dist-webapp", "index.html")} {
		outputs[path] = ugRead(t, path)
	}
	extracted := filepath.Join(tmp, "icebath-agent-main")
	newSource := filepath.Join(extracted, "apps", "desktop", "package.json")
	ugWrite(t, newSource, "{\"name\":\"updated-source\"}\n")
	archive := filepath.Join(tmp, "source.zip")
	handle, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(handle)
	member, err := writer.Create("icebath-agent-main/apps/desktop/package.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := member.Write([]byte(ugRead(t, newSource))); err != nil {
		t.Fatal(err)
	}
	if err := writer.SetComment("8192da90e0afb20010a1c2f5da83db305d05ac5a"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	archiveURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(archive)}).String()
	unchanged := func() {
		t.Helper()
		for path, data := range outputs {
			if got := ugRead(t, path); got != data {
				t.Fatalf("%s = %q, want %q", path, got, data)
			}
		}
	}

	ugWrite(t, source, original+"\n")
	_, err = downloadAndSwapZip(ctx, &bytes.Buffer{}, "main", archiveURL, "")
	var exit *pm.SystemExit
	if !errors.As(err, &exit) || exit.Code != 1 {
		t.Fatalf("err = %v, want exit status 1", err)
	}
	if got := ugRead(t, source); got != original+"\n" {
		t.Fatalf("source = %q", got)
	}
	unchanged()
	if statExists(filepath.Join(root, "apps"+zipStagingSuffix)) {
		t.Fatal("a staging copy was left behind")
	}
	ugWrite(t, source, original)

	staged, err := stageEntries(&bytes.Buffer{}, extracted, []string{"apps"}, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range outputs {
		rel, err := filepath.Rel(filepath.Join(root, "apps"), path)
		if err != nil {
			t.Fatal(err)
		}
		if got := ugRead(t, filepath.Join(staged[0].staging, rel)); got != data {
			discardStaged(staged)
			t.Fatalf("staged %s = %q, want %q", rel, got, data)
		}
	}
	discardStaged(staged)
	var out bytes.Buffer
	if _, err := downloadAndSwapZip(ctx, &out, "main", archiveURL, ""); err != nil {
		t.Fatalf("swap failed: %v\n%s", err, out.String())
	}
	if got := ugRead(t, source); got != ugRead(t, newSource) {
		t.Fatalf("source = %q", got)
	}
	unchanged()
	for _, suffix := range []string{zipStagingSuffix, zipOldSuffix} {
		if statExists(filepath.Join(root, "apps"+suffix)) {
			t.Fatalf("apps%s was left behind", suffix)
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
