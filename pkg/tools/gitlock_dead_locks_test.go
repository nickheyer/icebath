package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Isolated ICEBATH_HOME with a fresh receipt scope
func glReceiptHome(t *testing.T) context.Context {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("ICEBATH_UPDATE_CORRELATION_ID", "")
	return pm.WithReceiptScope(updaterecord.UpdateReceiptScope(context.Background()))
}

// A real running receipt whose owner is a pid that already exited, started five seconds before any lock
func glKilledUpdateReceipt(t *testing.T) context.Context {
	t.Helper()
	ctx := glReceiptHome(t)
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	updaterecord.BeginUpdateReceipt(ctx, nil, "")
	current := updaterecord.CurrentReceipt(ctx)
	if current == nil {
		t.Fatal("no receipt begun")
	}
	pid := dead.ProcessState.Pid()
	current.Data.Set("pid", pid)
	current.Data.Set("pid_create_time", nil)
	current.Data.Set("writer_pid", pid)
	current.Data.Set("writer_create_time", nil)
	started, _, err := pydatetime.ParseISOFormat(pyrepr.Str(current.Data.Vals["started_at"]))
	if err != nil {
		t.Fatal(err)
	}
	current.Data.Set("started_at", pydatetime.ISOFormat(started.Add(-5*time.Second)))
	payload, err := updaterecord.ReceiptPayload(current.Data, false)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, '\n')
	dir := updaterecord.ReceiptDir(ctx)
	stale, _ := filepath.Glob(filepath.Join(dir, "update_*_"+current.CorrelationID+".json"))
	for _, p := range stale {
		os.Remove(p)
	}
	if err := os.WriteFile(updaterecord.RunFile(dir, current.Data), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := updaterecord.WriteLatest(ctx, payload); err != nil {
		t.Fatal(err)
	}
	updaterecord.ClearCurrentReceipt(ctx)
	return ctx
}

func TestAFreshLockWithNoKilledUpdateBehindItIsKept(t *testing.T) {
	repo := glRepo(t)
	ctx := glReceiptHome(t)
	lock := filepath.Join(repo, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	released, err := ReleaseDeadIndexLock(ctx, repo)
	if err != nil || released {
		t.Fatalf("released %v %v", released, err)
	}
	if !pathExists(lock) {
		t.Fatal("the lock went")
	}
}

// Injected lsof and ps answers naming one process with the given command and cwd
func glPsLists(command, cwd string) func(context.Context, []string, pysubprocess.Options) (*pysubprocess.Completed, error) {
	return func(_ context.Context, argv []string, _ pysubprocess.Options) (*pysubprocess.Completed, error) {
		joined := strings.Join(argv, " ")
		if argv[0] == "lsof" {
			if strings.Contains(joined, "cwd") {
				return &pysubprocess.Completed{Args: argv, Returncode: 0, Stdout: []byte("p4242\nn" + cwd + "\n")}, nil
			}
			return &pysubprocess.Completed{Args: argv, Returncode: 1}, nil
		}
		rows := "4242 " + command
		if strings.Contains(joined, "-ocomm=") {
			rows = "4242 " + strconv.Itoa(os.Getuid()) + " " + strings.Fields(command)[0]
		}
		return &pysubprocess.Completed{Args: argv, Returncode: 0, Stdout: []byte(rows + "\n")}, nil
	}
}

func TestLockKeepingGitIsRecognisedInEveryFormAndByPathComponents(t *testing.T) {
	if got := pm.GitSubcommandOf(pm.GitArgvBytes([]byte("/usr/lib/git-core/git-commit\x00-a\x00"))); got != "commit" {
		t.Fatalf("dashed %q", got)
	}
	if got := pm.GitSubcommandOf([]string{`C:\Git\mingw64\libexec\git-core\GIT-COMMIT.EXE`}); got != "commit" {
		t.Fatalf("exe %q", got)
	}
	if got := pm.GitSubcommandOf([]string{"git", "-C", "x", "-c", "k=v", "--no-pager", "save", "-a"}); got != "save" {
		t.Fatalf("value options %q", got)
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, "icebath")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "icebath-backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	prevWhich := pm.SwapLockWhich(func(name string) (string, bool) { return name, true })
	prevRun := pm.SwapLockRunner(glPsLists("/Library/Developer/CommandLineTools/usr/libexec/git-core/git-commit -a", root))
	if !pm.HeldOpenLsof(lock, root, false).Held() {
		t.Fatal("macOS: a dashed git-commit with its fd closed keeps the lock")
	}
	pm.SwapLockRunner(glPsLists("git commit -a", filepath.Join(tmp, "icebath-backup")))
	if !pm.HeldOpenLsof(lock, root, false).Free() {
		t.Fatal("a commit in another repository is not this checkout's")
	}
	pm.SwapLockRunner(glPsLists("git log --oneline", root))
	if !pm.HeldOpenLsof(lock, root, false).Free() || !pm.HeldOpenLsof(lock, root, true).Held() {
		t.Fatal("a reader keeps the lock only when any git counts")
	}
	pm.SwapLockWhich(prevWhich)
	pm.SwapLockRunner(prevRun)
	scan := func(cwd, name string) *bool {
		prev := gitlockProcessIter
		gitlockProcessIter = func() ([]gitlockProcess, error) {
			return []gitlockProcess{{PID: 4242, Name: name, Cwd: cwd, Cmdline: []string{name, "commit"}, Environ: map[string]string{}, IsRunning: func() bool { return true }}}, nil
		}
		defer func() { gitlockProcessIter = prev }()
		got, err := windowsGitInCheckout(root)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := scan(filepath.Join(tmp, "icebath-backup"), "git.exe"); got == nil || *got {
		t.Fatalf("a sibling directory is not this checkout: %v", got)
	}
	if got := scan(filepath.Join(root, "sub"), "git.exe"); got == nil || !*got {
		t.Fatalf("a subdirectory cwd: %v", got)
	}
	if got := scan(root, "git-commit.exe"); got == nil || !*got {
		t.Fatalf("a dashed exe: %v", got)
	}
}

func TestAFailedRetryNeverErasesTheKilledUpdatesClaim(t *testing.T) {
	repo := glRepo(t)
	ctx := glKilledUpdateReceipt(t)
	lock := filepath.Join(repo, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	updaterecord.BeginUpdateReceipt(ctx, nil, "")
	updaterecord.FinalizeUpdateReceipt(ctx, "failed", nil, "")
	if got := updaterecord.ReadLatestReceipt(ctx)["outcome"]; got != "failed" {
		t.Fatalf("latest outcome %v", got)
	}
	released, err := ReleaseDeadIndexLock(ctx, repo)
	if err != nil || !released {
		t.Fatalf("released %v %v", released, err)
	}
	if pathExists(lock) {
		t.Fatal("the killed update's lock stays")
	}
}
