package provider

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Binary of an installed managed package, false when it is not installed or PM cannot answer
func pmInstalledBinary(name string) (string, bool) {
	selected, err := pm.Installed(context.Background(), name, false)
	if err != nil || selected == nil {
		return "", false
	}
	return selected.Binary, true
}

// Ensures a managed package, explicit installs re-verifying a warm entry, and returns its binary
func pmEnsure(name string, explicit bool) (string, error) {
	ctx := context.Background()
	if _, err := pm.Ensure(ctx, name, pm.EnsureOptions{Explicit: explicit}); err != nil {
		return "", err
	}
	selected, err := pm.Installed(ctx, name, false)
	if err != nil {
		return "", err
	}
	if selected == nil {
		return "", errors.New("'NoneType' object has no attribute 'binary'")
	}
	return selected.Binary, nil
}
