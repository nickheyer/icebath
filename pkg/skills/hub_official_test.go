package skills

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Optional source over a fresh optional-skills dir with the given live repo dirs already listed
func optionalSource(t *testing.T, root string, remoteDirs ...string) *OptionalSkillSource {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", root)
	h, _ := newTestHub(t)
	o := NewOptionalSkillSource(h, anonAuth(h))
	if remoteDirs != nil {
		o.remoteDirs = pyjson.NewObject()
		for _, d := range remoteDirs {
			o.remoteDirs.Set(d, true)
		}
	}
	return o
}

// GitHub source that fails the test on any use, the MagicMock whose fetch must never be called
func untouchableGitHub(t *testing.T, o *OptionalSkillSource) {
	o.github = NewGitHubSource(o.h, o.auth, nil)
	o.github.seams.fetch = func(id string) *SkillBundle { t.Fatalf("github fetch called with %q", id); return nil }
	o.github.seams.getRepoTree = func(repo string) *ghTree { t.Fatalf("repo tree read for %q", repo); return nil }
}

// GitHub source whose repo tree lists each dir's SKILL.md plus extra files served byte exact
func liveRepoGitHub(t *testing.T, o *OptionalSkillSource, remoteDirs []string, extra ...string) {
	var entries []any
	contents := map[string][]byte{}
	add := func(p string, data []byte) {
		entries = append(entries, objectOf("type", "blob", "path", p, "mode", "100644"))
		contents[p] = data
	}
	for _, d := range remoteDirs {
		add("optional-skills/"+d+"/SKILL.md", []byte("---\nname: "+d[strings.LastIndex(d, "/")+1:]+"\n---\nBody"))
	}
	for i := 0; i+1 < len(extra); i += 2 {
		add(extra[i], []byte(extra[i+1]))
	}
	o.github = NewGitHubSource(o.h, o.auth, nil)
	o.github.seams.getRepoTree = func(string) *ghTree { return &ghTree{Branch: "main", Entries: entries} }
	o.github.seams.fetchFileBytes = func(_, p, _ string) ([]byte, bool) {
		data, ok := contents[p]
		return data, ok
	}
}

func TestOptionalScanAllEmitsRepoRootRelativeMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "optional-skills")
	writeTestFile(t, filepath.Join(root, "finance", "3-statement-model", "SKILL.md"), "---\nname: 3-statement-model\ndescription: test\n---\n\nBody\n")
	o := optionalSource(t, root)
	meta, err := o.Inspect(context.Background(), "official/finance/3-statement-model")
	if err != nil || meta == nil || meta.Repo == nil || *meta.Repo != "NousResearch/hermes-agent" ||
		meta.Path == nil || *meta.Path != "optional-skills/finance/3-statement-model" {
		t.Fatalf("%+v %v", meta, err)
	}
}

func TestOptionalScanAllAcceptsInstallPrefixButRejectsNestedSupportSkills(t *testing.T) {
	root := filepath.Join(t.TempDir(), "venv", "lib", "site-packages", "optional-skills")
	real := filepath.Join(root, "research", "real-skill")
	writeTestFile(t, filepath.Join(real, "SKILL.md"), "---\nname: real-skill\ndescription: real\n---\n")
	writeTestFile(t, filepath.Join(real, "references", "archived-skill", "SKILL.md"), "---\nname: archived-skill\ndescription: nested\n---\n")
	o := optionalSource(t, root)
	var names []string
	for _, m := range o.scanAll() {
		names = append(names, m.Name)
	}
	if !slices.Equal(names, []string{"real-skill"}) {
		t.Fatalf("names %v", names)
	}
	if _, ok := o.findSkillDir("archived-skill"); ok {
		t.Fatal("nested support skill found")
	}
}

func TestOptionalFetchPreservesBinaryAssets(t *testing.T) {
	root := filepath.Join(t.TempDir(), "optional-skills")
	dir := filepath.Join(root, "mlops", "models", "neutts")
	writeTestFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: neutts\ndescription: test\n---\n\nBody\n")
	writeTestFile(t, filepath.Join(dir, "assets", "neutts-cli", "samples", "jo.wav"), "RIFF\x00\x01fakewav")
	writeTestFile(t, filepath.Join(dir, "assets", "neutts-cli", "samples", "jo.txt"), "hello\n")
	writeTestFile(t, filepath.Join(dir, "assets", "neutts-cli", "src", "neutts_cli", "__pycache__", "cli.cpython-312.pyc"), "junk")
	o := optionalSource(t, root)
	b, err := o.Fetch(context.Background(), "official/mlops/models/neutts")
	if err != nil || b == nil {
		t.Fatalf("%v %v", b, err)
	}
	if !bundleHas(b, "assets/neutts-cli/samples/jo.wav", "RIFF\x00\x01fakewav") || !bundleHas(b, "assets/neutts-cli/samples/jo.txt", "hello\n") {
		t.Fatalf("files %v", bundlePaths(b))
	}
	if _, ok := b.File("assets/neutts-cli/src/neutts_cli/__pycache__/cli.cpython-312.pyc"); ok {
		t.Fatal("pycache shipped")
	}
}

func TestOptionalFetchRejectsSiblingDirectoryTraversal(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "optional-skills")
	writeTestFile(t, filepath.Join(tmp, "optional-skills-escape", "pwned", "SKILL.md"), "---\nname: pwned\ndescription: traversal\n---\n\nBody\n")
	o := optionalSource(t, root)
	untouchableGitHub(t, o)
	o.remoteDirs = pyjson.NewObject()
	if b, err := o.Fetch(context.Background(), "official/../optional-skills-escape/pwned"); b != nil || err != nil {
		t.Fatalf("%v %v", b, err)
	}
}

func TestOptionalFetchFallsBackToLiveRepoWhenMissingLocally(t *testing.T) {
	o := optionalSource(t, filepath.Join(t.TempDir(), "optional-skills"), "software-development/ast-grep")
	liveRepoGitHub(t, o, []string{"software-development/ast-grep"},
		"optional-skills/software-development/ast-grep/install.sh", "#!/bin/sh\n",
		"optional-skills/software-development/ast-grep/LICENSE", "MIT")
	b, err := o.Fetch(context.Background(), "official/software-development/ast-grep")
	if err != nil || b == nil {
		t.Fatalf("%v %v", b, err)
	}
	if b.Source != "official" || b.Identifier != "official/software-development/ast-grep" || b.TrustLevel != "builtin" ||
		!bundleHas(b, "install.sh", "#!/bin/sh\n") || !bundleHas(b, "LICENSE", "MIT") {
		t.Fatalf("%+v %v", b, bundlePaths(b))
	}
}

func TestOptionalFetchBareNameResolvesViaRemoteTree(t *testing.T) {
	o := optionalSource(t, filepath.Join(t.TempDir(), "optional-skills"), "software-development/ast-grep")
	liveRepoGitHub(t, o, []string{"software-development/ast-grep"})
	b, err := o.Fetch(context.Background(), "official/ast-grep")
	if err != nil || b == nil || b.Identifier != "official/software-development/ast-grep" {
		t.Fatalf("%v %v", b, err)
	}
}

func TestOptionalFetchAmbiguousBareNameRefuses(t *testing.T) {
	o := optionalSource(t, filepath.Join(t.TempDir(), "optional-skills"), "security/scanner", "devops/scanner")
	untouchableGitHub(t, o)
	if b, err := o.Fetch(context.Background(), "official/scanner"); b != nil || err != nil {
		t.Fatalf("%v %v", b, err)
	}
}

func TestOptionalLocalCheckoutWinsOverRemote(t *testing.T) {
	root := filepath.Join(t.TempDir(), "optional-skills")
	writeTestFile(t, filepath.Join(root, "research", "local-skill", "SKILL.md"), "---\nname: local-skill\ndescription: local\n---\nBody")
	o := optionalSource(t, root, "research/local-skill")
	untouchableGitHub(t, o)
	if b, err := o.Fetch(context.Background(), "official/research/local-skill"); b == nil || err != nil {
		t.Fatalf("%v %v", b, err)
	}
}

func TestOptionalFallbackRejectsTraversalRel(t *testing.T) {
	o := optionalSource(t, filepath.Join(t.TempDir(), "optional-skills"), "security/whatever")
	untouchableGitHub(t, o)
	if b := o.fetchFromLiveRepo(context.Background(), "../../etc/passwd"); b != nil {
		t.Fatalf("%v", b)
	}
}

func TestOptionalSearchSurfacesRemoteOnlySkills(t *testing.T) {
	o := optionalSource(t, filepath.Join(t.TempDir(), "optional-skills"), "software-development/ast-grep")
	results, err := o.Search(context.Background(), "ast-grep", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(results, func(r SkillMeta) bool {
		return r.Identifier == "official/software-development/ast-grep" && r.TrustLevel == "builtin"
	}) {
		t.Fatalf("%+v", results)
	}
}

func TestOptionalInspectSurfacesRemoteOnlySkill(t *testing.T) {
	o := optionalSource(t, filepath.Join(t.TempDir(), "optional-skills"), "software-development/ast-grep")
	meta, err := o.Inspect(context.Background(), "official/software-development/ast-grep")
	if err != nil || meta == nil || *meta.Repo != "NousResearch/hermes-agent" || *meta.Path != "optional-skills/software-development/ast-grep" {
		t.Fatalf("%+v %v", meta, err)
	}
}

func TestOptionalOfflineDegradesToLocalOnly(t *testing.T) {
	o := optionalSource(t, filepath.Join(t.TempDir(), "optional-skills"))
	o.remoteDirs = pyjson.NewObject()
	untouchableGitHub(t, o)
	if b, err := o.Fetch(context.Background(), "official/never-heard-of-it"); b != nil || err != nil {
		t.Fatalf("%v %v", b, err)
	}
	if r, err := o.Search(context.Background(), "never-heard-of-it", 10); len(r) != 0 || err != nil {
		t.Fatalf("%v %v", r, err)
	}
}

func TestBundledOptionalSourceStillIncludesSupportFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "optional-skills")
	writeTestFile(t, filepath.Join(root, "category", "official-demo", "SKILL.md"), "---\nname: official-demo\ndescription: demo\n---\n")
	writeTestFile(t, filepath.Join(root, "category", "official-demo", "references", "all.md"), "all")
	o := optionalSource(t, root)
	b, err := o.Fetch(context.Background(), "official/category/official-demo")
	if err != nil || b == nil {
		t.Fatalf("%v %v", b, err)
	}
	paths := slices.Sorted(func(yield func(string) bool) {
		for p := range bundlePaths(b) {
			if !yield(p) {
				return
			}
		}
	})
	if !slices.Equal(paths, []string{"SKILL.md", "references/all.md"}) {
		t.Fatalf("paths %v", paths)
	}
}

func TestOptionalSourceUpstreamPointerFetchesFromExternalRepo(t *testing.T) {
	root := filepath.Join(t.TempDir(), "optional-skills")
	writeTestFile(t, filepath.Join(root, "creative", "upstream-demo", "SKILL.md"), "---\nname: upstream-demo\n"+
		"description: Upstream-maintained catalog entry.\nmetadata:\n  hermes:\n    upstream:\n      repo: acme/design-skill\n"+
		"      path: .hermes/skills/design\n---\n# Stub\n")
	o := optionalSource(t, root)
	var fetched []string
	o.github = NewGitHubSource(o.h, o.auth, nil)
	o.github.seams.fetch = func(id string) *SkillBundle {
		fetched = append(fetched, id)
		return &SkillBundle{Name: "design", Files: []BundleFile{
			{Path: "SKILL.md", Data: []byte("---\nname: design\ndescription: real\n---\nreal body")},
			{Path: "reference/audit.md", Data: []byte("audit")}},
			Source: "github", Identifier: id, TrustLevel: "community", Metadata: objectOf("source_url", "https://github.com/acme/design-skill")}
	}
	b, err := o.Fetch(context.Background(), "official/creative/upstream-demo")
	if err != nil || b == nil || !slices.Equal(fetched, []string{"acme/design-skill/.hermes/skills/design"}) {
		t.Fatalf("%v %v %v", b, err, fetched)
	}
	md, _ := b.File("SKILL.md")
	repo, _ := b.Metadata.Get("upstream_repo")
	if b.Source != "official" || b.Identifier != "official/creative/upstream-demo" || b.TrustLevel != "trusted" ||
		!strings.HasPrefix(string(md), "---\nname: design") || repo != "acme/design-skill" {
		t.Fatalf("%+v", b)
	}
}

func TestOptionalSourceUpstreamPointerRejectsMalformed(t *testing.T) {
	for _, content := range []string{
		"---\nname: x\nmetadata:\n  hermes:\n    upstream:\n      repo: acme\n      path: skills/x\n---\n",
		"---\nname: x\nmetadata:\n  hermes:\n    upstream:\n      repo: a/b/c\n      path: skills/x\n---\n",
		"---\nname: x\nmetadata:\n  hermes:\n    upstream:\n      repo: acme/skill\n      path: ../../etc\n---\n",
		"---\nname: x\nmetadata:\n  hermes:\n    upstream:\n      repo: acme/skill\n---\n",
		"---\nname: x\n---\n",
	} {
		if repo, p, ok := upstreamPointerFromContent([]byte(content)); ok {
			t.Fatalf("%q accepted as %q %q", content, repo, p)
		}
	}
}
