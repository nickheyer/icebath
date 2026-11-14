package pm

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Dependency state dir recording its checkout, with one environment generation
func sttState(t *testing.T, installs, key, projectRoot string) string {
	t.Helper()
	state := filepath.Join(installs, key)
	writeFile(t, filepath.Join(state, "inputs", ".project-root"), []byte(projectRoot), 0o666)
	sttMkdir(t, filepath.Join(state, "environments", "gen", "venv"))
	return state
}

// Sets every path under a state dir and the dir itself to an mtime days in the past
func sttAge(t *testing.T, state string, days float64) {
	t.Helper()
	then := time.Now().Add(-time.Duration(days * 24 * float64(time.Hour)))
	var paths []string
	err := filepath.WalkDir(state, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Chtimes(paths[i], then, then); err != nil {
			t.Fatal(err)
		}
	}
}

// Writes a home config carrying the install state retention in days
func sttRetention(t *testing.T, dir string, days int) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "config.yaml"), []byte("worktrees:\n  install_state_retention_days: "+strconv.Itoa(days)+"\n"), 0o666)
	config.ResetCache()
}

func TestStartupPruneReclaimsOnlyProvablyDeletedCheckouts(t *testing.T) {
	tmp := isolateHome(t)
	installs := filepath.Join(tmp, "home", "installs")
	repo := filepath.Join(tmp, "repo")
	sttMkdir(t, filepath.Join(repo, ".worktrees"))
	liveRoot := sttMkdir(t, filepath.Join(tmp, "home", "icebath"))
	live := sttState(t, installs, "aaaa", liveRoot)
	goneScratch := sttState(t, installs, "bbbb", filepath.Join(tmp, "home", "cache", "scratch", "clone"))
	goneWorktree := sttState(t, installs, "cccc", filepath.Join(repo, ".worktrees", "removed-by-hand"))
	unknown := filepath.Join(installs, "dddd")
	sttMkdir(t, filepath.Join(unknown, "environments"))
	hostInstall := sttState(t, installs, "eeee", filepath.Join(tmp, "other-host-home", ".icebath", "icebath"))
	containerInstall := sttState(t, installs, "ffff", filepath.Join(tmp, "opt-icebath"))

	orphans, err := OrphanInstallStates(installs)
	if err != nil {
		t.Fatal(err)
	}
	sttSamePaths(t, orphans, []string{goneScratch, goneWorktree})
	removed, err := CollectOrphanInstallStates(installs)
	if err != nil {
		t.Fatal(err)
	}
	sttSamePaths(t, removed, []string{goneScratch, goneWorktree})
	if sttExists(goneScratch) || sttExists(goneWorktree) {
		t.Fatal("provably deleted checkouts kept their state")
	}
	for _, p := range []string{live, unknown, hostInstall, containerInstall} {
		if !sttIsDir(p) {
			t.Fatalf("%s was reclaimed", p)
		}
	}
}

func TestHeldOrphanIsKept(t *testing.T) {
	tmp := isolateHome(t)
	installs := filepath.Join(tmp, "installs")
	locked := sttState(t, installs, "dddd", filepath.Join(tmp, "gone-a"))
	leasedState := sttState(t, installs, "eeee", filepath.Join(tmp, "gone-b"))
	runtimeState := sttState(t, installs, "ffff", filepath.Join(tmp, "gone-c"))

	files := []*os.File{sttHold(t, filepath.Join(locked, ".install.lock"))}
	for _, leaseDir := range []string{
		filepath.Join(leasedState, "environments", "gen", ".leases"),
		filepath.Join(runtimeState, "pm-runtime", "generations", "gen", ".leases"),
	} {
		sttMkdir(t, leaseDir)
		files = append(files, sttHold(t, filepath.Join(leaseDir, "reader")))
	}
	removed, err := CollectOrphanInstallStates(installs)
	for _, f := range files {
		f.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	sttSamePaths(t, removed, nil)
	for _, p := range []string{locked, leasedState, runtimeState} {
		if !sttIsDir(p) {
			t.Fatalf("held %s was reclaimed", p)
		}
	}
	removed, err = CollectOrphanInstallStates(installs)
	if err != nil {
		t.Fatal(err)
	}
	sttSamePaths(t, removed, []string{locked, leasedState, runtimeState})
}

func TestPartlyRemovedOrphanKeepsItsRecordUntilRetried(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platforms posix")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	tmp := isolateHome(t)
	installs := filepath.Join(tmp, "installs")
	state := sttState(t, installs, "aaaa", filepath.Join(tmp, "gone"))
	stuck := filepath.Join(state, "environments", "gen", "venv")
	writeFile(t, filepath.Join(stuck, "pyvenv.cfg"), nil, 0o666)
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	removed, err := CollectOrphanInstallStates(installs)
	orphans, oerr := OrphanInstallStates(installs)
	os.Chmod(stuck, 0o700)
	if err != nil || oerr != nil {
		t.Fatal(err, oerr)
	}
	sttSamePaths(t, removed, nil)
	sttSamePaths(t, orphans, []string{state})
	removed, err = CollectOrphanInstallStates(installs)
	if err != nil {
		t.Fatal(err)
	}
	sttSamePaths(t, removed, []string{state})
	if sttExists(state) {
		t.Fatal("retried orphan still exists")
	}
}

func TestUnprovableMissingCheckoutIsReclaimedOnceUnusedPastRetention(t *testing.T) {
	tmp := isolateHome(t)
	installs := filepath.Join(tmp, "home", "installs")
	presentRoot := sttMkdir(t, filepath.Join(tmp, "review", "present"))
	fresh := sttState(t, installs, "aaaa", filepath.Join(tmp, "review", "fresh"))
	idle := sttState(t, installs, "bbbb", filepath.Join(tmp, "review", "idle"))
	leasedState := sttState(t, installs, "cccc", filepath.Join(tmp, "review", "leased"))
	present := sttState(t, installs, "dddd", presentRoot)
	leaseDir := sttMkdir(t, filepath.Join(leasedState, "environments", "gen", ".leases"))
	f := sttHold(t, filepath.Join(leaseDir, "reader"))
	for _, state := range []string{idle, leasedState, present} {
		sttAge(t, state, 30)
	}
	removed, err := CollectOrphanInstallStates(installs)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	sttSamePaths(t, removed, []string{idle})
	for _, p := range []string{fresh, leasedState, present} {
		if !sttIsDir(p) {
			t.Fatalf("%s was reclaimed", p)
		}
	}
	if sttExists(idle) {
		t.Fatal("idle state kept")
	}
}

func TestRetentionDaysFromConfigYAMLDecidesReclaim(t *testing.T) {
	for _, tc := range []struct {
		name      string
		days      *int
		reclaimed bool
	}{{"0-False", sttPtr(0), false}, {"1-True", sttPtr(1), true}, {"None-False", nil, false}} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := isolateHome(t)
			if tc.days != nil {
				sttRetention(t, sttMkdir(t, filepath.Join(tmp, "home")), *tc.days)
			}
			installs := filepath.Join(tmp, "home", "installs")
			state := sttState(t, installs, "aaaa", filepath.Join(tmp, "review", "gone"))
			sttAge(t, state, 2)
			removed, err := CollectOrphanInstallStates(installs)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(removed) == 1 && removed[0] == state; got != tc.reclaimed {
				t.Fatalf("removed %q, reclaimed want %v", removed, tc.reclaimed)
			}
			if sttExists(state) == tc.reclaimed {
				t.Fatalf("state exists %v with reclaimed %v", sttExists(state), tc.reclaimed)
			}
		})
	}
}

// Pointer to a value
func sttPtr[T any](v T) *T { return &v }

func TestRetentionComesFromTheStoreOwnerNotTheActiveProfile(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		rootDays, profileDays *int
		reclaimed             bool
	}{{"0-None-False", sttPtr(0), nil, false}, {"None-0-True", nil, sttPtr(0), true}} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := isolateHome(t)
			t.Setenv(home.EnvSuffix, "")
			root := filepath.Join(tmp, ".icebath")
			named := sttMkdir(t, filepath.Join(root, "profiles", "work"))
			if tc.rootDays != nil {
				sttRetention(t, root, *tc.rootDays)
			}
			if tc.profileDays != nil {
				sttRetention(t, named, *tc.profileDays)
			}
			ctx := home.WithOverride(context.Background(), named)
			installs := InstallsRoot(ctx)
			if installs != filepath.Join(root, "installs") {
				t.Fatalf("installs %s", installs)
			}
			state := sttState(t, installs, "aaaa", filepath.Join(tmp, "unmounted", "checkout"))
			sttAge(t, state, 30)
			removed, err := CollectOrphanInstallStates(installs)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(removed) == 1 && removed[0] == state; got != tc.reclaimed {
				t.Fatalf("removed %q, reclaimed want %v", removed, tc.reclaimed)
			}
			if sttExists(state) == tc.reclaimed {
				t.Fatalf("state exists %v with reclaimed %v", sttExists(state), tc.reclaimed)
			}
		})
	}
}

func TestSymlinkedInstallsReadsRetentionFromTheHomeNotTheLinkTarget(t *testing.T) {
	tmp := isolateHome(t)
	root := sttMkdir(t, filepath.Join(tmp, "home"))
	sttRetention(t, root, 0)
	target := sttMkdir(t, filepath.Join(tmp, "bigdisk", "installs"))
	if err := os.Symlink(target, filepath.Join(root, "installs")); err != nil {
		t.Fatal(err)
	}
	state := sttState(t, filepath.Join(root, "installs"), "aaaa", filepath.Join(tmp, "unmounted", "checkout"))
	sttAge(t, state, 30)
	removed, err := CollectOrphanInstallStates(filepath.Join(root, "installs"))
	if err != nil {
		t.Fatal(err)
	}
	sttSamePaths(t, removed, nil)
	if !sttExists(state) {
		t.Fatal("opted out state was reclaimed")
	}
	sttSamePaths(t, sttChildren(t, filepath.Dir(target)), []string{target})
}

func TestStateUsedWhileAnotherOrphanIsRemovedIsKept(t *testing.T) {
	tmp := isolateHome(t)
	installs := filepath.Join(tmp, "home", "installs")
	deleted := sttState(t, installs, "aaaa", filepath.Join(tmp, "home", "cache", "scratch", "gone"))
	used := sttState(t, installs, "bbbb", filepath.Join(tmp, "unmounted", "checkout"))
	sttTouch(t, filepath.Join(used, "environments", "gen", ".lease-managed"))
	sttAge(t, used, 30)
	realRemove := removeStateTree
	t.Cleanup(func() { removeStateTree = realRemove })
	var leaseErr error
	removeStateTree = func(path string) {
		if path == filepath.Join(deleted, "environments") {
			release, err := LeaseDirectory(filepath.Join(used, "environments", "gen"))
			if err != nil {
				leaseErr = err
			} else {
				release()
			}
		}
		realRemove(path)
	}
	removed, err := CollectOrphanInstallStates(installs)
	if err != nil || leaseErr != nil {
		t.Fatal(err, leaseErr)
	}
	sttSamePaths(t, removed, []string{deleted})
	if !sttIsDir(filepath.Join(used, "environments", "gen", "venv")) {
		t.Fatal("state used mid collection was reclaimed")
	}
}
