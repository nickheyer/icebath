package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type fakeTitlesHost struct {
	*fakeSearchHost
	ioerrRetries int
}

func (h *fakeTitlesHost) writeRowcount(ctx context.Context, query string, args []any, patienceS *float64) (int64, error) {
	var n int64
	err := h.executeWrite(ctx, func(conn SQLiteConn) error {
		result, err := conn.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err = result.RowsAffected()
		return err
	}, patienceS)
	return n, err
}

func (h *fakeTitlesHost) readRetryingIOErr(ctx context.Context, fn func(conn SQLiteConn) error) error {
	h.ioerrRetries++
	return fn(h.reader)
}

func (h *fakeTitlesHost) sessionRowDict(row *pyjson.Object) *pyjson.Object {
	if resolved, ok := row.Get("_system_prompt_resolved"); ok {
		row.Delete("_system_prompt_resolved")
		row.Set("system_prompt", resolved)
	}
	return row
}

func titlesFixture(t *testing.T) (*sessionTitles, *fakeTitlesHost) {
	t.Helper()
	_, host := searchStore(t, nil)
	mustExecConn(t, host.conn, "DELETE FROM sessions")
	th := &fakeTitlesHost{fakeSearchHost: host}
	return newSessionTitles(th), th
}

func addTitledSession(t *testing.T, conn SQLiteConn, id string, startedAt float64, cols map[string]any) {
	t.Helper()
	names := []string{"id", "source", "started_at"}
	source, ok := cols["source"]
	if !ok {
		source = "cli"
	}
	args := []any{id, source, startedAt}
	keys := make([]string, 0, len(cols))
	for k := range cols {
		if k != "source" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		names = append(names, k)
		args = append(args, cols[k])
	}
	query := fmt.Sprintf("INSERT INTO sessions (%s) VALUES (%s)", strings.Join(names, ", "), strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", "))
	if _, err := conn.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func titleRow(t *testing.T, conn SQLiteConn, id string) (any, any) {
	t.Helper()
	row, err := fetchOne(context.Background(), conn, "SELECT title, title_source FROM sessions WHERE id = ?", id)
	if err != nil {
		t.Fatal(err)
	}
	return row[0], row[1]
}

func wantValueError(t *testing.T, err error, msg string) {
	t.Helper()
	var pe *pyint.Error
	if !errors.As(err, &pe) || pe.Kind != "ValueError" || pe.Msg != msg {
		t.Fatalf("want ValueError %q, got %v", msg, err)
	}
}

func TestSanitizeTitle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"  hello\tworld\n ", "hello world"},
		{"a\x00b\x7fc\x1fd", "abcd"},
		{"a\x0bb\x0cc", "abc"},
		{"a\u200bb\u202ec\ufeffd\u2060e\ufffcf\ufff9g", "abcdefg"},
		{"\u200b\u2028", ""},
		{"a\u0085b\u3000c\u00a0d", "a b c d"},
		{"a\r\n\r\nb", "a b"},
		{"\xed\xa0\x80x", "\ufffdx"},
		{"a\u0080b", "a\u0080b"},
		{strings.Repeat("\u00e9", 100), strings.Repeat("\u00e9", 100)},
	}
	for _, c := range cases {
		got, err := SanitizeTitle(c.in)
		if err != nil || got != c.want {
			t.Fatalf("SanitizeTitle(%q) = %q, %v want %q", c.in, got, err, c.want)
		}
	}
	_, err := SanitizeTitle(strings.Repeat("x", 101))
	wantValueError(t, err, "Title too long (101 chars, max 100)")
	if got, err := SanitizeTitle(" " + strings.Repeat("x", 100) + "\u200b "); err != nil || got != strings.Repeat("x", 100) {
		t.Fatalf("cap applies after cleaning: %q %v", got, err)
	}
}

func TestTitleRankOf(t *testing.T) {
	for source, want := range map[any]int{nil: 2, "derived": 0, "llm": 1, "user": 2, "bogus": 0, int64(2): 0} {
		if got := titleRankOf(source); got != want {
			t.Fatalf("rank %v = %d want %d", source, got, want)
		}
	}
}

func TestNextTitleInLineage(t *testing.T) {
	ctx := context.Background()
	titles, host := titlesFixture(t)
	next := func(base string) string {
		t.Helper()
		got, err := titles.GetNextTitleInLineage(ctx, base)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := next("my session"); got != "my session" {
		t.Fatalf("no rows keeps the base: %q", got)
	}
	addTitledSession(t, host.conn, "a", 1, map[string]any{"title": "my session"})
	if got := next("my session"); got != "my session #2" {
		t.Fatalf("original counts as one: %q", got)
	}
	addTitledSession(t, host.conn, "b", 2, map[string]any{"title": "my session #3"})
	addTitledSession(t, host.conn, "c", 3, map[string]any{"title": "my session #7"})
	if got := next("my session #3"); got != "my session #8" {
		t.Fatalf("numbered base strips its suffix: %q", got)
	}
	addTitledSession(t, host.conn, "d", 4, map[string]any{"title": "50% #2"})
	addTitledSession(t, host.conn, "e", 5, map[string]any{"title": "50x #9"})
	if got := next("50%"); got != "50% #3" {
		t.Fatalf("LIKE wildcards are escaped: %q", got)
	}
	addTitledSession(t, host.conn, "f", 6, map[string]any{"title": "arabic #٣"})
	if got := next("arabic"); got != "arabic #4" {
		t.Fatalf("unicode digits count as numbers: %q", got)
	}
	addTitledSession(t, host.conn, "g", 7, map[string]any{"title": "huge #99999999999999999999"})
	if got := next("huge"); got != "huge #100000000000000000000" {
		t.Fatalf("numbers are unbounded: %q", got)
	}
	if got, err := LineageTitleOnConn(ctx, host.conn, "huge"); err != nil || got != "huge #100000000000000000000" {
		t.Fatalf("conn variant %q %v", got, err)
	}
	if host.ioerrRetries == 0 {
		t.Fatal("lineage reads go through the IOERR retrying reader")
	}
}

func TestSetTitleProvenancePrecedence(t *testing.T) {
	ctx := context.Background()
	titles, host := titlesFixture(t)
	addTitledSession(t, host.conn, "s", 1, nil)
	steps := []struct {
		source, title string
		want          bool
		wantTitle     any
	}{
		{"derived", "first words", true, "first words"},
		{"derived", "other words", false, "first words"},
		{"llm", "Generated", true, "Generated"},
		{"llm", "Regenerated", false, "Generated"},
		{"derived", "Lower", false, "Generated"},
		{"user", "Mine", true, "Mine"},
		{"llm", "Later", false, "Mine"},
		{"user", "Mine again", true, "Mine again"},
	}
	for _, s := range steps {
		var got bool
		var err error
		if s.source == "user" {
			got, err = titles.SetSessionTitle(ctx, "s", s.title)
		} else {
			got, err = titles.SetAutoTitle(ctx, "s", s.title, s.source)
		}
		title, source := titleRow(t, host.conn, "s")
		if err != nil || got != s.want || title != s.wantTitle {
			t.Fatalf("%s %q: got %v %v title %v source %v", s.source, s.title, got, err, title, source)
		}
	}
	if got, err := titles.SetSessionTitle(ctx, "s", " \u200b "); err != nil || !got {
		t.Fatalf("blank clears: %v %v", got, err)
	}
	if title, source := titleRow(t, host.conn, "s"); title != nil || source != nil {
		t.Fatalf("cleared row %v %v", title, source)
	}
	if got, err := titles.SetSessionTitle(ctx, "missing", "x"); err != nil || got {
		t.Fatalf("missing session %v %v", got, err)
	}
	_, err := titles.SetAutoTitle(ctx, "s", "x", "user")
	wantValueError(t, err, "invalid automatic title source: 'user'")
	addTitledSession(t, host.conn, "legacy", 2, map[string]any{"title": "Old manual"})
	if got, err := titles.SetAutoTitle(ctx, "legacy", "Generated", "llm"); err != nil || got {
		t.Fatalf("NULL provenance ranks as user: %v %v", got, err)
	}
	addTitledSession(t, host.conn, "odd", 3, map[string]any{"title": "Odd", "title_source": "imported"})
	if got, err := titles.SetAutoTitle(ctx, "odd", "Derived", "derived"); err != nil || got {
		t.Fatalf("unknown provenance ranks zero, equal to derived: %v %v", got, err)
	}
	if got, err := titles.SetAutoTitle(ctx, "odd", "Generated", "llm"); err != nil || !got {
		t.Fatalf("llm outranks unknown: %v %v", got, err)
	}
	_, err = titles.SetSessionTitle(ctx, "s", strings.Repeat("y", 101))
	wantValueError(t, err, "Title too long (101 chars, max 100)")
}

// Connection that lands a concurrent manual rename right before the compare and swap UPDATE
type racingConn struct{ SQLiteConn }

func (c racingConn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.HasPrefix(query, "UPDATE sessions SET title = ?, title_source = ?") {
		if _, err := c.SQLiteConn.ExecContext(ctx, "UPDATE sessions SET title = 'Manual', title_source = 'user' WHERE id = ?", args[2]); err != nil {
			return nil, err
		}
	}
	return c.SQLiteConn.ExecContext(ctx, query, args...)
}

func TestSetTitleCompareAndSwapLosesToAConcurrentWrite(t *testing.T) {
	ctx := context.Background()
	titles, host := titlesFixture(t)
	addTitledSession(t, host.conn, "s", 1, nil)
	host.conn = racingConn{host.conn}
	got, err := titles.SetAutoTitle(ctx, "s", "Generated", "llm")
	if err != nil || got {
		t.Fatalf("CAS must lose: %v %v", got, err)
	}
	if title, source := titleRow(t, host.reader, "s"); title != "Manual" || source != "user" {
		t.Fatalf("concurrent manual title survives: %v %v", title, source)
	}
}

func TestTitleConflictResolution(t *testing.T) {
	ctx := context.Background()
	titles, host := titlesFixture(t)
	addTitledSession(t, host.conn, "a", 1, map[string]any{"title": "taken", "title_source": "user"})
	addTitledSession(t, host.conn, "b", 2, nil)
	_, err := titles.SetSessionTitle(ctx, "b", "taken")
	wantValueError(t, err, "Title 'taken' is already in use by session a")
	if got, err := titles.SetSessionTitle(ctx, "a", "taken"); err != nil || !got {
		t.Fatalf("own title is no conflict: %v %v", got, err)
	}

	addTitledSession(t, host.conn, "root", 3, map[string]any{"title": "chain", "title_source": "user", "end_reason": "compression", "ended_at": 4.0, "hidden": 1})
	addTitledSession(t, host.conn, "mid", 5, map[string]any{"parent_session_id": "root", "end_reason": "compression", "ended_at": 6.0})
	addTitledSession(t, host.conn, "tip", 7, map[string]any{"parent_session_id": "mid"})
	var affected []string
	err = host.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		affected, err = titles.setSessionTitleInTransaction(ctx, conn, "tip", "chain", TitleSourceUser)
		return err
	}, nil)
	if err != nil || !slices.Equal(affected, []string{"root", "tip"}) {
		t.Fatalf("ancestor title transfers to the tip: %v %v", affected, err)
	}
	if title, source := titleRow(t, host.conn, "root"); title != nil || source != "user" {
		t.Fatalf("ancestor keeps its provenance, loses the title: %v %v", title, source)
	}

	addTitledSession(t, host.conn, "fork-parent", 8, map[string]any{"title": "forked", "end_reason": "compression", "ended_at": 9.0, "hidden": 1})
	addTitledSession(t, host.conn, "delegate", 10, map[string]any{"parent_session_id": "fork-parent", "model_config": `{"_delegate_from": "fork-parent"}`})
	_, err = titles.SetSessionTitle(ctx, "delegate", "forked")
	wantValueError(t, err, "Title 'forked' is already in use by session fork-parent")
	addTitledSession(t, host.conn, "tool-child", 11, map[string]any{"parent_session_id": "fork-parent", "source": "tool"})
	_, err = titles.SetSessionTitle(ctx, "tool-child", "forked")
	wantValueError(t, err, "Title 'forked' is already in use by session fork-parent")

	addTitledSession(t, host.conn, "ghost", 12, map[string]any{"title": "abandoned", "title_source": "derived", "ended_at": 13.0})
	addTitledSession(t, host.conn, "fresh", 14, nil)
	if got, err := titles.SetSessionTitle(ctx, "fresh", "abandoned"); err != nil || !got {
		t.Fatalf("visible empty ended ghost yields: %v %v", got, err)
	}
	if title, source := titleRow(t, host.conn, "ghost"); title != nil || source != nil {
		t.Fatalf("ghost title and provenance cleared: %v %v", title, source)
	}
	addTitledSession(t, host.conn, "hidden-ghost", 15, map[string]any{"title": "kept", "ended_at": 16.0, "hidden": 1})
	_, err = titles.SetSessionTitle(ctx, "b", "kept")
	wantValueError(t, err, "Title 'kept' is already in use by session hidden-ghost")
	addTitledSession(t, host.conn, "busy", 17, map[string]any{"title": "busy title", "ended_at": 18.0, "message_count": 2})
	_, err = titles.SetSessionTitle(ctx, "b", "busy title")
	wantValueError(t, err, "Title 'busy title' is already in use by session busy")

	addTitledSession(t, host.conn, "old-bot", 19, map[string]any{"title": CanonicalBotChatTitle, "title_source": "user", "hidden": 1, "archived": 1})
	addTitledSession(t, host.conn, "new-bot", 20, map[string]any{"hidden": 1})
	if got, err := titles.SetSessionTitle(ctx, "new-bot", CanonicalBotChatTitle); err != nil || !got {
		t.Fatalf("archived hidden Bot Chat retires its name: %v %v", got, err)
	}
	if title, _ := titleRow(t, host.conn, "old-bot"); title != nil {
		t.Fatalf("retired Bot Chat %v", title)
	}
	addTitledSession(t, host.conn, "third-bot", 21, nil)
	_, err = titles.SetSessionTitle(ctx, "third-bot", CanonicalBotChatTitle)
	wantValueError(t, err, "Title 'Bot Chat' is already in use by session new-bot")
}

func TestCanonicalBotChatKeepsItsName(t *testing.T) {
	ctx := context.Background()
	titles, host := titlesFixture(t)
	addTitledSession(t, host.conn, "bot", 1, map[string]any{"title": CanonicalBotChatTitle, "title_source": "derived", "hidden": 1})
	_, err := titles.SetSessionTitle(ctx, "bot", "Renamed")
	wantValueError(t, err, "This is the bot's canonical Bot Chat — its name is its identity, and renaming it would orphan the conversation. To start fresh, create a new bot instead.")
	if got, err := titles.SetAutoTitle(ctx, "bot", "Generated", "llm"); err != nil || got {
		t.Fatalf("automatic rename is a silent no-op: %v %v", got, err)
	}
	if got, err := titles.SetSessionTitle(ctx, "bot", " Bot   Chat "); err != nil || !got {
		t.Fatalf("restating the canonical name is allowed: %v %v", got, err)
	}
	if title, source := titleRow(t, host.conn, "bot"); title != CanonicalBotChatTitle || source != "user" {
		t.Fatalf("canonical row %v %v", title, source)
	}
	addTitledSession(t, host.conn, "visible", 2, map[string]any{"title": "Bot Chat #2"})
	mustExecConn(t, host.conn, "UPDATE sessions SET title = NULL WHERE id = 'bot'")
	mustExecConn(t, host.conn, "UPDATE sessions SET title = 'Bot Chat' WHERE id = 'visible'")
	if got, err := titles.SetSessionTitle(ctx, "visible", "Renamed"); err != nil || !got {
		t.Fatalf("a visible Bot Chat stays renameable: %v %v", got, err)
	}
}

func TestTitleSourceAccessors(t *testing.T) {
	ctx := context.Background()
	titles, host := titlesFixture(t)
	addTitledSession(t, host.conn, "untitled", 1, map[string]any{"title_source": "llm"})
	addTitledSession(t, host.conn, "titled", 2, map[string]any{"title": "T", "title_source": "derived"})
	if _, ok, err := titles.GetSessionTitleSource(ctx, "untitled"); ok || err != nil {
		t.Fatalf("untitled has no provenance: %v %v", ok, err)
	}
	if source, ok, err := titles.GetSessionTitleSource(ctx, "titled"); !ok || err != nil || source != "derived" {
		t.Fatalf("titled provenance %q %v %v", source, ok, err)
	}
	if _, ok, err := titles.GetSessionTitle(ctx, "untitled"); ok || err != nil {
		t.Fatalf("untitled title %v %v", ok, err)
	}
	if title, ok, err := titles.GetSessionTitle(ctx, "titled"); !ok || err != nil || title != "T" {
		t.Fatalf("title %q %v %v", title, ok, err)
	}
	if _, ok, err := titles.GetSessionTitle(ctx, "missing"); ok || err != nil {
		t.Fatalf("missing title %v %v", ok, err)
	}
	_, err := titles.SetSessionTitleSource(ctx, "titled", "auto")
	wantValueError(t, err, "invalid title source: 'auto'")
	if got, err := titles.SetSessionTitleSource(ctx, "untitled", "user"); got || err != nil {
		t.Fatalf("untitled row untouched: %v %v", got, err)
	}
	if got, err := titles.SetSessionTitleSource(ctx, "titled", "user"); !got || err != nil {
		t.Fatalf("titled row rewritten: %v %v", got, err)
	}
	if title, source := titleRow(t, host.conn, "titled"); title != "T" || source != "user" {
		t.Fatalf("text kept, provenance moved: %v %v", title, source)
	}
}

func TestResolveSessionByTitle(t *testing.T) {
	ctx := context.Background()
	titles, host := titlesFixture(t)
	mustExecConn(t, host.conn, "INSERT INTO system_prompts (hash, prompt) VALUES ('h1', 'resolved prompt')")
	addTitledSession(t, host.conn, "exact", 1, map[string]any{"title": "work", "system_prompt_hash": "h1"})
	session, err := titles.GetSessionByTitle(ctx, "work")
	if err != nil || session == nil {
		t.Fatal(err)
	}
	if prompt, _ := session.Get("system_prompt"); prompt != "resolved prompt" {
		t.Fatalf("system prompt resolved through the host row dict: %v", prompt)
	}
	if missing, err := titles.GetSessionByTitle(ctx, "nothing"); missing != nil || err != nil {
		t.Fatalf("absent title %v %v", missing, err)
	}
	resolve := func(title string) (string, bool) {
		t.Helper()
		id, ok, err := titles.ResolveSessionByTitle(ctx, title)
		if err != nil {
			t.Fatal(err)
		}
		return id, ok
	}
	if id, ok := resolve("work"); !ok || id != "exact" {
		t.Fatalf("exact only %q %v", id, ok)
	}
	addTitledSession(t, host.conn, "n2", 3, map[string]any{"title": "work #2"})
	addTitledSession(t, host.conn, "n3", 2, map[string]any{"title": "work #3"})
	if id, _ := resolve("work"); id != "n2" {
		t.Fatalf("latest started numbered continuation wins: %q", id)
	}
	addTitledSession(t, host.conn, "only-numbered", 4, map[string]any{"title": "solo #2"})
	if id, _ := resolve("solo"); id != "only-numbered" {
		t.Fatalf("numbered without exact %q", id)
	}
	addTitledSession(t, host.conn, "bot", 5, map[string]any{"title": CanonicalBotChatTitle})
	addTitledSession(t, host.conn, "bot-branch", 6, map[string]any{"title": "Bot Chat #2"})
	if id, _ := resolve(CanonicalBotChatTitle); id != "bot" {
		t.Fatalf("canonical Bot Chat resolves exactly: %q", id)
	}
	addTitledSession(t, host.conn, "underscore", 7, map[string]any{"title": "axb #2"})
	if _, ok := resolve("a_b"); ok {
		t.Fatal("LIKE wildcards are escaped")
	}
	if _, ok := resolve("nothing"); ok {
		t.Fatal("unknown title resolves to None")
	}
}
