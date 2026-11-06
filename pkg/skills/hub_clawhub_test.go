package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/tools"
)

// ClawHub source over a hub whose plain GETs answer through fn and whose cache reads miss
func clawhubTestSource(t *testing.T, fn func(url string, o hubGet) *pyhttpx.Response) (*ClawHubSource, *Hub, *[]string) {
	h, _ := newTestHub(t)
	var writes []string
	var mu sync.Mutex
	h.io.isSafeURL = func(string) bool { return true }
	h.io.siteAccess = func(string) *tools.WebsiteBlock { return nil }
	h.io.readIndexCache = func(string) (any, error) { return nil, nil }
	h.io.writeIndexCache = func(key string, _ any) error { mu.Lock(); writes = append(writes, key); mu.Unlock(); return nil }
	h.io.httpGet = plainGet(fn)
	c := NewClawHubSource(h)
	c.seams.sleep = func(time.Duration) {}
	return c, h, &writes
}

func statusResp(status int, body string) *pyhttpx.Response { return textResp(status, body) }

func zipOf(t *testing.T, files ...string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for i := 0; i+1 < len(files); i += 2 {
		w, err := zw.Create(files[i])
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(files[i+1]))
	}
	zw.Close()
	return buf.Bytes()
}

type closeTracker struct {
	io.Reader
	closed *bool
}

func (c closeTracker) Close() error { *c.closed = true; return nil }

func streamOf(status int, header http.Header, body []byte, closed *bool) *pyhttpx.StreamResponse {
	return &pyhttpx.StreamResponse{Response: pyhttpx.Response{StatusCode: status, Header: header}, Body: closeTracker{bytes.NewReader(body), closed}}
}

func TestClawHubSearchUsesListingEndpointAsFallback(t *testing.T) {
	c, _, _ := clawhubTestSource(t, func(u string, _ hubGet) *pyhttpx.Response {
		if strings.HasSuffix(strings.SplitN(u, "?", 2)[0], "/skills") {
			return statusResp(200, `{"items": [{"slug": "caldav-calendar", "displayName": "CalDAV Calendar", "summary": "Calendar integration", "tags": ["calendar", "productivity"]}]}`)
		}
		return statusResp(404, "{}")
	})
	c.seams.catalog = func(int) []SkillMeta { return []SkillMeta{} }
	res, err := c.Search(context.Background(), "caldav", 5)
	if err != nil || len(res) != 1 || res[0].Identifier != "caldav-calendar" || res[0].Name != "CalDAV Calendar" || res[0].Description != "Calendar integration" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestClawHubInspectShapes(t *testing.T) {
	for body, want := range map[string][3]string{
		`{"slug": "caldav-calendar", "displayName": "CalDAV Calendar", "summary": "Calendar integration", "tags": ["calendar"]}`:                                                                                                                {"CalDAV Calendar", "Calendar integration", ""},
		`{"skill": {"slug": "caldav-calendar", "displayName": "CalDAV Calendar", "summary": "Calendar integration", "tags": {"latest": "1.0.0"}}, "latestVersion": {"version": "1.0.0"}}`:                                                       {"CalDAV Calendar", "Calendar integration", ""},
		`{"skill": {"slug": "apple-docs", "displayName": "Apple Docs", "summary": "Documentation reader", "tags": {"latest": "1.0.0"}}, "latestVersion": {"version": "1.0.0"}, "owner": {"handle": "thesethrose", "displayName": "Seth Rose"}}`: {"Apple Docs", "Documentation reader", "thesethrose"},
	} {
		c, _, _ := clawhubTestSource(t, func(string, hubGet) *pyhttpx.Response { return statusResp(200, body) })
		m, err := c.Inspect(context.Background(), "caldav-calendar")
		if err != nil || m == nil || m.Name != want[0] || m.Description != want[1] {
			t.Fatalf("%s -> %+v", body, m)
		}
		owner, _ := m.Extra.Get("owner")
		if want[2] != "" && (owner != want[2] || m.Identifier != "@thesethrose/caldav-calendar") {
			t.Fatalf("owner %v %s", owner, m.Identifier)
		}
		if want[2] == "" && len(m.Tags) != 0 && !slices.Equal(m.Tags, []string{"calendar"}) {
			t.Fatal(m.Tags)
		}
	}
}

func TestClawHubFetchResolvesLatestVersionAndDownloadsRawFiles(t *testing.T) {
	c, h, _ := clawhubTestSource(t, func(u string, _ hubGet) *pyhttpx.Response {
		switch {
		case strings.HasSuffix(u, "/skills/caldav-calendar"):
			return statusResp(200, `{"slug": "caldav-calendar", "latestVersion": {"version": "1.0.1"}}`)
		case strings.HasSuffix(u, "/skills/caldav-calendar/versions/1.0.1"):
			return statusResp(200, `{"files": [{"path": "SKILL.md", "rawUrl": "https://files.example/skill-md"}, {"path": "README.md", "content": "hello"}]}`)
		}
		return statusResp(404, "{}")
	})
	var closed bool
	c.seams.stream = func(string, pyhttpx.Params, time.Duration) *pyhttpx.StreamResponse {
		return streamOf(404, http.Header{}, nil, &closed)
	}
	h.io.ssrfGet = func(context.Context, string, time.Duration, map[string]string) (*pyhttpx.Response, error) {
		return statusResp(200, "# Skill"), nil
	}
	b, err := c.Fetch(context.Background(), "caldav-calendar")
	if err != nil || b == nil || b.Name != "caldav-calendar" || !closed {
		t.Fatal(b, err)
	}
	if md, _ := b.File("SKILL.md"); string(md) != "# Skill" {
		t.Fatal(string(md))
	}
	if rd, _ := b.File("README.md"); string(rd) != "hello" {
		t.Fatal(string(rd))
	}
}

func TestClawHubFetchFallsBackToVersionsList(t *testing.T) {
	c, _, _ := clawhubTestSource(t, func(u string, _ hubGet) *pyhttpx.Response {
		switch {
		case strings.HasSuffix(u, "/skills/caldav-calendar"):
			return statusResp(200, `{"slug": "caldav-calendar"}`)
		case strings.HasSuffix(u, "/skills/caldav-calendar/versions"):
			return statusResp(200, `[{"version": "2.0.0"}]`)
		case strings.HasSuffix(u, "/skills/caldav-calendar/versions/2.0.0"):
			return statusResp(200, `{"files": {"SKILL.md": "# Skill"}}`)
		}
		return statusResp(404, "{}")
	})
	c.seams.stream = func(string, pyhttpx.Params, time.Duration) *pyhttpx.StreamResponse { return nil }
	b, _ := c.Fetch(context.Background(), "caldav-calendar")
	if md, _ := b.File("SKILL.md"); string(md) != "# Skill" {
		t.Fatal(string(md))
	}
}

func TestClawHubFetchBlocksPrivateRawURL(t *testing.T) {
	c, h, _ := clawhubTestSource(t, func(u string, _ hubGet) *pyhttpx.Response {
		switch {
		case strings.HasSuffix(u, "/skills/caldav-calendar"):
			return statusResp(200, `{"slug": "caldav-calendar", "latestVersion": {"version": "1.0.1"}}`)
		case strings.HasSuffix(u, "/skills/caldav-calendar/versions/1.0.1"):
			return statusResp(200, `{"files": [{"path": "SKILL.md", "rawUrl": "http://127.0.0.1/private-skill"}]}`)
		}
		return statusResp(404, "{}")
	})
	h.io.isSafeURL = func(u string) bool { return !strings.HasPrefix(u, "http://127.0.0.1/") }
	c.seams.stream = func(string, pyhttpx.Params, time.Duration) *pyhttpx.StreamResponse {
		var closed bool
		return streamOf(404, http.Header{}, nil, &closed)
	}
	safeCalls := 0
	h.io.ssrfGet = func(context.Context, string, time.Duration, map[string]string) (*pyhttpx.Response, error) {
		safeCalls++
		return statusResp(200, "x"), nil
	}
	if b, _ := c.Fetch(context.Background(), "caldav-calendar"); b != nil || safeCalls != 0 {
		t.Fatal(b, safeCalls)
	}
}

// Catalog pages served by index, each advertising the next cursor until last
func catalogPages(pages *int, last int) func(string, hubGet) *pyhttpx.Response {
	return func(u string, _ hubGet) *pyhttpx.Response {
		if !strings.HasSuffix(strings.SplitN(u, "?", 2)[0], "/skills") {
			return statusResp(404, "{}")
		}
		idx := *pages
		*pages++
		next := fmt.Sprintf(`"cursor-%d"`, idx+1)
		if last >= 0 && idx >= last {
			next = "null"
		}
		return statusResp(200, fmt.Sprintf(`{"items": [{"slug": "skill-%d", "displayName": "Skill %d"}], "nextCursor": %s}`, idx, idx, next))
	}
}

func TestClawHubCatalogWalk(t *testing.T) {
	pages := 0
	c, _, writes := clawhubTestSource(t, catalogPages(&pages, 2))
	res, _ := c.Search(context.Background(), "", 0)
	if len(res) != 3 || pages != 3 || !slices.Equal(*writes, []string{"clawhub_catalog_v1"}) {
		t.Fatalf("empty query walk %d %d %v", len(res), pages, *writes)
	}

	pages = 0
	c, _, writes = clawhubTestSource(t, catalogPages(&pages, -1))
	c.CatalogWalkBudgetSeconds = -1
	res = c.loadCatalogIndex(context.Background(), 10)
	if pages >= 750 || len(*writes) != 0 || res == nil {
		t.Fatalf("budget walk %d %v", pages, *writes)
	}

	pages = 0
	c, _, writes = clawhubTestSource(t, catalogPages(&pages, 1))
	if res := c.loadCatalogIndex(context.Background(), 10); len(res) != 2 || len(*writes) != 1 {
		t.Fatalf("natural end %d %v", len(res), *writes)
	}

	pages = 0
	c, _, writes = clawhubTestSource(t, catalogPages(&pages, -1))
	if res := c.loadCatalogIndex(context.Background(), 3); len(res) != 3 || pages != 3 || len(*writes) != 0 {
		t.Fatalf("max items %d %d %v", len(res), pages, *writes)
	}

	pages = 0
	c, _, _ = clawhubTestSource(t, catalogPages(&pages, 4))
	c.CatalogWalkBudgetSeconds = -1
	if res := c.loadCatalogIndex(context.Background(), 0); len(res) != 5 {
		t.Fatalf("unbounded walk ignores the budget %d", len(res))
	}

	pages = 0
	c, _, _ = clawhubTestSource(t, catalogPages(&pages, -1))
	if res, _ := c.Search(context.Background(), "", 2); len(res) != 2 || pages != 2 {
		t.Fatalf("browse bounded to limit %d %d", len(res), pages)
	}
}

func TestClawHubUnboundedWalkRetriesNonePages(t *testing.T) {
	for _, failing := range []int{2, 5} {
		calls := 0
		var sleeps []time.Duration
		c, _, writes := clawhubTestSource(t, func(string, hubGet) *pyhttpx.Response {
			calls++
			if calls <= failing {
				return statusResp(503, "")
			}
			return statusResp(200, `{"items": [{"slug": "a"}], "nextCursor": null}`)
		})
		c.seams.sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
		res := c.loadCatalogIndex(context.Background(), 0)
		if failing == 2 && (len(res) != 1 || len(*writes) != 1 || !slices.Equal(sleeps, []time.Duration{2 * time.Second, 4 * time.Second})) {
			t.Fatalf("retried walk %v %v", res, sleeps)
		}
		if failing == 5 && (len(res) != 0 || len(*writes) != 0 || calls != 5) {
			t.Fatalf("exhausted walk %v %d", *writes, calls)
		}
	}
}

func TestClawHubParseIdentifierShapes(t *testing.T) {
	for id, want := range map[string][2]string{"caldav": {"caldav", ""}, "clawhub/caldav": {"caldav", ""}, "@alice/caldav": {"caldav", "alice"},
		"clawhub/@alice/caldav": {"caldav", "alice"}, "alice/skills/caldav": {"caldav", "alice"}} {
		slug, owner, _, ok := clawhubParseIdentifier(id)
		if !ok || slug != want[0] || owner != want[1] {
			t.Fatalf("%s -> %s %s %v", id, slug, owner, ok)
		}
	}
	for _, id := range []string{"owner/repo/path", "anthropics/skills/skills/x/y", "", "a/b", "../x"} {
		if _, _, _, ok := clawhubParseIdentifier(id); ok {
			t.Fatal(id)
		}
	}
	calls := 0
	c, _, _ := clawhubTestSource(t, func(string, hubGet) *pyhttpx.Response {
		calls++
		return statusResp(200, `{"slug": "x", "owner": "bob"}`)
	})
	if m, _ := c.Inspect(context.Background(), "owner/repo/path"); m != nil || calls != 0 {
		t.Fatal("claimed a github id")
	}
	if b, _ := c.Fetch(context.Background(), "owner/repo/path"); b != nil || calls != 0 {
		t.Fatal("fetched a github id")
	}
	if m, _ := c.Inspect(context.Background(), "alice/skills/x"); m != nil {
		t.Fatal("owner mismatch accepted")
	}
}

func TestFetchOwnerHandleRetry(t *testing.T) {
	for _, c := range []struct {
		statuses []int
		want     string
		sleeps   []time.Duration
	}{
		{[]int{429, 200}, "alice", []time.Duration{3 * time.Second}},
		{[]int{429, 429, 429}, "", []time.Duration{3 * time.Second, 3 * time.Second}},
		{[]int{503, 200}, "alice", []time.Duration{2 * time.Second}},
		{[]int{404}, "", nil},
	} {
		statuses := slices.Clone(c.statuses)
		var sleeps []time.Duration
		src, _, _ := clawhubTestSource(t, func(string, hubGet) *pyhttpx.Response {
			s := statuses[0]
			statuses = statuses[1:]
			r := statusResp(s, `{"skill": {"slug": "x"}, "owner": {"handle": "alice"}}`)
			if s == 429 {
				r.Header.Set("Retry-After", "3")
			}
			return r
		})
		src.seams.sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
		if got := src.fetchOwnerHandle(context.Background(), "x"); got != c.want || !slices.Equal(sleeps, c.sleeps) {
			t.Fatalf("%v -> %q %v", c.statuses, got, sleeps)
		}
	}
}

func TestClawHubEnrichOwners(t *testing.T) {
	c, _, _ := clawhubTestSource(t, func(u string, _ hubGet) *pyhttpx.Response {
		if strings.HasSuffix(u, "/skills/b") {
			return statusResp(404, "{}")
		}
		return statusResp(200, `{"owner": {"handle": "alice"}}`)
	})
	skills := []SkillMeta{{Name: "a", Identifier: "a", Source: "clawhub"}, {Name: "b", Identifier: "b", Source: "clawhub"},
		{Name: "g", Identifier: "g", Source: "github"}}
	if n := c.EnrichOwners(context.Background(), skills, 4, nil); n != 1 {
		t.Fatal(n)
	}
	if o, _ := skills[0].Extra.Get("owner"); o != "alice" || skills[1].Extra != nil || skills[2].Extra != nil {
		t.Fatalf("%+v", skills)
	}
}

func TestClawHubZipStream(t *testing.T) {
	data := zipOf(t, "SKILL.md", "# A normal skill")
	for _, declared := range []string{"", "1", "invalid", "-1"} {
		c, _, _ := clawhubTestSource(t, func(string, hubGet) *pyhttpx.Response { return statusResp(404, "") })
		c.ZipDownloadMaxBytes = int64(len(data) - 1)
		var closed bool
		c.seams.stream = func(string, pyhttpx.Params, time.Duration) *pyhttpx.StreamResponse {
			hd := http.Header{}
			if declared != "" {
				hd.Set("content-length", declared)
			}
			return streamOf(200, hd, data, &closed)
		}
		if files := c.downloadZip(context.Background(), "example", "1", ""); len(files) != 0 || !closed {
			t.Fatalf("%q -> %v", declared, files)
		}
	}
	c, _, _ := clawhubTestSource(t, nil)
	c.ZipDownloadMaxBytes = int64(len(data))
	var closed bool
	c.seams.stream = func(string, pyhttpx.Params, time.Duration) *pyhttpx.StreamResponse {
		return streamOf(200, http.Header{}, data, &closed)
	}
	files := c.downloadZip(context.Background(), "example", "1", "")
	if len(files) != 1 || files[0].Path != "SKILL.md" || string(files[0].Data) != "# A normal skill" || !closed {
		t.Fatalf("%v", files)
	}
	c.ZipDownloadMaxBytes = 100
	closed = false
	c.seams.stream = func(string, pyhttpx.Params, time.Duration) *pyhttpx.StreamResponse {
		hd := http.Header{}
		hd.Set("content-length", "101")
		return &pyhttpx.StreamResponse{Response: pyhttpx.Response{StatusCode: 200, Header: hd}, Body: closeTracker{failReader{t}, &closed}}
	}
	if files := c.downloadZip(context.Background(), "example", "1", ""); len(files) != 0 || !closed {
		t.Fatal("declared oversize read")
	}
	var delays []time.Duration
	var responses []*bool
	c.seams.sleep = func(d time.Duration) { delays = append(delays, d) }
	c.seams.stream = func(string, pyhttpx.Params, time.Duration) *pyhttpx.StreamResponse {
		hd := http.Header{}
		hd.Set("retry-after", []string{"-1", "1000", "invalid"}[len(responses)])
		cl := false
		responses = append(responses, &cl)
		return streamOf(429, hd, nil, &cl)
	}
	if files := c.downloadZip(context.Background(), "example", "1", ""); len(files) != 0 || !slices.Equal(delays, []time.Duration{0, 15 * time.Second}) {
		t.Fatal(delays)
	}
	for _, cl := range responses {
		if !*cl {
			t.Fatal("429 response left open")
		}
	}
}

type failReader struct{ t *testing.T }

func (f failReader) Read([]byte) (int, error) { f.t.Fatal("read oversized body"); return 0, io.EOF }

type mockTransport func(*http.Request) *http.Response

func (m mockTransport) RoundTrip(r *http.Request) (*http.Response, error) { return m(r), nil }

func TestStreamRechecksRedirectPolicy(t *testing.T) {
	for _, dest := range []string{"https://download.example/bundle", "http://127.0.0.1/private", "https://blocked.example/bundle"} {
		h, _ := newTestHub(t)
		var reqs []*http.Request
		h.io.streamClient = func(time.Duration) *http.Client {
			return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
				Transport: mockTransport(func(r *http.Request) *http.Response {
					reqs = append(reqs, r)
					if len(reqs) == 1 {
						return &http.Response{StatusCode: 302, Header: http.Header{"Location": {dest}}, Body: io.NopCloser(strings.NewReader("")), Request: r}
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("bundle")), Request: r}
				})}
		}
		h.io.isSafeURL = func(u string) bool { return !strings.Contains(u, "127.0.0.1") }
		h.io.siteAccess = func(u string) *tools.WebsiteBlock {
			if strings.Contains(u, "blocked.example") {
				return &tools.WebsiteBlock{Host: "blocked.example", Rule: "test"}
			}
			return nil
		}
		resp := h.guardedHTTPStream(context.Background(), "https://api.example/download", pyhttpx.Params{{"slug", "secret"}}, 30*time.Second)
		if strings.Contains(dest, "download.example") {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != "bundle" || reqs[1].URL.String() != dest {
				t.Fatal(dest)
			}
		} else if resp != nil || len(reqs) != 1 {
			t.Fatal(dest)
		}
		if reqs[0].URL.Query().Get("slug") != "secret" {
			t.Fatal(reqs[0].URL)
		}
	}
}

// Loopback ClawHub registry answering 409 to every request lacking owner alice
func clawhubRegistry(t *testing.T, fallback, mismatch bool) (*ClawHubSource, *[]*url.URL) {
	t.Setenv("ICEBATH_ALLOW_PRIVATE_URLS", "true")
	var mu sync.Mutex
	var reqs []*url.URL
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.URL)
		mu.Unlock()
		status := 200
		var payload any
		var raw []byte
		switch {
		case !reflect.DeepEqual(r.URL.Query()["owner"], []string{"alice"}):
			status, payload = 409, map[string]any{"code": "AMBIGUOUS_SKILL_SLUG"}
		case strings.HasSuffix(r.URL.Path, "/download"):
			if fallback {
				status, payload = 404, map[string]any{}
			} else {
				raw = zipOf(t, "SKILL.md", "# Alice fixture")
			}
		case strings.HasSuffix(r.URL.Path, "/versions"):
			payload = []any{map[string]any{"version": "1.0"}}
			if fallback {
				payload = map[string]any{"items": payload}
			}
		case strings.HasSuffix(r.URL.Path, "/versions/1.0"):
			payload = map[string]any{"files": map[string]any{"SKILL.md": "# Alice fixture"}}
		default:
			p := map[string]any{"skill": map[string]any{"slug": "collision"}}
			if mismatch {
				p["owner"] = map[string]any{"handle": "bob"}
			}
			payload = p
		}
		if raw == nil {
			raw, _ = json.Marshal(payload)
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(raw)))
		w.WriteHeader(status)
		w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	h, _ := newTestHub(t)
	h.io.readIndexCache = func(string) (any, error) { return nil, nil }
	h.io.writeIndexCache = func(string, any) error { return nil }
	c := NewClawHubSource(h)
	c.BaseURL = srv.URL + "/api/v1"
	return c, &reqs
}

func TestQualifiedOwnerSurvivesMetadataVersionsAndDownload(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		c, reqs := clawhubRegistry(t, fallback, false)
		for _, id := range []string{"@alice/collision", "clawhub/@alice/collision", "alice/skills/collision"} {
			m, err := c.Inspect(context.Background(), id)
			if err != nil || m == nil || m.Identifier != "@alice/collision" {
				t.Fatal(id, m, err)
			}
			b, err := c.Fetch(context.Background(), id)
			if err != nil || b == nil || b.Identifier != "@alice/collision" || len(b.Files) != 1 || string(b.Files[0].Data) != "# Alice fixture" {
				t.Fatal(id, b, err)
			}
		}
		paths := map[string]bool{}
		for _, u := range *reqs {
			if u.Query().Get("owner") != "alice" {
				t.Fatal(u)
			}
			paths[u.Path[strings.LastIndex(u.Path, "/"):]] = true
		}
		if !paths["/versions"] || !paths["/download"] || (fallback && !paths["/1.0"]) {
			t.Fatal(paths)
		}
	}
}

func TestAmbiguousOrMismatchedOwnerNeverDownloads(t *testing.T) {
	c, reqs := clawhubRegistry(t, false, false)
	for _, id := range []string{"collision", "github-owner/repository/collision"} {
		if b, _ := c.Fetch(context.Background(), id); b != nil {
			t.Fatal(id)
		}
	}
	if len(*reqs) != 1 {
		t.Fatal(len(*reqs))
	}
	c, reqs = clawhubRegistry(t, false, true)
	if b, _ := c.Fetch(context.Background(), "@alice/collision"); b != nil || len(*reqs) != 1 {
		t.Fatal(b, len(*reqs))
	}
}

func TestFetchOwnerHandleRetriesTransportErrorsAndEnrichBudget(t *testing.T) {
	calls := 0
	var sleeps []time.Duration
	c, h, _ := clawhubTestSource(t, nil)
	h.io.httpGet = func(context.Context, string, hubGet) (*pyhttpx.Response, error) {
		calls++
		if calls < 3 {
			return nil, &pyhttpx.TimeoutError{Phase: "read"}
		}
		return statusResp(200, `{"owner": {"handle": "alice"}}`), nil
	}
	c.seams.sleep = func(d time.Duration) { sleeps = append(sleeps, d) }
	if got := c.fetchOwnerHandle(context.Background(), "x"); got != "alice" || !slices.Equal(sleeps, []time.Duration{2 * time.Second, 4 * time.Second}) {
		t.Fatal(got, sleeps)
	}
	c.seams.sleep = func(time.Duration) {}
	burst := map[string]int{}
	var mu sync.Mutex
	h.io.httpGet = func(_ context.Context, u string, _ hubGet) (*pyhttpx.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		burst[u]++
		if burst[u] == 1 {
			return statusResp(429, ""), nil
		}
		return statusResp(200, `{"owner": {"handle": "h"}}`), nil
	}
	skills := []SkillMeta{{Identifier: "a", Source: "clawhub"}, {Identifier: "b", Source: "clawhub"}}
	if n := c.EnrichOwners(context.Background(), skills, 30, nil); n != 2 {
		t.Fatal(n)
	}
	now := 0.0
	c.seams.monotonic = func() float64 { now += 10; return now }
	budget := 5.0
	more := []SkillMeta{{Identifier: "c", Source: "clawhub"}, {Identifier: "d", Source: "clawhub"}, {Identifier: "e", Source: "clawhub"}}
	if n := c.EnrichOwners(context.Background(), more, 1, &budget); n != 0 {
		t.Fatal(n)
	}
}
