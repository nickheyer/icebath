package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	gitTimeout                     = 30
	ghTimeout                      = 30
	ghErrTailChars                 = 400
	untrackedLineMaxBytes          = 1024 * 1024
	untrackedScanCap               = 500
	commitContextDiffMaxChars      = 120_000
	commitContextUntrackedMax      = 80
	prQueryBranchChunk             = 50
	prQueryBranchCap               = 300
	prNodeFields                   = "number state isDraft isCrossRepository title url headRefName"
	gitInvocationFailed            = "git invocation failed"
	gitIdentityLookupFailedMessage = "git identity lookup failed"
)

var trunkBranches = []string{"main", "master"}

// Failed git or gh mutation the git routes answer with a 400, the Python RuntimeError
type GitOpError struct{ Msg string }

func (e *GitOpError) Error() string      { return e.Msg }
func (e *GitOpError) PyTypeName() string { return "RuntimeError" }

// Completed text mode run with both streams decoded as UTF-8 with replacement and universal newlines
type gitCompleted struct {
	returncode     int
	stdout, stderr string
}

// Text of captured output the way subprocess text mode reads it
func gitText(b []byte) string {
	s := pystr.DecodeUTF8Replace(b)
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// Environ list of an env mapping
func gitEnviron(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Non interactive run with stdin nulled, nil when the process could not run or timed out
func gitRun(argv []string, cwd string, timeout int, env map[string]string) *gitCompleted {
	res, err := pysubprocess.RunWith(context.Background(), argv, pysubprocess.Options{Dir: cwd, Env: gitEnviron(env),
		Timeout: time.Duration(timeout) * time.Second})
	if err != nil || res == nil {
		return nil
	}
	return &gitCompleted{returncode: res.Returncode, stdout: gitText(res.Stdout), stderr: gitText(res.Stderr)}
}

// Return code, stdout and stderr of git in cwd, never failing on a non zero exit
func gitExec(cwd string, args []string) (int, string, string) {
	proc := gitRun(append([]string{"git"}, tools.HardenGitArgv(args)...), cwd, gitTimeout, tools.NoninteractiveGitEnv(nil))
	if proc == nil {
		return 1, "", gitInvocationFailed
	}
	return proc.returncode, proc.stdout, proc.stderr
}

// Stdout of a git command, empty on any failure
func gitOut(cwd string, args []string) string {
	code, out, _ := gitExec(cwd, args)
	if code != 0 {
		return ""
	}
	return out
}

// Stripped stdout of a single value git query
func gitLine(cwd string, args []string) string { return pystr.Strip(gitOut(cwd, args)) }

// Runs a git mutation, failing with its stderr
func gitOk(cwd string, args []string) error {
	code, _, errText := gitExec(cwd, args)
	if code != 0 {
		msg := pystr.Strip(errText)
		if msg == "" {
			msg = "git " + strings.Join(args, " ") + " failed"
		}
		return &GitOpError{msg}
	}
	return nil
}

// Whether cwd is a directory, false on any error
func gitIsDir(cwd string) bool {
	info, err := os.Stat(cwd)
	return err == nil && info.IsDir()
}

// Return code and raw porcelain v2 NUL separated status
func statusZ(cwd string, extra ...string) (int, string) {
	args := append(append([]string{"status", "--porcelain=v2"}, extra...), "-z")
	code, raw, _ := gitExec(cwd, args)
	return code, raw
}

// Short name of origin's default branch, empty without a remote
func originHead(cwd string) string {
	return gitLine(cwd, []string{"symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"})
}

func originHeadAbbrev(cwd string) string {
	return gitLine(cwd, []string{"rev-parse", "--abbrev-ref", "origin/HEAD"})
}

func refExists(cwd, ref string) bool {
	code, _, _ := gitExec(cwd, []string{"rev-parse", "--verify", "--quiet", ref})
	return code == 0
}

// New path of a rename row, old => new and dir/{old => new}/f forms
func resolveRenamePath(raw string) string {
	path := pystr.Strip(raw)
	if !strings.Contains(path, " => ") {
		return path
	}
	head, tail, _ := strings.Cut(path, "{")
	if tail != "" && strings.Contains(tail, "}") {
		inner, suffix, _ := strings.Cut(tail, "}")
		_, to, _ := strings.Cut(inner, " => ")
		return strings.ReplaceAll(head+to+suffix, "//", "/")
	}
	parts := strings.Split(path, " => ")
	return pystr.Strip(parts[len(parts)-1])
}

// Added and removed counts per path of git diff numstat in first seen order, binary rows counting zero
type numstatCounts struct {
	keys   []string
	counts map[string][2]int
}

func (n numstatCounts) get(path string) [2]int { return n.counts[path] }

func numstat(cwd string, args []string) numstatCounts {
	out := numstatCounts{counts: map[string][2]int{}}
	for _, line := range pystr.Splitlines(gitOut(cwd, append([]string{"diff", "--numstat"}, args...)), false) {
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		var c [2]int
		for i, p := range parts[:2] {
			if p != "-" && p != "" {
				c[i], _ = strconv.Atoi(p)
			}
		}
		path := resolveRenamePath(parts[2])
		if _, seen := out.counts[path]; !seen {
			out.keys = append(out.keys, path)
		}
		out.counts[path] = c
	}
	return out
}

// Line count of an untracked file, zero for binary or oversized files
func untrackedInsertions(cwd, rel string) int {
	target := filepath.Join(cwd, rel)
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() || info.Size() > untrackedLineMaxBytes {
		return 0
	}
	data, err := os.ReadFile(target)
	if err != nil || bytes.IndexByte(data, 0) >= 0 {
		return 0
	}
	lines := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		return lines + 1
	}
	return lines
}

// Merge base with the remote default branch, empty when none resolves
func branchBase(cwd string) string {
	var candidates []string
	if head := originHeadAbbrev(cwd); head != "" {
		candidates = append(candidates, head)
	}
	candidates = append(candidates, "origin/main", "origin/master", "main", "master")
	for _, ref := range candidates {
		if b := gitLine(cwd, []string{"merge-base", "HEAD", ref}); b != "" {
			return b
		}
	}
	return ""
}

// Trunk name of the repo preferring origin HEAD, empty when none is found
func defaultBranchName(cwd string) string {
	if head := originHeadAbbrev(cwd); head != "" && head != "origin/HEAD" {
		parts := strings.SplitN(head, "/", 2)
		return parts[len(parts)-1]
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/master", "refs/remotes/origin/main", "refs/remotes/origin/master"} {
		if refExists(cwd, ref) {
			parts := strings.Split(ref, "/")
			return parts[len(parts)-1]
		}
	}
	return ""
}

type statusEntry struct{ tag, xy, path string }

// Tag, xy and path of each changed file in porcelain v2 output, rename origin records skipped
func walkEntries(raw string) []statusEntry {
	records := strings.Split(raw, "\x00")
	var out []statusEntry
	for i := 0; i < len(records); i++ {
		rec := records[i]
		tag := ""
		if rec != "" {
			tag = rec[:1]
		}
		switch tag {
		case "?":
			out = append(out, statusEntry{"?", "??", rec[min(2, len(rec)):]})
		case "u":
			parts := strings.SplitN(rec, " ", 11)
			out = append(out, statusEntry{"u", strings.Split(rec, " ")[1], parts[len(parts)-1]})
		case "1", "2":
			n := 9
			if tag == "2" {
				n = 10
				i++
			}
			parts := strings.SplitN(rec, " ", n)
			out = append(out, statusEntry{tag, strings.Split(rec, " ")[1], resolveRenamePath(parts[len(parts)-1])})
		}
	}
	return out
}

// Whether a tracked entry has its index code set
func entryStaged(tag, xy string) bool {
	return (tag == "1" || tag == "2") && xy[0] != '.' && xy[0] != '?'
}

func gitClassify(e statusEntry) *pyjson.Object {
	y := "."
	if len(e.xy) > 1 {
		y = e.xy[1:2]
	}
	tracked := e.tag == "1" || e.tag == "2"
	return pyjson.ObjectOf("path", e.path, "staged", entryStaged(e.tag, e.xy),
		"unstaged", e.tag == "?" || (tracked && y != "." && y != "?"), "untracked", e.tag == "?", "conflicted", e.tag == "u")
}

func statusLetter(tag, xy string) string {
	if tag == "?" || tag == "u" {
		return strings.ToUpper(tag)
	}
	code := xy[:1]
	if code == "." {
		code = "."
		if len(xy) > 1 {
			code = xy[1:2]
		}
	}
	if code == "." {
		code = "M"
	}
	return strings.ToUpper(code)
}

// Compact working tree status for the coding rail, nil on a non repo
func repoStatus(cwd string) *pyjson.Object {
	if !gitIsDir(cwd) {
		return nil
	}
	code, raw := statusZ(cwd, "--branch")
	if code != 0 {
		return nil
	}
	var branch any
	detached := false
	ahead, behind := 0, 0
	for _, rec := range strings.Split(raw, "\x00") {
		if strings.HasPrefix(rec, "# branch.head ") {
			head := rec[len("# branch.head "):]
			detached = head == "(detached)"
			branch = nil
			if !detached {
				branch = head
			}
		} else if strings.HasPrefix(rec, "# branch.ab ") {
			toks := pystr.SplitWhitespace(rec, -1)
			for _, tok := range toks[min(2, len(toks)):] {
				if strings.HasPrefix(tok, "+") {
					ahead, _ = strconv.Atoi(tok[1:])
				} else if strings.HasPrefix(tok, "-") {
					behind, _ = strconv.Atoi(tok[1:])
				}
			}
		}
	}
	var files []*pyjson.Object
	for _, e := range walkEntries(raw) {
		files = append(files, gitClassify(e))
	}
	counts := numstat(cwd, []string{"HEAD"})
	added, removed := 0, 0
	for _, k := range counts.keys {
		added += counts.counts[k][0]
		removed += counts.counts[k][1]
	}
	for _, f := range files[:min(untrackedScanCap, len(files))] {
		if f.Vals["untracked"] == true {
			added += untrackedInsertions(cwd, f.Vals["path"].(string))
		}
	}
	var defaultBranch any
	if d := defaultBranchName(cwd); d != "" {
		defaultBranch = d
	}
	out := pyjson.ObjectOf("branch", branch, "defaultBranch", defaultBranch, "detached", detached, "ahead", ahead, "behind", behind)
	for _, flag := range []string{"staged", "unstaged", "untracked", "conflicted"} {
		n := 0
		for _, f := range files {
			if f.Vals[flag] == true {
				n++
			}
		}
		out.Set(flag, n)
	}
	shown := make([]any, 0, min(200, len(files)))
	for _, f := range files[:min(200, len(files))] {
		shown = append(shown, f)
	}
	out.Set("changed", len(files))
	out.Set("added", added)
	out.Set("removed", removed)
	out.Set("files", shown)
	return out
}

// Rows sorted by path with untracked rows given their insertion count
func reviewResult(cwd string, files []*pyjson.Object, base any) *pyjson.Object {
	sort.SliceStable(files, func(i, j int) bool { return files[i].Vals["path"].(string) < files[j].Vals["path"].(string) })
	rows := make([]any, 0, len(files))
	for _, f := range files {
		if f.Vals["status"] == "?" && f.Vals["added"] == 0 && f.Vals["removed"] == 0 {
			f.Set("added", untrackedInsertions(cwd, f.Vals["path"].(string)))
		}
		rows = append(rows, f)
	}
	return pyjson.ObjectOf("files", rows, "base", base)
}

// Changed files of a review scope in the Electron reviewList shape
func reviewList(cwd, scope string, baseRef *string) *pyjson.Object {
	empty := func() *pyjson.Object { return pyjson.ObjectOf("files", []any{}, "base", nil) }
	if !gitIsDir(cwd) {
		return empty()
	}
	if scope == "branch" || scope == "lastTurn" {
		base := ""
		if scope == "branch" {
			base = branchBase(cwd)
		} else if baseRef != nil {
			base = *baseRef
		}
		if base == "" {
			return empty()
		}
		rng := base
		if scope == "branch" {
			rng = base + "...HEAD"
		}
		counts := numstat(cwd, []string{rng})
		var files []*pyjson.Object
		seen := map[string]bool{}
		for _, path := range counts.keys {
			c := counts.counts[path]
			files = append(files, pyjson.ObjectOf("path", path, "added", c[0], "removed", c[1], "status", "M", "staged", false))
			seen[path] = true
		}
		if scope == "lastTurn" {
			_, raw := statusZ(cwd)
			for _, e := range walkEntries(raw) {
				if e.tag == "?" && !seen[e.path] {
					files = append(files, pyjson.ObjectOf("path", e.path, "added", 0, "removed", 0, "status", "?", "staged", false))
				}
			}
		}
		return reviewResult(cwd, files, base)
	}
	code, raw := statusZ(cwd)
	if code != 0 {
		return empty()
	}
	staged := numstat(cwd, []string{"--cached"})
	unstaged := numstat(cwd, []string{})
	var files []*pyjson.Object
	for _, e := range walkEntries(raw) {
		s, u := staged.get(e.path), unstaged.get(e.path)
		files = append(files, pyjson.ObjectOf("path", e.path, "added", s[0]+u[0], "removed", s[1]+u[1],
			"status", statusLetter(e.tag, e.xy), "staged", entryStaged(e.tag, e.xy)))
	}
	return reviewResult(cwd, files, nil)
}

// Synthesized all add diff of an untracked file
func allAddDiff(cwd, filePath string) string {
	info, err := os.Stat(filepath.Join(cwd, filePath))
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	_, out, _ := gitExec(cwd, []string{"diff", "--no-index", "--", os.DevNull, filePath})
	return out
}

// Diff of exactly one file, refusing a pathspec that expands to a directory
func singleFileDiff(cwd string, args []string, filePath string) (string, error) {
	literal := ":(literal)" + filePath
	names := gitOut(cwd, append(append([]string{"diff"}, args...), "--no-renames", "--name-only", "-z", "--", literal))
	if names == "" {
		return "", nil
	}
	root := gitLine(cwd, []string{"rev-parse", "--show-toplevel"})
	target, _ := filepath.Abs(filepath.Join(cwd, filePath))
	target = gitResolve(target)
	for _, name := range strings.Split(names, "\x00") {
		if name == "" {
			continue
		}
		if gitResolve(filepath.Join(root, name)) != target {
			return "", &GitOpError{"Expected a single file, not a directory"}
		}
	}
	return gitOut(cwd, append(append([]string{"diff"}, args...), "--no-renames", "--", literal)), nil
}

// Non strict pathlib resolve of a path
func gitResolve(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	return abs
}

// Diff of one file in a review scope, an untracked file shown as all added
func reviewDiff(cwd, filePath, scope string, baseRef *string, staged bool) (string, error) {
	if !gitIsDir(cwd) {
		return "", nil
	}
	switch {
	case scope == "branch":
		if base := branchBase(cwd); base != "" {
			return singleFileDiff(cwd, []string{base + "...HEAD"}, filePath)
		}
		return "", nil
	case scope == "lastTurn":
		if baseRef != nil && *baseRef != "" {
			return singleFileDiff(cwd, []string{*baseRef}, filePath)
		}
		return "", nil
	case staged:
		return singleFileDiff(cwd, []string{"--cached"}, filePath)
	}
	worktree, err := singleFileDiff(cwd, []string{}, filePath)
	if err != nil {
		return "", err
	}
	if pystr.Strip(worktree) != "" {
		return worktree, nil
	}
	return allAddDiff(cwd, filePath), nil
}

// Working tree against HEAD diff of one file, all added only for a genuinely untracked file
func fileDiffVsHead(cwd, filePath string) (string, error) {
	if !gitIsDir(cwd) {
		return "", nil
	}
	literal := ":(literal)" + filePath
	head, err := singleFileDiff(cwd, []string{"HEAD"}, filePath)
	if err != nil {
		return "", err
	}
	if pystr.Strip(head) != "" {
		return head, nil
	}
	status := gitOut(cwd, []string{"status", "--porcelain", "--", literal})
	if strings.HasPrefix(pystr.Strip(status), "??") {
		return allAddDiff(cwd, filePath), nil
	}
	return "", nil
}

func gitOkResult(err error) (*pyjson.Object, error) {
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("ok", true), nil
}

func reviewStage(cwd string, filePath *string) (*pyjson.Object, error) {
	if filePath != nil && *filePath != "" {
		return gitOkResult(gitOk(cwd, []string{"add", "--", *filePath}))
	}
	return gitOkResult(gitOk(cwd, []string{"add", "-A"}))
}

func reviewUnstage(cwd string, filePath *string) (*pyjson.Object, error) {
	args := []string{"reset", "-q", "HEAD"}
	if filePath != nil && *filePath != "" {
		args = append(args, "--", *filePath)
	}
	return gitOkResult(gitOk(cwd, args))
}

// Discards changes back to the committed state, restoring tracked and removing untracked files
func reviewRevert(cwd string, filePath *string) *pyjson.Object {
	target := "."
	if filePath != nil && *filePath != "" {
		target = *filePath
	}
	gitExec(cwd, []string{"checkout", "HEAD", "--", target})
	gitExec(cwd, []string{"clean", "-fd", "--", target})
	return pyjson.ObjectOf("ok", true)
}

func reviewRevParse(cwd string, ref *string) any {
	r := "HEAD"
	if ref != nil && *ref != "" {
		r = *ref
	}
	if sha := gitLine(cwd, []string{"rev-parse", r}); sha != "" {
		return sha
	}
	return nil
}

func hasStaged(raw string) bool {
	for _, e := range walkEntries(raw) {
		if entryStaged(e.tag, e.xy) {
			return true
		}
	}
	return false
}

var gitIdentRE = regexp.MustCompile(`\A(.*) <([^<>]*)> -?\d+ [+-]\d{4}\z`)

// Commit environment carrying git's effective author and committer identity into the isolated commit
func reviewCommitEnv(cwd string) (map[string]string, error) {
	base := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			base[k] = v
		}
	}
	env := tools.NoninteractiveGitEnv(base)
	probeEnv := map[string]string{}
	for k, v := range env {
		probeEnv[k] = v
	}
	for _, key := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "GIT_CONFIG_NOSYSTEM"} {
		if v, ok := base[key]; ok {
			probeEnv[key] = v
		} else {
			delete(probeEnv, key)
		}
	}
	for _, role := range []string{"AUTHOR", "COMMITTER"} {
		proc := gitRun([]string{"git", "var", "GIT_" + role + "_IDENT"}, cwd, 5, probeEnv)
		if proc == nil {
			return nil, &GitOpError{gitIdentityLookupFailedMessage}
		}
		if proc.returncode != 0 {
			msg := pystr.Strip(proc.stderr)
			if msg == "" {
				msg = gitIdentityLookupFailedMessage
			}
			return nil, &GitOpError{msg}
		}
		m := gitIdentRE.FindStringSubmatch(pystr.Strip(proc.stdout))
		if m == nil {
			return nil, &GitOpError{"git returned an invalid commit identity"}
		}
		env["GIT_"+role+"_NAME"], env["GIT_"+role+"_EMAIL"] = m[1], m[2]
	}
	return env, nil
}

// Commits the working tree, staging everything first when nothing is staged, then pushes when asked
func reviewCommit(cwd, message string, push bool) (*pyjson.Object, error) {
	env, err := reviewCommitEnv(cwd)
	if err != nil {
		return nil, err
	}
	if _, raw := statusZ(cwd); !hasStaged(raw) {
		if err := gitOk(cwd, []string{"add", "-A"}); err != nil {
			return nil, err
		}
	}
	proc := gitRun(append([]string{"git"}, tools.HardenGitArgv([]string{"commit", "-m", message})...), cwd, gitTimeout, env)
	if proc == nil || proc.returncode != 0 {
		msg := ""
		if proc != nil {
			msg = pystr.Strip(proc.stderr)
		}
		if msg == "" {
			msg = "git commit failed"
		}
		return nil, &GitOpError{msg}
	}
	if push {
		if err := reviewPushBranch(cwd); err != nil {
			return nil, err
		}
	}
	return pyjson.ObjectOf("ok", true), nil
}

// Pushes to the upstream, or publishes the current branch to origin when it has none
func reviewPushBranch(cwd string) error {
	if gitLine(cwd, []string{"rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"}) != "" {
		return gitOk(cwd, []string{"push"})
	}
	if branch := gitLine(cwd, []string{"rev-parse", "--abbrev-ref", "HEAD"}); branch != "" && branch != "HEAD" {
		return gitOk(cwd, []string{"push", "-u", "origin", branch})
	}
	return nil
}

func reviewPush(cwd string) (*pyjson.Object, error) { return gitOkResult(reviewPushBranch(cwd)) }

// Diff of what will commit and recent subjects, credential paths dropped when a path filter is given
func reviewCommitContext(cwd string, pathAllowed func(string) bool) *pyjson.Object {
	code, raw := 1, ""
	if gitIsDir(cwd) {
		code, raw = statusZ(cwd)
	}
	if code != 0 {
		return pyjson.ObjectOf("diff", "", "recent", "")
	}
	entries := walkEntries(raw)
	args := []string{"diff", "HEAD"}
	if hasStaged(raw) {
		args = []string{"diff", "--cached"}
	}
	var diff string
	if pathAllowed == nil {
		diff = gitOut(cwd, args)
	} else {
		repo := gitLine(cwd, []string{"rev-parse", "--show-toplevel"})
		if repo == "" {
			return pyjson.ObjectOf("diff", "", "recent", "")
		}
		var allowed []string
		for _, name := range strings.Split(gitOut(repo, append(append([]string{}, args...), "--no-renames", "--name-only", "-z")), "\x00") {
			if name != "" && pathAllowed(filepath.Join(repo, name)) {
				allowed = append(allowed, ":(literal)"+name)
			}
		}
		if len(allowed) > 0 {
			diff = gitOut(repo, append(append(append([]string{}, args...), "--no-renames", "--"), allowed...))
		}
		kept := entries[:0:0]
		for _, e := range entries {
			if pathAllowed(filepath.Join(repo, e.path)) {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	if runes := []rune(diff); len(runes) > commitContextDiffMaxChars {
		omitted := len(runes) - commitContextDiffMaxChars
		diff = fmt.Sprintf("%s\n# diff truncated: %d chars omitted\n", string(runes[:commitContextDiffMaxChars]), omitted)
	}
	var untracked []string
	for _, e := range entries {
		if e.tag == "?" {
			untracked = append(untracked, e.path)
		}
	}
	if len(untracked) > 0 {
		visible := untracked[:min(commitContextUntrackedMax, len(untracked))]
		diff += "\n# New (untracked) files:\n"
		for _, p := range visible {
			diff += "#   " + p + "\n"
		}
		if len(untracked) > len(visible) {
			diff += fmt.Sprintf("#   ... %d more omitted\n", len(untracked)-len(visible))
		}
	}
	return pyjson.ObjectOf("diff", diff, "recent", gitLine(cwd, []string{"log", "-n", "10", "--pretty=format:%s"}))
}

// Ok flag, stdout and stderr of gh in cwd, never failing on a non zero exit
func ghCall(cwd string, args []string) (bool, string, string) {
	if _, err := exec.LookPath("gh"); err != nil {
		return false, "", ""
	}
	env := tools.NoninteractiveGitEnv(nil)
	env["GH_PROMPT_DISABLED"] = "1"
	proc := gitRun(append([]string{"gh"}, args...), cwd, ghTimeout, env)
	if proc == nil {
		return false, "", ""
	}
	return proc.returncode == 0, proc.stdout, proc.stderr
}

// Parsed JSON stdout of a successful gh call, nil otherwise
func ghJSON(cwd string, args []string) any {
	ok, out, _ := ghCall(cwd, args)
	if !ok {
		return nil
	}
	v, err := pyjson.Loads(out)
	if err != nil {
		return nil
	}
	return v
}

// Value of a key when v is a dict, nil otherwise
func ghGet(v any, key string) any {
	if o, ok := v.(*pyjson.Object); ok && o != nil {
		return o.Vals[key]
	}
	return nil
}

// Whether gh is installed and authenticated plus the current branch's PR
func reviewShipInfo(cwd string) *pyjson.Object {
	if !gitIsDir(cwd) {
		return pyjson.ObjectOf("ghReady", false, "pr", nil)
	}
	if ok, _, _ := ghCall(cwd, []string{"auth", "status"}); !ok {
		return pyjson.ObjectOf("ghReady", false, "pr", nil)
	}
	pr := ghJSON(cwd, []string{"pr", "view", "--json", "url,state,number"})
	if pyrepr.Truthy(pr) && pyrepr.Truthy(ghGet(pr, "url")) {
		return pyjson.ObjectOf("ghReady", true, "pr", pyjson.ObjectOf("url", ghGet(pr, "url"), "state", ghGet(pr, "state"), "number", ghGet(pr, "number")))
	}
	return pyjson.ObjectOf("ghReady", true, "pr", nil)
}

// JSON text of a GraphQL string literal, the Python json.dumps
func ghJSONString(s string) string {
	out, _ := pyjson.Dumps(s)
	return out
}

// GraphQL query asking each branch and each PR number of one repository
func prQuery(owner, name string, branches []string, numbers []int64) string {
	var fields []string
	for i, branch := range branches {
		fields = append(fields, fmt.Sprintf("b%d: pullRequests(headRefName: %s, first: 5, orderBy: {field: CREATED_AT, direction: DESC}) { nodes { %s } }",
			i, ghJSONString(branch), prNodeFields))
	}
	for i, n := range numbers {
		fields = append(fields, fmt.Sprintf("n%d: pullRequest(number: %d) { %s }", i, n, prNodeFields))
	}
	return fmt.Sprintf("query { repository(owner: %s, name: %s) {\n", ghJSONString(owner), ghJSONString(name)) +
		strings.Join(fields, "\n") + "\n} }"
}

func prPayload(pr any) *pyjson.Object {
	number := int64(0)
	if v := ghGet(pr, "number"); pyrepr.Truthy(v) {
		if n, ok := pyrepr.Int(v); ok && n.IsInt64() {
			number = n.Int64()
		}
	}
	text := func(key string) string {
		if v := ghGet(pr, key); pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
		return ""
	}
	return pyjson.ObjectOf("branch", pyrepr.Str(ghGet(pr, "headRefName")), "draft", pyrepr.Truthy(ghGet(pr, "isDraft")),
		"number", number, "state", pystr.Lower(text("state")), "title", text("title"), "url", text("url"))
}

// PR an alias resolved to, by number taken as is and by branch only an own repository node
func ownPr(key string, field any) any {
	if strings.HasPrefix(key, "n") {
		return field
	}
	nodes, _ := ghGet(field, "nodes").([]any)
	for _, n := range nodes {
		if pyrepr.Truthy(n) && !pyrepr.Truthy(ghGet(n, "isCrossRepository")) {
			return n
		}
	}
	return nil
}

// PRs on the given branches plus any asked for by number, queried per branch in chunks
func reviewPrList(cwd string, branches []string, numbers []int64) *pyjson.Object {
	notReady := func() *pyjson.Object { return pyjson.ObjectOf("ghReady", false, "prs", []any{}) }
	if !gitIsDir(cwd) {
		return notReady()
	}
	var wanted []string
	seenB := map[string]bool{}
	for _, b := range branches {
		if b != "" && !seenB[b] {
			seenB[b] = true
			wanted = append(wanted, b)
		}
	}
	wanted = wanted[:min(prQueryBranchCap, len(wanted))]
	var byNumber []int64
	seenN := map[int64]bool{}
	for _, n := range numbers {
		if n != 0 && !seenN[n] {
			seenN[n] = true
			byNumber = append(byNumber, n)
		}
	}
	byNumber = byNumber[:min(prQueryBranchCap, len(byNumber))]
	if len(wanted) == 0 && len(byNumber) == 0 {
		return notReady()
	}
	repoOK, repoOut, _ := ghCall(cwd, []string{"repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner"})
	owner, name, _ := strings.Cut(pystr.Strip(repoOut), "/")
	if !repoOK || owner == "" || name == "" {
		return notReady()
	}
	type chunk struct {
		branches []string
		numbers  []int64
	}
	var chunks []chunk
	for i := 0; i < len(wanted); i += prQueryBranchChunk {
		chunks = append(chunks, chunk{branches: wanted[i:min(i+prQueryBranchChunk, len(wanted))]})
	}
	for i := 0; i < len(byNumber); i += prQueryBranchChunk {
		chunks = append(chunks, chunk{numbers: byNumber[i:min(i+prQueryBranchChunk, len(byNumber))]})
	}
	prs := []any{}
	for _, c := range chunks {
		data := ghJSON(cwd, []string{"api", "graphql", "-f", "query=" + prQuery(owner, name, c.branches, c.numbers)})
		repository, _ := ghGet(ghGet(data, "data"), "repository").(*pyjson.Object)
		if repository == nil {
			continue
		}
		for _, key := range repository.Keys {
			field := repository.Vals[key]
			var pr any
			if pyrepr.Truthy(field) {
				pr = ownPr(key, field)
			}
			if pyrepr.Truthy(pr) && pyrepr.Truthy(ghGet(pr, "headRefName")) {
				prs = append(prs, prPayload(pr))
			}
		}
	}
	return pyjson.ObjectOf("ghReady", true, "prs", prs)
}

// Creates a PR for the current branch after a push, failing with the tail of gh's stderr
func reviewCreatePr(cwd string) (*pyjson.Object, error) {
	_ = reviewPushBranch(cwd)
	created, out, errText := ghCall(cwd, []string{"pr", "create", "--fill"})
	if !created {
		tail := []rune(pystr.Strip(errText))
		detail := string(tail[max(0, len(tail)-ghErrTailChars):])
		if detail == "" {
			detail = "is gh installed and authenticated?"
		}
		return nil, &GitOpError{"gh pr create failed: " + detail}
	}
	url := ""
	lines := pystr.Splitlines(pystr.Strip(out), false)
	for i := len(lines) - 1; i >= 0; i-- {
		if pystr.Strip(lines[i]) != "" {
			url = lines[i]
			break
		}
	}
	return pyjson.ObjectOf("url", url), nil
}

// One object per worktree of git worktree list, the main tree first
func worktreeList(cwd string) []*pyjson.Object {
	var trees []*pyjson.Object
	for _, line := range strings.Split(gitOut(cwd, []string{"worktree", "list", "--porcelain"}), "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			trees = append(trees, pyjson.ObjectOf("path", pystr.Strip(line[9:]), "branch", nil, "isMain", len(trees) == 0,
				"detached", false, "locked", false))
		case len(trees) == 0:
		case strings.HasPrefix(line, "branch "):
			trees[len(trees)-1].Set("branch", strings.Replace(pystr.Strip(line[7:]), "refs/heads/", "", 1))
		case line == "detached":
			trees[len(trees)-1].Set("detached", true)
		case strings.HasPrefix(line, "locked"):
			trees[len(trees)-1].Set("locked", true)
		}
	}
	return trees
}

func mainRoot(cwd string) string {
	if trees := worktreeList(cwd); len(trees) > 0 {
		return trees[0].Vals["path"].(string)
	}
	return cwd
}

var branchSanitizers = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`[\s\x0b\x1c-\x1f\x{85}\p{Z}]+`), "-"}, {regexp.MustCompile(`[^\pL\pN_./-]`), ""}, {regexp.MustCompile(`-{2,}`), "-"},
	{regexp.MustCompile(`/{2,}`), "/"}, {regexp.MustCompile(`\.{2,}`), "."}, {regexp.MustCompile(`^[-./]+|[-./]+$`), ""},
}

func sanitizeBranch(name string) string {
	value := name
	for _, s := range branchSanitizers {
		value = s.re.ReplaceAllString(value, s.repl)
	}
	return value
}

// Refspec that always writes the remote tracking ref, the update_cmd_check tracking_refspec
func gitTrackingRefspec(remote, branch string) string {
	return "+refs/heads/" + branch + ":refs/remotes/" + remote + "/" + branch
}

// Fetches one remote branch by explicit refspec, true when the remote has it
func fetchTrackingRef(root, remote, branch string) bool {
	code, _, _ := gitExec(root, []string{"fetch", remote, gitTrackingRefspec(remote, branch)})
	return code == 0
}

var slugNonAlnumRE = regexp.MustCompile(`[^a-z0-9]+`)
var slugEdgeDashRE = regexp.MustCompile(`^-+|-+$`)

func slugify(name string) string {
	slug := slugNonAlnumRE.ReplaceAllString(pystr.Lower(pystr.Strip(name)), "-")
	slug = slugEdgeDashRE.ReplaceAllString(slug, "")
	if r := []rune(slug); len(r) > 40 {
		slug = string(r[:40])
	}
	slug = strings.TrimRight(slug, "-")
	if slug == "" {
		return "work"
	}
	return slug
}

func defaultBranch(cwd string) string {
	if b := strings.Replace(originHead(cwd), "origin/", "", 1); b != "" {
		return b
	}
	if b := gitLine(cwd, []string{"config", "--get", "init.defaultBranch"}); b != "" {
		return b
	}
	for _, b := range trunkBranches {
		if gitLine(cwd, []string{"show-ref", "--verify", "refs/heads/" + b}) != "" {
			return b
		}
	}
	return ""
}

// Inits a folder that is not a repo, or has no commit, with an empty root commit
func ensureRepo(cwd string) error {
	needsRoot := false
	if gitLine(cwd, []string{"rev-parse", "--is-inside-work-tree"}) != "true" {
		if err := gitOk(cwd, []string{"init"}); err != nil {
			return err
		}
		needsRoot = true
	} else {
		needsRoot = !refExists(cwd, "HEAD")
	}
	if needsRoot {
		return gitOk(cwd, []string{"-c", "user.email=icebath@localhost", "-c", "user.name=Icebath",
			"commit", "--allow-empty", "-m", "Initial commit"})
	}
	return nil
}

func uniqueDir(base string) string {
	if _, err := os.Lstat(base); err != nil {
		return base
	}
	for n := 2; ; n++ {
		c := base + "-" + strconv.Itoa(n)
		if _, err := os.Lstat(c); err != nil {
			return c
		}
	}
}

// Remote a remote tracking ref belongs to, empty when the name is not one here
func remoteOfRef(cwd, name string) string {
	if !strings.Contains(name, "/") {
		return ""
	}
	if code, _, _ := gitExec(cwd, []string{"show-ref", "--verify", "--quiet", "refs/remotes/" + name}); code != 0 {
		return ""
	}
	return strings.SplitN(name, "/", 2)[0]
}

// Checks out an existing local or remote tracking branch into a worktree, or switches the main tree to trunk
func worktreeForExisting(root, rawName string) (*pyjson.Object, error) {
	requested := sanitizeBranch(rawName)
	if requested == "" {
		return nil, &GitOpError{"Branch name is required."}
	}
	remote := remoteOfRef(root, requested)
	fetched := false
	if remote == "" && strings.Contains(requested, "/") && !refExists(root, "refs/heads/"+requested) {
		maybeRemote, maybeBranch, _ := strings.Cut(requested, "/")
		if gitLine(root, []string{"remote", "get-url", maybeRemote}) != "" && fetchTrackingRef(root, maybeRemote, maybeBranch) {
			remote, fetched = maybeRemote, true
		}
	}
	existing := requested
	if remote != "" {
		existing = strings.SplitN(requested, "/", 2)[1]
	}
	if remote == "" && existing == defaultBranch(root) {
		if err := gitOk(root, []string{"switch", existing}); err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("path", root, "branch", existing, "repoRoot", root), nil
	}
	target := uniqueDir(filepath.Join(root, ".worktrees", slugify(existing)))
	if remote != "" {
		fetched = fetched || fetchTrackingRef(root, remote, existing)
		if code, _, _ := gitExec(root, []string{"worktree", "add", "--track", "-b", existing, target, requested}); code != 0 {
			if err := gitOk(root, []string{"worktree", "add", "-b", existing, target, requested}); err != nil {
				return nil, err
			}
			if fetched {
				gitExec(root, []string{"remote", "set-branches", "--add", remote, existing})
				gitExec(root, []string{"branch", "--set-upstream-to=" + requested, existing})
			}
		}
	} else if err := gitOk(root, []string{"worktree", "add", target, existing}); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("path", target, "branch", existing, "repoRoot", root), nil
}

// Option text of a worktree add request, empty when unset or falsy
func worktreeOption(options *pyjson.Object, key string) string {
	if options == nil {
		return ""
	}
	if v := options.Vals[key]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return ""
}

// Adds a worktree for a new or existing branch and records the repo in the worktree registry
func worktreeAdd(ctx context.Context, cwd string, options *pyjson.Object) (*pyjson.Object, error) {
	if err := ensureRepo(cwd); err != nil {
		return nil, err
	}
	root := mainRoot(cwd)
	if existing := worktreeOption(options, "existingBranch"); existing != "" {
		result, err := worktreeForExisting(root, existing)
		if err != nil {
			return nil, err
		}
		if result.Vals["path"] != root {
			cron.RecordWorktreeRepo(ctx, home.Dir(ctx), root)
		}
		return result, nil
	}
	name := worktreeOption(options, "name")
	if name == "" {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		name = "work-" + hex.EncodeToString(b)
	}
	slug := slugify(name)
	branch := sanitizeBranch(worktreeOption(options, "branch"))
	if branch == "" {
		branch = "icebath/" + slug
	}
	target := uniqueDir(filepath.Join(root, ".worktrees", slug))
	args := []string{"worktree", "add", "-b", branch, target}
	if base := worktreeOption(options, "base"); base != "" {
		if strings.HasPrefix(base, "origin/") {
			remoteBranch := base[len("origin/"):]
			if code, _, _ := gitExec(root, []string{"check-ref-format", "--branch", remoteBranch}); code == 0 {
				fetchTrackingRef(root, "origin", remoteBranch)
			}
			args = append(args, "--no-track")
		}
		args = append(args, base)
	}
	if code, _, errText := gitExec(root, args); code != 0 {
		if !strings.Contains(pystr.Lower(errText), "already exists") {
			msg := pystr.Strip(errText)
			if msg == "" {
				msg = "git worktree add failed"
			}
			return nil, &GitOpError{msg}
		}
		if err := gitOk(root, []string{"worktree", "add", target, branch}); err != nil {
			return nil, err
		}
	}
	cron.RecordWorktreeRepo(ctx, home.Dir(ctx), root)
	return pyjson.ObjectOf("path", target, "branch", branch, "repoRoot", root), nil
}

func worktreeRemove(cwd, worktreePath string, force bool) (*pyjson.Object, error) {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	if err := gitOk(mainRoot(cwd), append(args, worktreePath)); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("removed", worktreePath), nil
}

// Non empty for-each-ref lines, newest commit first
func refNames(cwd, format string, patterns ...string) []string {
	out := gitOut(cwd, append([]string{"for-each-ref", "--format=" + format, "--sort=-committerdate"}, patterns...))
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if s := pystr.Strip(line); s != "" {
			names = append(names, s)
		}
	}
	return names
}

// Local heads then remote tracking refs with no local head, for the convert a branch picker
func branchList(cwd string) []any {
	locals := refNames(cwd, "%(refname:short)", "refs/heads")
	if len(locals) == 0 {
		return []any{}
	}
	pathByBranch := map[string]string{}
	for _, t := range worktreeList(cwd) {
		if b, ok := t.Vals["branch"].(string); ok && b != "" {
			pathByBranch[b] = t.Vals["path"].(string)
		}
	}
	trunk := defaultBranch(cwd)
	localSet := map[string]bool{}
	for _, n := range locals {
		localSet[n] = true
	}
	out := []any{}
	for _, name := range locals {
		path, checked := pathByBranch[name]
		var wt any
		if checked {
			wt = path
		}
		out = append(out, pyjson.ObjectOf("name", name, "checkedOut", checked, "isDefault", trunk != "" && name == trunk,
			"isRemote", false, "worktreePath", wt))
	}
	for _, name := range refNames(cwd, "%(refname:short)", "refs/remotes") {
		parts := strings.SplitN(name, "/", 2)
		if strings.HasSuffix(name, "/HEAD") || localSet[parts[len(parts)-1]] {
			continue
		}
		out = append(out, pyjson.ObjectOf("name", name, "checkedOut", false, "isDefault", false, "isRemote", true, "worktreePath", nil))
	}
	return out
}

func branchSwitch(cwd, branch string) (*pyjson.Object, error) {
	target := sanitizeBranch(branch)
	if target == "" {
		return nil, &GitOpError{"Branch name is required."}
	}
	if err := gitOk(cwd, []string{"switch", target}); err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("branch", target), nil
}

// Local heads and remote tracking refs for the base branch picker, the remote default flagged
func baseBranchList(cwd string) []any {
	lines := refNames(cwd, "%(refname:short)\t%(committerdate:iso)", "refs/heads", "refs/remotes")
	if len(lines) == 0 {
		return []any{}
	}
	def := originHead(cwd)
	if def == "" {
		def = defaultBranch(cwd)
	}
	out := []any{}
	for _, line := range lines {
		name, _, _ := strings.Cut(line, "\t")
		out = append(out, pyjson.ObjectOf("name", name, "isRemote", strings.HasPrefix(name, "origin/"), "isDefault", def != "" && name == def))
	}
	return out
}
