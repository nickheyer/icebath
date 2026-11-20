package state

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestEachStorageCauseHasAStableCodeAndAnAction(t *testing.T) {
	cases := []struct {
		exc     any
		code    string
		command string
	}{
		{operationalError("database is locked"), "storage_locked", "try again"},
		{operationalError("attempt to write a readonly database"), "storage_readonly", "icebath doctor --fix"},
		{&SQLiteError{Class: SQLiteDatabaseError, Msg: "database disk image is malformed"}, "storage_corrupt", "icebath doctor --fix"},
		{syscall.ENOSPC, "disk_full", "Free some disk space"},
		{nil, "storage_unavailable", "icebath doctor --fix"},
	}
	for _, c := range cases {
		failure := DescribeStorageFailure(context.Background(), c.exc)
		if failure.Code != c.code || !strings.Contains(failure.Action, c.command) || failure.Gloss == "" {
			t.Errorf("%v: got %+v, want code %s with %q", c.exc, failure, c.code, c.command)
		}
		if strings.Contains(strings.ToLower(failure.Gloss), "sqlite") || strings.Contains(failure.Gloss, "OperationalError") {
			t.Errorf("%v: raw sqlite wording leaked into the gloss %q", c.exc, failure.Gloss)
		}
	}
}

func TestStorageFailureDetailsLineIsFlattenedAndBounded(t *testing.T) {
	details := StorageFailureDetails("line one\n   line two "+strings.Repeat("x", 400), 60)
	if strings.Contains(details, "\n") || len(details) != 60 || !strings.HasSuffix(details, "...") {
		t.Fatalf("details %q", details)
	}
	if got := StorageFailureDetails(nil, 200); got != "" {
		t.Fatalf("nil cause details %q", got)
	}
	if got := StorageFailureDetails("short  text", 200); got != "short text" {
		t.Fatalf("short details %q", got)
	}
}

func TestStorageFailureActionIsPinnedToTheFailingProfile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath", "profiles", "research"))
	selector := "-p research "
	for _, exc := range []any{operationalError("database is locked"), &SQLiteError{Class: SQLiteDatabaseError, Msg: "malformed"}, nil} {
		action := DescribeStorageFailure(context.Background(), exc).Action
		if strings.Contains(action, "{profile_arg}") || !strings.Contains(action, "`icebath "+selector) ||
			strings.Contains(action, "`icebath doctor") || strings.Contains(action, "`icebath gateway") {
			t.Errorf("%v: action %q not pinned to the research profile", exc, action)
		}
	}
}
