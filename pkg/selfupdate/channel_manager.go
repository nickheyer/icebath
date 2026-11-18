//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Persists channel for this install in config.yaml and returns its id, refusing commit builds and owned installs
func SetInstallChannel(ctx context.Context, channel, projectRoot string) (string, error) {
	root := rootOrInstall(projectRoot)
	if IsCommitBuild(root) {
		return "", &ValueError{Msg: CommitBuildUpdateMessage}
	}
	channel, err := ValidateChannelName(channel)
	if err != nil {
		return "", err
	}
	stamp := pm.ReadInstallStamp(root)
	if stampGet(stamp, "updateMechanism") == "external" {
		distribution := stampGet(stamp, "distribution")
		if !pyrepr.Truthy(distribution) {
			distribution = "an external steward"
		}
		return "", &ValueError{Msg: "channels don't apply here; updates are owned by " + pyrepr.Str(distribution)}
	}
	sha16 := InstallID(root)
	if _, err := writeChannelRecord(ctx, sha16, root, channel, nil, false, ""); err != nil {
		return "", err
	}
	return sha16, nil
}

// Prints the install id or persists a channel before any update side effect, true when handled
func HandleMetadataArgs(ctx context.Context, stdout io.Writer, installIDFlag bool, setChannel *string, projectRoot string) (bool, error) {
	if installIDFlag {
		fmt.Fprintln(stdout, InstallID(projectRoot))
		return true, nil
	}
	if setChannel == nil {
		return false, nil
	}
	channel := *setChannel
	key, err := SetInstallChannel(ctx, channel, projectRoot)
	if err != nil {
		if !IsValueError(err) {
			return true, err
		}
		fmt.Fprintln(stdout, err.Error())
		return true, &pm.SystemExit{Code: 2}
	}
	fmt.Fprintf(stdout, "Update channel for %s: %s\n", key, channel)
	switch channel {
	case ChannelCanary:
		fmt.Fprintln(stdout, "Canary builds can write forward-incompatible state. Back up your data before switching.")
	case ChannelStable:
		fmt.Fprintln(stdout, "Switching to an older stable release may not read state written by canary. Back up your data first.")
	}
	return true, nil
}
