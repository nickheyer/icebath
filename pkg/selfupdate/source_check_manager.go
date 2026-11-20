//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Fields explaining why this install cannot self update from git, nil when it can
func unsupportedReason(ctx context.Context, stamp *pyjson.Object, root, h string, explicitRoot bool, embedded string) *pyjson.Object {
	if stampGet(stamp, "source") == "commit-build" {
		return pyjson.ObjectOf("reason", "commit-build", "message", CommitBuildUpdateMessage)
	}
	if !explicitRoot {
		if method, err := config.DetectInstallMethod(ctx, h, root); err == nil && (method == "docker" || config.IsPackageManagerInstallMethod(method)) {
			return pyjson.ObjectOf("reason", "not-a-git-checkout")
		}
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
		err = config.AtomicJSONWrite(ctx, cacheFile, updated)
	}
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("Could not cache the release schema verdict: %v", err))
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

// A release install's downgrade refusal asked read only of the repository its releases come from
func releaseRefuses(ctx context.Context, root, tag, h string) bool {
	repo, err := install.ParseRepo(releaseRepository(root))
	return err == nil && downgradeWouldRefuse(ctx, repo.Owner+"/"+repo.Name, tag, h)
}

// Holds an offered release the updater refuses because it predates the runtime these stores use, a release install's by its tag
func holdRefusedRelease(ctx context.Context, result *pyjson.Object, co sourceCheckout, h string) {
	if !pyTruthy(stampGet(result, "updateAvailable")) {
		return
	}
	target, _ := stampGet(result, "targetSha").(string)
	tag, _ := stampGet(result, "releaseTag").(string)
	if (tag != "" && releaseRefuses(ctx, co.root, tag, h)) || (tag == "" && target != "" && updateRefuses(ctx, co, target, h)) {
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

// Passive source update status of an install for the version output
func PassiveCheckForUpdates(ctx context.Context, installRoot, home string) (*pyjson.Object, error) {
	return CheckForUpdates(ctx, SourceCheckOptions{InstallRoot: installRoot, Home: home, Passive: true})
}

// Binds the banner's passive update check
func init() {
	BannerUpdateCheck = func(ctx context.Context) (*pyjson.Object, error) {
		return CheckForUpdates(ctx, SourceCheckOptions{Passive: true})
	}
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
	if embedded == "" && opts.InstallRoot == "" {
		embedded = releaseRevision(ctx, root)
	}
	result := pyjson.ObjectOf("supported", false, "icebathRoot", root, "behind", nil, "commits", []any{})
	if unsupported := unsupportedReason(ctx, pm.ReadInstallStamp(root), root, h, opts.InstallRoot != "", embedded); unsupported != nil {
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
	selected := checkedOutBranch(co.currentBranch, "main")
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
	if release, err := releaseBinaryInstall(ctx, root); err != nil {
		return nil, err
	} else if release && opts.InstallRoot == "" {
		releaseCheckResult(ctx, result, root, channel, opts.Branch)
		writeCheckCache(ctx, cacheFile, identity, now, result)
		if pyTruthy(stampGet(result, "channel")) {
			holdRefusedRelease(ctx, result, co, h)
		}
		return result, nil
	}
	var target *SourceTarget
	if !isFullSHA(co.head) {
		result.Set("error", "head-unavailable")
		result.Set("message", "Could not read the installed revision.")
	} else if opts.Branch == nil {
		target = resolveCheckChannel(ctx, result, channel, co, forwardOnly)
		if target != nil && target.Commit == "" {
			selected = checkedOutBranch(co.currentBranch, target.Branch)
		}
	}
	_, failed := result.Get("error")
	if !failed && (target == nil || target.Branch != "") {
		if err := checkBranch(ctx, result, co, selected); err != nil {
			return nil, err
		}
	}
	writeCheckCache(ctx, cacheFile, identity, now, result)
	if pyTruthy(stampGet(result, "channel")) {
		holdRefusedRelease(ctx, result, co, h)
	}
	return result, nil
}

// Fills a release install's status from the release plan its update applies
func releaseCheckResult(ctx context.Context, result *pyjson.Object, root, channel string, branch *string) {
	explicit := branch != nil && *branch != ""
	selected := ""
	if explicit {
		selected = *branch
	}
	u, err := planReleaseUpdate(ctx, root, &channel, selected, explicit)
	if err != nil {
		result.Set("error", "release-unavailable")
		result.Set("message", launchErrorText(err))
		return
	}
	result.Set("targetSha", nullable(u.plan.Target.Commit))
	result.Set("releaseTag", u.plan.Target.Tag)
	switch {
	case u.plan.Order == install.ReleaseNewer && u.plan.Ahead >= 0:
		result.Set("behind", int64(u.plan.Ahead))
	case u.plan.Order == install.ReleaseNewer:
		result.Set("behind", int64(UpdateAvailableNoCount))
	default:
		result.Set("behind", int64(0))
	}
	result.Set("updateAvailable", u.moves())
	result.Set("rollback", u.rollback())
	if u.rollback() {
		result.Set("sourceVersion", strings.TrimPrefix(u.plan.Target.Tag, "v"))
	}
}
