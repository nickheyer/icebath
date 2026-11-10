package skills

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Bundled tree fixture with one categorized skill, one top level skill and a category description
func setupBundled(t *testing.T) (bundled string, s *Scope) {
	t.Helper()
	root := t.TempDir()
	bundled = filepath.Join(root, "bundled_skills")
	for p, body := range map[string]string{
		"category/new-skill/SKILL.md": "# New", "category/new-skill/main.py": "print(1)",
		"category/DESCRIPTION.md": "Category desc", "old-skill/SKILL.md": "# Old",
	} {
		full := filepath.Join(bundled, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(body), 0o644)
	}
	t.Setenv("ICEBATH_BUNDLED_SKILLS", bundled)
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", filepath.Join(root, "optional-skills"))
	h := filepath.Join(root, "home")
	os.MkdirAll(h, 0o755)
	s = NewScope(h, nil)
	s.SkillsDirOverride = filepath.Join(root, "user_skills")
	return bundled, s
}

func mustSync(t *testing.T, s *Scope) map[string]any {
	t.Helper()
	r, err := s.SyncSkills(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func mustManifest(t *testing.T, s *Scope) map[string]string {
	t.Helper()
	m, err := s.readManifest()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func config0() map[string]any { return map[string]any{} }

func TestSyncUpdatesPristineAndKeepsUserModified(t *testing.T) {
	bundled, s := setupBundled(t)
	mustSync(t, s)
	os.WriteFile(filepath.Join(bundled, "old-skill", "SKILL.md"), []byte("# Old v2"), 0o644)
	os.WriteFile(filepath.Join(bundled, "category", "new-skill", "SKILL.md"), []byte("# New v2"), 0o644)
	os.WriteFile(filepath.Join(s.SkillsDir(), "category", "new-skill", "SKILL.md"), []byte("# mine"), 0o644)
	r := mustSync(t, s)
	if !reflect.DeepEqual(r["updated"], []string{"old-skill"}) || !reflect.DeepEqual(r["user_modified"], []string{"new-skill"}) {
		t.Fatal(r)
	}
	if data, _ := os.ReadFile(filepath.Join(s.SkillsDir(), "old-skill", "SKILL.md")); string(data) != "# Old v2" {
		t.Fatal(string(data))
	}
	mods, err := s.ListUserModifiedBundledSkills()
	if err != nil || len(mods) != 1 || mods[0]["name"] != "new-skill" {
		t.Fatal(mods, err)
	}
	d, _ := s.DiffBundledSkill("new-skill")
	diffs := d["diffs"].([]map[string]any)
	if d["modified"] != true || len(diffs) != 1 || diffs[0]["status"] != "modified" || !strings.Contains(diffs[0]["diff"].(string), "--- stock/SKILL.md\n+++ yours/SKILL.md\n") {
		t.Fatal(d)
	}
	if d, _ := s.DiffBundledSkill("nope"); d["found"] != false || d["ok"] != false {
		t.Fatal(d)
	}
}

func TestOptOutSeedsEssentialSkillsOnlyAndRemovesPristine(t *testing.T) {
	bundled, s := setupBundled(t)
	os.MkdirAll(filepath.Join(bundled, "autonomous-ai-agents", "icebath-agent"), 0o755)
	os.WriteFile(filepath.Join(bundled, "autonomous-ai-agents", "icebath-agent", "SKILL.md"), []byte("---\nname: icebath-agent\n---\n"), 0o644)
	if r := s.SetBundledSkillsOptOut(true); r["changed"] != true || !strings.HasPrefix(r["message"].(string), "Opted out") {
		t.Fatal(r)
	}
	if r := s.SetBundledSkillsOptOut(true); r["changed"] != false {
		t.Fatal(r)
	}
	r := mustSync(t, s)
	if r["skipped_opt_out"] != true || !reflect.DeepEqual(r["copied"], []string{"icebath-agent"}) || r["total_bundled"] != 1 {
		t.Fatal(r)
	}
	if r := s.SetBundledSkillsOptOut(false); r["changed"] != true || lexists(filepath.Join(s.Home, NoBundledSkillsMarker)) {
		t.Fatal(r)
	}
	if r := s.SetBundledSkillsOptOut(false); r["message"] != "Not opted out — no marker to remove." {
		t.Fatal(r)
	}
	mustSync(t, s)
	os.WriteFile(filepath.Join(s.SkillsDir(), "old-skill", "SKILL.md"), []byte("# mine"), 0o644)
	dry, _ := s.RemovePristineBundledSkills(context.Background(), true)
	if !reflect.DeepEqual(dry["removed"], []string{"icebath-agent", "new-skill"}) || !isDir(filepath.Join(s.SkillsDir(), "category", "new-skill")) {
		t.Fatal(dry)
	}
	res, _ := s.RemovePristineBundledSkills(context.Background(), false)
	skipped := res["skipped"].([]map[string]any)
	if !reflect.DeepEqual(res["removed"], []string{"icebath-agent", "new-skill"}) || len(skipped) != 1 || skipped[0]["reason"] != "user-modified (kept)" {
		t.Fatal(res)
	}
	if lexists(filepath.Join(s.SkillsDir(), "category", "new-skill")) || !isDir(filepath.Join(s.SkillsDir(), "old-skill")) {
		t.Fatal("pristine removal touched the wrong skill")
	}
}

func TestEmbeddedExecPathsMatchSourceTree(t *testing.T) {
	found := map[string]bool{}
	for _, top := range []string{"bundled", "optional"} {
		filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if st, _ := d.Info(); st.Mode().Perm()&0o100 != 0 {
					found[filepath.ToSlash(p)] = true
				}
			}
			return nil
		})
	}
	if !reflect.DeepEqual(found, embeddedExecPaths) {
		t.Fatal("embed_modes.go is stale against the source tree")
	}
}

func TestSyncSkillsSeedsEmbeddedTreeIntoContextHome(t *testing.T) {
	t.Setenv("ICEBATH_BUNDLED_SKILLS", "")
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", "")
	h := t.TempDir()
	var out bytes.Buffer
	prev := syncOut
	syncOut = &out
	t.Cleanup(func() { syncOut = prev })
	r, err := SyncSkills(homeCtx(h), false)
	if err != nil || len(r["copied"].([]string)) == 0 || r["total_bundled"] != len(r["copied"].([]string)) {
		t.Fatal(r, err)
	}
	if !strings.Contains(out.String(), "  + ") {
		t.Fatal(out.String())
	}
	for p := range embeddedExecPaths {
		rel, ok := strings.CutPrefix(p, "bundled/")
		if !ok {
			continue
		}
		if st, err := os.Stat(filepath.Join(h, "skills", filepath.FromSlash(rel))); err == nil && st.Mode().Perm()&0o100 == 0 {
			t.Fatal("executable bit lost", rel)
		}
	}
	again, _ := SyncSkills(homeCtx(h), true)
	if len(again["copied"].([]string)) != 0 || again["skipped"] != r["total_bundled"] {
		t.Fatal(again)
	}
}

func homeCtx(h string) context.Context { return home.WithOverride(context.Background(), h) }

func TestSyncSkillsMainPrintsSummaryWithUserModifiedOverflow(t *testing.T) {
	tmp := t.TempDir()
	bundled := filepath.Join(tmp, "bundled")
	h := filepath.Join(tmp, "home")
	for i := 1; i <= 7; i++ {
		writeRel(t, bundled, "s"+itoa(i)+"/SKILL.md", "# v1")
	}
	t.Setenv("ICEBATH_BUNDLED_SKILLS", bundled)
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", filepath.Join(tmp, "optional"))
	var out bytes.Buffer
	prev := syncOut
	syncOut = &out
	t.Cleanup(func() { syncOut = prev })
	if err := SyncSkillsMain(homeCtx(h)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "Syncing bundled skills into ~/.icebath/skills/ ...\n") || !strings.HasSuffix(out.String(), "\nDone: 7 new, 0 updated, 0 unchanged. 7 total bundled.\n") {
		t.Fatalf("%q", out.String())
	}
	for i := 1; i <= 7; i++ {
		writeRel(t, bundled, "s"+itoa(i)+"/SKILL.md", "# v2")
		writeRel(t, filepath.Join(h, "skills"), "s"+itoa(i)+"/SKILL.md", "# mine")
	}
	writeRel(t, filepath.Join(h, "skills"), ".bundled_manifest", "s1:x\ns2:x\ns3:x\ns4:x\ns5:x\ns6:x\ns7:x\ngone:x\n")
	out.Reset()
	if err := SyncSkillsMain(homeCtx(h)); err != nil {
		t.Fatal(err)
	}
	want := "\nDone: 0 new, 0 updated, 0 unchanged, 7 user-modified (kept): s1, s2, s3, s4, s5, +2 more, 1 cleaned from manifest. 7 total bundled.\n"
	if !strings.HasSuffix(out.String(), want) {
		t.Fatalf("%q", out.String())
	}
}
