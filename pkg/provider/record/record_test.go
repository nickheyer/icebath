package record

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func server(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte{0, 255, 1})
		case "/fail":
			w.WriteHeader(http.StatusTeapot)
			io.WriteString(w, "no")
		default:
			w.Header().Set("X-Hit", string(rune('0'+n)))
			io.WriteString(w, "reply "+string(body)+" #"+string(rune('0'+n)))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func post(t *testing.T, c *http.Client, url, body string, header http.Header) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for k, vs := range header {
		req.Header[k] = vs
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), resp.Header
}

func TestRecordThenReplay(t *testing.T) {
	srv, hits := server(t)
	path := filepath.Join(t.TempDir(), "nested", "cassette.json")
	c, err := Open(path, Record)
	if err != nil {
		t.Fatal(err)
	}
	c.Redact = []string{"X-Secret"}
	client := c.Client(nil)
	auth := http.Header{"Authorization": {"Bearer s3cret"}, "X-Secret": {"shh"}, "X-Keep": {"yes"}}
	if _, body, h := post(t, client, srv.URL+"/a", "one", auth); body != "reply one #1" || h.Get("X-Hit") != "1" {
		t.Fatal(body)
	}
	if _, body, _ := post(t, client, srv.URL+"/a", "one", nil); body != "reply one #2" {
		t.Fatal(body)
	}
	if _, body, _ := post(t, client, srv.URL+"/a", "two", nil); body != "reply two #3" {
		t.Fatal(body)
	}
	if status, body, _ := post(t, client, srv.URL+"/fail", "", nil); status != http.StatusTeapot || body != "no" {
		t.Fatal(status, body)
	}
	if _, body, _ := post(t, client, srv.URL+"/bin", "", nil); body != "\x00\xff\x01" {
		t.Fatalf("%q", body)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("s3cret")) || bytes.Contains(raw, []byte("shh")) || !bytes.Contains(raw, []byte(`"X-Keep"`)) {
		t.Fatalf("redaction %s", raw)
	}
	if err := Valid(path); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 5 || len(c.Exchanges()) != 5 || !c.Exchanges()[4].ResponseBase64 || c.Exchanges()[0].Response != "reply one #1" {
		t.Fatalf("%d %+v", hits.Load(), c.Exchanges())
	}
	if urls := c.URLs(); len(urls) != 3 || urls[0] != srv.URL+"/a" {
		t.Fatalf("%v", urls)
	}
	replay, err := Open(path, Replay)
	if err != nil {
		t.Fatal(err)
	}
	client = replay.Client(nil)
	if _, body, h := post(t, client, srv.URL+"/a", "one", nil); body != "reply one #1" || h.Get("X-Hit") != "1" {
		t.Fatal(body)
	}
	if _, body, _ := post(t, client, srv.URL+"/a", "one", nil); body != "reply one #2" {
		t.Fatal("same request must replay in recorded order:", body)
	}
	if _, body, _ := post(t, client, srv.URL+"/a", "two", nil); body != "reply two #3" {
		t.Fatal(body)
	}
	if status, body, _ := post(t, client, srv.URL+"/fail", "", nil); status != http.StatusTeapot || body != "no" {
		t.Fatal(status, body)
	}
	if _, body, _ := post(t, client, srv.URL+"/bin", "", nil); body != "\x00\xff\x01" {
		t.Fatalf("%q", body)
	}
	if hits.Load() != 5 {
		t.Fatal("replay hit the network")
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/a", strings.NewReader("one"))
	if _, err := client.Do(req); err == nil || !strings.Contains(err.Error(), "no recorded exchange") {
		t.Fatalf("exhausted recordings must fail: %v", err)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/new", nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("unknown request replayed")
	}
	if err := replay.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestAutoModeRecordsOnlyNewExchanges(t *testing.T) {
	srv, hits := server(t)
	path := filepath.Join(t.TempDir(), "auto.json")
	c, _ := Open(path, Auto)
	if c.Mode() != Auto {
		t.Fatal(c.Mode())
	}
	client := c.Client(nil)
	post(t, client, srv.URL+"/a", "x", nil)
	c.Save()
	again, err := Open(path, Auto)
	if err != nil {
		t.Fatal(err)
	}
	client = again.Client(nil)
	if _, body, _ := post(t, client, srv.URL+"/a", "x", nil); body != "reply x #1" || hits.Load() != 1 {
		t.Fatal("auto must replay what it has")
	}
	if _, body, _ := post(t, client, srv.URL+"/b", "y", nil); body != "reply y #2" || hits.Load() != 2 {
		t.Fatal("auto must record what it lacks")
	}
	again.Save()
	final, _ := Open(path, Replay)
	if len(final.Exchanges()) != 2 {
		t.Fatalf("%+v", final.Exchanges())
	}
}

func TestOpenAndValidFailures(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(filepath.Join(dir, "missing.json"), Replay); err == nil {
		t.Fatal("replay without a cassette accepted")
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{"), 0o644)
	if _, err := Open(bad, Auto); err == nil {
		t.Fatal("corrupt cassette accepted")
	}
	if err := Valid(bad); err == nil {
		t.Fatal("corrupt cassette validated")
	}
	incomplete := filepath.Join(dir, "incomplete.json")
	os.WriteFile(incomplete, []byte(`[{"method":"GET"}]`), 0o644)
	if err := Valid(incomplete); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("%v", err)
	}
	broken := filepath.Join(dir, "broken.json")
	os.WriteFile(broken, []byte(`[{"method":"GET","url":"u","status":200,"response":"!!","response_base64":true}]`), 0o644)
	if err := Valid(broken); err == nil {
		t.Fatal("bad base64 validated")
	}
	if err := Valid(filepath.Join(dir, "none")); err == nil {
		t.Fatal("missing file validated")
	}
	if _, err := Open(broken, Replay); err == nil {
		t.Fatal("cassette with a bad body opened")
	}
	badReq := filepath.Join(dir, "badreq.json")
	os.WriteFile(badReq, []byte(`[{"method":"GET","url":"u","status":200,"body":"!!","body_base64":true}]`), 0o644)
	if _, err := Open(badReq, Auto); err == nil || !strings.Contains(err.Error(), "request body") {
		t.Fatalf("%v", err)
	}
	c, _ := Open(filepath.Join(dir, "r.json"), Record)
	rt := c.Transport(roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, io.ErrUnexpectedEOF }))
	req, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
	if _, err := rt.RoundTrip(req); err != io.ErrUnexpectedEOF {
		t.Fatalf("%v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRedactsResponseHeadersAndQueryCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=topsecret")
		w.Header().Set("X-Keep", "yes")
		io.WriteString(w, "q="+r.URL.Query().Get("q"))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "q.json")
	c, _ := Open(path, Record)
	c.RedactQuery = []string{"custom"}
	client := c.Client(nil)
	target := srv.URL + "/v1?key=AIzaSecret&q=hello&custom=mine"
	if _, body, h := post(t, client, target, "", nil); body != "q=hello" || h.Get("Set-Cookie") != "session=topsecret" {
		t.Fatalf("%q %v", body, h)
	}
	c.Save()
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("AIzaSecret")) || bytes.Contains(raw, []byte("topsecret")) || bytes.Contains(raw, []byte("mine")) || !bytes.Contains(raw, []byte("q=hello")) || !bytes.Contains(raw, []byte(`"X-Keep"`)) {
		t.Fatalf("%s", raw)
	}
	replay, err := Open(path, Replay)
	if err != nil {
		t.Fatal(err)
	}
	replay.RedactQuery = []string{"custom"}
	if _, body, h := post(t, replay.Client(nil), srv.URL+"/v1?key=OtherKey&q=hello&custom=yours", "", nil); body != "q=hello" || h.Get("Set-Cookie") != "" || h.Get("X-Keep") != "yes" {
		t.Fatalf("a redacted parameter must not take part in matching: %q %v", body, h)
	}
	if _, err := replay.Client(nil).Do(must(http.NewRequest(http.MethodPost, srv.URL+"/v1?key=OtherKey&q=other", nil))); err == nil {
		t.Fatal("other parameters still distinguish exchanges")
	}
	if urls := replay.URLs(); len(urls) != 1 || strings.Contains(urls[0], "AIzaSecret") || !strings.Contains(urls[0], "key=redacted") {
		t.Fatalf("%v", urls)
	}
}

func must(r *http.Request, err error) *http.Request {
	if err != nil {
		panic(err)
	}
	return r
}

func TestRecordingDeliversStreamedChunksBeforeTheBodyEnds(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("first\n"))
		w.(http.Flusher).Flush()
		<-release
		w.Write([]byte("second\n"))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "stream.json")
	c, err := Open(path, Record)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Client(nil).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	done := make(chan string, 1)
	go func() {
		n, _ := resp.Body.Read(buf)
		done <- string(buf[:n])
	}()
	select {
	case got := <-done:
		if got != "first\n" {
			t.Fatalf("%q", got)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("first chunk was held until the body ended")
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(rest) != "second\n" {
		t.Fatalf("%q", rest)
	}
	if ex := c.Exchanges(); len(ex) != 1 || ex[0].Response != "first\nsecond\n" {
		t.Fatalf("%+v", ex)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	replay, err := Open(path, Replay)
	if err != nil {
		t.Fatal(err)
	}
	again, err := replay.Client(nil).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(again.Body)
	if string(data) != "first\nsecond\n" {
		t.Fatalf("%q", data)
	}
}
