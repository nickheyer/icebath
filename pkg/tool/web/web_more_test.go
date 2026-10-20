package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPreBlocksBaseHrefAndCharsets(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/pre", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><head><base href=\"https://docs.example/v2/\"></head><body><p>Run   this:</p><pre>  indented\n    more  </pre><a href=\"guide\">guide</a></body></html>"))
	})
	mux.HandleFunc("/latin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=iso-8859-1")
		w.Write([]byte{'c', 'a', 'f', 0xe9})
	})
	mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><head><meta charset=\"windows-1252\"></head><body>" + string([]byte{0x93, 'q', 0x94}) + "</body></html>"))
	})
	mux.HandleFunc("/badcs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=klingon")
		w.Write([]byte("x"))
	})
	mux.HandleFunc("/wide", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(strings.Repeat("日", 20)))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := newFetch(t, Options{})
	out, isErr := run(t, f, `{"url":"`+srv.URL+`/pre"}`)
	if isErr || !strings.Contains(out, "Run this:\n\n```\n  indented\n    more  \n```\n\n[guide](https://docs.example/v2/guide)") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(t, f, `{"url":"`+srv.URL+`/latin"}`); isErr || !strings.HasSuffix(out, "café") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(t, f, `{"url":"`+srv.URL+`/meta"}`); isErr || !strings.HasSuffix(out, "“q”") {
		t.Fatalf("meta charset %v %q", isErr, out)
	}
	if out, isErr = run(t, f, `{"url":"`+srv.URL+`/badcs"}`); !isErr || !strings.Contains(out, "unsupported charset") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr = run(t, newFetch(t, Options{MaxText: 5}), `{"url":"`+srv.URL+`/wide"}`); isErr || !strings.HasSuffix(out, strings.Repeat("日", 5)+"\n[text cut at 5 characters]") {
		t.Fatalf("rune limit %v %q", isErr, out)
	}
	fresh := newFetch(t, Options{})
	res, err := fresh.Run(context.Background(), json.RawMessage(`{"url":"`+srv.URL+`/pre"}`))
	if err != nil {
		t.Fatal(err)
	}
	page := res.Data.(Page)
	if page.Status != 200 || page.ContentType != "text/html" || page.Charset != "utf-8" || page.Bytes == 0 || page.Duration <= 0 || page.Cached || page.FinalURL != srv.URL+"/pre" {
		t.Fatalf("%+v", page)
	}
	res, _ = fresh.Run(context.Background(), json.RawMessage(`{"url":"`+srv.URL+`/pre"}`))
	if !res.Data.(Page).Cached {
		t.Fatal("second fetch must report the cache")
	}
}

func TestCacheIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(r.URL.Path))
	}))
	defer srv.Close()
	tl := newFetch(t, Options{})
	f := tl.(*fetch)
	for i := 0; i < MaxCached+10; i++ {
		run(t, tl, `{"url":"`+srv.URL+`/p`+string(rune('a'+i%26))+string(rune('a'+i/26))+`"}`)
	}
	f.mu.Lock()
	n := len(f.cache)
	f.mu.Unlock()
	if n > MaxCached {
		t.Fatalf("%d cached pages", n)
	}
	expired := newFetch(t, Options{CacheTTL: time.Millisecond}).(*fetch)
	run(t, expired, `{"url":"`+srv.URL+`/x"}`)
	time.Sleep(5 * time.Millisecond)
	run(t, expired, `{"url":"`+srv.URL+`/y"}`)
	expired.mu.Lock()
	_, stale := expired.cache[srv.URL+"/x"]
	expired.mu.Unlock()
	if stale {
		t.Fatal("expired entries must be dropped on store")
	}
}

func TestCustomClientKeepsTheGuardAndTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(300 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	guarded, err := Fetch(Options{Client: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	if out, isErr := run(t, guarded, `{"url":"`+srv.URL+`/"}`); !isErr || !strings.Contains(out, "private address") {
		t.Fatalf("custom client must keep the guard: %v %q", isErr, out)
	}
	open, _ := Fetch(Options{Client: &http.Client{}, AllowPrivate: true, Timeout: 50 * time.Millisecond})
	if out, isErr := run(t, open, `{"url":"`+srv.URL+`/"}`); isErr || !strings.HasSuffix(out, "ok") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr := run(t, open, `{"url":"`+srv.URL+`/slow"}`); !isErr || !strings.Contains(out, "deadline") {
		t.Fatalf("custom client must honor the timeout: %v %q", isErr, out)
	}
}

func TestSameSiteRedirects(t *testing.T) {
	parse := func(s string) *url.URL {
		u, _ := url.Parse(s)
		return u
	}
	cases := map[[2]string]bool{
		{"https://example.com/a", "https://example.com:8443/b"}: true,
		{"https://example.com/a", "https://www.example.com/b"}:  true,
		{"https://www.example.com/a", "https://example.com/b"}:  true,
		{"https://example.com/a", "https://other.com/b"}:        false,
		{"https://example.com/a", "https://api.example.com/b"}:  false,
	}
	for pair, want := range cases {
		if got := sameSite(parse(pair[0]), parse(pair[1])); got != want {
			t.Fatalf("%v: got %v", pair, got)
		}
	}
	if got := TextFrom(`<a href="../x">t</a>`, parse("https://e.com/a/b/c")); got != "[t](https://e.com/a/x)" {
		t.Fatalf("%q", got)
	}
	if got := TextFrom(`<a href="::bad">t</a>`, parse("https://e.com/")); got != "[t](::bad)" {
		t.Fatalf("%q", got)
	}
	if got := Text("<pre>never closed"); got != "```\nnever closed\n```" {
		t.Fatalf("%q", got)
	}
	if got := title("<html><TITLE> A &amp;  B </TITLE>"); got != "A & B" {
		t.Fatalf("%q", got)
	}
	if title("<title>open") != "" || title("none") != "" {
		t.Fatal("unterminated titles")
	}
}
