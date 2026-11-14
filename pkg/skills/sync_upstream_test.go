package skills

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
)

// Writes text at root/rel creating parent directories and returns the full path
func writeRel(t *testing.T, root, rel, text string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// Scope whose skills tree is skillsDir with the bundled tree at bundled and optional beside it
func patchedSync(t *testing.T, bundled, skillsDir string) *Scope {
	t.Helper()
	t.Setenv("ICEBATH_BUNDLED_SKILLS", bundled)
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", filepath.Join(filepath.Dir(bundled), "optional-skills"))
	h := filepath.Join(filepath.Dir(bundled), "home")
	os.MkdirAll(h, 0o755)
	s := NewScope(h, nil)
	s.SkillsDirOverride = skillsDir
	return s
}

// String list stored under key in a sync result, nil when absent
func strs(r map[string]any, key string) []string {
	v, _ := r[key].([]string)
	return v
}

// Reads a file or fails the test
func mustRead(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// OSError raised by a patched copytree
func simulatedCopyFailure(dest, msg string) error {
	return &fs.PathError{Op: "copytree", Path: dest, Err: fmt.Errorf("%s", msg)}
}

// Swaps the copytree seam for the duration of the test
func withCopyTree(t *testing.T, fn func(t skillTree, dir, dest string, dirsExistOK, keepExisting bool) error) func() {
	prev := syncSteps.copyTree
	syncSteps.copyTree = fn
	restore := func() { syncSteps.copyTree = prev }
	t.Cleanup(restore)
	return restore
}

func TestReadWriteManifestRoundtripV2(t *testing.T) {
	tmp := t.TempDir()
	s := NewScope(tmp, config0())
	s.SkillsDirOverride = tmp
	entries := map[string]string{"zebra": "hash1", "alpha": "hash2", "middle": "hash3"}
	s.writeManifest(context.Background(), entries)
	if got := mustManifest(t, s); !reflect.DeepEqual(got, entries) {
		t.Fatal(got)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(mustRead(t, s.manifestFile())), "\n") {
		names = append(names, strings.Split(line, ":")[0])
	}
	if !reflect.DeepEqual(names, []string{"alpha", "middle", "zebra"}) {
		t.Fatal(names)
	}
	s.SkillsDirOverride = filepath.Join(tmp, "nonexistent")
	if got := mustManifest(t, s); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestReadWriteManifestReadsV1LinesBlanksAndMixedFormats(t *testing.T) {
	tmp := t.TempDir()
	s := NewScope(tmp, config0())
	s.SkillsDirOverride = tmp
	os.WriteFile(s.manifestFile(), []byte("old-skill\n\n  \nnew-skill:abc123\n"), 0o644)
	if got := mustManifest(t, s); !reflect.DeepEqual(got, map[string]string{"old-skill": "", "new-skill": "abc123"}) {
		t.Fatal(got)
	}
	os.WriteFile(s.manifestFile(), []byte("\xEF\xBB\xBFold-skill\n  spaced : h1 \n"), 0o644)
	if got := mustManifest(t, s); !reflect.DeepEqual(got, map[string]string{"old-skill": "", "spaced": "h1"}) {
		t.Fatal(got)
	}
	os.WriteFile(s.manifestFile(), []byte("a\x80b\n"), 0o644)
	if _, err := s.readManifest(); err == nil || !strings.Contains(err.Error(), "codec can't decode byte 0x80") {
		t.Fatal(err)
	}
}

func TestReadWriteManifestPreservesExistingFileMode(t *testing.T) {
	tmp := t.TempDir()
	s := NewScope(tmp, config0())
	s.SkillsDirOverride = tmp
	os.WriteFile(s.manifestFile(), []byte("old-skill:oldhash\n"), 0o644)
	os.Chmod(s.manifestFile(), 0o660)
	s.writeManifest(context.Background(), map[string]string{"new-skill": "newhash"})
	if got := mustRead(t, s.manifestFile()); got != "new-skill:newhash\n" {
		t.Fatalf("%q", got)
	}
	if st, _ := os.Stat(s.manifestFile()); st.Mode().Perm() != 0o660 {
		t.Fatal(st.Mode())
	}
}

func TestDirHashReflectsContentOnly(t *testing.T) {
	tmp := t.TempDir()
	a, b := filepath.Join(tmp, "a"), filepath.Join(tmp, "b")
	for _, d := range []string{a, b} {
		writeRel(t, d, "SKILL.md", "# Test")
		writeRel(t, d, "main.py", "print(1)")
	}
	if DirHash(a) != DirHash(b) {
		t.Fatal("same content hashed differently")
	}
	writeRel(t, b, "SKILL.md", "# Version 2")
	if DirHash(a) == DirHash(b) {
		t.Fatal("content change kept the hash")
	}
	empty := filepath.Join(tmp, "empty")
	os.Mkdir(empty, 0o755)
	if len(DirHash(empty)) != 32 || len(DirHash(filepath.Join(tmp, "nope"))) != 32 {
		t.Fatal(DirHash(empty))
	}
	writeRel(t, a, "tool.py", "x=1")
	withPy := DirHash(a)
	writeRel(t, a, "tool.pyc", "bytecode")
	if DirHash(a) != withPy {
		t.Fatal("pyc beside its source changed the hash")
	}
	writeRel(t, a, "shipped.pyc", "no source")
	if DirHash(a) == withPy {
		t.Fatal("source-less pyc must count")
	}
	os.Remove(filepath.Join(a, "shipped.pyc"))
	if !matchesOriginHash(a, dirHashFS(os.DirFS(a), ".", true), "") || matchesOriginHash(a, "", "") {
		t.Fatal("legacy origin hash not accepted")
	}
}

func TestDiscoverBundledSkillsFindsSkillDirsAndIgnoresNonSkills(t *testing.T) {
	tmp := t.TempDir()
	writeRel(t, tmp, "category/skill-a/SKILL.md", "# Skill A")
	writeRel(t, tmp, "skill-b/SKILL.md", "# Skill B")
	writeRel(t, tmp, "not-a-skill/README.md", "Not a skill")
	writeRel(t, tmp, ".git/hooks/SKILL.md", "# Fake")
	got := map[string]bool{}
	for _, b := range discoverBundledSkills(skillTree{fsys: os.DirFS(tmp), root: tmp}) {
		got[b.name] = true
	}
	if !reflect.DeepEqual(got, map[string]bool{"skill-a": true, "skill-b": true}) {
		t.Fatal(got)
	}
	missing := filepath.Join(tmp, "nonexistent")
	if got := discoverBundledSkills(skillTree{fsys: os.DirFS(missing), root: missing}); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestDiscoverBundledSkillsIgnoresNestedSkillPackagesInSupportDirs(t *testing.T) {
	tmp := t.TempDir()
	writeRel(t, tmp, "category/umbrella/SKILL.md", "---\nname: umbrella\n---\n")
	writeRel(t, tmp, "category/umbrella/references/archived-skill/SKILL.md", "---\nname: archived-skill\n---\n")
	var names []string
	for _, b := range discoverBundledSkills(skillTree{fsys: os.DirFS(tmp), root: tmp}) {
		names = append(names, b.name)
	}
	if !reflect.DeepEqual(names, []string{"umbrella"}) {
		t.Fatal(names)
	}
}

func TestReadSkillNameFromFrontmatterWithDirNameFallbacks(t *testing.T) {
	md := filepath.Join(t.TempDir(), "SKILL.md")
	for _, c := range []struct{ text, fallback, want string }{
		{"---\nname: audiocraft-audio-generation\n---\n# Skill", "audiocraft", "audiocraft-audio-generation"},
		{"---\nname: \"serving-llms-vllm\"\n---\n", "vllm", "serving-llms-vllm"},
		{"# Just a heading\nNo frontmatter here", "my-skill", "my-skill"},
		{"---\nname:\n---\n", "fallback", "fallback"},
	} {
		os.WriteFile(md, []byte(c.text), 0o644)
		if got := readSkillName(md, c.fallback); got != c.want {
			t.Fatal(c.text, got)
		}
	}
}

func TestReadSkillNameDiscoverUsesFrontmatterName(t *testing.T) {
	tmp := t.TempDir()
	writeRel(t, tmp, "category/audiocraft/SKILL.md", "---\nname: audiocraft-audio-generation\n---\n# Skill")
	if got := discoverBundledSkills(skillTree{fsys: os.DirFS(tmp), root: tmp}); got[0].name != "audiocraft-audio-generation" {
		t.Fatal(got)
	}
}

func TestRmtreeWritableRefusesAnythingNotAStrictChildOfSkills(t *testing.T) {
	icebath := filepath.Join(t.TempDir(), "home")
	skills := filepath.Join(icebath, "skills")
	os.MkdirAll(filepath.Join(skills, "keep"), 0o755)
	sibling := filepath.Join(icebath, "kanban.db")
	os.MkdirAll(sibling, 0o755)
	s := NewScope(icebath, config0())
	s.SkillsDirOverride = skills
	for _, target := range []string{"/", icebath, sibling, skills} {
		if err := s.rmtreeWritable(target); err == nil || !strings.Contains(err.Error(), "refusing to rmtree") {
			t.Fatal(target, err)
		}
	}
	if !lexists(filepath.Join(skills, "keep")) || !lexists(sibling) {
		t.Fatal("guard wiped a path")
	}
}

func TestRmtreeWritableAllowsSubdirectoryOfSkills(t *testing.T) {
	tmp := t.TempDir()
	skills := filepath.Join(tmp, "skills")
	sub := filepath.Join(skills, "category", "old-skill")
	writeRel(t, sub, "SKILL.md", "# old")
	s := NewScope(tmp, config0())
	s.SkillsDirOverride = skills
	if err := s.rmtreeWritable(sub); err != nil || !lexists(skills) || lexists(sub) {
		t.Fatal(err)
	}
	ro := filepath.Join(skills, "cat", "ro-skill")
	writeRel(t, ro, "sub/f", "x")
	os.Chmod(filepath.Join(ro, "sub", "f"), 0o444)
	os.Chmod(filepath.Join(ro, "sub"), 0o555)
	os.Chmod(ro, 0o555)
	if err := s.rmtreeWritable(ro); err != nil || lexists(ro) {
		t.Fatal(err)
	}
}

// Bundled tree with devops/clair-qa and creative/ascii-art
func externalDirsBundled(t *testing.T, tmp string) string {
	bundled := filepath.Join(tmp, "bundled_skills")
	writeRel(t, bundled, "devops/clair-qa/SKILL.md", "# bundled clair")
	writeRel(t, bundled, "creative/ascii-art/SKILL.md", "# bundled ascii")
	return bundled
}

func TestExternalDirsIndexingShadowedSkillSkippedAndNotManifested(t *testing.T) {
	tmp := t.TempDir()
	bundled := externalDirsBundled(t, tmp)
	skillsDir := filepath.Join(tmp, "user_skills")
	ext := filepath.Join(tmp, "external_skills")
	writeRel(t, ext, "devops/clair-qa/SKILL.md", "# external clair")
	writeRel(t, ext, "devops/clair-qa/main.py", "print('ext')")
	s := patchedSync(t, bundled, skillsDir)
	writeRel(t, s.Home, "config.yaml", "skills:\n  external_dirs:\n    - "+ext+"\n")
	r := mustSync(t, s)
	m := mustManifest(t, s)
	if !slices.Contains(strs(r, "shadowed_by_external"), "clair-qa") || slices.Contains(strs(r, "copied"), "clair-qa") || !slices.Contains(strs(r, "copied"), "ascii-art") {
		t.Fatal(r)
	}
	if lexists(filepath.Join(skillsDir, "devops", "clair-qa")) {
		t.Fatal("shadowed skill written locally")
	}
	if _, ok := m["clair-qa"]; ok {
		t.Fatal(m)
	}
	if _, ok := m["ascii-art"]; !ok {
		t.Fatal(m)
	}
}

func TestExternalDirsIndexingNoExternalDirsUnchanged(t *testing.T) {
	tmp := t.TempDir()
	s := patchedSync(t, externalDirsBundled(t, tmp), filepath.Join(tmp, "user_skills"))
	r := mustSync(t, s)
	if !slices.Contains(strs(r, "copied"), "clair-qa") || !slices.Contains(strs(r, "copied"), "ascii-art") || !reflect.DeepEqual(strs(r, "shadowed_by_external"), []string{}) {
		t.Fatal(r)
	}
}

// Skill dir at root/rel whose SKILL.md carries name and body
func renamedSkill(t *testing.T, root, rel, body, name string) string {
	d := filepath.Join(root, filepath.FromSlash(rel))
	writeRel(t, d, "SKILL.md", "---\nname: "+name+"\n---\n"+body)
	return d
}

func TestRenamedBundledSkillRecoveryRelocatesUnmodifiedCopy(t *testing.T) {
	tmp := t.TempDir()
	bundled, skillsDir := filepath.Join(tmp, "bundled"), filepath.Join(tmp, "user_skills")
	old := renamedSkill(t, skillsDir, "oldcat/moved-skill", "# Body\n", "moved-skill")
	writeRel(t, skillsDir, ".bundled_manifest", "moved-skill:"+DirHash(old)+"\n")
	renamedSkill(t, bundled, "newcat/moved-skill", "# Updated upstream\n", "moved-skill")
	s := patchedSync(t, bundled, skillsDir)
	r := mustSync(t, s)
	recorded := mustManifest(t, s)["moved-skill"]
	newDir := filepath.Join(skillsDir, "newcat", "moved-skill")
	if !lexists(newDir) || lexists(old) {
		t.Fatal("renamed skill was not relocated")
	}
	if !slices.Contains(strs(r, "relocated"), "moved-skill") || !slices.Contains(strs(r, "updated"), "moved-skill") {
		t.Fatal(r)
	}
	if !strings.Contains(mustRead(t, filepath.Join(newDir, "SKILL.md")), "Updated upstream") || recorded != DirHash(filepath.Join(bundled, "newcat", "moved-skill")) {
		t.Fatal(recorded)
	}
}

func TestRenamedBundledSkillRecoveryLeavesUserModifiedAndHubOwnedCopiesAlone(t *testing.T) {
	tmp := t.TempDir()
	bundled, skillsDir := filepath.Join(tmp, "bundled"), filepath.Join(tmp, "user_skills")
	edited := renamedSkill(t, skillsDir, "oldcat/moved-skill", "# Body\n", "moved-skill")
	editedHash := DirHash(edited)
	writeRel(t, edited, "SKILL.md", "---\nname: moved-skill\n---\n# MY EDITS\n")
	hub := renamedSkill(t, skillsDir, "oldcat/hub-skill", "# Body\n", "hub-skill")
	writeRel(t, skillsDir, ".bundled_manifest", "moved-skill:"+editedHash+"\nhub-skill:"+DirHash(hub)+"\n")
	writeRel(t, skillsDir, ".hub/lock.json", `{"version": 1, "installed": {"hub-skill": {"install_path": "oldcat/hub-skill"}}}`)
	renamedSkill(t, bundled, "newcat/moved-skill", "# Updated upstream\n", "moved-skill")
	renamedSkill(t, bundled, "newcat/hub-skill", "# Updated upstream\n", "hub-skill")
	r := mustSync(t, patchedSync(t, bundled, skillsDir))
	if !lexists(edited) || !strings.Contains(mustRead(t, filepath.Join(edited, "SKILL.md")), "MY EDITS") || !lexists(hub) {
		t.Fatal("user or hub copy moved")
	}
	if slices.Contains(strs(r, "relocated"), "moved-skill") || slices.Contains(strs(r, "relocated"), "hub-skill") {
		t.Fatal(r)
	}
}

func TestRenamedBundledSkillRecoveryGenuineUserDeletionStillRespected(t *testing.T) {
	tmp := t.TempDir()
	bundled, skillsDir := filepath.Join(tmp, "bundled"), filepath.Join(tmp, "user_skills")
	writeRel(t, skillsDir, ".bundled_manifest", "moved-skill:deadbeef\n")
	renamedSkill(t, bundled, "newcat/moved-skill", "# Body\n", "moved-skill")
	r := mustSync(t, patchedSync(t, bundled, skillsDir))
	if lexists(filepath.Join(skillsDir, "newcat", "moved-skill")) || slices.Contains(strs(r, "copied"), "moved-skill") || slices.Contains(strs(r, "relocated"), "moved-skill") {
		t.Fatal(r)
	}
}

// Bundled tree with category/new-skill, its DESCRIPTION.md and top level old-skill
func syncSkillsBundled(t *testing.T, tmp string) string {
	bundled := filepath.Join(tmp, "bundled_skills")
	writeRel(t, bundled, "category/new-skill/SKILL.md", "# New")
	writeRel(t, bundled, "category/new-skill/main.py", "print(1)")
	writeRel(t, bundled, "category/DESCRIPTION.md", "Category desc")
	writeRel(t, bundled, "old-skill/SKILL.md", "# Old")
	return bundled
}

func TestSyncSkillsSuppressedBuiltinNotReseeded(t *testing.T) {
	tmp := t.TempDir()
	skillsDir := filepath.Join(tmp, "user_skills")
	s := patchedSync(t, syncSkillsBundled(t, tmp), skillsDir)
	writeRel(t, skillsDir, ".curator_suppressed", "old-skill\n")
	r := mustSync(t, s)
	if !slices.Contains(strs(r, "suppressed"), "old-skill") || slices.Contains(strs(r, "copied"), "old-skill") || lexists(filepath.Join(skillsDir, "old-skill")) {
		t.Fatal(r)
	}
	if !slices.Contains(strs(r, "copied"), "new-skill") || !lexists(filepath.Join(skillsDir, "category", "new-skill", "SKILL.md")) {
		t.Fatal(r)
	}
}

func TestSyncSkillsFreshInstallCopiesAllAndRecordsOriginHashes(t *testing.T) {
	tmp := t.TempDir()
	skillsDir := filepath.Join(tmp, "user_skills")
	s := patchedSync(t, syncSkillsBundled(t, tmp), skillsDir)
	r := mustSync(t, s)
	m := mustManifest(t, s)
	if len(strs(r, "copied")) != 2 || r["total_bundled"] != 2 || len(strs(r, "updated")) != 0 || len(strs(r, "user_modified")) != 0 || len(strs(r, "cleaned")) != 0 {
		t.Fatal(r)
	}
	for _, p := range []string{"category/new-skill/SKILL.md", "old-skill/SKILL.md", "category/DESCRIPTION.md"} {
		if !lexists(filepath.Join(skillsDir, filepath.FromSlash(p))) {
			t.Fatal(p)
		}
	}
	if len(m["new-skill"]) != 32 || len(m["old-skill"]) != 32 {
		t.Fatal(m)
	}
}

func TestSyncSkillsUserDeletedSkillNotReAddedAndStaleEntriesCleaned(t *testing.T) {
	tmp := t.TempDir()
	bundled := syncSkillsBundled(t, tmp)
	skillsDir := filepath.Join(tmp, "user_skills")
	writeRel(t, skillsDir, ".bundled_manifest", "old-skill:"+DirHash(filepath.Join(bundled, "old-skill"))+"\nremoved-skill:def456\n")
	s := patchedSync(t, bundled, skillsDir)
	r := mustSync(t, s)
	m := mustManifest(t, s)
	if !slices.Contains(strs(r, "copied"), "new-skill") || slices.Contains(strs(r, "copied"), "old-skill") || slices.Contains(strs(r, "updated"), "old-skill") || lexists(filepath.Join(skillsDir, "old-skill")) {
		t.Fatal(r)
	}
	if _, kept := m["removed-skill"]; !slices.Contains(strs(r, "cleaned"), "removed-skill") || kept {
		t.Fatal(r, m)
	}
}

func TestSyncSkillsCopyFailureDoesNotPoisonManifestOrDestroyUserCopy(t *testing.T) {
	tmp := t.TempDir()
	skillsDir := filepath.Join(tmp, "user_skills")
	s := patchedSync(t, syncSkillsBundled(t, tmp), skillsDir)
	user := filepath.Join(skillsDir, "old-skill")
	writeRel(t, user, "SKILL.md", "# Old v1")
	writeRel(t, skillsDir, ".bundled_manifest", "old-skill:"+DirHash(user)+"\n")
	restore := withCopyTree(t, func(_ skillTree, _, dest string, _, _ bool) error {
		return simulatedCopyFailure(dest, "Simulated disk full")
	})
	r := mustSync(t, s)
	if slices.Contains(strs(r, "copied"), "new-skill") || slices.Contains(strs(r, "updated"), "old-skill") || !lexists(user) {
		t.Fatal(r)
	}
	if _, ok := mustManifest(t, s)["new-skill"]; ok {
		t.Fatal("failed copy recorded in manifest")
	}
	restore()
	r2 := mustSync(t, s)
	if !slices.Contains(strs(r2, "copied"), "new-skill") || !lexists(filepath.Join(skillsDir, "category", "new-skill", "SKILL.md")) {
		t.Fatal(r2)
	}
}

// Rebuilds the bundled tree with one cat/<name> skill per name
func droppedBundled(t *testing.T, tmp string, names ...string) string {
	bundled := filepath.Join(tmp, "bundled_skills")
	os.RemoveAll(bundled)
	for _, n := range names {
		writeRel(t, bundled, "cat/"+n+"/SKILL.md", "---\nname: "+n+"\n---\n# "+n+"\n")
	}
	return bundled
}

func TestDroppedBuiltinProvenanceCatalogDropKeepsBuiltinProvenanceWhileACopyExists(t *testing.T) {
	tmp := t.TempDir()
	h := filepath.Join(tmp, "home")
	os.MkdirAll(h, 0o755)
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", filepath.Join(tmp, "optional-skills"))
	s := NewScope(h, nil)
	skills := s.SkillsDir()
	t.Setenv("ICEBATH_BUNDLED_SKILLS", droppedBundled(t, tmp, "kept", "dropped", "dropped-archived", "dropped-deleted"))
	mustSync(t, s)
	os.Mkdir(filepath.Join(skills, ".archive"), 0o755)
	if _, err := pyfs.Move(filepath.Join(skills, "cat", "dropped-archived"), filepath.Join(skills, ".archive", "dropped-archived")); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(skills, "cat", "dropped-deleted"))
	droppedBundled(t, tmp, "kept")
	r := mustSync(t, s)
	m := mustManifest(t, s)
	if !reflect.DeepEqual(strs(r, "cleaned"), []string{"dropped-deleted"}) {
		t.Fatal(r)
	}
	if _, ok := m["dropped"]; !ok {
		t.Fatal(m)
	}
	if _, ok := m["dropped-archived"]; !ok {
		t.Fatal(m)
	}
	for _, n := range []string{"dropped", "dropped-archived"} {
		if p, err := s.Provenance(n); err != nil || p != "bundled" {
			t.Fatal(n, p, err)
		}
	}
}

func TestDroppedBuiltinProvenanceSuppressedBuiltinIsNotAgentAuthoredAfterManifestCleanup(t *testing.T) {
	s := NewScope(t.TempDir(), nil)
	writeRel(t, s.SkillsDir(), "resurrected/SKILL.md", "---\nname: resurrected\n---\n")
	writeRel(t, s.SkillsDir(), ".curator_suppressed", "resurrected\n")
	if p, err := s.Provenance("resurrected"); err != nil || p != "bundled" {
		t.Fatal(p, err)
	}
}

func TestGetBundledDirEnvVarOverrideWithDefaultFallback(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom_skills")
	os.Mkdir(custom, 0o755)
	t.Setenv("ICEBATH_BUNDLED_SKILLS", custom)
	if tr := bundledTree(); tr.root != custom {
		t.Fatal(tr)
	}
	t.Setenv("ICEBATH_BUNDLED_SKILLS", "")
	if tr := bundledTree(); tr.root != "" || tr.tag != "bundled" || !tr.exists() {
		t.Fatal(tr)
	}
	os.Unsetenv("ICEBATH_BUNDLED_SKILLS")
	if tr := bundledTree(); tr.root != "" || tr.tag != "bundled" || !tr.exists() {
		t.Fatal(tr)
	}
}

// Bundled tree with productivity/google-workspace at its upstream v2 text
func resetBundled(t *testing.T, tmp string) string {
	bundled := filepath.Join(tmp, "bundled_skills")
	writeRel(t, bundled, "productivity/google-workspace/SKILL.md", "---\nname: google-workspace\n---\n# GW v2 (upstream)\n")
	return bundled
}

func TestResetBundledSkillClearsStuckUserModifiedFlag(t *testing.T) {
	tmp := t.TempDir()
	bundled := resetBundled(t, tmp)
	skillsDir := filepath.Join(tmp, "user_skills")
	dest := filepath.Join(skillsDir, "productivity", "google-workspace")
	writeRel(t, dest, "SKILL.md", "---\nname: google-workspace\n---\n# GW v2 (upstream)\n")
	writeRel(t, skillsDir, ".bundled_manifest", "google-workspace:STALEHASH000000000000000000000000\n")
	s := patchedSync(t, bundled, skillsDir)
	if pre := mustSync(t, s); !slices.Contains(strs(pre, "user_modified"), "google-workspace") {
		t.Fatal(pre)
	}
	r, err := s.ResetBundledSkill(context.Background(), "google-workspace", false)
	if err != nil || r["ok"] != true || r["action"] != "manifest_cleared" {
		t.Fatal(r, err)
	}
	if got := mustManifest(t, s)["google-workspace"]; got != DirHash(filepath.Join(bundled, "productivity", "google-workspace")) {
		t.Fatal(got)
	}
	if !lexists(dest) || !strings.Contains(mustRead(t, filepath.Join(dest, "SKILL.md")), "GW v2") {
		t.Fatal("user copy lost")
	}
	if name, _ := s.BundledSkillForInstall("NousResearch/hermes-agent/skills/google-workspace"); name != "google-workspace" {
		t.Fatal(name)
	}
	if name, _ := s.BundledSkillForInstall("someone/else/google-workspace"); name != "" {
		t.Fatal(name)
	}
}

func TestResetBundledSkillErrorsWhenUntrackedOrRemovedUpstream(t *testing.T) {
	tmp := t.TempDir()
	skillsDir := filepath.Join(tmp, "user_skills")
	writeRel(t, skillsDir, ".bundled_manifest", "")
	s := patchedSync(t, resetBundled(t, tmp), skillsDir)
	untracked, _ := s.ResetBundledSkill(context.Background(), "some-hub-skill", false)
	if untracked["ok"] != false || untracked["action"] != "not_in_manifest" {
		t.Fatal(untracked)
	}
	writeRel(t, skillsDir, "productivity/ghost-skill/SKILL.md", "---\nname: ghost-skill\n---\n# Ghost\n")
	writeRel(t, skillsDir, ".bundled_manifest", "ghost-skill:OLDHASH00000000000000000000000000\n")
	removed, _ := s.ResetBundledSkill(context.Background(), "ghost-skill", true)
	if removed["ok"] != false || removed["action"] != "bundled_missing" {
		t.Fatal(removed)
	}
}

func TestResetBundledSkillRestoreSucceedsOnReadonlyNixTree(t *testing.T) {
	tmp := t.TempDir()
	skillsDir := filepath.Join(tmp, "user_skills")
	dest := filepath.Join(skillsDir, "productivity", "google-workspace")
	sub := filepath.Join(dest, "references")
	writeRel(t, dest, "SKILL.md", "# user version\n")
	writeRel(t, sub, "ref.md", "# nested ref\n")
	writeRel(t, skillsDir, ".bundled_manifest", "google-workspace:STALEHASH000000000000000000000000\n")
	os.Chmod(filepath.Join(sub, "ref.md"), 0o400)
	os.Chmod(filepath.Join(dest, "SKILL.md"), 0o400)
	os.Chmod(sub, 0o555)
	os.Chmod(dest, 0o555)
	t.Cleanup(func() {
		for _, p := range []string{sub, dest} {
			if lexists(p) {
				os.Chmod(p, 0o700)
			}
		}
	})
	s := patchedSync(t, resetBundled(t, tmp), skillsDir)
	r, err := s.ResetBundledSkill(context.Background(), "google-workspace", true)
	if err != nil || r["ok"] != true || r["action"] != "restored" || r["message"] != "Restored 'google-workspace' from bundled source." {
		t.Fatal(r, err)
	}
	if !strings.Contains(mustRead(t, filepath.Join(dest, "SKILL.md")), "upstream") || lexists(filepath.Join(sub, "ref.md")) {
		t.Fatal("read only user copy not replaced")
	}
	if !slices.Contains(strs(r["synced"].(map[string]any), "copied"), "google-workspace") {
		t.Fatal(r)
	}
}

func TestResetBundledSkillRestorePreservesManifestOnRmtreeFailure(t *testing.T) {
	tmp := t.TempDir()
	skillsDir := filepath.Join(tmp, "user_skills")
	dest := filepath.Join(skillsDir, "productivity", "google-workspace")
	writeRel(t, dest, "SKILL.md", "# user version\n")
	writeRel(t, skillsDir, ".bundled_manifest", "google-workspace:STALEHASH000000000000000000000000\n")
	s := patchedSync(t, resetBundled(t, tmp), skillsDir)
	prev := syncSteps.rmtree
	syncSteps.rmtree = func(*Scope, string) error { return syscall.EACCES }
	t.Cleanup(func() { syncSteps.rmtree = prev })
	r, err := s.ResetBundledSkill(context.Background(), "google-workspace", true)
	if err != nil || r["ok"] != false || r["action"] != "not_reset" {
		t.Fatal(r, err)
	}
	if !strings.Contains(mustRead(t, s.manifestFile()), "google-workspace") || !lexists(filepath.Join(dest, "SKILL.md")) {
		t.Fatal("manifest or user copy changed")
	}
}

func TestNoBundledSkillsOptOutMarkerSkipsSyncAndRemovalSeedsNormally(t *testing.T) {
	tmp := t.TempDir()
	bundled := filepath.Join(tmp, "bundled")
	writeRel(t, bundled, "category/new-skill/SKILL.md", "---\nname: new-skill\n---\nbody\n")
	skillsDir := filepath.Join(tmp, "user_skills")
	marker := writeRel(t, tmp, "home/.no-bundled-skills", "opted out\n")
	s := patchedSync(t, bundled, skillsDir)
	opted := mustSync(t, s)
	if opted["skipped_opt_out"] != true || !reflect.DeepEqual(strs(opted, "copied"), []string{}) || opted["total_bundled"] != 0 {
		t.Fatal(opted)
	}
	if lexists(filepath.Join(skillsDir, "category", "new-skill", "SKILL.md")) {
		t.Fatal("opted out sync wrote a skill")
	}
	os.Remove(marker)
	seeded := mustSync(t, s)
	if seeded["skipped_opt_out"] == true || !slices.Contains(strs(seeded, "copied"), "new-skill") || !lexists(filepath.Join(skillsDir, "category", "new-skill", "SKILL.md")) {
		t.Fatal(seeded)
	}
}

func TestOptOutToggleAndRemoveMarkerToggle(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	os.Mkdir(h, 0o755)
	marker := filepath.Join(h, ".no-bundled-skills")
	s := NewScope(h, nil)
	if lexists(marker) {
		t.Fatal("marker present")
	}
	if r := s.SetBundledSkillsOptOut(true); r["ok"] != true || r["changed"] != true || !lexists(marker) || !strings.HasPrefix(r["message"].(string), "Opted out") {
		t.Fatal(r)
	}
	if r := s.SetBundledSkillsOptOut(true); r["ok"] != true || r["changed"] != false {
		t.Fatal(r)
	}
	if r := s.SetBundledSkillsOptOut(false); r["ok"] != true || r["changed"] != true || lexists(marker) {
		t.Fatal(r)
	}
	if r := s.SetBundledSkillsOptOut(false); r["message"] != "Not opted out — no marker to remove." {
		t.Fatal(r)
	}
}

func TestOptOutToggleAndRemoveKeepsUserModified(t *testing.T) {
	tmp := t.TempDir()
	bundled := filepath.Join(tmp, "bundled")
	for _, n := range []string{"alpha", "beta"} {
		writeRel(t, bundled, n+"/SKILL.md", "---\nname: "+n+"\n---\nbody "+n+"\n")
	}
	skillsDir := filepath.Join(tmp, "user_skills")
	s := patchedSync(t, bundled, skillsDir)
	mustSync(t, s)
	writeRel(t, skillsDir, "beta/SKILL.md", "---\nname: beta\n---\nEDITED\n")
	writeRel(t, skillsDir, "mine/SKILL.md", "---\nname: mine\n---\nlocal\n")
	preview, err := s.RemovePristineBundledSkills(context.Background(), true)
	if err != nil || !slices.Contains(strs(preview, "removed"), "alpha") || slices.Contains(strs(preview, "removed"), "beta") {
		t.Fatal(preview, err)
	}
	res, err := s.RemovePristineBundledSkills(context.Background(), false)
	if err != nil || !slices.Contains(strs(res, "removed"), "alpha") || lexists(filepath.Join(skillsDir, "alpha")) {
		t.Fatal(res, err)
	}
	if !strings.Contains(mustRead(t, filepath.Join(skillsDir, "beta", "SKILL.md")), "EDITED") || !lexists(filepath.Join(skillsDir, "mine", "SKILL.md")) {
		t.Fatal("removal touched a kept skill")
	}
	if skipped := res["skipped"].([]map[string]any); len(skipped) != 1 || skipped[0]["reason"] != "user-modified (kept)" {
		t.Fatal(res)
	}
}

// Bundled flat old-skill plus a user copy whose hash matches the manifest origin
func backupRecoverySetup(t *testing.T) (s *Scope, bundled, skillsDir, dest string) {
	tmp := t.TempDir()
	bundled = filepath.Join(tmp, "bundled_skills")
	writeRel(t, bundled, "old-skill/SKILL.md", "# Old v2 (updated)")
	skillsDir = filepath.Join(tmp, "user_skills")
	s = patchedSync(t, bundled, skillsDir)
	dest = filepath.Join(skillsDir, "old-skill")
	writeRel(t, dest, "SKILL.md", "# Old v1")
	s.writeManifest(context.Background(), map[string]string{"old-skill": DirHash(dest)})
	return s, bundled, skillsDir, dest
}

func TestUpdateBackupRecoveryStaleBackupDoesNotPoisonFailedUpdate(t *testing.T) {
	s, _, skillsDir, dest := backupRecoverySetup(t)
	writeRel(t, skillsDir, "old-skill.bak/SKILL.md", "# stale junk from an earlier failure")
	withCopyTree(t, func(_ skillTree, _, d string, _, _ bool) error {
		return simulatedCopyFailure(d, "simulated copy failure")
	})
	mustSync(t, s)
	if mustRead(t, filepath.Join(dest, "SKILL.md")) != "# Old v1" || lexists(filepath.Join(dest, "old-skill")) || lexists(filepath.Join(skillsDir, "old-skill.bak")) {
		t.Fatal("stale backup poisoned the restore")
	}
}

func TestUpdateBackupRecoveryOrphanedBackupIsRecoveredNotTreatedAsDeleted(t *testing.T) {
	s, _, skillsDir, dest := backupRecoverySetup(t)
	if _, err := pyfs.Move(dest, filepath.Join(skillsDir, "old-skill.bak")); err != nil {
		t.Fatal(err)
	}
	r := mustSync(t, s)
	if mustRead(t, filepath.Join(dest, "SKILL.md")) != "# Old v2 (updated)" || !slices.Contains(strs(r, "updated"), "old-skill") || lexists(filepath.Join(skillsDir, "old-skill.bak")) {
		t.Fatal(r)
	}
}

func TestUpdateBackupRecoveryPartialCopyFailureRestoresOriginal(t *testing.T) {
	s, _, skillsDir, dest := backupRecoverySetup(t)
	restore := withCopyTree(t, func(_ skillTree, _, d string, _, _ bool) error {
		writeRel(t, d, "PARTIAL", "half-written")
		return simulatedCopyFailure(d, "simulated failure mid-copy")
	})
	mustSync(t, s)
	if mustRead(t, filepath.Join(dest, "SKILL.md")) != "# Old v1" || lexists(filepath.Join(dest, "PARTIAL")) || lexists(filepath.Join(skillsDir, "old-skill.bak")) {
		t.Fatal("partial copy not rolled back")
	}
	restore()
	r2 := mustSync(t, s)
	if !slices.Contains(strs(r2, "updated"), "old-skill") || !reflect.DeepEqual(strs(r2, "user_modified"), []string{}) {
		t.Fatal(r2)
	}
}

func TestCallTimeDirResolutionAccessorsFollowHomeOverride(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profiles", "research")
	s := NewScope(home.Dir(homeCtx(profile)), nil)
	if s.Home != profile || s.SkillsDir() != filepath.Join(profile, "skills") || s.manifestFile() != filepath.Join(profile, "skills", ".bundled_manifest") {
		t.Fatal(s.Home, s.SkillsDir(), s.manifestFile())
	}
}

func TestCallTimeDirResolutionExplicitModulePatchWinsOverOverride(t *testing.T) {
	tmp := t.TempDir()
	patched := filepath.Join(tmp, "patched-skills")
	s := NewScope(home.Dir(homeCtx(filepath.Join(tmp, "other-profile"))), nil)
	s.SkillsDirOverride = patched
	if s.SkillsDir() != patched || s.manifestFile() != filepath.Join(patched, ".bundled_manifest") {
		t.Fatal(s.SkillsDir(), s.manifestFile())
	}
}

func TestCallTimeDirResolutionRmtreeGuardAnchorsOnOverriddenProfile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, "process-home"))
	profile := filepath.Join(tmp, "profiles", "worker")
	victim := filepath.Join(profile, "skills", "doomed-skill")
	writeRel(t, victim, "SKILL.md", "---\nname: doomed-skill\n---\n")
	s := NewScope(home.Dir(homeCtx(profile)), nil)
	if err := s.rmtreeWritable(victim); err != nil || lexists(victim) {
		t.Fatal(err)
	}
	foreign := filepath.Join(home.Dir(context.Background()), "skills", "some-skill")
	if err := s.rmtreeWritable(foreign); err == nil || !strings.Contains(err.Error(), "refusing to rmtree") {
		t.Fatal(err)
	}
}

// Pre runtime cache dir_hash over every file in sorted path order
func legacyHash(t *testing.T, dir string) string {
	var rels []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			rels = append(rels, rel)
		}
		return nil
	})
	slices.SortFunc(rels, func(a, b string) int {
		return slices.Compare(strings.Split(a, string(filepath.Separator)), strings.Split(b, string(filepath.Separator)))
	})
	h := md5.New()
	for _, rel := range rels {
		h.Write([]byte(rel))
		h.Write([]byte(mustRead(t, filepath.Join(dir, rel))))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Profile with coding/demo bundled and synced, returning the source and installed dirs
func runtimeCacheTree(t *testing.T) (s *Scope, src, dest string) {
	base := filepath.Join(t.TempDir(), "profile")
	bundled := filepath.Join(base, "bundled")
	src = filepath.Join(bundled, "coding", "demo")
	writeRel(t, src, "SKILL.md", "---\nname: demo\n---\n# Demo\n")
	writeRel(t, src, "scripts/helper.py", "ANSWER = 1\n")
	t.Setenv("ICEBATH_BUNDLED_SKILLS", bundled)
	t.Setenv("ICEBATH_OPTIONAL_SKILLS", filepath.Join(base, "no-optional"))
	s = NewScope(base, nil)
	s.SkillsDirOverride = filepath.Join(base, "skills")
	if r := mustSync(t, s); !reflect.DeepEqual(strs(r, "copied"), []string{"demo"}) {
		t.Fatal(r)
	}
	return s, src, filepath.Join(base, "skills", "coding", "demo")
}

// Diff result of the demo skill
func demoDiff(t *testing.T, s *Scope) map[string]any {
	d, err := s.DiffBundledSkill("demo")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Names of the user modified bundled skills
func modifiedNames(t *testing.T, s *Scope) []string {
	mods, err := s.ListUserModifiedBundledSkills()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, m := range mods {
		names = append(names, m["name"].(string))
	}
	return names
}

func TestRuntimeCacheCompiledBytecodeDoesNotFreezeUpdate(t *testing.T) {
	s, src, dest := runtimeCacheTree(t)
	origin := mustManifest(t, s)["demo"]
	pyc := writeRel(t, dest, "scripts/__pycache__/helper.cpython-311.pyc", "\xa7\x0d\x0d\x0a\x00\x00\x00\x00compiled")
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(pyc), "*.pyc")); len(matches) == 0 {
		t.Fatal("no bytecode written")
	}
	if DirHash(dest) != origin || len(modifiedNames(t, s)) != 0 || len(demoDiff(t, s)["diffs"].([]map[string]any)) != 0 {
		t.Fatal("bytecode counted as a user edit")
	}
	writeRel(t, src, "scripts/helper.py", "ANSWER = 2\n")
	r := mustSync(t, s)
	if !reflect.DeepEqual(strs(r, "updated"), []string{"demo"}) || mustRead(t, filepath.Join(dest, "scripts", "helper.py")) != "ANSWER = 2\n" || mustManifest(t, s)["demo"] != DirHash(src) {
		t.Fatal(r)
	}
}

func TestRuntimeCacheIsNotHashOrDiffContent(t *testing.T) {
	for _, cachePath := range []string{
		"scripts/__pycache__/helper.cpython-311.pyc", ".pytest_cache/v/cache/nodeids", "scripts/.mypy_cache/3.11/helper.data.json",
		"scripts/.ruff_cache/0.15.0/abc", "scripts/helper.pyc", "scripts/helper.pyo",
	} {
		t.Run(cachePath, func(t *testing.T) {
			s, src, dest := runtimeCacheTree(t)
			writeRel(t, dest, cachePath, "generated")
			if DirHash(dest) != DirHash(src) || slices.Contains(skillFileListFS(os.DirFS(dest), "."), filepath.FromSlash(cachePath)) || demoDiff(t, s)["modified"] != false {
				t.Fatal(cachePath)
			}
		})
	}
}

func TestRuntimeCacheGenuineEditsNextToCacheArePreserved(t *testing.T) {
	for _, edited := range []string{"SKILL.md", "scripts/helper.py", "references/notes.md", "cache/data.json", "scripts/bytecode_only.pyc"} {
		t.Run(edited, func(t *testing.T) {
			s, src, dest := runtimeCacheTree(t)
			writeRel(t, dest, "scripts/__pycache__/helper.cpython-311.pyc", "generated")
			writeRel(t, dest, edited, "user-owned content")
			writeRel(t, src, "references/new-upstream.md", "new upstream documentation")
			if got := modifiedNames(t, s); !reflect.DeepEqual(got, []string{"demo"}) {
				t.Fatal(got)
			}
			r := mustSync(t, s)
			if !reflect.DeepEqual(strs(r, "user_modified"), []string{"demo"}) || mustRead(t, filepath.Join(dest, filepath.FromSlash(edited))) != "user-owned content" || lexists(filepath.Join(dest, "references", "new-upstream.md")) {
				t.Fatal(r)
			}
		})
	}
}

func TestRuntimeCacheSourceCacheIsNeitherSeededNorRecorded(t *testing.T) {
	s, src, dest := runtimeCacheTree(t)
	writeRel(t, src, "scripts/__pycache__/helper.cpython-311.pyc", "source generated")
	writeRel(t, src, "SKILL.md", "---\nname: demo\n---\n# Changed upstream\n")
	if r := mustSync(t, s); !reflect.DeepEqual(strs(r, "updated"), []string{"demo"}) || lexists(filepath.Join(dest, "scripts", "__pycache__")) {
		t.Fatal(r)
	}
	if err := s.rmtreeWritable(dest); err != nil {
		t.Fatal(err)
	}
	s.writeManifest(context.Background(), map[string]string{})
	if r := mustSync(t, s); !reflect.DeepEqual(strs(r, "copied"), []string{"demo"}) || lexists(filepath.Join(dest, "scripts", "__pycache__")) {
		t.Fatal(r)
	}
}

func TestRuntimeCacheLegacyCacheHashMigratesOnlyWithMatchingOrigin(t *testing.T) {
	s, src, dest := runtimeCacheTree(t)
	writeRel(t, dest, "scripts/__pycache__/helper.cpython-311.pyc", "legacy generated")
	s.writeManifest(context.Background(), map[string]string{"demo": legacyHash(t, dest)})
	if got := modifiedNames(t, s); len(got) != 0 {
		t.Fatal(got)
	}
	writeRel(t, src, "scripts/helper.py", "ANSWER = 2\n")
	if r := mustSync(t, s); !reflect.DeepEqual(strs(r, "updated"), []string{"demo"}) || mustManifest(t, s)["demo"] != DirHash(src) {
		t.Fatal(r)
	}
}

func TestRuntimeCacheLegacyHashMismatchNeverRebaselinesUserEdits(t *testing.T) {
	s, src, dest := runtimeCacheTree(t)
	cache := writeRel(t, dest, "scripts/__pycache__/helper.cpython-311.pyc", "legacy generated")
	origin := legacyHash(t, dest)
	s.writeManifest(context.Background(), map[string]string{"demo": origin})
	os.Remove(cache)
	writeRel(t, dest, "scripts/helper.py", "user edit\n")
	writeRel(t, src, "scripts/helper.py", "upstream edit\n")
	if r := mustSync(t, s); !reflect.DeepEqual(strs(r, "user_modified"), []string{"demo"}) || mustManifest(t, s)["demo"] != origin || mustRead(t, filepath.Join(dest, "scripts", "helper.py")) != "user edit\n" {
		t.Fatal(r)
	}
}

func TestRuntimeCacheCleanManifestHashIsBackwardsCompatible(t *testing.T) {
	_, src, dest := runtimeCacheTree(t)
	if DirHash(src) != legacyHash(t, src) || DirHash(dest) != legacyHash(t, dest) {
		t.Fatal("clean tree hash changed")
	}
}

func TestRuntimeCacheDoesNotHideDotfileEdit(t *testing.T) {
	s, src, dest := runtimeCacheTree(t)
	writeRel(t, dest, ".settings.json", "user settings")
	writeRel(t, dest, ".pytest_cache/v/cache/nodeids", "[]")
	if DirHash(dest) == DirHash(src) || !slices.Contains(skillFileListFS(os.DirFS(dest), "."), ".settings.json") {
		t.Fatal("dotfile edit hidden")
	}
	var paths []string
	for _, e := range demoDiff(t, s)["diffs"].([]map[string]any) {
		paths = append(paths, e["path"].(string))
	}
	if !reflect.DeepEqual(paths, []string{".settings.json"}) {
		t.Fatal(paths)
	}
}

func TestRuntimeCacheLegacyCacheOriginAllowsRenameWithoutFreezing(t *testing.T) {
	s, src, dest := runtimeCacheTree(t)
	writeRel(t, dest, "scripts/__pycache__/helper.cpython-311.pyc", "legacy generated")
	s.writeManifest(context.Background(), map[string]string{"demo": legacyHash(t, dest)})
	newSrc := filepath.Join(filepath.Dir(filepath.Dir(src)), "recategorized", "demo")
	os.MkdirAll(filepath.Dir(newSrc), 0o755)
	if err := os.Rename(src, newSrc); err != nil {
		t.Fatal(err)
	}
	writeRel(t, newSrc, "scripts/helper.py", "ANSWER = 3\n")
	r := mustSync(t, s)
	if !reflect.DeepEqual(strs(r, "updated"), []string{"demo"}) || lexists(dest) || mustRead(t, filepath.Join(s.SkillsDir(), "recategorized", "demo", "scripts", "helper.py")) != "ANSWER = 3\n" {
		t.Fatal(r)
	}
}

func TestRuntimeCacheHashFilterIsSkillRelative(t *testing.T) {
	src := filepath.Join(t.TempDir(), "__pycache__", "demo")
	writeRel(t, src, "SKILL.md", "real skill content")
	writeRel(t, src, "scripts/helper.py", "ANSWER = 1\n")
	got := skillFileListFS(os.DirFS(src), ".")
	slices.Sort(got)
	if DirHash(src) != legacyHash(t, src) || !reflect.DeepEqual(got, []string{"SKILL.md", filepath.Join("scripts", "helper.py")}) {
		t.Fatal(got)
	}
}

// Bundled tree with category/foo and its helper, synced into user_skills
func modifiedDiffEnv(t *testing.T) (*Scope, string) {
	tmp := t.TempDir()
	bundled := filepath.Join(tmp, "bundled_skills")
	writeRel(t, bundled, "category/foo/SKILL.md", "---\nname: foo\n---\n# Foo Skill\n")
	writeRel(t, bundled, "category/foo/helper.py", "print('stock')\n")
	skillsDir := filepath.Join(tmp, "user_skills")
	s := patchedSync(t, bundled, skillsDir)
	mustSync(t, s)
	return s, skillsDir
}

func TestListModifiedPristineSkillIsNotListedAsModified(t *testing.T) {
	s, _ := modifiedDiffEnv(t)
	if got := modifiedNames(t, s); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestListModifiedResetClearsModifiedState(t *testing.T) {
	s, skillsDir := modifiedDiffEnv(t)
	writeRel(t, skillsDir, "category/foo/helper.py", "print('mine')\n")
	if got := modifiedNames(t, s); !reflect.DeepEqual(got, []string{"foo"}) {
		t.Fatal(got)
	}
	if r, err := s.ResetBundledSkill(context.Background(), "foo", true); err != nil || r["ok"] != true {
		t.Fatal(r, err)
	}
	if got := modifiedNames(t, s); len(got) != 0 {
		t.Fatal(got)
	}
}
