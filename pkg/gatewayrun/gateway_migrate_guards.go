package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pypwd"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Owner uid of a live process, nil when unknown
var guardsPidUID = func(ctx context.Context, pid int) *int {
	if uid, ok := gateway.PidUID(ctx, pid); ok {
		return &uid
	}
	return nil
}

// Uid a system unit runs as, root without a User line, nil when the account does not resolve
func systemUnitUID(unitPath string) (*int, error) {
	user, err := gateway.ReadSystemdUserFromUnit(unitPath)
	if err != nil {
		return nil, err
	}
	if user == "" {
		root := 0
		return &root, nil
	}
	info, err := pypwd.Getpwnam(user)
	var keyErr *pypwd.KeyError
	if errors.As(err, &keyErr) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &info.UID, nil
}

// Uid and runtime home serving h, from the process owner, a system unit User, else the home owner
func gatewayIdentity(ctx context.Context, h string, pid int, services []ServiceUnit) (*int, string, error) {
	var uid *int
	if pid != 0 {
		uid = guardsPidUID(ctx, pid)
	}
	runtimeHome := h
	hasSystemUnit := false
	for _, s := range services {
		if s.Kind != "systemd" {
			continue
		}
		scoped, restore := HomeEnv(ctx, h)
		unit, err := migrateSystemdUnitPath(scoped, s.System)
		restore()
		if err != nil {
			return nil, "", err
		}
		pinned, err := gateway.HermesHomePinnedByUnit(unit)
		if err != nil {
			return nil, "", err
		}
		if pinned != "" && runtimeHome == h {
			if runtimeHome, err = pypath.PathExpanduser(pinned); err != nil {
				return nil, "", err
			}
		}
		if s.System {
			hasSystemUnit = true
			if uid == nil {
				if uid, err = systemUnitUID(unit); err != nil {
					return nil, "", err
				}
			}
		}
	}
	if uid == nil && !hasSystemUnit {
		if info, err := os.Stat(h); err == nil {
			if owner, ok := statUID(info); ok {
				uid = &owner
			}
		}
	}
	return uid, runtimeHome, nil
}

// Identity seam the plan builder reads each profile's uid and runtime home through
var migrateGatewayIdentity = gatewayIdentity

// Installed units of a profile, the detached wording without one
func guardServiceLabel(p *ProfileGateway) string {
	if len(p.Services) > 0 {
		return p.ServiceLabel()
	}
	return "no service manager (detached)"
}

func sameUnitSet(a, b []ServiceUnit) bool {
	set := func(units []ServiceUnit) map[ServiceUnit]bool {
		out := map[ServiceUnit]bool{}
		for _, u := range units {
			out[u] = true
		}
		return out
	}
	sa, sb := set(a), set(b)
	if len(sa) != len(sb) {
		return false
	}
	for u := range sa {
		if !sb[u] {
			return false
		}
	}
	return true
}

// Refusal for a secondary under another service manager or scope than the one the fleet converges on
func guardServiceDomain(plan *MigrationPlan, profile *ProfileGateway) string {
	if len(profile.Services) > 1 {
		return fmt.Sprintf("Profile '%s' has more than one installed service (%s): "+
			"an ambiguous service topology is not folded automatically.", profile.Name, profile.ServiceLabel())
	}
	target := plan.TargetServiceKind()
	reference := plan.Default().Services
	if len(reference) == 0 {
		reference = nil
		if target != nil {
			reference = []ServiceUnit{*target}
		}
	}
	if sameUnitSet(profile.Services, reference) {
		return ""
	}
	against := ""
	if len(plan.Default().Services) > 0 {
		against = "the default gateway runs under " + guardServiceLabel(plan.Default())
	} else {
		against = "the fleet converges on " + serviceUnitLabel(*target)
	}
	return fmt.Sprintf("Profile '%s' runs under %s while %s: a different service domain is not folded automatically.",
		profile.Name, guardServiceLabel(profile), against)
}

// Refusal for a secondary running as another uid than the default, or behind an unresolvable User
func guardUnixUser(plan *MigrationPlan, profile *ProfileGateway) string {
	defaultUID := plan.Default().UID
	if defaultUID == nil && plan.Default().HasSystemUnit() {
		return "The default gateway runs a system unit whose User= cannot be resolved on this host: " +
			"an unknown service principal is not folded into automatically."
	}
	if profile.UID == nil && profile.HasSystemUnit() {
		return fmt.Sprintf("Profile '%s' runs a system unit whose User= cannot be resolved on this host: "+
			"an unknown service principal is not folded automatically.", profile.Name)
	}
	if defaultUID == nil || profile.UID == nil || *profile.UID == *defaultUID {
		return ""
	}
	return fmt.Sprintf("Profile '%s' runs as uid %d while the default gateway runs as uid %d: "+
		"a UNIX privilege boundary is not folded automatically.", profile.Name, *profile.UID, *defaultUID)
}

// Refusal for a secondary whose live home sits outside the default profiles tree
func guardHomeTree(plan *MigrationPlan, profile *ProfileGateway) string {
	profilesRoot, _ := pypath.Realpath(filepath.Join(plan.DefaultHome, "profiles"), pypath.Lax)
	live := profile.RuntimeHome
	if live == "" {
		live = profile.Home
	}
	runtimeHome, _ := pypath.Realpath(live, pypath.Lax)
	if _, ok := pypath.PathRelativeTo(runtimeHome, profilesRoot); ok {
		return ""
	}
	return fmt.Sprintf("Profile '%s' runs with ICEBATH_HOME=%s, outside %s: the multiplexer would serve %s instead of the live home.",
		profile.Name, runtimeHome, profilesRoot, profile.Home)
}

// Every boundary a standalone secondary sits behind, each distinct finding once in order
func AutoMigrationBlockers(plan *MigrationPlan) []string {
	findings := []string{}
	seen := map[string]bool{}
	for _, profile := range plan.StandaloneSecondaries() {
		for _, guard := range []func(*MigrationPlan, *ProfileGateway) string{guardServiceDomain, guardUnixUser, guardHomeTree} {
			if finding := guard(plan, profile); finding != "" && !seen[finding] {
				seen[finding] = true
				findings = append(findings, finding)
			}
		}
	}
	return findings
}

// Default profile systemd unit the operator masked to dev null, empty when neither scope is masked
func MaskedDefaultUnit(ctx context.Context, defaultHome string) (string, error) {
	scoped, restore := HomeEnv(ctx, defaultHome)
	units := []string{}
	for _, system := range []bool{false, true} {
		unit, err := migrateSystemdUnitPath(scoped, system)
		if err != nil {
			restore()
			return "", err
		}
		units = append(units, unit)
	}
	restore()
	for _, unit := range units {
		info, err := os.Lstat(unit)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if resolved, err := pypath.Realpath(unit, pypath.Lax); err == nil && resolved == os.DevNull {
			return unit, nil
		}
	}
	return "", nil
}

// Whether the default profile's effective config sets gateway.auto_multiplex_migration false
func AutoMigrationOptedOut(ctx context.Context, defaultHome string) (bool, error) {
	scoped, restore := HomeEnv(ctx, defaultHome)
	cfg, err := config.LoadReadonly(scoped, home.Dir(scoped), nil)
	restore()
	if err != nil {
		return false, err
	}
	section, ok := cfg["gateway"].(map[string]any)
	if !ok {
		return false, nil
	}
	value, present := section["auto_multiplex_migration"]
	if !present || value == nil {
		return false, nil
	}
	return !provider.IsTruthyValue(value), nil
}
