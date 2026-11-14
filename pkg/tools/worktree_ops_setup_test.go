package tools

import (
	"context"
	"testing"
)

func TestMaintainPackHealthOutsideARepositoryIsHarmless(t *testing.T) {
	MaintainPackHealth(context.Background(), t.TempDir())
}
