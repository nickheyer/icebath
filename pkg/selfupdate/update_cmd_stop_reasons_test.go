package selfupdate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Writes the update marker of the fixture home, one value per line
func ucMarker(t *testing.T, lines ...any) {
	t.Helper()
	marker := updaterecord.UpdateMarkerPath()
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, line := range lines {
		fmt.Fprintf(&text, "%v\n", line)
	}
	if err := os.WriteFile(marker, []byte(text.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAnExitBeforeTheReceiptReadsDesktopOnlyUnderItsOwnHandOff(t *testing.T) {
	ctx, _, runs := ucMetricRows(t)
	ucUnsetenv(t, "ICEBATH_UPDATE_HANDOFF_PID")
	pid, other := os.Getpid(), 999_999
	ucMarker(t, other, 1, "ct:1.000", fmt.Sprintf("delegate:%d ct:2.000", pid))
	recordStopWithoutReceipt(ctx, "lock_held", "refused")
	t.Setenv("ICEBATH_UPDATE_HANDOFF_PID", fmt.Sprint(other))
	ucMarker(t, other, 1, "ct:1.000")
	recordStopWithoutReceipt(ctx, "git_in_progress", "failed")
	os.Unsetenv("ICEBATH_UPDATE_HANDOFF_PID")
	recordStopWithoutReceipt(ctx, "lock_held", "refused")
	if err := os.Remove(updaterecord.UpdateMarkerPath()); err != nil {
		t.Fatal(err)
	}
	recordStopWithoutReceipt(ctx, "managed_install", "refused")
	var got [][2]string
	for _, row := range runs() {
		got = append(got, [2]string{row["failure_class"], row["kind"]})
	}
	want := [][2]string{{"lock_held", "desktop"}, {"git_in_progress", "desktop"}, {"lock_held", "cli"}, {"managed_install", "cli"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}

func TestALockRefusalOfTheHandOffsOwnUpdateChildIsAFailedRun(t *testing.T) {
	ctx, _, runs := ucMetricRows(t)
	ucUnsetenv(t, "ICEBATH_UPDATE_HANDOFF_PID")
	pid, other := os.Getpid(), 999_999
	ucMarker(t, other, 1, "ct:1.000", fmt.Sprintf("delegate:%d ct:2.000", pid))
	recordStopWithoutReceipt(ctx, "lock_held", "refused")
	ucMarker(t, other, 1, "ct:1.000", fmt.Sprintf("delegate:%d ct:2.000", os.Getppid()))
	recordStopWithoutReceipt(ctx, "lock_held", "refused")
	t.Setenv("ICEBATH_UPDATE_HANDOFF_PID", fmt.Sprint(other))
	ucMarker(t, other, 1, "ct:1.000")
	recordStopWithoutReceipt(ctx, "lock_held", "refused")
	os.Unsetenv("ICEBATH_UPDATE_HANDOFF_PID")
	recordStopWithoutReceipt(ctx, "lock_held", "refused")
	var got [][3]string
	for _, row := range runs() {
		got = append(got, [3]string{row["outcome"], row["kind"], row["failure_class"]})
		dimensions := map[string]any{}
		for key, value := range row {
			dimensions[key] = value
		}
		if !observability.CounterDimensionsAreValid(observability.UpdateRunMetric, dimensions) {
			t.Fatalf("row %v carries dimensions outside the contract", row)
		}
	}
	want := [][3]string{{"failed", "desktop", "lock_held"}, {"failed", "desktop", "lock_held"}, {"refused", "desktop", "lock_held"},
		{"refused", "cli", "lock_held"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
}

func TestACheckoutMoveNamesAHeldIndexLockApartFromAPermissionError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX permissions; root ignores them")
	}
	ctx, _, _ := ucMetricRows(t)
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	ucPatchRoot(t, repo)
	cmd := []string{"git", "-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}
	ucGit(t, repo, "init", "-q")
	os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o644)
	ucGit(t, repo, "add", "a")
	ucGit(t, repo, "commit", "-qm", "one")
	old := ucGit(t, repo, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(repo, "a"), []byte("b"), 0o644)
	ucGit(t, repo, "commit", "-qam", "two")
	target := ucGit(t, repo, "rev-parse", "HEAD")
	ucGit(t, repo, "reset", "-q", "--hard", old)
	move := func() string {
		t.Helper()
		updaterecord.BeginUpdateReceipt(ctx, nil, "")
		var out bytes.Buffer
		err := moveCheckoutTo(ctx, &out, cmd, "main", "origin/main", target, old)
		var exit *pm.SystemExit
		if !errors.As(err, &exit) {
			t.Fatalf("the move did not exit: %v\n%s", err, out.String())
		}
		code := 1
		path := updaterecord.FinalizePendingUpdateReceipt(ctx, &code, "sys.exit(1)")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var receipt struct {
			StopClass string `json:"stop_class"`
		}
		if err := json.Unmarshal(raw, &receipt); err != nil {
			t.Fatal(err)
		}
		return receipt.StopClass
	}
	lock := filepath.Join(repo, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	held := move()
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	indexDir := filepath.Join(tmp, "readonly-index")
	if err := os.Mkdir(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(indexDir, "index"), index, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_INDEX_FILE", filepath.Join(indexDir, "index"))
	if err := os.Chmod(indexDir, 0o555); err != nil {
		t.Fatal(err)
	}
	denied := func() string {
		defer os.Chmod(indexDir, 0o755)
		return move()
	}()
	if held != "git_index_locked" || denied != "permission_denied" {
		t.Fatalf("stop classes = %q, %q", held, denied)
	}
	os.Unsetenv("GIT_INDEX_FILE")
	if got := ucGit(t, repo, "rev-parse", "HEAD"); got != old {
		t.Fatalf("HEAD moved to %s", got)
	}
}
