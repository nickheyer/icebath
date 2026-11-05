package pyhttpx

import (
	"errors"
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
