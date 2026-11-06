package skills

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Source answering fixed results after an optional sleep and counting its searches
type countingSource struct {
	SourceBase
	results   []SkillMeta
	sleep     time.Duration
	available bool
	err       error
	calls     atomic.Int32
}

func (c *countingSource) Search(context.Context, string, int) ([]SkillMeta, error) {
	c.calls.Add(1)
	time.Sleep(c.sleep)
	if c.err != nil {
		return nil, c.err
	}
	return slices.Clone(c.results), nil
}
func (c *countingSource) Fetch(context.Context, string) (*SkillBundle, error) { return nil, nil }
func (c *countingSource) Inspect(context.Context, string) (*SkillMeta, error) { return nil, nil }
func (c *countingSource) IsAvailable() bool                                   { return c.available }

func metaIDs(ms []SkillMeta) []string {
	out := []string{}
	for _, m := range ms {
		out = append(out, m.Identifier)
	}
	return out
}

func countsMap(o *pyjson.Object) map[string]any {
	out := map[string]any{}
	for _, k := range o.Keys {
		out[k], _ = o.Get(k)
	}
	return out
}

func TestUnifiedSearchDedupKeepsFirstSeen(t *testing.T) {
	h, _ := newTestHub(t)
	a := &countingSource{SourceBase: SourceBase{ID: "a"}, results: []SkillMeta{{Name: "skill", Description: "from A", Source: "a", Identifier: "shared/skill", TrustLevel: "community"}}}
	b := &countingSource{SourceBase: SourceBase{ID: "b"}, results: []SkillMeta{{Name: "skill", Description: "from B", Source: "b", Identifier: "shared/skill", TrustLevel: "community"}}}
	b.sleep = 20 * time.Millisecond
	got := h.UnifiedSearch(context.Background(), "skill", []SkillSource{a, b}, "all", 10)
	if len(got) != 1 || got[0].Description != "from A" {
		t.Fatalf("%+v", got)
	}
}

func TestUnifiedSearchDedupPrefersBuiltinOverTrusted(t *testing.T) {
	h, _ := newTestHub(t)
	a := &countingSource{SourceBase: SourceBase{ID: "a"}, results: []SkillMeta{{Name: "skill", Description: "builtin", Source: "a", Identifier: "shared/skill", TrustLevel: "builtin"}}}
	b := &countingSource{SourceBase: SourceBase{ID: "b"}, results: []SkillMeta{{Name: "skill", Description: "trusted", Source: "b", Identifier: "shared/skill", TrustLevel: "trusted"}}}
	got := h.UnifiedSearch(context.Background(), "skill", []SkillSource{a, b}, "all", 10)
	if len(got) != 1 || got[0].TrustLevel != "builtin" {
		t.Fatalf("%+v", got)
	}
}

func TestUnifiedSearchSourceErrorHandled(t *testing.T) {
	h, _ := newTestHub(t)
	failing := &countingSource{SourceBase: SourceBase{ID: "fail"}, err: errors.New("boom")}
	ok := &countingSource{SourceBase: SourceBase{ID: "ok"}, results: []SkillMeta{{Name: "s1", Description: "d", Source: "ok", Identifier: "x", TrustLevel: "community"}}}
	if got := h.UnifiedSearch(context.Background(), "query", []SkillSource{failing, ok}, "all", 10); len(got) != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestHermesIndexSearchMatchesIdentifierAndProvider(t *testing.T) {
	h, _ := newTestHub(t)
	x := NewHermesIndexSource(h, anonAuth(h))
	x.index, x.loaded = objectOf("skills", pyList(t, `[{"name": "accelerated-computing-cudf", "description": "GPU DataFrames.",
		"source": "github", "identifier": "NVIDIA/skills/skills/accelerated-computing-cudf", "tags": [], "extra": {"provider": "NVIDIA"}},
		{"name": "unrelated", "description": "nothing here", "source": "clawhub", "identifier": "clawhub/unrelated", "tags": []}]`)), true
	hits, err := x.Search(context.Background(), "nvidia", 25)
	if err != nil {
		t.Fatal(err)
	}
	ids := metaIDs(hits)
	if !slices.Contains(ids, "NVIDIA/skills/skills/accelerated-computing-cudf") || slices.Contains(ids, "clawhub/unrelated") {
		t.Fatalf("ids %v", ids)
	}
}

func TestFilterResultsByProviderNarrowsExactly(t *testing.T) {
	results := []SkillMeta{
		{Name: "a", Source: "github", Identifier: "NVIDIA/skills/a", TrustLevel: "trusted", Extra: objectOf("provider", "NVIDIA")},
		{Name: "b", Source: "github", Identifier: "openai/skills/b", TrustLevel: "trusted", Extra: objectOf("provider", "OpenAI")},
		{Name: "c", Source: "official", Identifier: "official/c", TrustLevel: "builtin"},
	}
	if ids := metaIDs(filterResultsByProvider(results, "nvidia")); !slices.Equal(ids, []string{"NVIDIA/skills/a"}) {
		t.Fatalf("nvidia %v", ids)
	}
	if ids := metaIDs(filterResultsByProvider(results, "openai")); !slices.Equal(ids, []string{"openai/skills/b"}) {
		t.Fatalf("openai %v", ids)
	}
}

func TestUnifiedSearchProviderFilterKeepsIndexSource(t *testing.T) {
	h, _ := newTestHub(t)
	src := &countingSource{SourceBase: SourceBase{ID: "hermes-index"}, available: true, results: []SkillMeta{
		{Name: "cuda", Description: "gpu", Source: "github", Identifier: "NVIDIA/skills/cuda", TrustLevel: "trusted", Extra: objectOf("provider", "NVIDIA")},
		{Name: "cuda-clone", Description: "gpu", Source: "clawhub", Identifier: "clawhub/cuda-clone", TrustLevel: "community"}}}
	if ids := metaIDs(h.UnifiedSearch(context.Background(), "cuda", []SkillSource{src}, "nvidia", 25)); !slices.Equal(ids, []string{"NVIDIA/skills/cuda"}) {
		t.Fatalf("ids %v", ids)
	}
}

func humanizar(sid string) SkillMeta {
	return SkillMeta{Name: "humanizar", Description: "x", Source: sid, Identifier: sid + "/humanizar", TrustLevel: "community"}
}

func indexMissSources(indexResults []SkillMeta) (index, skillsSh, github *countingSource) {
	index = &countingSource{SourceBase: SourceBase{ID: "hermes-index"}, results: indexResults, available: true}
	skillsSh = &countingSource{SourceBase: SourceBase{ID: "skills-sh"}, results: []SkillMeta{humanizar("skills-sh")}}
	github = &countingSource{SourceBase: SourceBase{ID: "github"}, results: []SkillMeta{humanizar("github")}}
	return
}

func TestIndexMissConsultsDisplacedRegistriesButNotGitHub(t *testing.T) {
	h, _ := newTestHub(t)
	index, skillsSh, github := indexMissSources(nil)
	results, counts, timedOut := h.ParallelSearchSources(context.Background(), []SkillSource{index, skillsSh, github}, "humanizar", nil, "all", 5*time.Second, nil)
	if !slices.Equal(metaIDs(results), []string{"skills-sh/humanizar"}) || !maps.Equal(countsMap(counts), map[string]any{"hermes-index": 0, "skills-sh": 1}) ||
		len(timedOut) != 0 || github.calls.Load() != 0 {
		t.Fatalf("%v %v %v github %d", metaIDs(results), countsMap(counts), timedOut, github.calls.Load())
	}
	index, skillsSh, github = indexMissSources(nil)
	results, _, _ = h.ParallelSearchSources(context.Background(), []SkillSource{index, skillsSh, github}, "", nil, "all", 5*time.Second, nil)
	if len(results) != 0 || skillsSh.calls.Load() != 0 {
		t.Fatalf("browse fanned out: %v %d", results, skillsSh.calls.Load())
	}
}

func TestIndexHitLeavesRegistriesUntouched(t *testing.T) {
	h, _ := newTestHub(t)
	index, skillsSh, github := indexMissSources([]SkillMeta{humanizar("hermes-index")})
	results, counts, _ := h.ParallelSearchSources(context.Background(), []SkillSource{index, skillsSh, github}, "humanizar", nil, "all", 5*time.Second, nil)
	if !slices.Equal(metaIDs(results), []string{"hermes-index/humanizar"}) || !maps.Equal(countsMap(counts), map[string]any{"hermes-index": 1}) ||
		skillsSh.calls.Load() != 0 || github.calls.Load() != 0 {
		t.Fatalf("%v %v", metaIDs(results), countsMap(counts))
	}
}

func TestProviderFilterMissSkipsRegistriesWithoutProviderData(t *testing.T) {
	h, _ := newTestHub(t)
	index, skillsSh, github := indexMissSources(nil)
	clawhub := &countingSource{SourceBase: SourceBase{ID: "clawhub"}, sleep: 5 * time.Second}
	started := time.Now()
	results, counts, timedOut := h.ParallelSearchSources(context.Background(), []SkillSource{index, skillsSh, clawhub, github}, "foo", nil, "nvidia", 5*time.Second, nil)
	if time.Since(started) >= time.Second || len(results) != 0 || len(timedOut) != 0 || !maps.Equal(countsMap(counts), map[string]any{"hermes-index": 0}) ||
		skillsSh.calls.Load() != 0 || clawhub.calls.Load() != 0 {
		t.Fatalf("%v %v %v", results, countsMap(counts), timedOut)
	}
}

func TestIndexMissFallbackPassHasItsOwnShortBudget(t *testing.T) {
	prev := indexMissFallbackBudget
	indexMissFallbackBudget = 300 * time.Millisecond
	t.Cleanup(func() { indexMissFallbackBudget = prev })
	h, _ := newTestHub(t)
	index, skillsSh, github := indexMissSources(nil)
	clawhub := &countingSource{SourceBase: SourceBase{ID: "clawhub"}, sleep: 5 * time.Second}
	started := time.Now()
	results, counts, timedOut := h.ParallelSearchSources(context.Background(), []SkillSource{index, skillsSh, clawhub, github}, "humanizar", nil, "all", 30*time.Second, nil)
	if time.Since(started) >= 2*time.Second || !slices.Equal(metaIDs(results), []string{"skills-sh/humanizar"}) ||
		!maps.Equal(countsMap(counts), map[string]any{"hermes-index": 0, "skills-sh": 1}) || !slices.Equal(timedOut, []string{"clawhub"}) {
		t.Fatalf("%v %v %v", metaIDs(results), countsMap(counts), timedOut)
	}
}

func TestLoadHermesIndexFetchDoesNotRequestBrotli(t *testing.T) {
	h, _ := newTestHub(t)
	var accept string
	h.io.httpGet = plainGet(func(_ string, o hubGet) *pyhttpx.Response {
		accept = o.Headers["Accept-Encoding"]
		return jsonResp(t, `{"skills": [{"name": "x"}]}`)
	})
	data, err := pyjson.Dumps(h.loadHermesIndex(context.Background()))
	if err != nil || data != `{"skills": [{"name": "x"}]}` {
		t.Fatalf("%s %v", data, err)
	}
	for _, tok := range strings.Split(accept, ",") {
		if strings.TrimSpace(tok) == "br" {
			t.Fatalf("index fetch must not request Brotli, got Accept-Encoding=%q", accept)
		}
	}
}

func TestLoadHermesIndexPersistentDecodingErrorFallsBackToStaleCache(t *testing.T) {
	h, _ := newTestHub(t)
	writeTestFile(t, h.hermesIndexCacheFile(), `{"skills": [{"name": "stale"}]}`)
	old := time.Now().Add(-(time.Duration(HermesIndexTTL*float64(time.Second)) + 100*time.Second))
	if err := os.Chtimes(h.hermesIndexCacheFile(), old, old); err != nil {
		t.Fatal(err)
	}
	var encodings []string
	h.io.httpGet = plainGet(func(_ string, o hubGet) *pyhttpx.Response {
		encodings = append(encodings, o.Headers["Accept-Encoding"])
		hd := http.Header{}
		hd.Set("Content-Encoding", "br")
		return &pyhttpx.Response{StatusCode: 200, Header: hd, Content: []byte("brotli boom")}
	})
	data, err := pyjson.Dumps(h.loadHermesIndex(context.Background()))
	if err != nil || data != `{"skills": [{"name": "stale"}]}` || !slices.Equal(encodings, []string{"gzip, deflate", "identity"}) {
		t.Fatalf("%s %v %v", data, err, encodings)
	}
}
