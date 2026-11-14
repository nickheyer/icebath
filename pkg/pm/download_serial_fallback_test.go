package pm

import (
	"bytes"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
)

func TestSerialFallbackSurvivesATransientRetry(t *testing.T) {
	for _, refusedStatus := range []int{403, 404} {
		t.Run(strconv.Itoa(refusedStatus), func(t *testing.T) {
			tmp := t.TempDir()
			srv := newRangeServer(t)
			payload := []byte("verified bytes")
			srv.set("/tool", payload)
			dest := filepath.Join(tmp, "tool")
			dl := NewDownload([]Source{{URL: srv.url("/tool"), Dest: dest, SHA256: hexSHA256(payload)}}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")})
			var mu sync.Mutex
			var modes []int
			dltConfigure(srv, func(s *rangeServer) {
				s.intercept = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Header.Get("Range") == "bytes=0-0" {
						return false
					}
					mu.Lock()
					modes = append(modes, dl.Connections)
					count := len(modes)
					mu.Unlock()
					if count >= 3 {
						return false
					}
					if count == 1 {
						dltSendError(w, refusedStatus)
					} else {
						dltSendError(w, 503)
					}
					return true
				}
			})
			waits := dltRecordWaits(t)
			if _, err := dl.Run(nil); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readFile(t, dest), payload) {
				t.Fatal("destination differs from the payload")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(modes) < 3 || modes[0] <= 1 {
				t.Fatalf("modes %v", modes)
			}
			if !slices.Equal(modes[1:], []int{1, 1}) {
				t.Fatalf("a network retry undid the CDN downgrade: %v", modes)
			}
			if len(waits.list()) != 1 {
				t.Fatalf("waits %v", waits.list())
			}
			if dl.Connections != modes[0] {
				t.Fatalf("connections %d after the transfer, want %d", dl.Connections, modes[0])
			}
		})
	}
}
