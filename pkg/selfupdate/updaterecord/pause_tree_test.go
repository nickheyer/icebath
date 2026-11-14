package updaterecord

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/pm"
)

func ptGitBinary(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("the tree gate tests run the git binary: %v", err)
	}
	return git
}

func ptGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command(ptGitBinary(t), append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("git %v: %v\n%s", args, err, exit.Stderr)
		}
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func ptWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ptInstall(t *testing.T, root string) {
	saved := pauseInstall
	pauseInstall = func() string { return root }
	t.Cleanup(func() { pauseInstall = saved })
}

func ptCheckout(t *testing.T, base string, files ...string) string {
	t.Helper()
	root := filepath.Join(base, "checkout")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	ptGit(t, root, "init", "-q")
	for _, name := range files {
		ptWrite(t, filepath.Join(root, name), "v1\n")
	}
	ptGit(t, root, "add", ".")
	ptGit(t, root, "commit", "-qm", "v1")
	return root
}

func ptStamp(t *testing.T, token map[string]any, root string) map[string]any {
	t.Helper()
	stamped, err := StampTree(context.Background(), token, root)
	if err != nil {
		t.Fatal(err)
	}
	return stamped
}

func ptWhole(t *testing.T, token map[string]any, root string) (bool, string) {
	t.Helper()
	whole, why, err := TreeIsWhole(context.Background(), token, root)
	if err != nil {
		t.Fatal(err)
	}
	return whole, why
}

func ptSaved(t *testing.T) map[string]any {
	t.Helper()
	body, err := ReadPauseRecord(PauseRecordPath())
	if err != nil || body == nil {
		t.Fatalf("pause record unreadable: %v", err)
	}
	return body["token"].(map[string]any)
}

func TestPauseTreeResumeWaitsForAWholeTree(t *testing.T) {
	root := ptCheckout(t, t.TempDir(), "a.py", "b.py", "local.txt")
	ptWrite(t, filepath.Join(root, "local.txt"), "user edit\n")
	token := ptStamp(t, map[string]any{"resume_needed": true}, root)
	if whole, why := ptWhole(t, token, root); !whole || why != "" {
		t.Fatalf("a pre update edit held the set: %v %q", whole, why)
	}
	ptWrite(t, filepath.Join(root, "a.py"), "v2\n")
	if whole, why := ptWhole(t, token, root); whole || !strings.Contains(why, "a.py") {
		t.Fatalf("a half written a.py passed: %v %q", whole, why)
	}
	ptWrite(t, filepath.Join(root, "a.py"), "v1\n")
	marker := filepath.Join(root, ".git", pm.InterruptedPullMarker)
	ptWrite(t, marker, "pid\n")
	if whole, _ := ptWhole(t, token, root); whole {
		t.Fatal("an interrupted pull marker passed")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	ptGit(t, root, "commit", "-qam", "v2")
	ptWrite(t, filepath.Join(root, "b.py"), "rewritten by a build step\n")
	if whole, why := ptWhole(t, token, root); !whole || why != "" {
		t.Fatalf("a committed update held the set for a build rewrite: %v %q", whole, why)
	}
}

func TestPauseTreeASetPausedOnAnAlreadyUnreadableHeadResumesUntilAMoveIsBound(t *testing.T) {
	root := ptCheckout(t, t.TempDir(), "a.py")
	readable := ptStamp(t, map[string]any{"resume_needed": true}, root)
	if err := os.Remove(filepath.Join(root, ".git", "HEAD")); err != nil {
		t.Fatal(err)
	}
	token := ptStamp(t, map[string]any{"resume_needed": true}, root)
	if whole, why := ptWhole(t, token, root); !whole || why != "" {
		t.Fatalf("unreadable at pause held: %v %q", whole, why)
	}
	token["baselines"].([]any)[0].(map[string]any)["move_targets"] = []any{strings.Repeat("0", 40)}
	if whole, why := ptWhole(t, token, root); whole || why != "the checkout HEAD is unreadable" {
		t.Fatalf("a bound move passed: %v %q", whole, why)
	}
	if whole, why := ptWhole(t, readable, root); whole || why != "the checkout HEAD is unreadable" {
		t.Fatalf("readable at pause passed: %v %q", whole, why)
	}
}

func TestAnUpdateAdoptingAnOrphanNeverCertifiesTheTreeItLeftTorn(t *testing.T) {
	base := t.TempDir()
	root := ptCheckout(t, base, "a.py")
	t.Setenv("ICEBATH_HOME", filepath.Join(base, "home"))
	ptInstall(t, root)
	ctx := context.Background()
	orphan := ptStamp(t, map[string]any{"resume_needed": true, "profiles": map[string]any{"default": 4242}}, "")
	if err := WritePause(ctx, orphan, PauseUnowned(), ""); err != nil {
		t.Fatal(err)
	}
	ptWrite(t, filepath.Join(root, "a.py"), "half-written\n")
	if whole, _ := ptWhole(t, ptSaved(t), root); whole {
		t.Fatal("premise: the orphan's gate refuses")
	}
	adopted, claims, err := AdoptPauseOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := RecordPause(ctx, map[string]any{"resume_needed": true, "profiles": map[string]any{"beta": 99}}, adopted, claims)
	if err != nil {
		t.Fatal(err)
	}
	if whole, why := ptWhole(t, token, root); whole || !strings.Contains(why, "a.py") {
		t.Fatalf("adoption certified the torn tree as the pre-update baseline: %v %q", whole, why)
	}
}

func TestAnAdoptedBaselineNeverReplacesThisRunsOwn(t *testing.T) {
	base := t.TempDir()
	root := ptCheckout(t, base, "a.py")
	t.Setenv("ICEBATH_HOME", filepath.Join(base, "home"))
	ptInstall(t, root)
	ctx := context.Background()
	orphan := ptStamp(t, map[string]any{"resume_needed": true, "profiles": map[string]any{"default": 4242}}, "")
	if err := WritePause(ctx, orphan, PauseUnowned(), ""); err != nil {
		t.Fatal(err)
	}
	ptWrite(t, filepath.Join(root, "a.py"), "v2\n")
	ptGit(t, root, "commit", "-qam", "Y")
	saved := pauseDepsHoldResume
	pauseDepsHoldResume = func(context.Context, string) bool { return false }
	t.Cleanup(func() { pauseDepsHoldResume = saved })
	adopted, claims, err := AdoptPauseOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := RecordPause(ctx, map[string]any{"resume_needed": true, "profiles": map[string]any{"beta": 99}}, adopted, claims)
	if err != nil {
		t.Fatal(err)
	}
	if whole, why := ptWhole(t, token, root); !whole || why != "" {
		t.Fatalf("a clean tree at this run's own HEAD must pass: %v %q", whole, why)
	}
	ptWrite(t, filepath.Join(root, "a.py"), "half-written\n")
	if whole, why := ptWhole(t, ptSaved(t), root); whole || !strings.Contains(why, "a.py") {
		t.Fatalf("this run's own baseline was dropped for the adopted one: %v %q", whole, why)
	}
}

func TestPauseTreeTheTreeGateFindsPMGitAndRunsItInTheUpdatersCustody(t *testing.T) {
	base := t.TempDir()
	root := ptCheckout(t, base, "a.py")
	head, pmGit := ptGit(t, root, "rev-parse", "HEAD"), ptGitBinary(t)
	var ran []string
	savedRun, savedInstalled := pauseRunGit, pauseInstalledPackage
	pauseRunGit = func(ctx context.Context, gitCmd, args []string, timeout time.Duration) (*GitResult, error) {
		ran = append(ran, gitCmd[0])
		return RunGitQuery(ctx, gitCmd, args, timeout)
	}
	pauseInstalledPackage = func(ctx context.Context, name string, allowOutdated bool) (*pm.InstalledPackage, error) {
		if name == "git" {
			return &pm.InstalledPackage{Binary: pmGit}, nil
		}
		return nil, nil
	}
	t.Cleanup(func() { pauseRunGit, pauseInstalledPackage = savedRun, savedInstalled })
	noGit := filepath.Join(base, "no-git")
	if err := os.Mkdir(noGit, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", noGit)
	if got := HeadSHA(context.Background(), root); got != head {
		t.Fatalf("head = %q, want %q", got, head)
	}
	if !reflect.DeepEqual(ran, []string{pmGit}) {
		t.Fatalf("the gate's git bypassed the custody runner: %v", ran)
	}
}

func TestPauseTreeAMovedHeadHoldsGatewaysForDependenciesOnlyWhereALaunchSyncsThem(t *testing.T) {
	cases := []struct {
		name      string
		mechanism string
		probe     func() (bool, error)
		holds     bool
	}{
		{"self_stale", "self", func() (bool, error) { return false, nil }, true},
		{"self_current", "self", func() (bool, error) { return true, nil }, false},
		{"self_unknown", "self", func() (bool, error) { return false, errors.New("invalid recorded dependency state") }, false},
		{"unmanaged", "", func() (bool, error) { return false, nil }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			ptGit(t, root, "init", "-q")
			ptWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname = 'x'\n")
			ptGit(t, root, "add", ".")
			ptGit(t, root, "commit", "-qm", "v1")
			token := ptStamp(t, map[string]any{"resume_needed": true}, root)
			ptGit(t, root, "commit", "-q", "--allow-empty", "-m", "v2")
			t.Setenv("ICEBATH_DISABLE_LAZY_INSTALLS", "")
			savedStamp, savedProbe := pauseReadInstallStamp, pauseVenvIsCurrent
			pauseReadInstallStamp = func(string) *pyjson.Object {
				if tc.mechanism == "" {
					return pyjson.NewObject()
				}
				return pyjson.ObjectOf("updateMechanism", tc.mechanism)
			}
			pauseVenvIsCurrent = func(context.Context, pm.VenvIsCurrentOptions) (bool, error) { return tc.probe() }
			t.Cleanup(func() { pauseReadInstallStamp, pauseVenvIsCurrent = savedStamp, savedProbe })
			whole, why := ptWhole(t, token, root)
			if whole == tc.holds {
				t.Fatalf("whole = %v, %q", whole, why)
			}
			if tc.holds && !strings.Contains(why, "dependencies") {
				t.Fatalf("reason %q", why)
			}
		})
	}
}

func TestPauseTreeAnUnmovedHeadHoldsGatewaysOnlyForPathsTheUpdatesMoveCouldWrite(t *testing.T) {
	base := t.TempDir()
	origin, root := filepath.Join(base, "origin"), filepath.Join(base, "checkout")
	if err := os.Mkdir(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	ptGit(t, origin, "init", "-q")
	for _, name := range []string{"a.py", "b.lock"} {
		ptWrite(t, filepath.Join(origin, name), "v1\n")
	}
	ptGit(t, origin, "add", ".")
	ptGit(t, origin, "commit", "-qm", "v1")
	ptGit(t, base, "clone", "-q", origin, root)
	ptWrite(t, filepath.Join(origin, "a.py"), "v2\n")
	ptGit(t, origin, "commit", "-qam", "v2")
	token := ptStamp(t, map[string]any{"resume_needed": true}, root)
	ptGit(t, root, "fetch", "-q", "origin")
	ptWrite(t, filepath.Join(root, "b.lock"), "rewritten by the dependency sync\n")
	if whole, why := ptWhole(t, token, root); !whole || why != "" {
		t.Fatalf("a build rewrite held the set: %v %q", whole, why)
	}
	ptWrite(t, filepath.Join(root, "a.py"), "v2 half\n")
	if whole, why := ptWhole(t, token, root); whole || !strings.Contains(why, "a.py") {
		t.Fatalf("a torn path of the move passed: %v %q", whole, why)
	}
	ptGit(t, root, "checkout", "-q", "--", "a.py", "b.lock")
	ptGit(t, root, "merge", "-q", "--ff-only", "origin/HEAD")
	token = ptStamp(t, map[string]any{"resume_needed": true}, root)
	ptWrite(t, filepath.Join(root, "b.lock"), "rewritten by the dependency sync\n")
	if whole, why := ptWhole(t, token, root); !whole || why != "" {
		t.Fatalf("a no-op update's build held the set: %v %q", whole, why)
	}
}

func ptClonedCheckout(t *testing.T, base string) (string, string) {
	t.Helper()
	origin, root := filepath.Join(base, "origin"), filepath.Join(base, "checkout")
	if err := os.Mkdir(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	ptGit(t, origin, "init", "-q")
	for _, name := range []string{"a.py", "b.py"} {
		ptWrite(t, filepath.Join(origin, name), "v1\n")
	}
	ptGit(t, origin, "add", ".")
	ptGit(t, origin, "commit", "-qm", "X")
	ptGit(t, base, "clone", "-q", origin, root)
	ptWrite(t, filepath.Join(origin, "a.py"), "v2\n")
	ptGit(t, origin, "commit", "-qam", "B")
	return origin, root
}

func TestALaterFetchNeverCertifiesBytesAnEarlierRecordedMoveLeft(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(base, "home"))
	origin, root := ptClonedCheckout(t, base)
	token := ptStamp(t, map[string]any{"resume_needed": true, "profiles": map[string]any{"default": 4242}}, root)
	ptGit(t, root, "fetch", "-q", "origin")
	if err := MarkMove(context.Background(), token, ptGit(t, root, "rev-parse", "origin/HEAD")); err != nil {
		t.Fatal(err)
	}
	ptWrite(t, filepath.Join(root, "a.py"), "v2 half\n")
	if whole, _ := ptWhole(t, token, root); whole {
		t.Fatal("premise: the torn path holds the set")
	}
	ptWrite(t, filepath.Join(origin, "a.py"), "v1\n")
	ptWrite(t, filepath.Join(origin, "b.py"), "v2\n")
	ptGit(t, origin, "commit", "-qam", "C")
	ptGit(t, root, "fetch", "-q", "origin")
	for _, judged := range []map[string]any{token, ptSaved(t)} {
		if whole, why := ptWhole(t, judged, root); whole || !strings.Contains(why, "a.py") {
			t.Fatalf("a later fetch certified the earlier move's partial bytes: %v %q", whole, why)
		}
	}
}

func TestPauseTreeAnAutostashedEditVouchesOnlyForItsOwnBytes(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(base, "home"))
	_, root := ptClonedCheckout(t, base)
	ptWrite(t, filepath.Join(root, "a.py"), "user edit\n")
	token := ptStamp(t, map[string]any{"resume_needed": true, "profiles": map[string]any{"default": 4242}}, root)
	ptGit(t, root, "stash", "-q")
	ptGit(t, root, "fetch", "-q", "origin")
	if err := MarkMove(context.Background(), token, ptGit(t, root, "rev-parse", "origin/HEAD")); err != nil {
		t.Fatal(err)
	}
	ptWrite(t, filepath.Join(root, "a.py"), "v2 half\n")
	if whole, why := ptWhole(t, token, root); whole || !strings.Contains(why, "a.py") {
		t.Fatalf("the dirty pathname admitted git's half-written bytes: %v %q", whole, why)
	}
	ptGit(t, root, "checkout", "-q", "--", "a.py")
	ptGit(t, root, "stash", "pop", "-q")
	if whole, why := ptWhole(t, token, root); !whole || why != "" {
		t.Fatalf("the restored edit held the set: %v %q", whole, why)
	}
}
