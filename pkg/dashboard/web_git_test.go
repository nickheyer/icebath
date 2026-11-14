package dashboard

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Acts as the gh binary when the test executable is run under that name with the fake variables set
func init() {
	if os.Getenv("ICEBATH_FAKE_GH") == "1" {
		fmt.Fprint(os.Stdout, os.Getenv("ICEBATH_FAKE_GH_STDOUT"))
		fmt.Fprint(os.Stderr, os.Getenv("ICEBATH_FAKE_GH_STDERR"))
		code, _ := strconv.Atoi(os.Getenv("ICEBATH_FAKE_GH_CODE"))
		os.Exit(code)
	}
}

// Puts a gh on PATH that exits with code after writing stdout and stderr
func fakeGh(t *testing.T, code int, stdout, stderr string) {
	t.Helper()
	dir := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name = "gh.exe"
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, name)
	if err := os.Link(self, target); err != nil {
		src, err := os.Open(self)
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o755)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(dst, src); err != nil {
			t.Fatal(err)
		}
		dst.Close()
	}
	t.Setenv("PATH", dir)
	t.Setenv("ICEBATH_FAKE_GH", "1")
	t.Setenv("ICEBATH_FAKE_GH_CODE", strconv.Itoa(code))
	t.Setenv("ICEBATH_FAKE_GH_STDOUT", stdout)
	t.Setenv("ICEBATH_FAKE_GH_STDERR", stderr)
}

func TestSuccessReturnsOkWithStdoutAndStderr(t *testing.T) {
	fakeGh(t, 0, "ok\n", "")
	ok, out, errText := ghCall(t.TempDir(), []string{"auth", "status"})
	if !ok || out != "ok\n" || errText != "" {
		t.Fatalf("%v %q %q", ok, out, errText)
	}
}

func TestNonzeroExitKeepsExitStatusAndStderr(t *testing.T) {
	fakeGh(t, 1, "", "no commits between main and feature")
	ok, out, errText := ghCall(t.TempDir(), []string{"pr", "create", "--fill"})
	if ok || out != "" || errText != "no commits between main and feature" {
		t.Fatalf("%v %q %q", ok, out, errText)
	}
}

func TestMissingGhBinaryIsNotOkWithoutRaising(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ok, out, errText := ghCall(t.TempDir(), []string{"auth", "status"})
	if ok || out != "" || errText != "" {
		t.Fatalf("%v %q %q", ok, out, errText)
	}
}

// Message of the GitOpError reviewCreatePr fails with
func createPrFailure(t *testing.T) string {
	t.Helper()
	_, err := reviewCreatePr(t.TempDir())
	var gerr *GitOpError
	if !errors.As(err, &gerr) {
		t.Fatalf("want RuntimeError, got %v", err)
	}
	return gerr.Msg
}

func TestFailureMessageCarriesGhStderrMarker(t *testing.T) {
	fakeGh(t, 1, "", "gh: no commits between main and bb/fix-wave2g")
	if msg := createPrFailure(t); !strings.Contains(msg, "no commits between main and bb/fix-wave2g") {
		t.Fatalf("message %q", msg)
	}
}

func TestFailureMessageStillNamesGhWhenStderrIsEmpty(t *testing.T) {
	fakeGh(t, 1, "", "   ")
	if msg := createPrFailure(t); !strings.Contains(msg, "is gh installed and authenticated") {
		t.Fatalf("message %q", msg)
	}
}

func TestSuccessPathReturnsThePrURL(t *testing.T) {
	fakeGh(t, 0, "https://github.com/org/repo/pull/1234\n", "")
	result, err := reviewCreatePr(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := pyjson.Dumps(result)
	if got != `{"url": "https://github.com/org/repo/pull/1234"}` {
		t.Fatalf("result %s", got)
	}
}

func TestOversizedStderrIsBoundedToItsTail(t *testing.T) {
	marker := "TRAILING-MARKER-reason"
	fakeGh(t, 1, "", strings.Repeat("x", 5000)+"\n"+marker)
	msg := createPrFailure(t)
	if !strings.Contains(msg, marker) || len(msg) > len("gh pr create failed: ")+ghErrTailChars ||
		strings.Contains(msg, strings.Repeat("x", ghErrTailChars)) {
		t.Fatalf("message %q", msg)
	}
}

// Stripped stdout of git in repo, failing the test on a non zero exit
func identityGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// Repo with one staged file under a global config naming a user, every GIT and EMAIL variable cleared
func identityRepo(t *testing.T) (string, string) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, value, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(key, "GIT_") || key == "EMAIL" {
			os.Unsetenv(key)
			t.Cleanup(func() { os.Setenv(key, value) })
		}
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "global.gitconfig")
	if err := os.WriteFile(config, []byte("[user]\n name = Global Name\n email = global@example.invalid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_DATE", "2001-02-03T04:05:06 +0230")
	t.Setenv("GIT_COMMITTER_DATE", "2002-03-04T05:06:07 -0430")
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	identityGit(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	identityGit(t, repo, "add", "file.txt")
	return repo, config
}

func TestReviewCommitMatchesNativeEffectiveIdentity(t *testing.T) {
	for _, source := range []string{"global", "local", "conditional", "explicit", "partial-env", "role-config"} {
		t.Run(source, func(t *testing.T) {
			repo, config := identityRepo(t)
			if source == "local" || source == "explicit" || source == "partial-env" {
				identityGit(t, repo, "config", "user.name", "Local Name")
				identityGit(t, repo, "config", "user.email", "local@example.invalid")
			}
			if source == "conditional" {
				included := filepath.Join(filepath.Dir(repo), "included.gitconfig")
				if err := os.WriteFile(included, []byte("[user]\n name = Conditional Name\n email = conditional@example.invalid\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				f, err := os.OpenFile(config, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(f, "[includeIf \"gitdir:%s/\"]\n path = %s\n", filepath.ToSlash(repo), filepath.ToSlash(included))
				f.Close()
			}
			if source == "explicit" || source == "partial-env" {
				t.Setenv("GIT_AUTHOR_NAME", "Explicit Author")
				t.Setenv("GIT_COMMITTER_EMAIL", "committer@example.invalid")
			}
			if source == "explicit" {
				t.Setenv("GIT_AUTHOR_EMAIL", "author@example.invalid")
				t.Setenv("GIT_COMMITTER_NAME", "Explicit Committer")
			}
			if source == "role-config" {
				identityGit(t, repo, "config", "author.name", "Configured Author")
				identityGit(t, repo, "config", "committer.email", "configured-committer@example.invalid")
			}
			expected := []string{identityGit(t, repo, "var", "GIT_AUTHOR_IDENT"), identityGit(t, repo, "var", "GIT_COMMITTER_IDENT")}
			result, err := reviewCommit(repo, "identity", false)
			if err != nil || result.Vals["ok"] != true || len(result.Keys) != 1 {
				t.Fatalf("commit %v %v", result, err)
			}
			headers := strings.Split(identityGit(t, repo, "cat-file", "commit", "HEAD"), "\n")
			for i, role := range []string{"author", "committer"} {
				got := ""
				for _, line := range headers {
					if strings.HasPrefix(line, role+" ") {
						got = line[len(role)+1:]
						break
					}
				}
				if got != expected[i] {
					t.Fatalf("%s %q, want %q", role, got, expected[i])
				}
			}
		})
	}
}

func TestReviewCommitKeepsBehaviorIsolatedAndMissingIdentityRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX hook script")
	}
	repo, config := identityRepo(t)
	hooks := filepath.Join(filepath.Dir(repo), "hooks")
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(repo, "hook-ran")
	hook := filepath.Join(hooks, "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf ran > hook-ran\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{{"core.hooksPath", hooks}, {"core.fsmonitor", hook}, {"core.pager", hook}, {"core.editor", hook},
		{"filter.fixture.clean", hook}, {"filter.fixture.smudge", hook}} {
		identityGit(t, repo, "config", kv[0], filepath.ToSlash(kv[1]))
	}
	result, err := reviewCommit(repo, "isolated", false)
	if err != nil || result.Vals["ok"] != true {
		t.Fatalf("commit %v %v", result, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("hook ran during the commit")
	}
	attributes := filepath.Join(repo, ".gitattributes")
	if err := os.WriteFile(attributes, []byte("* filter=fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewCommitEnv(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("filter ran during the identity read")
	}
	os.Remove(attributes)
	if err := os.WriteFile(config, []byte("[user]\n useConfigOnly = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = reviewCommit(repo, "missing", false)
	var gerr *GitOpError
	if !errors.As(err, &gerr) || !regexp.MustCompile(`identity|auto-detection`).MatchString(gerr.Msg) {
		t.Fatalf("want identity refusal, got %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("hook ran during the refused commit")
	}
}
