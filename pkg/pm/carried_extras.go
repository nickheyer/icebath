package pm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// CarriedFailure is why a carried extra keeps the environment from building and whether that is a build verdict
type CarriedFailure struct {
	Reason  string
	Verdict bool
}

// CarriedTrial is the sync a carried extra failed in, the selection inputs its trials rebuild from
type CarriedTrial struct {
	Extras   []string
	Carried  []string
	Fact     map[string]any
	Shipped  []string
	Frozen   []string
	Explicit bool
}

// Reports a resolver proof or a build failure, a verdict about the build rather than the moment
func isBuildVerdict(err error) bool {
	var conflict *ResolutionConflict
	var build *BuildFailure
	return errors.As(err, &conflict) || errors.As(err, &build)
}

// Carried extras keeping the environment from building with their reasons, the failure itself when they are not the cause
func failingCarried(ctx context.Context, p VenvState, failure error, trial CarriedTrial) (map[string]CarriedFailure, error) {
	var optional []string
	for _, extra := range trial.Carried {
		if !containsString(optional, extra) && containsString(trial.Extras, extra) {
			optional = append(optional, extra)
		}
	}
	if len(optional) == 0 {
		return nil, failure
	}
	var required []string
	for _, extra := range trial.Extras {
		if !containsString(optional, extra) {
			required = append(required, extra)
		}
	}
	base, _, _, err := selectTarget(ctx, p, trial.Fact, required, VenvApplyOptions{PluginDirs: []MemberDir{}}, false, trial.Shipped, trial.Frozen)
	if err != nil {
		return nil, err
	}
	verdict, err := evictionTrial(ctx, p, base, trial.Explicit, []MemberDir{})
	if err != nil {
		return nil, err
	}
	if verdict != nil {
		return nil, failure
	}
	var fitting []string
	failing := map[string]CarriedFailure{}
	for _, extra := range optional {
		verdict, err := evictionTrial(ctx, p, sortedUnion(base, fitting, []string{extra}), trial.Explicit, []MemberDir{})
		if err != nil {
			return nil, err
		}
		if verdict == nil {
			fitting = append(fitting, extra)
		} else {
			failing[extra] = *verdict
		}
	}
	if len(failing) == 0 {
		return nil, failure
	}
	return failing, nil
}

// Fails the sync without publishing when a carried extra failed for the moment rather than for good
func refuseTransient(failing map[string]CarriedFailure, failure error) error {
	var names, details, withouts []string
	for _, extra := range sortedKeys(failing) {
		if !failing[extra].Verdict {
			names = append(names, extra)
			details = append(details, failing[extra].Reason)
			withouts = append(withouts, "`icebath pm install --without "+extra+"`")
		}
	}
	if len(names) == 0 {
		return nil
	}
	return NewInstallError("venv", fmt.Sprintf("the extras %s icebath carried over from the old venv could not be prepared: %s",
		pyListRepr(names), pyPrefix(strings.Join(details, "; "), 600)),
		"fix the download or tooling problem, then rerun `icebath update`: it tries them again; or leave them out for good with "+strings.Join(withouts, " "))
}

const noBuildReason = "it no longer builds on this machine"

// Records the build verdicts so no later scan carries them, then warns the operator on stderr and the receipt
func recordLeftOut(ctx context.Context, p VenvState, failing map[string]CarriedFailure) error {
	names := sortedKeys(failing)
	reasons := map[string]string{}
	for _, extra := range names {
		reasons[extra] = noBuildReason
	}
	if _, err := recordDeclines(ctx, names, nil, reasons, p.ProjectRoot()); err != nil {
		return err
	}
	for _, extra := range names {
		message := fmt.Sprintf("Left the optional %s extra out of this update: icebath carried it over from the old venv, and %s "+
			"(the build output is above). Everything else is installed. Once that is fixed, run `%s` to add it back.",
			pyRepr(extra), noBuildReason, InstallHint(extra))
		fmt.Fprintf(os.Stderr, "⚠ %s\n", message)
		RecordWarning(ctx, message+" Detail: "+pyPrefix(failing[extra].Reason, 300))
	}
	return nil
}
