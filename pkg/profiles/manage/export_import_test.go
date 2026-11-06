package manage

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func archiveNames(t *testing.T, archive string) []string {
	t.Helper()
	f, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	return names
}

func containsSub(names []string, sub string) bool {
	return slices.ContainsFunc(names, func(n string) bool { return strings.Contains(n, sub) })
}

func TestExportDefaultIncludesProfileData(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(def, "config.yaml"), "model: test")
	writeFile(t, filepath.Join(def, ".env"), "KEY=val")
	writeFile(t, filepath.Join(def, "SOUL.md"), "Be nice.")
	writeFile(t, filepath.Join(def, "memories", "MEMORY.md"), "remember this")
	out := filepath.Join(t.TempDir(), "export", "default.tar.gz")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ExportProfile("default", out, nil); err != nil {
		t.Fatal(err)
	}
	names := archiveNames(t, out)
	for _, want := range []string{"default/config.yaml", "default/SOUL.md", "default/memories/MEMORY.md"} {
		if !slices.Contains(names, want) {
			t.Fatalf("%s missing from %v", want, names)
		}
	}
	if slices.Contains(names, "default/.env") {
		t.Fatal("credentials were exported")
	}
}

func TestExportDefaultHandlesBrokenSymlinks(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	def := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(def, "config.yaml"), "ok")
	broken := filepath.Join(def, "skills", "with-broken-links")
	writeFile(t, filepath.Join(broken, "valid_target.txt"), "real data")
	if err := os.Symlink("/nonexistent/path", filepath.Join(broken, "broken_link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(broken, "valid_target.txt"), filepath.Join(broken, "valid_link")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "export", "default.tar.gz")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := ExportProfile("default", out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !pathExists(result) {
		t.Fatal("archive missing")
	}
	names := archiveNames(t, result)
	if !slices.ContainsFunc(names, func(n string) bool { return strings.HasSuffix(n, "config.yaml") }) {
		t.Fatalf("config.yaml missing from %v", names)
	}
	for _, want := range []string{"with-broken-links/broken_link", "valid_link", "valid_target.txt"} {
		if !containsSub(names, want) {
			t.Fatalf("%s missing from %v", want, names)
		}
	}
}

func TestExportLeavesTheBotDesktopBrowserProfileOut(t *testing.T) {
	for _, name := range []string{"coder", "default"} {
		t.Run(name, func(t *testing.T) {
			tmp := manageEnv(t)
			captureOut(t)
			dir := filepath.Join(tmp, ".icebath")
			if name != "default" {
				d, err := CreateProfile(t.Context(), name, CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				dir = d
			}
			writeFile(t, filepath.Join(dir, "config.yaml"), "model: test")
			writeFile(t, filepath.Join(dir, "bot-desktop", "browser-profile", "Default", "Cookies"), "SQLite format 3\x00")
			out := filepath.Join(t.TempDir(), "export", name+".tar.gz")
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := ExportProfile(name, out, nil); err != nil {
				t.Fatal(err)
			}
			names := archiveNames(t, out)
			if !slices.Contains(names, name+"/config.yaml") {
				t.Fatalf("config missing from %v", names)
			}
			if containsSub(names, "bot-desktop") {
				t.Fatalf("bot-desktop exported %v", names)
			}
		})
	}
}
