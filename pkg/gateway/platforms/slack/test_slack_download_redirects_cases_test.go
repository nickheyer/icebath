package slack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
	"golang.org/x/net/dns/dnsmessage"
)

const (
	dlStart  = "https://files.slack.com/files-pri/TSECOND-F123/image.png"
	dlOrigin = "https://files-origin.slack.com/files-pri/TSECOND-F123/image.png"
	dlToken  = "test-download-token"
)

var dlImage = []byte("\x89PNG\r\n\x1a\nimage bytes")

// Answers A queries over a stream conn with the address resolve picks for each host
func serveFakeDNS(conn net.Conn, resolve func(host string) [4]byte) {
	defer conn.Close()
	for {
		var size [2]byte
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return
		}
		query := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		var p dnsmessage.Parser
		head, err := p.Start(query)
		if err != nil {
			return
		}
		q, err := p.Question()
		if err != nil {
			return
		}
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: head.ID, Response: true, Authoritative: true, RecursionAvailable: true})
		_ = b.StartQuestions()
		_ = b.Question(q)
		_ = b.StartAnswers()
		if q.Type == dnsmessage.TypeA {
			host := strings.TrimSuffix(q.Name.String(), ".")
			_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: resolve(host)})
		}
		msg, err := b.Finish()
		if err != nil {
			return
		}
		binary.BigEndian.PutUint16(size[:], uint16(len(msg)))
		if _, err := conn.Write(append(size[:], msg...)); err != nil {
			return
		}
	}
}

// Routes every lookup through resolve, the getaddrinfo patch of the hermes fixture
func patchResolver(t *testing.T, resolve func(host string) [4]byte) {
	orig := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		client, server := net.Pipe()
		go serveFakeDNS(server, resolve)
		return client, nil
	}}
	t.Cleanup(func() { net.DefaultResolver = orig })
}

func publicResolve(host string) [4]byte {
	if host == "169.254.169.254" {
		return [4]byte{169, 254, 169, 254}
	}
	return [4]byte{93, 184, 216, 34}
}

// Round tripper serving every request from the handler
type dlTransport func(req *http.Request) (*http.Response, error)

func (f dlTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func dlResponse(req *http.Request, status int, headers map[string]string, body []byte) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(body)), Request: req}
}

// Adapter of the download cases with the SSRF client swapped for the handler transport
func dlAdapter(t *testing.T, handler func(req *http.Request) (*http.Response, error)) *SlackAdapter {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	patchResolver(t, publicResolve)
	a := NewSlackAdapter(context.Background(), &gateway.PlatformConfig{Token: "primary-test-token"})
	a.teamClients.Set("TSECOND", &webClient{token: dlToken})
	prev := newDownloadClient
	newDownloadClient = func(*tools.Env, time.Duration) *http.Client {
		return &http.Client{Transport: dlTransport(handler), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	t.Cleanup(func() { newDownloadClient = prev })
	return a
}

func TestEnterpriseGridRedirectRetainsBearer(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		var auths []string
		a := dlAdapter(t, func(req *http.Request) (*http.Response, error) {
			auths = append(auths, req.Header.Get("Authorization"))
			if req.URL.String() == dlStart {
				return dlResponse(req, status, map[string]string{"location": dlOrigin}, nil), nil
			}
			if req.Header.Get("Authorization") != "Bearer "+dlToken {
				return dlResponse(req, 200, map[string]string{"content-type": "text/html"}, []byte("sign in")), nil
			}
			return dlResponse(req, 200, map[string]string{"content-type": "image/png"}, dlImage), nil
		})
		got, err := a.downloadSlackFileBytes(context.Background(), dlStart, "", "file bytes")
		if err != nil || !bytes.Equal(got, dlImage) || len(auths) != 2 || auths[0] != "Bearer "+dlToken || auths[1] != "Bearer "+dlToken {
			t.Fatalf("status %d got %q err %v auths %v", status, got, err, auths)
		}
	}
}

func TestRedirectRefusesUntrustedTarget(t *testing.T) {
	for _, target := range []string{"https://evil.example.com/file", "https://files.slack.com.evil.example.com/file",
		"http://files-origin.slack.com/file", "https://169.254.169.254/latest/meta-data/"} {
		var urls []string
		a := dlAdapter(t, func(req *http.Request) (*http.Response, error) {
			urls = append(urls, req.URL.String())
			if req.URL.String() != dlStart {
				if req.Header.Get("Authorization") != "" {
					t.Errorf("bearer reached an untrusted target")
				}
				return dlResponse(req, 200, nil, []byte("not Slack")), nil
			}
			return dlResponse(req, 302, map[string]string{"location": target}, nil), nil
		})
		_, err := a.downloadSlackFileBytes(context.Background(), dlStart, "", "file bytes")
		if err == nil || !strings.Contains(err.Error(), "Blocked") || len(urls) != 1 || urls[0] != dlStart {
			t.Fatalf("target %s err %v urls %v", target, err, urls)
		}
	}
}

func TestRedirectRejectsSlackHostResolvingPrivate(t *testing.T) {
	requests := 0
	a := dlAdapter(t, func(req *http.Request) (*http.Response, error) {
		requests++
		return dlResponse(req, 302, map[string]string{"location": dlOrigin}, nil), nil
	})
	patchResolver(t, func(host string) [4]byte {
		if host == "files-origin.slack.com" {
			return [4]byte{10, 0, 0, 1}
		}
		return [4]byte{93, 184, 216, 34}
	})
	_, err := a.downloadSlackFileBytes(context.Background(), dlStart, "", "file bytes")
	if err == nil || !strings.Contains(err.Error(), "Blocked") || requests != 1 {
		t.Fatalf("err %v requests %d", err, requests)
	}
}

func TestEveryRedirectChecksCDNAllowlist(t *testing.T) {
	var urls []string
	a := dlAdapter(t, func(req *http.Request) (*http.Response, error) {
		urls = append(urls, req.URL.String())
		if h := req.URL.Hostname(); h != "files.slack.com" && h != "files-origin.slack.com" {
			t.Errorf("request reached %s", h)
		}
		target := "https://evil.example.com/file"
		if req.URL.String() == dlStart {
			target = dlOrigin
		}
		return dlResponse(req, 302, map[string]string{"location": target}, nil), nil
	})
	_, err := a.downloadSlackFileBytes(context.Background(), dlStart, "", "file bytes")
	if err == nil || !strings.Contains(err.Error(), "Blocked non-Slack-CDN") || len(urls) != 2 || urls[0] != dlStart || urls[1] != dlOrigin {
		t.Fatalf("err %v urls %v", err, urls)
	}
}

func TestRelativeRedirectsAllowThreeHops(t *testing.T) {
	var paths []string
	a := dlAdapter(t, func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		if req.Header.Get("Authorization") != "Bearer "+dlToken {
			t.Errorf("hop without bearer")
		}
		if len(paths) <= 3 {
			return dlResponse(req, 302, map[string]string{"location": "/hop-" + string(rune('0'+len(paths)))}, nil), nil
		}
		return dlResponse(req, 200, nil, dlImage), nil
	})
	got, err := a.downloadSlackFileBytes(context.Background(), dlStart, "", "file bytes")
	if err != nil || !bytes.Equal(got, dlImage) || strings.Join(paths[1:], ",") != "/hop-1,/hop-2,/hop-3" {
		t.Fatalf("got %q err %v paths %v", got, err, paths)
	}
}

func TestRedirectLoopIsBounded(t *testing.T) {
	requests := 0
	a := dlAdapter(t, func(req *http.Request) (*http.Response, error) {
		requests++
		return dlResponse(req, 302, map[string]string{"location": dlOrigin}, nil), nil
	})
	_, err := a.downloadSlackFileBytes(context.Background(), dlStart, "", "file bytes")
	if err == nil || !strings.Contains(err.Error(), "Too many Slack file redirects") || requests != 4 {
		t.Fatalf("err %v requests %d", err, requests)
	}
}

// Transport timeout the way httpx raises ReadTimeout
type dlTimeout struct{}

func (dlTimeout) Error() string   { return "test timeout" }
func (dlTimeout) Timeout() bool   { return true }
func (dlTimeout) Temporary() bool { return true }

func TestRedirectedDownloadRetriesFromOriginalURL(t *testing.T) {
	for _, failure := range []string{"429", "503", "timeout"} {
		var urls []string
		a := dlAdapter(t, func(req *http.Request) (*http.Response, error) {
			urls = append(urls, req.URL.String())
			if req.Header.Get("Authorization") != "Bearer "+dlToken {
				t.Errorf("request without bearer")
			}
			if req.URL.String() == dlStart {
				return dlResponse(req, 302, map[string]string{"location": dlOrigin}, nil), nil
			}
			if len(urls) < 6 {
				switch failure {
				case "timeout":
					return nil, dlTimeout{}
				case "429":
					return dlResponse(req, 429, nil, nil), nil
				}
				return dlResponse(req, 503, nil, nil), nil
			}
			return dlResponse(req, 200, nil, dlImage), nil
		})
		var sleeps []time.Duration
		prev := sleepCtx
		sleepCtx = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
		got, err := a.downloadSlackFileBytes(context.Background(), dlStart, "", "file bytes")
		sleepCtx = prev
		want := strings.Repeat(dlStart+","+dlOrigin+",", 3)
		if err != nil || !bytes.Equal(got, dlImage) || strings.Join(urls, ",")+"," != want ||
			len(sleeps) != 2 || sleeps[0] != 1500*time.Millisecond || sleeps[1] != 3*time.Second {
			t.Fatalf("failure %s got %q err %v urls %v sleeps %v", failure, got, err, urls, sleeps)
		}
	}
}

func TestHTMLRejectExplainsEnterpriseGridRedirect(t *testing.T) {
	a := dlAdapter(t, func(req *http.Request) (*http.Response, error) {
		return dlResponse(req, 200, map[string]string{"content-type": "text/html"}, []byte("sign in")), nil
	})
	_, err := a.downloadSlackFileBytes(context.Background(), dlStart, "", "file bytes")
	var valueErr *mediacache.ValueError
	if !errors.As(err, &valueErr) {
		t.Fatalf("err %v is no ValueError", err)
	}
}
