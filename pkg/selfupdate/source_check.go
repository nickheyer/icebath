package selfupdate

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

const (
	UpdateAvailableNoCount         = -1
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
	return pystr.Strip(result.Stdout), true
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
	value, ok := pystr.IntText(pystr.Strip(result.Stdout))
	if !ok || !value.IsInt64() {
		return 0, false
	}
	return value.Int64(), true
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
	detail := pystr.Splitlines(pystr.Strip(result.Stderr), false)
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

// Fields explaining why this install cannot self update from git, nil when it can
func unsupportedReason(stamp *pyjson.Object, root, h string, explicitRoot bool, embedded string) *pyjson.Object {
	if stampGet(stamp, "source") == "commit-build" {
		return pyjson.ObjectOf("reason", "commit-build", "message", CommitBuildUpdateMessage)
	}
	payload := stampGet(stamp, "payload")
	bundled := payload == "bundled" || payload == "light" || payload == "runtime"
	if !bundled && !explicitRoot {
		if method, err := config.DetectInstallMethod(h, root); err == nil && (method == "docker" || method == "apt") {
			bundled = true
		}
	}
	if bundled {
		return pyjson.ObjectOf("reason", "not-a-git-checkout")
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); embedded == "" && err != nil {
		return pyjson.ObjectOf("reason", "not-a-git-checkout", "message", "This install has no git checkout to update.")
	}
	if mechanism := stampGet(stamp, "updateMechanism"); mechanism != nil && mechanism != "self" && embedded == "" {
		return pyjson.ObjectOf("reason", "update-root-steward-owned-git-tree",
			"message", "This installation is managed by its install method; use its updater.", "advice", "git pull")
	}
	return nil
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

// Branch the Desktop setting pins, empty when unset or blank
func configuredBranch(desktopConfig any) string {
	obj, ok := desktopConfig.(*pyjson.Object)
	if !ok {
		return ""
	}
	value, _ := stampGet(obj, "branch").(string)
	return pystr.Strip(value)
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

func writeCheckCache(cacheFile string, identity *pyjson.Object, now float64, result *pyjson.Object) {
	err := os.MkdirAll(filepath.Dir(cacheFile), 0o755)
	if err == nil {
		err = config.AtomicJSONWrite(cacheFile, pyjson.ObjectOf("identity", identity, "ts", now, "status", result))
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not cache source check: %v", err))
	}
}

// Schema verdict of a tree read from GitHub within one deadline, at most four schema sources in flight
func readRuntimeVerdict(ctx context.Context, repository, sha string) *bool {
	ctx, cancel := context.WithTimeout(ctx, runtimeLookupTimeout)
	defer cancel()
	replies := make(chan *string, 8)
	launch := func(target string, api bool) {
		go func() {
			var text string
			var err error
			if api {
				text, err = sourceRequest(ctx, target, githubJSONAccept)
			} else {
				text, err = sourceRequestWith(ctx, target, "*/*", "")
			}
			var reply *string
			if err == nil {
				reply = &text
			}
			select {
			case replies <- reply:
			case <-ctx.Done():
			}
		}()
	}
	receive := func() (*string, bool) {
		if ctx.Err() != nil {
			return nil, false
		}
		select {
		case reply := <-replies:
			return reply, true
		case <-ctx.Done():
			return nil, false
		}
	}
	dir := filepath.ToSlash(filepath.Dir(StateModules))
	launch(fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", sourceGitHubAPI, repository, dir, pyurl.Quote(sha, "")), true)
	reply, ok := receive()
	if !ok || reply == nil {
		return nil
	}
	listing, err := pyjson.Loads(*reply)
	entries, isList := listing.([]any)
	if err != nil || !isList {
		return nil
	}
	var paths []string
	for _, item := range entries {
		entry, ok := item.(*pyjson.Object)
		if !ok || stampGet(entry, "type") != "file" {
			continue
		}
		if path, ok := stampGet(entry, "path").(string); ok && fnmatch.FnmatchCase(path, StateModules) {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	next := 0
	pending := 0
	for ; next < len(paths) && next < 4; next++ {
		launch(fmt.Sprintf("%s/%s/%s/%s", sourceRawBase, repository, sha, pyurl.Quote(paths[next], "/")), false)
		pending++
	}
	for pending > 0 {
		text, ok := receive()
		if !ok || text == nil {
			return nil
		}
		if definesRuntime(*text) {
			verdict := false
			return &verdict
		}
		pending--
		if next < len(paths) {
			launch(fmt.Sprintf("%s/%s/%s/%s", sourceRawBase, repository, sha, pyurl.Quote(paths[next], "/")), false)
			next++
			pending++
		}
	}
	verdict := true
	return &verdict
}

// Whether an unfetched release predates the runtime, definite verdicts cached per ref and unknown ones retried after an hour
func githubPredatesRuntime(ctx context.Context, repository, sha, cacheFile string) *bool {
	known, ok := readJSONFile(cacheFile).(*pyjson.Object)
	if !ok {
		known = pyjson.NewObject()
	}
	cached, _ := known.Get(sha)
	if verdict, ok := cached.(bool); ok {
		return &verdict
	}
	now := sourceCheckClock()
	if entry, ok := cached.(*pyjson.Object); ok {
		switch retryAt := stampGet(entry, "retryAt").(type) {
		case float64:
			if now < retryAt {
				return nil
			}
		case int64:
			if now < float64(retryAt) {
				return nil
			}
		}
	}
	verdict := readRuntimeVerdict(ctx, repository, sha)
	updated := known.Copy()
	if verdict != nil {
		updated.Set(sha, *verdict)
	} else {
		updated.Set(sha, pyjson.ObjectOf("retryAt", sourceCheckClock()+updateCheckFailureCacheSeconds))
	}
	err := os.MkdirAll(filepath.Dir(cacheFile), 0o755)
	if err == nil {
		err = config.AtomicJSONWrite(cacheFile, updated)
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not cache the release schema verdict: %v", err))
	}
	return verdict
}

// The updater's downgrade refusal asked read only, an embedded revision has no git to ask
func updateRefuses(ctx context.Context, co sourceCheckout, target, h string) bool {
	if co.embedded != "" {
		return false
	}
	git := func(args ...string) int {
		result := checkGitRun(ctx, args, co.root, 20*time.Second, co.git)
		if result == nil {
			return -1
		}
		return result.ReturnCode
	}
	repository := firstNonEmpty(co.repository, OfficialRepository)
	remote := func(sha string) *bool {
		return githubPredatesRuntime(ctx, repository, sha, filepath.Join(h, "source-checks", "release-runtime.json"))
	}
	return wouldRefuse(ctx, co.root, target, h, git, remote)
}

// Holds an offered release the updater refuses because it predates the runtime these stores use
func holdRefusedRelease(ctx context.Context, result *pyjson.Object, co sourceCheckout, h string) {
	target, _ := stampGet(result, "targetSha").(string)
	if pyTruthy(stampGet(result, "updateAvailable")) && target != "" && updateRefuses(ctx, co, target, h) {
		result.Set("updateAvailable", false)
		result.Set("behind", int64(0))
		result.Set("held", "downgrade")
		result.Set("rollback", false)
	}
}

// Resolves a release channel's target into result, nil on error
func resolveCheckChannel(ctx context.Context, result *pyjson.Object, channel string, co sourceCheckout, forwardOnly bool) *SourceTarget {
	var gitCmd []string
	if co.embedded == "" {
		gitCmd = []string{co.git}
	}
	target, err := ResolveSourceTarget(ctx, channel, gitCmd, co.root, firstNonEmpty(co.repository, OfficialRepository), forwardOnly)
	if err != nil {
		result.Set("error", "release-unavailable")
		result.Set("message", fmt.Sprintf("Could not resolve the %s source channel: %v", channel, err))
		return nil
	}
	if target.Commit != "" {
		result.Delete("branch")
		behind := int64(UpdateAvailableNoCount)
		if co.head == target.Commit {
			behind = 0
		}
		result.Set("channel", channel)
		result.Set("targetSha", target.Commit)
		result.Set("updateAvailable", co.head != target.Commit)
		result.Set("behind", behind)
		result.Set("sourceVersion", nullable(target.Version))
		result.Set("buildId", nullable(target.BuildID))
		result.Set("aheadOfRelease", target.Ahead)
		result.Set("rollback", co.head != "" && co.head != target.Commit && co.embedded == "" &&
			checkGitOK(ctx, []string{"merge-base", "--is-ancestor", target.Commit, co.head}, co.root, co.git))
		if target.Retired() {
			result.Set("retirement", pyjson.ObjectOf("destination", target.Channel, "sourceOnly", true))
		}
	}
	return &target
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

// Points the Desktop branch setting back at main unless it changed while the probe ran
func healDeletedBranch(branchConfigPath string, desktopConfig any) error {
	if !pyjson.Equal(readJSONFile(branchConfigPath), desktopConfig) {
		return nil
	}
	obj, _ := desktopConfig.(*pyjson.Object)
	healed := pyjson.NewObject()
	if obj != nil {
		healed = obj.Copy()
	}
	healed.Set("branch", "main")
	return config.AtomicJSONWrite(branchConfigPath, healed)
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
			for _, line := range pystr.Splitlines(cherry.Stdout, false) {
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

type branchHeal struct {
	path          string
	desktopConfig any
}

// Compares the checkout with the selected branch's remote tip, falling back to main when it was deleted
func checkBranch(ctx context.Context, result *pyjson.Object, co sourceCheckout, selectedBranch string, heal *branchHeal) error {
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
		if heal != nil {
			if err := healDeletedBranch(heal.path, heal.desktopConfig); err != nil {
				return err
			}
		}
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
	InstallRoot      string
	Home             string
	Branch           *string
	Channel          *string
	CachePath        string
	BranchConfigPath string
	Force            bool
	Passive          bool
	Git              string
}

// Presentation ready source update status of one install and profile, never fetching or writing git state
func CheckForUpdates(ctx context.Context, opts SourceCheckOptions) (*pyjson.Object, error) {
	git := opts.Git
	if git == "" {
		git = "git"
	}
	embedded := ""
	rootPath := opts.InstallRoot
	if rootPath == "" {
		embedded = os.Getenv("ICEBATH_REVISION")
		rootPath = checkProjectRoot()
	}
	root := resolvedPath(rootPath)
	homePath := opts.Home
	if homePath == "" {
		homePath = home.Dir(ctx)
	}
	h := resolvedPath(homePath)
	result := pyjson.ObjectOf("supported", false, "icebathRoot", root, "behind", nil, "commits", []any{})
	if unsupported := unsupportedReason(pm.ReadInstallStamp(root), root, h, opts.InstallRoot != "", embedded); unsupported != nil {
		merged := result.Copy()
		merged.Update(unsupported)
		return merged, nil
	}
	cfg, err := config.RequireReadableConfigBeforeWrite(ctx, filepath.Join(h, "config.yaml"))
	if err != nil {
		return nil, err
	}
	if opts.Passive {
		if updates, ok := cfg["updates"].(map[string]any); ok && updates["check"] == false {
			disabled := result.Copy()
			disabled.Set("reason", "disabled")
			return disabled, nil
		}
	}
	var channel string
	if opts.Channel == nil {
		if channel, err = ResolveUpdateChannel(cfg, root); err != nil {
			return nil, err
		}
	} else if channel, err = ValidateChannelName(*opts.Channel); err != nil {
		return nil, err
	}
	forwardOnly, err := RidesDefaultChannel(ChannelRecord(cfg, root), channel, root)
	if err != nil {
		return nil, err
	}
	co := readSourceCheckout(ctx, root, installedGit(ctx, git), embedded)
	var desktopConfig any
	if opts.BranchConfigPath != "" {
		desktopConfig = readJSONFile(opts.BranchConfigPath)
	}
	configured := configuredBranch(desktopConfig)
	selected := checkedOutBranch(co.currentBranch, "main")
	if configured != "" {
		selected = configured
	}
	if opts.Branch != nil && *opts.Branch != "" {
		selected = *opts.Branch
	}
	result.Set("supported", true)
	result.Set("currentSha", nullable(co.head))
	result.Set("currentBranch", nullable(co.currentBranch))
	result.Set("dirty", co.dirty)
	if channel != ChannelMain {
		result.Set("channel", channel)
	} else {
		result.Set("branch", selected)
	}
	identity := pyjson.ObjectOf("root", root, "home", h, "head", nullable(co.head), "origin", co.origin, "branch", selected,
		"channel", channel, "embedded", nullable(embedded), "branchOverride", opts.Branch != nil, "channelProtocol", int64(3),
		"forwardOnly", forwardOnly)
	cacheFile := opts.CachePath
	if cacheFile == "" {
		cacheFile = filepath.Join(h, "source-checks", InstallID(root)+".json")
	}
	now := sourceCheckClock()
	if !opts.Force {
		if cached := cachedStatus(cacheFile, identity, now); cached != nil {
			cached = cached.Copy()
			cached.Set("dirty", co.dirty)
			cached.Set("currentBranch", nullable(co.currentBranch))
			if pyTruthy(stampGet(cached, "channel")) {
				holdRefusedRelease(ctx, cached, co, h)
			}
			return cached, nil
		}
	}
	result.Set("fetchedAt", int64(now*1000))
	var target *SourceTarget
	if !isFullSHA(co.head) {
		result.Set("error", "head-unavailable")
		result.Set("message", "Could not read the installed revision.")
	} else if opts.Branch == nil {
		target = resolveCheckChannel(ctx, result, channel, co, forwardOnly)
		if target != nil && target.Commit == "" {
			selected = firstNonEmpty(configured, checkedOutBranch(co.currentBranch, target.Branch))
		}
	}
	_, failed := result.Get("error")
	if !failed && (target == nil || target.Branch != "") {
		var heal *branchHeal
		if opts.BranchConfigPath != "" && opts.Branch == nil && configured == selected {
			heal = &branchHeal{path: opts.BranchConfigPath, desktopConfig: desktopConfig}
		}
		if err := checkBranch(ctx, result, co, selected, heal); err != nil {
			return nil, err
		}
	}
	writeCheckCache(cacheFile, identity, now, result)
	if pyTruthy(stampGet(result, "channel")) {
		holdRefusedRelease(ctx, result, co, h)
	}
	return result, nil
}

const sourceCheckUsage = "usage: icebath source-check [-h] --install-root INSTALL_ROOT --home HOME [--git GIT]\n" +
	"                             [--branch BRANCH] [--channel CHANNEL]\n" +
	"                             [--cache-path CACHE_PATH]\n" +
	"                             [--branch-config-path BRANCH_CONFIG_PATH] [--force]\n" +
	"                             [--set-channel SET_CHANNEL]\n"

type optionalFlag struct{ value *string }

func (f *optionalFlag) String() string {
	if f.value == nil {
		return ""
	}
	return *f.value
}

func (f *optionalFlag) Set(value string) error {
	f.value = &value
	return nil
}

// Desktop's passive check entry, reporting the status as one JSON line and persisting a selected channel first
func SourceCheckMain(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fail := func(message string) int {
		fmt.Fprint(stderr, sourceCheckUsage)
		fmt.Fprintf(stderr, "icebath source-check: error: %s\n", message)
		return 2
	}
	set := flag.NewFlagSet("icebath source-check", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	var installRoot, homeDir, branch, channel, cachePath, branchConfig, setChannel optionalFlag
	git := set.String("git", "git", "")
	force := set.Bool("force", false, "")
	set.Var(&installRoot, "install-root", "")
	set.Var(&homeDir, "home", "")
	set.Var(&branch, "branch", "")
	set.Var(&channel, "channel", "")
	set.Var(&cachePath, "cache-path", "")
	set.Var(&branchConfig, "branch-config-path", "")
	set.Var(&setChannel, "set-channel", "")
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, sourceCheckUsage)
			return 0
		}
		return fail(err.Error())
	}
	if set.NArg() > 0 {
		return fail("unrecognized arguments: " + strings.Join(set.Args(), " "))
	}
	var missing []string
	if installRoot.value == nil {
		missing = append(missing, "--install-root")
	}
	if homeDir.value == nil {
		missing = append(missing, "--home")
	}
	if len(missing) > 0 {
		return fail("the following arguments are required: " + strings.Join(missing, ", "))
	}
	for _, named := range []struct {
		flag  string
		value *optionalFlag
	}{{"--channel", &channel}, {"--set-channel", &setChannel}} {
		if named.value.value != nil {
			if _, err := ValidateChannelName(*named.value.value); err != nil {
				return fail(fmt.Sprintf("argument %s: invalid validate_name value: %s", named.flag, pyrepr.Repr(*named.value.value)))
			}
		}
	}
	if setChannel.value != nil {
		scoped := home.WithOverride(ctx, *homeDir.value)
		if _, err := SetInstallChannel(scoped, *setChannel.value, *installRoot.value); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		*force = true
	}
	result, err := CheckForUpdates(ctx, SourceCheckOptions{InstallRoot: *installRoot.value, Home: *homeDir.value,
		Branch: branch.value, Channel: channel.value, CachePath: branchOr(cachePath.value),
		BranchConfigPath: branchOr(branchConfig.value), Force: *force, Git: *git})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	result.Set("channelSelectable", true)
	text, err := pyjson.Dumps(result)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, text)
	return 0
}

func branchOr(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
