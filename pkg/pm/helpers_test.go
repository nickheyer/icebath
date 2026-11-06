package pm

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Moves machine scoped PM state under the test's temp dir the way the pm conftest does
func isolateHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	h := filepath.Join(tmp, ".icebath")
	if err := os.MkdirAll(h, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Cleanup(home.PatchUserHome(tmp))
	t.Setenv(home.EnvHome, h)
	t.Setenv(EnvRuntimeDir, "")
	os.Unsetenv(EnvRuntimeDir)
	config.ResetCache()
	return tmp
}

// Hex sha256 of bytes for test assertions
func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// One request range the server served
type rangeSeen struct {
	Path       string
	Start, End int64
}

// Range honoring loopback server configured per test, mirroring the shared Python RangeHandler
type rangeServer struct {
	*httptest.Server
	mu           sync.Mutex
	payloads     map[string][]byte
	rangesSeen   []rangeSeen
	abortAfter   int64
	slowPerChunk time.Duration
	noRange      bool
	etags        bool
	chunk        int
	intercept    func(w http.ResponseWriter, r *http.Request) bool
}

// Starts a range server with default behaviour that closes when the test ends
func newRangeServer(t *testing.T) *rangeServer {
	t.Helper()
	s := &rangeServer{payloads: map[string][]byte{}, abortAfter: -1, etags: true, chunk: 1 << 20}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// URL of a path on the server
func (s *rangeServer) url(path string) string { return s.Server.URL + path }

// Sets a served payload
func (s *rangeServer) set(path string, payload []byte) {
	s.mu.Lock()
	s.payloads[path] = payload
	s.mu.Unlock()
}

// Ranges served so far
func (s *rangeServer) seen() []rangeSeen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rangeSeen{}, s.rangesSeen...)
}

// Snapshot of the server's configuration for one request
func (s *rangeServer) config() (map[string][]byte, int64, time.Duration, bool, bool, int, func(http.ResponseWriter, *http.Request) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.payloads, s.abortAfter, s.slowPerChunk, s.noRange, s.etags, s.chunk, s.intercept
}

func (s *rangeServer) handle(w http.ResponseWriter, r *http.Request) {
	payloads, abortAfter, slow, noRange, etags, chunk, intercept := s.config()
	if intercept != nil && intercept(w, r) {
		return
	}
	s.serve(w, r, payloads, abortAfter, slow, noRange, etags, chunk)
}

// Default GET behaviour, callable from an intercept that falls through
func (s *rangeServer) serve(w http.ResponseWriter, r *http.Request, payloads map[string][]byte, abortAfter int64, slow time.Duration, noRange, etags bool, chunk int) {
	payload, ok := payloads[r.URL.Path]
	if !ok {
		http.Error(w, "Not Found", 404)
		return
	}
	if etags {
		w.Header().Set("ETag", `"`+hexSHA256(payload)+`"`)
	}
	flusher := w.(http.Flusher)
	stream := func(body []byte, offset int64) {
		served := 0
		for served < len(body) {
			if abortAfter >= 0 && offset+int64(served) >= abortAfter {
				flusher.Flush()
				panic(http.ErrAbortHandler)
			}
			if slow > 0 {
				time.Sleep(slow)
			}
			end := served + chunk
			if abortAfter >= 0 {
				end = min(end, int(abortAfter-offset))
			}
			end = min(end, len(body))
			w.Write(body[served:end])
			flusher.Flush()
			served = end
		}
	}
	if noRange {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(200)
		stream(payload, 0)
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		spec := strings.TrimPrefix(rng, "bytes=")
		if spec == "0-0" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", len(payload)))
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(206)
			w.Write(payload[:min(1, len(payload))])
			return
		}
		startS, endS, _ := strings.Cut(spec, "-")
		start, _ := strconv.ParseInt(startS, 10, 64)
		end, _ := strconv.ParseInt(endS, 10, 64)
		s.mu.Lock()
		s.rangesSeen = append(s.rangesSeen, rangeSeen{r.URL.Path, start, end})
		s.mu.Unlock()
		body := payload[start:min(int(end)+1, len(payload))]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(206)
		stream(body, start)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(200)
	w.Write(payload)
}

// One named archive member
type member struct {
	Name string
	Data []byte
}

// Gzipped tarball of regular files with the given mode
func makeTarGz(t *testing.T, files []member, mode int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.Name, Mode: mode, Size: int64(len(f.Data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.Data)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// Tarball of arbitrary headers, gzipped when asked
func makeTarHeaders(t *testing.T, headers []*tar.Header, data map[string][]byte, gzipped bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var gz *gzip.Writer
	if gzipped {
		gz = gzip.NewWriter(&buf)
		w = gz
	}
	tw := tar.NewWriter(w)
	for _, h := range headers {
		body := data[h.Name]
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if len(body) > 0 {
			tw.Write(body)
		}
	}
	tw.Close()
	if gz != nil {
		gz.Close()
	}
	return buf.Bytes()
}

// Zip of members with optional unix modes keyed by name
func makeZip(t *testing.T, files []member, modes map[string]os.FileMode) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		h := &zip.FileHeader{Name: f.Name, Method: zip.Deflate}
		if mode, ok := modes[f.Name]; ok {
			h.CreatorVersion = 3 << 8
			h.ExternalAttrs = uint32(mode) << 16
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(f.Data)
	}
	zw.Close()
	return buf.Bytes()
}

// One ar member with its padded header
func arMember(name string, data []byte) []byte {
	hdr := fmt.Sprintf("%-16s%-12s%-6s%-6s%-8s%-10d`\n", name, "0", "0", "0", "100644", len(data))
	out := append([]byte(hdr), data...)
	if len(data)%2 == 1 {
		out = append(out, '\n')
	}
	return out
}

// Writes a deb with a control stanza and data members, data compressed as gz or uncompressed
func buildDeb(t *testing.T, path string, control []member, files []*tar.Header, data map[string][]byte, compression string) {
	t.Helper()
	var stanza strings.Builder
	for _, c := range control {
		fmt.Fprintf(&stanza, "%s: %s\n", c.Name, c.Data)
	}
	controlTar := makeTarHeaders(t, []*tar.Header{{Name: "control", Mode: 0o644, Typeflag: tar.TypeReg}},
		map[string][]byte{"control": []byte(stanza.String())}, compression == "gz")
	dataTar := makeTarHeaders(t, files, data, compression == "gz")
	suffix := ".tar"
	if compression != "" {
		suffix += "." + compression
	}
	out := []byte("!<arch>\n")
	out = append(out, arMember("debian-binary", []byte("2.0\n"))...)
	out = append(out, arMember("control"+suffix, controlTar)...)
	out = append(out, arMember("data"+suffix, dataTar)...)
	if err := os.WriteFile(path, out, 0o666); err != nil {
		t.Fatal(err)
	}
}

// Writes a file creating its parents
func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

// Reads a file failing the test on error
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
