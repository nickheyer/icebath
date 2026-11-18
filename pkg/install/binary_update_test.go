package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tree holding the fixture build of tag and commit as its install binary
func installedTree(t *testing.T, tag, commit string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(InstallBinaryPath(dir), fixtureBuild(t, tag, commit), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestApplyReleaseReplacesTheTreeBinaryWithThePlannedRelease(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	goos, goarch := hostKey()
	f := newReleaseFixture(t, []fixtureRelease{
		{Tag: "v1.1.0", Target: "main", Published: "2026-10-01T00:00:00Z", Assets: map[string][]byte{ReleaseArchiveName("v1.1.0", goos, goarch): fixtureArchive(t, "v1.1.0")}},
		{Tag: "v1.2.0", Target: "main", Published: "2026-11-01T00:00:00Z", Assets: map[string][]byte{ReleaseArchiveName("v1.2.0", goos, goarch): fixtureArchive(t, "v1.2.0")}},
	}, nil)
	dir := installedTree(t, "v1.1.0", "")
	plan, err := PlanRelease(context.Background(), dir, f.repo, ReleaseQuery{Branch: "main"})
	if err != nil || plan.Order != ReleaseNewer || plan.Target.Tag != "v1.2.0" || plan.Ahead != -1 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	var out bytes.Buffer
	if err := ApplyRelease(context.Background(), dir, plan.Target, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if InstalledRelease(context.Background(), InstallBinaryPath(dir)).Tag != "v1.2.0" {
		t.Fatal("binary not replaced")
	}
	if _, err := os.Stat(InstallBinaryPath(dir) + ".download"); !os.IsNotExist(err) {
		t.Fatal("staged download left behind")
	}
	again, err := PlanRelease(context.Background(), dir, f.repo, ReleaseQuery{Branch: "main"})
	if err != nil || again.Order != ReleaseSame {
		t.Fatalf("second plan %+v %v", again, err)
	}
	if _, err := PlanRelease(context.Background(), dir, f.repo, ReleaseQuery{Branch: "feature"}); err == nil || !strings.Contains(err.Error(), "follows branch feature") {
		t.Fatalf("unknown branch %v", err)
	}
}

func TestApplyReleaseRefusesAReleaseWithoutAChecksum(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	f := newReleaseFixture(t, []fixtureRelease{
		{Tag: "v1.2.0", Target: "main", Published: "2026-11-01T00:00:00Z", Assets: map[string][]byte{"icebath-plan9-amd64": []byte("other")}},
	}, nil)
	dir := installedTree(t, "v1.1.0", "")
	before, _ := os.ReadFile(InstallBinaryPath(dir))
	plan, err := PlanRelease(context.Background(), dir, f.repo, ReleaseQuery{Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyRelease(context.Background(), dir, plan.Target, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "lists no checksum") {
		t.Fatalf("a release without a checksum for this host must not install: %v", err)
	}
	if now, _ := os.ReadFile(InstallBinaryPath(dir)); !bytes.Equal(now, before) {
		t.Fatal("installed binary changed")
	}
}

// An older, an equal and a newer release order by commit ancestry, then by stable tags, an unknown build refused
func TestPlanReleaseOrdersOlderEqualAndNewerReleases(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c1, c2, c3 := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	refs := map[string]string{"refs/tags/v1.0.0": c1, "refs/tags/v1.1.0": c2, "refs/tags/v1.2.0": c3}
	release := func(tag, published string) fixtureRelease {
		return fixtureRelease{Tag: tag, Target: "main", Published: published, Assets: map[string][]byte{}}
	}
	for _, tc := range []struct {
		name      string
		newest    string
		installed ReleaseInfo
		history   []string
		order     int
		ahead     int
	}{
		{"newer by ancestry", "v1.2.0", ReleaseInfo{Tag: "v1.1.0", Commit: c2}, []string{c1, c2, c3}, ReleaseNewer, 1},
		{"equal commit", "v1.1.0", ReleaseInfo{Tag: "v1.1.0", Commit: c2}, []string{c1, c2, c3}, ReleaseSame, 0},
		{"older by ancestry", "v1.0.0", ReleaseInfo{Tag: "v1.1.0", Commit: c2}, []string{c1, c2, c3}, ReleaseOlder, 0},
		{"untagged build older by ancestry", "v1.0.0", ReleaseInfo{Commit: c2}, []string{c1, c2, c3}, ReleaseOlder, 0},
		{"untagged build newer by ancestry", "v1.2.0", ReleaseInfo{Commit: c1}, []string{c1, c2, c3}, ReleaseNewer, 2},
		{"older by tag without a comparable history", "v1.0.0", ReleaseInfo{Tag: "v1.1.0", Commit: c2}, nil, ReleaseOlder, 0},
		{"newer by tag without a commit", "v1.2.0", ReleaseInfo{Tag: "v1.1.0"}, nil, ReleaseNewer, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			published := map[string]string{"v1.0.0": "2026-09-01T00:00:00Z", "v1.1.0": "2026-10-01T00:00:00Z", "v1.2.0": "2026-11-01T00:00:00Z"}
			published[tc.newest] = "2026-12-01T00:00:00Z"
			f := newReleaseFixture(t, []fixtureRelease{release("v1.0.0", published["v1.0.0"]), release("v1.1.0", published["v1.1.0"]),
				release("v1.2.0", published["v1.2.0"])}, refs)
			f.history = tc.history
			dir := installedTree(t, tc.installed.Tag, tc.installed.Commit)
			plan, err := PlanRelease(context.Background(), dir, f.repo, ReleaseQuery{Branch: "main"})
			if err != nil || plan.Target.Tag != tc.newest || plan.Order != tc.order || plan.Ahead != tc.ahead {
				t.Fatalf("plan %+v %v", plan, err)
			}
		})
	}
	f := newReleaseFixture(t, []fixtureRelease{release("v1.0.0", "2026-09-01T00:00:00Z")}, refs)
	if _, err := PlanRelease(context.Background(), installedTree(t, "", ""), f.repo, ReleaseQuery{Branch: "main"}); !errors.Is(err, ErrUnknownBuild) {
		t.Fatalf("a build naming no tag and no commit has no order: %v", err)
	}
}

func TestInstallLadderStampsItsMethodAndRepositoryBesideTheBinary(t *testing.T) {
	homeDir, dir := t.TempDir(), t.TempDir()
	t.Setenv("ICEBATH_HOME", homeDir)
	binary := filepath.Join(t.TempDir(), "icebath")
	if err := os.WriteFile(binary, fixtureBinary(t, "v1.2.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := NewRunner(Options{Dir: dir, Binary: binary, Version: "v1.2.0", InstallMethod: MethodScript, RepoURL: "https://example.invalid/fork/icebath"}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	r.stdout, r.stderr = &bytes.Buffer{}, &bytes.Buffer{}
	if err := r.stageRepository(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stamp, _ := os.ReadFile(filepath.Join(dir, InstallMethodFile)); string(stamp) != MethodScript+"\n" {
		t.Fatalf("method stamp %q", stamp)
	}
	if got := InstalledRepository(dir); got != "https://example.invalid/fork/icebath" {
		t.Fatalf("recorded repository %q", got)
	}
}
