package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestImpeccableCommandDefShape(t *testing.T) {
	def, err := impeccableCommandDef("impeccable")
	if err != nil {
		t.Fatal(err)
	}
	wantHint := "[shape · audit|critique · animate|bolder|colorize|delight|layout|overdrive|quieter|typeset · adapt|clarify|distill · harden|onboard|optimize|polish · init|document|extract|live|generate] [target]"
	if def.Name != "impeccable" || len(def.Subcommands) != 24 || def.Subcommands[0] != "craft" || def.ArgsHint != wantHint || def.Plugin != "impeccable" {
		t.Fatalf("def %+v", def)
	}
}

// Fake engine scripts directory pinning a version whose linux x64 digest is the given sum
func engineFixture(t *testing.T, sum string) (*engineLauncher, string, map[string]string) {
	scripts := t.TempDir()
	os.WriteFile(filepath.Join(scripts, "VERSION"), []byte("9.9.9\n"), 0o644)
	os.WriteFile(filepath.Join(scripts, "engine-checksums.txt"), []byte("# digests\n9.9.9 impeccable-linux-x64 "+sum+"\n"), 0o644)
	cache := t.TempDir()
	env := map[string]string{"HOME": t.TempDir(), "IMPECCABLE_HOME": cache}
	l := newEngineLauncher(scripts)
	l.Getenv = func(k string) string { return env[k] }
	l.GOOS, l.GOARCH = "linux", "amd64"
	l.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
	l.Stderr = nil
	return l, cache, env
}

const fakeEngine = "#!/bin/sh\necho impeccable-engine 9.9.9\n"

func sumOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestEngineDownloadVerifiedAgainstChecksums(t *testing.T) {
	var hits atomic.Int32
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath.Store(r.URL.Path)
		w.Write([]byte(fakeEngine))
	}))
	defer srv.Close()
	l, cache, env := engineFixture(t, sumOf(fakeEngine))
	env["IMPECCABLE_DOWNLOAD_BASE"] = srv.URL
	bin, err := l.resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bin != filepath.Join(cache, "bin", "9.9.9", "impeccable") || gotPath.Load() != "/engine-v9.9.9/impeccable-linux-x64" || !isExecutable(bin) {
		t.Fatalf("bin %s path %v", bin, gotPath.Load())
	}
	if stamp, size := readStamp(bin + ".verified"); stamp != sumOf(fakeEngine) || size != fileSize(bin) {
		t.Fatalf("stamp %s %s", stamp, size)
	}
	if again, err := l.resolve(context.Background()); err != nil || again != bin || hits.Load() != 1 {
		t.Fatalf("cached reuse %s %v hits %d", again, err, hits.Load())
	}
}

func TestEngineRefusesUnverifiedDownloads(t *testing.T) {
	body := fakeEngine
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	cases := []struct {
		name, sum, body string
		status          int
		want            string
	}{
		{"mismatch", sumOf("other"), fakeEngine, 200, "impeccable: checksum mismatch downloading " + srv.URL + "/engine-v9.9.9/impeccable-linux-x64"},
		{"empty", sumOf(""), "", 200, "downloaded file is empty: " + srv.URL + "/engine-v9.9.9/impeccable-linux-x64; refusing the unverified download"},
		{"missing", sumOf(fakeEngine), "", 404, "impeccable: could not download engine 9.9.9 from " + srv.URL},
	}
	for _, c := range cases {
		body, status = c.body, c.status
		l, cache, env := engineFixture(t, c.sum)
		env["IMPECCABLE_DOWNLOAD_BASE"] = srv.URL
		_, err := l.resolve(context.Background())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
		if isFile(filepath.Join(cache, "bin", "9.9.9", "impeccable")) {
			t.Errorf("%s: unverified engine cached", c.name)
		}
	}
	l, _, env := engineFixture(t, sumOf(fakeEngine))
	os.WriteFile(filepath.Join(l.ScriptsDir, "engine-checksums.txt"), []byte("9.9.9 impeccable-darwin-arm64 abc\n"), 0o644)
	env["IMPECCABLE_DOWNLOAD_BASE"] = srv.URL
	if _, err := l.resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "no checksum for engine 9.9.9 (impeccable-linux-x64)") {
		t.Fatalf("no checksum: %v", err)
	}
}

func TestEngineLocalSourcesAndPlatform(t *testing.T) {
	l, _, env := engineFixture(t, sumOf(fakeEngine))
	pre := filepath.Join(t.TempDir(), "engine")
	os.WriteFile(pre, []byte(fakeEngine), 0o755)
	env["IMPECCABLE_BIN"] = pre
	if bin, err := l.resolve(context.Background()); err != nil || bin != pre {
		t.Fatalf("IMPECCABLE_BIN %s %v", bin, err)
	}
	delete(env, "IMPECCABLE_BIN")
	bundled := filepath.Join(l.ScriptsDir, "bin", "linux-x64", "impeccable")
	os.MkdirAll(filepath.Dir(bundled), 0o755)
	os.WriteFile(bundled, []byte(fakeEngine), 0o644)
	if bin, err := l.resolve(context.Background()); err != nil || bin != bundled {
		t.Fatalf("bundled %s %v", bin, err)
	}
	os.Remove(bundled)
	l.GOARCH = "riscv64"
	if _, err := l.resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "impeccable: no engine binary for linux-unknown found") {
		t.Fatalf("unknown arch %v", err)
	}
	if got := strings.Join(l.exports(), " "); got != "IMPECCABLE_SKILL_DIR="+filepath.Dir(l.ScriptsDir)+" IMPECCABLE_SELF="+filepath.Join(l.ScriptsDir, "impeccable") {
		t.Fatalf("exports %s", got)
	}
}
