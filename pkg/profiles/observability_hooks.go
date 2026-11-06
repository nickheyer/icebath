package profiles

import (
	"context"

	"github.com/nickheyer/icebath/pkg/observability"
)

// Lets the install snapshot count profiles the way shared_metrics_snapshot reads list_profile_names
func init() {
	observability.ProfileNamesHook = func(context.Context) []string { return ListProfileNames() }
}
