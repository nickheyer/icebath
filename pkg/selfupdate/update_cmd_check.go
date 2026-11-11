package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Calls of the target selection the hermes tests replace, the resolver, the config read and the run's channel
var (
	checkResolveSourceTarget = ResolveSourceTarget
	checkReadableConfig      = config.RequireReadableConfigBeforeWrite
	checkRunChannel          = updateRunChannel
)

// Runs a check step's git in the updater's custody with captured text and a hidden console
func updateCmdCheckGit(ctx context.Context, gitCmd []string, root string, args []string, kwargs ChildOptions) (*CompletedProcess, error) {
	if kwargs.Attr == nil {
		kwargs.Attr = tools.WindowsHideAttr()
	}
	kwargs.Dir, kwargs.Capture, kwargs.Text = root, true, true
	return RunGit(ctx, gitCmd, args, kwargs)
}

// Removes abandoned git locks and aborted fetch pack temps and settles a partial clone before fetching
func clearGitDebris(ctx context.Context, w io.Writer, root string) {
	for _, lockPath := range tools.ClearStaleGitLocks(root, nil) {
		fmt.Fprintf(w, "  (removed stale git lock: %s)\n", lockPath)
	}
	if swept := tools.ClearStaleTmpPacks(root, nil); len(swept) > 0 {
		fmt.Fprintf(w, "  (removed %d aborted-fetch pack temp file(s))\n", len(swept))
	}
	tools.SettlePartialCloneMaintenance(ctx, root)
}

// Spends the bounded slice on a partial clone's on demand packs and prints what it did
func reportPackTidy(ctx context.Context, w io.Writer, root string) {
	tidy := TidyPartialClonePacks(ctx, root, TidyBudgetSeconds)
	if tidy.Erased != 0 || tidy.Merged != 0 {
		fmt.Fprintf(w, "  (git cleanup: erased %d duplicate pack(s), %.0f MB freed; merged %d; %d left)\n",
			tidy.Erased, float64(tidy.FreedBytes)/1e6, tidy.Merged, tidy.PacksLeft)
	}
	if tidy.OutOfTime {
		fmt.Fprintf(w, "  (git cleanup stopped at its %ss limit; the next update continues it)\n", pyrepr.Repr(TidyBudgetSeconds))
	}
}

// Whether the update would refuse target, so the check never offers it
func refuses(ctx context.Context, gitCmd []string, root, target, repository string) bool {
	h := home.Dir(ctx)
	env := tools.NoninteractiveGitEnv(nil)
	env["GIT_NO_LAZY_FETCH"] = "1"
	environ := make([]string, 0, len(env))
	for key, value := range env {
		environ = append(environ, key+"="+value)
	}
	git := func(args ...string) int {
		result, err := updateCmdCheckGit(ctx, gitCmd, root, args, ChildOptions{Env: environ, Timeout: 20 * time.Second})
		if err != nil {
			if isOSOrSubprocessError(err) {
				return -1
			}
			panic(err)
		}
		return result.ReturnCode
	}
	remote := func(sha string) *bool {
		return githubPredatesRuntime(ctx, repository, sha, filepath.Join(h, "source-checks", "release-runtime.json"))
	}
	return wouldRefuse(ctx, root, target, h, git, remote)
}

// Whether a channel resolution failure is one the update reports instead of crashing on
func isSourceResolveError(err error) bool {
	if err == nil || isUpdateExit(err) {
		return false
	}
	var source *sourceCalledProcessError
	var http *pm.HTTPError
	var urlErr *url.Error
	var netErr net.Error
	return isOSOrSubprocessError(err) || IsValueError(err) || pm.ErrorClass(err) == "ValueError" ||
		errors.As(err, &source) || errors.As(err, &http) || errors.As(err, &urlErr) || errors.As(err, &netErr)
}

// Prints a release pinned channel's verdict, or returns the branch to compare against with true
func channelCompareBranch(ctx context.Context, w io.Writer, selectedChannel string, gitCmd []string, root string) (string, bool, error) {
	fmt.Fprintf(w, "→ Update channel: %s\n", selectedChannel)
	cfg, err := checkReadableConfig(ctx, home.ConfigPath(home.Dir(ctx)))
	if err != nil {
		return "", false, err
	}
	forwardOnly, err := RidesDefaultChannel(ChannelRecord(cfg, root), selectedChannel, root)
	if err != nil {
		return "", false, err
	}
	target, err := checkResolveSourceTarget(ctx, selectedChannel, gitCmd, root, "", forwardOnly)
	if err != nil {
		if !isSourceResolveError(err) {
			return "", false, err
		}
		fmt.Fprintf(w, "✗ Could not resolve the %s source channel: %s\n", selectedChannel, launchErrorText(err))
		return "", false, &pm.SystemExit{Code: 1}
	}
	if target.Commit == "" {
		return target.Branch, true, nil
	}
	if target.Retired() {
		fmt.Fprintf(w, "→ %s retired; source destination: %s\n", selectedChannel, target.Channel)
	}
	switch {
	case target.Ahead:
		fmt.Fprintf(w, "✓ No newer release: %s.\n", target.Label())
	case captureHeadSHA(ctx, gitCmd, root) == target.Commit:
		fmt.Fprintf(w, "✓ Up to date with the latest release (%s).\n", target.Label())
	case refuses(ctx, gitCmd, root, target.Commit, target.Repository):
		fmt.Fprintf(w, "→ Holding at this build: %s predates the gateway runtime your sessions use.\n", target.Label())
		fmt.Fprintln(w, "  `icebath update` stays on this build; the next release that includes the runtime"+
			" installs normally (`icebath update --set-channel main` follows every commit).")
	default:
		ancestor, err := updateCmdCheckGit(ctx, gitCmd, root, []string{"merge-base", "--is-ancestor", target.Commit, "HEAD"}, ChildOptions{})
		if err != nil {
			return "", false, err
		}
		if ancestor.ReturnCode == 0 {
			fmt.Fprintf(w, "→ This checkout is newer than the selected release: %s.\n", target.Label())
			fmt.Fprintln(w, "  `icebath update` moves it back to that release"+
				" (`icebath update --set-channel main` follows every commit).")
		} else {
			fmt.Fprintf(w, "→ Selected release available: %s\n", target.Label())
			fmt.Fprintln(w, "  Run `icebath update` to install it.")
		}
	}
	return "", false, nil
}

// Whether git reports the checkout at root as shallow
func isShallowRepository(ctx context.Context, gitCmd []string, root string) (bool, error) {
	result, err := updateCmdCheckGit(ctx, gitCmd, root, []string{"rev-parse", "--is-shallow-repository"}, ChildOptions{})
	if err != nil {
		return false, err
	}
	return pystr.Strip(result.Stdout) == "true", nil
}

// Refspec that always writes the remote tracking ref of branch, forced for a depth 1 clone
func trackingRefspec(remote, branch string) string {
	return fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch)
}

// Announces and runs the promptless scoped fetch of one branch from one remote
func updateCmdCheckFetch(ctx context.Context, w io.Writer, gitCmd []string, root string, depthArgs []string, remote, branch string) (*CompletedProcess, error) {
	fmt.Fprintf(w, "→ Fetching from %s...\n", remote)
	args := append(append([]string{"fetch"}, depthArgs...), remote, trackingRefspec(remote, branch))
	return updateCmdCheckGit(ctx, gitCmd, root, args, noPromptGitKwargs())
}

// Plain runner result of a custody child for the partial clone fetch recovery
func plainCompleted(done *CompletedProcess) *pysubprocess.Completed {
	if done == nil {
		return nil
	}
	return &pysubprocess.Completed{Args: done.Args, Returncode: done.ReturnCode, Stdout: []byte(done.Stdout), Stderr: []byte(done.Stderr)}
}

// Custody result of a plain runner outcome
func custodyCompleted(done *pysubprocess.Completed) *CompletedProcess {
	if done == nil {
		return nil
	}
	return &CompletedProcess{Args: done.Args, ReturnCode: done.Returncode, Stdout: string(done.Stdout), Stderr: string(done.Stderr)}
}

// Fetches only branch, main preferring an upstream remote, and returns the result with the ref to compare
func fetchCompareBranch(ctx context.Context, w io.Writer, gitCmd []string, root, branch string, depthArgs []string) (*CompletedProcess, string, error) {
	if branch == "main" {
		probe, err := updateCmdCheckGit(ctx, gitCmd, root, []string{"remote", "get-url", "upstream"}, ChildOptions{})
		if err != nil {
			return nil, "", err
		}
		if probe.ReturnCode == 0 {
			fetchResult, err := updateCmdCheckFetch(ctx, w, gitCmd, root, depthArgs, "upstream", branch)
			if err != nil {
				return nil, "", err
			}
			if fetchResult.ReturnCode == 0 {
				return fetchResult, "upstream/" + branch, nil
			}
		}
	}
	fmt.Fprintln(w, "→ Fetching from origin...")
	runner := func(gc, a []string) (*pysubprocess.Completed, error) {
		done, err := updateCmdCheckGit(ctx, gc, root, a, noPromptGitKwargs())
		return plainCompleted(done), err
	}
	args := append(append([]string{"fetch"}, depthArgs...), "origin", trackingRefspec("origin", branch))
	fetched, err := tools.FetchWithPartialCloneRecovery(runner, gitCmd, args, root)
	if err != nil {
		return nil, "", err
	}
	return custodyCompleted(fetched), "origin/" + branch, nil
}

// Restores broken shallow boundaries and drops the stale grafts depth 1 fetches leave behind
func repairShallowGrafts(ctx context.Context, w io.Writer, root string) {
	if repaired := tools.RepairBrokenShallowBoundaries(ctx, root); repaired != 0 {
		fmt.Fprintf(w, "  (restored %d broken shallow boundary(ies))\n", repaired)
	}
	if pruned := tools.PruneStaleShallowGrafts(ctx, root); pruned != 0 {
		fmt.Fprintf(w, "  (pruned %d stale shallow graft(s) left by past depth-1 checks)\n", pruned)
	}
}

// Whether the fetched compare ref resolves in the checkout
func compareRefExists(ctx context.Context, gitCmd []string, root, compareBranch string) (bool, error) {
	result, err := updateCmdCheckGit(ctx, gitCmd, root, []string{"rev-parse", "--verify", "--quiet", compareBranch}, ChildOptions{})
	if err != nil {
		return false, err
	}
	return result.ReturnCode == 0, nil
}

// Singular or plural noun of a commit count
func commitsWord(count int) string {
	if count == 1 {
		return "commit"
	}
	return "commits"
}

// Reports behind-ness across a shallow boundary from the tip SHAs and GitHub's comparison
func reportShallowVerdict(ctx context.Context, w io.Writer, gitCmd []string, root, compareBranch string) error {
	head, err := updateCmdCheckGit(ctx, gitCmd, root, []string{"rev-parse", "HEAD"}, ChildOptions{})
	if err != nil {
		return err
	}
	target, err := updateCmdCheckGit(ctx, gitCmd, root, []string{"rev-parse", compareBranch}, ChildOptions{})
	if err != nil {
		return err
	}
	headSHA, targetSHA := pystr.Strip(head.Stdout), pystr.Strip(target.Stdout)
	if headSHA != "" && targetSHA != "" && headSHA == targetSHA {
		fmt.Fprintln(w, "✓ Already up to date.")
		return nil
	}
	counted := updateCompareBehind(ctx, headSHA, targetSHA, OfficialRepository)
	if counted == 0 {
		fmt.Fprintln(w, "✓ Already up to date.")
		return nil
	}
	if counted > 0 {
		fmt.Fprintf(w, "⚕ Update available: %d %s behind %s.\n", counted, commitsWord(counted), compareBranch)
	} else {
		fmt.Fprintf(w, "⚕ Update available (behind %s).\n", compareBranch)
	}
	command, err := config.RecommendedUpdateCommand(ctx, home.Dir(ctx))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "  Run '%s' to install.\n", command)
	return nil
}

// Reports behind-ness from the commit count between HEAD and the compare ref
func reportRevListVerdict(ctx context.Context, w io.Writer, gitCmd []string, root, compareBranch string) error {
	revResult, err := updateCmdCheckGit(ctx, gitCmd, root, []string{"rev-list", "HEAD.." + compareBranch, "--count"}, ChildOptions{Check: true})
	if err != nil {
		return err
	}
	behind, err := strconv.Atoi(pystr.Strip(revResult.Stdout))
	if err != nil {
		return &ValueError{Msg: fmt.Sprintf("invalid literal for int() with base 10: %s", pyrepr.Repr(pystr.Strip(revResult.Stdout)))}
	}
	if behind == 0 {
		fmt.Fprintln(w, "✓ Already up to date.")
		return nil
	}
	fmt.Fprintf(w, "⚕ Update available: %d %s behind %s.\n", behind, commitsWord(behind), compareBranch)
	command, err := config.RecommendedUpdateCommand(ctx, home.Dir(ctx))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "  Run '%s' to install.\n", command)
	return nil
}

// Target an update moves to, its release commit, whether HEAD already passed it, and its repository
type applyTarget struct {
	Ref, ReleaseSHA string
	IsHead          bool
	Repository      string
}

// Resolves the update's target before any tree write, recording branch, pin and retirement on the request
func selectApplyTarget(ctx context.Context, w io.Writer, args map[string]any, branch string, request map[string]any, gitCmd []string, stop func() error) (applyTarget, error) {
	if pyrepr.Truthy(args["branch"]) {
		return applyTarget{Ref: "origin/" + branch}, nil
	}
	root := fleetProjectRoot()
	selected, err := checkRunChannel(ctx, args)
	if err != nil {
		return applyTarget{}, err
	}
	cfg, err := checkReadableConfig(ctx, filepath.Join(pyrepr.Str(request["home"]), "config.yaml"))
	if err != nil {
		return applyTarget{}, err
	}
	original := deepCopyRecord(ChannelRecord(cfg, root))
	fmt.Fprintf(w, "→ Update channel: %s\n", selected)
	forwardOnly, err := RidesDefaultChannel(original, selected, root)
	var target SourceTarget
	if err == nil {
		target, err = checkResolveSourceTarget(WithRetryingReads(ctx), selected, gitCmd, root, "", forwardOnly)
	}
	if err != nil {
		if !isSourceResolveError(err) {
			return applyTarget{}, err
		}
		fmt.Fprintf(w, "✗ Could not resolve the %s source channel: %s. No update was applied.\n", selected, launchErrorText(err))
		if err := stop(); err != nil {
			return applyTarget{}, err
		}
		recordStop(ctx, "channel_unresolved")
		return applyTarget{}, &pm.SystemExit{Code: 1}
	}
	if target.Retired() {
		fmt.Fprintf(w, "→ %s retired; source destination: %s\n", selected, target.Channel)
		previous, recorded := original["channel"]
		if !recorded {
			previous = "main"
		}
		if !pyrepr.Truthy(args["channel"]) && pyrepr.Equal(previous, selected) {
			request["channel_retirement"] = map[string]any{"original": original, "destination": target.Channel}
		}
	}
	if target.Commit != "" {
		label := "Latest release"
		if target.Ahead {
			label = "Release"
		}
		fmt.Fprintf(w, "→ %s: %s\n", label, target.Label())
		request["expected_sha"] = target.Commit
		if target.Channel == "stable" && target.Version != "" && !target.Ahead {
			request["release_tag"] = "v" + target.Version
		}
		return applyTarget{Ref: target.Commit, ReleaseSHA: target.Commit, IsHead: target.Ahead, Repository: target.Repository}, nil
	}
	request["branch"] = target.Branch
	return applyTarget{Ref: "origin/" + target.Branch, Repository: target.Repository}, nil
}

// Fetches a pinned release's own tag when the checkout lacks it, a failure warned and never fatal
func fetchReleaseTag(ctx context.Context, w io.Writer, gitCmd []string, root string, request map[string]any) error {
	if !pyrepr.Truthy(request["release_tag"]) {
		return nil
	}
	tag := pyrepr.Str(request["release_tag"])
	detail, failed, err := func() ([]string, bool, error) {
		have, err := updateCmdCheckGit(ctx, gitCmd, root, []string{"rev-parse", "--verify", "--quiet", "refs/tags/" + tag}, ChildOptions{})
		if err == nil && have.ReturnCode == 0 {
			return nil, false, nil
		}
		var fetched *CompletedProcess
		if err == nil {
			kwargs := noPromptGitKwargs()
			kwargs.Timeout = networkGitTimeoutSeconds * time.Second
			fetched, err = updateCmdCheckGit(ctx, gitCmd, root, []string{"fetch", "--no-tags", "origin", fmt.Sprintf("refs/tags/%s:refs/tags/%s", tag, tag)}, kwargs)
		}
		if err != nil {
			if !isOSOrSubprocessError(err) {
				return nil, false, err
			}
			return []string{launchErrorText(err)}, true, nil
		}
		if fetched.ReturnCode == 0 {
			return nil, false, nil
		}
		lines := pystr.Splitlines(pystr.Strip(fetched.Stderr), false)
		if len(lines) > 1 {
			lines = lines[len(lines)-1:]
		}
		return lines, true, nil
	}()
	if err != nil || !failed {
		return err
	}
	reason := "git fetch failed"
	if len(detail) > 0 {
		reason = detail[0]
	}
	updateCheckLog.Warn(fmt.Sprintf("Could not fetch release tag %s: %s", tag, reason))
	fmt.Fprintf(w, "⚠ Could not fetch the %s tag (%s); the version shown may name the previous "+
		"release until the next update fetches it.\n", tag, reason)
	return nil
}

var updateCheckLog = updateLog.With("logger", "hermes_cli.update_cmd_check")

// HEAD's sha when the channel's release predates the gateway runtime these stores use, else empty
func holdChannelReleaseBehindRuntime(ctx context.Context, w io.Writer, gitCmd []string, targetRef, channel string) string {
	root := fleetProjectRoot()
	if downgradeRefusalGit(ctx, gitCmd, root, targetRef, home.Dir(ctx)) == nil {
		return ""
	}
	head := captureHeadSHA(ctx, gitCmd, root)
	if head == "" {
		return ""
	}
	fmt.Fprintf(w, "→ This build is newer than the %s release %s: that release predates the "+
		"gateway runtime your sessions use, so there is nothing to update to yet.\n", channel, clip(targetRef, 12))
	fmt.Fprintf(w, "  Icebath stays on %s until a newer %s release ships. "+
		"To follow every commit instead: `icebath update --set-channel main`.\n", clip(head, 12), channel)
	return head
}

// First n characters of a string, the whole of a shorter one
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
