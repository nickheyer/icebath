package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Launchd calls of the restart phase the hermes tests replace
var (
	fleetLaunchdLabel          = gateway.GetLaunchdLabel
	fleetLaunchdRestart        = gateway.LaunchdRestart
	fleetLaunchctlSupervised   = gateway.LaunchctlSupervisedPID
	fleetWaitLaunchdSupervised = func(ctx context.Context, label string, oldPid int, haveOld bool) (bool, error) {
		return waitForLaunchdGatewaySupervision(ctx, label, oldPid, haveOld)
	}
	fleetLaunchdLabelsForInstall = gateway.LaunchdGatewayLabelsForInstall
	fleetLegacyLaunchdLabels     = func(ctx context.Context, exclude map[string]bool) ([]string, error) {
		return legacyLaunchdLabelsForInstall(ctx, exclude)
	}
	fleetLocateLaunchdService = gateway.LocateLaunchdGatewayService
	fleetRestartOwnLaunchd    = restartLaunchdGatewayAfterUpdate
	fleetLaunchdKickstart     = gateway.LaunchdKickstart
	fleetWaitLaunchdPID       = gateway.WaitForLaunchdServicePID
)

// Restarts the invoking profile's launchd gateway and returns the restarted and the failed labels
func restartLaunchdGatewayAfterUpdate(ctx context.Context, w io.Writer, supervisionVerify bool, selfRestartPending map[int]bool) ([]string, []string, error) {
	currentLabel, err := fleetLaunchdLabel(ctx)
	if err != nil {
		return nil, nil, err
	}
	oldPID, haveOld := 0, false
	failure := func() error {
		plistPath, err := fleetLaunchdPlistPath(ctx)
		if err != nil {
			return err
		}
		if !existsFollow(plistPath) {
			return errNoLaunchdInstall
		}
		if supervisionVerify {
			oldPID, haveOld = fleetLaunchctlSupervised(ctx, currentLabel)
		}
		return fleetLaunchdRestart(ctx)
	}()
	var called *gateway.CalledProcessError
	switch {
	case failure == nil:
	case errors.Is(failure, errNoLaunchdInstall):
		return []string{}, []string{}, nil
	case errors.As(failure, &called):
		fmt.Fprintf(w, "  ⚠ Gateway restart failed: %s\n    The gateway may be DOWN on pre-update code. Recover manually: icebath gateway restart\n", pystr.Strip(called.Stderr))
		return []string{}, []string{currentLabel}, nil
	case isCommandMissing(failure):
		fmt.Fprintf(w, "  ⚠ Could not restart the gateway (FileNotFoundError: %s).\n    Recover manually: icebath gateway restart\n", launchErrorText(failure))
		return []string{}, []string{currentLabel}, nil
	case asCommandTimeout(failure) != nil:
		fmt.Fprintf(w, "  ⚠ Could not restart the gateway (TimeoutExpired: %s).\n    Recover manually: icebath gateway restart\n", launchErrorText(failure))
		return []string{}, []string{currentLabel}, nil
	default:
		return nil, nil, failure
	}

	if !supervisionVerify {
		return []string{currentLabel}, []string{}, nil
	}
	if haveOld && fleetIsPidAncestor(oldPID) {
		if selfRestartPending != nil {
			selfRestartPending[oldPID] = true
		}
		return []string{currentLabel}, []string{}, nil
	}
	supervised, err := fleetWaitLaunchdSupervised(ctx, currentLabel, oldPID, haveOld)
	if err != nil {
		return nil, nil, err
	}
	if supervised {
		return []string{currentLabel}, []string{}, nil
	}
	fmt.Fprintf(w, "  ✗ %s restarted but launchd is not supervising a new process for it.\n    Check logs, then: icebath gateway restart\n", currentLabel)
	return []string{}, []string{currentLabel}, nil
}

var errNoLaunchdInstall = errors.New("no launchd plist is installed")

// Restarts every launchd managed gateway of this install, a launchctl timeout isolated to its label
func restartMacosLaunchdGateways(ctx context.Context, w io.Writer, restartedServices, failedOrStaleUnits *[]string, drainBudget float64, requireSupervision bool, selfRestartPending map[int]bool) error {
	if requireSupervision {
		listing, err := fleetRunCommand(ctx, []string{"launchctl", "list"}, 10)
		if err != nil {
			return err
		}
		if listing.Returncode != 0 {
			*failedOrStaleUnits = append(*failedOrStaleUnits, "launchd (listing failed)")
			return nil
		}
	}
	currentLabel, err := fleetLaunchdLabel(ctx)
	if err != nil {
		return err
	}
	resumed := alreadyRestarted().Labels
	if !resumed[currentLabel] {
		restarted, failed, err := fleetRestartOwnLaunchd(ctx, w, true, selfRestartPending)
		if err != nil {
			return err
		}
		*restartedServices = append(*restartedServices, restarted...)
		*failedOrStaleUnits = append(*failedOrStaleUnits, failed...)
	}

	derivedLabels, err := fleetLaunchdLabelsForInstall(ctx)
	if err != nil {
		return err
	}
	exclude := map[string]bool{currentLabel: true}
	for _, label := range derivedLabels {
		exclude[label] = true
	}
	legacyLabels, err := fleetLegacyLaunchdLabels(ctx, exclude)
	if err != nil {
		return err
	}
	if len(legacyLabels) > 0 {
		fmt.Fprintf(w, "  ↻ legacy-labelled units of this install join the restart: %s\n", strings.Join(legacyLabels, ", "))
	}
	for _, label := range append(append([]string{}, derivedLabels...), legacyLabels...) {
		if label == currentLabel || resumed[label] {
			continue
		}
		if foreignHome, foreign := launchdLabelForeignHome(ctx, label); foreign {
			fmt.Fprintln(w, describeSkippedRuntime("launchd job", label, foreignHome, true))
			continue
		}
		err := restartSiblingLaunchdGateway(ctx, w, label, restartedServices, failedOrStaleUnits, drainBudget, requireSupervision)
		if err == nil {
			continue
		}
		if asCommandTimeout(err) == nil {
			return err
		}
		*failedOrStaleUnits = append(*failedOrStaleUnits, label)
		fmt.Fprintf(w, "  ⚠ launchctl timed out restarting %s; continuing with remaining gateways\n", label)
	}
	return nil
}

// Drains then kickstarts one sibling label in the domain that manages it, recording where it settled
func restartSiblingLaunchdGateway(ctx context.Context, w io.Writer, label string, restartedServices, failedOrStaleUnits *[]string, drainBudget float64, requireSupervision bool) error {
	domain, oldPID, haveOld, err := fleetLocateLaunchdService(ctx, label)
	if err != nil {
		return err
	}
	if domain == "" {
		if requireSupervision {
			plistPath, err := fleetLaunchdPlistPath(ctx)
			if err != nil {
				return err
			}
			if existsFollow(filepath.Join(filepath.Dir(plistPath), label+".plist")) {
				*failedOrStaleUnits = append(*failedOrStaleUnits, label)
			}
		}
		return nil
	}
	gracefulOK := false
	if haveOld && oldPID > 0 {
		fmt.Fprintf(w, "  → %s: draining (up to %.0fs)...\n", label, drainBudget)
		progress := drainProgressReporter(ctx, gatewayHomeForPid(oldPID), drainBudget, drainReportIntervalS, func(report string) { fmt.Fprintln(w, report) })
		gracefulOK = fleetGracefulRestart(oldPID, drainBudget, progress)
	}
	if gracefulOK {
		fresh, err := fleetWaitLaunchdPID(ctx, label, oldPID, haveOld, 10.0, domain)
		if err != nil {
			return err
		}
		if fresh {
			*restartedServices = append(*restartedServices, label)
			return nil
		}
	}
	if err := fleetLaunchdKickstart(ctx, label, domain); err != nil {
		var called *gateway.CalledProcessError
		if !errors.As(err, &called) {
			return err
		}
		*failedOrStaleUnits = append(*failedOrStaleUnits, label)
		fmt.Fprintf(w, "  ⚠ Failed to restart %s: %s\n    Recover manually: launchctl kickstart -k %s/%s\n", label, pystr.Strip(called.Stderr), domain, label)
		return nil
	}
	fresh, err := fleetWaitLaunchdPID(ctx, label, oldPID, haveOld, 15.0, domain)
	if err != nil {
		return err
	}
	if fresh {
		*restartedServices = append(*restartedServices, label)
		return nil
	}
	*failedOrStaleUnits = append(*failedOrStaleUnits, label)
	fmt.Fprintf(w, "  ✗ %s failed to come back after restart.\n    Check logs, then: launchctl kickstart -k %s/%s\n", label, domain, label)
	return nil
}
