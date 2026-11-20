package manage

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/skills"
)

func swapLeftovers(t *testing.T, skillsDir string) []string {
	t.Helper()
	found, _ := filepath.Glob(filepath.Join(skillsDir, ".*icebath-dist*"))
	return found
}

func TestForceInstallPreservesExistingDirWhenNewCopyFails(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	plan := mustInstall(t, staged, "target", false)
	skill := filepath.Join(plan.TargetDir, "skills", "demo", "SKILL.md")
	writeFile(t, skill, "old skill\n")
	writeFile(t, filepath.Join(staged, "skills", "demo", "SKILL.md"), "new skill\n")
	old := distCopyTree
	t.Cleanup(func() { distCopyTree = old })
	distCopyTree = func(src, dst string) error {
		if filepath.Base(src) == "demo" {
			writeFile(t, filepath.Join(dst, "SKILL.md"), "partial\n")
			return errors.New("[WinError 145] The directory is not empty")
		}
		return old(src, dst)
	}
	_, err := InstallDistribution(t.Context(), staged, "target", true, false)
	if err == nil || !strings.Contains(err.Error(), "WinError 145") {
		t.Fatal(err)
	}
	if readText(t, skill) != "old skill\n" || len(swapLeftovers(t, filepath.Join(plan.TargetDir, "skills"))) != 0 {
		t.Fatal("old tree lost or swap leftovers kept")
	}
}

func TestForceInstallRestoresExistingDirWhenStagedSwapFails(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	plan := mustInstall(t, staged, "target", false)
	skill := filepath.Join(plan.TargetDir, "skills", "demo", "SKILL.md")
	writeFile(t, skill, "old skill\n")
	writeFile(t, filepath.Join(staged, "skills", "demo", "SKILL.md"), "new skill\n")
	old := distRename
	t.Cleanup(func() { distRename = old })
	distRename = func(from, to string) error {
		if name := filepath.Base(from); strings.Contains(name, "demo") && strings.Contains(name, "staging-") {
			return errors.New("staged swap failed")
		}
		return old(from, to)
	}
	_, err := InstallDistribution(t.Context(), staged, "target", true, false)
	if err == nil || !strings.Contains(err.Error(), "staged swap failed") {
		t.Fatal(err)
	}
	if readText(t, skill) != "old skill\n" || len(swapLeftovers(t, filepath.Join(plan.TargetDir, "skills"))) != 0 {
		t.Fatal("old tree not restored")
	}
}

func TestForceInstallLockedOldTreeIsNotDiscovered(t *testing.T) {
	tmp, _ := distEnv(t)
	staged := makeStagingDir(t, tmp, "src", nil)
	plan := mustInstall(t, staged, "target", false)
	skillsDir := filepath.Join(plan.TargetDir, "skills")
	writeFile(t, filepath.Join(skillsDir, "demo", "SKILL.md"), "old skill\n")
	writeFile(t, filepath.Join(staged, "skills", "demo", "SKILL.md"), "new skill\n")
	old := distRemoveExisting
	t.Cleanup(func() { distRemoveExisting = old })
	distRemoveExisting = func(p string) error {
		if name := filepath.Base(p); strings.Contains(name, "demo") && strings.Contains(name, "old-") {
			return &os.PathError{Op: "unlink", Path: p, Err: errors.New("[WinError 32] in use")}
		}
		return old(p)
	}
	mustInstall(t, staged, "target", true)
	var all []string
	filepath.WalkDir(skillsDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "SKILL.md" {
			all = append(all, readText(t, p))
		}
		return nil
	})
	if !slices.Contains(all, "old skill\n") {
		t.Fatal("the locked leftover should stay on disk")
	}
	var walked, globbed []string
	for _, p := range skills.IterSkillIndexFiles(skillsDir, "SKILL.md") {
		walked = append(walked, readText(t, p))
	}
	filepath.WalkDir(skillsDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "SKILL.md" && !skills.IsExcludedSkillPath(p, "") {
			globbed = append(globbed, readText(t, p))
		}
		return nil
	})
	for _, found := range [][]string{walked, globbed} {
		if !slices.Contains(found, "new skill\n") || slices.Contains(found, "old skill\n") {
			t.Fatalf("%q", found)
		}
	}
}
