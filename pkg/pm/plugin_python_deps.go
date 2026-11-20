//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"strings"
)

// Whether the environment marker of a requirement admits this machine, true for one that cannot be read
func managedRequirementAppliesHere(ctx context.Context, req string) bool {
	if !strings.Contains(req, ";") {
		return true
	}
	parsed, err := ParseRequirement(req)
	if err != nil || len(parsed.Marker) == 0 {
		return true
	}
	environment := markerEnvironment()
	var defaults map[string]string
	if markerNeedsDefaults(parsed.Marker, environment) {
		if defaults, err = loaderMarkerEnvironment(ctx); err != nil {
			return true
		}
	}
	holds, err := evaluateMarker(parsed.Marker, defaults, environment)
	return err != nil || holds
}
