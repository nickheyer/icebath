package kanban

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Secure default of the SSRF guard, private targets refused
func defaultURLGuard(t *testing.T) {
	t.Helper()
	unsetEnv(t, "ICEBATH_ALLOW_PRIVATE_URLS")
}

// Calls attach_url and asserts the SSRF guard fired with no attachment row written
func assertAttachURLBlocked(t *testing.T, tid, url string) {
	t.Helper()
	d := runKanbanTool(t, "kanban_attach_url", `{"url": "`+url+`"}`)
	e := errText(d)
	if e == "" || (!strings.Contains(e, "SSRF") && !strings.Contains(strings.ToLower(e), "blocked")) {
		t.Fatal(d)
	}
	if atts := boardAttachments(t, tid); len(atts) != 0 {
		t.Fatal(atts)
	}
}

// Resolver answering the mapped hosts with public addresses, literal IPs as themselves and anything else as NXDOMAIN
func fakePublicDNS(t *testing.T, mapping map[string]string) {
	t.Helper()
	t.Cleanup(tools.SwapLookupHostIPs(func(_ context.Context, host string) ([]netip.Addr, error) {
		ip, mapped := mapping[host]
		if !mapped {
			if _, err := netip.ParseAddr(host); err != nil {
				return nil, &net.DNSError{Err: fmt.Sprintf("fake DNS: unknown host '%s'", host), Name: host, IsNotFound: true}
			}
			ip = host
		}
		return []netip.Addr{netip.MustParseAddr(ip)}, nil
	}))
}

// RoundTripper answering every request through a function, the stand in for httpx.stream
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Routes the attachment download through a fake transport for one test
func patchAttachTransport(t *testing.T, rt roundTripFunc) {
	t.Helper()
	prev := attachHTTPClient
	t.Cleanup(func() { attachHTTPClient = prev })
	attachHTTPClient = func() *http.Client {
		return &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
}

func TestAttachURLRejectsNonHTTPScheme(t *testing.T) {
	toolWorkerEnv(t)
	d := runKanbanTool(t, "kanban_attach_url", `{"url": "file:///etc/passwd"}`)
	if !strings.Contains(errText(d), "scheme") {
		t.Fatal(d)
	}
}

func TestAttachURLBlocksLoopback(t *testing.T) {
	tid := toolWorkerEnv(t)
	defaultURLGuard(t)
	assertAttachURLBlocked(t, tid, "http://127.0.0.1/")
}

func TestAttachURLHappyPathPublicHost(t *testing.T) {
	tid := toolWorkerEnv(t)
	defaultURLGuard(t)
	fakePublicDNS(t, map[string]string{"files.example.com": "93.184.216.34"})
	payload := []byte("public fetch body")
	patchAttachTransport(t, func(r *http.Request) (*http.Response, error) {
		if got := r.URL.String(); got != "http://files.example.com/docs/spec.pdf" {
			t.Errorf("url %s", got)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/pdf; charset=binary"}},
			Body: io.NopCloser(bytes.NewReader(payload)), Request: r}, nil
	})
	d := runKanbanTool(t, "kanban_attach_url", `{"url": "http://files.example.com/docs/spec.pdf"}`)
	if d["ok"] != true || d["size"] != float64(len(payload)) {
		t.Fatal(d)
	}
	atts := boardAttachments(t, tid)
	if len(atts) != 1 || atts[0].Filename != "spec.pdf" || atts[0].ContentType == nil || *atts[0].ContentType != "application/pdf" {
		t.Fatal(atts)
	}
	if stored, err := os.ReadFile(atts[0].StoredPath); err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("%q %v", stored, err)
	}
}

func TestDownloadURLWithCapFollowsRelativeRedirectsPerHop(t *testing.T) {
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "true")
	var hops []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops = append(hops, r.URL.Path+" "+r.Header.Get("User-Agent"))
		switch r.URL.Path {
		case "/start":
			w.Header().Set("Location", "../files/next")
			w.WriteHeader(http.StatusFound)
		case "/files/next":
			w.Header().Set("Location", "/final")
			w.WriteHeader(http.StatusPermanentRedirect)
		case "/final":
			w.Header().Add("Content-Type", "text/plain ; charset=utf-8")
			w.Header().Add("Content-Type", "ignored")
			_, _ = w.Write([]byte("redirected body"))
		}
	}))
	defer srv.Close()
	data, contentType, err := downloadURLWithCap(context.Background(), nil, srv.URL+"/start", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "redirected body" || contentType == nil || *contentType != "text/plain" {
		t.Fatalf("%q %v", data, contentType)
	}
	want := []string{"/start icebath-kanban/attach", "/files/next icebath-kanban/attach", "/final icebath-kanban/attach"}
	if strings.Join(hops, ",") != strings.Join(want, ",") {
		t.Fatal(hops)
	}
}

func TestDownloadURLWithCapRedirectFailures(t *testing.T) {
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "true")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/empty":
			w.Header()["Location"] = []string{""}
			w.WriteHeader(http.StatusMovedPermanently)
		case "/loop":
			w.Header().Set("Location", "/loop")
			w.WriteHeader(http.StatusSeeOther)
		case "/nolocation":
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/private":
			w.Header().Set("Location", "http://169.254.169.254/latest/meta-data")
			w.WriteHeader(http.StatusFound)
		case "/gone":
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	for _, c := range []struct {
		path, want string
		value      bool
	}{
		{"/empty", "redirect without Location header from " + srv.URL + "/empty", true},
		{"/loop", "too many redirects fetching " + srv.URL + "/loop", true},
		{"/private", "URL blocked by SSRF protection (private/internal address): http://169.254.169.254/latest/meta-data", true},
		{"/nolocation", "Redirect response '307 Temporary Redirect' for url '" + srv.URL + "/nolocation'", false},
		{"/gone", "Client error '404 Not Found' for url '" + srv.URL + "/gone'", false},
	} {
		_, _, err := downloadURLWithCap(ctx, nil, srv.URL+c.path, 1024)
		if err == nil || !strings.HasPrefix(err.Error(), c.want) || isValueError(err) != c.value {
			t.Fatalf("%s: %v (value error %v)", c.path, err, err != nil && isValueError(err))
		}
	}
	if _, _, err := downloadURLWithCap(ctx, nil, "ftp://example.com/x", 1024); err == nil || err.Error() != "unsupported URL scheme 'ftp'; only http/https are allowed" {
		t.Fatal(err)
	}
}

func TestDownloadURLWithCapRefusesABodyOverTheCap(t *testing.T) {
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "true")
	const mb = 1024 * 1024
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size := mb
		if r.URL.Path == "/over" {
			size = mb + 1
		}
		_, _ = w.Write(bytes.Repeat([]byte("x"), size))
	}))
	defer srv.Close()
	ctx := context.Background()
	data, _, err := downloadURLWithCap(ctx, nil, srv.URL+"/exact", mb)
	if err != nil || len(data) != mb {
		t.Fatalf("%d %v", len(data), err)
	}
	_, _, err = downloadURLWithCap(ctx, nil, srv.URL+"/over", mb)
	var ve *ValueError
	if !errors.As(err, &ve) || ve.Msg != "attachment exceeds 1 MB limit" {
		t.Fatal(err)
	}
}

func TestAttachURLReportsDownloadFailures(t *testing.T) {
	toolWorkerEnv(t)
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "true")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Location", "/loop")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	loop := runKanbanTool(t, "kanban_attach_url", `{"url": " `+srv.URL+`/loop "}`)
	if errText(loop) != "kanban_attach_url: too many redirects fetching "+srv.URL+"/loop" {
		t.Fatal(loop)
	}
	gone := runKanbanTool(t, "kanban_attach_url", `{"url": "`+srv.URL+`/gone"}`)
	if !strings.HasPrefix(errText(gone), "kanban_attach_url: failed to fetch "+srv.URL+"/gone: Client error '404 Not Found'") {
		t.Fatal(gone)
	}
}

func TestAttachStoresInlineBytesAndRejectsBadBase64(t *testing.T) {
	tid := toolWorkerEnv(t)
	d := runKanbanTool(t, "kanban_attach", `{"filename": "../notes.txt", "content_base64": "aGVsbG8=", "content_type": "text/plain"}`)
	if d["ok"] != true || d["task_id"] != tid || d["size"] != 5.0 {
		t.Fatal(d)
	}
	atts := boardAttachments(t, tid)
	if len(atts) != 1 || atts[0].Filename != "notes.txt" || *atts[0].ContentType != "text/plain" || *atts[0].UploadedBy != "agent" {
		t.Fatal(atts)
	}
	bad := runKanbanTool(t, "kanban_attach", `{"filename": "x.bin", "content_base64": "not base64!"}`)
	if !strings.HasPrefix(errText(bad), "content_base64 is not valid base64: ") {
		t.Fatal(bad)
	}
	missing := runKanbanTool(t, "kanban_attach", `{"filename": "  ", "content_base64": "aGVsbG8="}`)
	if errText(missing) != "filename is required" {
		t.Fatal(missing)
	}
}

func TestAttachContentTypeBindingFailsAtTheRowInsertAndRemovesTheBlob(t *testing.T) {
	tid := toolWorkerEnv(t)
	d := runKanbanTool(t, "kanban_attach", `{"filename": "x.txt", "content_base64": "aGVsbG8=", "content_type": ["text/plain"]}`)
	if errText(d) != "kanban_attach: Error binding parameter 4: type 'list' is not supported" {
		t.Fatal(d)
	}
	if atts := boardAttachments(t, tid); len(atts) != 0 {
		t.Fatal(atts)
	}
	dir, err := TaskAttachmentsDir(context.Background(), tid, nil)
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("orphan blob %v", entries)
	}
	unnamed := runKanbanTool(t, "kanban_attach", `{"task_id": "`+tid+`", "filename": "...", "content_base64": "aGVsbG8=", "content_type": {"a": 1}}`)
	if errText(unnamed) != "kanban_attach: invalid attachment filename" {
		t.Fatal(unnamed)
	}
}
