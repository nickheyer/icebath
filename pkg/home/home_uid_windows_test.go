package home

import "testing"

func TestResolveHermesUidGidWindowsReturnsNoneNone(t *testing.T) {
	t.Setenv("ICEBATH_UID", "1000")
	t.Setenv("ICEBATH_GID", "911")
	if uid, gid := resolveIcebathUIDGID(); uid != nil || gid != nil {
		t.Fatalf("uid %v gid %v", uid, gid)
	}
}
