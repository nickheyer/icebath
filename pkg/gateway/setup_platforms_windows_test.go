package gateway

import (
	"context"
	"testing"
)

func TestMatrixHiddenOnWindowsMatrixAbsentOnWindows(t *testing.T) {
	ctx := context.Background()
	entry := NewPlatformEntry("matrix", "Matrix", nil, func() bool { return false })
	if err := Registry.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Registry.Unregister(ctx, "matrix", "") })
	for _, p := range AllPlatforms(ctx) {
		if p.Key == "matrix" {
			t.Fatal("matrix must be hidden on Windows")
		}
	}
}
