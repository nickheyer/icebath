package pm

import (
	"net/http"
	"strconv"
	"testing"
)

func TestPMMetadataReadsRetryTransientHTTPFailure(t *testing.T) {
	for _, shortBody := range []bool{false, true} {
		t.Run("hash/"+strconv.FormatBool(shortBody), func(t *testing.T) {
			srv := newRangeServer(t)
			body := []byte(`{"assets": [{"name": "tool.zip", "digest": "sha256:abc123"}]}`)
			srv.set("/metadata", body)
			requests := &dltLog{}
			dltConfigure(srv, func(s *rangeServer) {
				s.intercept = func(w http.ResponseWriter, r *http.Request) bool {
					if requests.add(r.URL.Path) != 1 {
						return false
					}
					if !shortBody {
						dltSendError(w, 503)
						return true
					}
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
					w.Header().Set("Connection", "close")
					w.WriteHeader(200)
					w.Write(body[:len(body)/2])
					w.(http.Flusher).Flush()
					return true
				}
			})
			digest, err := HashURL(srv.url("/metadata"))
			if err != nil {
				t.Fatal(err)
			}
			if digest != hexSHA256(body) {
				t.Fatalf("digest %s", digest)
			}
			if n := len(requests.list()); n != 2 {
				t.Fatalf("%d requests, want 2", n)
			}
		})
	}
}
