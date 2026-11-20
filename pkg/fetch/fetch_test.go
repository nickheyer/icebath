package fetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func archive(t *testing.T, entries map[string]string, links map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	for name, target := range links {
		if err := tw.WriteHeader(&tar.Header{Name: name, Linkname: target, Typeflag: tar.TypeSymlink}); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func pinned(name string, data []byte, url string) Asset {
	sum := sha256.Sum256(data)
	return Asset{Name: name, URL: url, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
}

// Serves data, cutting the first response short so the client has to resume with a range
func flaky(t *testing.T, data []byte, hits *atomic.Int32) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		start := 0
		if spec := r.Header.Get("Range"); spec != "" {
			start, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(spec, "bytes="), "-"))
			w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(len(data)-1)+"/"+strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusPartialContent)
		}
		if n == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data[:len(data)/2])
			return
		}
		w.Write(data[start:])
	}))
	t.Cleanup(server.Close)
	return server
}

func TestFileResumesVerifiesAndServesFromCache(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	retryGap = time.Millisecond
	data := bytes.Repeat([]byte("icebath model bytes "), 4096)
	var hits atomic.Int32
	server := flaky(t, data, &hits)
	asset := pinned("model.onnx", data, server.URL+"/model.onnx")
	path, err := File(context.Background(), asset)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, data) || hits.Load() != 2 {
		t.Fatalf("resumed file differs or took %d requests", hits.Load())
	}
	if again, err := File(context.Background(), asset); err != nil || again != path || hits.Load() != 2 {
		t.Fatalf("cached asset fetched again: %v %d", err, hits.Load())
	}
	if !strings.HasPrefix(path, Root()) {
		t.Fatalf("%s is outside the cache root %s", path, Root())
	}
}

func TestFileRefusesWrongBytesAndFailingServers(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	retryGap = time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte("tampered"))
	}))
	defer server.Close()
	asset := pinned("voice.bin", []byte("original"), server.URL+"/voice.bin")
	var digest *DigestError
	if _, err := File(context.Background(), asset); !errors.As(err, &digest) {
		t.Fatalf("tampered download error = %v", err)
	}
	left, _ := os.ReadDir(filepath.Join(Root(), asset.SHA256))
	if len(left) != 0 {
		t.Fatalf("rejected bytes stayed in the cache: %v", left)
	}
	asset.URL = server.URL + "/gone"
	var status *StatusError
	if _, err := File(context.Background(), asset); !errors.As(err, &status) || status.Code != http.StatusNotFound {
		t.Fatalf("missing asset error = %v", err)
	}
	if _, err := File(context.Background(), Asset{Name: "x", URL: server.URL}); err == nil {
		t.Fatal("an asset without a digest was fetched")
	}
}

func TestTreeUnpacksOnceAndRefusesEscapes(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	good := archive(t, map[string]string{"pkg/lib/libx.so.1": "elf"}, map[string]string{"pkg/lib/libx.so": "libx.so.1"})
	evil := archive(t, map[string]string{"../escape": "x"}, nil)
	link := archive(t, nil, map[string]string{"pkg/out": "../../outside"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(map[string][]byte{"/good.tar.gz": good, "/evil.tar.gz": evil, "/link.tar.gz": link}[r.URL.Path])
	}))
	defer server.Close()
	tree, err := Tree(context.Background(), pinned("good.tar.gz", good, server.URL+"/good.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(filepath.Join(tree, "pkg", "lib", "libx.so")); err != nil || string(body) != "elf" {
		t.Fatalf("link inside the tree unreadable: %q %v", body, err)
	}
	if again, err := Tree(context.Background(), pinned("good.tar.gz", good, "http://127.0.0.1:1/unreachable")); err != nil || again != tree {
		t.Fatalf("unpacked tree fetched again: %v", err)
	}
	for name, data := range map[string][]byte{"evil.tar.gz": evil, "link.tar.gz": link} {
		if _, err := Tree(context.Background(), pinned(name, data, server.URL+"/"+name)); err == nil || !strings.Contains(err.Error(), "outside its tree") {
			t.Fatalf("%s unpacked: %v", name, err)
		}
	}
}
