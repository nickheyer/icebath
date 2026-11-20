package msalruntime

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// Serves one archive holding the library bytes under member and pins this platform to it
func serveArchive(t *testing.T, member string, library []byte) (*atomic.Int32, source) {
	t.Helper()
	if _, ok := LibraryName(); !ok {
		t.Skip("no msalruntime library is published for this platform")
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range map[string][]byte{"pymsalruntime/__init__.py": []byte("x"), member: library} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	src := source{url: srv.URL + "/library.whl", sha256: sum(buf.Bytes()), member: "pymsalruntime/library.bin", memberSHA256: sum(library), glibc: [2]int{2, 24}}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	prevSources, prevGlibc := sources[platform], systemGlibc
	sources[platform] = []source{src}
	systemGlibc = func() (int, int, bool) { return 2, 31, true }
	t.Cleanup(func() {
		sources[platform], systemGlibc = prevSources, prevGlibc
		verified = map[string]string{}
	})
	return &hits, src
}

func TestEnsureFetchesThePinnedLibraryOnceAndRepairsIt(t *testing.T) {
	library := bytes.Repeat([]byte("native code "), 4096)
	hits, _ := serveArchive(t, "pymsalruntime/library.bin", library)
	home := t.TempDir()
	path, err := Ensure(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := LibraryPath(home)
	got, _ := os.ReadFile(path)
	if st, _ := os.Stat(path); path != want || !bytes.Equal(got, library) || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed %s with %d bytes", path, len(got))
	}
	if !strings.HasPrefix(path, home) || !strings.Contains(path, Version) {
		t.Fatalf("library placed at %s", path)
	}
	verified = map[string]string{}
	if _, err := Ensure(context.Background(), home); err != nil || hits.Load() != 1 {
		t.Fatalf("an intact library was fetched again: %v, %d requests", err, hits.Load())
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	verified = map[string]string{}
	if _, err := Ensure(context.Background(), home); err != nil || hits.Load() != 2 {
		t.Fatalf("a tampered library was kept: %v, %d requests", err, hits.Load())
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, library) {
		t.Fatal("the tampered library was not replaced")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover files %v", entries)
	}
}

func TestEnsureRefusesBytesThatMissThePins(t *testing.T) {
	library := []byte("native code")
	platform := runtime.GOOS + "/" + runtime.GOARCH
	for name, change := range map[string]func(*source){
		"archive sha256":  func(s *source) { s.sha256 = strings.Repeat("0", 64) },
		"library sha256":  func(s *source) { s.memberSHA256 = strings.Repeat("0", 64) },
		"absent member":   func(s *source) { s.member = "pymsalruntime/other.bin" },
		"missing archive": func(s *source) { s.url = "http://127.0.0.1:1/absent.whl" },
	} {
		t.Run(name, func(t *testing.T) {
			_, src := serveArchive(t, "pymsalruntime/library.bin", library)
			change(&src)
			sources[platform] = []source{src}
			home := t.TempDir()
			path, err := Ensure(context.Background(), home)
			if err == nil {
				t.Fatalf("installed %s", path)
			}
			if want, _ := LibraryPath(home); fileExists(want) {
				t.Fatalf("a refused library was left at %s", want)
			}
		})
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestPickSourceFollowsTheSystemCLibrary(t *testing.T) {
	if runtime.GOOS != "linux" || len(sources["linux/"+runtime.GOARCH]) < 2 {
		t.Skip("one archive serves this platform")
	}
	prev := systemGlibc
	defer func() { systemGlibc = prev }()
	for _, c := range []struct {
		major, minor int
		ok           bool
		want         [2]int
		fails        bool
	}{{2, 39, true, [2]int{2, 35}, false}, {2, 35, true, [2]int{2, 35}, false}, {2, 31, true, [2]int{2, 24}, false}, {2, 17, true, [2]int{}, true}, {0, 0, false, [2]int{}, true}} {
		systemGlibc = func() (int, int, bool) { return c.major, c.minor, c.ok }
		src, err := pickSource()
		if (err != nil) != c.fails || src.glibc != c.want {
			t.Errorf("glibc %d.%d %v picked %v, %v", c.major, c.minor, c.ok, src.glibc, err)
		}
	}
	if major, minor, ok := glibcVersion(); ok && (major < 2 || minor < 0) {
		t.Errorf("glibc version read as %d.%d", major, minor)
	}
}
