package manage

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/skills"
)

func names(t *testing.T, lazy bool) ([]string, []ProfileInfo) {
	t.Helper()
	infos, err := ListProfiles(context.Background(), lazy)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range infos {
		out = append(out, p.Name)
	}
	return out, infos
}

func TestListProfilesReturnsDefaultWhenNoNamedProfiles(t *testing.T) {
	manageEnv(t)
	if got, _ := names(t, false); len(got) != 1 || got[0] != "default" {
		t.Fatalf("%v", got)
	}
}

func TestListProfilesIncludesNamedProfiles(t *testing.T) {
	tmp := manageEnv(t)
	alpha := namedProfile(t, tmp, "alpha")
	namedProfile(t, tmp, "beta")
	writeFile(t, filepath.Join(alpha, "config.yaml"), "model:\n  default: m1\n  provider: p1\n")
	writeFile(t, filepath.Join(alpha, "distribution.yaml"), "name: dist\nversion: 1.0\nsource: gh\n")
	writeFile(t, filepath.Join(alpha, ".env"), "")
	got, infos := names(t, false)
	if len(got) != 3 || got[1] != "alpha" || got[2] != "beta" {
		t.Fatalf("%v", got)
	}
	a := infos[1]
	if a.Model != "m1" || a.Provider != "p1" || a.DistributionName != "dist" || a.DistributionVersion != 1.0 || !a.HasEnv || a.IsDefault {
		t.Fatalf("%+v", a)
	}
	if infos[2].HasEnv || infos[2].Model != nil {
		t.Fatalf("%+v", infos[2])
	}
}

func TestProfileInfoGatewayRunningFallsBackToMultiplexer(t *testing.T) {
	tmp := manageEnv(t)
	namedProfile(t, tmp, "coder")
	oldRunning, oldServed := checkGatewayRunning, servedByRunningMultiplexer
	defer func() { checkGatewayRunning, servedByRunningMultiplexer = oldRunning, oldServed }()
	checkGatewayRunning = func(string) bool { return false }
	servedByRunningMultiplexer = func(name string) bool { return true }
	_, infos := names(t, false)
	if infos[0].GatewayRunning || !infos[1].GatewayRunning {
		t.Fatalf("default %v coder %v", infos[0].GatewayRunning, infos[1].GatewayRunning)
	}
}

func TestLazySkillCountNeverWalksInThePolledRequest(t *testing.T) {
	tmp := manageEnv(t)
	for _, s := range []string{"s0", "s1", "s2"} {
		writeFile(t, filepath.Join(tmp, ".icebath", "skills", "cat", s, "SKILL.md"), "# s\n")
	}
	var walks atomic.Int32
	release := make(chan struct{})
	old := walkSkillCount
	defer func() { walkSkillCount = old }()
	walkSkillCount = func(dir string) int {
		<-release
		walks.Add(1)
		return old(dir)
	}
	_, first := names(t, true)
	if first[0].SkillCount != 0 || walks.Load() != 0 {
		t.Fatalf("polled request walked %d %d", first[0].SkillCount, walks.Load())
	}
	close(release)
	skillCountRefreshes.Wait()
	if walks.Load() != 1 {
		t.Fatalf("walks %d", walks.Load())
	}
	if _, again := names(t, true); again[0].SkillCount != 3 || walks.Load() != 1 {
		t.Fatalf("second poll %d walks %d", again[0].SkillCount, walks.Load())
	}
	skillCountMu.Lock()
	clear(skillCountCache)
	skillCountMu.Unlock()
	if _, sync := names(t, false); sync[0].SkillCount != 3 || walks.Load() != 2 {
		t.Fatalf("sync count %d walks %d", sync[0].SkillCount, walks.Load())
	}
}

func TestListProfilesSkillCountSurvivesSubtreeVanishingMidWalk(t *testing.T) {
	tmp := manageEnv(t)
	cat := filepath.Join(tmp, ".icebath", "skills", "cat")
	for _, s := range []string{"s0", "s1", "s2", "s3"} {
		if err := os.MkdirAll(filepath.Join(cat, s, "references"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(cat, s, "SKILL.md"), "# s\n")
	}
	resetSkillCountCache()
	t.Cleanup(skills.SwapWalkReadDir(func(dir string) ([]os.DirEntry, error) {
		entries, err := os.ReadDir(dir)
		if dir == cat {
			os.RemoveAll(filepath.Join(cat, "s3"))
		}
		return entries, err
	}))
	if n := countSkills(filepath.Join(tmp, ".icebath")); n != 3 {
		t.Fatalf("count %d", n)
	}
	if got, _ := names(t, false); len(got) != 1 || got[0] != "default" {
		t.Fatalf("%v", got)
	}
}

func TestListProfilesSurfacesCustomAlias(t *testing.T) {
	tmp := manageEnv(t)
	namedProfile(t, tmp, "steve")
	if _, err := profiles.CreateWrapperScript("qiaobusi", "steve"); err != nil {
		t.Fatal(err)
	}
	_, infos := names(t, false)
	steve := infos[1]
	if steve.Name != "steve" || steve.AliasName != "qiaobusi" || filepath.Base(steve.AliasPath) != "qiaobusi" {
		t.Fatalf("%+v", steve)
	}
}

func TestCountSkillsPublishesTimestampAfterTheWalk(t *testing.T) {
	tmp := t.TempDir()
	resetSkillCountCache()
	skillsDir := filepath.Join(tmp, "skills")
	if err := os.Mkdir(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	clock := 1000.0
	oldNow, oldWalk := nowSeconds, walkSkillCount
	defer func() { nowSeconds, walkSkillCount = oldNow, oldWalk }()
	nowSeconds = func() float64 { return clock }
	walkSkillCount = func(string) int {
		clock += skillCountTTL + 5
		return 3
	}
	if countSkills(tmp) != 3 {
		t.Fatal("count")
	}
	entry := skillCountCache[skillsDir]
	if entry.count != 3 || entry.stamped < clock {
		t.Fatalf("%+v clock %v", entry, clock)
	}
	walkSkillCount = func(string) int { t.Fatal("re-walked a fresh entry"); return 0 }
	if countSkills(tmp) != 3 {
		t.Fatal("cached count")
	}
}
