package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const catalogTestSha = "38fe0fb53eff98d477f807432e965429e665ca33"

// Catalog entry mapping with overrides applied
func catalogEntryMap(name string, over map[string]any) map[string]any {
	data := map[string]any{"name": name, "repo": "https://github.com/owner/repo", "sha": catalogTestSha, "description": "d",
		"maintainer": "owner", "tier": "community", "capabilities": map[string]any{"provides_tools": []any{"t1"}}}
	for k, v := range over {
		data[k] = v
	}
	return data
}

// Writes one YAML catalog file
func writeCatalogYAML(t *testing.T, dir, file string, data any, prefix string) {
	t.Helper()
	raw, err := yaml.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), append([]byte(prefix), raw...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Entries of a catalog directory keyed by name
func catalogByName(entries []*PluginCatalogEntry) map[string]*PluginCatalogEntry {
	out := map[string]*PluginCatalogEntry{}
	for _, e := range entries {
		out[e.Name] = e
	}
	return out
}

// Names of entries in order
func catalogNames(entries []*PluginCatalogEntry) []string {
	out := []string{}
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

func TestShippedCatalogEntriesAreAllValidAndPinned(t *testing.T) {
	files, err := fs.Glob(inTreeCatalog, "catalog/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	entries := LoadCatalog("")
	if len(entries) != len(files)-1 || len(entries) < 1 {
		t.Fatalf("%d entries from %d files", len(entries), len(files))
	}
	for _, e := range entries {
		if !catalogShaRe.MatchString(e.Sha) || !strings.HasPrefix(e.Repo, "https://") || !strings.HasPrefix(e.InstallIdentifier(), e.Repo) {
			t.Fatalf("bad shipped entry %+v", e)
		}
	}
	if got, err := GetPluginCatalogEntry("honcho"); err != nil || got == nil || got.Category != "memory" {
		t.Fatalf("honcho entry %+v %v", got, err)
	}
	if got, err := GetPluginCatalogEntry("no-such-plugin"); err != nil || got != nil {
		t.Fatalf("unknown entry %+v %v", got, err)
	}
}

func TestCatalogTierCategoryVersionImageAndScreenshots(t *testing.T) {
	dir := t.TempDir()
	writeCatalogYAML(t, dir, "a.yaml", catalogEntryMap("vendor", map[string]any{"tier": "verified"}), "")
	writeCatalogYAML(t, dir, "b.yaml", catalogEntryMap("bogus", map[string]any{"tier": "platinum"}), "")
	if got := catalogByName(LoadCatalog(dir)); len(got) != 1 || got["vendor"].Tier != "verified" {
		t.Fatalf("tiers %v", got)
	}
	dir = t.TempDir()
	writeCatalogYAML(t, dir, "a.yaml", catalogEntryMap("no-cat", nil), "")
	writeCatalogYAML(t, dir, "b.yaml", catalogEntryMap("mem", map[string]any{"category": "memory"}), "")
	writeCatalogYAML(t, dir, "c.yaml", catalogEntryMap("typo", map[string]any{"category": "memmory"}), "")
	got := catalogByName(LoadCatalog(dir))
	if len(got) != 2 || got["no-cat"].Category != "desktop" || got["mem"].ToDict()["category"] != "memory" {
		t.Fatalf("categories %v", got)
	}
	banner := "https://raw.githubusercontent.com/owner/repo/" + catalogTestSha + "/banner.png"
	dir = t.TempDir()
	writeCatalogYAML(t, dir, "a.yaml", catalogEntryMap("labelled", map[string]any{"version": "1.4.0", "image": banner}), "")
	writeCatalogYAML(t, dir, "b.yaml", catalogEntryMap("offhost", map[string]any{"version": "1.4.0 beta", "image": "https://evil.example/x.png"}), "")
	got = catalogByName(LoadCatalog(dir))
	if len(got) != 2 || got["labelled"].Version != "1.4.0" || got["labelled"].Image != banner ||
		got["offhost"].Version != "" || got["offhost"].Image != "" || got["labelled"].ToDict()["version"] != "1.4.0" {
		t.Fatalf("labels %+v %+v", got["labelled"], got["offhost"])
	}
	shot := "https://raw.githubusercontent.com/owner/repo/" + catalogTestSha + "/docs/1.png"
	dir = t.TempDir()
	writeCatalogYAML(t, dir, "a.yaml", catalogEntryMap("paged", map[string]any{"screenshots": []any{shot, "https://evil.example/x.png"}, "readme": true}), "")
	writeCatalogYAML(t, dir, "b.yaml", catalogEntryMap("plain", map[string]any{"readme": false}), "")
	writeCatalogYAML(t, dir, "c.yaml", catalogEntryMap("bare", nil), "")
	got = catalogByName(LoadCatalog(dir))
	if len(got["paged"].Screenshots) != 1 || got["paged"].Screenshots[0] != shot || !got["paged"].Readme ||
		len(got["plain"].Screenshots) != 0 || got["plain"].Readme || !got["bare"].Readme {
		t.Fatalf("screens %+v %+v %+v", got["paged"], got["plain"], got["bare"])
	}
	if d := got["paged"].ToDict(); len(d["screenshots"].([]any)) != 1 || d["readme"] != true {
		t.Fatalf("to dict %v", d)
	}
}

func TestInvalidCatalogEntriesAreSkippedNotRaised(t *testing.T) {
	dir := t.TempDir()
	writeCatalogYAML(t, dir, "a.yaml", catalogEntryMap("ok", map[string]any{"description": "café"}), "\ufeff")
	writeCatalogYAML(t, dir, "b.yaml", catalogEntryMap("short-sha", map[string]any{"sha": "abc123"}), "")
	writeCatalogYAML(t, dir, "c.yaml", catalogEntryMap("http-repo", map[string]any{"repo": "http://x/y"}), "")
	writeCatalogYAML(t, dir, "d.yaml", catalogEntryMap("Bad Name", nil), "")
	if err := os.WriteFile(filepath.Join(dir, "e.yaml"), []byte("- not\n- a mapping\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := LoadCatalog(dir)
	if len(entries) != 1 || entries[0].Name != "ok" || entries[0].Description != "café" {
		t.Fatalf("entries %v", catalogNames(entries))
	}
}

func TestFindRemovedReadsTheDirectoryKillList(t *testing.T) {
	dir := t.TempDir()
	writeCatalogYAML(t, dir, "removed.yaml", map[string]any{"removed": []any{map[string]any{"name": "evil",
		"repo": "https://github.com/x/evil.git", "reason": "malware", "date": "2026-01-01"}}}, "")
	ctx := context.Background()
	if r := FindRemoved(ctx, "evil", dir); r == nil || r.Reason != "malware" {
		t.Fatalf("by name %v", r)
	}
	if FindRemoved(ctx, "https://github.com/x/EVIL/", dir) == nil || FindRemoved(ctx, "https://github.com/x/fine", dir) != nil {
		t.Fatal("repo spelling")
	}
	for _, spelling := range []string{"git@github.com:x/evil.git", "ssh://git@github.com/x/evil", "http://github.com/x/evil",
		"https://www.github.com/x/evil/", "git://github.com/x/evil.git"} {
		if FindRemoved(ctx, spelling, dir) == nil {
			t.Fatal(spelling)
		}
	}
	if FindRemoved(ctx, "git@gitlab.com:x/evil.git", dir) != nil {
		t.Fatal("other host matched")
	}
}

// Counts requests the live client sends
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

// Points the live catalog at an unreachable URL under a fresh home and returns the cache path and request counter
func liveCatalogHome(t *testing.T) (context.Context, string, *countingTransport) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	counter := &countingTransport{}
	oldURL, oldClient, oldTree, oldTime := LiveCatalogURL, liveCatalogClient, inTreeCatalogEntries, inTreeCatalogTime
	LiveCatalogURL = "http://127.0.0.1:9/nope"
	liveCatalogClient = &http.Client{Timeout: liveCatalogRequestTimeout, Transport: counter}
	liveFetchFailedUntil = 0
	t.Cleanup(func() {
		LiveCatalogURL, liveCatalogClient, inTreeCatalogEntries, inTreeCatalogTime = oldURL, oldClient, oldTree, oldTime
		liveFetchFailedUntil = 0
		resetInTreeCatalogTime()
	})
	return context.Background(), liveCachePath(h), counter
}

// Writes a live catalog cache document
func writeLiveCache(t *testing.T, cache string, doc any, prefix string) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, append([]byte(prefix), raw...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLiveCatalogFallsBackToInTreeAndUnionsRemovals(t *testing.T) {
	for _, stale := range []bool{false, true} {
		ctx, cache, counter := liveCatalogHome(t)
		inTree := catalogNames(LoadCatalog(""))
		if got := catalogNames(LoadCatalogLive(ctx)); strings.Join(got, ",") != strings.Join(inTree, ",") {
			t.Fatalf("offline fallback %d entries", len(got))
		}
		writeLiveCache(t, cache, map[string]any{"entries": []any{catalogEntryMap("live-only", map[string]any{"description": "café"})},
			"removed": []any{map[string]any{"name": "pulled-live", "reason": "cve"}}}, "\ufeff")
		if stale {
			if err := os.Chtimes(cache, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
		}
		counter.n.Store(0)
		liveFetchFailedUntil = 0
		entries := LoadCatalogLive(ctx)
		if stale {
			if len(entries) != len(inTree) {
				t.Fatalf("stale cache kept pins %v", catalogNames(entries))
			}
		} else if len(entries) != 1 || entries[0].Name != "live-only" || entries[0].Description != "café" {
			t.Fatalf("fresh cache %v", catalogNames(entries))
		}
		if (counter.n.Load() > 0) != stale {
			t.Fatalf("stale=%v requests=%d", stale, counter.n.Load())
		}
		if r := FindRemoved(ctx, "pulled-live", ""); r == nil || r.Reason != "cve" {
			t.Fatalf("cached removal %v", r)
		}
		if err := os.WriteFile(cache, []byte("\ufeff{"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := LoadCatalogLive(ctx); len(got) != len(inTree) {
			t.Fatalf("corrupt cache %d", len(got))
		}
	}
}

// Seeds a fresh live cache document
func freshLiveCache(t *testing.T, doc map[string]any) (context.Context, string) {
	ctx, cache, _ := liveCatalogHome(t)
	writeLiveCache(t, cache, doc, "")
	return ctx, cache
}

// Live merge sha of one entry
func liveShaOf(ctx context.Context, name string) string {
	return catalogByName(LoadCatalogLive(ctx))[name].Sha
}

func TestNewerInTreePinOutranksAFreshLiveCache(t *testing.T) {
	old, newer := catalogTestSha, strings.Repeat("a", 40)
	ctx, _ := freshLiveCache(t, map[string]any{"generated_at": "2026-09-22T10:00:00Z",
		"entries": []any{catalogEntryMap("shared", map[string]any{"sha": old}), catalogEntryMap("live-only", nil)}, "removed": []any{}})
	inTreeCatalogEntries = func() []*PluginCatalogEntry {
		return []*PluginCatalogEntry{EntryFromMapping(catalogEntryMap("shared", map[string]any{"sha": newer}), "t")}
	}
	bump := *liveGeneratedTime(map[string]any{"generated_at": "2026-09-22T10:00:00Z"}) + 3600
	inTreeCatalogTime = func() *float64 { return &bump }
	byName := catalogByName(LoadCatalogLive(ctx))
	if byName["shared"].Sha != newer || byName["live-only"] == nil {
		t.Fatalf("newer tree %v", byName["shared"].Sha)
	}
	older := bump - 7200
	inTreeCatalogTime = func() *float64 { return &older }
	if liveShaOf(ctx, "shared") != old {
		t.Fatal("older checkout should take the live pin")
	}
}

func TestLiveCacheWriteNeverTruncatesThePreviousCopy(t *testing.T) {
	ctx, cache, _ := liveCatalogHome(t)
	writeLiveCache(t, cache, map[string]any{"entries": []any{catalogEntryMap("previous", nil)}, "removed": []any{}}, "")
	previous, _ := os.ReadFile(cache)
	fresh, _ := json.Marshal(map[string]any{"entries": []any{catalogEntryMap("fresh", nil)}, "removed": []any{}})
	liveCatalogClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: ioNopCloser(string(fresh)), Request: r, Header: http.Header{}}, nil
	})}
	oldWrite := writeLiveCatalog
	writeLiveCatalog = func(path, content, prefix string) error { return errors.New("disk full") }
	t.Cleanup(func() { writeLiveCatalog = oldWrite })
	got := FetchLiveCatalog(ctx, true)
	if now, _ := os.ReadFile(cache); string(now) != string(previous) {
		t.Fatalf("cache changed to %s", now)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(cache), "*.tmp*")); len(leftovers) != 0 {
		t.Fatalf("temp files %v", leftovers)
	}
	if entries, _ := got["entries"].([]any); len(entries) != 1 {
		t.Fatalf("stale copy %v", got)
	}
}

// Response body over a string
func ioNopCloser(s string) *nopBody { return &nopBody{strings.NewReader(s)} }

type nopBody struct{ *strings.Reader }

func (nopBody) Close() error { return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCuratedFieldsThePublishedDocLacksComeFromTheCheckout(t *testing.T) {
	saysNo := catalogEntryMap("says-no", map[string]any{"onboarding": false})
	ctx, _ := freshLiveCache(t, map[string]any{"generated_at": "2026-09-22T10:00:00Z", "entries": []any{
		catalogEntryMap("same", nil), saysNo, catalogEntryMap("repinned", map[string]any{"sha": strings.Repeat("b", 40)})}, "removed": []any{}})
	inTreeCatalogEntries = func() []*PluginCatalogEntry {
		var out []*PluginCatalogEntry
		for _, n := range []string{"same", "says-no", "repinned"} {
			out = append(out, EntryFromMapping(catalogEntryMap(n, map[string]any{"onboarding": true, "title": "T"}), n))
		}
		return out
	}
	zero := 0.0
	inTreeCatalogTime = func() *float64 { return &zero }
	byName := catalogByName(LoadCatalogLive(ctx))
	if !byName["same"].Onboarding || byName["same"].Title != "T" {
		t.Fatalf("same %+v", byName["same"])
	}
	if byName["says-no"].Onboarding || byName["says-no"].Title != "T" {
		t.Fatalf("says-no %+v", byName["says-no"])
	}
	if byName["repinned"].Onboarding || byName["repinned"].Sha != strings.Repeat("b", 40) {
		t.Fatalf("repinned %+v", byName["repinned"])
	}
}

// Runs git in dir with a fixed identity, failing the test on error
func catalogGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// Object types present in a repository without fetching anything
func localObjectTypes(dir string) map[string]bool {
	cmd := exec.Command("git", "cat-file", "--batch-all-objects", "--batch-check")
	cmd.Dir = dir
	out, _ := cmd.Output()
	types := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 1 {
			types[f[1]] = true
		}
	}
	return types
}

// A seeded source repository and its treeless no checkout clone
func treelessCatalogClone(t *testing.T, bump bool) (string, string) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(filepath.Join(src, "plugin-catalog"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(src, "plugin-catalog", "a.yaml"), "name: a\n")
	catalogGit(t, src, "init", "-q")
	catalogGit(t, src, "add", "-A")
	catalogGit(t, src, "commit", "-qm", "seed")
	if bump {
		writeFile(t, filepath.Join(src, "plugin-catalog", "a.yaml"), "name: a2\n")
		catalogGit(t, src, "add", "-A")
		catalogGit(t, src, "commit", "-qm", "bump")
	}
	catalogGit(t, src, "config", "uploadpack.allowFilter", "true")
	dst := filepath.Join(tmp, "treeless")
	catalogGit(t, tmp, "clone", "-q", "--filter=tree:0", "--no-checkout", "file://"+filepath.ToSlash(src), dst)
	if localObjectTypes(dst)["tree"] {
		t.Skip("file:// transport did not honor --filter=tree:0")
	}
	return src, dst
}

// Points the in-tree catalog time at a checkout and resolves it afresh
func useCatalogCheckout(t *testing.T, root string) {
	old := catalogCheckout
	catalogCheckout = func() (string, string) { return root, "plugin-catalog" }
	t.Cleanup(func() { catalogCheckout = old; resetInTreeCatalogTime() })
	resetInTreeCatalogTime()
}

func TestInTreeCatalogTimeDoesNotLazyFetchOnTreelessClones(t *testing.T) {
	src, dst := treelessCatalogClone(t, false)
	useCatalogCheckout(t, dst)
	if got := InTreeCatalogTime(); got != nil {
		t.Fatalf("treeless clone dated %v", *got)
	}
	if localObjectTypes(dst)["tree"] {
		t.Fatal("the probe lazy fetched trees")
	}
	useCatalogCheckout(t, src)
	if InTreeCatalogTime() == nil {
		t.Fatal("a full checkout should resolve its commit time")
	}
}

func TestTreelessCheckoutStillOutranksADocFetchedBeforeTheBump(t *testing.T) {
	_, dst := treelessCatalogClone(t, true)
	catalogGit(t, dst, "checkout", "-q")
	probe := exec.Command("git", "-C", dst, "log", "-1", "--format=%ct", "--", "plugin-catalog")
	probe.Env = append(os.Environ(), "GIT_NO_LAZY_FETCH=1")
	if probe.Run() == nil {
		t.Fatal("the probe should be blind on this layout")
	}
	old, newer := catalogTestSha, strings.Repeat("a", 40)
	ctx, _ := freshLiveCache(t, map[string]any{"generated_at": "2026-09-22T10:00:00Z",
		"entries": []any{catalogEntryMap("shared", map[string]any{"sha": old})}, "removed": []any{}})
	inTreeCatalogEntries = func() []*PluginCatalogEntry {
		return []*PluginCatalogEntry{EntryFromMapping(catalogEntryMap("shared", map[string]any{"sha": newer}), "t")}
	}
	useCatalogCheckout(t, dst)
	if liveShaOf(ctx, "shared") != newer {
		t.Fatal("fresh checkout should keep its pin")
	}
	_ = filepath.WalkDir(filepath.Join(dst, "plugin-catalog"), func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			_ = os.Chtimes(p, time.Unix(0, 0), time.Unix(0, 0))
		}
		return nil
	})
	resetInTreeCatalogTime()
	if liveShaOf(ctx, "shared") != old {
		t.Fatal("older catalog files should take the live pin")
	}
}

func TestKnownIssuesParseRoundTripAndSummary(t *testing.T) {
	base := map[string]any{"name": "hindsight", "repo": "https://github.com/vectorize-io/hindsight",
		"sha": "176f8c2de1369f569c489b831d143b78128b5535", "tier": "community", "category": "memory"}
	with := map[string]any{"known_issues": []any{"First issue.", "Second issue."}}
	for k, v := range base {
		with[k] = v
	}
	e := EntryFromMapping(with, "test-entry")
	if strings.Join(e.KnownIssues, "|") != "First issue.|Second issue." || len(e.ToDict()["known_issues"].([]any)) != 2 {
		t.Fatalf("known issues %v", e.KnownIssues)
	}
	dir := filepath.Join(t.TempDir(), "catalog")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "hindsight.yaml"), "name: hindsight\nrepo: https://github.com/vectorize-io/hindsight\n"+
		"sha: 176f8c2de1369f569c489b831d143b78128b5535\ndescription: test entry\nmaintainer: vectorize-io\n"+
		"known_issues:\n  - 'Trap one.'\n  - 'Trap two.'\n")
	loaded := LoadCatalog(dir)
	if len(loaded) != 1 || strings.Join(loaded[0].KnownIssues, "|") != "Trap one.|Trap two." {
		t.Fatalf("loaded %+v", loaded)
	}
	if bare := EntryFromMapping(base, "b"); len(bare.KnownIssues) != 0 || len(bare.ToDict()["known_issues"].([]any)) != 0 {
		t.Fatal("missing key should default empty")
	}
	issue := EntryFromMapping(map[string]any{"name": "hindsight", "repo": base["repo"], "sha": base["sha"],
		"known_issues": []any{"Local embedded mode is not supported on PM-managed Icebath."}}, "x")
	if s := EntryCapabilitySummary(issue); !strings.Contains(s, "Known issues: Local embedded mode is not supported on PM-managed Icebath.") {
		t.Fatalf("summary %q", s)
	}
}
