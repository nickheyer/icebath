package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

// Launchd label of the default profile gateway, profile labels append a dash and the profile suffix
const LaunchdLabelBase = launchdLabelBase

// Launchd label prefixes earlier releases installed gateway and runtime jobs under
var RetiredLaunchdLabelPrefixes = []string{"ai.icebath.", "io.nousresearch.icebath-agent."}

// One gateway service definition installed under a name this release replaced
type RetiredService struct {
	Name, Path string
	System     bool
}

// Whether a launchd label belongs to an icebath job under the current prefix or a retired one
func IsIcebathLaunchdLabel(label string) bool {
	if strings.HasPrefix(label, launchdJobLabelPrefix) {
		return true
	}
	for _, prefix := range RetiredLaunchdLabelPrefixes {
		if strings.HasPrefix(label, prefix) {
			return true
		}
	}
	return false
}

// LaunchAgent file patterns of every gateway label of every profile, the current base first then each retired one
func LaunchdGatewayPlistPatterns() []string {
	out := []string{launchdLabelBase + "*.plist"}
	for _, prefix := range RetiredLaunchdLabelPrefixes {
		out = append(out, prefix+"gateway*.plist")
	}
	return out
}

// Whether a launchd label names a gateway service under the current base or a retired one
func isGatewayLaunchdLabel(label string) bool {
	if strings.HasPrefix(label, launchdLabelBase) {
		return true
	}
	for _, prefix := range RetiredLaunchdLabelPrefixes {
		if strings.HasPrefix(label, prefix+"gateway") {
			return true
		}
	}
	return false
}

// Whether the resolved pinned home of a definition is the home in effect, a definition pinning none owned by the bare name owner
func retiredDefinitionBelongs(ctx context.Context, definition string, unpinnedOwned bool) (bool, error) {
	raw, err := ServicePinnedHome(definition)
	if err != nil {
		return false, err
	}
	if raw == "" {
		return unpinnedOwned, nil
	}
	pinned, okPinned := resolveOrNone(raw)
	current, okCurrent := resolveOrNone(home.Dir(ctx))
	return okPinned && okCurrent && pinned == current, nil
}

// LaunchAgents of the home in effect under every retired label, the suffix the current label carries
func retiredLaunchdServices(ctx context.Context) ([]RetiredService, error) {
	plist, err := launchdPlistPathOf(ctx)
	if err != nil {
		return nil, err
	}
	suffix, err := ProfileSuffix(ctx)
	if err != nil {
		return nil, err
	}
	var out []RetiredService
	for _, prefix := range RetiredLaunchdLabelPrefixes {
		label := prefix + "gateway"
		if suffix != "" {
			label += "-" + suffix
		}
		path := filepath.Join(filepath.Dir(plist), label+".plist")
		if !pathExistsSafe(path) {
			continue
		}
		belongs, err := retiredDefinitionBelongs(ctx, path, true)
		if err != nil {
			return nil, err
		}
		if belongs {
			out = append(out, RetiredService{Name: label, Path: path})
		}
	}
	return out, nil
}

// Pre rename systemd units that run the gateway of the home in effect
func retiredSystemdServices(ctx context.Context) ([]RetiredService, error) {
	resolved, err := pathutil.Resolve(home.Dir(ctx))
	if err != nil {
		return nil, err
	}
	ownsBare, err := homeOwnsBareServiceName(ctx, resolved)
	if err != nil {
		return nil, err
	}
	var out []RetiredService
	for _, u := range findLegacyIcebathUnits(ctx) {
		belongs, err := retiredDefinitionBelongs(ctx, u.path, ownsBare)
		if err != nil {
			return nil, err
		}
		if belongs {
			out = append(out, RetiredService{Name: u.name, Path: u.path, System: u.system})
		}
	}
	return out, nil
}

// Gateway services of the home in effect installed under a retired name, LaunchAgents on macOS and units under systemd
func RetiredGatewayServices(ctx context.Context) ([]RetiredService, error) {
	switch {
	case IsMacOS():
		return retiredLaunchdServices(ctx)
	case SupportsSystemdServices(ctx):
		return retiredSystemdServices(ctx)
	}
	return nil, nil
}

// Boots out and deletes one retired LaunchAgent, reporting whether launchd was running its gateway
func removeRetiredLaunchdService(ctx context.Context, w io.Writer, svc RetiredService) (bool, error) {
	domain, err := launchdDomainOf(ctx)
	if err != nil {
		return false, err
	}
	running := installHooks.supervising(ctx, svc.Name)
	if _, err := launchctlRun(ctx, launchdLongTimeout, true, "launchctl", "bootout", domain+"/"+svc.Name); err != nil {
		return false, err
	}
	if err := os.Remove(svc.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	fmt.Fprintf(w, "✓ Removed %s (gateway service under the retired label %s)\n", svc.Path, svc.Name)
	return running, nil
}

// Stops, disables and deletes one retired unit, reporting whether systemd was running it
func removeRetiredSystemdUnit(ctx context.Context, w io.Writer, svc RetiredService) (bool, error) {
	active, err := lifecycleRunSystemctl(ctx, []string{"is-active", svc.Name}, svc.System, 30*time.Second)
	if err != nil {
		return false, err
	}
	running := active.ReturnCode == 0 && strings.TrimSpace(active.Stdout) == "active"
	for _, step := range []struct {
		verb    string
		timeout time.Duration
	}{{"stop", 90 * time.Second}, {"disable", 30 * time.Second}} {
		if _, err := lifecycleRunSystemctl(ctx, []string{step.verb, svc.Name}, svc.System, step.timeout); err != nil {
			return false, err
		}
	}
	if err := os.Remove(svc.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := runSystemctlChecked(ctx, []string{"daemon-reload"}, svc.System, 30*time.Second); err != nil {
		return false, err
	}
	if _, err := lifecycleRunSystemctl(ctx, []string{"reset-failed", svc.Name}, svc.System, 30*time.Second); err != nil {
		return false, err
	}
	fmt.Fprintf(w, "✓ Removed %s (gateway service under the retired name %s, %s scope)\n", svc.Path, svc.Name, ServiceScopeLabel(svc.System))
	return running, nil
}

// Stops and deletes each service find reports, telling whether any was removed, whether any ran and whether any was system scope
func removeRetired(ctx context.Context, w io.Writer, find func(context.Context) ([]RetiredService, error),
	remove func(context.Context, io.Writer, RetiredService) (bool, error)) (removed, running, system bool, err error) {
	retired, err := find(ctx)
	if err != nil {
		return false, false, false, err
	}
	for _, svc := range retired {
		if svc.System && serviceGeteuid() != 0 {
			con := serviceConsole(w)
			con.PrintWarning(fmt.Sprintf("System gateway service %s runs under the retired name %s and needs root to move.", svc.Path, svc.Name))
			con.PrintInfo("  Re-run with: sudo icebath gateway install --system")
			continue
		}
		ran, err := remove(ctx, w, svc)
		if err != nil {
			return removed, running, system, err
		}
		removed, running, system = true, running || ran, system || svc.System
	}
	return removed, running, system, nil
}

// Stops and deletes every retired service of the home in effect, reporting whether any ran and whether any was system scope
func RemoveRetiredGatewayServices(ctx context.Context, w io.Writer) (removed, running, system bool, err error) {
	switch {
	case IsMacOS():
		return removeRetired(ctx, w, retiredLaunchdServices, removeRetiredLaunchdService)
	case SupportsSystemdServices(ctx):
		return removeRetired(ctx, w, retiredSystemdServices, removeRetiredSystemdUnit)
	}
	return false, false, false, nil
}

// One service moved off a retired name, its current name and whether it is system scope
type MovedService struct {
	Name   string
	System bool
}

// Moves retired services of the home in effect to the current name, starting the new one when the old one ran
func MigrateRetiredGatewayServices(ctx context.Context, w io.Writer) (bool, error) {
	moved, err := migrateRetired(ctx, w)
	return moved != nil, err
}

// Moved service of the home in effect, nil when nothing was installed under a retired name
func migrateRetired(ctx context.Context, w io.Writer) (*MovedService, error) {
	removed, running, system, err := RemoveRetiredGatewayServices(ctx, w)
	if err != nil || !removed {
		return nil, err
	}
	name, err := currentServiceName(ctx)
	if err != nil {
		return nil, err
	}
	moved := &MovedService{Name: name, System: system}
	installed, err := isServiceInstalled(ctx)
	if err != nil {
		return moved, err
	}
	if IsMacOS() {
		if installed {
			if running {
				return moved, LaunchdStart(ctx)
			}
			return moved, nil
		}
		return moved, launchdInstall(ctx, w, false, running, false)
	}
	if !installed {
		if err := systemdInstall(ctx, w, false, system, "", true, true, false); err != nil {
			return moved, err
		}
	}
	if running {
		return moved, SystemdStart(ctx, system)
	}
	return moved, nil
}

// Moves the retired services of every profile of this install, the default home first
func MigrateRetiredGatewayServicesForInstall(ctx context.Context, w io.Writer) ([]MovedService, error) {
	list, err := listInstallProfiles(ctx)
	if err != nil {
		return nil, err
	}
	var moved []MovedService
	for _, p := range list {
		h := profiles.DefaultHome()
		if !p.IsDefault {
			h = filepath.Join(profiles.ProfilesRoot(), p.Name)
		}
		one, err := migrateRetired(home.WithOverride(ctx, h), w)
		if one != nil {
			moved = append(moved, *one)
		}
		if err != nil {
			return moved, err
		}
	}
	return moved, nil
}

// Launchd label on macOS and systemd unit name elsewhere of the home in effect
func currentServiceName(ctx context.Context) (string, error) {
	if IsMacOS() {
		return GetLaunchdLabel(ctx)
	}
	return GetServiceName(ctx)
}
