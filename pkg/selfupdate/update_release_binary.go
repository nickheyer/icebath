package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Release download and swap, source check and build revision of a release binary update
var (
	releaseBinaryUpdate = install.UpdateBinary
	releaseSourceCheck  = CheckForUpdates
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

// Reports a release install's distance from its channel through the source check of its build revision
func checkReleaseUpdate(ctx context.Context, w io.Writer, branch string, branchExplicit bool, channel *string) error {
	opts := SourceCheckOptions{Force: true, Channel: channel}
	if branchExplicit {
		opts.Branch = &branch
	}
	status, err := releaseSourceCheck(ctx, opts)
	if err != nil {
		return err
	}
	behind, _ := status.Get("behind")
	label := branch
	if selected, ok := status.Get("channel"); ok && pyrepr.Truthy(selected) {
		label = pyrepr.Str(selected)
	} else if selected, ok := status.Get("branch"); ok && pyrepr.Truthy(selected) {
		label = pyrepr.Str(selected)
	}
	count, counted := pyrepr.Int(behind)
	switch {
	case !counted:
		reason := "the release this binary was built from is unknown"
		for _, key := range []string{"message", "error", "reason"} {
			if value, ok := status.Get(key); ok && pyrepr.Truthy(value) {
				reason = pyrepr.Str(value)
				break
			}
		}
		fmt.Fprintf(w, "✗ Could not check for updates: %s\n", reason)
		return &pm.SystemExit{Code: 1}
	case count.Sign() == 0:
		fmt.Fprintln(w, "✓ Already up to date.")
		return nil
	case pyrepr.Equal(behind, UpdateAvailableNoCount):
		fmt.Fprintf(w, "⚕ Update available (behind %s).\n", label)
	default:
		n := int(count.Int64())
		fmt.Fprintf(w, "⚕ Update available: %d %s behind %s.\n", n, commitsWord(n), label)
	}
	command, err := config.RecommendedUpdateCommand(ctx, home.Dir(ctx))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "  Run '%s' to install.\n", command)
	return nil
}

// Replaces the binary with the selected release, then hands the completion to the new binary
func updateViaRelease(ctx context.Context, w io.Writer, target applyTarget, completionRequest map[string]any) error {
	if completionRequest == nil {
		return pm.StopForRelaunchIncomplete(ctx)
	}
	repository := target.Repository
	if repository == "" {
		repository = OfficialRepository
	}
	branch := ""
	if target.ReleaseSHA == "" {
		branch = strings.TrimPrefix(target.Ref, "origin/")
	}
	result, err := releaseBinaryUpdate(ctx, fleetProjectRoot(), "https://github.com/"+repository, target.ReleaseSHA, branch, w)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fmt.Fprintf(w, "✗ Release update failed: %s\n", launchErrorText(err))
		fmt.Fprintln(w, "  Your existing install was left in place.")
		fmt.Fprintln(w, "  Re-run `icebath update` to retry.")
		return &pm.SystemExit{Code: 1}
	}
	if result.Commit != "" {
		completionRequest["expected_sha"] = result.Commit
	} else {
		completionRequest["expected_sha"] = nil
	}
	if !result.Changed {
		completionRequest["completion_message"] = "✓ Already up to date!"
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

// Tells a source checkout the command that rebuilds icebath and its browser bundles from the updated code
func announceCheckoutRebuild(w io.Writer, root string) {
	if sourceCheckoutBuildsBinary(root) {
		fmt.Fprintf(w, "→ Run make build in %s to rebuild icebath from the updated code\n", root)
	}
}
