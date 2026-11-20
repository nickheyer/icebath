package pm

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestProgressCountsCachedFilesAndDistinguishesEqualBasenames(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	bodies := map[string][]byte{"/a": []byte("first"), "/b": []byte("second-file")}
	var sources []Source
	var expected int64
	for _, path := range []string{"/a", "/b"} {
		srv.set(path, bodies[path])
		sources = append(sources, Source{URL: srv.url(path), Dest: filepath.Join(tmp, path[1:], "model.bin")})
		expected += int64(len(bodies[path]))
	}
	for attempt := range 2 {
		var ticks []dltTick
		if _, err := NewDownload(sources, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")}).Run(dltRecorder(&ticks)); err != nil {
			t.Fatal(err)
		}
		if len(ticks) == 0 {
			t.Fatalf("attempt %d did not report its completed files", attempt)
		}
		last := ticks[len(ticks)-1]
		if last.Done != expected || last.Total != expected {
			t.Fatalf("attempt %d last tick %d of %d, want %d", attempt, last.Done, last.Total, expected)
		}
		if len(last.Ranges) != len(sources) || dltCovered(last.Ranges) != last.Done {
			t.Fatalf("attempt %d ranges %v", attempt, last.Ranges)
		}
	}
}

func TestUnknownLengthSourcesDoNotLoseCompletedBytes(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	srv.set("/a", []byte("first"))
	srv.set("/b", []byte("second-file"))
	dltConfigure(srv, func(s *rangeServer) {
		s.intercept = func(w http.ResponseWriter, r *http.Request) bool {
			payloads, _, _, _, _, _, _ := srv.config()
			payload, ok := payloads[r.URL.Path]
			if !ok {
				dltSendError(w, 404)
				return true
			}
			w.Header().Set("ETag", `"`+hexSHA256(payload)+`"`)
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			w.Write(payload)
			w.(http.Flusher).Flush()
			return true
		}
	})
	var ticks []dltTick
	sources := []Source{{URL: srv.url("/a"), Dest: filepath.Join(tmp, "a")}, {URL: srv.url("/b"), Dest: filepath.Join(tmp, "b")}}
	if _, err := NewDownload(sources, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")}).Run(dltRecorder(&ticks)); err != nil {
		t.Fatal(err)
	}
	var expected int64
	for _, source := range sources {
		st, err := os.Stat(source.Dest)
		if err != nil {
			t.Fatal(err)
		}
		expected += st.Size()
	}
	last := ticks[len(ticks)-1]
	if last.Done != expected || last.Total != expected {
		t.Fatalf("last tick %d of %d, want %d", last.Done, last.Total, expected)
	}
	var previous int64
	for _, tick := range ticks {
		if dltCovered(tick.Ranges) != tick.Done {
			t.Fatalf("tick %d disagrees with its ranges %v", tick.Done, tick.Ranges)
		}
		if tick.Done < previous {
			t.Fatalf("done went backwards from %d to %d", previous, tick.Done)
		}
		previous = tick.Done
	}
}
