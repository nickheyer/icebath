package state

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func searchStore(t *testing.T, rows [][3]string) (*sessionSearch, *fakeSearchHost) {
	t.Helper()
	conn, path := schemaConn(t)
	mustExecConn(t, conn, "INSERT INTO sessions (id, source, started_at, model) VALUES ('s1', 'cli', 1.0, 'm')")
	for i, r := range rows {
		var tool any
		if r[2] != "" {
			tool = r[2]
		}
		if _, err := conn.ExecContext(context.Background(),
			"INSERT INTO messages (session_id, timestamp, role, content, tool_name) VALUES ('s1', ?, ?, ?, ?)",
			float64(i+1), r[0], r[1], tool); err != nil {
			t.Fatal(err)
		}
	}
	conn.Close()
	return searchFixture(t, path)
}

func snippets(rows []*pyjson.Object) string {
	var out []string
	for _, r := range rows {
		v, _ := r.Get("snippet")
		s, _ := v.(string)
		out = append(out, strings.ToLower(s))
	}
	return strings.Join(out, " ")
}

func searchRowsT(t *testing.T, s *sessionSearch, query string, mutate func(*SearchMessagesOptions)) []*pyjson.Object {
	t.Helper()
	o := NewSearchMessagesOptions()
	if mutate != nil {
		mutate(&o)
	}
	rows, err := s.SearchMessages(context.Background(), query, o)
	if err != nil {
		t.Fatalf("search %q: %v", query, err)
	}
	return rows
}

func TestParaphrasedQueryRecoversViaORRetry(t *testing.T) {
	s, _ := searchStore(t, [][3]string{
		{"user", "Sarah prefers the standup meeting scheduled early on Thursday mornings", ""},
		{"assistant", "Noted, standup moved to Thursday.", ""},
		{"user", "graphiti daemon looks healthy today", ""},
	})
	if exact := searchRowsT(t, s, "standup Thursday", nil); len(exact) == 0 || !strings.Contains(snippets(exact[:1]), "standup") {
		t.Fatal("exact hits are untouched")
	}
	if rows := searchRowsT(t, s, "when does Sarah like her standup scheduled", nil); !strings.Contains(snippets(rows), "standup") {
		t.Fatal("the paraphrase recovers through the OR retry")
	}
	if joined := snippets(searchRowsT(t, s, "sarah standup thursday daemon", nil)); !strings.Contains(joined, "standup") || !strings.Contains(joined, "daemon") {
		t.Fatalf("every partially matching row returns: %s", joined)
	}
	assistant := searchRowsT(t, s, "when does Sarah like her standup scheduled", func(o *SearchMessagesOptions) { o.RoleFilter = []string{"assistant"} })
	for _, r := range assistant {
		if role, _ := r.Get("role"); role != "assistant" {
			t.Fatal("the role filter applies to the retry")
		}
	}
	if len(assistant) == 0 {
		t.Fatal("the assistant row is recovered")
	}
	for _, r := range searchRowsT(t, s, "standup NOT Thursday", nil) {
		if v, _ := r.Get("snippet"); strings.Contains(strings.ToLower(v.(string)), "thursday") {
			t.Fatal("explicit NOT keeps its exclusion")
		}
	}
	if rows := searchRowsT(t, s, "zebra xylophone quantum", nil); len(rows) != 0 {
		t.Fatal("a genuine miss stays empty")
	}
	if rows := searchRowsT(t, s, "站会 周五", nil); len(rows) != 0 {
		t.Fatal("a CJK miss stays empty")
	}
}

func TestSearchRoutesFieldsToolsAndGap(t *testing.T) {
	ctx := context.Background()
	t.Setenv("ICEBATH_SEARCH_SLOW_MS", "0")
	logs := captureSlog(t)
	s, host := searchStore(t, [][3]string{
		{"user", "deploy the walrus service", ""},
		{"tool", "walrus tool output body", "terminal"},
		{"assistant", "修改youer服务端 done", ""},
		{"user", "广西壮族自治区 桂林", ""},
		{"user", "older walrus memory", ""},
	})
	rows := searchRowsT(t, s, "walrus", func(o *SearchMessagesOptions) { o.Fields = []string{"role", "id"}; o.Sort = "oldest" })
	if len(rows) != 3 || strings.Join(rows[0].Keys, ",") != "id,role" {
		t.Fatalf("projection in canonical order: %d %v", len(rows), rows[0].Keys)
	}
	if !strings.Contains(logs.String(), "slow session search: path=fts5") || !strings.Contains(logs.String(), "query='walrus'") {
		t.Fatalf("a zero threshold logs every search: %s", logs.String())
	}
	tool := searchRowsT(t, s, "body", func(o *SearchMessagesOptions) { o.RoleFilter = []string{"tool"} })
	if len(tool) != 1 {
		t.Fatalf("an explicit tool search scans canonical rows: %d", len(tool))
	}
	if rows := searchRowsT(t, s, "youer", nil); len(rows) != 1 {
		t.Fatalf("a Latin term inside a CJK run is found through trigram: %d", len(rows))
	}
	if rows := searchRowsT(t, s, "桂林", nil); len(rows) != 1 {
		t.Fatalf("a two character CJK token takes the LIKE route: %d", len(rows))
	}
	if rows := searchRowsT(t, s, "广西壮族", nil); len(rows) != 1 {
		t.Fatalf("a four character CJK token takes the trigram route: %d", len(rows))
	}
	mustExecConn(t, host.conn, "DELETE FROM messages_fts WHERE rowid = 5")
	mustExecConn(t, host.conn, "INSERT INTO state_meta (key, value) VALUES ('fts_rebuild_high_water', '5'), ('fts_rebuild_progress', '4')")
	if rows := searchRowsT(t, s, "walrus memory", nil); len(rows) != 1 {
		t.Fatalf("the unindexed gap is topped up by LIKE while the rebuild is pending: %d", len(rows))
	}
	if gap, err := s.searchUnindexedGap(ctx, "AND", 5, searchFilter{}); err != nil || len(gap) != 0 {
		t.Fatalf("operator only queries scan nothing: %v %v", gap, err)
	}
	mustExecConn(t, host.conn, "INSERT INTO state_meta (key, value) VALUES ('fts_stale', '1')")
	if rows := searchRowsT(t, s, "deploy", nil); len(rows) != 1 || !s.fts.ftsStale.Load() {
		t.Fatalf("stale FTS answers from canonical rows: %d", len(rows))
	}
	if rows := searchRowsT(t, s, "   ", nil); len(rows) != 0 {
		t.Fatal("a blank query returns nothing")
	}
	if _, err := s.SearchMessages(ctx, "x", SearchMessagesOptions{Fields: []string{"bogus"}}); err == nil {
		t.Fatal("unknown fields raise before searching")
	}
}
