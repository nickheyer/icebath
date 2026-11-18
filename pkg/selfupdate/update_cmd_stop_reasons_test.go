package selfupdate

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

func TestACheckoutMoveNamesAHeldIndexLockApartFromAPermissionError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX permissions; root ignores them")
	}
	ctx, _ := ucReceiptHome(t)
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
