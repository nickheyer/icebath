package tuiserver

import "context"

// Pins the launch model and its custom gateway into a new profile, false when the profile already chose one
func InheritLaunchModel(ctx context.Context, path string) (bool, error) {
	return inheritLaunchModel(ctx, path)
}
