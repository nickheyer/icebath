package state

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestSanitizeFTS5QueryStripsDangerousChars(t *testing.T) {
	s := sanitizeFTS5Query
	cases := []struct{ in, want string }{
		{"hello world", "hello world"}, {"hello AND", "hello"}, {"OR world", "world"}, {"***", ""},
		{"deploy*", "deploy*"}, {"TODO: fix", "TODO  fix"}, {"my-app.config.ts", `"my-app.config.ts"`},
		{`"exact phrase" more`, `"exact phrase" more`}, {"chat-send P2.2", `"chat-send" "P2.2"`},
		{"ANDé foo", "ANDé foo"}, {"foo\u3000*bar", "foo\u3000bar"}, {"完成50%", "完成50%"},
	}
	for _, c := range cases {
		if got := s(c.in); got != c.want {
			t.Errorf("sanitize(%q) = %q want %q", c.in, got, c.want)
		}
	}
	for _, in := range []string{"C++", `"unterminated`, "(problem", "{test}", "error:timeout", "50%"} {
		if got := s(in); strings.ContainsAny(got, `+"({:%`) {
			t.Errorf("sanitize(%q) = %q keeps a special character", in, got)
		}
	}
	if got := strings.Fields(s("TODO: fix")); strings.Join(got, ",") != "TODO,fix" {
		t.Errorf("both terms survive: %v", got)
	}
	long := strings.Repeat(`"`, 50_000) + strings.Repeat(" bounded", 10_000)
	start := time.Now()
	if got := s(long); utf8Len(got) > MaxFTS5QueryChars || time.Since(start) > time.Second {
		t.Errorf("long query capped and bounded: %d runes in %v", utf8Len(got), time.Since(start))
	}
}

func utf8Len(s string) int { return len([]rune(s)) }

func TestSanitizedQueriesStayParsable(t *testing.T) {
	conn, err := ConnectSQLite(filepath.Join(t.TempDir(), "t.db"), SQLiteConfig{Timeout: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	mustExecConn(t, conn, "CREATE VIRTUAL TABLE t USING fts5(content)")
	mustExecConn(t, conn, "INSERT INTO t (content) VALUES ('meet me at user host about gateway run py it s 50 a b')")
	for _, q := range []string{"it's", "gateway/run.py", "user@host", "a,b", "why?", "e=mc2", "a;b", "a!b", "a&b",
		"a|b", "x~y", "#tag", "$dollar", "[bracket]", "<tag>", `C:\path\file`, "gate*", "50%"} {
		sanitized := sanitizeFTS5Query(q)
		if strings.TrimSpace(sanitized) == "" {
			continue
		}
		row, err := fetchOne(context.Background(), conn, "SELECT count(*) FROM t WHERE t MATCH ?", sanitized)
		if err != nil {
			t.Fatalf("%q sanitized to %q fails MATCH: %v", q, sanitized, err)
		}
		if q == "gate*" && row[0].(int64) != 1 {
			t.Fatal("a prefix star still matches")
		}
	}
}

func TestOrRelaxedQueryRewrite(t *testing.T) {
	cases := []struct{ in, want string }{
		{"sarah standup scheduled", "sarah OR standup OR scheduled"}, {"alpha AND beta", "alpha OR beta"},
		{`"docker networking" tls`, `"docker networking" OR tls`}, {"standup", ""}, {`"docker networking"`, ""},
		{"alpha OR beta", ""}, {"python NOT java", ""},
	}
	for _, c := range cases {
		got, ok := orRelaxedQuery(c.in)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("relax(%q) = %q %v want %q", c.in, got, ok, c.want)
		}
	}
}

func TestCJKRoutingPredicates(t *testing.T) {
	if !hasLoneCJKRun("가") || !hasLoneCJKRun("a 가 b") || hasLoneCJKRun("기존") || hasLoneCJKRun("abc") {
		t.Fatal("lone CJK runs")
	}
	if countCJK("广西 OR 桂林") != 4 || !queryContainsCJK("x桂") || queryContainsCJK("plain") {
		t.Fatal("CJK counting")
	}
	if !hasShortCJKToken("广西 OR 桂林 OR 漓江") || hasShortCJKToken("广西壮族 OR 桂林山水") {
		t.Fatal("short CJK tokens")
	}
	if !trigramEligibleTokens(`"docker networking"`) || trigramEligibleTokens("go lang") || trigramEligibleTokens(`""`) {
		t.Fatal("trigram eligibility")
	}
	fts := newSessionFTSSetup(&fakeFTSHost{path: "/x/state.db"})
	s := newSessionSearch(nil, fts)
	fts.trigramAvailable.Store(true)
	paths := map[string]string{"": "empty", "hello": "fts5", "가": "like_scan", "广西壮族": "trigram", "广西 OR 桂林": "like_scan"}
	for q, want := range paths {
		if got := s.describeSearchPath(q); got != want {
			t.Errorf("path(%q) = %q want %q", q, got, want)
		}
	}
	fts.ftsCJKAvailable.Store(true)
	if s.describeSearchPath("기존") != "fts_cjk" || s.describeSearchPath("가") != "like_scan" {
		t.Fatal("the bigram index serves runs of two or more")
	}
	fts.ftsStale.Store(true)
	if s.describeSearchPath("hello") != "like_scan_fts_stale" {
		t.Fatal("stale FTS routes to the LIKE scan")
	}
}

func TestCompileLikeBooleanQuery(t *testing.T) {
	predicate, params, snippet, ok := compileLikeBooleanQuery(`alpha NOT "beta*" OR AND gamma NEAR OR NOT delta`)
	want := fmt.Sprintf("(%s AND NOT %s) OR (%s)", likeCoalescedColumnSQL, likeCoalescedColumnSQL, likeCoalescedColumnSQL)
	if predicate != want || !ok || snippet != "alpha" {
		t.Fatalf("predicate %q snippet %q", predicate, snippet)
	}
	if len(params) != 3*len(likeParams("x")) || params[len(likeParams("x"))] != likeParams("beta")[0] {
		t.Fatalf("params %v", params)
	}
	if p, _, _, ok := compileLikeBooleanQuery("NOT alone"); p != "" || ok {
		t.Fatalf("a group with no positive term compiles to nothing: %q", p)
	}
}

func searchRowsStore(t *testing.T) (*sessionSearch, *fakeSearchHost) {
	t.Helper()
	conn, path := schemaConn(t)
	mustExecConn(t, conn, "INSERT INTO sessions (id, source, started_at) VALUES ('s1', 'cli', 1.0)")
	for i, c := range []string{"walrus tusk notes", "unrelated chatter", "the walrus sings", "closing words"} {
		if _, err := conn.ExecContext(context.Background(),
			"INSERT INTO messages (session_id, timestamp, role, content) VALUES ('s1', ?, ?, ?)",
			float64(i+1), []string{"user", "assistant"}[i%2], c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(context.Background(), "INSERT INTO messages (session_id, timestamp, role, content) "+
		"VALUES ('s1', 5.0, 'user', ?)", "\x00json:[{\"type\": \"text\", \"text\": \"structured walrus\"}]"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	return searchFixture(t, path)
}

func TestMatchRowsLikeFallbackAndFinalize(t *testing.T) {
	ctx := context.Background()
	s, host := searchRowsStore(t)
	rows, ok, err := s.matchRows(ctx, "messages_fts", "walrus", ftsOrderBy["oldest"], "", "", 10, 0, searchFilter{})
	if err != nil || !ok || len(rows) != 3 {
		t.Fatalf("match rows %v %v %v", len(rows), ok, err)
	}
	if snip, _ := rows[0].Get("snippet"); !strings.Contains(snip.(string), ">>>walrus<<<") {
		t.Fatalf("snippet %v", snip)
	}
	if _, ok, err := s.matchRows(ctx, "messages_fts", "walrus AND (", "", "", "syntax", 10, 0, searchFilter{}); ok || err != nil {
		t.Fatalf("an unparsable MATCH reads as None: %v %v", ok, err)
	}
	like, err := s.searchMessagesLikeFallback(ctx, "walrus NOT sings", 10, 0, " Oldest ", searchFilter{})
	if err != nil || len(like) != 1 {
		t.Fatalf("like fallback %d %v", len(like), err)
	}
	if first, _ := like[0].Get("id"); first.(int64) != 1 {
		t.Fatalf("oldest first: %v", first)
	}
	final := s.finalizeSearchMatches(ctx, rows, nil)
	ctxEntries, _ := final[0].Get("context")
	if len(ctxEntries.([]any)) == 0 {
		t.Fatal("context attached when every field is requested")
	}
	for _, m := range final {
		if _, has := m.Get("content"); has {
			t.Fatal("content is popped")
		}
	}
	last, _ := final[2].Get("context")
	found := false
	for _, e := range last.([]any) {
		if text, _ := e.(*pyjson.Object).Get("content"); text == "structured walrus" {
			found = true
		}
	}
	if !found {
		t.Fatalf("structured content is decoded and flattened: %v", last)
	}
	projected := s.finalizeSearchMatches(ctx, rows, []string{"id", "role"})
	if projected[0].Len() != 2 {
		t.Fatalf("projection keeps only requested fields: %v", projected[0].Keys)
	}
	if err := s.refreshFTSStaleState(ctx); err != nil || s.fts.ftsStale.Load() {
		t.Fatal("no breadcrumb keeps FTS live")
	}
	mustExecConn(t, host.conn, "INSERT INTO state_meta (key, value) VALUES ('fts_stale', '1')")
	if err := s.refreshFTSStaleState(ctx); err != nil || !s.fts.ftsStale.Load() || s.fts.ftsEnabled.Load() ||
		s.fts.trigramAvailable.Load() {
		t.Fatal("a foreign breadcrumb disables every index")
	}
}

func TestOptimizeAndRebuildFTS(t *testing.T) {
	ctx := context.Background()
	s, host := searchRowsStore(t)
	n, err := s.OptimizeFTS(ctx)
	if err != nil || n != 2 {
		t.Fatalf("both present indexes optimized: %d %v", n, err)
	}
	n, err = s.RebuildFTS(ctx)
	if err != nil || n != 2 {
		t.Fatalf("both present indexes rebuilt: %d %v", n, err)
	}
	if c := ftsMatchCount(t, host.reader, "messages_fts", "walrus"); c != 3 {
		t.Fatalf("rebuild keeps the index: %d", c)
	}
	host.corruptErr = &StateDBCorruptError{Msg: "quarantined"}
	if _, err := s.OptimizeFTS(ctx); err == nil {
		t.Fatal("a quarantined handle never optimizes")
	}
	if _, err := s.RebuildFTS(ctx); err == nil {
		t.Fatal("a quarantined handle never rebuilds")
	}
}
