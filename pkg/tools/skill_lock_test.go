package tools

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Skill lock source over a temp home with a locked names set
type testSkillLocks struct {
	locked map[string]bool
	byName map[string]string
}

func (s *testSkillLocks) LockedSkillNames(*Env) []string {
	var out []string
	for n := range s.locked {
		out = append(out, n)
	}
	return out
}
func (s *testSkillLocks) SkillsDir(env *Env) string { return filepath.Join(env.Home, "skills") }
func (s *testSkillLocks) AllSkillsDirs(env *Env) []string {
	return []string{filepath.Join(env.Home, "skills")}
}
func (s *testSkillLocks) IsExcludedSkillPath(string, string) bool { return false }
func (s *testSkillLocks) FindSkillDirName(_ *Env, name string) string {
	return s.byName[strings.TrimPrefix(name, "training/")]
}

func lockedHome(t *testing.T) (*Env, string) {
	t.Helper()
	h := t.TempDir()
	skills := filepath.Join(h, "skills")
	for _, pair := range [][2]string{{"ops/locked-skill", "locked-skill"}, {"ops/free-skill", "free-skill"}, {"training/accelerate", "huggingface-accelerate"}} {
		dir := filepath.Join(skills, pair[0])
		if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+pair[1]+"\ndescription: x\n---\nbody\n"), 0o644)
	}
	os.WriteFile(filepath.Join(skills, ".usage.json"), []byte("{}"), 0o644)
	prev := SkillLocks
	SkillLocks = &testSkillLocks{locked: map[string]bool{"locked-skill": true}, byName: map[string]string{"huggingface-accelerate": "accelerate", "accelerate": "accelerate"}}
	t.Cleanup(func() { SkillLocks = prev })
	return &Env{Home: h, Secrets: config.NewSecrets(h), Values: &sync.Map{}}, h
}

func TestFileToolsRefuseALockedSkillAndItsRecord(t *testing.T) {
	env, h := lockedHome(t)
	locked := filepath.Join(h, "skills/ops/locked-skill/SKILL.md")
	for _, p := range []string{locked, filepath.Join(filepath.Dir(locked), "references/new.md"), filepath.Join(h, "skills/.usage.json")} {
		if reason := SkillLockPathRefusal(env, []string{p}); !strings.Contains(reason, "locked") {
			t.Fatalf("allowed %s: %q", p, reason)
		}
	}
	if reason := SkillLockPathRefusal(env, []string{filepath.Join(h, "skills/ops/free-skill/SKILL.md")}); reason != "" {
		t.Fatalf("free skill refused: %s", reason)
	}
}

func TestTerminalRefusesWritesIntoALockedSkillButNotReads(t *testing.T) {
	env, h := lockedHome(t)
	l := filepath.Join(h, "skills/ops/locked-skill")
	cases := []struct {
		command string
		blocked bool
	}{
		{"echo x > {L}/SKILL.md", true},
		{"sed -i s/a/b/ {L}/SKILL.md", true},
		{"cd {L} && echo x > SKILL.md", true},
		{"rm -rf {H}/skills/ops", true},
		{"git -C {L} checkout -- .", true},
		{"bash -c 'cp /etc/hosts {L}/SKILL.md'", true},
		{"python3 -c \"open('{L}/SKILL.md','w').write('x')\"", true},
		{"echo {} > {H}/skills/.usage.json", true},
		{"icebath curator unlock locked-skill", true},
		{"echo checking\nsed -i s/body/x/ {L}/SKILL.md", true},
		{"sudo -u root rm -rf {L}", true},
		{"cd {L} && (cd /) ; echo x > SKILL.md", true},
		{"cp -t {L} {H}/SKILL.md", true},
		{"cp --target-directory={L} {H}/SKILL.md", true},
		{"cp -r {H}/locked-skill {H}/skills/ops", true},
		{"tar xf {H}/a.tar -C {H}/skills/ops", true},
		{"sed -i s/body/x/ {H}/skills/ops/*/SKILL.md", true},
		{"p={L}/SKILL.md; printf x > \"$p\"", true},
		{"export D={L} && cp /etc/hosts $D/SKILL.md", true},
		{"cd {L} && python3 -c \"from pathlib import Path; Path('SKILL.md').write_text('x')\"", true},
		{"find {H}/skills -name SKILL.md -exec sed -i s/a/b/ {} +", true},
		{"cat {L}/SKILL.md", false},
		{"python3 {L}/scripts/run.py > {H}/out.log 2>&1", false},
		{"cp {L}/SKILL.md {H}/copy.md", false},
		{"echo x > {H}/skills/ops/free-skill/SKILL.md", false},
		{"git -C {L} log --oneline", false},
		{"tar xf {H}/a.tar -C {H}", false},
		{"python3 -c \"print(open('{L}/SKILL.md').read())\"", false},
		{"find {H}/skills -name SKILL.md -exec cat {} +", false},
		{"p={H}/out.txt; echo x > \"$p\"", false},
		{"cp -r {H}/new-skill {H}/skills/ops/new-skill", false},
	}
	for _, tc := range cases {
		command := strings.ReplaceAll(strings.ReplaceAll(tc.command, "{L}", l), "{H}", h)
		reason := SkillLockCommandRefusal(env, command, h)
		if (reason != "") != tc.blocked {
			t.Fatalf("%q -> %q (want blocked=%v)", command, reason, tc.blocked)
		}
	}
}

func TestLockResolvesByDisplayNameAsWellAsDirectory(t *testing.T) {
	env, _ := lockedHome(t)
	SkillLocks.(*testSkillLocks).locked["accelerate"] = true
	if !IsLockedSkill(env, "huggingface-accelerate", nil) || !IsLockedSkill(env, "training/accelerate", nil) {
		t.Fatal("display or category name did not resolve")
	}
	if IsLockedSkill(env, "free-skill", nil) {
		t.Fatal("free skill reported locked")
	}
}

func (s *testSkillLocks) ExternalSkillsDirs(*Env) []string { return nil }
func (s *testSkillLocks) ProjectSkillsDirs(*Env) []string  { return nil }
func (s *testSkillLocks) IsExcludedSkillDir(string) bool   { return false }
