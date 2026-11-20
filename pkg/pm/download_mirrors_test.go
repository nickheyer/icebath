package pm

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Round tripper built from a function
type dltRoundTrip func(*http.Request) (*http.Response, error)

func (f dltRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Response a server would send for an error status with the given reason
func dltStatusResponse(r *http.Request, code int, reason string) *http.Response {
	return &http.Response{
		StatusCode: code, Status: strconv.Itoa(code) + " " + reason, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{}, Body: http.NoBody, Request: r,
	}
}

func TestFallbackRetainsHashDestinationAndProgress(t *testing.T) {
	for _, phase := range []string{"probe", "body"} {
		t.Run(phase, func(t *testing.T) {
			tmp := t.TempDir()
			srv := newRangeServer(t)
			payload := dltPayload(100, "the same pinned bytes")
			srv.set("/primary", payload)
			srv.set("/mirror", payload)
			srv.set("/other", []byte("other"))
			requests := &dltLog{}
			dltConfigure(srv, func(s *rangeServer) {
				s.intercept = func(w http.ResponseWriter, r *http.Request) bool {
					requests.add(r.URL.Path)
					if r.URL.Path == "/primary" && (phase == "probe" || r.Header.Get("Range") != "bytes=0-0") {
						dltSendError(w, 404)
						return true
					}
					return false
				}
			})
			destination := filepath.Join(tmp, "original-name.zip")
			sources := []Source{
				{URL: srv.url("/primary"), Dest: destination, SHA256: hexSHA256(payload), Fallbacks: []string{srv.url("/mirror")}},
				{URL: srv.url("/other"), Dest: filepath.Join(tmp, "other")},
			}
			var ticks []dltTick
			result, err := NewDownload(sources, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials"), Connections: 1}).Run(dltRecorder(&ticks))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(result, []string{sources[0].Dest, sources[1].Dest}) {
				t.Fatalf("result %v", result)
			}
			if !bytes.Equal(readFile(t, destination), payload) {
				t.Fatal("destination differs from the pinned payload")
			}
			expected := int64(len(payload) + len("other"))
			last := ticks[len(ticks)-1]
			if last.Done != expected || last.Total != expected {
				t.Fatalf("last tick %d of %d, want %d", last.Done, last.Total, expected)
			}
			keys := dltRanges(last.Ranges)
			_, hasFirst := keys[sources[0].Dest]
			_, hasSecond := keys[sources[1].Dest]
			if len(keys) != 2 || !hasFirst || !hasSecond {
				t.Fatalf("last tick keys %v", keys)
			}
			seen := requests.list()
			primary, mirror := slices.Index(seen, "/primary"), slices.Index(seen, "/mirror")
			if primary < 0 || mirror < 0 || primary > mirror {
				t.Fatalf("requests %v", seen)
			}
		})
	}
}

func TestBadHashIsNotAMirrorMiss(t *testing.T) {
	tmp := t.TempDir()
	srv := newRangeServer(t)
	srv.set("/primary", []byte("changed upstream"))
	srv.set("/mirror", []byte("pinned"))
	destination := filepath.Join(tmp, "keep.zip")
	writeFile(t, destination, []byte("previous destination"), 0o666)
	source := Source{URL: srv.url("/primary"), Dest: destination, SHA256: hexSHA256([]byte("pinned")), Fallbacks: []string{srv.url("/mirror")}}
	_, err := NewDownload([]Source{source}, DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")}).Run(nil)
	if !IsDownloadKind(err, DownloadHashFailed) {
		t.Fatalf("want a hash error, got %v", err)
	}
	if string(readFile(t, destination)) != "previous destination" {
		t.Fatal("previous destination replaced")
	}
	for _, r := range srv.seen() {
		if r.Path == "/mirror" {
			t.Fatal("a hash failure fell back to the mirror")
		}
	}
}

func TestExhaustedMirrorsNameEachAttemptedURL(t *testing.T) {
	for _, mirrorStatus := range []int{401, 403, 404} {
		t.Run(strconv.Itoa(mirrorStatus), func(t *testing.T) {
			tmp := t.TempDir()
			srv := newRangeServer(t)
			dltConfigure(srv, func(s *rangeServer) {
				s.intercept = func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path == "/also-missing" {
						dltSendError(w, mirrorStatus)
						return true
					}
					return false
				}
			})
			primary, mirror := srv.url("/missing"), srv.url("/also-missing")
			_, err := NewDownload([]Source{{URL: primary, Dest: filepath.Join(tmp, "absent"), SHA256: strings.Repeat("0", 64), Fallbacks: []string{mirror}}},
				DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")}).Run(nil)
			if !isDownloadError(err) {
				t.Fatalf("want a download error, got %v", err)
			}
			message := err.Error()
			if !strings.Contains(message, primary) || !strings.Contains(message, mirror) {
				t.Fatalf("message does not name both urls: %s", message)
			}
			if !strings.Contains(message, "404") || !strings.Contains(message, strconv.Itoa(mirrorStatus)) {
				t.Fatalf("message does not name both statuses: %s", message)
			}
			if dltExists(filepath.Join(tmp, "absent")) {
				t.Fatal("destination published")
			}
		})
	}
}

func TestOnlyAvailabilityFailuresCanUseAMirror(t *testing.T) {
	for _, failure := range []string{"503", "521", "dns", "tls", "disk", "pause"} {
		t.Run(failure, func(t *testing.T) {
			tmp := t.TempDir()
			srv := newRangeServer(t)
			payload := []byte("still pinned")
			srv.set("/primary", payload)
			srv.set("/mirror", payload)
			requests := &dltLog{}
			dest := filepath.Join(tmp, "tool")
			dl := NewDownload([]Source{{URL: srv.url("/primary"), Dest: dest, SHA256: hexSHA256(payload), Fallbacks: []string{srv.url("/mirror")}}},
				DownloadOptions{PartialsDir: filepath.Join(tmp, "partials")})
			waits := dltRecordWaits(t)
			original := downloadClient
			dltSwap(t, &downloadClient, &http.Client{CheckRedirect: original.CheckRedirect, Transport: dltRoundTrip(func(r *http.Request) (*http.Response, error) {
				requests.add(r.URL.String())
				if strings.HasSuffix(r.URL.String(), "/primary") {
					switch failure {
					case "tls":
						return nil, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
					case "disk":
						return nil, &os.PathError{Op: "open", Path: "local path denied", Err: fs.ErrPermission}
					case "pause":
						dl.Pause()
					case "503":
						return dltStatusResponse(r, 503, "Unavailable"), nil
					case "521":
						return dltStatusResponse(r, 521, "Web Server Is Down"), nil
					case "dns":
						return nil, &net.DNSError{Err: "nodename nor servname provided", Name: r.URL.Hostname(), IsNotFound: true}
					}
				}
				return original.Transport.RoundTrip(r)
			})})
			_, err := dl.Run(nil)
			primaryCount := 0
			for _, u := range requests.list() {
				if u == srv.url("/primary") {
					primaryCount++
				}
			}
			switch failure {
			case "dns", "521":
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(readFile(t, dest), payload) || primaryCount != 1 || len(waits.list()) != 0 {
					t.Fatalf("primary requests %d waits %v", primaryCount, waits.list())
				}
			case "503":
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(readFile(t, dest), payload) || primaryCount != networkAttempts || len(waits.list()) != networkAttempts-1 {
					t.Fatalf("primary requests %d waits %v", primaryCount, waits.list())
				}
			default:
				if !isDownloadError(err) {
					t.Fatalf("want a download error, got %v", err)
				}
				if dltExists(dest) || len(waits.list()) != 0 || slices.Contains(requests.list(), srv.url("/mirror")) {
					t.Fatalf("requests %v waits %v", requests.list(), waits.list())
				}
			}
		})
	}
}
