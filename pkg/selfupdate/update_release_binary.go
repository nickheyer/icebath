//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Release planning and swap, checkout rebuild and build revision, replaced by tests
var (
	releasePlanner      = install.PlanRelease
	releaseApply        = install.ApplyRelease
	checkoutBinaryBuild = buildCheckoutBinary
	releaseCheckoutSHA  = updaterecord.ReadCheckoutSHA
)

// Reports a tree whose stamped method updates by replacing its binary from a published release
func releaseBinaryInstall(ctx context.Context, root string) (bool, error) {
	if existsFollow(filepath.Join(root, ".git")) || pm.ResolveInstallBinary(root) == "" {
		return false, nil
	}
	method, err := config.DetectInstallMethod(ctx, home.Dir(ctx), root)
	if err != nil {
		return false, err
	}
	return config.IsReleaseBinaryInstallMethod(method), nil
}

// Commit a release binary install was built from, the source check's embedded revision, empty elsewhere
func releaseRevision(ctx context.Context, root string) string {
	if release, err := releaseBinaryInstall(ctx, root); err != nil || !release {
		return ""
	}
	if sha, err := releaseCheckoutSHA(); err == nil && sha != nil && install.IsFullSHA(*sha) {
		return *sha
	}
	return ""
}

// Repository a release install updates from, ICEBATH_REPO_URL first, then the one it recorded, then the official one
func releaseRepository(root string) string {
	if repo := strings.TrimSpace(os.Getenv("ICEBATH_REPO_URL")); repo != "" {
		return repo
	}
	if repo := install.InstalledRepository(root); repo != "" {
		return repo
	}
	return install.DefaultRepoURL
}

// Release a release install's update selects, ordered against the installed build under its channel's rules
type releaseUpdate struct {
	plan    *install.ReleasePlan
	channel string
	follows string
	pinned  bool
	forward bool
	source  *SourceTarget
	record  map[string]any
}

// Failure to name or resolve the update channel, the stop an update records as channel_unresolved
type releaseChannelError struct{ err error }

func (e *releaseChannelError) Error() string { return e.err.Error() }

func (e *releaseChannelError) Unwrap() error { return e.err }

// Whether the update moves back to an older release, only a pinning channel the user chose consenting
func (u *releaseUpdate) rollback() bool {
	return u.plan.Order == install.ReleaseOlder && u.pinned && !u.forward
}

// Whether the update replaces the binary, a newer release or a consented move back
func (u *releaseUpdate) moves() bool { return u.plan.Order == install.ReleaseNewer || u.rollback() }

// Repository GitHub serves the planned release's sources under
func (u *releaseUpdate) sourceRepository() string {
	return u.plan.Target.Repo.Owner + "/" + u.plan.Target.Repo.Name
}

// Whether the update holds at this build, a channel's release it would refuse because it predates the gateway runtime
func (u *releaseUpdate) held(ctx context.Context) bool {
	return u.pinned && u.moves() && downgradeWouldRefuse(ctx, u.sourceRepository(), u.plan.Target.Tag, home.Dir(ctx))
}

// Why the update must not swap in the planned release, nil when it moves nowhere or no ledgered store would meet a pre runtime release
func (u *releaseUpdate) refusal(ctx context.Context) *string {
	if !u.moves() {
		return nil
	}
	h, tag := home.Dir(ctx), u.plan.Target.Tag
	return downgradeRefusalFor(ctx, tag, h, func() bool { return releasePredatesRuntime(ctx, u.sourceRepository(), h, tag) })
}

// Release, its version and commit, the way release channels name their target
func (u *releaseUpdate) label() string {
	label := u.follows + " " + u.plan.Target.Tag
	if commit := u.plan.Target.Commit; commit != "" {
		label += " (" + commit[:min(12, len(commit))] + ")"
	}
	return label
}

// Selects the release and its order for the channel or explicit branch, the one plan check and update read
func planReleaseUpdate(ctx context.Context, root string, channelArg *string, branch string, branchExplicit bool) (*releaseUpdate, error) {
	cfg, err := config.RequireReadableConfigBeforeWrite(ctx, filepath.Join(home.Dir(ctx), "config.yaml"))
	if err != nil {
		return nil, err
	}
	var channel string
	if channelArg != nil && *channelArg != "" {
		channel, err = ValidateChannelName(*channelArg)
	} else {
		channel, err = ResolveUpdateChannel(cfg, root)
	}
	if err != nil {
		return nil, &releaseChannelError{err}
	}
	record := ChannelRecord(cfg, root)
	forward, err := RidesDefaultChannel(record, channel, root)
	if err != nil {
		return nil, err
	}
	u := &releaseUpdate{channel: channel, follows: channel, forward: forward, record: deepCopyRecord(record)}
	repository := releaseRepository(root)
	query := install.ReleaseQuery{Branch: channel}
	switch {
	case branchExplicit:
		query, u.follows = install.ReleaseQuery{Branch: branch}, branch
	case channel == ChannelStable:
		query, u.pinned = install.ReleaseQuery{Stable: true}, true
	case channel != ChannelMain:
		repo, err := install.ParseRepo(repository)
		if err != nil {
			return nil, err
		}
		source, err := checkResolveSourceTarget(WithRetryingReads(ctx), channel, nil, root, repo.Owner+"/"+repo.Name, false)
		if err != nil {
			return nil, &releaseChannelError{err}
		}
		u.source, u.follows = &source, source.Channel
		if source.Commit != "" {
			query, u.pinned = install.ReleaseQuery{Commit: source.Commit}, true
		} else {
			query = install.ReleaseQuery{Branch: source.Branch}
		}
	}
	if u.plan, err = releasePlanner(ctx, root, repository, query); err != nil {
		return nil, err
	}
	return u, nil
}

// Verdict line of a plan that moves nowhere or back, printed alike by the check and the update
func (u *releaseUpdate) heldLine() string {
	switch {
	case u.rollback():
		return fmt.Sprintf("→ This build is newer than the selected release: %s.\n"+
			"  `icebath update` moves it back to that release (`icebath update --set-channel main` follows every commit).", u.label())
	case u.plan.Order == install.ReleaseOlder && u.pinned:
		return fmt.Sprintf("✓ No newer release: %s; this build is newer and waits for the next release"+
			" (`icebath update --set-channel main` follows every commit).", u.label())
	case u.pinned:
		return fmt.Sprintf("✓ Up to date with the latest release (%s).", u.label())
	}
	return "✓ Already up to date."
}

// Reports a release install's distance from its channel through the same plan the update applies
func checkReleaseUpdate(ctx context.Context, w io.Writer, branch string, branchExplicit bool, channel *string) error {
	root := fleetProjectRoot()
	u, err := planReleaseUpdate(ctx, root, channel, branch, branchExplicit)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(w, "✗ Could not check for updates: %s\n", launchErrorText(err))
		return &pm.SystemExit{Code: 1}
	}
	fmt.Fprintf(w, "→ Update channel: %s\n", u.channel)
	if u.held(ctx) {
		fmt.Fprintf(w, "→ Holding at this build: %s predates the gateway runtime your sessions use.\n", u.label())
		fmt.Fprintln(w, "  `icebath update` stays on this build; the next release that includes the runtime"+
			" installs normally (`icebath update --set-channel main` follows every commit).")
		return nil
	}
	if u.plan.Order != install.ReleaseNewer {
		fmt.Fprintln(w, u.heldLine())
		return nil
	}
	command, err := config.RecommendedUpdateCommand(ctx, home.Dir(ctx))
	if err != nil {
		return err
	}
	switch {
	case u.pinned:
		fmt.Fprintf(w, "→ Selected release available: %s\n  Run `%s` to install it.\n", u.label(), command)
	case u.plan.Ahead >= 0:
		fmt.Fprintf(w, "󰆧 Update available: %d %s behind %s.\n  Run '%s' to install.\n", u.plan.Ahead, commitsWord(u.plan.Ahead), u.follows, command)
	default:
		fmt.Fprintf(w, "󰆧 Update available (behind %s).\n  Run '%s' to install.\n", u.follows, command)
	}
	return nil
}

// Swaps in the planned release when it moves, holds or refuses one that predates the runtime, then hands the completion to the binary in place
func updateViaRelease(ctx context.Context, w io.Writer, args map[string]any, completionRequest map[string]any) error {
	if completionRequest == nil {
		return pm.StopForRelaunchIncomplete(ctx)
	}
	root := fleetProjectRoot()
	var channelArg *string
	if pyrepr.Truthy(args["channel"]) {
		text := pyrepr.Str(args["channel"])
		channelArg = &text
	}
	u, err := planReleaseUpdate(ctx, root, channelArg, resolveUpdateBranch(args), pyrepr.Truthy(args["branch"]))
	heldMessage := ""
	if err == nil {
		fmt.Fprintf(w, "→ Update channel: %s\n", u.channel)
		if u.source != nil {
			noteChannelRetirement(w, args, completionRequest, u.record, u.channel, *u.source)
		}
		refusal := u.refusal(ctx)
		switch {
		case refusal != nil && !u.pinned:
			fmt.Fprintln(w, *refusal)
			fmt.Fprintln(w, "  No update was applied; your install is unchanged.")
			recordStop(ctx, "downgrade_refused")
			return updateExit(1)
		case refusal != nil:
			fmt.Fprintf(w, "→ This build is newer than the %s release %s: that release predates the "+
				"gateway runtime your sessions use, so there is nothing to update to yet.\n", u.channel, u.plan.Target.Tag)
			fmt.Fprintf(w, "  Icebath stays on %s until a newer %s release ships. "+
				"To follow every commit instead: `icebath update --set-channel main`.\n", updaterecord.BuildVersion(), u.channel)
			heldMessage = fmt.Sprintf("✓ Holding at this build until a newer %s release ships.", u.channel)
		case u.moves():
			fmt.Fprintf(w, "→ Release: %s\n", u.label())
			err = releaseApply(ctx, root, u.plan.Target, w)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if unresolved := (*releaseChannelError)(nil); errors.As(err, &unresolved) {
			recordStop(ctx, "channel_unresolved")
		}
		fmt.Fprintf(w, "✗ Release update failed: %s\n", launchErrorText(err))
		fmt.Fprintln(w, "  Your existing install was left in place.")
		fmt.Fprintln(w, "  Re-run `icebath update` to retry.")
		return &pm.SystemExit{Code: 1}
	}
	commit := u.plan.Installed.Commit
	switch {
	case heldMessage != "":
		completionRequest["completion_message"] = heldMessage
	case u.moves():
		commit = u.plan.Target.Commit
	default:
		fmt.Fprintln(w, u.heldLine())
		completionRequest["completion_message"] = "✓ Already up to date!"
	}
	if commit != "" {
		completionRequest["expected_sha"] = commit
	} else {
		completionRequest["expected_sha"] = nil
	}
	completionRequest["apply_mode"] = "release"
	return zipCompleteSourceUpdate(ctx, w, completionRequest)
}

// Reports a source checkout of icebath that builds its own binary at the checkout root
func sourceCheckoutBuildsBinary(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return false
	}
	first, _, _ := strings.Cut(string(data), "\n")
	if fields := strings.Fields(first); len(fields) != 2 || fields[0] != "module" || fields[1] != "github.com/nickheyer/icebath" {
		return false
	}
	st, err := os.Stat(filepath.Join(root, "cmd", "icebath"))
	return err == nil && st.IsDir()
}

// Rebuilds the checkout binary from the pulled source as setup-icebath does, the build kept in the update's custody
func buildCheckoutBinary(ctx context.Context, w io.Writer, root string) error {
	gobin, err := exec.LookPath("go")
	if err != nil {
		return fmt.Errorf("Go is required to rebuild %s (see go.mod for the version)", root)
	}
	run := func(env []string, args ...string) error {
		contained, err := ContainedCommand(ctx, append([]string{gobin}, args...), true, root)
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, contained.Argv[0], contained.Argv[1:]...)
		cmd.Dir, cmd.Stdout, cmd.Stderr = root, w, w
		cmd.Env = append(os.Environ(), env...)
		cmd.ExtraFiles, cmd.SysProcAttr = contained.ExtraFiles, contained.Attr
		runErr := cmd.Run()
		if err := contained.Close(); err != nil {
			return err
		}
		if runErr != nil {
			return fmt.Errorf("go %s: %w", strings.Join(args, " "), runErr)
		}
		return nil
	}
	binary := filepath.Join(root, filepath.Base(install.InstallBinaryPath(root)))
	staged := binary + ".build"
	fmt.Fprintln(w, "→ Building the icebath binary...")
	if err := run([]string{"CGO_ENABLED=0"}, "build", "-tags", "goolm", "-trimpath", "-o", staged, "./cmd/icebath"); err != nil {
		os.Remove(staged)
		return err
	}
	if err := install.ReplaceFile(staged, binary); err != nil {
		return fmt.Errorf("cannot place %s: %w", binary, err)
	}
	fmt.Fprintln(w, "✓ Built "+binary)
	return nil
}

// Rebuilds a source checkout's binary after its code moved, the old binary left when the build fails
func rebuildSourceCheckout(ctx context.Context, w io.Writer, root string) error {
	if !sourceCheckoutBuildsBinary(root) {
		return nil
	}
	if err := checkoutBinaryBuild(ctx, w, root); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(w, "✗ Rebuilding icebath failed (the code update itself succeeded): %s\n", launchErrorText(err))
		fmt.Fprintln(w, "  Fix the error above, then run ./setup-icebath.sh or `icebath update` again.")
		return &pm.SystemExit{Code: 1}
	}
	return nil
}
