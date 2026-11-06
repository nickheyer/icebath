package pyhttpx

import (
	"bytes"
	"compress/gzip"
	"errors"
	"github.com/andybalholm/brotli"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMergeParamsKeepsOrderAndReplacesKeys(t *testing.T) {
	got := MergeParams("https://h/p?a=1&b=x+y#frag", Params{{"b", "new val"}, {"c", "&="}})
	if want := "https://h/p?a=1&b=new+val&c=%26%3D#frag"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := MergeParams("https://h/p", nil); got != "https://h/p" {
		t.Fatalf("no params changed the URL to %q", got)
	}
}

func TestDecodeTextUsesCharsetThenUTF8Replace(t *testing.T) {
	h := http.Header{"Content-Type": {"text/plain; charset=ISO-8859-1"}}
	if got := DecodeText(h, []byte{0xe9}); got != "é" {
		t.Fatalf("latin-1 decode gave %q", got)
	}
	if got := DecodeText(http.Header{}, []byte("a\xffb")); got != "a�b" {
		t.Fatalf("utf-8 replace gave %q", got)
	}
}

func TestDoReadsBodyAndTimesOutPerPhase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(300 * time.Millisecond)
		}
		if r.URL.Path == "/redir" {
			http.Redirect(w, r, "/ok", http.StatusFound)
			return
		}
		w.Write([]byte("\xef\xbb\xbf{\"q\": \"" + r.URL.Query().Get("q") + "\"}"))
	}))
	defer srv.Close()
	req, _ := http.NewRequest("GET", MergeParams(srv.URL+"/ok", Params{{"q", "v"}}), nil)
	resp, err := Do(PlainClient(false), req, time.Second)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("Do: %v %v", resp, err)
	}
	if v, err := resp.JSON(); err != nil || v == nil {
		t.Fatalf("JSON: %v %v", v, err)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/slow", nil)
	_, err = Do(PlainClient(false), req, 50*time.Millisecond)
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("slow response gave %v, want TimeoutError", err)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/redir", nil)
	if resp, err := Do(PlainClient(false), req, time.Second); err != nil || resp.StatusCode != 302 {
		t.Fatalf("unfollowed redirect gave %v %v", resp, err)
	}
	req, _ = http.NewRequest("GET", srv.URL+"/redir", nil)
	if resp, err := Do(PlainClient(true), req, time.Second); err != nil || resp.StatusCode != 200 {
		t.Fatalf("followed redirect gave %v %v", resp, err)
	}
}

func TestRetryAfterSecondsAndHeaderGet(t *testing.T) {
	h := http.Header{}
	if _, ok := RetryAfterSeconds(h); ok {
		t.Fatal("absent header parsed")
	}
	for raw, want := range map[string]float64{"5": 5, " 2.5 ": 2.5, "-3": 0, "1_0": 10, "nan": 0} {
		h.Set("Retry-After", raw)
		if got, ok := RetryAfterSeconds(h); !ok || got != want {
			t.Fatalf("%q -> %v %v", raw, got, ok)
		}
	}
	h.Set("retry-after", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	if got, ok := RetryAfterSeconds(h); !ok || got != 0 {
		t.Fatalf("past date -> %v %v", got, ok)
	}
	h.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat))
	if got, ok := RetryAfterSeconds(h); !ok || got < 25 || got > 31 {
		t.Fatalf("future date -> %v %v", got, ok)
	}
	h.Set("Retry-After", "soon")
	if _, ok := RetryAfterSeconds(h); ok {
		t.Fatal("garbage parsed")
	}
	h.Add("X-A", "1")
	h.Add("X-A", "2")
	if HeaderGet(h, "x-a", "") != "1, 2" || HeaderGet(h, "X-B", "d") != "d" {
		t.Fatal("HeaderGet joins repeated values")
	}
}

func TestDecodeContentUndoesEncodingsLikeHttpx(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte(`{"skills": []}`))
	w.Close()
	h := http.Header{}
	h.Set("Content-Encoding", "gzip")
	if out, err := DecodeContent(h, gz.Bytes()); err != nil || string(out) != `{"skills": []}` {
		t.Fatal(string(out), err)
	}
	var br bytes.Buffer
	bw := brotli.NewWriter(&br)
	bw.Write([]byte("brotli body"))
	bw.Close()
	h.Set("Content-Encoding", "br")
	if out, err := DecodeContent(h, br.Bytes()); err != nil || string(out) != "brotli body" {
		t.Fatal(string(out), err)
	}
	var de *DecodingError
	if _, err := DecodeContent(h, []byte("not brotli at all")); !errors.As(err, &de) {
		t.Fatal(err)
	}
	if out, _ := DecodeContent(http.Header{}, []byte("plain")); string(out) != "plain" {
		t.Fatal(string(out))
	}
}

func TestRetryAfterValueMatchesParseRetryAfterSeconds(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want float64
		ok   bool
	}{{nil, 0, false}, {true, 0, false}, {4, 4, true}, {int64(-3), 0, true}, {2.5, 2.5, true}, {" 7 ", 7, true}, {"", 0, false}, {"soon", 0, false}, {[]any{}, 0, false}, {map[string]any{"retry-after": "3"}, 3, true}, {map[string]any{"Retry-After": map[string]any{}}, 0, false}} {
		if got, ok := RetryAfterValue(tc.in); got != tc.want || ok != tc.ok {
			t.Fatalf("%v -> %v %v", tc.in, got, ok)
		}
	}
}
