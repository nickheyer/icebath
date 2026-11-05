package tools

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Scripted backend at cwd answering every command with output and exit code
func searchScriptEnv(cwd string, answer func(command string) (string, int)) *fakeTermEnv {
	return &fakeTermEnv{envType: "fake", cwd: cwd, execute: func(_ context.Context, command string, _ ExecOptions) (*ExecResult, error) {
		out, rc := answer(command)
		return &ExecResult{Output: out, ReturnCode: rc}, nil
	}}
}

// Scripted backend that declares it does not run on the controller host
type remoteSearchEnv struct{ *fakeTermEnv }

func (remoteSearchEnv) IsLocal() bool { return false }

// Second backend class sharing the scripted behaviour, for per class admission keys
type otherRemoteSearchEnv struct{ *fakeTermEnv }

func (otherRemoteSearchEnv) IsLocal() bool { return false }

// File backend over a terminal env with a throwaway agent env
func searchOpsOver(t *testing.T, term Environment) *shellFileOps {
	t.Helper()
	return newShellFileOps(remoteTestEnv(t), term, "")
}

// Commands the backend saw that contain the fragment
func commandsWith(f *fakeTermEnv, frag string) []string {
	var out []string
	for _, c := range envCommands(f) {
		if strings.Contains(c, frag) {
			out = append(out, c)
		}
	}
	return out
}

// Engine selection RecordingEnvironment answer, rg resolved to a path with a space
func rgRecordingAnswer(rgOutput string, rgCode int) func(string) (string, int) {
	return func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "test -e "):
			return "exists\n", 0
		case strings.HasPrefix(c, "command -v rg"):
			return "/opt/Rip Grep/rg\n", 0
		case strings.Contains(c, "--version"):
			return "ripgrep 14.1.1\n", 0
		case strings.Contains(c, "--files"):
			return rgOutput, rgCode
		}
		return "", 1
	}
}

// Recording backend at /repo that is not the controller host
func rgRecordingEnv(rgOutput string, rgCode int) *fakeTermEnv {
	return searchScriptEnv("/repo", rgRecordingAnswer(rgOutput, rgCode))
}

// Fails the test unless the strings are equal
func wantSlice(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %q, want %q", label, got, want)
	}
}

func TestSearchDefaultFileSearchRunsOneBoundedUnsortedRg(t *testing.T) {
	env := rgRecordingEnv("/repo/one.py\n/repo/two.py\n", 0)
	r := searchOpsOver(t, env).search("*.py", "/repo", "files", "", 1, 1, "content", 0, "discovery")
	wantSlice(t, "files", r.Files, []string{"/repo/two.py"})
	rg := commandsWith(env, "--files")
	if len(rg) != 1 || strings.Contains(rg[0], "--sortr") || !strings.Contains(rg[0], "head -n 3") {
		t.Fatalf("rg commands %q", rg)
	}
}

func TestSearchBoundedFilenameTotalIsALowerBound(t *testing.T) {
	var files []string
	for i := range 200 {
		files = append(files, fmt.Sprintf("/repo/file-%03d.py", i))
	}
	headRe := regexp.MustCompile(`head -n (\d+)`)
	for _, engine := range []string{"rg", "find"} {
		t.Run(engine, func(t *testing.T) {
			env := searchScriptEnv("/repo", func(c string) (string, int) {
				switch {
				case strings.HasPrefix(c, "test -e "):
					return "exists\n", 0
				case strings.HasPrefix(c, "command -v rg"):
					if engine == "rg" {
						return "/usr/bin/rg\n", 0
					}
					return "", 1
				case strings.HasPrefix(c, "command -v find"):
					if engine == "find" {
						return "yes\n", 0
					}
					return "", 1
				case strings.Contains(c, "--files") || strings.HasPrefix(c, "set -o pipefail; find "):
					var n int
					fmt.Sscan(headRe.FindStringSubmatch(c)[1], &n)
					return strings.Join(files[:n], "\n") + "\n", 0
				}
				return "", 1
			})
			r := searchOpsOver(t, env).search("*.py", "/repo", "files", "", 50, 0, "content", 0, "discovery")
			d := searchResultDict(t, r)
			if r.TotalCount != 51 || len(r.Files) != 50 || d["truncated"] != true || d["total_count_is_lower_bound"] != true {
				t.Fatalf("result %v", d)
			}
		})
	}
}

// Decoded dict of a search result in its to_dict shape
func searchResultDict(t *testing.T, r *SearchResult) map[string]any {
	t.Helper()
	b, err := r.toDict(false).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return ftDecode(t, string(b))
}

func TestSearchModifiedRunsOneExactOrderRg(t *testing.T) {
	env := rgRecordingEnv("/repo/one.py\n/repo/two.py\n", 0)
	r := searchOpsOver(t, env).search("*.py", "/repo", "files", "", 50, 0, "content", 0, "modified")
	rg := commandsWith(env, "--files")
	if r.Error != "" || len(rg) != 1 || !strings.Contains(rg[0], "--sortr=modified") {
		t.Fatalf("error %q commands %q", r.Error, rg)
	}
}

func TestSearchModifiedZeroMatchExitOneIsValid(t *testing.T) {
	env := rgRecordingEnv("", 1)
	r := searchOpsOver(t, env).search("*.missing", "/repo", "files", "", 50, 0, "content", 0, "modified")
	if r.Error != "" || len(r.Files) != 0 || len(commandsWith(env, "--files")) != 1 {
		t.Fatalf("result %+v", r)
	}
}

func TestSearchModifiedRequiresParseableRipgrep14(t *testing.T) {
	for _, version := range []string{"ripgrep 13.0.0\n", "ripgrep unknown\n", "ripgrep 14.1\n", "ripgrep 14.1.1-alpha..1\n"} {
		t.Run(version, func(t *testing.T) {
			env := searchScriptEnv("/repo", func(c string) (string, int) {
				switch {
				case strings.HasPrefix(c, "test -e "):
					return "exists\n", 0
				case strings.HasPrefix(c, "command -v rg"):
					return "/opt/Rip Grep/rg\n", 0
				case strings.Contains(c, "--version"):
					return version, 0
				}
				t.Errorf("search must not run: %s", c)
				return "", 1
			})
			ops := searchOpsOver(t, env)
			first := ops.search("*.py", "/repo", "files", "", 50, 0, "content", 0, "modified")
			second := ops.search("*.py", "/repo", "files", "", 50, 0, "content", 0, "modified")
			if !strings.Contains(strings.ToLower(first.Error), "ripgrep 14") || second.Error != first.Error {
				t.Fatalf("errors %q %q", first.Error, second.Error)
			}
			if len(commandsWith(env, "--files")) != 0 || len(commandsWith(env, "--version")) != 1 {
				t.Fatalf("commands %q", envCommands(env))
			}
		})
	}
}

func TestSearchModifiedAcceptsFullRipgrepSemver(t *testing.T) {
	for _, version := range []string{"ripgrep 14.1.1 (rev abc123)\n", "ripgrep 14.1.1-alpha.1+build.2\n"} {
		t.Run(version, func(t *testing.T) {
			base := rgRecordingAnswer("/repo/one.py\n", 0)
			env := searchScriptEnv("/repo", func(c string) (string, int) {
				if strings.Contains(c, "--version") {
					return version, 0
				}
				return base(c)
			})
			r := searchOpsOver(t, env).search("*.py", "/repo", "files", "", 50, 0, "content", 0, "modified")
			if r.Error != "" || len(commandsWith(env, "--files")) != 1 {
				t.Fatalf("error %q", r.Error)
			}
		})
	}
}

func TestSearchEmptyDiscoveryOutputIsZeroMatches(t *testing.T) {
	env := rgRecordingEnv("", 0)
	r := searchOpsOver(t, env).search("*.missing", "/repo", "files", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || len(r.Files) != 0 || r.TotalCount != 0 || len(commandsWith(env, "--files")) != 1 {
		t.Fatalf("result %+v", r)
	}
}

func TestSearchModifiedCapabilityFailureIsActionable(t *testing.T) {
	env := rgRecordingEnv("", 2)
	r := searchOpsOver(t, env).search("*.py", "/repo", "files", "", 50, 0, "content", 0, "modified")
	if len(commandsWith(env, "--files")) != 1 || !strings.Contains(strings.ToLower(r.Error), "exact modification-time order") || !strings.Contains(r.Error, "ripgrep") {
		t.Fatalf("error %q", r.Error)
	}
}

func TestSearchRgPartialOutputWithErrorExitFailsClosed(t *testing.T) {
	for _, order := range []string{"discovery", "modified"} {
		env := rgRecordingEnv("/repo/partial.py\n", 2)
		r := searchOpsOver(t, env).search("*.py", "/repo", "files", "", 50, 0, "content", 0, order)
		if r.Error == "" || len(r.Files) != 0 || len(commandsWith(env, "--files")) != 1 {
			t.Fatalf("%s result %+v", order, r)
		}
	}
}

func TestSearchRgSigpipeBenignOnlyAfterFetchLimit(t *testing.T) {
	for _, order := range []string{"discovery", "modified"} {
		env := rgRecordingEnv("/repo/a.py\n/repo/b.py\n/repo/c.py\n/repo/d.py\n", 141)
		r := searchOpsOver(t, env).search("*.py", "/repo", "files", "", 2, 1, "content", 0, order)
		if r.Error != "" || !r.Truncated {
			t.Fatalf("%s result %+v", order, r)
		}
		wantSlice(t, "files", r.Files, []string{"/repo/b.py", "/repo/c.py"})
		env = rgRecordingEnv("/repo/partial.py\n", 141)
		r = searchOpsOver(t, env).search("*.py", "/repo", "files", "", 2, 1, "content", 0, order)
		if r.Error == "" || len(r.Files) != 0 || r.TotalCount != 0 {
			t.Fatalf("%s short sigpipe %+v", order, r)
		}
	}
}

func TestSearchInvalidFileOrderIsStructuredError(t *testing.T) {
	env := rgRecordingEnv("", 0)
	r := searchOpsOver(t, env).search("*.py", "/repo", "files", "", 50, 0, "content", 0, "random")
	if !strings.Contains(r.Error, "random") || len(commandsWith(env, "--files")) != 0 {
		t.Fatalf("error %q", r.Error)
	}
	if r.Error != "Invalid file search order 'random'; expected 'discovery' or 'modified'." {
		t.Fatalf("error text %q", r.Error)
	}
}

// RipgrepInvocationEnvironment answer covering files, content and count probes
func rgInvocationAnswer(c string) (string, int) {
	switch {
	case strings.HasPrefix(c, "test -e "):
		return "exists\n", 0
	case strings.HasPrefix(c, "command -v rg"):
		return "/opt/Rip Grep/rg\n", 0
	case strings.Contains(c, "--version"):
		return "ripgrep 14.1.1\n", 0
	case strings.Contains(c, "--files"):
		return "/repo/a.py\n", 0
	case strings.Contains(c, "--line-number"):
		return "/repo/a.py:1:needle\n", 0
	}
	return "", 1
}

func TestSearchResolvedExecutableWithSpacesIsUsedEverywhere(t *testing.T) {
	env := searchScriptEnv("/repo", rgInvocationAnswer)
	ops := searchOpsOver(t, env)
	if len(ops.search("*.py", "/repo", "files", "", 50, 0, "content", 0, "discovery").Files) == 0 {
		t.Fatal("no files")
	}
	if len(ops.search("needle", "/repo", "content", "", 50, 0, "content", 0, "discovery").Matches) == 0 {
		t.Fatal("no matches")
	}
	if hint, err := ops.zeroMatchProbe("absent", "/repo", ""); hint != "" || err != nil {
		t.Fatalf("probe %q %v", hint, err)
	}
	bareRg := regexp.MustCompile(`(?:^|[; ])rg\s`)
	var invocations []string
	for _, c := range envCommands(env) {
		if strings.Contains(c, "--files") || strings.Contains(c, "--line-number") || strings.Contains(c, "--count-matches") {
			invocations = append(invocations, c)
		}
	}
	if len(invocations) == 0 {
		t.Fatal("no rg invocations")
	}
	for _, c := range invocations {
		if !strings.Contains(c, "'/opt/Rip Grep/rg'") || bareRg.MatchString(c) {
			t.Fatalf("invocation %q", c)
		}
	}
	if n := len(commandsWith(env, "command -v rg")); n != 1 {
		t.Fatalf("rg resolved %d times", n)
	}
}

func TestSearchOffPathWindowsRgMissIsReprobedThenCached(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	tmp := t.TempDir()
	localAppData := pathJoinOS(tmp, "Local Data")
	candidate := pathJoinOS(pathJoinOS(pathJoinOS(pathJoinOS(localAppData, "Microsoft"), "WinGet"), "Links"), "rg.exe")
	t.Setenv("LOCALAPPDATA", localAppData)
	t.Setenv("USERPROFILE", pathJoinOS(tmp, "User Profile"))
	t.Setenv("SCOOP", "")
	env := &fakeTermEnv{envType: "local", cwd: tmp, execute: func(context.Context, string, ExecOptions) (*ExecResult, error) {
		return &ExecResult{ReturnCode: 1}, nil
	}}
	ops := searchOpsOver(t, env)
	if got := ops.resolveCommand("rg"); got != "" {
		t.Fatalf("resolved %q", got)
	}
	writeTestFile(t, pathDirnameOS(candidate), "rg.exe", nil)
	expected := strings.ReplaceAll(candidate, `\`, "/")
	if a, b := ops.resolveCommand("rg"), ops.resolveCommand("rg"); a != expected || b != expected {
		t.Fatalf("resolved %q %q want %q", a, b, expected)
	}
	wantSlice(t, "probes", envCommands(env), []string{"command -v rg 2>/dev/null", "command -v rg 2>/dev/null"})
}

func TestSearchRemoteResolutionNeverProbesControllerPaths(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", pathJoinOS(tmp, "controller-local"))
	t.Setenv("USERPROFILE", pathJoinOS(tmp, "controller-user"))
	env := searchScriptEnv("/repo", func(string) (string, int) { return "", 1 })
	if got := searchOpsOver(t, env).resolveCommand("rg"); got != "" {
		t.Fatalf("resolved %q", got)
	}
	cmds := envCommands(env)
	wantSlice(t, "commands", cmds, []string{"command -v rg 2>/dev/null"})
	if strings.Contains(cmds[0], tmp) {
		t.Fatalf("controller path leaked %q", cmds[0])
	}
}

func TestSearchRemoteMsysExecutableIsNotRewritten(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	env := searchScriptEnv("/repo", func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "test -e "):
			return "exists\n", 0
		case strings.HasPrefix(c, "command -v rg"):
			return "/c/remote-tools/rg\n", 0
		case strings.Contains(c, "--files"):
			return "/repo/a.py\n", 0
		}
		return "", 1
	})
	r := searchOpsOver(t, env).search("*.py", "/repo", "files", "", 50, 0, "content", 0, "discovery")
	rg := commandsWith(env, "--files")
	if len(r.Files) == 0 || !strings.Contains(rg[0], "'/c/remote-tools/rg' --files") || strings.Contains(rg[0], "C:/remote-tools/rg") {
		t.Fatalf("rg command %q", rg)
	}
}

func TestSearchEveryWindowsDriveRootIsBroad(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	prev := searchHome
	searchHome = "C:/Users/alice"
	t.Cleanup(func() { searchHome = prev })
	ops := searchOpsOver(t, &fakeTermEnv{envType: "local", cwd: t.TempDir()})
	if !ops.isBroadLocalSearchRoot("D:/") || ops.isBroadLocalSearchRoot("D:/repo") {
		t.Fatal("drive root classification")
	}
}

func TestSearchModifiedMultiPathPreservesExactOrder(t *testing.T) {
	env := searchScriptEnv("/repo", func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "test -e "):
			if strings.Contains(c, "'/one /two'") {
				return "not_found\n", 0
			}
			return "exists\n", 0
		case strings.HasPrefix(c, "command -v rg"):
			return "/usr/bin/rg\n", 0
		case strings.Contains(c, "--version"):
			return "ripgrep 14.1.1\n", 0
		case strings.Contains(c, "--files"):
			return "/two/new.py\n/one/old.py\n", 0
		}
		return "", 1
	})
	r := searchOpsOver(t, env).search("*.py", "/one /two", "files", "", 50, 0, "content", 0, "modified")
	wantSlice(t, "files", r.Files, []string{"/two/new.py", "/one/old.py"})
	rg := commandsWith(env, "--files")
	if len(rg) != 1 || !strings.Contains(rg[0], "--sortr=modified") || !strings.Contains(rg[0], "'/one'") || !strings.Contains(rg[0], "'/two'") {
		t.Fatalf("rg commands %q", rg)
	}
}

func TestSearchCommaDelimitedRootsPreserveInternalSpaces(t *testing.T) {
	checks := 0
	env := searchScriptEnv("/repo", func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "test -e "):
			checks++
			if checks == 1 {
				return "not_found\n", 0
			}
			return "exists\n", 0
		case strings.HasPrefix(c, "command -v rg"):
			return "/usr/bin/rg\n", 0
		case strings.Contains(c, "--files"):
			return "C:/root one/a.py\nC:/root two/b.py\n", 0
		}
		return "", 1
	})
	r := searchOpsOver(t, env).search("*.py", "C:/root one, C:/root two", "files", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" {
		t.Fatal(r.Error)
	}
	wantSlice(t, "files", r.Files, []string{"C:/root one/a.py", "C:/root two/b.py"})
	rg := commandsWith(env, "--files")
	if len(rg) != 1 || !strings.Contains(rg[0], "'C:/root one' 'C:/root two'") {
		t.Fatalf("rg commands %q", rg)
	}
}

func TestSearchMultiPathModifiedCapabilityErrorPropagates(t *testing.T) {
	env := searchScriptEnv("/repo", func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "test -e "):
			if strings.Contains(c, "'/one /two'") {
				return "not_found\n", 0
			}
			return "exists\n", 0
		case strings.HasPrefix(c, "command -v rg"):
			return "/usr/bin/rg\n", 0
		case strings.Contains(c, "--version"):
			return "ripgrep 13.0.0\n", 0
		}
		t.Errorf("unexpected %s", c)
		return "", 1
	})
	r := searchOpsOver(t, env).search("*.py", "/one /two", "files", "", 50, 0, "content", 0, "modified")
	if !strings.Contains(strings.ToLower(r.Error), "ripgrep 14") || len(commandsWith(env, "--files")) != 0 {
		t.Fatalf("error %q", r.Error)
	}
}

func TestSearchModifiedTimeoutPreservesPartialResults(t *testing.T) {
	env := rgRecordingEnv("/repo/partial.py\n[Command timed out after 60s]\n", 124)
	r := searchOpsOver(t, env).search("*.py", "/repo", "files", "", 50, 0, "content", 0, "modified")
	wantSlice(t, "files", r.Files, []string{"/repo/partial.py"})
	if !r.Truncated || r.LimitReason != "search_timeout" {
		t.Fatalf("result %+v", r)
	}
}

func TestSearchOrderIsIgnoredForContentSearch(t *testing.T) {
	env := searchScriptEnv("/repo", rgInvocationAnswer)
	r := searchOpsOver(t, env).search("needle", "/repo", "content", "", 50, 0, "content", 0, "not-a-file-order")
	if r.Error != "" || len(r.Matches) == 0 {
		t.Fatalf("result %+v", r)
	}
}

func TestSearchPermissionDiagnosticHelpers(t *testing.T) {
	if !isPermissionOnlyError("rg: /srv/locked: Permission denied (os error 13)\nrg: /srv/other: Permission denied (os error 13)\n") {
		t.Fatal("all denied lines")
	}
	if isPermissionOnlyError("rg: /srv/locked: Permission denied (os error 13)\nrg: error parsing glob '[': unclosed character class\n") {
		t.Fatal("mixed diagnostics")
	}
	if isPermissionOnlyError("") {
		t.Fatal("empty diagnostics")
	}
	wantSlice(t, "rg shapes", extractUnreadablePaths("rg: /srv/a: Permission denied (os error 13)\n"+
		"rg: /srv/b: IO error for operation on /srv/b: Permission denied (os error 13)\n"+
		"rg: /srv/a: Permission denied (os error 13)\n"), []string{"/srv/a", "/srv/b"})
	grepEACCES := "grep: /srv/locked: Permission denied\ngrep: /srv/unread.txt: Permission denied\n"
	if !isPermissionOnlyError(grepEACCES) || isPermissionOnlyError(grepEACCES+"grep: /srv/gone: No such file or directory\n") {
		t.Fatal("grep shapes")
	}
	wantSlice(t, "grep shape", extractUnreadablePaths(grepEACCES), []string{"/srv/locked", "/srv/unread.txt"})
	if got := extractUnreadablePaths("rg: regex parse error:\n^"); len(got) != 0 {
		t.Fatalf("other diagnostics %q", got)
	}
	for _, quoted := range []string{"\u2018/r/locked\u2019", "'/r/locked'"} {
		line := "find: " + quoted + ": Permission denied"
		if !isPermissionOnlyError(line) {
			t.Fatalf("find %q", line)
		}
		wantSlice(t, "find quotes", extractUnreadablePaths(line), []string{"/r/locked"})
		diag, payload := splitToolDiagnostics(line + "\n/r/a.txt:1:needle")
		if diag != line || payload != "/r/a.txt:1:needle" {
			t.Fatalf("split %q %q", diag, payload)
		}
	}
	if isPermissionOnlyError("find: \u2018/r\u2019: No such file or directory") {
		t.Fatal("unrecognized find diagnostic")
	}
}

// Permission degradation RecordingEnvironment with canned rg output for files and content
func permissionAnswer(rgOutput string, rgCode int) func(string) (string, int) {
	return func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "test -e "):
			return "exists\n", 0
		case strings.HasPrefix(c, "command -v rg"):
			return "/usr/bin/rg\n", 0
		case strings.Contains(c, "--version"):
			return "ripgrep 14.1.0\n", 0
		case strings.Contains(c, "--files"), strings.Contains(c, "--line-number"):
			return rgOutput, rgCode
		}
		return "", 1
	}
}

// No rg, find present, find commands answered with the canned output
func findOnlyAnswer(output string, code int) func(string) (string, int) {
	return func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "test -e "):
			return "exists\n", 0
		case strings.HasPrefix(c, "command -v rg"):
			return "", 1
		case strings.HasPrefix(c, "command -v find"):
			return "yes\n", 0
		case strings.Contains(c, "find "):
			return output, code
		}
		return "", 1
	}
}

// No rg, grep present, the plain grep lane answered with the canned output
func grepContentAnswer(output string, code int) func(string) (string, int) {
	return func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "test -e "):
			return "exists\n", 0
		case strings.HasPrefix(c, "command -v rg"):
			return "", 1
		case strings.HasPrefix(c, "command -v grep"):
			return "yes\n", 0
		case strings.Contains(c, "grep -rnHE"):
			return output, code
		}
		return "", 1
	}
}

const (
	eaccesDir       = "rg: /srv/locked: Permission denied (os error 13)\n"
	eaccesDirSorted = "rg: /srv/locked: IO error for operation on /srv/locked: Permission denied (os error 13)\n"
)

func TestSearchFileSearchUnreadableDirReturnsPartialResults(t *testing.T) {
	for _, order := range []string{"discovery", "modified"} {
		r := searchOpsOver(t, remoteSearchEnv{searchScriptEnv("/repo", permissionAnswer(eaccesDir+"/repo/partial.py\n", 2))}).
			search("*.py", "/repo", "files", "", 50, 0, "content", 0, order)
		if r.Error != "" || r.TotalCount != 1 || !strings.Contains(r.Warning, "unreadable path") || !strings.Contains(r.Warning, "/srv/locked") {
			t.Fatalf("%s %+v", order, r)
		}
		wantSlice(t, "files", r.Files, []string{"/repo/partial.py"})
	}
	r := searchOpsOver(t, searchScriptEnv("/repo", permissionAnswer(eaccesDirSorted+"/repo/partial.py\n", 2))).
		search("*.py", "/repo", "files", "", 50, 0, "content", 0, "modified")
	if r.Error != "" || !strings.Contains(r.Warning, "/srv/locked") {
		t.Fatalf("sorted shape %+v", r)
	}
	wantSlice(t, "files", r.Files, []string{"/repo/partial.py"})
}

func TestSearchFileSearchFullyUnreadableDegradesToActionableEmpty(t *testing.T) {
	for _, order := range []string{"discovery", "modified"} {
		r := searchOpsOver(t, searchScriptEnv("/repo", permissionAnswer(eaccesDir, 2))).
			search("*.py", "/srv", "files", "", 50, 0, "content", 0, order)
		if r.Error != "" || len(r.Files) != 0 || r.TotalCount != 0 || !strings.Contains(r.Warning, "unreadable path") || !strings.Contains(r.Warning, "fix permissions") {
			t.Fatalf("%s %+v", order, r)
		}
	}
}

func TestSearchFileSearchNonPermissionExit2FailsClosedWithDetail(t *testing.T) {
	r := searchOpsOver(t, searchScriptEnv("/repo", permissionAnswer("rg: error parsing glob '[': unclosed character class\n", 2))).
		search("*.py", "/repo", "files", "", 50, 0, "content", 0, "discovery")
	if !strings.Contains(r.Error, "File search failed while running ripgrep") || !strings.Contains(r.Error, "unclosed character class") || len(r.Files) != 0 {
		t.Fatalf("%+v", r)
	}
	r = searchOpsOver(t, searchScriptEnv("/repo", permissionAnswer("rg: /srv/broken: IO error: disk exploded\n", 2))).
		search("*.py", "/repo", "files", "", 50, 0, "content", 0, "modified")
	for _, want := range []string{"ripgrep", "invocation failure", "disk exploded"} {
		if !strings.Contains(r.Error, want) {
			t.Fatalf("missing %q in %q", want, r.Error)
		}
	}
	if !strings.Contains(strings.ToLower(r.Error), "exact modification-time order") || strings.Contains(r.Error, "Upgrade ripgrep") {
		t.Fatalf("error %q", r.Error)
	}
}

func TestSearchFileSearchPermissionDegradeKeepsTimeoutReason(t *testing.T) {
	r := searchOpsOver(t, searchScriptEnv("/repo", permissionAnswer(eaccesDir+"/repo/partial.py\n[Command timed out after 60s]\n", 124))).
		search("*.py", "/repo", "files", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || r.LimitReason != "search_timeout" || !strings.Contains(r.Warning, "unreadable path") {
		t.Fatalf("%+v", r)
	}
	wantSlice(t, "files", r.Files, []string{"/repo/partial.py"})
}

func TestSearchContentUnreadableDirReturnsPartialMatches(t *testing.T) {
	for _, other := range []string{"", "rg: /repo/gone: No such file or directory (os error 2)\n"} {
		r := searchOpsOver(t, searchScriptEnv("/repo", permissionAnswer(eaccesDir+other+"/repo/a.py:3:needle\n", 2))).
			search("needle", "/repo", "content", "", 50, 0, "content", 0, "discovery")
		if r.Error != "" || len(r.Matches) != 1 || r.Matches[0].Path != "/repo/a.py" || fmt.Sprint(r.Matches[0].LineNumber) != "3" || r.Matches[0].Content != "needle" {
			t.Fatalf("%+v", r)
		}
		if !strings.Contains(r.Warning, "unreadable path") || !strings.Contains(r.Warning, "/srv/locked") || (other != "" && !strings.Contains(r.Warning, strings.TrimSpace(other))) {
			t.Fatalf("warning %q", r.Warning)
		}
	}
}

// Recording answer whose count probe returns the canned output
func countAnswer(rgOutput string, rgCode int) func(string) (string, int) {
	base := permissionAnswer(rgOutput, rgCode)
	return func(c string) (string, int) {
		if strings.Contains(c, "--count-matches") {
			return rgOutput, rgCode
		}
		return base(c)
	}
}

func TestSearchCountModePartialWarning(t *testing.T) {
	r := searchOpsOver(t, searchScriptEnv("/repo", countAnswer(eaccesDir+"/repo/a.py:2\n", 2))).
		search("needle", "/repo", "content", "", 50, 0, "count", 0, "discovery")
	if r.Error != "" || !reflect.DeepEqual(r.Counts, []SearchCount{{"/repo/a.py", 2}}) || !strings.Contains(r.Warning, "/srv/locked") || !strings.Contains(r.Warning, "unreadable path") {
		t.Fatalf("%+v", r)
	}
	r = searchOpsOver(t, searchScriptEnv("/repo", countAnswer("/repo/a.py:2\n", 0))).
		search("needle", "/repo", "content", "", 50, 0, "count", 0, "discovery")
	if !reflect.DeepEqual(r.Counts, []SearchCount{{"/repo/a.py", 2}}) || r.Warning != "" {
		t.Fatalf("clean %+v", r)
	}
}

func TestSearchContentFullyUnreadableAndHardErrors(t *testing.T) {
	r := searchOpsOver(t, searchScriptEnv("/repo", permissionAnswer(eaccesDir, 2))).
		search("needle", "/srv", "content", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || r.TotalCount != 0 || !strings.Contains(r.Warning, "unreadable path") || !strings.Contains(r.Warning, "fix permissions") {
		t.Fatalf("degrade %+v", r)
	}
	r = searchOpsOver(t, searchScriptEnv("/repo", permissionAnswer("rg: regex parse error:\n    (?:[\n       ^\nerror: unclosed character class\n", 2))).
		search("[", "/repo", "content", "", 50, 0, "content", 0, "discovery")
	if !strings.HasPrefix(r.Error, "Search failed: rg: regex parse error:") || !strings.Contains(r.Error, "unclosed character class") || r.Warning != "" {
		t.Fatalf("hard error %+v", r)
	}
	for _, code := range []int{126, 2} {
		r = searchOpsOver(t, searchScriptEnv("/repo", permissionAnswer("bash: /usr/bin/rg: Permission denied\n", code))).
			search("needle", "/repo", "content", "", 50, 0, "content", 0, "discovery")
		if !strings.Contains(r.Error, "/usr/bin/rg: Permission denied") || strings.Contains(r.Warning, "unreadable path") {
			t.Fatalf("code %d %+v", code, r)
		}
	}
}

func TestSearchGrepContentLanePermissionHandling(t *testing.T) {
	env := searchScriptEnv("/repo", grepContentAnswer("grep: /repo/locked: Permission denied\n/repo/a.py:3:needle\n", 2))
	r := searchOpsOver(t, env).search("needle", "/repo", "content", "", 50, 0, "content", 0, "discovery")
	if len(commandsWith(env, "grep -rnHE")) == 0 || r.Error != "" || len(r.Matches) != 1 || r.Matches[0].Path != "/repo/a.py" ||
		!strings.Contains(r.Warning, "unreadable path") || !strings.Contains(r.Warning, "/repo/locked") {
		t.Fatalf("partial %+v", r)
	}
	r = searchOpsOver(t, searchScriptEnv("/repo", grepContentAnswer("grep: /srv/locked: Permission denied\ngrep: /srv/unread.txt: Permission denied\n", 2))).
		search("needle", "/srv", "content", "", 50, 0, "content", 0, "discovery")
	if r.Error != "" || r.TotalCount != 0 || !strings.Contains(r.Warning, "/srv/locked") || !strings.Contains(r.Warning, "fix permissions") {
		t.Fatalf("empty %+v", r)
	}
	r = searchOpsOver(t, searchScriptEnv("/repo", grepContentAnswer("grep: Unmatched ( or \\(\n", 2))).
		search("(", "/repo", "content", "", 50, 0, "content", 0, "discovery")
	if !strings.Contains(r.Error, "Unmatched") {
		t.Fatalf("hard %+v", r)
	}
}

func TestSearchFindExitOneWithPayloadDegradesToPartial(t *testing.T) {
	cases := []struct {
		diagnostic string
		expected   []string
	}{
		{"", []string{"unreadable", "partial"}},
		{"find: '/narrow/gone': No such file or directory\n", []string{"Some paths could not be read: find: '/narrow/gone': No such file or directory"}},
	}
	for _, order := range []string{"discovery", "modified"} {
		for _, tc := range cases {
			output := "/narrow/partial.py\n"
			if order == "modified" {
				output = "10 /narrow/partial.py\n"
			}
			r := searchOpsOver(t, searchScriptEnv("/repo", findOnlyAnswer(tc.diagnostic+output, 1))).
				search("*.py", "/narrow", "files", "", 50, 0, "content", 0, order)
			if r.Error != "" || r.TotalCount != 1 {
				t.Fatalf("%s %+v", order, r)
			}
			wantSlice(t, "files", r.Files, []string{"/narrow/partial.py"})
			for _, text := range tc.expected {
				if !strings.Contains(r.Warning, text) {
					t.Fatalf("missing %q in %q", text, r.Warning)
				}
			}
			if tc.diagnostic != "" && strings.Contains(strings.ToLower(r.Warning), "permission") {
				t.Fatalf("blamed permissions %q", r.Warning)
			}
		}
		r := searchOpsOver(t, searchScriptEnv("/repo", findOnlyAnswer("", 1))).
			search("*.py", "/narrow", "files", "", 50, 0, "content", 0, order)
		if r.Error == "" || len(r.Files) != 0 {
			t.Fatalf("%s empty exit 1 %+v", order, r)
		}
	}
	r := searchOpsOver(t, searchScriptEnv("/repo", findOnlyAnswer("/narrow/partial.py\n", 2))).
		search("*.py", "/narrow", "files", "", 50, 0, "content", 0, "discovery")
	if r.Error == "" || len(r.Files) != 0 {
		t.Fatalf("exit 2 %+v", r)
	}
}

const searchTimeoutMarker = "[Command timed out after 60s]"

func TestSearchTimeoutHelperStripsOnlyTrailingMarker(t *testing.T) {
	if out, reason := searchStdoutAndLimit(&ExecuteResult{Stdout: "a.py\n" + searchTimeoutMarker, ExitCode: 124}); out != "a.py" || reason != "search_timeout" {
		t.Fatalf("%q %q", out, reason)
	}
	if out, reason := searchStdoutAndLimit(&ExecuteResult{Stdout: "a.py\nnot a marker"}); out != "a.py\nnot a marker" || reason != "" {
		t.Fatalf("%q %q", out, reason)
	}
}

// Budget truncation backend where rg resolves and every other command times out with raw output
func timeoutAnswer(raw string, code int) func(string) (string, int) {
	return func(c string) (string, int) {
		switch {
		case strings.Contains(c, "test -e"):
			return "exists", 0
		case strings.HasPrefix(c, "command -v rg"):
			return "/usr/bin/rg\n", 0
		case strings.HasPrefix(c, "command -v "):
			return "", 1
		}
		return raw, code
	}
}

func TestSearchRgTimeoutReturnsPartialResultsWithoutMarker(t *testing.T) {
	cases := []struct {
		target, outputMode, raw string
		expected                []string
	}{
		{"files", "content", "src/a.py\nsrc/b.py\n" + searchTimeoutMarker, []string{"src/a.py", "src/b.py"}},
		{"content", "files_only", "src/a.py\nsrc/b.py\n" + searchTimeoutMarker, []string{"src/a.py", "src/b.py"}},
		{"content", "content", "src/a.py:10:foo\nsrc/b.py:20:foo\n" + searchTimeoutMarker, []string{"src/a.py", "src/b.py"}},
	}
	for _, tc := range cases {
		r := searchOpsOver(t, searchScriptEnv("/tmp/test", timeoutAnswer(tc.raw, 124))).
			search("foo", "/big", tc.target, "", 50, 0, tc.outputMode, 0, "discovery")
		d := searchResultDict(t, r)
		if r.Error != "" || !r.Truncated || r.LimitReason != "search_timeout" || d["limit_reason"] != "search_timeout" {
			t.Fatalf("%v", d)
		}
		var got []string
		if tc.target == "content" && tc.outputMode == "content" {
			for _, m := range r.Matches {
				got = append(got, m.Path)
				if strings.Contains(m.Content, "timed out") {
					t.Fatalf("marker leaked %q", m.Content)
				}
			}
		} else {
			got = r.Files
		}
		wantSlice(t, "paths", got, tc.expected)
	}
	r := searchOpsOver(t, searchScriptEnv("/tmp/test", timeoutAnswer("rg: regex parse error:", 2))).
		search("[", "/big", "content", "", 50, 0, "content", 0, "discovery")
	if r.Error != "Search failed: rg: regex parse error:" || r.LimitReason != "" {
		t.Fatalf("%+v", r)
	}
}

// Find commands a recording backend saw, bare or behind a prefix
func findCommands(f *fakeTermEnv) []fakeExecCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeExecCall
	for _, c := range f.calls {
		if strings.HasPrefix(c.command, "find ") || strings.Contains(c.command, "; find ") {
			out = append(out, c)
		}
	}
	return out
}

// FindRecordingEnvironment at /narrow, not the controller host
func findRecordingEnv(output string, code int) *fakeTermEnv {
	return searchScriptEnv("/narrow", findOnlyAnswer(output, code))
}

func TestSearchFindDiscoveryIsOneUnsortedPrunedBoundedScan(t *testing.T) {
	env := findRecordingEnv("/narrow/a.py\n/narrow/b.py\n/narrow/c.py\n/narrow/d.py\n", 0)
	r := searchOpsOver(t, env).searchFiles("*.py", []string{"/narrow"}, false, 2, 1, "discovery")
	wantSlice(t, "files", r.Files, []string{"/narrow/b.py", "/narrow/c.py"})
	fc := findCommands(env)
	if !r.Truncated || len(fc) != 1 {
		t.Fatalf("%+v %v", r, fc)
	}
	c := fc[0].command
	if strings.Contains(c, "-printf") || strings.Contains(c, "sort ") || !strings.Contains(c, "-prune") || !strings.Contains(c, "head -n 4") || fc[0].opts.Timeout > 60 {
		t.Fatalf("command %q timeout %d", c, fc[0].opts.Timeout)
	}
}

func TestSearchFindModifiedIsOneExactScan(t *testing.T) {
	env := findRecordingEnv("30 /narrow/new.py\n20 /narrow/mid.py\n10 /narrow/old.py\n", 0)
	r := searchOpsOver(t, env).searchFiles("*.py", []string{"/narrow"}, false, 1, 1, "modified")
	wantSlice(t, "files", r.Files, []string{"/narrow/mid.py"})
	fc := findCommands(env)
	if !r.Truncated || len(fc) != 1 || !strings.Contains(fc[0].command, `-printf '%T@ %p\n'`) || !strings.Contains(fc[0].command, "sort -rn") || !strings.Contains(fc[0].command, "head -n 3") {
		t.Fatalf("%+v %v", r, fc)
	}
}

func TestSearchNoRgMultiRootModifiedIsOneGloballySortedScan(t *testing.T) {
	base := findOnlyAnswer("30 /two/.cache/new.py\n10 /one/.hidden/old.py\n", 0)
	env := searchScriptEnv("/narrow", func(c string) (string, int) {
		if strings.HasPrefix(c, "test -e ") {
			if strings.Contains(c, "'/one/.hidden /two/.cache'") {
				return "not_found\n", 0
			}
			return "exists\n", 0
		}
		return base(c)
	})
	r := searchOpsOver(t, env).search("*.py", "/one/.hidden /two/.cache", "files", "", 1, 0, "content", 0, "modified")
	if r.Error != "" || !r.Truncated {
		t.Fatalf("%+v", r)
	}
	wantSlice(t, "files", r.Files, []string{"/two/.cache/new.py"})
	fc := findCommands(env)
	if len(fc) != 1 {
		t.Fatalf("find commands %v", fc)
	}
	for _, want := range []string{"find -H '/one/.hidden' '/two/.cache'", "sort -rn", "head -n 2", "! -path '/one/.hidden'", "! -path '/two/.cache'"} {
		if !strings.Contains(fc[0].command, want) {
			t.Fatalf("missing %q in %q", want, fc[0].command)
		}
	}
	if fc[0].opts.Timeout > 60 {
		t.Fatalf("timeout %d", fc[0].opts.Timeout)
	}
}

func TestSearchFindVerdicts(t *testing.T) {
	env := findRecordingEnv("", 1)
	r := searchOpsOver(t, env).searchFiles("*.py", []string{"/narrow"}, false, 2, 0, "modified")
	if !strings.Contains(r.Error, "modification-time") || len(findCommands(env)) != 1 {
		t.Fatalf("capability %+v", r)
	}
	for _, tc := range []struct{ order, output string }{
		{"discovery", "/narrow/a.py\n/narrow/b.py\n/narrow/c.py\n/narrow/d.py\n"},
		{"modified", "40 /narrow/a.py\n30 /narrow/b.py\n20 /narrow/c.py\n10 /narrow/d.py\n"},
	} {
		r = searchOpsOver(t, findRecordingEnv(tc.output, 141)).searchFiles("*.py", []string{"/narrow"}, false, 2, 1, tc.order)
		if r.Error != "" || !r.Truncated {
			t.Fatalf("sigpipe %s %+v", tc.order, r)
		}
		wantSlice(t, "files", r.Files, []string{"/narrow/b.py", "/narrow/c.py"})
	}
	for _, tc := range []struct{ order, output, fragment string }{
		{"discovery", "/narrow/partial.py\n", "bounded find traversal"},
		{"modified", "10 /narrow/partial.py\n", "modification-time"},
	} {
		r = searchOpsOver(t, findRecordingEnv(tc.output, 141)).searchFiles("*.py", []string{"/narrow"}, false, 2, 1, tc.order)
		if !strings.Contains(r.Error, tc.fragment) || len(r.Files) != 0 || r.TotalCount != 0 {
			t.Fatalf("short sigpipe %s %+v", tc.order, r)
		}
	}
	for _, tc := range []struct{ order, output, fragment string }{
		{"discovery", "/narrow/partial.py\n", "bounded find traversal"},
		{"modified", "/narrow/not-a-timestamp.py\n", "modification-time"},
	} {
		r = searchOpsOver(t, findRecordingEnv(tc.output, 2)).searchFiles("*.py", []string{"/narrow"}, false, 2, 0, tc.order)
		if !strings.Contains(r.Error, tc.fragment) || len(r.Files) != 0 || r.TotalCount != 0 {
			t.Fatalf("hard error %s %+v", tc.order, r)
		}
	}
	r = searchOpsOver(t, findRecordingEnv("/narrow/partial.py\n"+searchTimeoutMarker, 124)).searchFiles("*.py", []string{"/narrow"}, false, 2, 0, "discovery")
	wantSlice(t, "files", r.Files, []string{"/narrow/partial.py"})
	if r.Error != "" || !r.Truncated || r.LimitReason != "search_timeout" {
		t.Fatalf("timeout %+v", r)
	}
	r = searchOpsOver(t, findRecordingEnv("", 0)).searchFiles("*.missing", []string{"/narrow"}, false, 2, 0, "discovery")
	if r.Error != "" || len(r.Files) != 0 {
		t.Fatalf("zero %+v", r)
	}
}

func TestSearchLocalBroadNoRgRefusesBeforeFind(t *testing.T) {
	home := t.TempDir()
	prev := searchHome
	searchHome = home
	t.Cleanup(func() { searchHome = prev })
	env := &fakeTermEnv{envType: "local", cwd: home, execute: func(_ context.Context, c string, _ ExecOptions) (*ExecResult, error) {
		switch {
		case strings.HasPrefix(c, "command -v rg"):
			return &ExecResult{ReturnCode: 1}, nil
		case strings.HasPrefix(c, "command -v find"):
			return &ExecResult{Output: "yes\n"}, nil
		}
		t.Errorf("broad fallback must not execute: %s", c)
		return &ExecResult{ReturnCode: 1}, nil
	}}
	r := searchOpsOver(t, env).searchFiles("*.py", []string{home}, false, 10, 0, "discovery")
	if !strings.Contains(strings.ToLower(r.Error), "ripgrep") || len(commandsWith(env, "find ")) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestSearchContextLineParsing(t *testing.T) {
	path, n, content, ok := parseSearchContextLine("dir/file-12-name.py-8-context here")
	if !ok || path != "dir/file-12-name.py" || n != "8" || content != "context here" {
		t.Fatalf("%q %q %q %v", path, n, content, ok)
	}
	env := searchScriptEnv("/tmp", func(string) (string, int) { return "dir/file-12-name.py-8-context here\n", 0 })
	r := searchOpsOver(t, env).searchWithGrep("needle", ".", "", 10, 0, "content", 1)
	if r.Error != "" || r.TotalCount != 1 || r.Matches[0].Path != "dir/file-12-name.py" || fmt.Sprint(r.Matches[0].LineNumber) != "8" || r.Matches[0].Content != "context here" {
		t.Fatalf("%+v", r)
	}
}

func TestSearchSplitToolDiagnostics(t *testing.T) {
	diag, payload := splitToolDiagnostics("rg: regex parse error:\n    (?:[)\n       ^\nerror: unclosed character class\n")
	if strings.TrimSpace(payload) != "" || !strings.Contains(diag, "regex parse error") {
		t.Fatalf("%q %q", diag, payload)
	}
	diag, payload = splitToolDiagnostics("a.py:5:hit\na.py-6-after\n--\nb.py:9:hit\n")
	if diag != "" || !strings.Contains(payload, "--") || !strings.Contains(payload, "a.py-6-after") {
		t.Fatalf("%q %q", diag, payload)
	}
}

func TestSearchRegexNewlineDetection(t *testing.T) {
	if !patternHasRegexNewline(`needle\n`) || !patternHasRegexNewline(`needle\\\n`) || patternHasRegexNewline(`a\\nb`) || !patternHasRegexNewline("a\nb") {
		t.Fatal("newline detection")
	}
}

func TestSearchDrainStderrCapKeepsARealErrorPastTheCap(t *testing.T) {
	prev := stderrRetainBytes
	stderrRetainBytes = 1
	t.Cleanup(func() { stderrRetainBytes = prev })
	var perm []string
	for i := range 5 {
		perm = append(perm, fmt.Sprintf("rg: /r/d%d: Permission denied (os error 13)\n", i))
	}
	lines := append(append(append([]string{}, perm[:3]...), "rg: /r/gone: No such file or directory (os error 2)\n"), perm[3:]...)
	var chunks [][]byte
	omitted := 0
	drainStderrCapped(strings.NewReader(strings.Join(lines, "")), &chunks, &omitted)
	text := string(joinBytes(chunks))
	if !strings.Contains(text, "No such file or directory") || omitted != 4 || isPermissionOnlyError(text) {
		t.Fatalf("%q omitted %d", text, omitted)
	}
}

func TestSearchMacosProtectedExclusionsUnits(t *testing.T) {
	tmp := t.TempDir()
	home := pathJoinOS(pathJoinOS(tmp, "Users"), "alice")
	got := map[string]bool{}
	for _, item := range macosProtectedSearchExclusions(home, tmp, home, "darwin") {
		got[strings.Split(item, "/")[0]] = true
	}
	if !reflect.DeepEqual(got, setOf(macosTCCProtectedHomeDirs...)) {
		t.Fatalf("exclusions %v", got)
	}
	if ex := macosProtectedSearchExclusions(home+"/Downloads", tmp, home, "darwin"); len(ex) != 0 {
		t.Fatalf("explicit %v", ex)
	}
	if ex := macosProtectedSearchExclusions(home, tmp, home, "linux"); len(ex) != 0 {
		t.Fatalf("linux %v", ex)
	}
}

// Models a macOS controller with the given home for the test duration
func asMacosHost(t *testing.T, home string) {
	t.Helper()
	prevPlatform, prevHome := searchHostPlatform, searchHome
	searchHostPlatform, searchHome = "darwin", home
	t.Cleanup(func() { searchHostPlatform, searchHome = prevPlatform, prevHome })
}

// macOS RecordingEnvironment answering existence and command probes
func macosRecordingAnswer(c string) (string, int) {
	switch {
	case strings.HasPrefix(c, "test -e"):
		return "exists\n", 0
	case strings.HasPrefix(c, "command -v"):
		return "yes\n", 0
	}
	return "", 1
}

func TestSearchMacosRemoteBackendNeverPrunes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX path model")
	}
	home := pathJoinOS(pathJoinOS(t.TempDir(), "Users"), "alice")
	asMacosHost(t, home)
	env := searchScriptEnv(home, macosRecordingAnswer)
	r := searchOpsOver(t, remoteSearchEnv{env}).search("*.txt", home, "files", "", 50, 0, "content", 0, "discovery")
	rg := commandsWith(env, "--files")
	if len(rg) == 0 || strings.Contains(rg[0], "!Downloads/**") || r.Warning != "" {
		t.Fatalf("command %q warning %q", rg, r.Warning)
	}
}

func TestSearchMacosRgMultiRootScopesProtectedGlobs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX path model")
	}
	asMacosHost(t, "/Users/alice")
	env := searchScriptEnv("/", func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "test -e"):
			if strings.Contains(c, "'/Users/alice /repo'") {
				return "not_found\n", 0
			}
			return "exists\n", 0
		case strings.HasPrefix(c, "command -v rg"):
			return "/usr/bin/rg\n", 0
		case strings.Contains(c, "--version"):
			return "ripgrep 14.1.1\n", 0
		case strings.Contains(c, "--files"):
			return "repo/Downloads/visible.txt\nUsers/alice/safe.txt\n", 0
		}
		t.Errorf("unexpected %s", c)
		return "", 1
	})
	r := searchOpsOver(t, env).search("*.txt", "/Users/alice /repo", "files", "", 50, 0, "content", 0, "modified")
	rg := commandsWith(env, "--files")
	if len(rg) != 1 {
		t.Fatalf("commands %q", rg)
	}
	c := rg[0]
	if !strings.HasPrefix(c, "set -o pipefail; cd '/' && ") || !strings.Contains(c, "--sortr=modified") || !strings.Contains(c, "'!Users/alice/Downloads/**'") ||
		strings.Contains(c, "'!repo/Downloads/**'") || !strings.Contains(c, "'Users/alice' 'repo'") {
		t.Fatalf("command %q", c)
	}
	wantSlice(t, "files", r.Files, []string{"/repo/Downloads/visible.txt", "/Users/alice/safe.txt"})
}

func TestSearchMacosScopedMultiRootTerminatesOptions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX path model")
	}
	asMacosHost(t, "/Users/alice")
	env := searchScriptEnv("/Users/alice", func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "test -e"):
			if strings.Contains(c, "'., --version'") {
				return "not_found\n", 0
			}
			return "exists\n", 0
		case strings.HasPrefix(c, "command -v rg"):
			return "/usr/bin/rg\n", 0
		case strings.Contains(c, "--files"):
			return "", 0
		}
		t.Errorf("unexpected %s", c)
		return "", 1
	})
	r := searchOpsOver(t, env).search("*.txt", "., --version", "files", "", 50, 0, "content", 0, "discovery")
	c := commandsWith(env, "--files")[0]
	if !strings.Contains(c, "cd '/Users/alice' &&") || !strings.Contains(c, " -- '.' '--version'") || r.Error != "" {
		t.Fatalf("command %q error %q", c, r.Error)
	}
}

func TestSearchSchemaMacosNoteFollowsTheHost(t *testing.T) {
	description, _ := searchFilesSchema["description"].(string)
	if runtime.GOOS == "darwin" {
		if !strings.Contains(description, "TCC-protected folders") {
			t.Fatalf("description %q", description)
		}
		for _, name := range macosTCCProtectedHomeDirs {
			if !strings.Contains(description, name) {
				t.Fatalf("schema omits %s", name)
			}
		}
		return
	}
	if strings.Contains(description, "macOS") || strings.Contains(description, "TCC") {
		t.Fatalf("description %q", description)
	}
	if ex := macosProtectedSearchExclusions(searchHome, "", searchHome, searchHostPlatform); len(ex) != 0 {
		t.Fatalf("non macOS host prunes %v", ex)
	}
}

// Remote backend whose rg files run is a scan the test controls
func admissionOps(t *testing.T, wrap func(*fakeTermEnv) Environment, scan func(command string) (string, int, error)) *shellFileOps {
	env := &fakeTermEnv{envType: "fake", cwd: "/workspace", execute: func(_ context.Context, c string, _ ExecOptions) (*ExecResult, error) {
		switch {
		case strings.HasPrefix(c, "command -v rg"):
			return &ExecResult{Output: "/usr/bin/rg\n"}, nil
		case strings.Contains(c, "--files"):
			out, rc, err := scan(c)
			if err != nil {
				return nil, err
			}
			return &ExecResult{Output: out, ReturnCode: rc}, nil
		case strings.HasPrefix(c, "command -v "):
			return &ExecResult{ReturnCode: 1}, nil
		}
		t.Errorf("unexpected backend command: %s", c)
		return &ExecResult{ReturnCode: 1}, nil
	}}
	var term Environment = remoteSearchEnv{env}
	if wrap != nil {
		term = wrap(env)
	}
	return newShellFileOps(remoteTestEnv(t), term, "")
}

// Number of roots currently admitted
func activeAdmissions() int {
	filenameSearchMu.Lock()
	defer filenameSearchMu.Unlock()
	return len(activeFilenameSearchRoots)
}

func TestSearchSameBackendAndRootSerializeFilenameWalks(t *testing.T) {
	var mu sync.Mutex
	active, maximum, completed := 0, 0, 0
	entered := make(chan struct{}, 5)
	release := make(chan struct{})
	scan := func(string) (string, int, error) {
		mu.Lock()
		active++
		maximum = max(maximum, active)
		mu.Unlock()
		entered <- struct{}{}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			return "", 0, errors.New("release timed out")
		}
		mu.Lock()
		active--
		completed++
		mu.Unlock()
		return "/repo/a.py\n", 0, nil
	}
	var wg sync.WaitGroup
	results := make([]*SearchResult, 5)
	for i := range 5 {
		ops := admissionOps(t, nil, scan)
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = ops.searchFiles("*.py", []string{"/repo"}, false, 50, 0, "discovery")
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	for _, r := range results {
		if r.Error != "" {
			t.Fatal(r.Error)
		}
	}
	if completed != 5 || maximum != 1 {
		t.Fatalf("completed %d maximum %d", completed, maximum)
	}
}

func TestSearchDifferentRootsOrBackendClassesWalkTogether(t *testing.T) {
	for _, tc := range []struct {
		name         string
		wrapB        func(*fakeTermEnv) Environment
		rootA, rootB string
	}{
		{"roots", nil, "/one", "/two"},
		{"classes", func(f *fakeTermEnv) Environment { return otherRemoteSearchEnv{f} }, "/same", "/same"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var arrived atomic.Int32
			both := make(chan struct{})
			scan := func(string) (string, int, error) {
				if arrived.Add(1) == 2 {
					close(both)
				}
				select {
				case <-both:
					return "/x.py\n", 0, nil
				case <-time.After(5 * time.Second):
					return "", 0, errors.New("barrier timed out")
				}
			}
			first, second := admissionOps(t, nil, scan), admissionOps(t, tc.wrapB, scan)
			errs := make(chan string, 2)
			go func() { errs <- first.searchFiles("*.py", []string{tc.rootA}, false, 50, 0, "discovery").Error }()
			go func() { errs <- second.searchFiles("*.py", []string{tc.rootB}, false, 50, 0, "discovery").Error }()
			for range 2 {
				if e := <-errs; e != "" {
					t.Fatal(e)
				}
			}
		})
	}
}

// Swaps the admission wait for one that signals before it sleeps
func observeAdmissionWait(t *testing.T) chan struct{} {
	waiting := make(chan struct{}, 64)
	prev := filenameSearchAwait
	filenameSearchAwait = func(changed <-chan struct{}) {
		select {
		case waiting <- struct{}{}:
		default:
		}
		prev(changed)
	}
	t.Cleanup(func() { filenameSearchAwait = prev })
	return waiting
}

func TestSearchOverlappingMultiRootSetsAreClaimedAtomically(t *testing.T) {
	var mu sync.Mutex
	active, maximum := 0, 0
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	scan := func(c string) (string, int, error) {
		mu.Lock()
		active++
		maximum = max(maximum, active)
		mu.Unlock()
		if strings.Contains(c, "'/a' '/b'") {
			close(firstEntered)
			<-releaseFirst
		}
		mu.Lock()
		active--
		mu.Unlock()
		return "/x.py\n", 0, nil
	}
	waiting := observeAdmissionWait(t)
	first, second := admissionOps(t, nil, scan), admissionOps(t, nil, scan)
	firstDone, secondDone := make(chan string, 1), make(chan string, 1)
	go func() { firstDone <- first.searchFiles("*.py", []string{"/a", "/b"}, true, 50, 0, "discovery").Error }()
	<-firstEntered
	go func() { secondDone <- second.searchFiles("*.py", []string{"/b", "/c"}, true, 50, 0, "discovery").Error }()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("second walk never waited")
	}
	close(releaseFirst)
	if e := <-firstDone; e != "" {
		t.Fatal(e)
	}
	if e := <-secondDone; e != "" {
		t.Fatal(e)
	}
	if maximum != 1 {
		t.Fatalf("maximum %d", maximum)
	}
}

func TestSearchInterruptedWaiterReturnsWithoutDispatch(t *testing.T) {
	var dispatches atomic.Int32
	holderEntered := make(chan struct{})
	releaseHolder := make(chan struct{})
	var once sync.Once
	scan := func(string) (string, int, error) {
		dispatches.Add(1)
		once.Do(func() { close(holderEntered) })
		<-releaseHolder
		return "/x.py\n", 0, nil
	}
	waiting := observeAdmissionWait(t)
	holder := admissionOps(t, nil, scan)
	id := NewWorkerID()
	waiter := admissionOps(t, nil, scan).withContext(WithWorker(context.Background(), id))
	t.Cleanup(func() { SetInterrupt(id, false, "") })
	holderDone, waiterDone := make(chan string, 1), make(chan string, 1)
	go func() { holderDone <- holder.searchFiles("*.py", []string{"/repo"}, false, 50, 0, "discovery").Error }()
	<-holderEntered
	go func() { waiterDone <- waiter.searchFiles("*.py", []string{"/repo"}, false, 50, 0, "discovery").Error }()
	<-waiting
	SetInterrupt(id, true, "")
	if e := <-waiterDone; !strings.Contains(strings.ToLower(e), "interrupted") || dispatches.Load() != 1 {
		t.Fatalf("waiter %q dispatches %d", e, dispatches.Load())
	}
	close(releaseHolder)
	if e := <-holderDone; e != "" || dispatches.Load() != 1 {
		t.Fatalf("holder %q dispatches %d", e, dispatches.Load())
	}
}

func TestSearchInterruptAfterFinalSamplePreventsDispatch(t *testing.T) {
	var dispatches atomic.Int32
	ops := admissionOps(t, nil, func(string) (string, int, error) {
		dispatches.Add(1)
		return "/x.py\n", 0, nil
	})
	id := NewWorkerID()
	ops = ops.withContext(WithWorker(context.Background(), id))
	t.Cleanup(func() { SetInterrupt(id, false, "") })
	sampledClear := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	prev := searchIsInterrupted
	searchIsInterrupted = func(ctx context.Context) bool {
		interrupted := prev(ctx)
		if !interrupted && WorkerOf(ctx) == id {
			once.Do(func() { close(sampledClear) })
			<-resume
		}
		return interrupted
	}
	t.Cleanup(func() { searchIsInterrupted = prev })
	done := make(chan string, 1)
	go func() { done <- ops.searchFiles("*.py", []string{"/repo"}, false, 50, 0, "discovery").Error }()
	<-sampledClear
	SetInterrupt(id, true, "")
	close(resume)
	if e := <-done; !strings.Contains(strings.ToLower(e), "interrupted") || dispatches.Load() != 0 || activeAdmissions() != 0 {
		t.Fatalf("error %q dispatches %d active %d", e, dispatches.Load(), activeAdmissions())
	}
}

func TestSearchEmptyFilenameRootsRejectedBeforeResolution(t *testing.T) {
	env := &fakeTermEnv{envType: "fake", cwd: "/workspace", execute: func(_ context.Context, c string, _ ExecOptions) (*ExecResult, error) {
		t.Errorf("engine resolution attempted: %s", c)
		return &ExecResult{ReturnCode: 1}, nil
	}}
	r := searchOpsOver(t, env).searchFiles("*.py", nil, true, 50, 0, "discovery")
	if !strings.Contains(strings.ToLower(r.Error), "at least one search root") || activeAdmissions() != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestSearchAdmissionReleasesAfterAPanickingEngine(t *testing.T) {
	attempts := 0
	ops := admissionOps(t, nil, func(string) (string, int, error) {
		attempts++
		if attempts == 1 {
			return "", 0, errors.New("engine failed")
		}
		return "/repo/a.py\n", 0, nil
	})
	func() {
		defer func() {
			if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "engine failed") {
				t.Fatalf("recovered %v", r)
			}
		}()
		ops.searchFiles("*.py", []string{"/repo"}, false, 50, 0, "discovery")
	}()
	r := ops.searchFiles("*.py", []string{"/repo"}, false, 50, 0, "discovery")
	if r.Error != "" || attempts != 2 || activeAdmissions() != 0 {
		t.Fatalf("%+v attempts %d active %d", r, attempts, activeAdmissions())
	}
}

func TestSearchRemoteRootsNormalizeLexicallyAgainstBackendCwd(t *testing.T) {
	env := remoteSearchEnv{&fakeTermEnv{envType: "fake", cwd: "/workspace"}}
	relative := normalizedFilenameSearchRoot(env, "repo/../repo", "/controller")
	absolute := normalizedFilenameSearchRoot(env, "/workspace/repo", "/controller")
	if relative != "/workspace/repo" || absolute != relative {
		t.Fatalf("%q %q", relative, absolute)
	}
}

func TestSearchWindowsLocalRootSpellingsShareOneKey(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	env := &fakeTermEnv{envType: "local", cwd: "C:/Repo"}
	native := normalizedFilenameSearchRoot(env, `C:\Repo\src\..`, "C:/ignored")
	msys := normalizedFilenameSearchRoot(env, "/c/Repo", "C:/ignored")
	if native != msys {
		t.Fatalf("%q %q", native, msys)
	}
}

func TestSearchPythonTextHelpers(t *testing.T) {
	if !pyIsDigitText("²") || pyIsDigitText("") || pyIsDigitText("1a") || !pyIsDigitText("١٢") {
		t.Fatal("isdigit")
	}
	for in, want := range map[string]int{" +1_0 ": 10, "١٢": 12, "007": 7, "-3": -3} {
		if n, ok := pyIntText(in); !ok || n != want {
			t.Fatalf("int(%q) = %d %v", in, n, ok)
		}
	}
	for _, bad := range []string{"1__0", "_1", "²", "", "1.5"} {
		if _, ok := pyIntText(bad); ok {
			t.Fatalf("int(%q) accepted", bad)
		}
	}
	if f, ok := pyFloatText("1_0.5"); !ok || f != 10.5 {
		t.Fatalf("float %v %v", f, ok)
	}
	if _, ok := pyFloatText("0x10"); ok {
		t.Fatal("hex float accepted")
	}
	if f, ok := pyFloatText("-nan"); !ok || f == f {
		t.Fatal("-nan")
	}
}

func TestSearchZeroMatchProbeUnparseableCountRaisesAndHintSwallowsIt(t *testing.T) {
	env := searchScriptEnv("/repo", func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "command -v rg"):
			return "/usr/bin/rg\n", 0
		case strings.Contains(c, "--count-matches"):
			return "/repo/a.py:²\n", 0
		}
		return "", 1
	})
	ops := searchOpsOver(t, env)
	if _, err := ops.zeroMatchProbe("needle", "/repo", ""); err == nil {
		t.Fatal("superscript count must raise like int()")
	}
	if hint := ops.zeroMatchHint("needle", "/repo", ""); hint != "" {
		t.Fatalf("hint %q", hint)
	}
}

func TestSearchZeroMatchHintTemplatesAndTally(t *testing.T) {
	var lines []string
	for i := range 7 {
		lines = append(lines, fmt.Sprintf("/repo/f%d.py:2", i))
	}
	env := searchScriptEnv("/repo", func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "command -v rg"):
			return "/usr/bin/rg\n", 0
		case strings.Contains(c, "--count-matches") && strings.Contains(c, " -i "):
			return strings.Join(lines, "\n") + "\n", 0
		}
		return "", 1
	})
	hint := searchOpsOver(t, env).zeroMatchHint("needle", "/repo", "")
	want := "0 exact matches, but 14 case-insensitive match(es) in 7 file(s): /repo/f0.py, /repo/f1.py, /repo/f2.py, /repo/f3.py, /repo/f4.py (+2 more) — the pattern's casing may be wrong."
	if hint != want {
		t.Fatalf("hint %q", hint)
	}
}

func TestSearchMergeChildWarningsCollapsesSkipSentences(t *testing.T) {
	a := skippedFields("rg: /a/x: Permission denied (os error 13)\n").apply(&SearchResult{})
	a.Warning = skippedSentence(a) + " " + permissionSearchHint
	b := skippedFields("rg: /b/y: Permission denied (os error 13)\nrg: /b/z: Permission denied (os error 13)\n").apply(&SearchResult{})
	b.Warning = skippedSentence(b) + " extra note"
	got := mergeChildWarnings([]*SearchResult{a, b}, true)
	want := []string{skippedUnreadableWarning([]string{"/a/x", "/b/y", "/b/z"}, 0), "extra note"}
	wantSlice(t, "merged", got, want)
	got = mergeChildWarnings([]*SearchResult{a}, false)
	wantSlice(t, "no results", got, []string{skippedUnreadableWarning([]string{"/a/x"}, 0), permissionSearchHint})
}

func TestSearchPathNotFoundSuggestsSimilarEntries(t *testing.T) {
	env := searchScriptEnv("/repo", func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "test -e "):
			return "not_found\n", 0
		case strings.HasPrefix(c, "test -d "):
			return "yes\n", 0
		case strings.HasPrefix(c, "ls -1 "):
			return "project\nprod\nother\n", 0
		}
		return "", 1
	})
	r := searchOpsOver(t, env).search("x", "/repo/proj", "content", "", 50, 0, "content", 0, "discovery")
	if r.Error != "Path not found: /repo/proj. Similar paths: /repo/project, /repo/prod" {
		t.Fatalf("error %q", r.Error)
	}
}

func TestSearchUnavailableBackendAndCwdErrors(t *testing.T) {
	r := searchOpsOver(t, searchScriptEnv("/repo", func(string) (string, int) { return "garbage", 0 })).
		search("x", "/repo", "content", "", 50, 0, "content", 0, "discovery")
	if r.Error != "Terminal environment unavailable: could not stat /repo (the sandbox may still be starting or was removed). Retry shortly." {
		t.Fatalf("error %q", r.Error)
	}
	r = searchOpsOver(t, searchScriptEnv("/gone", func(string) (string, int) { return "bash: cd: /gone: No such file or directory", 126 })).
		search("x", "/repo", "content", "", 50, 0, "content", 0, "discovery")
	if !strings.HasPrefix(r.Error, "working directory '/gone' does not exist on the active terminal backend") {
		t.Fatalf("error %q", r.Error)
	}
}

func TestSearchMacosProtectedWarningOnBroadSearch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX path model")
	}
	asMacosHost(t, "/Users/alice")
	env := searchScriptEnv("/Users/alice", func(c string) (string, int) {
		switch {
		case strings.HasPrefix(c, "test -e"):
			return "exists\n", 0
		case strings.HasPrefix(c, "command -v rg"):
			return "/usr/bin/rg\n", 0
		case strings.Contains(c, "--line-number"):
			return "/Users/alice/a.py:1:needle\n", 0
		}
		return "", 1
	})
	r := searchOpsOver(t, env).search("needle", "/Users/alice", "content", "", 50, 0, "content", 0, "discovery")
	want := "Skipped macOS protected folders during broad search to avoid an unattended privacy prompt: Desktop, Documents, Downloads, Library, Movies, Music, Pictures. Search a protected folder directly when access is intentional."
	if r.Warning != want {
		t.Fatalf("warning %q", r.Warning)
	}
	c := commandsWith(env, "--line-number")[0]
	if !strings.Contains(c, "--glob '!Downloads/**'") {
		t.Fatalf("command %q", c)
	}
}
