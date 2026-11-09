package tools

import "testing"

func TestMaintainPackHealthOutsideARepositoryIsHarmless(t *testing.T) {
	MaintainPackHealth(t.TempDir())
}
