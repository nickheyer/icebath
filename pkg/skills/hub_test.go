package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Hub whose skills dir is tmp/skills, the patched SKILLS_DIR of the hermes tests
func newTestHub(t *testing.T) (*Hub, string) {
	t.Helper()
	tmp := t.TempDir()
	s := NewScope(filepath.Join(tmp, "home"), nil)
	s.SkillsDirOverride = filepath.Join(tmp, "skills")
	return NewHub(s), tmp
}

func writeTestFile(t *testing.T, p, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Source returning a fixed bundle and counting fetches
type fakeHubSource struct {
	SourceBase
	bundle  *SkillBundle
	fetches int
}

func (f *fakeHubSource) Search(context.Context, string, int) ([]SkillMeta, error) { return nil, nil }
func (f *fakeHubSource) Inspect(context.Context, string) (*SkillMeta, error)      { return nil, nil }
func (f *fakeHubSource) Fetch(context.Context, string) (*SkillBundle, error) {
	f.fetches++
	return f.bundle, nil
}

func textBundle(name string, files ...string) *SkillBundle {
	b := &SkillBundle{Name: name, Source: "github", Identifier: "owner/repo/" + name, TrustLevel: "community"}
	for i := 0; i+1 < len(files); i += 2 {
		b.Files = append(b.Files, BundleFile{Path: files[i], Data: []byte(files[i+1])})
	}
	return b
}

func TestHubLockFileLoadCorruptJSON(t *testing.T) {
	h, tmp := newTestHub(t)
	writeTestFile(t, filepath.Join(tmp, "lock.json"), "not json{{{")
	data, err := h.LockFile(filepath.Join(tmp, "lock.json")).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := pyjson.Dumps(data); got != `{"version": 1, "installed": {}}` {
		t.Fatalf("load gave %s", got)
	}
}

func TestHubLockFileListInstalled(t *testing.T) {
	h, tmp := newTestHub(t)
	lock := h.LockFile(filepath.Join(tmp, "lock.json"))
	for _, n := range []string{"s1", "s2"} {
		if err := lock.RecordInstall(n, "github", "x", "trusted", "pass", "h1", n, []string{"SKILL.md"}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	installed, err := lock.ListInstalled()
	if err != nil || len(installed) != 2 {
		t.Fatalf("list gave %v %v", installed, err)
	}
	for i, n := range []string{"s1", "s2"} {
		if v, _ := installed[i].Get("name"); v != n || installed[i].Keys[0] != "name" {
			t.Fatalf("entry %d is %v", i, installed[i].Keys)
		}
	}
}

func TestTapsManagerLoadCorruptJSONAndRemoveExistingTap(t *testing.T) {
	h, tmp := newTestHub(t)
	writeTestFile(t, filepath.Join(tmp, "bad.json"), "bad json")
	if taps, err := h.Taps(filepath.Join(tmp, "bad.json")).Load(); err != nil || len(taps) != 0 {
		t.Fatalf("corrupt load gave %v %v", taps, err)
	}
	mgr := h.Taps(filepath.Join(tmp, "taps.json"))
	if added, err := mgr.Add("owner/repo", "skills/"); !added || err != nil {
		t.Fatalf("add gave %v %v", added, err)
	}
	if added, _ := mgr.Add("owner/repo", "skills/"); added {
		t.Fatal("second add of the same repo reported added")
	}
	if removed, err := mgr.Remove("owner/repo"); !removed || err != nil {
		t.Fatalf("remove gave %v %v", removed, err)
	}
	if taps, _ := mgr.Load(); len(taps) != 0 {
		t.Fatalf("taps left %v", taps)
	}
}

func TestAppendAuditLogCreatesLogEntry(t *testing.T) {
	h, tmp := newTestHub(t)
	h.AuditLogOverride = filepath.Join(tmp, "audit.log")
	if err := h.AppendAuditLog("INSTALL", "test-skill", "github", "trusted", "pass", ""); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(h.AuditLogOverride)
	if !strings.Contains(string(raw), "INSTALL") || !strings.Contains(string(raw), "test-skill github:trusted pass\n") {
		t.Fatalf("audit log is %q", raw)
	}
}

func TestQuarantineBundleWritesBinaryFilesAndHashesLikeTheBundle(t *testing.T) {
	h, _ := newTestHub(t)
	b := &SkillBundle{Name: "neutts", Source: "official", Identifier: "official/mlops/models/neutts", TrustLevel: "builtin",
		Files: []BundleFile{{Path: "SKILL.md", Data: []byte("---\nname: neutts\n---\n\nBody line one.\nBody line two.\n")},
			{Path: "assets/neutts-cli/samples/jo.wav", Data: []byte("RIFF\x00\x01fakewav"), Binary: true}}}
	q, err := h.QuarantineBundle(b)
	if err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(filepath.Join(q, "SKILL.md"))
	wav, _ := os.ReadFile(filepath.Join(q, "assets", "neutts-cli", "samples", "jo.wav"))
	if !strings.HasPrefix(string(md), "---") || strings.Contains(string(md), "\r\n") || string(wav) != "RIFF\x00\x01fakewav" {
		t.Fatalf("quarantined %q %q", md, wav)
	}
	if want, _ := bundleContentHash(b); ContentHash(q) != want {
		t.Fatalf("content hash %s, bundle hash %s", ContentHash(q), want)
	}
}

func TestQuarantineBundleRejectsUnsafeFilePaths(t *testing.T) {
	h, tmp := newTestHub(t)
	outside := filepath.Join(tmp, "outside.txt")
	for _, bad := range []string{"../../../escape.txt", outside, "scripts/helper.py:payload"} {
		b := textBundle("demo", "SKILL.md", "---\nname: demo\n---\n", bad, "owned")
		if _, err := h.QuarantineBundle(b); err == nil || !strings.Contains(err.Error(), "Unsafe bundle file path") {
			t.Fatalf("%s gave %v", bad, err)
		}
	}
	for _, p := range []string{filepath.Join(tmp, "skills", "escape.txt"), outside, filepath.Join(tmp, "skills", "scripts")} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s was written", p)
		}
	}
}

func TestBundleContentHashMatchesInstalledContentHash(t *testing.T) {
	b := textBundle("demo-skill", "SKILL.md", "same content", "references/checklist.md", "- [ ] security\n")
	dir := filepath.Join(t.TempDir(), "demo-skill")
	writeTestFile(t, filepath.Join(dir, "SKILL.md"), "same content")
	writeTestFile(t, filepath.Join(dir, "references", "checklist.md"), "- [ ] security\n")
	if got, _ := bundleContentHash(b); got != ContentHash(dir) {
		t.Fatalf("bundle hash %s, content hash %s", got, ContentHash(dir))
	}
	b.Files[1].Path = `references\checklist.md`
	if got, _ := bundleContentHash(b); got != ContentHash(dir) {
		t.Fatal("backslash keys hash differently")
	}
}

// Lock file holding one demo-skill entry with the given install path value
func writeDemoLock(t *testing.T, path string, installPath any, extra map[string]any) {
	t.Helper()
	e := pyjson.NewObject()
	for _, kv := range [][2]any{{"source", "github"}, {"identifier", "owner/repo/demo-skill"}, {"content_hash", "oldhash"},
		{"install_path", installPath}, {"trust_level", "trusted"}} {
		e.Set(kv[0].(string), kv[1])
	}
	for k, v := range extra {
		e.Set(k, v)
	}
	inst := pyjson.NewObject()
	inst.Set("demo-skill", e)
	doc := pyjson.NewObject()
	doc.Set("installed", inst)
	text, _ := pyjson.Dumps(doc)
	writeTestFile(t, path, text)
}

func TestCheckForSkillUpdatesReportsUpdateWhenRemoteHashDiffers(t *testing.T) {
	h, tmp := newTestHub(t)
	os.MkdirAll(filepath.Join(tmp, "skills", "demo-skill"), 0o755)
	writeDemoLock(t, filepath.Join(tmp, "lock.json"), "demo-skill", nil)
	src := &fakeHubSource{SourceBase: SourceBase{ID: "github"}, bundle: textBundle("demo-skill", "SKILL.md", "new content")}
	results, err := h.CheckForSkillUpdates(context.Background(), "", h.LockFile(filepath.Join(tmp, "lock.json")), []SkillSource{src}, nil)
	if err != nil || len(results) != 1 || results[0].Name != "demo-skill" || results[0].Status != "update_available" || results[0].Bundle == nil {
		t.Fatalf("results %+v %v", results, err)
	}
}

func TestCheckForSkillUpdatesUnusableEntryReportedWithoutRemoteFetch(t *testing.T) {
	for _, kind := range []string{"missing", "regular_file", "unsafe", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			h, tmp := newTestHub(t)
			os.MkdirAll(filepath.Join(tmp, "skills"), 0o755)
			if kind == "regular_file" {
				writeTestFile(t, filepath.Join(tmp, "skills", "demo-skill"), "not an installed directory")
			}
			ip := map[string]any{"unsafe": "../outside", "corrupt": []any{"bad"}}[kind]
			if ip == nil {
				ip = "demo-skill"
			}
			writeDemoLock(t, filepath.Join(tmp, "lock.json"), ip, nil)
			src := &fakeHubSource{SourceBase: SourceBase{ID: "github"}}
			results, err := h.CheckForSkillUpdates(context.Background(), "", h.LockFile(filepath.Join(tmp, "lock.json")), []SkillSource{src}, nil)
			want := "orphaned"
			if kind == "unsafe" || kind == "corrupt" {
				want = "invalid_install"
			}
			if err != nil || len(results) != 1 || results[0].Status != want || results[0].Bundle != nil || src.fetches != 0 {
				t.Fatalf("results %+v %v fetches %d", results, err, src.fetches)
			}
		})
	}
}

func TestRecordInstallRejectsUnsafePaths(t *testing.T) {
	h, tmp := newTestHub(t)
	lock := h.LockFile(filepath.Join(tmp, "lock.json"))
	for _, bad := range []string{"", ".", "..", "../../etc/passwd", "/etc/passwd", "skills/../../tmp", "C:/Windows/System32"} {
		err := lock.RecordInstall("evil", "github", "x", "trusted", "pass", "h1", bad, []string{"SKILL.md"}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "Unsafe") {
			t.Fatalf("%q gave %v", bad, err)
		}
	}
}

func TestUninstallRejectsPoisonedTraversalAndEmptyInstallPaths(t *testing.T) {
	for _, kind := range []string{"absolute", "traversal", "empty"} {
		t.Run(kind, func(t *testing.T) {
			h, tmp := newTestHub(t)
			skills := filepath.Join(tmp, "skills")
			writeTestFile(t, filepath.Join(skills, "bystander", "SKILL.md"), "safe")
			victim := filepath.Join(tmp, "victim")
			writeTestFile(t, filepath.Join(victim, "file.txt"), "important")
			writeTestFile(t, filepath.Join(tmp, "sibling", "data"), "nope")
			ip := map[string]string{"absolute": victim, "traversal": "../sibling", "empty": ""}[kind]
			h.LockFileOverride = filepath.Join(tmp, "lock.json")
			writeDemoLock(t, h.LockFileOverride, ip, nil)
			ok, msg, err := h.UninstallSkill("demo-skill")
			if err != nil || ok || !(strings.Contains(msg, "Unsafe") || strings.Contains(msg, "Refusing")) {
				t.Fatalf("uninstall gave %v %q %v", ok, msg, err)
			}
			for _, p := range []string{filepath.Join(victim, "file.txt"), filepath.Join(tmp, "sibling", "data"), filepath.Join(skills, "bystander", "SKILL.md")} {
				if _, err := os.Stat(p); err != nil {
					t.Fatalf("%s was removed", p)
				}
			}
		})
	}
}

// Quarantine dir holding a pending SKILL.md for name under the hub of h
func pendingQuarantine(t *testing.T, h *Hub, name string) string {
	q := filepath.Join(h.QuarantineDir(), "pending")
	writeTestFile(t, filepath.Join(q, "SKILL.md"), "---\nname: "+name+"\n---\n")
	return q
}

func TestInstallFromQuarantineRefusals(t *testing.T) {
	cases := []struct {
		name, skill, category, want string
		setup                       func(t *testing.T, skills string)
		intact                      []string
	}{
		{"category_bucket", "research", "", "Refusing to overwrite category directory", func(t *testing.T, skills string) {
			for _, s := range []string{"alpha", "bravo", "charlie"} {
				writeTestFile(t, filepath.Join(skills, "research", s, "SKILL.md"), "name: "+s)
			}
		}, []string{"research/alpha/SKILL.md", "research/bravo/SKILL.md", "research/charlie/SKILL.md"}},
		{"nested_only_category", "mlops", "", "category directory", func(t *testing.T, skills string) {
			writeTestFile(t, filepath.Join(skills, "mlops", "training", "trl", "SKILL.md"), "---\nname: trl\n---\n")
			writeTestFile(t, filepath.Join(skills, "mlops", "inference", "vllm", "SKILL.md"), "---\nname: vllm\n---\n")
		}, []string{"mlops/training/trl/SKILL.md", "mlops/inference/vllm/SKILL.md"}},
		{"category_inside_skill", "docker", "devops", "existing skill directory", func(t *testing.T, skills string) {
			writeTestFile(t, filepath.Join(skills, "devops", "SKILL.md"), "---\nname: devops\n---\n")
		}, []string{"devops/SKILL.md"}},
		{"regular_file_collision", "notes", "", "not a directory", func(t *testing.T, skills string) {
			writeTestFile(t, filepath.Join(skills, "notes"), "not a directory")
		}, []string{"notes"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, tmp := newTestHub(t)
			skills := filepath.Join(tmp, "skills")
			c.setup(t, skills)
			q := pendingQuarantine(t, h, c.skill)
			b := textBundle(c.skill, "SKILL.md", "---\nname: "+c.skill+"\n---\n")
			_, err := h.InstallFromQuarantine(context.Background(), q, c.skill, c.category, b, ScanResult{Verdict: "safe"}, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("install gave %v", err)
			}
			for _, p := range c.intact {
				if _, err := os.Stat(filepath.Join(skills, p)); err != nil {
					t.Fatalf("%s was destroyed", p)
				}
			}
			if _, err := os.Stat(filepath.Join(skills, "devops", "docker")); err == nil {
				t.Fatal("nested install dir was created")
			}
		})
	}
}

func TestInstallFromQuarantineRejectsSymlinks(t *testing.T) {
	h, tmp := newTestHub(t)
	q := pendingQuarantine(t, h, "bad-skill")
	writeTestFile(t, filepath.Join(tmp, "secret.txt"), "data exfiltration payload\n")
	if err := os.Symlink(filepath.Join(tmp, "secret.txt"), filepath.Join(q, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	b := textBundle("bad-skill", "SKILL.md", "---\nname: bad-skill\n---\n")
	_, err := h.InstallFromQuarantine(context.Background(), q, "bad-skill", "", b, ScanResult{Verdict: "safe"}, nil)
	if err == nil || !strings.Contains(err.Error(), "symlinks") {
		t.Fatalf("install gave %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "skills", "bad-skill")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("skill dir was created")
	}
}

func TestInstallFromQuarantineAllowsExistingSkillAndEmptyCategoryDir(t *testing.T) {
	h, tmp := newTestHub(t)
	skills := filepath.Join(tmp, "skills")
	writeTestFile(t, filepath.Join(skills, "my-skill", "SKILL.md"), "old content")
	writeTestFile(t, filepath.Join(skills, "my-skill", "refs", "guide.md"), "old guide")
	q := filepath.Join(h.QuarantineDir(), "pending")
	writeTestFile(t, filepath.Join(q, "SKILL.md"), "---\nname: my-skill\n---\nnew")
	writeTestFile(t, filepath.Join(q, "refs", "guide.md"), "new guide")
	b := textBundle("my-skill", "SKILL.md", "---\nname: my-skill\n---\nnew")
	installed, err := h.InstallFromQuarantine(context.Background(), q, "my-skill", "", b, ScanResult{Verdict: "safe"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(installed, "SKILL.md")); string(raw) != "---\nname: my-skill\n---\nnew" {
		t.Fatalf("installed SKILL.md %q", raw)
	}
	os.MkdirAll(filepath.Join(skills, "research"), 0o755)
	q = pendingQuarantine(t, h, "research")
	installed, err = h.InstallFromQuarantine(context.Background(), q, "research", "", textBundle("research", "SKILL.md", "x"), ScanResult{Verdict: "safe"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(installed, "SKILL.md")); err != nil {
		t.Fatal("empty category install has no SKILL.md")
	}
}

func TestInstallFromQuarantineRecordsSuccessfulInstall(t *testing.T) {
	h, tmp := newTestHub(t)
	skills := filepath.Join(tmp, "skills")
	md := "---\nname: good-skill\n---\n\n# Good skill\n"
	q := filepath.Join(h.QuarantineDir(), "pending")
	writeTestFile(t, filepath.Join(q, "SKILL.md"), md)
	b := &SkillBundle{Name: "good-skill", Files: []BundleFile{{Path: "SKILL.md", Data: []byte(md)}}, Source: "community",
		Identifier: "good/source", TrustLevel: "community"}
	installed, err := h.InstallFromQuarantine(context.Background(), q, "good-skill", "", b,
		ScanResult{SkillName: "good-skill", Source: "community", TrustLevel: "community", Verdict: "safe"}, nil)
	if err != nil || installed != resolvePath(filepath.Join(skills, "good-skill")) || !isDir(installed) {
		t.Fatalf("installed %q %v", installed, err)
	}
	rec, err := h.S.GetRecord("good-skill")
	if err != nil || rec == nil {
		t.Fatalf("usage record %v %v", rec, err)
	}
	if v, _ := rec.Get("created_by"); v != "installed" {
		t.Fatalf("usage record created_by %v", v)
	}
	entry, _ := h.LockFile("").GetInstalled("good-skill")
	if o, ok := entry.(*pyjson.Object); !ok || o.Vals["install_path"] != "good-skill" || o.Vals["scan_verdict"] != "safe" {
		t.Fatalf("lock entry %v", entry)
	}
}

func TestInstalledEntryExportMatchesDirectAndClawHubAliasKeys(t *testing.T) {
	direct := pyjson.NewObject()
	direct.Set("name", "direct")
	aliased := pyjson.NewObject()
	aliased.Set("name", "aliased")
	alias, ok := clawhubCatalogIdentifier("@alice/weather")
	if !ok {
		t.Fatal("expected a clawhub catalog identifier for @alice/weather")
	}
	installed := map[string]*pyjson.Object{"github/x/y": direct, alias: aliased}
	if got := InstalledEntry(installed, "github/x/y", "github"); got != direct {
		t.Fatalf("direct key: got %v", got)
	}
	if got := InstalledEntry(installed, "@alice/weather", "clawhub"); got != aliased {
		t.Fatalf("clawhub alias: got %v", got)
	}
	if got := InstalledEntry(installed, "@alice/weather", "github"); got != nil {
		t.Fatalf("non clawhub miss must be nil, got %v", got)
	}
	if got := InstalledEntry(installed, "nobody/nothing", "clawhub"); got != nil {
		t.Fatalf("clawhub miss must be nil, got %v", got)
	}
}
