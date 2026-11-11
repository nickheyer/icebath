package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Replaces the updater's git runner for one test
func ucPatchGit(t *testing.T, run func(ctx context.Context, gitCmd, args []string, cwd string, check, network bool) (*CompletedProcess, error)) {
	t.Helper()
	saved := updateGitRunner
	updateGitRunner = run
	t.Cleanup(func() { updateGitRunner = saved })
}

// Makes root the checkout the update moves for one test
func ucPatchRoot(t *testing.T, root string) {
	t.Helper()
	saved := fleetProjectRoot
	fleetProjectRoot = func() string { return root }
	t.Cleanup(func() { fleetProjectRoot = saved })
}

// Git runner answering only the configured CA lookup, exit 1 when none is configured
func ucGitConfig(t *testing.T, configured string) func(ctx context.Context, gitCmd, args []string, cwd string, check, network bool) (*CompletedProcess, error) {
	return func(ctx context.Context, gitCmd, args []string, cwd string, check, network bool) (*CompletedProcess, error) {
		if !slices.Equal(args, []string{"config", "--get", "http.sslCAInfo"}) {
			t.Fatalf("unexpected git args %v", args)
		}
		code := 1
		if configured != "" {
			code = 0
		}
		return &CompletedProcess{Args: gitCmd, ReturnCode: code, Stdout: configured}, nil
	}
}

// Unsets an environment variable for one test and restores it afterwards
func ucUnsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	os.Unsetenv(name)
}

func TestSslCertFileReachesNetworkGitChildren(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "corp-bundle.pem")
	t.Setenv("SSL_CERT_FILE", bundle)
	ucUnsetenv(t, "GIT_SSL_CAINFO")
	ucPatchGit(t, ucGitConfig(t, ""))
	if err := mapSSLCertFileForGit(context.Background(), []string{"git"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(noPromptGitKwargs().Env, "GIT_SSL_CAINFO="+bundle) {
		t.Fatalf("network git env lacks the bundle")
	}
	if got := os.Getenv("GIT_SSL_CAINFO"); got != bundle {
		t.Fatalf("GIT_SSL_CAINFO = %q", got)
	}
}

func TestConfiguredGitCaIsNotOverridden(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSL_CERT_FILE", filepath.Join(dir, "corp-bundle.pem"))
	ucUnsetenv(t, "GIT_SSL_CAINFO")
	ucPatchGit(t, ucGitConfig(t, filepath.Join(dir, "configured.pem")))
	if err := mapSSLCertFileForGit(context.Background(), []string{"git"}); err != nil {
		t.Fatal(err)
	}
	if _, set := os.LookupEnv("GIT_SSL_CAINFO"); set {
		t.Fatal("GIT_SSL_CAINFO was set over a configured http.sslCAInfo")
	}
}

// Custody runner that times out every bounded call and answers ok to an unbounded one, recording the last options
func ucTimeoutRunner(t *testing.T) *ChildOptions {
	t.Helper()
	last := &ChildOptions{}
	saved := runInCustody
	runInCustody = func(ctx context.Context, argv []string, inheritLock bool, opts ChildOptions) (*CompletedProcess, error) {
		*last = opts
		if opts.Timeout > 0 {
			return nil, &updaterecord.GitTimeout{Args: argv, Timeout: opts.Timeout}
		}
		return &CompletedProcess{Args: argv, Stdout: "ok"}, nil
	}
	t.Cleanup(func() { runInCustody = saved })
	return last
}

func TestNetworkFetchStallBecomesAFailedRunWithANamedCause(t *testing.T) {
	ucPatchRoot(t, "/repo")
	last := ucTimeoutRunner(t)
	result, err := updateGitRun(context.Background(), []string{"git"}, []string{"fetch", "origin", "main"}, "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReturnCode == 0 {
		t.Fatal("a stalled fetch reported success")
	}
	if !strings.Contains(result.Stderr, "timed out") || !strings.Contains(result.Stderr, "fetch") {
		t.Fatalf("stderr = %q", result.Stderr)
	}
	if last.Timeout != networkGitTimeoutSeconds*time.Second {
		t.Fatalf("timeout = %v", last.Timeout)
	}
	if !slices.Contains(last.Env, "GIT_TERMINAL_PROMPT=0") {
		t.Fatal("the no prompt guard did not ride along with the bound")
	}
}

func TestLocalGitStaysUnboundedAndCheckTrueRaises(t *testing.T) {
	ucPatchRoot(t, "/repo")
	last := ucTimeoutRunner(t)
	result, err := updateGitRun(context.Background(), []string{"git"}, []string{"rev-parse", "HEAD"}, "", false, false)
	if err != nil || result.ReturnCode != 0 {
		t.Fatalf("local git = %+v, %v", result, err)
	}
	if last.Timeout != 0 {
		t.Fatalf("local git was bounded by %v", last.Timeout)
	}
	_, err = updateGitRun(context.Background(), []string{"git"}, []string{"fetch", "origin", "main"}, "", true, true)
	var called *CalledProcessError
	if !errors.As(err, &called) {
		t.Fatalf("check=True must raise on a timed-out fetch, got %v", err)
	}
	if called.ReturnCode != 124 {
		t.Fatalf("returncode = %d", called.ReturnCode)
	}
}

// A failed checked subprocess with the given argv and captured streams
func ucCPE(cmd []string, returncode int, stderr, stdout string) error {
	return &CalledProcessError{ReturnCode: returncode, Cmd: cmd, Output: stdout, Stderr: stderr}
}

func TestUvPipInstallIsADependencyFailureNotGit(t *testing.T) {
	exc := ucCPE([]string{`C:\venv\Scripts\uv.exe`, "pip", "install", "-e", "."}, 2, "", "")
	if calledProcessErrorIsGit(exc) {
		t.Fatal("uv pip install classified as git")
	}
	if !calledProcessErrorIsPythonDepInstall(exc) {
		t.Fatal("uv pip install not classified as a dependency install")
	}
	if got := formatUpdateFailureStage(exc); got != "Python dependency install failed" {
		t.Fatalf("stage = %q", got)
	}
}

func TestVenvPipInstallIsADependencyFailure(t *testing.T) {
	exc := ucCPE([]string{`C:\venv\Scripts\python.exe`, "-m", "pip", "install", "-e", "."}, 2, "", "")
	if !calledProcessErrorIsPythonDepInstall(exc) || calledProcessErrorIsGit(exc) {
		t.Fatal("venv pip install misclassified")
	}
}

func TestEnsurepipIsADependencyFailure(t *testing.T) {
	exc := ucCPE([]string{`C:\venv\Scripts\python.exe`, "-m", "ensurepip", "--upgrade"}, 2, "", "")
	if !calledProcessErrorIsPythonDepInstall(exc) {
		t.Fatal("ensurepip not classified as a dependency install")
	}
	if got := formatUpdateFailureStage(exc); got != "Python dependency install failed" {
		t.Fatalf("stage = %q", got)
	}
}

func TestGitPullIsClassifiedAsGit(t *testing.T) {
	exc := ucCPE([]string{"git", "-c", "windows.appendAtomically=false", "pull"}, 1, "", "")
	if !calledProcessErrorIsGit(exc) || calledProcessErrorIsPythonDepInstall(exc) {
		t.Fatal("git pull misclassified")
	}
	if got := formatUpdateFailureStage(exc); got != "Git update failed" {
		t.Fatalf("stage = %q", got)
	}
}

func TestGitExePathIsStillGit(t *testing.T) {
	exc := ucCPE([]string{`C:\Program Files\Git\cmd\git.exe`, "fetch", "origin", "main"}, 2, "", "")
	if !calledProcessErrorIsGit(exc) {
		t.Fatal("git.exe path not classified as git")
	}
}

func TestUnknownCommandGetsGenericStage(t *testing.T) {
	exc := ucCPE([]string{"npm", "install"}, 1, "", "")
	if got := formatUpdateFailureStage(exc); got != "Update step failed" {
		t.Fatalf("stage = %q", got)
	}
}

func TestPosixGitFailureDoesNotZip(t *testing.T) {
	saved := installRepairIsWindows
	installRepairIsWindows = func() bool { return false }
	t.Cleanup(func() { installRepairIsWindows = saved })
	exc := ucCPE([]string{"git", "pull"}, 1, "", "")
	fallback, err := shouldZipFallbackOnUpdateError(context.Background(), exc)
	if err != nil || fallback {
		t.Fatalf("fallback = %v, %v", fallback, err)
	}
}
