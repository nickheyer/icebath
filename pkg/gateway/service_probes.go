package gateway

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// True when a systemctl failure is the RuntimeError or timeout the probes swallow
func systemctlSoftFailure(err error) bool {
	var unavailable SystemctlUnavailableError
	return errors.As(err, &unavailable) || errors.Is(err, ErrCommandTimeout)
}

// Effective scope, the system scope when only the system unit is installed
func SelectSystemdScope(ctx context.Context, system bool) (bool, error) {
	if system {
		return true, nil
	}
	sysUnit, err := GetSystemdUnitPath(ctx, true)
	if err != nil {
		return false, err
	}
	if !pathExistsSafe(sysUnit) {
		return false, nil
	}
	userUnit, err := GetSystemdUnitPath(ctx, false)
	if err != nil {
		return false, err
	}
	return !pathExistsSafe(userUnit), nil
}

// Scope labels with an installed unit, user first, each unit path counted once
func GetInstalledSystemdScopes(ctx context.Context) ([]string, error) {
	var scopes []string
	seen := map[string]bool{}
	for _, system := range []bool{false, true} {
		p, err := GetSystemdUnitPath(ctx, system)
		if err != nil {
			return nil, err
		}
		if !seen[p] && pathExistsSafe(p) {
			scopes = append(scopes, ServiceScopeLabel(system))
			seen[p] = true
		}
	}
	return scopes, nil
}

// True when the scope's unit is installed and systemctl reports it active
func SystemdUnitIsActive(ctx context.Context, system bool) (bool, error) {
	unit, err := GetSystemdUnitPath(ctx, system)
	if err != nil || !pathExistsSafe(unit) {
		return false, err
	}
	name, err := GetServiceName(ctx)
	if err != nil {
		return false, err
	}
	res, err := RunSystemctl(ctx, []string{"is-active", name}, system, 10*time.Second)
	if err != nil {
		if systemctlSoftFailure(err) {
			return false, nil
		}
		return false, err
	}
	return pystr.Strip(res.Stdout) == "active", nil
}

// Selected scope and whether its unit is active
func ProbeSystemdServiceRunning(ctx context.Context, system bool) (bool, bool, error) {
	selected, err := SelectSystemdScope(ctx, system)
	if err != nil {
		return false, false, err
	}
	active, err := SystemdUnitIsActive(ctx, selected)
	return selected, active, err
}

// Key value map of name=value items with values stripped
func parseKVPairs(items []string) map[string]string {
	out := map[string]string{}
	for _, item := range items {
		if k, v, ok := strings.Cut(item, "="); ok {
			out[k] = pystr.Strip(v)
		}
	}
	return out
}

// Requested systemctl show properties of the gateway unit in the selected scope, empty on any failure
func systemctlShow(ctx context.Context, properties []string, system bool) (map[string]string, error) {
	name, err := GetServiceName(ctx)
	if err != nil {
		return nil, err
	}
	scope, err := SelectSystemdScope(ctx, system)
	if err != nil {
		return nil, err
	}
	res, err := RunSystemctl(ctx, []string{"show", name, "--no-pager", "--property", strings.Join(properties, ",")}, scope, 10*time.Second)
	if err != nil || res.ReturnCode != 0 {
		return map[string]string{}, nil
	}
	return parseKVPairs(pystr.Splitlines(res.Stdout, false)), nil
}

// Unit state properties, the default ActiveState SubState Result ExecMainStatus MainPID set when none are named
func ReadSystemdUnitProperties(ctx context.Context, system bool, properties ...string) (map[string]string, error) {
	if len(properties) == 0 {
		properties = []string{"ActiveState", "SubState", "Result", "ExecMainStatus", "MainPID"}
	}
	return systemctlShow(ctx, properties, system)
}

// MainPID of a properties map when positive
func systemdMainPIDFromProps(props map[string]string) (int, bool) {
	v := props["MainPID"]
	if v == "" {
		v = "0"
	}
	return positivePID(v)
}

// MainPID of the gateway unit when positive
func SystemdMainPID(ctx context.Context, system bool) (int, bool, error) {
	props, err := ReadSystemdUnitProperties(ctx, system)
	if err != nil {
		return 0, false, err
	}
	pid, ok := systemdMainPIDFromProps(props)
	return pid, ok, nil
}

// Adopts the system unit's pinned home into ICEBATH_HOME so runtime status and pid reads follow the unit
func SyncHermesHomeFromSystemdUnit(ctx context.Context, system bool) error {
	if !system {
		return nil
	}
	unitHome, err := hermesHomeFromSystemdUnitFile(ctx, true)
	if err != nil {
		return err
	}
	unitHome = pystr.Strip(unitHome)
	if unitHome == "" {
		props, err := systemctlShow(ctx, []string{"Environment"}, true)
		if err != nil {
			return err
		}
		unitHome = pystr.Strip(parseKVPairs(pystr.SplitWhitespace(props["Environment"], -1))["ICEBATH_HOME"])
	}
	if unitHome != "" && pystr.Strip(os.Getenv("ICEBATH_HOME")) != unitHome {
		os.Setenv("ICEBATH_HOME", unitHome)
	}
	return nil
}

// Pid launchd runs for a label, false when unregistered, idle or the probe fails
func LaunchctlSupervisedPID(ctx context.Context, label string) (int, bool) {
	res, err := RunServiceCommand(ctx, 10*time.Second, "launchctl", "list", label)
	if err != nil || res.ReturnCode != 0 {
		return 0, false
	}
	return ParseLaunchdPIDFromListOutput(res.Stdout)
}

// True when launchd knows the label and runs a process for it
func LaunchctlLabelSupervisingProcess(ctx context.Context, label string) bool {
	_, ok := LaunchctlSupervisedPID(ctx, label)
	return ok
}

// True when the plist exists and launchd runs a process for this profile's label
func ProbeLaunchdServiceRunning(ctx context.Context) (bool, error) {
	plist, err := GetLaunchdPlistPath(ctx)
	if err != nil || !pathExistsSafe(plist) {
		return false, err
	}
	label, err := GetLaunchdLabel(ctx)
	if err != nil {
		return false, err
	}
	return LaunchctlLabelSupervisingProcess(ctx, label), nil
}
