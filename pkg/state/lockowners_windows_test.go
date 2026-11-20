package state

import (
	"errors"
	"testing"
)

func TestWindowsHasNoMakedevAndNamesNoWriteLockHolders(t *testing.T) {
	_, err := ParseProcLocks("1: POSIX ADVISORY WRITE 1 zz:02:3 120 120\n", map[DevIno]string{})
	var conv *pyConvError
	if !errors.As(err, &conv) || conv.Class != "AttributeError" {
		t.Fatalf("parse on windows %v", err)
	}
	lines, err := StateDBWriteLockHolders(t.TempDir() + `\state.db`)
	if err != nil || len(lines) != 0 {
		t.Fatalf("holders on windows %v %v", lines, err)
	}
}
