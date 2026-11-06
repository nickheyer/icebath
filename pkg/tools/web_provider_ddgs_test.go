package tools

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

// Joins an XPath result the way the recorder printed it, elements as their bracketed tag
func ddgsXPathRendered(t *testing.T, tree *lxmlTree, ctx *html.Node, expr string) string {
	set, err := tree.xpath(ctx, expr)
	if err != nil {
		return "!" + err.Error()
	}
	var b strings.Builder
	for _, n := range set {
		if n.attr < 0 && n.n.Type == html.ElementNode {
			b.WriteString("<" + n.n.Data + ">")
			continue
		}
		b.WriteString(n.str())
	}
	return b.String()
}

// Every engine and function fixture evaluates to the strings lxml returned for the same item and expression
func TestDDGSXPathMatchesLxml(t *testing.T) {
	for _, f := range ddgsXPathRecorded {
		tree, err := lxmlFromString(f.html)
		if err != nil {
			t.Fatalf("%s: parse: %v", f.name, err)
		}
		if tree.root.Data != f.root {
			t.Errorf("%s: root %q, lxml %q", f.name, tree.root.Data, f.root)
		}
		items, err := tree.xpath(tree.root, f.items)
		if err != nil {
			t.Fatalf("%s: items: %v", f.name, err)
		}
		if len(items) != len(f.rows) {
			t.Errorf("%s: %d items, lxml %d", f.name, len(items), len(f.rows))
			continue
		}
		for i, item := range items {
			for j, expr := range f.exprs {
				got := ddgsXPathRendered(t, tree, item.n, expr)
				if got != f.rows[i][j] {
					t.Errorf("%s item %d %s = %q, lxml %q", f.name, i, expr, got, f.rows[i][j])
				}
			}
		}
	}
}

// Swaps a package variable for the duration of a test
func swapDDGS[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// Installs a fake ddgs package whose text search yields the given rows, the importable latch set
func fakeDDGSPackage(t *testing.T, rows [][3]string, fail error) {
	t.Helper()
	was := ddgsImported.Load()
	ddgsImported.Store(true)
	t.Cleanup(func() { ddgsImported.Store(was) })
	swapDDGS(t, &ddgsText, func(_ context.Context, _ string, _ int) ([]*ddgsTextResult, error) {
		if fail != nil {
			return nil, fail
		}
		var out []*ddgsTextResult
		for _, r := range rows {
			res := newDDGSTextResult()
			for i, k := range []string{"title", "href", "body"} {
				if err := res.set(k, r[i]); err != nil {
					return nil, err
				}
			}
			out = append(out, res)
		}
		return out, nil
	})
}

// Routes the bounded search through the in process helper like _force_inprocess_search
func forceInprocessDDGS(t *testing.T) {
	swapDDGS(t, &ddgsSearchBoundedFn, func(ctx context.Context, _ *Env, q string, n int) ([]WebSearchHit, error) {
		return runDDGSSearch(ctx, q, n)
	})
}

// Bounds the worker for the process tests with the given hook, timeout and interrupt probe
func ddgsWorkerKnobs(t *testing.T, hook string, timeout any, interrupted func(context.Context) bool) {
	installTestEnvHooks(t)
	fakeDDGSPackage(t, nil, nil)
	swapDDGS(t, &ddgsTestHook, hook)
	swapDDGS(t, &ddgsSearchTimeoutSecs, timeout)
	swapDDGS(t, &ddgsTerminateGraceSecs, 0.5)
	swapDDGS(t, &ddgsIsInterrupted, interrupted)
}

func assertDDGSWorkerReaped(t *testing.T) {
	t.Helper()
	p := ddgsLastWorker.Load()
	if p == nil {
		t.Fatal("expected a DDGS worker process to have been started")
	}
	if !p.poll() {
		t.Fatalf("DDGS worker still alive (pid=%d)", p.proc.Pid)
	}
}

func ddgsP() *ddgsProvider { return webProvider(nil, "ddgs").(*ddgsProvider) }

func TestDDGSProviderIsConfiguredWhenPackageImportable(t *testing.T) {
	fakeDDGSPackage(t, nil, nil)
	if !ddgsP().Available(httpxProviderEnv(t, "", nil)) {
		t.Fatal("ddgs not available with the package importable")
	}
}

func TestDDGSHappyPathNormalizesResults(t *testing.T) {
	fakeDDGSPackage(t, [][3]string{{"A", "https://a.example.com", "desc A"}, {"B", "https://b.example.com", "desc B"}, {"C", "https://c.example.com", "desc C"}}, nil)
	forceInprocessDDGS(t)
	res, _ := ddgsP().Search(context.Background(), httpxProviderEnv(t, "", nil), "q", 5)
	if !res.Success || len(res.Data.Web) != 3 {
		t.Fatalf("%+v", res)
	}
	got := jsonCompact(res.Data.Web[0])
	if got != `{"title":"A","url":"https://a.example.com","description":"desc A","position":1}` || res.Data.Web[2].Position != 3 {
		t.Fatalf("%s %+v", got, res.Data.Web[2])
	}
}

func TestDDGSEmptyResults(t *testing.T) {
	fakeDDGSPackage(t, nil, nil)
	forceInprocessDDGS(t)
	res, _ := ddgsP().Search(context.Background(), httpxProviderEnv(t, "", nil), "nothing", 5)
	out := jsonCompact(res)
	if !res.Success || out != `{"success":true,"data":{"web":[]}}` {
		t.Fatalf("%s", out)
	}
}

func TestDDGSHungSearchTimesOutAndReturnsFailure(t *testing.T) {
	ddgsWorkerKnobs(t, "sleep", 0.4, func(context.Context) bool { return false })
	start := time.Now()
	res, _ := ddgsP().Search(context.Background(), httpxProviderEnv(t, "", nil), "hangs forever", 5)
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("search did not return promptly (%s)", elapsed)
	}
	if res.Success || !strings.Contains(strings.ToLower(res.Error), "timed out") {
		t.Fatalf("%+v", res)
	}
	if res.Error != "DuckDuckGo search timed out after 0.4s — DuckDuckGo may be rate-limiting or slow. Try again later or switch to a different search provider." {
		t.Fatalf("%q", res.Error)
	}
	assertDDGSWorkerReaped(t)
}

func TestDDGSFastSearchNotAffectedByTimeoutWrapper(t *testing.T) {
	fakeDDGSPackage(t, [][3]string{{"T", "https://e.com", "B"}}, nil)
	forceInprocessDDGS(t)
	res, _ := ddgsP().Search(context.Background(), httpxProviderEnv(t, "", nil), "q", 5)
	if !res.Success || res.Data.Web[0].URL != "https://e.com" || res.Data.Web[0].Title != "T" {
		t.Fatalf("%+v", res)
	}
}

func TestDDGSGILHoldingWorkerTimesOutAndIsReaped(t *testing.T) {
	ddgsWorkerKnobs(t, "gil", 0.5, func(context.Context) bool { return false })
	start := time.Now()
	res, _ := ddgsP().Search(context.Background(), httpxProviderEnv(t, "", nil), "gil hold", 5)
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("GIL-hold search did not time out promptly (%s)", elapsed)
	}
	if res.Success || !strings.Contains(strings.ToLower(res.Error), "timed out") {
		t.Fatalf("%+v", res)
	}
	assertDDGSWorkerReaped(t)
}

func TestDDGSInterruptTerminatesWorkerPromptly(t *testing.T) {
	calls := 0
	ddgsWorkerKnobs(t, "sleep", 30, func(context.Context) bool { calls++; return calls >= 2 })
	start := time.Now()
	res, _ := ddgsP().Search(context.Background(), httpxProviderEnv(t, "", nil), "interrupt me", 5)
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("interrupt did not return promptly (%s)", elapsed)
	}
	if res.Success || res.Error != "DuckDuckGo search interrupted" {
		t.Fatalf("%+v", res)
	}
	assertDDGSWorkerReaped(t)
}

func TestDDGSNoOrphanAfterSuccessfulSearch(t *testing.T) {
	ddgsWorkerKnobs(t, "empty", 30, func(context.Context) bool { return false })
	res, _ := ddgsP().Search(context.Background(), httpxProviderEnv(t, "", nil), "q", 5)
	if !res.Success || len(res.Data.Web) != 0 {
		t.Fatalf("%+v", res)
	}
	assertDDGSWorkerReaped(t)
}

func TestDDGSBackendWiring(t *testing.T) {
	swapDDGS(t, &ddgsPackageImportable, func(context.Context) bool { return true })
	env := httpxProviderEnv(t, "", nil)
	if !isWebBackendAvailable(env, "ddgs") {
		t.Fatal("ddgs backend unavailable with the package importable")
	}
	for _, k := range []string{"FIRECRAWL_GATEWAY_URL", "TOOL_GATEWAY_DOMAIN", "TOOL_GATEWAY_USER_TOKEN", "TOOL_GATEWAY_SCHEME"} {
		t.Setenv(k, "")
	}
	if got := getWebBackend(env); got != "ddgs" {
		t.Fatalf("autodetect picked %q", got)
	}
	if !CheckWebAPIKey(httpxProviderEnv(t, "", map[string]any{"web.backend": "ddgs"})) {
		t.Fatal("check_web_api_key false with ddgs configured")
	}
}

func TestDDGSWebExtractReturnsSearchOnlyError(t *testing.T) {
	swapDDGS(t, &ddgsPackageImportable, func(context.Context) bool { return true })
	out := mustJSONMap(WebExtractTool(context.Background(), httpxProviderEnv(t, "", map[string]any{"web.backend": "ddgs"}), []any{"https://example.com"}, "", nil))
	msg := strings.ToLower(asStr(out["error"]))
	if out["success"] != false || !strings.Contains(msg, "search-only") || !(strings.Contains(msg, "duckduckgo") || strings.Contains(msg, "ddgs")) {
		t.Fatalf("%v", out)
	}
}

// Every malformed envelope raises the RuntimeError text _parse_envelope gives
func TestDDGSParseEnvelope(t *testing.T) {
	p := &ddgsWorkerProc{exited: make(chan struct{})}
	close(p.exited)
	cases := []struct{ raw, want string }{
		{"  \n", "DDGS worker exited without a result (code=None)"},
		{"not json", "DDGS worker returned invalid JSON: 'not json'"},
		{"[1, 2]", "DDGS worker returned an invalid envelope: [1, 2]"},
		{`{"ok": false, "error": "boom"}`, "boom"},
		{`{"ok": 0, "error": ""}`, "DDGS worker failed"},
		{`{"ok": true, "results": {"a": 1}}`, "DDGS worker returned non-list results"},
	}
	for _, c := range cases {
		_, err := ddgsParseEnvelope(c.raw, p)
		if err == nil || err.Error() != c.want || pyExcClass(err) != "RuntimeError" {
			t.Errorf("%q -> %v", c.raw, err)
		}
	}
	hits, err := ddgsParseEnvelope(`{"ok": true, "results": null}`, p)
	if err != nil || len(hits) != 0 {
		t.Fatalf("%v %v", hits, err)
	}
	hits, err = ddgsParseEnvelope(` {"ok": true, "results": [{"title": "t", "url": "u", "description": "d", "position": 2}]} `, p)
	if err != nil || len(hits) != 1 || hits[0] != titleHit("t", "u", "d", 2) {
		t.Fatalf("%v %v", hits, err)
	}
}

// The worker writes one envelope and exits 0, 1, 2 or 3 exactly like _search_worker.main
func TestDDGSWorkerMainExitCodes(t *testing.T) {
	fakeDDGSPackage(t, [][3]string{{"<b>T</b>", "https://e.com/a%20b", "B"}, {"T2", "https://f.com", "B2"}}, nil)
	allow := func(k string) string {
		if k == "ICEBATH_DDGS_ALLOW_TEST_HOOKS" {
			return "1"
		}
		return ""
	}
	deny := func(string) string { return "" }
	cases := []struct {
		in     string
		getenv func(string) string
		code   int
		out    string
	}{
		{"", deny, 2, `{"ok": false, "error": "invalid request: Expecting value: line 1 column 1 (char 0)"}`},
		{`{"test_hook": "empty"}`, deny, 3, `{"ok": false, "error": "test_hook refused (hooks not enabled)"}`},
		{`{"test_hook": "empty"}`, allow, 0, `{"ok": true, "results": []}`},
		{`{"test_hook": "nope"}`, allow, 1, `{"ok": false, "error": "unknown test_hook: 'nope'"}`},
		{`{"test_hook": 0, "query": "q", "safe_limit": 1}`, deny, 0, `{"ok": true, "results": [{"title": "T", "url": "https://e.com/a+b", "description": "B", "position": 1}]}`},
		{`{"query": "q", "safe_limit": "5"}`, deny, 0, `{"ok": true, "results": [{"title": "T", "url": "https://e.com/a+b", "description": "B", "position": 1}, {"title": "T2", "url": "https://f.com", "description": "B2", "position": 2}]}`},
		{`{"query": "q", "safe_limit": 0}`, deny, 0, `{"ok": true, "results": [{"title": "T", "url": "https://e.com/a+b", "description": "B", "position": 1}]}`},
		{`{"query": "q", "safe_limit": "x"}`, deny, 1, ``},
		{`[1]`, deny, 1, ``},
	}
	for _, c := range cases {
		var out bytes.Buffer
		if code := ddgsWorkerMain(context.Background(), strings.NewReader(c.in), &out, c.getenv); code != c.code || out.String() != c.out {
			t.Errorf("%s -> %d %s", c.in, code, out.String())
		}
	}
	fakeDDGSPackage(t, nil, raise("DDGSException", "No results found."))
	var out bytes.Buffer
	if code := ddgsWorkerMain(context.Background(), strings.NewReader(`{"query": "q"}`), &out, deny); code != 1 || out.String() != `{"ok": false, "error": "DDGSException: No results found."}` {
		t.Fatalf("%d %s", code, out.String())
	}
}

// Normalizers strip tags, unescape, drop control and format runes, collapse whitespace and unquote URLs
func TestDDGSNormalizers(t *testing.T) {
	text := []struct {
		in   any
		want any
	}{
		{"<b>A</b> &amp; B\u200b  C\x07\n", "A & B C"},
		{"Cafe\u0301 &lt;x&gt;", "Caf\u00e9 <x>"},
		{"", ""},
		{nil, nil},
	}
	for _, c := range text {
		if got, err := ddgsNormalizeText(c.in); err != nil || got != c.want {
			t.Errorf("text %q -> %q %v", c.in, got, err)
		}
	}
	if _, err := ddgsNormalizeText(float64(3)); err == nil || err.Error() != "expected string or bytes-like object, got 'float'" {
		t.Errorf("%v", err)
	}
	if got, _ := ddgsNormalizeURL("https://x.com/a%20b%2Fc d"); got != "https://x.com/a+b/c+d" {
		t.Errorf("url %q", got)
	}
}

// A page with no element raises the lxml ParserError ddgs reports
func TestDDGSLxmlDocumentIsEmpty(t *testing.T) {
	for _, in := range []string{"", "  \n", "<!-- c -->"} {
		if _, err := lxmlFromString(in); err == nil || err.Error() != "ParserError: Document is empty" {
			t.Errorf("%q -> %v", in, err)
		}
	}
}
