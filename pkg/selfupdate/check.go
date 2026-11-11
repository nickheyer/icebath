package selfupdate

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Reports whether the live binary is the target release's host asset, by digest or by the stamped tag
func upToDateWith(projectRoot string, target ReleaseTarget) (bool, error) {
	if asset := target.Release.Asset(hostAssetName()); asset != nil && digestSHA256(asset.Digest) != "" {
		live, err := LiveBinarySHA256(projectRoot)
		if err != nil {
			return false, err
		}
		return live == digestSHA256(asset.Digest), nil
	}
	return updaterecord.BuildVersion() == target.Release.Tag, nil
}

var resolveCheckTarget = ResolveReleaseTarget

// Prints the selected channel's release verdict for the update check, exit 1 when the channel cannot resolve
func CheckChannel(ctx context.Context, w io.Writer, selected, projectRoot string) error {
	root := rootOrRepo(projectRoot)
	fmt.Fprintf(w, "→ Update channel: %s\n", selected)
	cfg, err := config.RequireReadableConfigBeforeWrite(ctx, home.ConfigPath(home.Dir(ctx)))
	if err != nil {
		return err
	}
	forwardOnly, err := RidesDefaultChannel(ChannelRecord(cfg, root), selected, root)
	if err != nil {
		return err
	}
	target, err := resolveCheckTarget(ctx, selected, forwardOnly)
	if err != nil {
		fmt.Fprintf(w, "✗ Could not resolve the %s source channel: %v\n", selected, err)
		return &pm.SystemExit{Code: 1}
	}
	if target.Retired() {
		fmt.Fprintf(w, "→ %s retired; source destination: %s\n", selected, target.Channel)
	}
	if target.Ahead {
		fmt.Fprintf(w, "✓ No newer release: %s.\n", target.Label())
		return nil
	}
	current, err := upToDateWith(root, target)
	if err != nil {
		return err
	}
	if current {
		fmt.Fprintf(w, "✓ Up to date with the latest release (%s).\n", target.Label())
		return nil
	}
	refuses, err := downgradeWouldRefuse(ctx, root, target.Release)
	if err != nil {
		return err
	}
	switch {
	case refuses:
		fmt.Fprintf(w, "→ Holding at this build: %s predates the gateway runtime your sessions use.\n", target.Label())
		fmt.Fprintln(w, "  `icebath update` stays on this build; the next release that includes the runtime"+
			" installs normally (`icebath update --set-channel main` follows every commit).")
	case CompareVersions(updaterecord.BuildVersion(), target.Release.Tag) > 0:
		fmt.Fprintf(w, "→ This checkout is newer than the selected release: %s.\n", target.Label())
		fmt.Fprintln(w, "  `icebath update` moves it back to that release"+
			" (`icebath update --set-channel main` follows every commit).")
	default:
		fmt.Fprintf(w, "→ Selected release available: %s\n", target.Label())
		fmt.Fprintln(w, "  Run `icebath update` to install it.")
	}
	return nil
}

// Resolves the update's release before any swap and records expected_sha256, release_tag and retirement on request
func SelectApplyTarget(ctx context.Context, w io.Writer, pinnedVersion, transientChannel *string, request map[string]any, stop func()) (ReleaseTarget, error) {
	if pinnedVersion != nil {
		rel, err := ReleaseByTag(ctx, OfficialRepository, *pinnedVersion)
		if err != nil {
			return ReleaseTarget{}, err
		}
		request["release_tag"] = rel.Tag
		return ReleaseTarget{RequestedChannel: *pinnedVersion, Channel: *pinnedVersion, Repository: OfficialRepository, Release: rel, Version: strings.TrimPrefix(rel.Tag, "v")}, nil
	}
	root := rootOrRepo("")
	selected, err := releaseRunChannel(ctx, transientChannel)
	if err != nil {
		return ReleaseTarget{}, err
	}
	homeDir, _ := request["home"].(string)
	cfg, err := config.RequireReadableConfigBeforeWrite(ctx, home.ConfigPath(homeDir))
	if err != nil {
		return ReleaseTarget{}, err
	}
	original := deepCopyRecord(ChannelRecord(cfg, root))
	fmt.Fprintf(w, "→ Update channel: %s\n", selected)
	forwardOnly, err := RidesDefaultChannel(original, selected, root)
	if err != nil {
		return ReleaseTarget{}, err
	}
	target, err := ResolveReleaseTarget(ctx, selected, forwardOnly)
	if err != nil {
		fmt.Fprintf(w, "✗ Could not resolve the %s source channel: %v. No update was applied.\n", selected, err)
		stop()
		recordStop(ctx, "channel_unresolved")
		return ReleaseTarget{}, &pm.SystemExit{Code: 1}
	}
	if target.Retired() {
		fmt.Fprintf(w, "→ %s retired; source destination: %s\n", selected, target.Channel)
		previous, ok := original["channel"]
		if !ok {
			previous = ChannelMain
		}
		if transientChannel == nil && previous == selected {
			request["channel_retirement"] = map[string]any{"original": original, "destination": target.Channel}
		}
	}
	label := "Latest release"
	if target.Ahead {
		label = "Release"
	}
	fmt.Fprintf(w, "→ %s: %s\n", label, target.Label())
	if asset := target.Release.Asset(hostAssetName()); asset != nil && !target.Ahead {
		request["expected_sha256"] = digestSHA256(asset.Digest)
	}
	if !target.Ahead {
		request["release_tag"] = target.Release.Tag
	}
	return target, nil
}

// Copies a channel record so later config writes cannot alias the retirement baseline
func deepCopyRecord(record map[string]any) map[string]any {
	out := make(map[string]any, len(record))
	for key, value := range record {
		if nested, ok := value.(map[string]any); ok {
			value = deepCopyRecord(nested)
		}
		out[key] = value
	}
	return out
}

// Holds the install when the channel's release predates the gateway runtime these stores use, true when held
func HoldChannelReleaseBehindRuntime(ctx context.Context, w io.Writer, projectRoot string, target ReleaseTarget, channel string) (bool, error) {
	refusal, err := downgradeRefusal(ctx, rootOrRepo(projectRoot), target.Release)
	if err != nil || refusal == nil {
		return false, err
	}
	fmt.Fprintf(w, "→ This build is newer than the %s release %s: that release predates the "+
		"gateway runtime your sessions use, so there is nothing to update to yet.\n", channel, target.Release.Tag)
	fmt.Fprintf(w, "  icebath stays on %s until a newer %s release ships. "+
		"To follow every commit instead: `icebath update --set-channel main`.\n", updaterecord.BuildVersion(), channel)
	return true, nil
}

// Channel of this run, recording the channel_unresolved stop when the configured channel is invalid
func releaseRunChannel(ctx context.Context, transientChannel *string) (string, error) {
	channel, err := SourceUpdateChannel(ctx, false, transientChannel, "")
	if err != nil && IsValueError(err) {
		recordStop(ctx, "channel_unresolved")
	}
	return channel, err
}

// Names the exit about to stop this update as one closed token on the open receipt
func recordStop(ctx context.Context, reason string) {
	updaterecord.RecordStopReason(ctx, reason)
}

// Names an exit that fires before the receipt opens as a metrics row only
func recordStopWithoutReceipt(ctx context.Context, reason, withoutReceipt string) {
	updaterecord.RecordStopWithoutReceipt(ctx, reason, withoutReceipt)
}
