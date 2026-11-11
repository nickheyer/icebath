package selfupdate

import (
	"bytes"
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// A fresh commit point over this test's own home and host state directory, returning an empty install root
func uoFresh(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(state, "home"))
	t.Setenv("ICEBATH_GATEWAY_LOCK_DIR", filepath.Join(state, "gateway-locks"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(state, "xdg"))
	root := filepath.Join(state, "install")
	for _, dir := range []string{filepath.Join(state, "home"), root} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	beginUpdateAttempt()
	t.Cleanup(beginUpdateAttempt)
	return root
}

// Host obligation path with its directory made and prior content written
func uoHost(t *testing.T, prior string) string {
	t.Helper()
	host := updaterecord.HostObligationPath()
	ugWrite(t, host, prior)
	return host
}

func uoArm(t *testing.T, root, sha string) {
	t.Helper()
	if err := armCommitObligations(context.Background(), root, sha); err != nil {
		t.Fatalf("arm %s: %v", sha[:4], err)
	}
}

func uoDisarm(t *testing.T) {
	t.Helper()
	if err := disarmCommitObligations(context.Background()); err != nil {
		t.Fatalf("disarm: %v", err)
	}
}

// The expected_sha and owners of the host obligation record, empty when there is none
func uoRecord() (string, []any) {
	record := updaterecord.ReadHostObligation()
	if record == nil {
		return "", nil
	}
	sha, _ := record.Vals["expected_sha"].(string)
	owners, _ := record.Vals["owners"].([]any)
	return sha, owners
}

// The per attempt state of one update run
type uoRun struct {
	snapshot []armedRecord
	owner    string
	bytes    map[string]armedRecord
}

func uoRunState() uoRun {
	return uoRun{snapshot: slices.Clone(armedSnapshot), owner: commitOwner, bytes: maps.Clone(armedBytes)}
}

func uoEnterRun(state uoRun) {
	armedSnapshot, commitOwner = state.snapshot, state.owner
	clear(armedBytes)
	maps.Copy(armedBytes, state.bytes)
}

func TestArmRefusesWhenNoStoreAcceptsTheFleetRestart(t *testing.T) {
	root := uoFresh(t)
	for _, dir := range []string{updaterecord.HostObligationPath(), fleetRestartPendingMarkerPath(context.Background())} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := armCommitObligations(context.Background(), root, strings.Repeat("b", 40)); err == nil || !isUpdateOSError(err) {
		t.Fatalf("err = %v, want an OSError", err)
	}
}

func TestDisarmLeavesAnotherInstallsNewerHostRecord(t *testing.T) {
	root := uoFresh(t)
	host := uoHost(t, "OLD")
	uoArm(t, root, strings.Repeat("a", 40))
	ugWrite(t, host, "OTHER-INSTALL")

	uoDisarm(t)

	if got := ugRead(t, host); got != "OTHER-INSTALL" {
		t.Fatalf("host record = %q", got)
	}
}

func TestPlainArmAndDisarmRestoresWhatTheRunFound(t *testing.T) {
	root := uoFresh(t)
	host := uoHost(t, "OLD")
	uoArm(t, root, strings.Repeat("a", 40))
	if got := ugRead(t, host); got == "OLD" {
		t.Fatal("the arm left the host record as it found it")
	}

	uoDisarm(t)

	if got := ugRead(t, host); got != "OLD" {
		t.Fatalf("host record = %q", got)
	}
}

func TestAnUnreadableRecordRefusesTheArmAndSurvives(t *testing.T) {
	root := uoFresh(t)
	host := uoHost(t, "OLD")
	real := obligationReadFile
	obligationReadFile = func(path string) ([]byte, error) {
		if path == host {
			return nil, &fs.PathError{Op: "open", Path: path, Err: syscall.EACCES}
		}
		return real(path)
	}
	t.Cleanup(func() { obligationReadFile = real })
	if err := armCommitObligations(context.Background(), root, strings.Repeat("a", 40)); err == nil || !isUpdateOSError(err) {
		t.Fatalf("err = %v, want an OSError", err)
	}
	obligationReadFile = real
	uoDisarm(t)

	if got := ugRead(t, host); got != "OLD" {
		t.Fatalf("host record = %q", got)
	}
}

func TestARefusedPullAfterTheBranchSwitchOwesTheHeadTheSwitchLandedOn(t *testing.T) {
	uoFresh(t)
	ugEnv(t)
	tmp := t.TempDir()
	up, clone := filepath.Join(tmp, "up"), filepath.Join(tmp, "clone")
	if err := os.Mkdir(up, 0o755); err != nil {
		t.Fatal(err)
	}
	ugGit(t, up, "init", "-q", "-b", "main")
	ugWrite(t, filepath.Join(up, "f"), "A")
	ugGit(t, up, "add", "-A")
	ugGit(t, up, "commit", "-qm", "A")
	a := strings.TrimSpace(ugGit(t, up, "rev-parse", "HEAD"))
	ugGit(t, tmp, "clone", "-q", up, clone)
	ugGit(t, clone, "checkout", "-q", "-b", "feat")
	ugWrite(t, filepath.Join(clone, "g"), "X")
	ugGit(t, clone, "add", "-A")
	ugGit(t, clone, "commit", "-qm", "X")
	x := strings.TrimSpace(ugGit(t, clone, "rev-parse", "HEAD"))
	ugWrite(t, filepath.Join(up, "f"), "B")
	ugGit(t, up, "commit", "-qam", "B")
	b := strings.TrimSpace(ugGit(t, up, "rev-parse", "HEAD"))
	ugGit(t, clone, "fetch", "-q", "origin")
	ucPatchRoot(t, clone)
	t.Chdir(clone)
	ctx := context.Background()

	if err := recordRunStart(ctx, []string{"git"}, clone); err != nil {
		t.Fatal(err)
	}
	switched, err := switchBranchAtCommitPoint(ctx, &bytes.Buffer{}, []string{"git"}, "main", "origin/main", x, "")
	if err != nil {
		t.Fatal(err)
	}
	if head := strings.TrimSpace(ugGit(t, clone, "rev-parse", "HEAD")); switched.ReturnCode != 0 || head != a {
		t.Fatalf("switch exit %d, HEAD %s, want %s", switched.ReturnCode, head, a)
	}
	marker, err := pm.InterruptedPullMarkerPath(clone)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	reason, err := armCommitPoint(ctx, []string{"git"}, clone, b, treeMove{Pre: a, Target: b})
	if err != nil {
		t.Fatal(err)
	}

	if reason == "" || strings.Contains(reason, "the checkout was not changed") || !strings.Contains(reason, a[:10]) {
		t.Fatalf("reason = %q", reason)
	}
	if sha, _ := uoRecord(); sha != a {
		t.Fatalf("the debt names %q, want %s", sha, a)
	}
}

func TestOneInstallsDisarmNeverDeletesAnotherInstallsDebtForTheSameSha(t *testing.T) {
	root := uoFresh(t)
	other := filepath.Join(t.TempDir(), "other-install")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	uoArm(t, root, sha)
	runA := uoRunState()
	beginUpdateAttempt()
	uoArm(t, other, sha)
	runB := uoRunState()

	uoEnterRun(runA)
	uoDisarm(t)
	if owed, _ := uoRecord(); owed != sha {
		t.Fatalf("the debt names %q after the first install left", owed)
	}

	uoEnterRun(runB)
	uoDisarm(t)
	if statExists(updaterecord.HostObligationPath()) {
		t.Fatal("the last owner left a record the first one never found")
	}
}

// Upstream commits A, B and C and a clone of it the update is pointed at
func uoABCClone(t *testing.T) (clone, a, b, c string) {
	t.Helper()
	ugEnv(t)
	tmp := t.TempDir()
	up := filepath.Join(tmp, "up")
	clone = filepath.Join(tmp, "clone")
	if err := os.Mkdir(up, 0o755); err != nil {
		t.Fatal(err)
	}
	ugGit(t, up, "init", "-q", "-b", "main")
	var shas []string
	for _, name := range []string{"A", "B", "C"} {
		ugWrite(t, filepath.Join(up, "f"), name)
		ugGit(t, up, "add", "-A")
		ugGit(t, up, "commit", "-qm", name)
		shas = append(shas, strings.TrimSpace(ugGit(t, up, "rev-parse", "HEAD")))
	}
	ugGit(t, tmp, "clone", "-q", up, clone)
	ucPatchRoot(t, clone)
	t.Chdir(clone)
	return clone, shas[0], shas[1], shas[2]
}

// Moves ref to sha right after the commit point armed its marker and debt
func uoMoveRefAfterArm(t *testing.T, clone, ref, sha string) {
	t.Helper()
	real := armCommitPoint
	armCommitPoint = func(ctx context.Context, gitCmd []string, root, expectedSHA string, move treeMove) (string, error) {
		refused, err := real(ctx, gitCmd, root, expectedSHA, move)
		ugGit(t, clone, "update-ref", ref, sha)
		return refused, err
	}
	t.Cleanup(func() { armCommitPoint = real })
}

func TestThePullMergesTheArmedCommitNotATrackingRefThatMoved(t *testing.T) {
	uoFresh(t)
	clone, a, b, c := uoABCClone(t)
	ugGit(t, clone, "reset", "-q", "--hard", a)
	ugGit(t, clone, "update-ref", "refs/remotes/origin/main", b)
	ctx := context.Background()
	if err := recordRunStart(ctx, []string{"git"}, clone); err != nil {
		t.Fatal(err)
	}
	uoMoveRefAfterArm(t, clone, "refs/remotes/origin/main", c)
	if _, err := pullUpdates(ctx, &bytes.Buffer{}, []string{"git"}, "main", "", pullOptions{}); err != nil {
		t.Fatal(err)
	}
	if head := strings.TrimSpace(ugGit(t, clone, "rev-parse", "HEAD")); head != b {
		t.Fatalf("HEAD = %s, want %s", head, b)
	}
	if sha, _ := uoRecord(); sha != b {
		t.Fatalf("the debt names %q, want %s", sha, b)
	}
	if marker, err := pm.InterruptedPullMarkerPath(clone); err != nil || lexists(marker) {
		t.Fatalf("marker left: %v", err)
	}
}

func TestABranchThatMovesDuringTheSwitchLeavesHeadAndDebtAgreeing(t *testing.T) {
	uoFresh(t)
	clone, a, b, _ := uoABCClone(t)
	ugGit(t, clone, "reset", "-q", "--hard", a)
	ugGit(t, clone, "checkout", "-q", "-b", "feat")
	ugWrite(t, filepath.Join(clone, "g"), "X")
	ugGit(t, clone, "add", "-A")
	ugGit(t, clone, "commit", "-qm", "X")
	x := strings.TrimSpace(ugGit(t, clone, "rev-parse", "HEAD"))
	ctx := context.Background()
	if err := recordRunStart(ctx, []string{"git"}, clone); err != nil {
		t.Fatal(err)
	}
	uoMoveRefAfterArm(t, clone, "refs/heads/main", b)
	switched, err := switchBranchAtCommitPoint(ctx, &bytes.Buffer{}, []string{"git"}, "main", "origin/main", x, "")
	if err != nil {
		t.Fatal(err)
	}
	if switched.ReturnCode != 1 || !strings.Contains(switched.Stderr, "moved") {
		t.Fatalf("switch exit %d, stderr %q", switched.ReturnCode, switched.Stderr)
	}
	if head := strings.TrimSpace(ugGit(t, clone, "rev-parse", "HEAD")); head != b {
		t.Fatalf("HEAD = %s, want %s", head, b)
	}
	if sha, _ := uoRecord(); sha != b {
		t.Fatalf("the debt names %q, want %s", sha, b)
	}
	if marker, err := pm.InterruptedPullMarkerPath(clone); err != nil || lexists(marker) {
		t.Fatalf("marker left: %v", err)
	}
}

func TestARetargetWithinOneRunKeepsTheHostRecordsSavedBaseline(t *testing.T) {
	uoFresh(t)
	ugEnv(t)
	host := uoHost(t, "PRIOR-DEBT")
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	ugGit(t, repo, "init", "-q", "-b", "main")
	ugWrite(t, filepath.Join(repo, "f"), "A")
	ugGit(t, repo, "add", "-A")
	ugGit(t, repo, "commit", "-qm", "A")
	if err := recordRunStart(context.Background(), []string{"git"}, repo); err != nil {
		t.Fatal(err)
	}

	uoArm(t, repo, strings.Repeat("b", 40))
	uoArm(t, repo, strings.Repeat("c", 40))
	uoDisarm(t)

	if got := ugRead(t, host); got != "PRIOR-DEBT" {
		t.Fatalf("host record = %q", got)
	}
}

func TestALaterRunsFailedRetargetPutsBackTheEarlierRunsDebt(t *testing.T) {
	root := uoFresh(t)
	host := uoHost(t, "PRIOR-DEBT")
	uoArm(t, root, strings.Repeat("b", 40))
	runOneRecord := ugRead(t, host)
	beginUpdateAttempt()

	uoArm(t, root, strings.Repeat("c", 40))
	uoDisarm(t)

	if got := ugRead(t, host); got != runOneRecord {
		t.Fatalf("host record = %q, want run one's %q", got, runOneRecord)
	}
}

func TestAnOwnerLeavingASharedRecordForAnotherTargetHandsTheOthersStakeBack(t *testing.T) {
	root := uoFresh(t)
	host := uoHost(t, "PRIOR-DEBT")
	other := filepath.Join(t.TempDir(), "other-install")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	uoArm(t, other, sha)
	runY := uoRunState()
	beginUpdateAttempt()
	uoArm(t, root, sha)
	uoArm(t, root, strings.Repeat("d", 40))
	uoDisarm(t)

	owed, owners := uoRecord()
	if owed != sha || len(owners) != 1 || owners[0] != runY.owner {
		t.Fatalf("record names %q owned by %v, want %s owned by %q", owed, owners, sha, runY.owner)
	}
	uoEnterRun(runY)
	uoDisarm(t)
	if got := ugRead(t, host); got != "PRIOR-DEBT" {
		t.Fatalf("host record = %q", got)
	}
}

func TestASecondUpdateInOneProcessNeverHandsBackTheFirstUpdatesDebt(t *testing.T) {
	root := uoFresh(t)
	ctx := context.Background()
	uoArm(t, root, strings.Repeat("b", 40))
	tail, host := CompletionPendingPath(ctx, root), updaterecord.HostObligationPath()
	owed := map[string]string{tail: ugRead(t, tail), host: ugRead(t, host)}

	merging := filepath.Join(t.TempDir(), "checkout")
	ugWrite(t, filepath.Join(merging, ".git", "MERGE_HEAD"), strings.Repeat("0", 40)+"\n")
	ucPatchRoot(t, merging)
	if err := cmdUpdateImpl(ctx, &bytes.Buffer{}, map[string]any{}, false); !isUpdateExit(err) {
		t.Fatalf("the second update's entry did not stop at the merge in progress: %v", err)
	}
	uoArm(t, root, strings.Repeat("c", 40))
	uoDisarm(t)

	for path, want := range owed {
		if got := ugRead(t, path); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
}
