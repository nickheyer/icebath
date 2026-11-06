package skills

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Anonymous credential chain, the MagicMock(spec=GitHubAuth) of the hermes tests
func anonAuth(h *Hub) *GitHubAuth {
	a := NewGitHubAuth(h.S)
	none := func() string { return "" }
	a.tryPat, a.tryGhCLI, a.tryGitHubApp = none, none, none
	return a
}

func githubTestSource(t *testing.T) (*GitHubSource, *Hub) {
	h, _ := newTestHub(t)
	h.io.readIndexCache = func(string) (any, error) { return nil, nil }
	h.io.writeIndexCache = func(string, any) error { return nil }
	h.io.isSafeURL = func(string) bool { return true }
	h.io.siteAccess = func(string) *tools.WebsiteBlock { return nil }
	return NewGitHubSource(h, anonAuth(h), nil), h
}

func pyList(t *testing.T, js string) []any {
	t.Helper()
	v, err := pyjson.Loads(js)
	if err != nil {
		t.Fatal(err)
	}
	return v.([]any)
}

func plainGet(fn func(url string, o hubGet) *pyhttpx.Response) func(context.Context, string, hubGet) (*pyhttpx.Response, error) {
	return func(_ context.Context, url string, o hubGet) (*pyhttpx.Response, error) { return fn(url, o), nil }
}

func TestParseFrontmatterQuick(t *testing.T) {
	fm := githubParseFrontmatterQuick("---\nname: test-skill\ndescription: A test.\n---\n\n# Body\n")
	if fm["name"] != "test-skill" || fm["description"] != "A test." {
		t.Fatalf("%v", fm)
	}
	nested := githubParseFrontmatterQuick("---\nname: test\nmetadata:\n  hermes:\n    tags: [a, b]\n---\n\nBody.\n")
	tags := nested["metadata"].(map[string]any)["hermes"].(map[string]any)["tags"]
	if !reflect.DeepEqual(tags, []any{"a", "b"}) {
		t.Fatalf("%v", tags)
	}
	for _, content := range []string{"# Just a heading\nSome body text.\n", "---\nname: test\nno closing here\n", "",
		"---\n: : : invalid{{\n---\n\nBody.\n", "---\n- just a list\n- of items\n---\n\nBody.\n"} {
		if fm := githubParseFrontmatterQuick(content); len(fm) != 0 {
			t.Fatalf("%q -> %v", content, fm)
		}
	}
}

func TestSkillsShGroupings(t *testing.T) {
	mapping, ok := parseSkillshGroupings(`{"$schema": "https://skills.sh/schemas/skills.sh.schema.json", "groupings": [` +
		`{"title": "Inference AI", "skills": ["dynamo-router", "dynamo-recipe"]}, {"title": "Decision Optimization", "skills": ["cuopt-developer"]}]}`)
	want := map[string]string{"dynamo-router": "Inference AI", "dynamo-recipe": "Inference AI", "cuopt-developer": "Decision Optimization"}
	if !ok || !reflect.DeepEqual(mapping, want) {
		t.Fatalf("%v", mapping)
	}
	for _, c := range []struct {
		repo, dir, name, trust string
		groupings              map[string]string
		want                   string
	}{
		{"K-Dense-AI/scientific-agent-skills", "rdkit", "rdkit", "community", nil, "science"},
		{"NVIDIA/skills", "cuopt-developer", "cuopt-developer", "trusted", map[string]string{"cuopt-developer": "Decision Optimization"}, "Decision Optimization"},
	} {
		src, _ := githubTestSource(t)
		src.seams.getSkillshGroupings = func(string) map[string]string { return c.groupings }
		src.seams.inspect = func(string) *SkillMeta {
			return &SkillMeta{Name: c.name, Description: "d", Source: "github", Identifier: c.repo + "/skills/" + c.dir, TrustLevel: c.trust, Extra: pyjson.NewObject()}
		}
		src.seams.githubGet = func(string, ghGet) *pyhttpx.Response {
			return textResp(200, `[{"type": "dir", "name": "`+c.dir+`"}]`)
		}
		skills := src.listSkillsInRepo(context.Background(), c.repo, "skills/", "science")
		if len(skills) != 1 {
			t.Fatalf("%v", skills)
		}
		if got, _ := skills[0].Extra.Get("category"); got != c.want {
			t.Fatalf("category %v want %s", got, c.want)
		}
	}
}

func TestTrustLevelFor(t *testing.T) {
	src, _ := githubTestSource(t)
	taps := map[string]bool{}
	for _, tap := range DefaultGitHubTaps {
		taps[tap.Repo] = true
	}
	for repo := range TrustedRepos {
		if src.TrustLevelFor(repo+"/some-skill") != "trusted" {
			t.Fatalf("%s not trusted", repo)
		}
		if !taps[repo] {
			t.Fatalf("Trusted repo %q is in TRUSTED_REPOS but missing from GitHubSource.DEFAULT_TAPS", repo)
		}
	}
}

func TestGitHubSourceFileFetchQuotesDecodedSupportPath(t *testing.T) {
	src, _ := githubTestSource(t)
	var urls []string
	src.seams.githubGet = func(url string, _ ghGet) *pyhttpx.Response { urls = append(urls, url); return nil }
	src.fetchFileBytes(context.Background(), "owner/repo", "skill/references/foo#bar.md", "")
	if urls[0] != "https://api.github.com/repos/owner/repo/contents/skill/references/foo%23bar.md" {
		t.Fatal(urls)
	}
}

func TestRejectedTokenFallsThroughToNextCredential(t *testing.T) {
	for _, gh := range []string{"gh-live", "gh-dead"} {
		src, h := githubTestSource(t)
		src.Auth.tryPat = func() string { return "pat-dead" }
		src.Auth.tryGhCLI = func() string { return gh }
		var sent []map[string]string
		h.io.httpGet = plainGet(func(_ string, o hubGet) *pyhttpx.Response {
			sent = append(sent, o.Headers)
			if strings.HasSuffix(o.Headers["Authorization"], "-dead") {
				return textResp(401, "body")
			}
			return textResp(200, "body")
		})
		b, ok := src.fetchFileBytes(context.Background(), "o/r", "s/SKILL.md", "")
		if !ok || string(b) != "body" {
			t.Fatal(gh)
		}
		var got []string
		for _, hd := range sent {
			got = append(got, hd["Authorization"])
		}
		want := []string{"token pat-dead", "token gh-live"}
		if gh == "gh-dead" {
			want = []string{"token pat-dead", "token gh-dead", ""}
		}
		if !slices.Equal(got, want) || sent[len(sent)-1]["Accept"] != "application/vnd.github.v3.raw" || src.Auth.Rejected()[0] != "GITHUB_TOKEN/GH_TOKEN" {
			t.Fatalf("%s %v", gh, sent)
		}
	}
}

func TestPinnedFileComesFromRawAndSpendsNoAPICall(t *testing.T) {
	for _, rawStatus := range []int{200, 404} {
		src, h := githubTestSource(t)
		var hosts []string
		h.io.httpGet = plainGet(func(url string, _ hubGet) *pyhttpx.Response {
			hosts = append(hosts, strings.Split(url, "/")[2])
			if strings.Contains(url, "raw.githubusercontent.com") && rawStatus != 200 {
				return textResp(rawStatus, "")
			}
			return textResp(200, "body")
		})
		b, _ := src.fetchFileBytes(context.Background(), "o/r", "s/a b.md", strings.Repeat("c", 40))
		want := []string{"raw.githubusercontent.com"}
		if rawStatus != 200 {
			want = append(want, "api.github.com")
		}
		if string(b) != "body" || !slices.Equal(hosts, want) {
			t.Fatalf("%d %v", rawStatus, hosts)
		}
	}
}

func TestUnreachableRawHostFallsBackToTheAPIAndIsTriedOnce(t *testing.T) {
	src, h := githubTestSource(t)
	var hosts []string
	h.io.httpGet = func(_ context.Context, url string, _ hubGet) (*pyhttpx.Response, error) {
		hosts = append(hosts, strings.Split(url, "/")[2])
		if strings.Contains(url, "raw.githubusercontent.com") {
			return nil, &tools.SSRFConnectionBlocked{}
		}
		return textResp(200, "body"), nil
	}
	for i := range 3 {
		if b, _ := src.fetchFileBytes(context.Background(), "o/r", "s/"+string(rune('0'+i))+".md", strings.Repeat("c", 40)); string(b) != "body" {
			t.Fatal(i)
		}
	}
	if !slices.Equal(hosts, []string{"raw.githubusercontent.com", "api.github.com", "api.github.com", "api.github.com"}) {
		t.Fatal(hosts)
	}
}

func TestFindSkillInRepoTree(t *testing.T) {
	src, h := githubTestSource(t)
	entries := `[{"path": "README.md", "type": "blob"}, {"path": "cli-tool/components/skills/development/senior-backend/SKILL.md", "type": "blob"},` +
		` {"path": "cli-tool/components/skills/development/other/SKILL.md", "type": "blob"}]`
	h.io.httpGet = plainGet(func(url string, _ hubGet) *pyhttpx.Response {
		switch {
		case strings.HasSuffix(url, "/davila7/claude-code-templates"):
			return textResp(200, `{"default_branch": "main"}`)
		case strings.Contains(url, "/git/trees/main"):
			return textResp(200, `{"tree": `+entries+`}`)
		}
		return textResp(404, "")
	})
	id, ok := src.findSkillInRepoTree(context.Background(), "davila7/claude-code-templates", "senior-backend")
	if !ok || id != "davila7/claude-code-templates/cli-tool/components/skills/development/senior-backend" {
		t.Fatal(id)
	}
	src2, h2 := githubTestSource(t)
	h2.io.httpGet = plainGet(func(string, hubGet) *pyhttpx.Response { return textResp(404, "") })
	if _, ok := src2.findSkillInRepoTree(context.Background(), "owner/repo", "my-skill"); ok {
		t.Fatal("found in a failed repo")
	}
}

func TestFindsSingleGenericSkillsDirectoryWhenSlugIsStale(t *testing.T) {
	for _, c := range []struct{ lone, want string }{{"skills/SKILL.md", "tencent/wechatreading/skills"}, {"skills/bar/SKILL.md", ""}} {
		src, h := githubTestSource(t)
		calls := 0
		h.io.httpGet = plainGet(func(string, hubGet) *pyhttpx.Response {
			calls++
			if calls == 1 {
				return textResp(200, `{"default_branch": "main"}`)
			}
			return textResp(200, `{"tree": [{"path": "README.md", "type": "blob"}, {"path": "`+c.lone+`", "type": "blob"}]}`)
		})
		if got, _ := src.findSkillInRepoTree(context.Background(), "tencent/wechatreading", "weread-skills"); got != c.want {
			t.Fatalf("%s -> %q", c.lone, got)
		}
	}
}

// GitHub source whose API serves one tree holding SKILL.md and two support blobs, counting file reads
func githubSourceWithTree(t *testing.T, treeSHA string, calls *int) *GitHubSource {
	src, _ := githubTestSource(t)
	entries := `[{"path": "demo-skill/SKILL.md", "type": "blob", "sha": "sha-SKILL.md", "size": 3},` +
		`{"path": "demo-skill/scripts/run.sh", "type": "blob", "sha": "sha-scripts/run.sh", "size": 3},` +
		`{"path": "demo-skill/references/notes.md", "type": "blob", "sha": "sha-references/notes.md", "size": 3}]`
	api := map[string]string{"/repos/owner/repo": `{"default_branch": "main"}`, "/repos/owner/repo/git/trees/main": `{"sha": "` + treeSHA + `", "tree": ` + entries + `}`}
	src.seams.githubJSON = func(url string, _ ghGet) any {
		v, _ := pyjson.Loads(api[strings.SplitN(url, "api.github.com", 2)[1]])
		return v
	}
	body := "---\nname: demo-skill\n---\nSee scripts/run.sh and references/notes.md\n"
	src.seams.fetchFileContent = func(string, string, string) (string, bool) { *calls++; return body, true }
	src.seams.fetchFileBytes = func(string, string, string) ([]byte, bool) { *calls++; return []byte(body), true }
	return src
}

func TestUnchangedUpstreamRevisionSkipsBundleDownload(t *testing.T) {
	for _, c := range []struct {
		recorded, status string
		gets             int
	}{{strings.Repeat("a", 40), "up_to_date", 0}, {strings.Repeat("b", 40), "update_available", 3}} {
		calls := 0
		src := githubSourceWithTree(t, strings.Repeat("a", 40), &calls)
		h := src.h
		if err := os.MkdirAll(filepath.Join(h.S.SkillsDir(), "demo-skill"), 0o755); err != nil {
			t.Fatal(err)
		}
		lock := h.LockFile("")
		md := pyjson.NewObject()
		md.Set("source_revision", c.recorded)
		if err := lock.RecordInstall("demo-skill", "github", "owner/repo/demo-skill", "community", "safe", "installed-hash", "demo-skill", nil, md, nil); err != nil {
			t.Fatal(err)
		}
		results, err := h.CheckForSkillUpdates(context.Background(), "", lock, []SkillSource{src}, nil)
		if err != nil || len(results) != 1 || results[0].Status != c.status || calls != c.gets || (results[0].Bundle != nil) != (c.status == "update_available") {
			t.Fatalf("%+v %d %v", results, calls, err)
		}
		if c.status == "up_to_date" && (results[0].CurrentHash != "installed-hash" || results[0].LatestHash != "installed-hash") {
			t.Fatalf("%+v", results[0])
		}
	}
}

func TestBundleWithAFailedBlobFetchRecordsNoRevision(t *testing.T) {
	calls := 0
	src := githubSourceWithTree(t, strings.Repeat("a", 40), &calls)
	good := src.seams.fetchFileBytes
	src.seams.fetchFileBytes = func(repo, path, ref string) ([]byte, bool) {
		if strings.HasSuffix(path, "notes.md") {
			return nil, false
		}
		return good(repo, path, ref)
	}
	b, err := src.Fetch(context.Background(), "owner/repo/demo-skill")
	if err != nil || b == nil {
		t.Fatal(err)
	}
	if _, has := b.File("references/notes.md"); has {
		t.Fatal("gap filled")
	}
	if rev, _ := b.Metadata.Get("source_revision"); rev != "" {
		t.Fatal(rev)
	}
	src.seams.fetchFileBytes = good
	b, _ = src.Fetch(context.Background(), "owner/repo/demo-skill")
	if rev, _ := b.Metadata.Get("source_revision"); rev != strings.Repeat("a", 40) {
		t.Fatal(rev)
	}
}

func TestInspectStampsProviderInExtra(t *testing.T) {
	src, _ := githubTestSource(t)
	src.seams.fetchFileContent = func(string, string, string) (string, bool) {
		return "---\nname: accelerated-computing-cudf\ndescription: NVIDIA cuDF GPU DataFrames.\n---\n# body\n", true
	}
	m, err := src.Inspect(context.Background(), "NVIDIA/skills/skills/accelerated-computing-cudf")
	if err != nil || m == nil || m.Source != "github" {
		t.Fatal(err)
	}
	if p, _ := m.Extra.Get("provider"); p != "NVIDIA" {
		t.Fatal(p)
	}
}

func treeOf(t *testing.T, js string) func(string) *ghTree {
	entries := pyList(t, js)
	return func(string) *ghTree { return &ghTree{Branch: "main", Entries: entries} }
}

func TestFetchSkipsMissingReferencedFileAndKeepsTheRest(t *testing.T) {
	src, _ := githubTestSource(t)
	src.seams.fetchFileContent = func(string, string, string) (string, bool) {
		return "---\nname: demo\ndescription: demo\n---\n\nSee [guide](references/guide.md) and `references/missing.md`.\n", true
	}
	src.seams.getRepoTree = treeOf(t, `[{"path": "skills/demo/references/guide.md", "type": "blob", "mode": "100644"}]`)
	src.seams.fetchFileBytes = func(string, string, string) ([]byte, bool) { return []byte("# guide"), true }
	b, _ := src.Fetch(context.Background(), "owner/repo/skills/demo")
	if b == nil || b.Name != "demo" {
		t.Fatal(b)
	}
	if g, _ := b.File("references/guide.md"); string(g) != "# guide" {
		t.Fatal(string(g))
	}
	if _, has := b.File("references/missing.md"); has {
		t.Fatal("missing file bundled")
	}
}

func TestGitHubFetchesPinToTheTreeRevision(t *testing.T) {
	src, _ := githubTestSource(t)
	var refs []string
	src.seams.fetchFileContent = func(_, path, ref string) (string, bool) {
		refs = append(refs, ref)
		return "---\nname: x\ndescription: x\n---\nSee [the format](CONTEXT-FORMAT.md).\n", true
	}
	src.seams.fetchFileBytes = func(_, path, ref string) ([]byte, bool) { refs = append(refs, ref); return []byte("x"), true }
	src.treeCache["owner/repo"] = &ghTree{Branch: "main", Entries: pyList(t, `[{"path": "skill/SKILL.md", "type": "blob", "mode": "100644"},`+
		`{"path": "skill/CONTEXT-FORMAT.md", "type": "blob", "mode": "100644"}]`)}
	src.treeRevisions["owner/repo"] = "treesha123"
	if b, _ := src.Fetch(context.Background(), "owner/repo/skill"); b == nil || len(refs) == 0 {
		t.Fatal("no fetches")
	}
	for _, r := range refs {
		if r != "treesha123" {
			t.Fatal(refs)
		}
	}
}

func TestGitHubSourceFetchTreeShapes(t *testing.T) {
	const guideMD = "---\nname: dangling\ndescription: d\n---\nRead [the guide](references/guide.md).\n"
	src, _ := githubTestSource(t)
	src.seams.fetchFileContent = func(string, string, string) (string, bool) { return guideMD, true }
	src.seams.fetchFileBytes = func(string, string, string) ([]byte, bool) { return []byte("x"), true }
	src.treeCache["owner/repo"] = &ghTree{Branch: "main", Entries: pyList(t, `[{"path": "skill/SKILL.md", "type": "blob", "mode": "100644"}]`)}
	b, _ := src.Fetch(context.Background(), "owner/repo/skill")
	if b == nil {
		t.Fatal("dangling link aborted")
	}
	if _, has := b.File("references/guide.md"); has {
		t.Fatal("dangling file bundled")
	}
	src.treeCache["owner/repo"] = &ghTree{Branch: "main", Entries: pyList(t, `[{"path": "skill/SKILL.md", "type": "blob", "mode": "100644"},`+
		`{"path": "skill/references/guide.md", "type": "blob", "mode": "120000"}]`)}
	if b, _ := src.Fetch(context.Background(), "owner/repo/skill"); b != nil {
		t.Fatal("symlinked reference accepted")
	}

	full, _ := githubTestSource(t)
	full.seams.fetchFileContent = func(string, string, string) (string, bool) {
		return "---\nname: full-dir\ndescription: d\n---\nSee [audit](reference/audit.md) and run `node scripts/pin.mjs`.\n", true
	}
	full.seams.fetchFileBytes = func(_, path, _ string) ([]byte, bool) { return []byte("content-of-" + path), true }
	full.treeCache["owner/repo"] = &ghTree{Branch: "main", Entries: pyList(t, `[{"path": "skill/SKILL.md", "type": "blob", "mode": "100644"},`+
		`{"path": "skill/reference/audit.md", "type": "blob", "mode": "100644"}, {"path": "skill/reference/deep/native.md", "type": "blob", "mode": "100644"},`+
		`{"path": "skill/scripts/pin.mjs", "type": "blob", "mode": "100644"}, {"path": "skill/LICENSE", "type": "blob", "mode": "100644"},`+
		`{"path": "skill/reference/link.md", "type": "blob", "mode": "120000"}, {"path": "skill/.hidden", "type": "blob", "mode": "100644"},`+
		`{"path": "skill/scripts/x.pyc", "type": "blob", "mode": "100644"}, {"path": "other/README.md", "type": "blob", "mode": "100644"}]`)}
	b, _ = full.Fetch(context.Background(), "owner/repo/skill")
	var paths []string
	for _, f := range b.Files {
		paths = append(paths, f.Path)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"LICENSE", "SKILL.md", "reference/audit.md", "reference/deep/native.md", "scripts/pin.mjs"}) {
		t.Fatal(paths)
	}
}

func TestGitHubGetRetriesRateLimitAndFlagsExhaustion(t *testing.T) {
	src, h := githubTestSource(t)
	var waits []time.Duration
	src.seams.sleep = func(d time.Duration) { waits = append(waits, d) }
	statuses := []int{429, 503, 200}
	h.io.httpGet = plainGet(func(string, hubGet) *pyhttpx.Response {
		r := textResp(statuses[0], "{}")
		if statuses[0] == 429 {
			r.Header.Set("Retry-After", "7")
		}
		statuses = statuses[1:]
		return r
	})
	if r := src.githubGet(context.Background(), githubAPI+"/o/r", ghGet{}); r == nil || r.StatusCode != 200 || src.IsRateLimited() {
		t.Fatal(r)
	}
	if !slices.Equal(waits, []time.Duration{7 * time.Second, 2 * time.Second}) {
		t.Fatal(waits)
	}
	h.io.httpGet = plainGet(func(string, hubGet) *pyhttpx.Response {
		r := textResp(403, "{}")
		r.Header.Set("X-RateLimit-Remaining", "0")
		return r
	})
	if r := src.githubGet(context.Background(), githubAPI+"/o/r", ghGet{}); r == nil || r.StatusCode != 403 || !src.IsRateLimited() {
		t.Fatal("terminal exhaustion not flagged")
	}
	h.io.httpGet = plainGet(func(string, hubGet) *pyhttpx.Response { return textResp(403, "{}") })
	waits = nil
	if r := src.githubGet(context.Background(), githubAPI+"/o/r", ghGet{}); r.StatusCode != 403 || len(waits) != 0 {
		t.Fatal("plain 403 retried")
	}
}
