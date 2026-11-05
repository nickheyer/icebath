package state

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// Reports a SQLite version WAL is safe on, the reset bug predicate duplicated so the gate never touches the home
func walIsUsable(info VersionTuple) bool {
	switch {
	case info.Cmp(NewVersionTuple(3, 7, 0)) < 0:
		return true
	case info.Cmp(NewVersionTuple(3, 51, 3)) >= 0:
		return true
	case info.Cmp(NewVersionTuple(3, 50, 7)) >= 0 && info.Cmp(NewVersionTuple(3, 51, 0)) < 0:
		return true
	case info.Cmp(NewVersionTuple(3, 44, 6)) >= 0 && info.Cmp(NewVersionTuple(3, 45, 0)) < 0:
		return true
	}
	return false
}

func TestConftestGateAgreesWithState(t *testing.T) {
	versions := [][3]int64{
		{3, 6, 23}, {3, 7, 0}, {3, 44, 5}, {3, 44, 6}, {3, 45, 0}, {3, 50, 4},
		{3, 50, 6}, {3, 50, 7}, {3, 51, 2}, {3, 51, 3}, {3, 53, 1},
	}
	for _, v := range versions {
		info := NewVersionTuple(v[0], v[1], v[2])
		if walIsUsable(info) == WALResetVulnerable(&info) {
			t.Errorf("gate and state disagree for SQLite %v", v)
		}
	}
	linked := LinkedSQLiteVersionInfo()
	if walIsUsable(linked) == WALResetVulnerable(nil) {
		t.Errorf("gate and state disagree for the linked SQLite %s", LinkedSQLiteVersion())
	}
}

func TestConftestGateDoesNotResolveTheHome(t *testing.T) {
	t.Setenv("ICEBATH_HOME", "")
	t.Setenv("HOME", "")
	pinVar(t, &resolveJournalMode, func(context.Context) string {
		t.Fatal("the gate resolved the configured journal mode")
		return ""
	})
	info := LinkedSQLiteVersionInfo()
	if walIsUsable(info) == WALResetVulnerable(&info) {
		t.Fatal("gate disagrees with state without a home")
	}
}

func TestLinkedSQLiteVersionMatchesTheLibrary(t *testing.T) {
	conn := openWALTestConn(t, ":memory:")
	row, err := fetchOne(context.Background(), conn, "SELECT sqlite_version(), sqlite_source_id()")
	if err != nil {
		t.Fatal(err)
	}
	if got := pyStr(row[0]); got != LinkedSQLiteVersion() {
		t.Fatalf("sqlite_version() = %q, LinkedSQLiteVersion = %q", got, LinkedSQLiteVersion())
	}
	if got := pyStr(row[1]); got != SQLiteSourceID() || got == "" {
		t.Fatalf("sqlite_source_id() = %q, SQLiteSourceID = %q", got, SQLiteSourceID())
	}
	var parts []int64
	for _, p := range strings.Split(pyStr(row[0]), ".") {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, n)
	}
	if LinkedSQLiteVersionInfo().Cmp(NewVersionTuple(parts...)) != 0 {
		t.Fatalf("version info = %v", LinkedSQLiteVersionInfo())
	}
	if WALResetVulnerable(nil) != IsSQLiteWALResetVulnerable(LinkedSQLiteVersionInfo()) {
		t.Fatal("linked verdict differs from the predicate")
	}
}
