package app

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var repairProfilesKindLabels = map[string]string{
	"mislabelled":             "profile_name disagrees with the row's own session key",
	"wrong_store":             "rows in another profile's store",
	"legacy_main":             "legacy agent:main rows inside a named profile's store",
	"unclaimed_namespace":     "rows keyed to a profile that does not exist",
	"crossed_parent":          "parent_session_id crossing profile namespaces",
	"routing_stray":           "routing rows outside the default store",
	"routing_unclaimed":       "routing rows for a profile that does not exist",
	"topic_profile_less":      "Telegram topic bindings without their bot's profile",
	"voice_profile_less":      "voice-mode entries without their bot's profile",
	"sessions_json_unclaimed": "sessions.json entries for a profile that does not exist",
}

// Findings grouped by kind, kinds in the order they were first found
func groupFindings(findings []*repairProfilesFinding) ([]string, map[string][]*repairProfilesFinding) {
	var kinds []string
	grouped := map[string][]*repairProfilesFinding{}
	for _, finding := range findings {
		if _, seen := grouped[finding.kind]; !seen {
			kinds = append(kinds, finding.kind)
		}
		grouped[finding.kind] = append(grouped[finding.kind], finding)
	}
	return kinds, grouped
}

// Prints every finding under the label of its kind with the repair it gets or the reason it gets none
func printReport(r *sessionsRun, findings []*repairProfilesFinding) {
	kinds, grouped := groupFindings(findings)
	for _, kind := range kinds {
		label, ok := repairProfilesKindLabels[kind]
		if !ok {
			label = kind
		}
		rows := grouped[kind]
		r.printf("\n%s (%d):", label, len(rows))
		for _, finding := range rows {
			r.printf("  [%s] %s\n      %s", finding.store, finding.subject, finding.detail)
			if finding.action != nil && *finding.action != "" {
				r.printf("      → %s", *finding.action)
			} else {
				reason := "None"
				if finding.reason != nil {
					reason = *finding.reason
				}
				r.printf("      ✗ not repaired — %s", reason)
			}
		}
	}
}

// JSON list of the findings as their dicts
func findingDicts(findings []*repairProfilesFinding) []any {
	out := make([]any, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.asDict())
	}
	return out
}

// Scans every profile store for crossed profile state and repairs it on apply after a snapshot
func cmdRepairProfiles(r *sessionsRun) (int, error) {
	return repairProfilesCommand(r, liveGatewayHomes, defaultSnapshot)
}

// Repair profiles over the given live gateway reader and store snapshotter
func repairProfilesCommand(r *sessionsRun, liveHomes func(context.Context, []repairProfilesStore) ([]liveGatewayHome, error),
	snapshot func(context.Context, repairProfilesStore) (string, bool)) (int, error) {
	stores, err := enumerateStores(r.ctx)
	if err != nil {
		return 1, err
	}
	apply, asJSON := nsTruthy(r.ns, "apply"), nsTruthy(r.ns, "json")
	legacyMain := nsText(r.ns, "legacy_main")
	if legacyMain == "" {
		legacyMain = "report"
	}
	profiles := make([]string, 0, len(stores))
	for _, s := range stores {
		profiles = append(profiles, s.profile)
	}
	if apply {
		live, err := liveHomes(r.ctx, stores)
		if err != nil {
			return 1, err
		}
		if len(live) > 0 {
			names := make([]string, 0, len(live))
			for _, l := range live {
				names = append(names, fmt.Sprintf("%s (pid %d)", l.profile, l.pid))
			}
			fmt.Fprintln(stderrWriter(), "A gateway is running for: "+strings.Join(names, ", ")+". It holds the routing index in memory and would "+
				"write it back over this repair. Stop it (`icebath gateway stop`), then re-run --apply.")
			return 1, nil
		}
	}
	plan, session, err := scanStores(r.ctx, stores, legacyMain, !apply)
	if err != nil {
		return 1, err
	}
	dump := func(v any) error {
		text, err := pyjson.Dumps(v, pyjson.Indent(2))
		if err == nil {
			r.print(text)
		}
		return err
	}
	findings := plan.findings
	result, code, done, err := func() (*pyjson.Object, int, bool, error) {
		defer session.close()
		repairable := plan.repairable()
		if asJSON && !apply {
			storeNames := make([]any, 0, len(profiles))
			for _, p := range profiles {
				storeNames = append(storeNames, p)
			}
			return nil, 0, true, dump(pyjson.ObjectOf("stores", storeNames, "findings", findingDicts(findings), "repairable", len(repairable)))
		}
		if !asJSON {
			r.printf("Scanned %d profile store(s): %s", len(stores), strings.Join(profiles, ", "))
			if len(findings) == 0 {
				r.print("✓ No crossed-profile state found.")
				return nil, 0, true, nil
			}
			printReport(r, findings)
		}
		if !apply {
			r.printf("\n%d of %d finding(s) can be repaired. "+
				"Re-run with --apply to perform them.", len(repairable), len(findings))
			return nil, 0, true, nil
		}
		if len(repairable) == 0 {
			if asJSON {
				return nil, 0, true, dump(pyjson.ObjectOf("findings", findingDicts(findings), "applied", pyjson.NewObject()))
			}
			r.print("\nNothing to repair.")
			return nil, 0, true, nil
		}
		if !nsTruthy(r.ns, "yes") && !asJSON {
			code, refused, err := confirmOrExitCode(r, fmt.Sprintf("\nApply %d repair(s)? A snapshot of every store is taken first. [y/N] ", len(repairable)),
				"Aborted — nothing was changed.", "--yes")
			if err != nil || refused {
				return nil, code, true, err
			}
		}
		result, err := plan.apply(r.ctx, session, snapshot)
		return result, 0, false, err
	}()
	if err != nil {
		return 1, err
	}
	if done {
		return code, nil
	}
	failures, _ := rowGet(result, "failures").([]any)
	if asJSON {
		if err := dump(pyjson.ObjectOf("findings", findingDicts(findings), "applied", result)); err != nil {
			return 1, err
		}
	} else {
		if snapshots, ok := rowGet(result, "snapshots").(*pyjson.Object); ok {
			for _, profile := range snapshots.Keys {
				snap := "FAILED"
				if v := snapshots.Vals[profile]; pyrepr.Truthy(v) {
					snap = pyrepr.Str(v)
				}
				r.printf("  snapshot [%s]: %s", profile, snap)
			}
		}
		var applied []string
		if totals, ok := rowGet(result, "totals").(*pyjson.Object); ok {
			keys := slices.Clone(totals.Keys)
			slices.Sort(keys)
			for _, k := range keys {
				if v := totals.Vals[k]; pyrepr.Truthy(v) {
					applied = append(applied, k+"="+pyrepr.Str(v))
				}
			}
		}
		appliedText := strings.Join(applied, ", ")
		if appliedText == "" {
			appliedText = "nothing"
		}
		r.printf("\nApplied: %s", appliedText)
		for _, item := range failures {
			failure, _ := item.(*pyjson.Object)
			r.printf("  ✗ %s %s: %s", pyrepr.Str(rowGet(failure, "kind")), pyrepr.Str(rowGet(failure, "subject")), pyrepr.Str(rowGet(failure, "error")))
		}
		r.print("Re-run without --apply to confirm the stores are clean.")
	}
	if len(failures) > 0 {
		return 1, nil
	}
	return 0, nil
}
