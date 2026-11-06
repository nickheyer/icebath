package skills

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hub whose policy checks pass and whose guarded hops answer from fn, the patched module attributes
func publicHub(t *testing.T, fn func(url string) *pyhttpx.Response) (*Hub, *[]string) {
	h, _ := newTestHub(t)
	var calls []string
	h.io.isSafeURL = func(string) bool { return true }
	h.io.siteAccess = func(string) *tools.WebsiteBlock { return nil }
	h.io.readIndexCache = func(string) (any, error) { return nil, nil }
	h.io.writeIndexCache = func(string, any) error { return nil }
	h.io.ssrfGet = func(_ context.Context, url string, _ time.Duration, _ map[string]string) (*pyhttpx.Response, error) {
		calls = append(calls, url)
		return fn(url), nil
	}
	return h, &calls
}

func jsonResp(t *testing.T, body string) *pyhttpx.Response {
	if _, err := pyjson.Loads(body); err != nil {
		t.Fatal(err)
	}
	return &pyhttpx.Response{StatusCode: 200, Header: http.Header{}, Content: []byte(body)}
}

func textResp(status int, body string) *pyhttpx.Response {
	return &pyhttpx.Response{StatusCode: status, Header: http.Header{}, Content: []byte(body)}
}

func TestWellKnownSearchReadsIndexFromWellKnownURL(t *testing.T) {
	h, _ := publicHub(t, func(string) *pyhttpx.Response {
		return jsonResp(t, `{"skills": [{"name": "git-workflow", "description": "Git rules", "files": ["SKILL.md"]},
			{"name": "code-review", "description": "Review code", "files": ["SKILL.md", "references/checklist.md"]}]}`)
	})
	results, err := NewWellKnownSkillSource(h).Search(context.Background(), "https://example.com/.well-known/skills/index.json", 10)
	if err != nil || len(results) != 2 {
		t.Fatalf("search gave %v %v", results, err)
	}
	for i, want := range []string{"well-known:https://example.com/.well-known/skills/git-workflow", "well-known:https://example.com/.well-known/skills/code-review"} {
		if results[i].Identifier != want || results[i].Source != "well-known" {
			t.Fatalf("result %d is %+v", i, results[i])
		}
	}
}

func TestWellKnownFetchRejectsUnsafeFilePaths(t *testing.T) {
	h, calls := publicHub(t, func(url string) *pyhttpx.Response {
		switch {
		case strings.HasSuffix(url, "/index.json"):
			return jsonResp(t, `{"skills": [{"name": "code-review", "description": "Review code", "files": ["SKILL.md", "../../../escape.txt"]}]}`)
		case strings.HasSuffix(url, "/code-review/SKILL.md"):
			return textResp(200, "# Code Review\n")
		}
		t.Fatalf("unexpected URL %s", url)
		return nil
	})
	b, err := NewWellKnownSkillSource(h).Fetch(context.Background(), "well-known:https://example.com/.well-known/skills/code-review")
	if err != nil || b != nil {
		t.Fatalf("fetch gave %v %v", b, err)
	}
	for _, c := range *calls {
		if strings.Contains(c, "escape") {
			t.Fatalf("unsafe path was fetched: %s", c)
		}
	}
}

func TestUrlSourceMatchesBareMDURLAndLeavesWellKnownURLs(t *testing.T) {
	u := NewUrlSource(NewHub(NewScope(t.TempDir(), nil)))
	if !u.matches("https://example.com/path/SKILL.md") {
		t.Fatal("bare SKILL.md URL not claimed")
	}
	for _, id := range []string{"https://example.com/.well-known/skills/git-workflow/SKILL.md", "https://example.com/.well-known/skills/index.json"} {
		if u.matches(id) {
			t.Fatalf("%s claimed", id)
		}
	}
}

func TestUrlSourceBlocksPrivateURLsAndRedirects(t *testing.T) {
	h, calls := publicHub(t, func(string) *pyhttpx.Response { return textResp(200, "x") })
	h.io.isSafeURL = func(string) bool { return false }
	u := NewUrlSource(h)
	if m, err := u.Inspect(context.Background(), "http://127.0.0.1/SKILL.md"); m != nil || err != nil {
		t.Fatalf("inspect gave %v %v", m, err)
	}
	if b, err := u.Fetch(context.Background(), "http://127.0.0.1/SKILL.md"); b != nil || err != nil {
		t.Fatalf("fetch gave %v %v", b, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("private URL was requested %v", *calls)
	}
	safe := []bool{true, false}
	h.io.isSafeURL = func(string) bool { v := safe[0]; safe = safe[1:]; return v }
	h.io.ssrfGet = func(_ context.Context, url string, _ time.Duration, _ map[string]string) (*pyhttpx.Response, error) {
		*calls = append(*calls, url)
		return &pyhttpx.Response{StatusCode: 302, Header: http.Header{"Location": {"http://127.0.0.1/private/SKILL.md"}}}, nil
	}
	if b, err := u.Fetch(context.Background(), "https://example.com/SKILL.md"); b != nil || err != nil || len(*calls) != 1 {
		t.Fatalf("redirect fetch gave %v %v after %v", b, err, *calls)
	}
}

func TestUrlSourceFetchSkipsMissingSupportFileInsteadOfAborting(t *testing.T) {
	md := "---\nname: my-skill\ndescription: Has support files.\n---\n\nSee `references/good.md` and `references/missing.md`.\n"
	h, _ := publicHub(t, func(url string) *pyhttpx.Response {
		switch {
		case strings.HasSuffix(url, "SKILL.md"):
			return textResp(200, md)
		case strings.HasSuffix(url, "references/good.md"):
			return textResp(200, "good content")
		case strings.HasSuffix(url, "references/missing.md"):
			return textResp(404, "")
		}
		t.Fatalf("unexpected URL %s", url)
		return nil
	})
	b, err := NewUrlSource(h).Fetch(context.Background(), "https://example.com/my-skill/SKILL.md")
	if err != nil || b == nil || b.Name != "my-skill" {
		t.Fatalf("fetch gave %+v %v", b, err)
	}
	if got, _ := b.File("SKILL.md"); string(got) != md {
		t.Fatalf("SKILL.md %q", got)
	}
	if got, _ := b.File("references/good.md"); string(got) != "good content" {
		t.Fatalf("good.md %q", got)
	}
	if _, ok := b.File("references/missing.md"); ok {
		t.Fatal("missing support file is in the bundle")
	}
}

func TestUrlSourceIsValidSkillNameRejectsSentinelAndGarbage(t *testing.T) {
	for _, n := range []string{"", "SKILL", "skill", "README", "readme", "INDEX", "index", "unnamed-skill", "../evil", "a/b",
		"has space", "has.dot", "-leading-dash", "1-leading-digit"} {
		if urlIsValidSkillName(n) {
			t.Fatalf("accepted %q", n)
		}
	}
	for _, fmName := range []any{nil, int64(123), []any{"list"}} {
		if got := urlResolveSkillName(map[string]any{"name": fmName}, "https://x.com/repo/SKILL.md"); got == nil || *got != "repo" {
			t.Fatalf("frontmatter name %v resolved %v", fmName, got)
		}
	}
	if got := urlResolveSkillName(map[string]any{}, "https://x.com/SKILL.md"); got != nil {
		t.Fatalf("bare SKILL.md resolved %q", *got)
	}
}

func TestLobeHubConvertToSkillMDBasicConversion(t *testing.T) {
	data, _ := pyjson.Loads(`{"identifier": "test-agent", "meta": {"title": "Test Agent", "description": "A test agent.",
		"tags": ["testing", "demo"]}, "config": {"systemRole": "You are a helpful test agent."}}`)
	md := lobeHubConvertToSkillMD(data)
	for _, want := range []string{"---", "name: test-agent", "description: A test agent.", "tags: [testing, demo]", "# Test Agent",
		"You are a helpful test agent."} {
		if !strings.Contains(md, want) {
			t.Fatalf("missing %q in %q", want, md)
		}
	}
}

func TestHubIndexCacheAndPlainGets(t *testing.T) {
	h, _ := newTestHub(t)
	if err := h.writeIndexCache("k", []any{"é"}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(h.IndexCacheDir(), "k.json")); string(raw) != `["é"]` {
		t.Fatalf("cache file %q", raw)
	}
	if _, err := os.Stat(filepath.Join(h.HubDir(), ".ignore")); err != nil {
		t.Fatal("no .ignore written")
	}
	if v, err := h.readIndexCache("k"); err != nil || v == nil {
		t.Fatalf("fresh read gave %v %v", v, err)
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(filepath.Join(h.IndexCacheDir(), "k.json"), old, old)
	if v, _ := h.readIndexCache("k"); v != nil {
		t.Fatalf("stale read gave %v", v)
	}
	if err := h.cacheMetas("m", []SkillMeta{{Name: "a", Source: "s", Identifier: "i", TrustLevel: "community", Tags: []string{"t"}}}); err != nil {
		t.Fatal(err)
	}
	if metas, ok, err := h.cachedMetas("m"); err != nil || !ok || metas[0].Name != "a" || metas[0].Tags[0] != "t" {
		t.Fatalf("cached metas %v %v %v", metas, ok, err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			w.Write([]byte(`{"q": "` + r.URL.Query().Get("q") + `"}`))
		case "/hop":
			http.Redirect(w, r, "/json?q=redirected", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if v, err := h.getJSON(ctx, srv.URL+"/json", hubGet{Params: pyhttpx.Params{{"q", "v"}}}); err != nil || v.(*pyjson.Object).Vals["q"] != "v" {
		t.Fatalf("getJSON gave %v %v", v, err)
	}
	if v, err := h.getJSON(ctx, srv.URL+"/missing", hubGet{}); v != nil || err != nil {
		t.Fatalf("404 getJSON gave %v %v", v, err)
	}
	if _, ok, _ := h.getText(ctx, srv.URL+"/hop", hubGet{}); ok {
		t.Fatal("unfollowed redirect read as 200")
	}
	if text, ok, _ := h.getText(ctx, srv.URL+"/hop", hubGet{FollowRedirects: true}); !ok || !strings.Contains(text, "redirected") {
		t.Fatalf("followed redirect gave %q %v", text, ok)
	}
	h.io.isSafeURL = func(string) bool { return true }
	h.io.siteAccess = func(string) *tools.WebsiteBlock { return nil }
	h.io.ssrfGet = func(ctx context.Context, url string, timeout time.Duration, headers map[string]string) (*pyhttpx.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		return pyhttpx.Do(pyhttpx.PlainClient(false), req, timeout)
	}
	if text, ok := h.fetchText(ctx, srv.URL+"/hop"); !ok || !strings.Contains(text, "redirected") {
		t.Fatalf("guarded redirect gave %q %v", text, ok)
	}
	h.io.siteAccess = func(string) *tools.WebsiteBlock { return &tools.WebsiteBlock{Host: "127.0.0.1", Rule: "127.0.0.1"} }
	if _, ok := h.fetchText(ctx, srv.URL+"/json"); ok {
		t.Fatal("website policy block was ignored")
	}
}

const provenanceSkillMD = "---\nname: demo-bundle\ndescription: A multi-file test skill.\n---\n# Demo\n\nRead [the guide](references/guide.md#usage), use `templates/report.md?raw=1`, and run\n`scripts/run.py`, `references/foo%23bar.md`, and `references/my%20guide.md`. See\n`examples/endpoint-inventory.md`. The repository also\ncontains assets/logo.png.\n"

// Loopback file server of the upstream fixture repo with private URLs allowed, the served_repo fixture
func servedRepo(t *testing.T) (*Hub, string, string) {
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "true")
	h, tmp := newTestHub(t)
	h.io.isSafeURL = func(string) bool { return true }
	h.io.siteAccess = func(string) *tools.WebsiteBlock { return nil }
	repo := filepath.Join(tmp, "upstream")
	files := map[string]string{"SKILL.md": provenanceSkillMD, "references/guide.md": "safe guide\n", "references/foo#bar.md": "encoded delimiter\n",
		"references/my guide.md": "encoded space\n", "templates/report.md": "report\n", "scripts/run.py": "print('ok')\n",
		"assets/logo.png": "\x89PNG\r\n\x1a\n\x00\xff", "examples/endpoint-inventory.md": "example\n",
		"examples/not-installed.md": "must not be copied\n", "README.md": "must not be copied\n"}
	for rel, content := range files {
		writeTestFile(t, filepath.Join(repo, filepath.FromSlash(rel)), content)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(repo)))
	t.Cleanup(srv.Close)
	return h, repo, srv.URL + "/SKILL.md"
}

func bundlePaths(b *SkillBundle) map[string]string {
	out := map[string]string{}
	for _, f := range b.Files {
		out[f.Path] = string(f.Data)
	}
	return out
}

func TestUrlSourceFetchesOnlyReferencedAllowedSupportDirectories(t *testing.T) {
	h, _, url := servedRepo(t)
	b, err := NewUrlSource(h).Fetch(context.Background(), url)
	if err != nil || b == nil {
		t.Fatalf("fetch gave %v %v", b, err)
	}
	got := bundlePaths(b)
	want := []string{"SKILL.md", "references/guide.md", "references/foo#bar.md", "references/my guide.md", "templates/report.md",
		"scripts/run.py", "assets/logo.png", "examples/endpoint-inventory.md"}
	if len(got) != len(want) {
		t.Fatalf("bundle files %v", got)
	}
	for _, p := range want {
		if _, ok := got[p]; !ok {
			t.Fatalf("missing %s in %v", p, got)
		}
	}
	if got["assets/logo.png"] != "\x89PNG\r\n\x1a\n\x00\xff" || got["references/foo#bar.md"] != "encoded delimiter\n" ||
		got["references/my guide.md"] != "encoded space\n" || b.Metadata.Vals["source_url"] != url {
		t.Fatalf("bundle contents %v metadata %v", got, b.Metadata.Vals)
	}
}

func TestUrlSourceRejectsTraversalReference(t *testing.T) {
	h, _ := publicHub(t, func(string) *pyhttpx.Response {
		return textResp(200, "---\nname: bad\ndescription: bad\n---\n[bad](references/../../secret.txt)\n")
	})
	if b, err := NewUrlSource(h).Fetch(context.Background(), "https://example.com/bad/SKILL.md"); b != nil || err != nil {
		t.Fatalf("fetch gave %v %v", b, err)
	}
}

func TestSameDirLinkedSiblingsAreFetchedAndOutsideLinksSkipped(t *testing.T) {
	h, repo, url := servedRepo(t)
	writeTestFile(t, filepath.Join(repo, "CONTEXT-FORMAT.md"), "format\n")
	writeTestFile(t, filepath.Join(repo, "DEEPENING.md"), "deepening\n")
	writeTestFile(t, filepath.Join(repo, "DEFS.md"), "defs\n")
	writeTestFile(t, filepath.Join(repo, "SKILL.md"), provenanceSkillMD+"See [the format](./CONTEXT-FORMAT.md) and [deepening](DEEPENING.md).\n"+
		"[registry](../../tools/REGISTRY.md) and [defs](./DEFS.md)\n")
	b, err := NewUrlSource(h).Fetch(context.Background(), url)
	if err != nil || b == nil {
		t.Fatalf("fetch gave %v %v", b, err)
	}
	got := bundlePaths(b)
	if got["CONTEXT-FORMAT.md"] != "format\n" || got["DEEPENING.md"] != "deepening\n" || got["DEFS.md"] != "defs\n" {
		t.Fatalf("siblings %v", got)
	}
	for p := range got {
		if p == "README.md" || strings.Contains(p, "..") {
			t.Fatalf("unexpected bundle member %s", p)
		}
	}
}

func TestBrowseShSearchAndInspect(t *testing.T) {
	h, _ := newTestHub(t)
	catalog, _ := pyjson.Loads(`[{"slug": "airbnb.com/search-listings-ddgioa", "name": "search-listings", "title": "Airbnb Search Listings",
		"description": "Search and browse Airbnb listings by location and dates.", "hostname": "airbnb.com", "category": "travel",
		"tags": ["travel", "accommodation"], "sourceUrl": "https://github.com/browserbase/browse.sh/blob/main/skills/airbnb.com/search-listings-ddgioa/SKILL.md",
		"recommendedMethod": "stagehand", "proxies": false, "installCount": 42},
		{"slug": "amazon.com/search-products-xyz", "name": "search-products", "title": "Amazon Product Search",
		"description": "Search for products on Amazon.", "hostname": "amazon.com", "category": "shopping", "tags": ["shopping", "ecommerce"],
		"sourceUrl": "https://github.com/browserbase/browse.sh/blob/main/skills/amazon.com/search-products-xyz/SKILL.md",
		"recommendedMethod": "stagehand", "proxies": false, "installCount": 99}]`)
	h.io.readIndexCache = func(key string) (any, error) {
		if key != "browse_sh_catalog" {
			t.Fatalf("cache key %s", key)
		}
		return catalog, nil
	}
	src := NewBrowseShSource(h)
	results, err := src.Search(context.Background(), "airbnb", 10)
	if err != nil || len(results) < 1 {
		t.Fatalf("search gave %v %v", results, err)
	}
	m := results[0]
	if m.Name != "search-listings" || m.Source != "browse-sh" || m.TrustLevel != "community" ||
		m.Identifier != "browse-sh/airbnb.com/search-listings-ddgioa" || m.Tags[0] != "travel" {
		t.Fatalf("result %+v", m)
	}
	meta, err := src.Inspect(context.Background(), "browse-sh/airbnb.com/search-listings-ddgioa")
	if err != nil || meta == nil || meta.Name != "search-listings" || meta.Extra.Vals["hostname"] != "airbnb.com" ||
		meta.Extra.Vals["category"] != "travel" || meta.Extra.Vals["install_count"] != int64(42) {
		t.Fatalf("inspect gave %+v %v", meta, err)
	}
}

func TestGuardedRedirectToInvalidIPv6RaisesLikeUrljoin(t *testing.T) {
	h, _ := publicHub(t, func(string) *pyhttpx.Response {
		return &pyhttpx.Response{StatusCode: 302, Header: http.Header{"Location": {"http://[bad/SKILL.md"}}}
	})
	b, err := NewUrlSource(h).Fetch(context.Background(), "https://example.com/SKILL.md")
	if b != nil || err == nil || err.Error() != "Invalid IPv6 URL" {
		t.Fatalf("fetch gave %v %v", b, err)
	}
}
