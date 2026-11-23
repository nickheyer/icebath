//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"os"
)

// Cheap stamp comparisons of installed state against the lockfile, never installing or fetching
func Drift(ctx context.Context, includeVenv bool) (*DriftReport, error) {
	report := &DriftReport{Reasons: map[string]string{}}
	factsPath, err := FactsPath(ctx)
	if err != nil {
		return nil, err
	}
	if !isRegularFile(factsPath) && !isRegularFile(RuntimeFactsPath(ctx)) {
		return report, nil
	}
	lockfile := currentLockfile()
	facts, err := storeFacts(ctx)
	if err != nil {
		return nil, err
	}
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	toolsRecorded := isRegularFile(factsPath) || !binaryCoreTree(RepoRoot())
	for _, name := range lockfile.Names() {
		p, err := GetPackage(name)
		if err != nil || !toolsRecorded {
			continue
		}
		if p.Base().Optional || p.Base().Internal || p.MissingReason(target) != "" {
			continue
		}
		loc, err := InstalledLocation(ctx, p, lockfile, target, LocationOptions{})
		if err != nil {
			return nil, err
		}
		if loc == nil {
			report.set(name, "not installed or outdated")
		}
	}
	_, venvErr := GetPackage("venv")
	recordedVenv := facts.packages["venv"] != nil
	if includeVenv && venvErr == nil && (isRegularFile(RuntimeFactsPath(ctx)) || recordedVenv) {
		current, err := venvIsCurrent(ctx, nil, nil, "")
		switch {
		case err != nil && !caughtAsOSRuntimeValue(err):
			return nil, err
		case err != nil:
			report.set("venv", err.Error())
		case !current:
			report.set("venv", "out of sync with uv.lock")
		}
	}
	return report, nil
}

// Human readable startup diagnostics, one name and reason per problem
func Check(ctx context.Context, includeVenv bool) ([]string, error) {
	report, err := Drift(ctx, includeVenv)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(report.Names))
	for _, name := range report.Names {
		out = append(out, name+": "+report.Reasons[name])
	}
	return out, nil
}

// Store verdict activation reports, the engine check unless a caller counts it
var activationCheck = Check

// Puts the installed store's tool dirs at the front of the process PATH and returns the check problems
func Activate(ctx context.Context, allowIncomplete bool) ([]string, error) {
	problems, err := activationCheck(ctx, !allowIncomplete)
	if err != nil {
		return nil, err
	}
	if err := ActivatePath(ctx); err != nil {
		return nil, err
	}
	return problems, nil
}

// Puts the installed store's tool dirs at the front of the process PATH, checking nothing
func ActivatePath(ctx context.Context) error {
	path := os.Getenv("PATH")
	first, err := StoreFirstPath(ctx, path)
	if err != nil {
		return err
	}
	if first != path {
		return os.Setenv("PATH", first)
	}
	return nil
}

// Required tools whose installed copy this version's lockfile has moved past, and the venv when it is behind its lock, never installing or fetching
func Outdated(ctx context.Context) (*DriftReport, error) {
	drift, err := Drift(ctx, true)
	if err != nil {
		return nil, err
	}
	report := &DriftReport{Reasons: map[string]string{}}
	lockfile := currentLockfile()
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	for _, name := range drift.Names {
		if name == "venv" {
			report.set(name, drift.Reasons[name])
			continue
		}
		p, err := GetPackage(name)
		if err != nil {
			return nil, err
		}
		loc, err := InstalledLocation(ctx, p, lockfile, target, LocationOptions{AllowOutdated: true})
		if err != nil {
			return nil, err
		}
		if loc != nil {
			report.set(name, "installed at an older pin")
		}
	}
	return report, nil
}
