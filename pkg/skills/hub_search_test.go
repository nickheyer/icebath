package skills

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const repoRootSkillMD = "---\nname: win-disk-cleaner\ndescription: Clean a Windows disk safely.\n---\n\n# Win Disk Cleaner\n\nSee references/free_tools.md\n"

// skills.sh source whose GitHub API serves one repo root skill tree and its root files
func repoRootSource(t *testing.T, tree string) *SkillsShSource {
	_, h := githubTestSource(t)
	h.io.httpGet = plainGet(func(u string, _ hubGet) *pyhttpx.Response {
		base := strings.SplitN(u, "?", 2)[0]
		switch {
		case strings.HasSuffix(strings.TrimRight(base, "/"), "orzcls/win-disk-cleaner"):
			return textResp(200, `{"default_branch": "main"}`)
		case strings.Contains(base, "/git/trees/main"):
			return textResp(200, `{"sha": "`+strings.Repeat("b", 40)+`", "truncated": false, "tree": `+tree+`}`)
		case strings.HasSuffix(base, "/contents/SKILL.md"):
			return textResp(200, repoRootSkillMD)
		case strings.HasSuffix(base, "/contents/references/free_tools.md"):
			return textResp(200, "# Free tools\n")
		case strings.HasSuffix(base, "/contents/LICENSE"), strings.HasSuffix(base, "/contents/README.md"):
			return textResp(200, "root-level file\n")
		}
		return textResp(404, "")
	})
	s := NewSkillsShSource(h, anonAuth(h))
	s.GitHub.Auth = s.Auth
	return s
}

const rootTree = `[{"path": "LICENSE", "type": "blob"}, {"path": "README.md", "type": "blob"}, {"path": "SKILL.md", "type": "blob"}, {"path": "references/free_tools.md", "type": "blob"}`

func TestRepoRootSkillLayout(t *testing.T) {
	const id = "skills-sh/orzcls/win-disk-cleaner/win-disk-cleaner"
	m, err := repoRootSource(t, rootTree+"]").Inspect(context.Background(), id)
	if err != nil || m == nil || m.Name != "win-disk-cleaner" || *m.Repo != "orzcls/win-disk-cleaner" || m.Identifier != id {
		t.Fatal(m, err)
	}
	b, err := repoRootSource(t, rootTree+"]").Fetch(context.Background(), id)
	if err != nil || b == nil || b.Name != "win-disk-cleaner" || b.Identifier != id {
		t.Fatal(b, err)
	}
	if md, _ := b.File("SKILL.md"); string(md) != repoRootSkillMD {
		t.Fatal(string(md))
	}
	if ft, _ := b.File("references/free_tools.md"); string(ft) != "# Free tools\n" {
		t.Fatal(string(ft))
	}
	if u, _ := b.Metadata.Get("source_url"); u != "https://github.com/orzcls/win-disk-cleaner/tree/"+strings.Repeat("b", 40) {
		t.Fatal(u)
	}
	if m, _ := repoRootSource(t, rootTree+`, {"path": "skills/other/SKILL.md", "type": "blob"}]`).Inspect(context.Background(), id); m != nil {
		t.Fatal("multi skill repo resolved as root layout")
	}
}

func TestFetchResolvesGenericSkillDirectoryFromIndexSlug(t *testing.T) {
	row := `{"name": "weread-skills", "description": "WeChat Reading skills", "source": "skills.sh", "identifier": "skills-sh/tencent/wechatreading/weread-skills", "repo": "tencent/wechatreading", "path": "weread-skills"`
	generic := func(string) *SkillBundle {
		return &SkillBundle{Name: "skills", Files: []BundleFile{{Path: "SKILL.md", Data: []byte("---\nname: weread-skills\n---\n")}},
			Source: "github", Identifier: "tencent/wechatreading/skills", TrustLevel: "community"}
	}
	treeCalls := 0
	index := func(entry string, fetched []*SkillBundle) *HermesIndexSource {
		_, h := githubTestSource(t)
		x := NewHermesIndexSource(h, anonAuth(h))
		x.index, x.loaded = pyjson.NewObject(), true
		x.index.(*pyjson.Object).Set("skills", pyList(t, "["+entry+"]"))
		x.github = NewGitHubSource(h, x.Auth, nil)
		x.github.seams.fetch = func(string) *SkillBundle { b := fetched[0]; fetched = fetched[1:]; return b }
		x.github.seams.findSkillInRepoTree = func(repo, name string) (string, bool) {
			treeCalls++
			return "tencent/wechatreading/skills", repo == "tencent/wechatreading" && name == "weread-skills"
		}
		x.github.seams.findRepoRootSkill = func(string) (string, bool) { return "", false }
		return x
	}
	for _, c := range []struct {
		extra   string
		fetched []*SkillBundle
	}{{"}", []*SkillBundle{nil, generic("")}}, {`, "resolved_github_id": "tencent/wechatreading/skills"}`, []*SkillBundle{generic("")}}} {
		b, _ := index(row+c.extra, c.fetched).Fetch(context.Background(), "skills-sh/tencent/wechatreading/weread-skills")
		if b == nil || b.Name != "weread-skills" || b.Identifier != "skills-sh/tencent/wechatreading/weread-skills" {
			t.Fatal(b)
		}
	}
	if treeCalls != 1 {
		t.Fatal(treeCalls)
	}
	_, h := githubTestSource(t)
	ss := NewSkillsShSource(h, anonAuth(h))
	ss.seams.fetchDetailPage = func(string) *pyjson.Object { return nil }
	ss.seams.candidateIdentifiers = func(string) []string { return nil }
	ss.seams.discoverIdentifier = func(string, *pyjson.Object) (string, bool) { return "tencent/wechatreading/skills", true }
	ss.GitHub.seams.fetch = generic
	if b, _ := ss.Fetch(context.Background(), "skills-sh/tencent/wechatreading/weread-skills"); b == nil || b.Name != "weread-skills" {
		t.Fatal(b)
	}
	treeCalls = 0
	gone := `{"name": "weread-skills", "source": "github", "identifier": "tencent/wechatreading/gone", "repo": "tencent/wechatreading", "path": "gone"}`
	if b, _ := index(gone, []*SkillBundle{nil}).Fetch(context.Background(), "tencent/wechatreading/gone"); b != nil || treeCalls != 0 {
		t.Fatal(b, treeCalls)
	}
}

func TestCreateSourceRouterOrder(t *testing.T) {
	h, _ := newTestHub(t)
	var ids []string
	for _, s := range h.createSourceRouter(nil) {
		ids = append(ids, s.SourceID())
	}
	want := []string{"official", "hermes-index", "skills-sh", "well-known", "url", "github", "clawhub", "lobehub", "browse-sh"}
	if !slices.Equal(ids, want) {
		t.Fatal(ids)
	}
}

// Source answering fixed results after an optional delay
type fixedSource struct {
	SourceBase
	results []SkillMeta
	delay   time.Duration
}

func (f *fixedSource) Search(context.Context, string, int) ([]SkillMeta, error) {
	time.Sleep(f.delay)
	return f.results, nil
}
func (f *fixedSource) Fetch(context.Context, string) (*SkillBundle, error) { return nil, nil }
func (f *fixedSource) Inspect(context.Context, string) (*SkillMeta, error) { return nil, nil }

func TestUnifiedSearchTrustRankSurvivesLimitCut(t *testing.T) {
	h, _ := newTestHub(t)
	var community []SkillMeta
	for i := range 5 {
		community = append(community, SkillMeta{Name: "c", Identifier: "c" + string(rune('0'+i)), TrustLevel: "community"})
	}
	srcs := []SkillSource{&fixedSource{SourceBase{ID: "clawhub"}, community, 0},
		&fixedSource{SourceBase{ID: "official"}, []SkillMeta{{Name: "o", Identifier: "official/o", TrustLevel: "builtin"}}, 20 * time.Millisecond}}
	got := h.UnifiedSearch(context.Background(), "x", srcs, "all", 3)
	if len(got) != 3 || got[0].Identifier != "official/o" {
		t.Fatal(got)
	}
}

func TestParallelSearchSourcesTimeoutAndIndexMissFallback(t *testing.T) {
	h, _ := newTestHub(t)
	fast := &fixedSource{SourceBase{ID: "github"}, []SkillMeta{{Name: "a", Identifier: "a"}}, 0}
	slow := &fixedSource{SourceBase{ID: "clawhub"}, []SkillMeta{{Name: "b", Identifier: "b"}}, time.Second}
	all, counts, late := h.ParallelSearchSources(context.Background(), []SkillSource{fast, slow}, "q", nil, "all", 200*time.Millisecond, nil)
	if len(all) != 1 || !slices.Equal(late, []string{"clawhub"}) {
		t.Fatal(all, late)
	}
	if n, _ := counts.Get("github"); n != 1 {
		t.Fatal(n)
	}
	x := NewHermesIndexSource(h, nil)
	x.index, x.loaded = pyjson.NewObject(), true
	x.index.(*pyjson.Object).Set("skills", pyList(t, `[{"name": "zzz", "identifier": "zzz"}]`))
	skillsSh := &fixedSource{SourceBase{ID: "skills-sh"}, []SkillMeta{{Name: "live", Identifier: "skills-sh/o/r/live"}}, 0}
	gh := &fixedSource{SourceBase{ID: "github"}, []SkillMeta{{Name: "g", Identifier: "g"}}, 0}
	all, _, _ = h.ParallelSearchSources(context.Background(), []SkillSource{x, skillsSh, gh}, "live", nil, "all", 5*time.Second, nil)
	if len(all) != 1 || all[0].Name != "live" {
		t.Fatal(all)
	}
	all, _, _ = h.ParallelSearchSources(context.Background(), []SkillSource{x, skillsSh, gh}, "", nil, "all", 5*time.Second, nil)
	if len(all) != 1 || all[0].Name != "zzz" {
		t.Fatal(all)
	}
}

func TestLoadHermesIndexGzipAndStaleFallback(t *testing.T) {
	h, _ := newTestHub(t)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write([]byte(`{"skills": [{"name": "x"}]}`))
	w.Close()
	h.io.httpGet = plainGet(func(_ string, o hubGet) *pyhttpx.Response {
		hd := http.Header{}
		hd.Set("Content-Encoding", "gzip")
		return &pyhttpx.Response{StatusCode: 200, Header: hd, Content: gz.Bytes()}
	})
	v := h.loadHermesIndex(context.Background())
	if o, ok := v.(*pyjson.Object); !ok || o.Len() != 1 {
		t.Fatal(v)
	}
	if _, err := os.Stat(filepath.Join(h.IndexCacheDir(), "hermes-index.json")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-7 * time.Hour)
	os.Chtimes(h.hermesIndexCacheFile(), old, old)
	h.io.httpGet = plainGet(func(string, hubGet) *pyhttpx.Response { return textResp(500, "") })
	if o, ok := h.loadHermesIndex(context.Background()).(*pyjson.Object); !ok || o.Len() != 1 {
		t.Fatal("stale cache not served")
	}
}

func TestOptionalSkillSourceUpstreamStubAndBinaryAssets(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", dir)
	writeTestFile(t, filepath.Join(dir, "cat", "stub", "SKILL.md"), "---\nname: stub\ndescription: d\nmetadata:\n  hermes:\n    upstream:\n      repo: acme/skills\n      path: ./skills/stub/\n---\n")
	writeTestFile(t, filepath.Join(dir, "cat", "real", "SKILL.md"), "---\nname: real\ndescription: d\n---\n")
	writeTestFile(t, filepath.Join(dir, "cat", "real", "assets", "logo.png"), "\x89PNG\x00\xff")
	writeTestFile(t, filepath.Join(dir, "cat", "real", "x.pyc"), "x")
	h, _ := newTestHub(t)
	o := NewOptionalSkillSource(h, anonAuth(h))
	var fetched []string
	o.github = NewGitHubSource(h, o.auth, nil)
	o.github.seams.fetch = func(id string) *SkillBundle {
		fetched = append(fetched, id)
		return &SkillBundle{Name: "stub", Files: []BundleFile{{Path: "SKILL.md", Data: []byte("live")}}, Metadata: objectOf("source_url", "u")}
	}
	b, err := o.Fetch(context.Background(), "official/cat/stub")
	if err != nil || b == nil || b.TrustLevel != "trusted" || b.Identifier != "official/cat/stub" || !slices.Equal(fetched, []string{"acme/skills/skills/stub"}) {
		t.Fatal(b, err, fetched)
	}
	if r, _ := b.Metadata.Get("upstream_repo"); r != "acme/skills" {
		t.Fatal(r)
	}
	b, _ = o.Fetch(context.Background(), "official/cat/real")
	if logo, _ := b.File("assets/logo.png"); string(logo) != "\x89PNG\x00\xff" || b.TrustLevel != "builtin" || len(b.Files) != 2 {
		t.Fatal(b.Files)
	}
	if b, _ := o.Fetch(context.Background(), "official/../../etc"); b != nil {
		t.Fatal("traversal fetched")
	}
	if _, _, ok := upstreamPointerFromContent([]byte("---\nmetadata:\n  hermes:\n    upstream:\n      repo: a/b/c\n      path: x\n---\n")); ok {
		t.Fatal("malformed upstream accepted")
	}
	m, _ := o.Inspect(context.Background(), "official/real")
	if m == nil || m.Identifier != "official/cat/real" || *m.Path != "optional-skills/cat/real" {
		t.Fatal(m)
	}
}

func TestScanCacheFileKeepsPythonKeyOrderAndASCII(t *testing.T) {
	dir := t.TempDir()
	skill := filepath.Join(dir, "s")
	writeTestFile(t, filepath.Join(skill, "SKILL.md"), "---\nname: s\ndescription: d\n---\nrm -rf / café\n")
	if _, _, err := ScanSkillCached(skill, "github", "https://x/<y>", filepath.Join(dir, "cache")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "cache"))
	data, _ := os.ReadFile(filepath.Join(dir, "cache", entries[0].Name()))
	text := string(data)
	var order []int
	for _, k := range scanProvenanceOrder {
		order = append(order, strings.Index(text, "\n  \""+k+"\": "))
	}
	if !strings.HasPrefix(text, "{\n  \"bundle_hash\": ") || !slices.IsSorted(order) || order[0] < 0 || !strings.HasSuffix(text, "\n  \"fresh\": true\n}\n") {
		t.Fatal(text)
	}
	if !strings.Contains(text, "https://x/<y>") || strings.ContainsFunc(text, func(r rune) bool { return r > 127 }) {
		t.Fatal(text)
	}
}
