package skills

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

type sitemapCall struct {
	url     string
	headers map[string]string
}

// skills.sh source whose guarded GETs answer from fn and whose cache writes are counted
func sitemapSource(t *testing.T, fn func(url string) *pyhttpx.Response) (*SkillsShSource, *[]sitemapCall, *[]string) {
	h, _ := newTestHub(t)
	var calls []sitemapCall
	var writes []string
	h.io.isSafeURL = func(string) bool { return true }
	h.io.siteAccess = func(string) *tools.WebsiteBlock { return nil }
	h.io.readIndexCache = func(string) (any, error) { return nil, nil }
	h.io.writeIndexCache = func(key string, _ any) error { writes = append(writes, key); return nil }
	h.io.ssrfGet = func(_ context.Context, url string, _ time.Duration, headers map[string]string) (*pyhttpx.Response, error) {
		calls = append(calls, sitemapCall{url, maps.Clone(headers)})
		return fn(url), nil
	}
	s := NewSkillsShSource(h, anonAuth(h))
	s.seams.sleep = func(time.Duration) {}
	return s, &calls, &writes
}

func TestSkillsShSitemapFetchesGoThroughGuardedGetAndAskForGzipOnly(t *testing.T) {
	s, calls, _ := sitemapSource(t, func(url string) *pyhttpx.Response {
		if strings.HasSuffix(url, "/sitemap.xml") {
			return textResp(200, "<sitemapindex><sitemap><loc>https://www.skills.sh/sitemap-skills-0.xml</loc></sitemap></sitemapindex>")
		}
		return textResp(200, "<urlset><url><loc>https://skills.sh/acme/repo/my-skill</loc></url></urlset>")
	})
	var ids []string
	for _, m := range s.sitemapCatalog(context.Background(), 5) {
		ids = append(ids, m.Identifier)
	}
	if !slices.Equal(ids, []string{"skills-sh/acme/repo/my-skill"}) {
		t.Fatalf("ids %v", ids)
	}
	var urls []string
	for _, c := range *calls {
		urls = append(urls, c.url)
		if !maps.Equal(c.headers, map[string]string{"Accept-Encoding": "gzip"}) {
			t.Fatalf("headers %v", c.headers)
		}
	}
	if !slices.Equal(urls, []string{"https://www.skills.sh/sitemap.xml", "https://www.skills.sh/sitemap-skills-0.xml"}) {
		t.Fatalf("urls %v", urls)
	}
}

func TestSkillsShSearchMapsResultsToPrefixedIdentifiers(t *testing.T) {
	h, _ := newTestHub(t)
	h.io.readIndexCache = func(string) (any, error) { return nil, nil }
	h.io.writeIndexCache = func(string, any) error { return nil }
	h.io.httpGet = plainGet(func(string, hubGet) *pyhttpx.Response {
		return jsonResp(t, `{"skills": [{"id": "vercel-labs/agent-skills/vercel-react-best-practices",
			"skillId": "vercel-react-best-practices", "name": "vercel-react-best-practices", "installs": 207679,
			"source": "vercel-labs/agent-skills"}]}`)
	})
	results, err := NewSkillsShSource(h, anonAuth(h)).Search(context.Background(), "react", 5)
	if err != nil || len(results) != 1 {
		t.Fatalf("%v %v", results, err)
	}
	r := results[0]
	installs, _ := r.Extra.Get("installs")
	if r.Source != "skills.sh" || r.Identifier != "skills-sh/vercel-labs/agent-skills/vercel-react-best-practices" ||
		r.Repo == nil || *r.Repo != "vercel-labs/agent-skills" || r.Path == nil || *r.Path != "vercel-react-best-practices" || !installsIs(installs, "207679") {
		t.Fatalf("%+v", r)
	}
}

func TestSkillsShSitemapCatalogRetriesFailedShardAndNeverCachesPartial(t *testing.T) {
	index := "<sitemapindex><sitemap><loc>https://www.skills.sh/sitemap-skills-0.xml</loc></sitemap>" +
		"<sitemap><loc>https://www.skills.sh/sitemap-skills-1.xml</loc></sitemap></sitemapindex>"
	s, calls, writes := sitemapSource(t, func(url string) *pyhttpx.Response {
		switch {
		case strings.HasSuffix(url, "/sitemap.xml"):
			return textResp(200, index)
		case strings.HasSuffix(url, "sitemap-skills-0.xml"):
			return textResp(200, "<urlset><url><loc>https://www.skills.sh/o/r/skill-a</loc></url></urlset>")
		}
		return textResp(503, "")
	})
	var ids []string
	for _, m := range s.sitemapCatalog(context.Background(), 0) {
		ids = append(ids, m.Identifier)
	}
	if !slices.Equal(ids, []string{"skills-sh/o/r/skill-a"}) {
		t.Fatalf("ids %v", ids)
	}
	shard1 := 0
	for _, c := range *calls {
		if c.url == "https://www.skills.sh/sitemap-skills-1.xml" {
			shard1++
		}
	}
	if shard1 != CatalogPageRetries || len(*writes) != 0 {
		t.Fatalf("shard1 %d writes %v", shard1, *writes)
	}
}

func TestSkillsShFetchFallsBackToTreeSearchForDeeplyNestedSkills(t *testing.T) {
	h, _ := newTestHub(t)
	h.io.readIndexCache = func(string) (any, error) { return nil, nil }
	h.io.writeIndexCache = func(string, any) error { return nil }
	h.io.isSafeURL = func(string) bool { return true }
	h.io.siteAccess = func(string) *tools.WebsiteBlock { return nil }
	tree := `{"tree": [{"path": "README.md", "type": "blob"},
		{"path": "cli-tool/components/skills/development/my-skill/SKILL.md", "type": "blob"},
		{"path": "cli-tool/components/skills/development/other-skill/SKILL.md", "type": "blob"}]}`
	get := func(url string, _ hubGet) *pyhttpx.Response {
		switch {
		case strings.Contains(url, "/api/search"):
			return textResp(404, "")
		case strings.HasSuffix(url, "/contents/"):
			return jsonResp(t, "[]")
		case strings.Contains(url, "/contents/"):
			return textResp(404, "")
		case strings.HasSuffix(url, "owner/repo"):
			return jsonResp(t, `{"default_branch": "main"}`)
		case strings.Contains(url, "/git/trees/main"):
			return jsonResp(t, tree)
		}
		return &pyhttpx.Response{StatusCode: 200, Header: http.Header{}, Content: []byte("<h1>my-skill</h1>")}
	}
	h.io.httpGet = plainGet(get)
	h.io.ssrfGet = func(_ context.Context, url string, _ time.Duration, _ map[string]string) (*pyhttpx.Response, error) {
		return get(url, hubGet{}), nil
	}
	s := NewSkillsShSource(h, anonAuth(h))
	var fetched []string
	resolved := &SkillBundle{Name: "my-skill", Files: []BundleFile{{Path: "SKILL.md", Data: []byte("# My Skill")}}, Source: "github",
		Identifier: "owner/repo/cli-tool/components/skills/development/my-skill", TrustLevel: "community"}
	s.GitHub.seams.fetch = func(identifier string) *SkillBundle {
		fetched = append(fetched, identifier)
		if strings.Contains(identifier, "cli-tool/components") {
			return resolved
		}
		return nil
	}
	bundle, err := s.Fetch(context.Background(), "skills-sh/owner/repo/my-skill")
	if err != nil || bundle == nil {
		t.Fatalf("%v %v", bundle, err)
	}
	if bundle.Source != "skills.sh" || !bundleHas(bundle, "SKILL.md", "# My Skill") {
		t.Fatalf("%+v", bundle)
	}
	if !slices.Contains(fetched, "owner/repo/cli-tool/components/skills/development/my-skill") {
		t.Fatalf("fetched %v", fetched)
	}
}

func bundleHas(b *SkillBundle, path, text string) bool {
	data, ok := b.File(path)
	return ok && string(data) == text
}

func installsIs(v any, want string) bool {
	s, err := pyjson.Dumps(v)
	return err == nil && s == want
}
