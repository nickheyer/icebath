package pm

import (
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestDownloadFailureNeverPublishesPartialBytes(t *testing.T) {
	type scenario struct{ failure, phase string }
	var cases []scenario
	for _, failure := range []string{"503", "404", "403"} {
		for _, phase := range []string{"probe", "ranged", "single"} {
			cases = append(cases, scenario{failure, phase})
		}
	}
	cases = append(cases, scenario{"hash", "ranged"}, scenario{"hash", "single"}, scenario{"disk", "probe"})
	for _, c := range cases {
		t.Run(c.failure+"-"+c.phase, func(t *testing.T) {
			tmp := t.TempDir()
			srv := newRangeServer(t)
			payload := []byte("verified bytes")
			srv.set("/tool", payload)
			status, numeric := strconv.Atoi(c.failure)
			isStatus := numeric == nil
			requests := &dltLog{}
			dltConfigure(srv, func(s *rangeServer) {
				s.noRange = c.phase == "single"
				s.intercept = func(w http.ResponseWriter, r *http.Request) bool {
					requestRange := r.Header.Get("Range")
					requests.add(requestRange)
					isProbe := requestRange == "bytes=0-0"
					if isStatus && isProbe == (c.phase == "probe") {
						dltSendError(w, status)
						return true
					}
					return false
				}
			})
			dest := filepath.Join(tmp, "tool")
			digest := hexSHA256(payload)
			if c.failure == "hash" {
				digest = strings.Repeat("0", 64)
			}
			dl := NewDownload([]Source{{URL: srv.url("/tool"), Dest: dest, SHA256: digest}}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials"), Connections: 1})
			waits := dltRecordWaits(t)
			if c.failure == "disk" {
				writeFile(t, filepath.Join(dest, "occupied"), []byte("keep"), 0o666)
			}
			_, err := dl.Run(nil)
			switch {
			case c.failure == "hash":
				if !IsDownloadKind(err, DownloadHashFailed) {
					t.Fatalf("want a hash error, got %v", err)
				}
			case c.failure == "disk":
				if !errors.Is(err, syscall.EISDIR) {
					t.Fatalf("want a directory destination error, got %v", err)
				}
			default:
				var de *DownloadError
				if !errors.As(err, &de) || de.Kind != DownloadTransportFailed {
					t.Fatalf("want a transport error, got %v", err)
				}
				if de.Status != status || !strings.Contains(de.Error(), srv.url("/tool")) {
					t.Fatalf("status %d message %q", de.Status, de.Error())
				}
			}
			if c.failure == "disk" {
				if string(readFile(t, filepath.Join(dest, "occupied"))) != "keep" || len(requests.list()) != 0 || len(waits.list()) != 0 {
					t.Fatalf("requests %v waits %v", requests.list(), waits.list())
				}
				return
			}
			if dltExists(dest) {
				t.Fatal("failed download published")
			}
			errorRequests := requests.list()
			if c.phase != "probe" {
				errorRequests = slices.DeleteFunc(errorRequests, func(v string) bool { return v == "bytes=0-0" })
			}
			switch c.failure {
			case "503":
				if len(errorRequests) != networkAttempts || len(waits.list()) != networkAttempts-1 {
					t.Fatalf("requests %v waits %v", errorRequests, waits.list())
				}
			case "hash":
				if len(waits.list()) != 0 {
					t.Fatalf("waits %v", waits.list())
				}
			default:
				if len(waits.list()) != 0 || len(errorRequests) != 1 {
					t.Fatalf("requests %v waits %v", errorRequests, waits.list())
				}
			}
		})
	}
}

func TestPauseInterruptsRealHTTPRetryBackoff(t *testing.T) {
	for _, phase := range []string{"probe", "ranged", "single"} {
		t.Run(phase, func(t *testing.T) {
			tmp := t.TempDir()
			srv := newRangeServer(t)
			srv.set("/tool", []byte("keep these bytes"))
			requests := &dltLog{}
			dltConfigure(srv, func(s *rangeServer) {
				s.noRange = phase == "single"
				s.intercept = func(w http.ResponseWriter, r *http.Request) bool {
					requestRange := r.Header.Get("Range")
					requests.add(requestRange)
					if (requestRange == "bytes=0-0") == (phase == "probe") {
						w.Header().Set("Retry-After", "30")
						w.Header().Set("Content-Length", "0")
						w.WriteHeader(503)
						return true
					}
					return false
				}
			})
			dest := filepath.Join(tmp, "tool")
			dl := NewDownload([]Source{{URL: srv.url("/tool"), Dest: dest}}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")})
			waits := &dltWaits{}
			waiting := make(chan struct{})
			var waitingOnce sync.Once
			original := downloadWaitRetry
			dltSwap(t, &downloadWaitRetry, func(d *Download, delay time.Duration) error {
				waits.add(delay)
				waitingOnce.Do(func() { close(waiting) })
				return original(d, delay)
			})
			finished := make(chan struct{})
			var runErr error
			go func() {
				defer close(finished)
				_, runErr = dl.Run(nil)
			}()
			reached := dltWaitClosed(waiting, 10*time.Second)
			dl.Pause()
			stopped := dltWaitClosed(finished, 5*time.Second)
			if !reached {
				t.Fatal("the failed request never reached backoff")
			}
			if !stopped {
				t.Fatal("pause did not interrupt the backoff")
			}
			if !IsDownloadKind(runErr, DownloadWasPaused) {
				t.Fatalf("want a pause, got %v", runErr)
			}
			failed := 0
			for _, v := range requests.list() {
				if (v == "bytes=0-0") == (phase == "probe") {
					failed++
				}
			}
			if failed != 1 {
				t.Fatalf("%d failing requests", failed)
			}
			if got := waits.list(); !slices.Equal(got, []time.Duration{time.Duration(maxDelaySeconds * float64(time.Second))}) {
				t.Fatalf("waits %v", got)
			}
			if dltExists(dest) {
				t.Fatal("paused download published")
			}
		})
	}
}
