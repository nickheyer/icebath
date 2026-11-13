package pm

import (
	"context"
	"fmt"
	"strings"
)

// ConfiguredFeatureGap is one configured feature whose dependencies are missing, its label, install hint and candidate extra
type ConfiguredFeatureGap struct {
	Feature, Hint, Extra string
}

// Configured platforms whose adapter check fails, assigned by the gateway aware update code
var ConfiguredFeaturesMissingDeps func(ctx context.Context) []ConfiguredFeatureGap

// Adds the extras configured features need to the dependency environment, then lists the features still missing theirs
func InstallConfiguredFeaturesMissingDeps(ctx context.Context, projectRoot string) error {
	missing := ConfiguredFeaturesMissingDeps(ctx)
	if len(missing) == 0 {
		return nil
	}
	names, err := declaredExtras(projectRoot)
	if err != nil {
		return err
	}
	declined, err := declinedExtras(ctx, projectRoot)
	if err != nil {
		return err
	}
	declared := map[string]bool{}
	for _, name := range names {
		if !declined[name] {
			declared[name] = true
		}
	}
	chosen := map[string]bool{}
	for _, gap := range missing {
		extra := strings.ReplaceAll(gap.Extra, "_", "-")
		if !declared[extra] {
			continue
		}
		supported, err := extraSupported(ctx, extra, nil, nil)
		if err != nil {
			return err
		}
		if supported {
			chosen[extra] = true
		}
	}
	extras := sortedKeys(chosen)
	out := SyncOutput(ctx)
	if len(extras) > 0 {
		err := clientSyncVenv(ctx, SyncVenvOptions{Extras: extras, Explicit: true, ProjectRoot: projectRoot,
			EvictIncompatiblePlugins: true, Automatic: extras})
		if err != nil {
			fmt.Fprintf(out, "  ⚠ Could not install %s for configured features: %s\n", strings.Join(extras, ", "), pyExceptionText(err))
		} else {
			after, err := declinedExtras(ctx, projectRoot)
			if err != nil {
				return err
			}
			var still []ConfiguredFeatureGap
			for _, gap := range missing {
				extra := strings.ReplaceAll(gap.Extra, "_", "-")
				if !chosen[extra] || after[extra] {
					still = append(still, gap)
				}
			}
			missing = still
		}
	}
	if len(missing) > 0 {
		fmt.Fprintln(out, "  ⚠ Configured features whose dependencies are still missing — the gateway will fail to load them on restart:")
		for _, gap := range missing {
			fmt.Fprintf(out, "    - %s: %s\n", gap.Feature, gap.Hint)
		}
	}
	return nil
}
