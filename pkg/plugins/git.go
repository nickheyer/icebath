package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gitcred"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

var exactCommitRE = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

var sparseEscapeRE = regexp.MustCompile(`([\\*?\[])`)

const gitDefaultTimeout = 60 * time.Second

// Completed git subprocess with its exit code and utf-8 replace decoded output
type gitResult struct {
	Code           int
	Stdout, Stderr string
}

// Git subprocess killed at its deadline, the subprocess.TimeoutExpired of hermes
type GitTimeoutError struct{ Timeout time.Duration }

func (e *GitTimeoutError) Error() string {
	return fmt.Sprintf("git timed out after %g seconds", e.Timeout.Seconds())
}

// Git executable missing at spawn, the FileNotFoundError of hermes
var errGitNotFound = errors.New("git is not installed or not in PATH.")

func installMetadataPath(h string) string {
	return filepath.Join(home.PluginsDir(h), ".install-metadata.json")
}

// Replaces the profile local plugin install metadata atomically with sorted two space JSON
func writeInstallMetadata(h string, metadata map[string]any) error {
	text, err := pyjson.Dumps(metadata, pyjson.Indent(2), pyjson.SortKeys(true))
	if err != nil {
		return err
	}
	path := installMetadataPath(h)
	return pyfs.AtomicWriteText(path, text+"\n", filepath.Base(path)+".tmp-")
}

type metadataLockKey struct{ path string }

// Runs fn holding the cross process install metadata lock, reentrant along one context chain
func withInstallMetadataLock(ctx context.Context, h string, fn func(context.Context) error) error {
	path := installMetadataPath(h) + ".lock"
	key := metadataLockKey{path}
	if ctx.Value(key) != nil {
		return fn(ctx)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	defer f.Close()
	held, err := pm.LockFD(f, true, 10*time.Second)
	if err != nil {
		return err
	}
	if !held {
		message := "Timed out waiting for the plugin install metadata lock"
		if hint := lockHolderHint(path); hint != "" {
			message += "; " + hint
		}
		return &pm.PyError{Class: "TimeoutError", Message: message}
	}
	if err := f.Truncate(0); err == nil {
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
		_ = f.Sync()
	}
	defer func() {
		_ = f.Truncate(0)
		_ = f.Sync()
		pm.UnlockFD(f)
	}()
	return fn(context.WithValue(ctx, key, true))
}

// Names the live process whose pid a holder stamped into the lock file, empty for none or a dead one
func lockHolderHint(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	fields := strings.FieldsFunc(strings.TrimPrefix(strings.ToValidUTF8(string(raw), "\ufffd"), "\ufeff"), pystr.IsSpace)
	if len(fields) == 0 {
		return ""
	}
	var pid int
	if _, err := fmt.Sscanf(strings.ReplaceAll(fields[0], "_", ""), "%d", &pid); err != nil || pid <= 0 || pid == os.Getpid() {
		return ""
	}
	if !proc.Exists(pid) {
		return ""
	}
	return fmt.Sprintf("another icebath process (pid %d) probably still holds it", pid)
}

// Rewrites one plugin record of the current metadata under the lock, a nil result removing it
func updateInstallRecord(ctx context.Context, h, name string, update func(map[string]any) map[string]any) error {
	return withInstallMetadataLock(ctx, h, func(context.Context) error {
		metadata, err := readInstallMetadata(h)
		if err != nil {
			return err
		}
		current, _ := metadata[name].(map[string]any)
		record := update(current)
		if record == nil {
			if _, ok := metadata[name]; !ok {
				return nil
			}
			delete(metadata, name)
		} else {
			metadata[name] = record
		}
		return writeInstallMetadata(h, metadata)
	})
}

// Full sha a ref install of the plugin is pinned to, empty when it is not pinned
func pinnedRevision(h, name string, metadata map[string]any) (string, error) {
	if metadata == nil {
		var err error
		if metadata, err = readInstallMetadata(h); err != nil {
			return "", err
		}
	}
	entry, _ := metadata[name].(map[string]any)
	if pinned, ok := entry["pinned"].(bool); ok && pinned {
		if rev, ok := entry["revision"].(string); ok {
			return rev, nil
		}
	}
	return "", nil
}

func pinAnnotation(h, name string, metadata map[string]any) string {
	if sha, _ := pinnedRevision(h, name, metadata); sha != "" {
		return "git pinned@" + sha[:min(8, len(sha))]
	}
	return ""
}

// Lowercased full 40 hex commit sha, an operation error for anything else
func normalizeExactRevision(ref string) (string, error) {
	if !exactCommitRE.MatchString(ref) {
		return "", &PluginOperationError{Msg: "--ref must be a full 40-character commit SHA."}
	}
	return strings.ToLower(ref), nil
}

// Diagnosable git output with the source url scrubbed and every credential redacted
func safeGitError(result *gitResult, sourceURL string) (string, error) {
	text := result.Stderr
	if text == "" {
		text = result.Stdout
	}
	text = pystr.Strip(text)
	if sourceURL != "" {
		scrubbed, err := scrubGitURL(sourceURL)
		if err != nil {
			return "", err
		}
		text = strings.ReplaceAll(text, sourceURL, scrubbed)
	}
	return tools.RedactSensitiveText(text), nil
}

type gitCall struct {
	timeout   time.Duration
	sourceURL string
	authURL   string
}

// Runs git in the repo, a nonzero exit becoming an operation error of the prefix and scrubbed output
func gitOrRaise(ctx context.Context, gitExe, repo, failurePrefix string, call gitCall, args ...string) (*gitResult, error) {
	if call.timeout == 0 {
		call.timeout = gitDefaultTimeout
	}
	result, err := runPluginGit(ctx, gitExe, repo, call.timeout, call.authURL, args...)
	if err != nil {
		return nil, err
	}
	if result.Code != 0 {
		detail, err := safeGitError(result, call.sourceURL)
		if err != nil {
			return nil, err
		}
		return nil, &PluginOperationError{Msg: failurePrefix + detail, FailureClass: "clone_failed"}
	}
	return result, nil
}

func gitHeadRevision(ctx context.Context, repo, gitExe string) (string, error) {
	result, err := gitOrRaise(ctx, gitExe, repo, "Could not determine installed Git revision:\n", gitCall{timeout: 15 * time.Second}, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.ToLower(pystr.Strip(result.Stdout)), nil
}

// Commit a revision names with annotated tags peeled, the revision itself when it resolves to nothing
func gitResolveCommit(ctx context.Context, repo, gitExe, revision string) string {
	result, err := runPluginGit(ctx, gitExe, repo, 15*time.Second, "", "rev-parse", "--verify", "--quiet", revision+"^{commit}")
	if err != nil {
		return revision
	}
	resolved := strings.ToLower(pystr.Strip(result.Stdout))
	if result.Code == 0 && resolved != "" {
		return resolved
	}
	return revision
}

// Fetches and detaches at one immutable commit, then verifies HEAD is that commit
func checkoutExactRevision(ctx context.Context, h, repo, gitExe, revision, sourceURL string) error {
	timeout := time.Duration(cloneTimeoutSeconds(h)) * time.Second
	steps := []struct {
		verb, prefix string
		args         []string
	}{
		{"fetch", fmt.Sprintf("Git commit '%s' could not be fetched:\n", revision), []string{"fetch", "--depth", "1", "origin", revision}},
		{"checkout", fmt.Sprintf("Git checkout of commit '%s' failed:\n", revision), []string{"checkout", "--detach", revision}},
	}
	for _, step := range steps {
		_, err := gitOrRaise(ctx, gitExe, repo, step.prefix, gitCall{timeout: timeout, sourceURL: sourceURL, authURL: sourceURL}, step.args...)
		var te *GitTimeoutError
		if errors.As(err, &te) {
			return &PluginOperationError{Msg: fmt.Sprintf("Git %s of commit '%s' timed out after %d seconds. %s",
				step.verb, revision, int(timeout.Seconds()), CloneTimeoutHint), FailureClass: "clone_failed"}
		}
		if err != nil {
			return err
		}
	}
	actual, err := gitHeadRevision(ctx, repo, gitExe)
	if err != nil {
		return err
	}
	if actual != gitResolveCommit(ctx, repo, gitExe, revision) {
		return &PluginOperationError{Msg: fmt.Sprintf("Checked-out revision '%s' does not match requested commit '%s'.", actual, revision),
			FailureClass: "clone_failed"}
	}
	return nil
}

// HTTP git url without credentials, query or fragment, other urls unchanged
func scrubGitURL(gitURL string) (string, error) {
	parsed, err := pyurl.Split(gitURL)
	if err != nil {
		return "", err
	}
	hostname := parsed.Hostname()
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || hostname == "" {
		return gitURL, nil
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	port, ok, err := parsed.Port()
	if err != nil {
		return "", err
	}
	if ok {
		host = fmt.Sprintf("%s:%d", host, port)
	}
	return pyurl.Unsplit(pyurl.URL{Scheme: parsed.Scheme, Netloc: host, Path: parsed.Path}), nil
}

func canonicalSource(gitURL, subdir string) (string, error) {
	scrubbed, err := scrubGitURL(gitURL)
	if err != nil || subdir == "" {
		return scrubbed, err
	}
	return scrubbed + "#" + subdir, nil
}

// Rewrites the cloned origin so clone credentials never survive in the git config
func scrubClonedOrigin(ctx context.Context, repo, gitExe, gitURL string) error {
	scrubbed, err := scrubGitURL(gitURL)
	if err != nil || scrubbed == gitURL {
		return err
	}
	_, err = gitOrRaise(ctx, gitExe, repo, "Could not sanitize installed Git remote:\n", gitCall{timeout: 15 * time.Second, sourceURL: gitURL},
		"remote", "set-url", "origin", scrubbed)
	return err
}

// Sparse checks out only the subdirectory through the classic info sparse-checkout file
func restrictCheckoutToSubdir(ctx context.Context, repo, gitExe, subdir string) error {
	if _, err := gitOrRaise(ctx, gitExe, repo, "Could not enable sparse checkout:\n", gitCall{timeout: 15 * time.Second},
		"config", "core.sparseCheckout", "true"); err != nil {
		return err
	}
	patternFile := filepath.Join(repo, ".git", "info", "sparse-checkout")
	if err := os.MkdirAll(filepath.Dir(patternFile), 0o777); err != nil {
		return err
	}
	escaped := sparseEscapeRE.ReplaceAllString(strings.Trim(subdir, "/"), `\$1`)
	return os.WriteFile(patternFile, []byte("/"+escaped+"/\n"), 0o666)
}

// Shallow clones the url into tmpClone, detached at the revision when given, and returns the installed HEAD sha
func clonePluginRepo(ctx context.Context, h, tmpClone, gitURL, revision, subdir string) (string, error) {
	gitExe := resolveGitExecutable()
	if gitExe == "" {
		return "", &PluginOperationError{Msg: "git is not installed or not in PATH.", FailureClass: "git_missing"}
	}
	timeout := time.Duration(cloneTimeoutSeconds(h)) * time.Second
	timedOut := func(what string) error {
		return &PluginOperationError{Msg: fmt.Sprintf("%s timed out after %d seconds. %s", what, int(timeout.Seconds()), CloneTimeoutHint),
			FailureClass: "clone_failed"}
	}
	args := []string{"clone", "--depth", "1"}
	if subdir != "" {
		args = append(args, "--filter=blob:none")
	}
	if revision != "" || subdir != "" {
		args = append(args, "--no-checkout")
	}
	args = append(args, gitURL, tmpClone)
	result, err := runPluginGit(ctx, gitExe, filepath.Dir(tmpClone), timeout, gitURL, args...)
	var te *GitTimeoutError
	switch {
	case errors.Is(err, errGitNotFound):
		return "", &PluginOperationError{Msg: "git is not installed or not in PATH.", FailureClass: "git_missing"}
	case errors.As(err, &te):
		return "", timedOut("Git clone")
	case err != nil:
		return "", err
	}
	if result.Code != 0 {
		detail, err := safeGitError(result, gitURL)
		if err != nil {
			return "", err
		}
		return "", &PluginOperationError{Msg: cloneFailureMessage(gitURL, detail), FailureClass: "clone_failed"}
	}
	if err := scrubClonedOrigin(ctx, tmpClone, gitExe, gitURL); err != nil {
		return "", err
	}
	if subdir != "" {
		if err := restrictCheckoutToSubdir(ctx, tmpClone, gitExe, subdir); err != nil {
			return "", err
		}
	}
	if revision != "" {
		if err := checkoutExactRevision(ctx, h, tmpClone, gitExe, revision, gitURL); err != nil {
			return "", err
		}
	} else if subdir != "" {
		_, err := gitOrRaise(ctx, gitExe, tmpClone, "Git checkout of the plugin subdirectory failed:\n",
			gitCall{timeout: timeout, sourceURL: gitURL, authURL: gitURL}, "checkout", "HEAD")
		if errors.As(err, &te) {
			return "", timedOut("Git checkout")
		}
		if err != nil {
			return "", err
		}
	}
	return gitHeadRevision(ctx, tmpClone, gitExe)
}

// Runs one non interactive git command in a plugin checkout, a stored credential attached only when the remote refuses
var runPluginGit = func(ctx context.Context, gitExe, target string, timeout time.Duration, authURL string, args ...string) (*gitResult, error) {
	return gitWithCredentialFallback(ctx, append([]string{gitExe}, args...), authURL, tools.NoninteractiveGitEnv(nil), target, timeout)
}

// Runs git anonymously, then with each owned credential while the remote keeps refusing for one
func gitWithCredentialFallback(ctx context.Context, argv []string, url string, env map[string]string, cwd string, timeout time.Duration) (*gitResult, error) {
	env = maps.Clone(env)
	delete(env, "GIT_ASKPASS")
	delete(env, "SSH_ASKPASS")
	run := func(e map[string]string) (*gitResult, error) { return runGitProcess(ctx, argv, e, cwd, timeout) }
	refused := func(r *gitResult) bool { return r.Code != 0 && gitcred.IsCredentialRequiredError(r.Stderr, r.Stdout) }
	result, err := run(env)
	if err != nil || result.Code == 0 || url == "" || !refused(result) {
		return result, err
	}
	authEnv, err := gitcred.WithGitAuth(ctx, env, url, nil)
	if err != nil {
		return nil, err
	}
	if maps.Equal(authEnv, env) {
		return result, nil
	}
	if result, err = run(authEnv); err != nil {
		return nil, err
	}
	sent := gitAuthHeaders(authEnv)
	var rejected []string
	for cred, iterErr := range gitcred.IterGitBasicAuth(ctx, url) {
		if iterErr != nil {
			return nil, iterErr
		}
		if !refused(result) {
			break
		}
		auth := cred.Auth
		candidate, err := gitcred.WithGitAuth(ctx, env, url, &auth)
		if err != nil {
			return nil, err
		}
		headers := gitAuthHeaders(candidate)
		subset := true
		for header := range headers {
			if !sent[header] {
				subset = false
				break
			}
		}
		if subset {
			rejected = append(rejected, cred.Source)
			continue
		}
		maps.Copy(sent, headers)
		origin := url
		if u, err := pyurl.Split(url); err == nil {
			origin = u.Scheme + "://" + u.Netloc
		}
		from := strings.Join(rejected, ", ")
		if from == "" {
			from = "the stored credential"
		}
		slog.Warn(fmt.Sprintf("%s rejected the credential from %s; retrying with %s", origin, from, cred.Source))
		if result, err = run(candidate); err != nil {
			return nil, err
		}
		if refused(result) {
			rejected = append(rejected, cred.Source)
		}
	}
	if result.Code != 0 && slices.Contains(rejected, gitcred.EnvTokenSource) {
		result.Stderr += "\nhint: the GITHUB_TOKEN/GH_TOKEN in your .env was rejected by GitHub;" +
			" replace it or remove it (gh auth login is used when it is absent)."
	}
	return result, nil
}

// Authorization header values the auth env hands git through GIT_CONFIG_VALUE entries
func gitAuthHeaders(env map[string]string) map[string]bool {
	out := map[string]bool{}
	for k, v := range env {
		if strings.HasPrefix(k, "GIT_CONFIG_VALUE_") && strings.HasPrefix(v, "Authorization:") {
			out[v] = true
		}
	}
	return out
}

// Spawns git with captured text output under the timeout, killing it at the deadline
func runGitProcess(ctx context.Context, argv []string, env map[string]string, cwd string, timeout time.Duration) (*gitResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return nil, &GitTimeoutError{Timeout: timeout}
	}
	var exitErr *exec.ExitError
	code := 0
	switch {
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return nil, errGitNotFound
	case err != nil:
		return nil, err
	}
	return &gitResult{Code: code, Stdout: textModeOutput(stdout.Bytes()), Stderr: textModeOutput(stderr.Bytes())}, nil
}

// Decodes subprocess output as utf-8 with replacement and universal newlines
func textModeOutput(b []byte) string {
	s := strings.ToValidUTF8(string(b), "\ufffd")
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// Current refs/stash commit, empty when no stash exists
func stashRef(ctx context.Context, gitExe, target string) (string, error) {
	probe, err := runPluginGit(ctx, gitExe, target, gitDefaultTimeout, "", "rev-parse", "--verify", "refs/stash")
	if err != nil {
		return "", err
	}
	if probe.Code != 0 {
		return "", nil
	}
	return pystr.Strip(probe.Stdout), nil
}

// Applies the autostash commit and drops it on a clean apply, false when it left errors or unmerged paths
func reapplyStash(ctx context.Context, gitExe, target, stashSHA string) (bool, error) {
	restore, err := runPluginGit(ctx, gitExe, target, gitDefaultTimeout, "", "stash", "apply", stashSHA)
	if err != nil {
		return false, err
	}
	unmerged, err := runPluginGit(ctx, gitExe, target, gitDefaultTimeout, "", "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return false, err
	}
	if restore.Code != 0 || pystr.Strip(unmerged.Stdout) != "" {
		return false, nil
	}
	newest, err := stashRef(ctx, gitExe, target)
	if err != nil {
		return false, err
	}
	if newest == stashSHA {
		if _, err := runPluginGit(ctx, gitExe, target, gitDefaultTimeout, "", "stash", "drop"); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Paths porcelain -z output records as intent to add, which a stash push refuses
func intentToAddPaths(porcelainZ string) []string {
	var out []string
	for _, record := range strings.Split(porcelainZ, "\x00") {
		if len(record) > 3 && record[0] == ' ' && record[1] == 'A' {
			out = append(out, record[3:])
		}
	}
	return out
}

// Stashes local edits before a pull, an error text when the tree is dirty and nothing was saved
func autostashDirtyTree(ctx context.Context, gitExe, target string) (string, string, error) {
	status, err := runPluginGit(ctx, gitExe, target, gitDefaultTimeout, "", "status", "--porcelain", "-z")
	if err != nil {
		return "", "", err
	}
	if status.Code != 0 || pystr.Strip(status.Stdout) == "" {
		return "", "", nil
	}
	if paths := intentToAddPaths(status.Stdout); len(paths) > 0 {
		if _, err := runPluginGit(ctx, gitExe, target, gitDefaultTimeout, "", append([]string{"add", "--"}, paths...)...); err != nil {
			return "", "", err
		}
	}
	pre, err := stashRef(ctx, gitExe, target)
	if err != nil {
		return "", "", err
	}
	push, err := runPluginGit(ctx, gitExe, target, gitDefaultTimeout, "", "stash", "push", "--include-untracked", "-m", "icebath-plugin-update-autostash")
	if err != nil {
		return "", "", err
	}
	post, err := stashRef(ctx, gitExe, target)
	if err != nil {
		return "", "", err
	}
	if post == "" || post == pre {
		detail, err := safeGitError(push, "")
		if err != nil {
			return "", "", err
		}
		msg := "Local changes in the plugin checkout could not be stashed; update aborted before touching the checkout."
		if detail != "" {
			msg += "\n" + detail
		}
		return "", msg, nil
	}
	if push.Code != 0 {
		if _, err := runPluginGit(ctx, gitExe, target, gitDefaultTimeout, "", "reset", "--hard", "HEAD"); err != nil {
			return "", "", err
		}
	}
	return post, "", nil
}

// Fast forward pulls a plugin checkout, autostashing local edits and re-applying them afterwards
func gitPullPluginDir(ctx context.Context, target string) (bool, string, error) {
	gitExe := resolveGitExecutable()
	if gitExe == "" {
		return false, "git is not installed or not in PATH.", nil
	}
	ok, msg, err := gitPullSteps(ctx, gitExe, target)
	var te *GitTimeoutError
	switch {
	case errors.Is(err, errGitNotFound):
		return false, "git is not installed or not in PATH.", nil
	case errors.As(err, &te):
		return false, "Git operation timed out after 60 seconds.", nil
	}
	return ok, msg, err
}

func gitPullSteps(ctx context.Context, gitExe, target string) (bool, string, error) {
	stashSHA, stashErr, err := autostashDirtyTree(ctx, gitExe, target)
	if err != nil || stashErr != "" {
		return false, stashErr, err
	}
	origin, err := runPluginGit(ctx, gitExe, target, 15*time.Second, "", "remote", "get-url", "origin")
	if err != nil {
		return false, "", err
	}
	result, err := runPluginGit(ctx, gitExe, target, gitDefaultTimeout, pystr.Strip(origin.Stdout), "pull", "--ff-only")
	if err != nil {
		return false, "", err
	}
	if result.Code != 0 {
		detail, err := safeGitError(result, "")
		if err != nil {
			return false, "", err
		}
		if detail == "" {
			detail = "git pull failed."
		}
		if stashSHA == "" {
			return false, detail, nil
		}
		restored, err := reapplyStash(ctx, gitExe, target, stashSHA)
		if err != nil {
			return false, "", err
		}
		note := "Local changes are preserved in git stash (restore with: git stash pop)."
		if restored {
			note = "Local changes were restored."
		}
		return false, detail + "\n" + note, nil
	}
	pulled := pystr.Strip(result.Stdout)
	if stashSHA == "" {
		return true, pulled, nil
	}
	restored, err := reapplyStash(ctx, gitExe, target, stashSHA)
	if err != nil {
		return false, "", err
	}
	if restored {
		return true, pulled + "\nLocal changes were re-applied on top of the update.", nil
	}
	if _, err := runPluginGit(ctx, gitExe, target, gitDefaultTimeout, "", "reset", "--hard", "HEAD"); err != nil {
		return false, "", err
	}
	return true, pulled + "\n\u26a0 Local changes in this plugin conflicted with the update and " +
		"were NOT re-applied. They are preserved in git stash \u2014 inspect " +
		"with `git stash show -p` and re-apply with " +
		"`git stash pop` inside " + target + ".", nil
}
