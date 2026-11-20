package pm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestInvalidWorkerResponseCannotPublish(t *testing.T) {
	for _, reply := range []string{"whole-body", "wrong-bounds", "changed-size", "changed-etag", "short-body"} {
		t.Run(reply, func(t *testing.T) {
			tmp := t.TempDir()
			payload := bytes.Repeat(dltBytes256(), 12*1024)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				bounds := r.Header.Get("Range")
				status, contentRange, body := 206, fmt.Sprintf("bytes 0-0/%d", len(payload)), payload[:1]
				if bounds != "bytes=0-0" {
					start, end := dltBounds(t, r)
					total, reportedStart := int64(len(payload)), start
					body = payload[start : end+1]
					switch reply {
					case "whole-body":
						status, body = 200, payload
					case "wrong-bounds":
						reportedStart++
					case "changed-size":
						total++
					case "short-body":
						body = body[:len(body)-1]
					}
					contentRange = fmt.Sprintf("bytes %d-%d/%d", reportedStart, end, total)
				}
				w.Header().Set("Content-Range", contentRange)
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				etag := `"original"`
				if reply == "changed-etag" && bounds != "bytes=0-0" {
					etag = `"new"`
				}
				w.Header().Set("ETag", etag)
				w.WriteHeader(status)
				w.Write(body)
			}))
			defer srv.Close()
			destination := filepath.Join(tmp, "model.gguf")
			_, err := NewDownload([]Source{{URL: srv.URL + "/model", Dest: destination}}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials"), Connections: 1}).Run(nil)
			if !isDownloadError(err) {
				t.Fatalf("want a download error, got %v", err)
			}
			if dltExists(destination) {
				t.Fatal("invalid worker response published")
			}
		})
	}
}

func TestFailedCopyPublicationKeepsDestinationAtomic(t *testing.T) {
	for _, previous := range [][]byte{nil, []byte("previous complete model")} {
		t.Run(strconv.FormatBool(previous != nil), func(t *testing.T) {
			tmp := t.TempDir()
			part := filepath.Join(tmp, "partials", "model.part")
			payload := []byte("new complete model bytes")
			writeFile(t, part, payload, 0o666)
			side := filepath.Join(tmp, "partials", "model.ranges")
			writeFile(t, side, []byte("[]"), 0o666)
			destination := filepath.Join(tmp, "other-volume", "model.gguf")
			if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
				t.Fatal(err)
			}
			if previous != nil {
				writeFile(t, destination, previous, 0o666)
			}
			source := Source{URL: "https://example.invalid/model", Dest: destination}
			download := NewDownload([]Source{source}, DownloadOptions{PartialsDir: filepath.Dir(part)})
			dltSwap(t, &copyFileObj, func(dst io.Writer, src io.Reader) error {
				head := make([]byte, 4)
				n, _ := io.ReadFull(src, head)
				dst.Write(head[:n])
				return fmt.Errorf("destination full: %w", syscall.ENOSPC)
			})
			err := download.finalize(source, part, side)
			if err == nil || !strings.Contains(err.Error(), "destination full") || !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("want the disk full error, got %v", err)
			}
			if previous == nil && dltExists(destination) {
				t.Fatal("partial copy published")
			}
			if previous != nil && !bytes.Equal(readFile(t, destination), previous) {
				t.Fatal("previous destination damaged")
			}
			if !bytes.Equal(readFile(t, part), payload) || !dltExists(side) {
				t.Fatal("partial or sidecar lost")
			}
			names := dltNames(t, filepath.Dir(destination))
			if (previous == nil && len(names) != 0) || (previous != nil && (len(names) != 1 || names[0] != "model.gguf")) {
				t.Fatalf("destination directory holds %v", names)
			}
		})
	}
}

func TestFragmentedResumeObeysConnectionLimit(t *testing.T) {
	tmp := t.TempDir()
	payload := bytes.Repeat(dltBytes256(), 16)
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce sync.Once
	var mu sync.Mutex
	active, maximum := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := dltBounds(t, r)
		isProbe := start == 0 && end == 0
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.Header().Set("ETag", `"same-object"`)
		w.WriteHeader(206)
		w.(http.Flusher).Flush()
		if !isProbe {
			mu.Lock()
			active++
			maximum = max(maximum, active)
			enteredOnce.Do(func() { close(entered) })
			mu.Unlock()
			dltWaitClosed(release, 8*time.Second)
			mu.Lock()
			active--
			mu.Unlock()
		}
		w.Write(payload[start : end+1])
	}))
	defer srv.Close()
	rawURL := srv.URL + "/fragmented"
	partials := filepath.Join(tmp, "partials")
	key := hexSHA256([]byte(rawURL))
	writeFile(t, filepath.Join(partials, key+".part"), payload, 0o666)
	sidecar, _ := json.Marshal(map[string]any{
		"total": len(payload), "etag": `"same-object"`, "sha256": hexSHA256(payload),
		"ranges": [][2]int{{0, 512}, {1024, 1536}, {2048, 2560}},
	})
	writeFile(t, filepath.Join(partials, key+".ranges"), sidecar, 0o666)
	source := Source{URL: rawURL, Dest: filepath.Join(tmp, "result"), SHA256: hexSHA256(payload)}
	finished := make(chan struct{})
	var runErr error
	go func() {
		defer close(finished)
		_, runErr = NewDownload([]Source{source}, DownloadOptions{PartialsDir: partials, Connections: 1}).Run(nil)
	}()
	reached := dltWaitClosed(entered, 8*time.Second)
	if reached {
		time.Sleep(2 * time.Second)
	}
	close(release)
	stopped := dltWaitClosed(finished, 15*time.Second)
	if !reached {
		t.Fatal("no range request reached the server")
	}
	if !stopped || runErr != nil {
		t.Fatalf("download stopped %v with %v", stopped, runErr)
	}
	if !bytes.Equal(readFile(t, source.Dest), payload) {
		t.Fatal("destination differs from the payload")
	}
	mu.Lock()
	defer mu.Unlock()
	if maximum != 1 {
		t.Fatalf("%d concurrent range requests with one connection", maximum)
	}
}

func TestProcessesSharePartialOwnershipWithoutLosingDestinations(t *testing.T) {
	tmp := t.TempDir()
	payload := bytes.Repeat(dltBytes256(), 4096*2)
	entered, both, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var enteredOnce, bothOnce sync.Once
	var mu sync.Mutex
	active, maximum := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := dltBounds(t, r)
		probe := start == 0 && end == 0
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.Header().Set("ETag", `"unchanged"`)
		w.WriteHeader(206)
		w.(http.Flusher).Flush()
		if !probe {
			mu.Lock()
			active++
			maximum = max(maximum, active)
			enteredOnce.Do(func() { close(entered) })
			if active > 1 {
				bothOnce.Do(func() { close(both) })
			}
			mu.Unlock()
			dltWaitClosed(release, 15*time.Second)
			mu.Lock()
			active--
			mu.Unlock()
		}
		w.Write(payload[start : end+1])
	}))
	defer srv.Close()
	destinations := []string{filepath.Join(tmp, "first", "model"), filepath.Join(tmp, "second", "model")}
	var children []*dltChild
	reached := true
	for _, destination := range destinations {
		children = append(children, dltStartChild(t, srv.URL+"/model", destination, filepath.Join(tmp, "partials"), "", 0))
		if !dltWaitClosed(entered, 10*time.Second) {
			reached = false
			break
		}
	}
	if reached {
		dltWaitClosed(both, 2*time.Second)
	}
	close(release)
	type result struct {
		code   int
		output string
	}
	var results []result
	for _, child := range children {
		code, output := child.finish(25 * time.Second)
		results = append(results, result{code, output})
	}
	if !reached {
		t.Fatalf("no child reached the server: %v", results)
	}
	for _, r := range results {
		if r.code != 0 {
			t.Fatalf("child failed: %v", results)
		}
	}
	for _, destination := range destinations {
		if !bytes.Equal(readFile(t, destination), payload) {
			t.Fatalf("%s differs from the payload", destination)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maximum != 1 {
		t.Fatal("two processes wrote one partial concurrently")
	}
}

func TestResumeNeverTrustsUnboundCoverage(t *testing.T) {
	for _, damage := range []string{"changed-etag", "missing-part", "invalid-bounds", "no-validator"} {
		t.Run(damage, func(t *testing.T) {
			tmp := t.TempDir()
			srv := newRangeServer(t)
			payload := dltPayload(1000, "new remote data")
			srv.set("/fresh", payload)
			sourceURL := srv.url("/fresh")
			key := hexSHA256([]byte(sourceURL))
			partials := filepath.Join(tmp, "partials")
			if err := os.MkdirAll(partials, 0o777); err != nil {
				t.Fatal(err)
			}
			if damage != "missing-part" {
				writeFile(t, filepath.Join(partials, key+".part"), dltPayload(len(payload), "x"), 0o666)
			}
			etag := `"` + hexSHA256(payload) + `"`
			if damage == "changed-etag" {
				etag = `"previous-object"`
			}
			if damage == "no-validator" {
				etag = ""
				dltConfigure(srv, func(s *rangeServer) { s.etags = false })
			}
			ranges := [][2]int{{0, len(payload)}}
			if damage == "invalid-bounds" {
				ranges = [][2]int{{-1, len(payload)}}
			}
			sidecar, _ := json.Marshal(map[string]any{"total": len(payload), "etag": etag, "sha256": "", "ranges": ranges})
			writeFile(t, filepath.Join(partials, key+".ranges"), sidecar, 0o666)
			destination := filepath.Join(tmp, "model")
			if _, err := NewDownload([]Source{{URL: sourceURL, Dest: destination}}, DownloadOptions{PartialsDir: partials}).Run(nil); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readFile(t, destination), payload) {
				t.Fatal("unbound coverage was trusted")
			}
		})
	}
}
