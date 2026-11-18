package main

import (
	"context"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/state"
)

// Exit status of the hidden verb reporting this binary's linked SQLite, false when argv names another command
func runSQLiteRuntimeVerb(argv []string, stdout, stderr io.Writer) (int, bool) {
	if len(argv) == 0 || argv[0] != state.SQLiteRuntimeVerb {
		return 0, false
	}
	if len(argv) != 1 {
		fmt.Fprintln(stderr, "usage: icebath "+state.SQLiteRuntimeVerb)
		return 2, true
	}
	if err := state.WriteSQLiteRuntimeReport(context.Background(), stdout); err != nil {
		fmt.Fprintln(stderr, err)
		return 1, true
	}
	return 0, true
}
