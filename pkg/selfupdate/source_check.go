package selfupdate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	UpdateAvailableNoCount         = updaterecord.UpdateAvailableNoCount
	updateCheckCacheSeconds        = 24 * 3600
	updateCheckFailureCacheSeconds = 3600
)

var (
	runtimeLookupTimeout = 8 * time.Second
	sourceCheckClock     = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
	checkGitExecutable   = updaterecord.GitExecutable
	checkProjectRoot     = pm.RepoRoot
)

// Read only git state without prompts or inherited targeting, nil when git could not run
func checkGitRun(ctx context.Context, args []string, cwd string, timeout time.Duration, git string) *sourceGitResult {
	result, err := sourceGitRun(ctx, gitArgs([]string{git}, args...), cwd, timeout)
	if err != nil {
		return nil
	}
	return result
}

// Stripped stdout of a successful git query, false when it failed
func checkGitStdout(ctx context.Context, args []string, cwd, git string) (string, bool) {
	result := checkGitRun(ctx, args, cwd, 5*time.Second, git)
	if result == nil || result.ReturnCode != 0 {
		return "", false
	}
	return strings.TrimSpace(result.Stdout), true
}

// Reports whether git ran and exited 0
func checkGitOK(ctx context.Context, args []string, cwd, git string) bool {
	result := checkGitRun(ctx, args, cwd, 5*time.Second, git)
	return result != nil && result.ReturnCode == 0
}

// Integer a successful count style git query prints, false otherwise
func checkGitCount(ctx context.Context, args []string, cwd, git string) (int64, bool) {
	result := checkGitRun(ctx, args, cwd, 5*time.Second, git)
	if result == nil || result.ReturnCode != 0 {
		return 0, false
	}
	value, ok := pystr.IntText(strings.TrimSpace(result.Stdout))
	if !ok || !value.IsInt64() {
		return 0, false
	}
	return value.Int64(), true
}

// Stripped stdout of a read only git query in root within five seconds, empty when git failed
func GitStdout(args []string, root string) string {
	out, _ := GitStdoutWithin(context.Background(), args, root, 5*time.Second)
	return out
}

// Stripped stdout of a read only git query in root within the timeout, false when git failed
func GitStdoutWithin(ctx context.Context, args []string, root string, timeout time.Duration) (string, bool) {
	result := checkGitRun(ctx, args, root, timeout, "git")
	if result == nil || result.ReturnCode != 0 {
		return "", false
	}
	return strings.TrimSpace(result.Stdout), true
}

// Integer a successful count style git query in root prints, false when git failed or printed no integer
func GitCount(ctx context.Context, args []string, root string) (int64, bool) {
	return checkGitCount(ctx, args, root, "git")
}

// Remote tip of branch, whether the remote confirmed it absent, and why no tip could be read
func branchTip(ctx context.Context, repository, branch, root, git, remote string) (string, bool, string) {
	failure := ""
	if repository != "" {
		sha, err := sourceRequest(ctx, sourceGitHubAPI+"/repos/"+repository+"/commits/"+pyurl.Quote(branch, ""), "application/vnd.github.sha")
		if err != nil {
			sha = ""
			_, tokened := githubTokenSource()
			failure = plugins.DescribeGithubFailure(err, tokened, nil)
		}
		if isFullSHA(sha) {
			return sha, false, ""
		}
		if failure == "" {
			failure = "api.github.com returned no commit for the branch."
		}
		if branch == "main" && remote == "origin" {
			return "", false, failure
		}
	}
	result := checkGitRun(ctx, []string{"ls-remote", "--exit-code", "--heads", remote, "refs/heads/" + branch}, root, 10*time.Second, git)
	if result == nil {
		return "", false, firstNonEmpty(failure, "`git ls-remote "+remote+"` could not run.")
	}
	sha := ""
	if fields := strings.Fields(result.Stdout); result.ReturnCode == 0 && len(fields) > 0 {
		sha = fields[0]
	}
	if isFullSHA(sha) {
		return sha, false, ""
	}
	if result.ReturnCode == 2 {
		return "", true, ""
	}
	detail := textutil.Lines(strings.TrimSpace(result.Stderr))
	if len(detail) > 0 {
		return "", false, firstNonEmpty(failure, "`git ls-remote "+remote+"` failed: "+detail[len(detail)-1])
	}
	return "", false, firstNonEmpty(failure, "`git ls-remote "+remote+"` returned no tip.")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// Milliseconds since the epoch of an ISO 8601 time, naive times read as local
func isoMillis(when string) (int64, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00"} {
		if parsed, err := time.Parse(layout, when); err == nil {
			return parsed.UnixMilli(), true
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, when, time.Local); err == nil {
			return parsed.UnixMilli(), true
		}
	}
	return 0, false
}

// Passive update check the banner runs, set by the updater and nil in a build without it
var BannerUpdateCheck func(ctx context.Context) (*pyjson.Object, error)

// Commit rows of a GitHub comparison, oldest last, false when a commit entry is malformed
func compareCommits(payload *pyjson.Object) ([]any, bool) {
	var entries []any
	if payload != nil {
		if value, ok := payload.Get("commits"); ok {
			entries, _ = value.([]any)
		}
	}
	rows := []any{}
	for _, item := range entries {
		entry, ok := item.(*pyjson.Object)
		if !ok {
			continue
		}
		sha, ok := stampGet(entry, "sha").(string)
		if !ok {
			continue
		}
		commit := pyjson.NewObject()
		if value := stampGet(entry, "commit"); pyTruthy(value) {
			if commit, ok = value.(*pyjson.Object); !ok {
				return nil, false
			}
		}
		committer := pyjson.NewObject()
		if value := stampGet(commit, "committer"); pyTruthy(value) {
			if committer, ok = value.(*pyjson.Object); !ok {
				return nil, false
			}
		}
		when := stampGet(committer, "date")
		if !pyTruthy(when) {
			when = ""
		}
		var at int64
		if text, ok := when.(string); ok {
			at, _ = isoMillis(text)
		}
		message, ok := commit.Get("message")
		if !ok {
			message = ""
		}
		summary, _, _ := strings.Cut(pyrepr.Str(message), "\n")
		author := pyjson.NewObject()
		if value := stampGet(commit, "author"); pyTruthy(value) {
			if author, ok = value.(*pyjson.Object); !ok {
				return nil, false
			}
		}
		name, ok := author.Get("name")
		if !ok {
			name = ""
		}
		rows = append(rows, pyjson.ObjectOf("sha", sha, "summary", summary, "author", pyrepr.Str(name), "at", at))
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, true
}

func pyTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case *pyjson.Object:
		return v != nil && v.Len() > 0
	case []any:
		return len(v) > 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	}
	return true
}

// Read only facts about the checkout under test, gathered once per check
type sourceCheckout struct {
	root, git, embedded string
	head, currentBranch string
	origin, repository  string
	dirty               bool
}

func readJSONFile(path string) any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text, err := pystr.DecodeUTF8Sig(data)
	if err != nil {
		return nil
	}
	value, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	return value
}

// Git as given unless it is a bare git PATH lacks, then the git the updater runs
func installedGit(ctx context.Context, git string) string {
	if git != "git" {
		return git
	}
	if _, err := exec.LookPath(git); err == nil {
		return git
	}
	return checkGitExecutable(ctx)
}

func readSourceCheckout(ctx context.Context, root, git, embedded string) sourceCheckout {
	co := sourceCheckout{root: root, git: git, embedded: embedded, head: embedded}
	if embedded != "" {
		co.repository = OfficialRepository
		return co
	}
	co.head, _ = checkGitStdout(ctx, []string{"rev-parse", "HEAD"}, root, git)
	co.currentBranch, _ = checkGitStdout(ctx, []string{"rev-parse", "--abbrev-ref", "HEAD"}, root, git)
	co.origin, _ = checkGitStdout(ctx, []string{"remote", "get-url", "origin"}, root, git)
	if match := githubOriginRE.FindStringSubmatch(co.origin); match != nil {
		co.repository = match[1]
	}
	status, _ := checkGitStdout(ctx, []string{"status", "--porcelain"}, root, git)
	co.dirty = status != ""
	return co
}

// The checkout's branch, or fallback when detached or unreadable
func checkedOutBranch(currentBranch, fallback string) string {
	if currentBranch != "" && currentBranch != "HEAD" {
		return currentBranch
	}
	return fallback
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// Still fresh supported status cached for exactly this identity, failures expiring after an hour
func cachedStatus(cacheFile string, identity *pyjson.Object, now float64) *pyjson.Object {
	cached, ok := readJSONFile(cacheFile).(*pyjson.Object)
	if !ok || !pyjson.Equal(stampGet(cached, "identity"), identity) {
		return nil
	}
	status, ok := stampGet(cached, "status").(*pyjson.Object)
	if !ok || stampGet(status, "supported") != true {
		return nil
	}
	ttl := float64(updateCheckCacheSeconds)
	if pyTruthy(stampGet(status, "error")) {
		ttl = updateCheckFailureCacheSeconds
	}
	var ts float64
	switch value := stampGet(cached, "ts").(type) {
	case float64:
		ts = value
	case int64:
		ts = float64(value)
	default:
		if n, ok := pyIntValue(value); ok {
			ts = float64(n)
		} else {
			return nil
		}
	}
	if age := now - ts; age >= 0 && age < ttl {
		return status
	}
	return nil
}

func writeCheckCache(ctx context.Context, cacheFile string, identity *pyjson.Object, now float64, result *pyjson.Object) {
	err := os.MkdirAll(filepath.Dir(cacheFile), 0o755)
	if err == nil {
		err = config.AtomicJSONWrite(ctx, cacheFile, pyjson.ObjectOf("identity", identity, "ts", now, "status", result))
	}
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Could not cache source check: %v", err))
	}
}

// Remote a branch tip is read from, the public official URL for embedded and official ssh checkouts
func branchRemote(co sourceCheckout, selectedBranch string) string {
	lowerOrigin := strings.ToLower(co.origin)
	officialSSH := co.repository != "" && strings.EqualFold(co.repository, OfficialRepository) &&
		(strings.HasPrefix(lowerOrigin, "git@") || strings.HasPrefix(lowerOrigin, "ssh://"))
	if co.embedded != "" || (officialSSH && selectedBranch != "main") {
		return "https://github.com/" + OfficialRepository + ".git"
	}
	return "origin"
}

// Why a branch the remote does not advertise keeps its pin, empty when healing loses nothing
func unhealableReason(ctx context.Context, co sourceCheckout, branch string) string {
	if co.embedded != "" {
		return ""
	}
	local := "refs/heads/" + branch
	if !checkGitOK(ctx, []string{"rev-parse", "--verify", "--quiet", local}, co.root, co.git) {
		return ""
	}
	if !checkGitOK(ctx, []string{"rev-parse", "--verify", "--quiet", "refs/remotes/origin/" + branch}, co.root, co.git) &&
		!checkGitOK(ctx, []string{"config", "--get", "branch." + branch + ".merge"}, co.root, co.git) {
		return "never-pushed"
	}
	for _, base := range []string{"refs/remotes/origin/main", "refs/heads/main"} {
		cherry := checkGitRun(ctx, []string{"cherry", base, local}, co.root, 10*time.Second, co.git)
		if cherry != nil && cherry.ReturnCode == 0 {
			for _, line := range textutil.Lines(cherry.Stdout) {
				if strings.HasPrefix(line, "+") {
					return "unmerged"
				}
			}
			return ""
		}
	}
	return "unmerged"
}

// Commits behind target and their rows, local ancestry first, then the GitHub comparison
func behindCount(ctx context.Context, co sourceCheckout, target string) (int64, []any) {
	if co.head == target || (co.embedded == "" && checkGitOK(ctx, []string{"merge-base", "--is-ancestor", target, co.head}, co.root, co.git)) {
		return 0, []any{}
	}
	if co.repository != "" {
		payload := githubCompare(ctx, co.head, target, co.repository)
		if payload == nil {
			return UpdateAvailableNoCount, []any{}
		}
		if ahead, ok := pyIntValue(stampGet(payload, "ahead_by")); ok && ahead >= 0 {
			if ahead == 0 {
				return 0, []any{}
			}
			rows, ok := compareCommits(payload)
			if !ok {
				rows = []any{}
			}
			return ahead, rows
		}
	}
	return UpdateAvailableNoCount, []any{}
}

// Compares the checkout with the selected branch's remote tip, falling back to main when it was deleted
func checkBranch(ctx context.Context, result *pyjson.Object, co sourceCheckout, selectedBranch string) error {
	result.Set("branch", selectedBranch)
	remote := branchRemote(co, selectedBranch)
	target, missing, failure := branchTip(ctx, co.repository, selectedBranch, co.root, co.git, remote)
	reason := ""
	if missing && selectedBranch != "main" {
		reason = unhealableReason(ctx, co, selectedBranch)
	}
	if reason != "" {
		detail := "is gone from the remote but has commits that are not in main"
		if reason == "never-pushed" {
			detail = "has never been pushed"
		}
		result.Set("error", "branch-local-only")
		result.Set("localOnly", true)
		result.Set("message", fmt.Sprintf("Branch '%s' %s; keeping it instead of switching to main.", selectedBranch, detail))
		return nil
	}
	if missing && selectedBranch != "main" {
		result.Set("branch", "main")
		mainRemote := "origin"
		if co.embedded != "" {
			mainRemote = remote
		}
		target, _, failure = branchTip(ctx, co.repository, "main", co.root, co.git, mainRemote)
	}
	if target == "" {
		message := "Could not resolve the remote branch tip."
		if failure != "" {
			message = "Could not resolve the remote branch tip: " + failure
		}
		result.Set("error", "fetch-failed")
		result.Set("message", message)
		return nil
	}
	behind, commits := behindCount(ctx, co, target)
	result.Set("commits", commits)
	result.Set("targetSha", target)
	result.Set("behind", behind)
	result.Set("updateAvailable", behind != 0)
	return nil
}

// Inputs of one passive source update check, empty strings and nil pointers take the running install's defaults
type SourceCheckOptions struct {
	InstallRoot string
	Home        string
	Branch      *string
	Channel     *string
	CachePath   string
	Force       bool
	Passive     bool
	Git         string
}
