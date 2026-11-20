package main

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestBareContinueFindsTheLastSessionTheCliCreated(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	writer, err := state.NewSessionDB(ctx, home.StatePath(home.Dir(ctx)), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range [][2]string{{"local-cli", "hi"}, {"local-tui", "later"}} {
		if err := writer.CreateSession(state.NewSession{ID: row[0], Source: "local"}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.AppendMessage(ctx, row[0], map[string]any{"role": "user", "content": row[1]}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	err = writer.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		for _, row := range [][2]string{{"cli", "local-cli"}, {"tui", "local-tui"}} {
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET created_source=? WHERE id=?", row[0], row[1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if got := resolveLastSession("cli"); got != "local-cli" {
		t.Fatalf("cli resolved %q", got)
	}
	if got := resolveLastSession("tui"); got != "local-tui" {
		t.Fatalf("tui resolved %q", got)
	}
}
