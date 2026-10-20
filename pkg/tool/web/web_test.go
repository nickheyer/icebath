package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
)

func run(t *testing.T, tl tool.Tool, args string) (string, bool) {
	t.Helper()
	res, err := tl.Run(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(protocol.Text).Text, res.IsError
}

func newFetch(t *testing.T, o Options) tool.Tool {
	t.Helper()
	o.AllowPrivate = true
	f, err := Fetch(o)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func server(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><head><title>Hello &amp; Welcome</title><style>p{}</style></head><body><script>alert(1)</script><h1>Top</h1><p>Some   text with a <a href="/next">link</a>.</p><ul><li>one</li><li>two</li></ul><!-- hidden --><img alt="logo"><p>Tail</p></body></html>`))
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("x", 100)))
	})
	mux.HandleFunc("/bin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write([]byte{0, 1, 2})
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/json", http.StatusFound) })
	mux.HandleFunc("/away", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/elsewhere", http.StatusFound)
	})
	mux.HandleFunc("/ua", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(r.Header.Get("User-Agent")))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestFetchConvertsHTML(t *testing.T) {
	srv, hits := server(t)
	f := newFetch(t, Options{})
	out, isErr := run(t, f, `{"url":"`+srv.URL+`/page"}`)
	if isErr {
		t.Fatal(out)
	}
	want := "URL: " + srv.URL + "/page\nContent-Type: text/html\n\n# Hello & Welcome\n\n# Top\n\nSome text with a [link](/next).\n\n- one\n- two\n[image: logo]\nTail"
	if out != want {
		t.Fatalf("%q", out)
	}
	run(t, f, `{"url":"`+srv.URL+`/page"}`)
	if hits.Load() != 1 {
		t.Fatalf("cache missed, %d hits", hits.Load())
	}
	if out, _ := run(t, newFetch(t, Options{NoCache: true}), `{"url":"`+srv.URL+`/page"}`); !strings.Contains(out, "Top") || hits.Load() != 2 {
		t.Fatalf("no cache must refetch: %d", hits.Load())
	}
	if out, isErr := run(t, f, `{"url":"`+srv.URL+`/json"}`); isErr || !strings.HasSuffix(out, `{"ok":true}`) {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, _ := run(t, newFetch(t, Options{UserAgent: "bot/1"}), `{"url":"`+srv.URL+`/ua"}`); !strings.HasSuffix(out, "bot/1") {
		t.Fatalf("%q", out)
	}
}

func TestFetchLimitsAndFailures(t *testing.T) {
	srv, _ := server(t)
	if out, isErr := run(t, newFetch(t, Options{MaxBytes: 40}), `{"url":"`+srv.URL+`/big"}`); isErr || !strings.HasSuffix(out, strings.Repeat("x", 40)+"\n[page cut at 40 bytes]") {
		t.Fatalf("%v %q", isErr, out)
	}
	if out, isErr := run(t, newFetch(t, Options{MaxText: 10}), `{"url":"`+srv.URL+`/big"}`); isErr || !strings.HasSuffix(out, "xxxxxxxxxx\n[text cut at 10 characters]") {
		t.Fatalf("%v %q", isErr, out)
	}
	f := newFetch(t, Options{})
	if out, isErr := run(t, f, `{"url":"`+srv.URL+`/hop"}`); isErr || !strings.HasSuffix(out, `{"ok":true}`) || !strings.HasPrefix(out, "URL: "+srv.URL+"/json") {
		t.Fatalf("same host redirect: %v %q", isErr, out)
	}
	for path, want := range map[string]string{"/away": "another host", "/missing": "HTTP 404", "/bin": "unsupported content type"} {
		if out, isErr := run(t, f, `{"url":"`+srv.URL+path+`"}`); !isErr || !strings.Contains(out, want) {
			t.Fatalf("%s: %v %q", path, isErr, out)
		}
	}
	for args, want := range map[string]string{`{"url":"ftp://x"}`: "absolute http", `{"url":"/relative"}`: "absolute http", `{"url":"` + srv.URL + `/json","prompt":"q"}`: "prompt is not supported", `{"url":1}`: "cannot unmarshal"} {
		if out, isErr := run(t, f, args); !isErr || !strings.Contains(out, want) {
			t.Fatalf("%s: %v %q", args, isErr, out)
		}
	}
	guarded, err := Fetch(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if out, isErr := run(t, guarded, `{"url":"`+srv.URL+`/json"}`); !isErr || !strings.Contains(out, "private address") {
		t.Fatalf("loopback must be refused by default: %v %q", isErr, out)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	if out, isErr := run(t, newFetch(t, Options{Timeout: 50 * time.Millisecond}), `{"url":"`+slow.URL+`"}`); !isErr || !strings.Contains(out, "deadline") && !strings.Contains(out, "Timeout") {
		t.Fatalf("%v %q", isErr, out)
	}
}

type fakeModel struct {
	req  protocol.Request
	resp protocol.Response
	err  error
}

func (m *fakeModel) Complete(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	m.req = req
	return m.resp, m.err
}

func TestFetchWithPrompt(t *testing.T) {
	srv, _ := server(t)
	if _, err := Fetch(Options{Summarizer: &fakeModel{}}); err == nil {
		t.Fatal("summarizer without model accepted")
	}
	m := &fakeModel{resp: protocol.Response{Content: []protocol.Part{protocol.Text{Text: "It says "}, protocol.Text{Text: "hello"}}}}
	f := newFetch(t, Options{Summarizer: m, Model: "small", MaxSummaryTokens: 77})
	def := f.Definition()
	if !strings.Contains(string(def.Parameters), `"prompt"`) || !strings.Contains(def.Description, "prompt") {
		t.Fatalf("%s", def.Parameters)
	}
	if !strings.Contains(string(newFetch(t, Options{}).Definition().Parameters), `"required":["url"]`) {
		t.Fatal("prompt must be absent without a summarizer")
	}
	out, isErr := run(t, f, `{"url":"`+srv.URL+`/page","prompt":"What does it say?"}`)
	if isErr || out != "It says hello" {
		t.Fatalf("%v %q", isErr, out)
	}
	if m.req.Model != "small" || m.req.MaxTokens != 77 || !strings.Contains(m.req.Messages[0].Content[0].(protocol.Text).Text, "Prompt: What does it say?") || !strings.Contains(m.req.Messages[0].Content[0].(protocol.Text).Text, "# Top") {
		t.Fatalf("%+v", m.req)
	}
	if out, isErr := run(t, f, `{"url":"`+srv.URL+`/page"}`); !isErr || !strings.Contains(out, "prompt is required") {
		t.Fatalf("%v %q", isErr, out)
	}
	m.err = errors.New("down")
	if out, isErr := run(t, f, `{"url":"`+srv.URL+`/page","prompt":"q"}`); !isErr || !strings.Contains(out, "down") {
		t.Fatalf("%v %q", isErr, out)
	}
	m.err, m.resp = nil, protocol.Response{}
	if out, isErr := run(t, f, `{"url":"`+srv.URL+`/page","prompt":"q"}`); !isErr || !strings.Contains(out, "no text") {
		t.Fatalf("%v %q", isErr, out)
	}
}

func TestHTMLText(t *testing.T) {
	cases := map[string]string{
		`<p>a</p><p>b</p>`:                                        "a\n\nb",
		`<div>x<br>y</div>`:                                       "x\ny",
		`<a href='http://e.com/x'>  go  there </a>`:               "[go there](http://e.com/x)",
		`<a href="javascript:void(0)">nope</a>`:                   "nope",
		`<a href="/x"></a>`:                                       "/x",
		`<A HREF=/bare>b</A>`:                                     "[b](/bare)",
		`<h2>T</h2>body`:                                          "## T\n\nbody",
		`<script>var x = "<p>";</script>after`:                    "after",
		`<SCRIPT type="x">a</SCRIPT>b`:                            "b",
		`<style>p{}</style><noscript>n</noscript>c`:               "c",
		`<table><tr><td>a</td><td>b</td></tr><tr><td>c</td></tr>`: "a b\n\nc",
		`&lt;tag&gt; &amp; &#169;`:                                "<tag> & ©",
		`<p title=">">x</p>`:                                      "x",
		`<!-- c --><p>d`:                                          "d",
		`<img alt="pic" src="x.png"><img src="y.png">`:            "[image: pic]",
		`<script>never closed`:                                    "",
		`<p>a<`:                                                   "a",
		`<a href="/x" data-href="/y">t</a>`:                       "[t](/x)",
	}
	for in, want := range cases {
		if got := Text(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
