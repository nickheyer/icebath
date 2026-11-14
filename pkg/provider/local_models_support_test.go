package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Temp home with a fresh job registry and every route seam restored when the test ends
func lmTestHome(t *testing.T, homeName string) string {
	t.Helper()
	dir := t.TempDir()
	h := filepath.Join(dir, homeName)
	t.Setenv("ICEBATH_HOME", h)
	savedDeps, savedLoad, savedReadonly, savedSave := lmDeps, configLoadFn, configLoadReadonlyFn, configSaveFn
	savedSelect, savedSwitch, savedOut, savedWatcher := catalogSelectVariantFn, assignmentSwitchModel, assignmentOut, loadEnsureWatcher
	savedCatalog := Catalog()
	loadEnsureWatcher = func() {}
	lmJobsMu.Lock()
	savedJobs, savedOrder, savedRunning := lmJobs, lmJobOrder, lmRunning
	lmJobs, lmJobOrder, lmRunning = map[string]map[string]any{}, nil, map[string]*lmRunningEntry{}
	lmJobsMu.Unlock()
	t.Cleanup(func() {
		lmWaitSettled(t)
		lmJobsMu.Lock()
		lmJobs, lmJobOrder, lmRunning = savedJobs, savedOrder, savedRunning
		lmJobsMu.Unlock()
		lmDeps, configLoadFn, configLoadReadonlyFn, configSaveFn = savedDeps, savedLoad, savedReadonly, savedSave
		catalogSelectVariantFn, assignmentSwitchModel, assignmentOut, loadEnsureWatcher = savedSelect, savedSwitch, savedOut, savedWatcher
		setCatalog(savedCatalog)
	})
	return h
}

// Waits for every job of the test's registry to leave the running state
func lmWaitSettled(t *testing.T) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		lmJobsMu.Lock()
		busy := false
		for _, job := range lmJobs {
			if job["status"] == "running" {
				busy = true
			}
		}
		lmJobsMu.Unlock()
		if !busy {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("a job was still running when the test ended")
}

// Polls a job's wire view until its status is one of until
func lmPoll(t *testing.T, jobID string, timeout time.Duration, until ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var view map[string]any
	for time.Now().Before(deadline) {
		var err error
		view, err = LocalModelsJob(context.Background(), jobID)
		if err != nil {
			t.Fatalf("job poll: %v", err)
		}
		for _, status := range until {
			if view["status"] == status {
				return view
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job did not settle: %v", view)
	return nil
}

// Status code of a route refusal, zero for any other error
func lmStatusOf(err error) int {
	if e, ok := err.(*HTTPException); ok {
		return e.StatusCode
	}
	return 0
}

// Sum of the covered bytes the job's ranges mapping records
func lmRangesTotal(t *testing.T, v any) int64 {
	t.Helper()
	obj, ok := v.(*pyjson.Object)
	if !ok {
		t.Fatalf("ranges is %T", v)
	}
	var total int64
	for _, key := range obj.Keys {
		for _, row := range obj.Vals[key].([]any) {
			pair := row.([]any)
			total += pair[1].(int64) - pair[0].(int64)
		}
	}
	return total
}

// Range honoring loopback server mirroring the shared Python RangeHandler, one request served at a time
type lmRangeServer struct {
	*httptest.Server
	serving      sync.Mutex
	mu           sync.Mutex
	payloads     map[string][]byte
	slowPerChunk time.Duration
	chunk        int
	etags        bool
	contentLen   map[string]int
	requests     []string
	fallback     []byte
}

// Starts a range server that closes when the test ends
func newLMRangeServer(t *testing.T) *lmRangeServer {
	t.Helper()
	s := &lmRangeServer{payloads: map[string][]byte{}, chunk: 1 << 20, etags: true, contentLen: map[string]int{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// URL of a path on the server
func (s *lmRangeServer) url(path string) string { return s.Server.URL + path }

// Serves payload at path
func (s *lmRangeServer) set(path string, payload []byte) {
	s.mu.Lock()
	s.payloads[path] = payload
	s.mu.Unlock()
}

// Sets the per chunk delay
func (s *lmRangeServer) slow(d time.Duration, chunk int) {
	s.mu.Lock()
	s.slowPerChunk, s.chunk = d, chunk
	s.mu.Unlock()
}

// Range headers the server received, an empty string for a plain GET
func (s *lmRangeServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *lmRangeServer) handle(w http.ResponseWriter, r *http.Request) {
	s.serving.Lock()
	defer s.serving.Unlock()
	s.mu.Lock()
	payload, ok := s.payloads[r.URL.Path]
	etags := s.etags
	declared, lies := s.contentLen[r.URL.Path]
	fallback := s.fallback
	s.requests = append(s.requests, r.Header.Get("Range"))
	s.mu.Unlock()
	if !ok && fallback != nil {
		payload, ok = fallback, true
	}
	if !ok {
		http.Error(w, "Not Found", 404)
		return
	}
	length := len(payload)
	if lies {
		length = declared
	}
	if etags {
		sum := sha256.Sum256(payload)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	}
	flusher := w.(http.Flusher)
	stream := func(body []byte) {
		for served := 0; served < len(body); {
			s.mu.Lock()
			slow, chunk := s.slowPerChunk, s.chunk
			s.mu.Unlock()
			if slow > 0 {
				time.Sleep(slow)
			}
			end := min(served+chunk, len(body))
			_, _ = w.Write(body[served:end])
			flusher.Flush()
			served = end
		}
	}
	if rng := r.Header.Get("Range"); rng != "" {
		spec := strings.TrimPrefix(rng, "bytes=")
		if spec == "0-0" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", length))
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(206)
			_, _ = w.Write(payload[:min(1, len(payload))])
			return
		}
		startS, endS, _ := strings.Cut(spec, "-")
		start, _ := strconv.Atoi(startS)
		end, _ := strconv.Atoi(endS)
		body := payload[min(start, len(payload)):min(end+1, len(payload))]
		declaredBody := len(body)
		if lies {
			declaredBody = end - start + 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, length))
		w.Header().Set("Content-Length", strconv.Itoa(declaredBody))
		w.WriteHeader(206)
		stream(body)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(length))
	w.WriteHeader(200)
	stream(payload)
}

// Writes a fake GGUF of a GGUF magic and size zero bytes
func lmWriteFakeGGUF(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("GGUF"), make([]byte, size)...), 0o644); err != nil {
		t.Fatal(err)
	}
}
