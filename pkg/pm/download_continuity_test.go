package pm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestDownloadRetriesRenewOnlyWhileBytesAreSaved(t *testing.T) {
	for _, stallsAfterProgress := range []bool{false, true} {
		t.Run(strconv.FormatBool(stallsAfterProgress), func(t *testing.T) {
			tmp := t.TempDir()
			block := int64(1 << 20)
			payload := bytes.Repeat(dltBytes256(), int(10*block/256))
			var mu sync.Mutex
			var starts []int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				start, end := dltBounds(t, r)
				probe := start == 0 && end == 0
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
				w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
				w.Header().Set("ETag", `"opaque-stable-representation"`)
				w.Header().Set("Connection", "close")
				w.WriteHeader(206)
				count := 0
				if !probe {
					mu.Lock()
					starts = append(starts, start)
					count = len(starts)
					mu.Unlock()
				}
				stop := end + 1
				if !probe && count <= 6 {
					stop = min(end+1, start+block)
				}
				if !probe && stallsAfterProgress && count > 1 {
					stop = start
				}
				w.Write(payload[start:stop])
				w.(http.Flusher).Flush()
			}))
			defer srv.Close()
			destination := filepath.Join(tmp, "model.gguf")
			source := Source{URL: srv.URL + "/model", Dest: destination, SHA256: hexSHA256(payload)}
			download := NewDownload([]Source{source}, DownloadOptions{Connections: 1, PartialsDir: filepath.Join(tmp, "partials")})
			dltSwap(t, &downloadWaitRetry, func(*Download, time.Duration) error { return nil })
			_, err := download.Run(nil)
			mu.Lock()
			got := slices.Clone(starts)
			mu.Unlock()
			if stallsAfterProgress {
				if !IsDownloadKind(err, DownloadTransportFailed) {
					t.Fatalf("want a transport error, got %v", err)
				}
				if dltExists(destination) {
					t.Fatal("stalled download published")
				}
				if want := []int64{0, block, block, block, block}; !slices.Equal(got, want) {
					t.Fatalf("request starts %v, want %v", got, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readFile(t, destination), payload) {
				t.Fatal("destination differs from the payload")
			}
			var want []int64
			for i := int64(0); i < 7; i++ {
				want = append(want, i*block)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("request starts %v, want %v", got, want)
			}
		})
	}
}

func TestForcedTerminationRecoversCheckpointBeforeRequestFinishes(t *testing.T) {
	tmp := t.TempDir()
	payload := dltPayload(1<<20, "durable model bytes")
	waiting, release := make(chan struct{}), make(chan struct{})
	var waitingOnce sync.Once
	var mu sync.Mutex
	var requests [][2]int64
	checkpoint := int64(8 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := dltBounds(t, r)
		probe := start == 0 && end == 0
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.Header().Set("ETag", `"same-file-after-restart"`)
		w.WriteHeader(206)
		w.(http.Flusher).Flush()
		count := 0
		if !probe {
			mu.Lock()
			requests = append(requests, [2]int64{start, end})
			count = len(requests)
			mu.Unlock()
		}
		if !probe && count == 1 {
			w.Write(payload[start : start+checkpoint])
			w.(http.Flusher).Flush()
			waitingOnce.Do(func() { close(waiting) })
			dltWaitClosed(release, 15*time.Second)
			return
		}
		w.Write(payload[start : end+1])
	}))
	defer srv.Close()
	partials := filepath.Join(tmp, "partials")
	destination := filepath.Join(tmp, "model.gguf")
	rawURL := srv.URL + "/model"
	side := filepath.Join(partials, hexSHA256([]byte(rawURL))+".ranges")
	child := dltStartChild(t, rawURL, destination, partials, hexSHA256(payload), 200*time.Millisecond)
	received := dltWaitClosed(waiting, 10*time.Second)
	checkpointed := received && dltWaitUntil(func() bool { return dltIsFile(side) }, 10*time.Second)
	var retained int64
	if checkpointed {
		var saved struct{ Ranges [][2]int64 }
		if err := json.Unmarshal(trimBOM(readFile(t, side)), &saved); err != nil {
			t.Fatal(err)
		}
		for _, r := range saved.Ranges {
			retained += r[1] - r[0]
		}
	}
	child.cmd.Process.Kill()
	_, output := child.finish(5 * time.Second)
	close(release)
	if !received {
		t.Fatalf("child never received model data: %s", output)
	}
	if !checkpointed {
		t.Fatalf("no checkpoint during an unfinished request: %s", output)
	}
	if retained < checkpoint {
		t.Fatalf("checkpoint retained %d bytes, want at least %d", retained, checkpoint)
	}
	if dltExists(destination) {
		t.Fatal("killed child published its destination")
	}
	if _, err := NewDownload([]Source{{URL: rawURL, Dest: destination, SHA256: hexSHA256(payload)}}, DownloadOptions{PartialsDir: partials, Connections: 1}).Run(nil); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, destination), payload) {
		t.Fatal("recovered download differs from the payload")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) < 2 || requests[1][0] < checkpoint {
		t.Fatalf("resume requests %v did not start from the checkpoint", requests)
	}
}

func TestFailedPeriodicCheckpointRetriesWithoutClaimingSavedBytes(t *testing.T) {
	logs := dltCaptureLogs(t)
	tmp := t.TempDir()
	payload := []byte("checkpointed model bytes")
	part := filepath.Join(tmp, "model.part")
	side := filepath.Join(tmp, "model.ranges")
	writeFile(t, part, payload, 0o666)
	source := Source{URL: "https://fixture.invalid/model", Dest: filepath.Join(tmp, "model")}
	download := NewDownload([]Source{source}, DownloadOptions{PartialsDir: tmp, Connections: 1})
	remote := remoteInfo{total: int64(len(payload)), ranged: true, etag: `"stable"`}
	var mu sync.Mutex
	var attempts []int64
	dltSwap(t, &durableWrite, func(path string, data []byte) error {
		if path == side {
			mu.Lock()
			attempts = append(attempts, download.durableDone.Load())
			first := len(attempts) == 1
			mu.Unlock()
			if first {
				return &os.LinkError{Op: "rename", Old: path + ".publish", New: path, Err: syscall.EACCES}
			}
		}
		return DurableWriteBytes(path, data)
	})
	dltSwap(t, &heldRetryDelays, nil)
	dltSwap(t, &checkpointInterval, 10*time.Millisecond)
	dltSwap(t, &downloadReadRange, func(_ *Download, _ Source, _ remoteInfo, _ string, start, end int64, stop *PauseEvent, record func(int64, int64)) error {
		record(start, end)
		if !dltWaitUntil(func() bool { return dltIsFile(side) || stop.IsSet() }, 5*time.Second) {
			return fmt.Errorf("checkpoint never published")
		}
		return nil
	})
	covered, errs := download.rangedAttempt(source, remote, part, nil, 1, func([]Range) error { return nil })
	if len(errs) > 0 || !slices.Equal(covered, []Range{{0, int64(len(payload))}}) {
		t.Fatalf("covered %v errors %v", covered, errs)
	}
	mu.Lock()
	if !slices.Equal(attempts, []int64{0, 0}) {
		t.Fatalf("checkpoint attempts saw durable bytes %v", attempts)
	}
	mu.Unlock()
	var saved struct{ Ranges [][2]int64 }
	if err := json.Unmarshal(trimBOM(readFile(t, side)), &saved); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(saved.Ranges, [][2]int64{{0, int64(len(payload))}}) {
		t.Fatalf("sidecar ranges %v", saved.Ranges)
	}
	if got := download.durableDone.Load(); got != int64(len(payload)) {
		t.Fatalf("durable bytes %d", got)
	}
	if !logs.has(slog.LevelWarn, "could not save download progress for model; continuing") {
		t.Fatal("failed checkpoint was not logged as a warning")
	}
}

func TestLongPrimaryWaitYieldsToAMirrorThatCanWaitLonger(t *testing.T) {
	for _, retryAfter := range []int{5, 290, 600} {
		t.Run(strconv.Itoa(retryAfter), func(t *testing.T) {
			tmp := t.TempDir()
			srv := newRangeServer(t)
			payload := []byte("pinned mirror bytes")
			srv.set("/primary", payload)
			srv.set("/mirror", payload)
			var mu sync.Mutex
			mirrorRequests := 0
			dltConfigure(srv, func(s *rangeServer) {
				s.intercept = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Header.Get("Range") == "bytes=0-0" {
						return false
					}
					mu.Lock()
					if r.URL.Path == "/mirror" {
						mirrorRequests++
					}
					refuse := r.URL.Path == "/primary" || mirrorRequests == 1
					mu.Unlock()
					if !refuse {
						return false
					}
					after := 60
					if r.URL.Path == "/primary" {
						after = retryAfter
					}
					w.Header().Set("Retry-After", strconv.Itoa(after))
					w.Header().Set("Content-Length", "0")
					w.WriteHeader(503)
					return true
				}
			})
			destination := filepath.Join(tmp, "model")
			source := Source{URL: srv.url("/primary"), Dest: destination, SHA256: hexSHA256(payload), Fallbacks: []string{srv.url("/mirror")}}
			waits := dltRecordWaits(t)
			if _, err := NewDownload([]Source{source}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials"), Connections: 1}).Run(nil); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readFile(t, destination), payload) {
				t.Fatal("destination differs from the payload")
			}
			var want []time.Duration
			if retryAfter == 5 {
				want = []time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second}
			}
			want = append(want, 60*time.Second)
			if got := waits.list(); !slices.Equal(got, want) {
				t.Fatalf("waits %v, want %v", got, want)
			}
		})
	}
}
