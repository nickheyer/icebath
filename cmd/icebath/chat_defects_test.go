package main

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestExplicitZeroMaxTurnsIsKeptApartFromAnUnsetFlag(t *testing.T) {
	for _, row := range []struct {
		argv []string
		want *int
	}{
		{[]string{"chat", "--max-turns", "0"}, new(int)},
		{[]string{"chat", "--max-turns", "7"}, func() *int { v := 7; return &v }()},
		{[]string{"chat"}, nil},
	} {
		a, ok := lightChatArgs(row.argv)
		if !ok {
			t.Fatalf("%v did not parse", row.argv)
		}
		if (a.MaxTurns == nil) != (row.want == nil) || (a.MaxTurns != nil && *a.MaxTurns != *row.want) {
			t.Fatalf("%v: max turns %v want %v", row.argv, a.MaxTurns, row.want)
		}
	}
}

func TestWorkspaceSearchErrorResolvesNoSessionInsteadOfTheGlobalMostRecent(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Chdir(t.TempDir())
	writer, err := state.NewSessionDB(ctx, home.StatePath(home.Dir(ctx)), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.CreateSession(state.NewSession{ID: "local-cli", Source: "local"}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.AppendMessage(ctx, "local-cli", map[string]any{"role": "user", "content": "hi"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	err = writer.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET created_source='cli' WHERE id='local-cli'"); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='index' AND sql LIKE '%git_repo_root%'")
		if err != nil {
			return err
		}
		var indexes []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			indexes = append(indexes, name)
		}
		rows.Close()
		for _, name := range indexes {
			if _, err := conn.ExecContext(ctx, "DROP INDEX "+name); err != nil {
				return err
			}
		}
		_, err = conn.ExecContext(ctx, "ALTER TABLE sessions DROP COLUMN git_repo_root")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if got := resolveLastSession("cli"); got != "" {
		t.Fatalf("a failed workspace search resolved %q", got)
	}
}
