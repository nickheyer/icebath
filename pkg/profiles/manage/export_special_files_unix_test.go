//go:build unix

package manage

import (
	"archive/tar"
	"compress/gzip"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Binds a unix socket by its cwd relative name and leaves the stale entry behind after close
func bindUnixSocket(t *testing.T, path string) {
	t.Helper()
	t.Chdir(filepath.Dir(path))
	l, err := net.Listen("unix", filepath.Base(path))
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
}

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

func anyName(names []string, pred func(string) bool) bool {
	for _, n := range names {
		if pred(n) {
			return true
		}
	}
	return false
}

func TestNamedProfileExportSurvivesUnixSocket(t *testing.T) {
	tmp := manageEnv(t)
	dir := namedProfile(t, tmp, "sockety")
	browser := filepath.Join(dir, "workspace", ".agent-browser")
	writeFile(t, filepath.Join(dir, "config.yaml"), "model: gpt-4\n")
	writeFile(t, filepath.Join(browser, "state.json"), "{}\n")
	bindUnixSocket(t, filepath.Join(browser, "dev-12345.sock"))
	bindUnixSocket(t, filepath.Join(browser, "control"))
	if err := syscall.Mkfifo(filepath.Join(browser, "events"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := ExportProfile("sockety", filepath.Join(tmp, "sockety.tar.gz"), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := archiveNames(t, result)
	has := func(s string) bool { return anyName(names, func(n string) bool { return strings.Contains(n, s) }) }
	if !has("config.yaml") || !has("state.json") || has(".sock") ||
		anyName(names, func(n string) bool { return strings.HasSuffix(n, "control") || strings.HasSuffix(n, "events") }) {
		t.Fatalf("%v", names)
	}
}

func TestDefaultProfileExportSurvivesUnixSocket(t *testing.T) {
	tmp := manageEnv(t)
	def := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(def, "config.yaml"), "model: gpt-4\n")
	writeFile(t, filepath.Join(def, "sessions", "log.jsonl"), "{}\n")
	bindUnixSocket(t, filepath.Join(def, "sessions", "ipc"))
	result, err := ExportProfile("default", filepath.Join(tmp, "default.tar.gz"), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := archiveNames(t, result)
	if !anyName(names, func(n string) bool { return strings.Contains(n, "config.yaml") }) ||
		!anyName(names, func(n string) bool { return strings.Contains(n, "log.jsonl") }) ||
		anyName(names, func(n string) bool { return strings.HasSuffix(n, "ipc") }) {
		t.Fatalf("%v", names)
	}
}
