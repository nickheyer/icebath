package tools

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

func riStore(t *testing.T, tmp, cwd string) string {
	t.Helper()
	db := filepath.Join(tmp, "state.db")
	conn, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, stmt := range []string{
		"CREATE TABLE sessions (id TEXT, cwd TEXT, message_count INTEGER)",
		"CREATE TABLE session_admissions (target_session_id TEXT, status TEXT)",
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.Exec("INSERT INTO sessions VALUES (?, ?, 3)", "s1", cwd); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestRetainedTreeUsesWindowsPathIdentity(t *testing.T) {
	cases := []struct {
		name  string
		spell func(p string) string
		live  bool
	}{
		{"same path, other casing", strings.ToUpper, true},
		{"nested, backslash separators", func(p string) string { return strings.ReplaceAll(strings.ToUpper(p), "/", `\`) + `\sub` }, true},
		{"sibling prefix is not inside", func(p string) string { return p + "-other" }, false},
		{"parent dir is not inside", filepath.Dir, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			tree := filepath.Join(tmp, "Tree")
			if err := os.Mkdir(tree, 0o755); err != nil {
				t.Fatal(err)
			}
			resolved, err := pypath.Resolve(tree)
			if err != nil {
				t.Fatal(err)
			}
			db := riStore(t, tmp, tc.spell(resolved))
			prev := insideTreeNormcase
			t.Cleanup(func() { insideTreeNormcase = prev })
			insideTreeNormcase = pypath.NtNormcase
			if got := retainedTreeIsLive(db, tree, false); got != tc.live {
				t.Fatalf("live %v want %v", got, tc.live)
			}
		})
	}
}
