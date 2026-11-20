package state

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Fresh SQLite file carrying the declared schema, deferred objects and both FTS layouts
func declaredSchemaDB(t *testing.T, legacyFTS bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	scripts := []string{SchemaSQL, DeferredIndexSQL, FTSSQL, FTSTrigramSQL}
	if legacyFTS {
		scripts = []string{SchemaSQL, DeferredIndexSQL, LegacyFTSSQL, LegacyFTSTrigramSQL}
	}
	for _, script := range scripts {
		if _, err := db.Exec(script); err != nil {
			t.Fatalf("schema script failed: %v", err)
		}
	}
	return db
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func TestDeclaredSchemaScriptsRunAndIndex(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		db := declaredSchemaDB(t, legacy)
		mustExec(t, db, "INSERT INTO sessions (id, source, started_at) VALUES ('s1', 'cli', 1)")
		mustExec(t, db, "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1', 'user', 'ripe banana bread', 2)")
		mustExec(t, db, "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('s1', 'tool', ?, 3)", strings.Repeat("x", FTSToolContentPrefixChars)+" tailword")
		var hits int
		if err := db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'banana'").Scan(&hits); err != nil || hits != 1 {
			t.Fatalf("legacy=%v word index hits %d err %v", legacy, hits, err)
		}
		if err := db.QueryRow("SELECT COUNT(*) FROM messages_fts_trigram WHERE messages_fts_trigram MATCH 'nan'").Scan(&hits); err != nil || hits != 1 {
			t.Fatalf("legacy=%v trigram hits %d err %v", legacy, hits, err)
		}
		if !legacy {
			if err := db.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'tailword'").Scan(&hits); err != nil || hits != 0 {
				t.Fatalf("tool row past the prefix must stay out of the word index, hits %d err %v", hits, err)
			}
		}
	}
	if !strings.Contains(SchemaSQL, "WHERE principal_id = '"+CronPrincipalID+"';") {
		t.Fatal("cron principal literal missing from the admissions index")
	}
	if FTSTrigramSessionSQL != `source NOT IN ('cron', 'subagent') AND json_extract((CASE WHEN json_valid(model_config) THEN model_config ELSE json_object() END), '$._delegate_from') IS NULL` {
		t.Fatalf("trigram predicate %q", FTSTrigramSessionSQL)
	}
}

func TestPreviewSelectsFirstEligibleUserMessage(t *testing.T) {
	db := declaredSchemaDB(t, false)
	longForm, _, _ := strings.Cut(SummaryPrefix, "Do NOT answer")
	skillTurn := skillInvocationPrefix + `"work" skill. ` + singleSkillMarker + "\n\n" + strings.Repeat("body ", 300) +
		"\n\n" + singleSkillInstruction + "fix the title leak\n\n[Runtime note: x]"
	cases := []struct {
		name string
		rows []string
		kind []string
		want string
	}{
		{"plain", []string{"hello\nworld"}, nil, "hello world"},
		{"standalone summary skipped", []string{SummaryPrefix + " old work", "real question"}, nil, "real question"},
		{"legacy summary skipped", []string{"  " + LegacySummaryPrefix + " old", "next"}, nil, "next"},
		{"bare label is not a carrier", []string{"[CONTEXT COMPACTION — REFERENCE ONLY] mine"}, nil, "[CONTEXT COMPACTION — REFERENCE ONLY] mine"},
		{"force user remainder", []string{longForm + "summary\n" + SummaryEndMarker + "\n do the thing "}, nil, "do the thing"},
		{"force user without remainder", []string{longForm + "summary\n" + SummaryEndMarker + "  \n", "after"}, nil, "after"},
		{"merged prior", []string{MergedPriorContextHeader + "\n prior text \n" + MergedSummaryDelimiter + "\n" + SummaryPrefix}, nil, "prior text"},
		{"merged empty prior", []string{MergedPriorContextHeader + "\n\n" + MergedSummaryDelimiter + "\n" + LegacySummaryPrefix, "later"}, nil, "later"},
		{"hidden skipped", []string{"scaffolding", "visible"}, []string{"hidden", ""}, "visible"},
		{"skill excerpt", []string{skillTurn}, nil, "/work — fix the title leak [Runtime note: x]"},
		{"long head", []string{strings.Repeat("abcdefghij", 10)}, nil, strings.Repeat("abcdefghij", 6) + "..."},
	}
	for i, tc := range cases {
		sid := "p" + string(rune('a'+i))
		mustExec(t, db, "INSERT INTO sessions (id, source, started_at) VALUES (?, 'cli', 1)", sid)
		for j, content := range tc.rows {
			kind := any(nil)
			if j < len(tc.kind) && tc.kind[j] != "" {
				kind = tc.kind[j]
			}
			mustExec(t, db, "INSERT INTO messages (session_id, role, content, timestamp, display_kind) VALUES (?, 'user', ?, ?, ?)", sid, content, j+1, kind)
		}
		var raw any
		if err := db.QueryRow("SELECT "+sqlPreviewRaw("s.id")+" FROM sessions s WHERE s.id = ?", sid).Scan(&raw); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := shapePreview(raw); got != tc.want {
			t.Fatalf("%s: preview %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestLineagePredicatesClassifyChildren(t *testing.T) {
	db := declaredSchemaDB(t, false)
	rows := [][]any{
		{"root", nil, 1.0, nil, nil, nil, nil},
		{"bp", nil, 1.0, 5.0, "branched", nil, nil},
		{"bc", "bp", 6.0, nil, nil, nil, nil},
		{"cp", nil, 1.0, 5.0, "compression", nil, nil},
		{"cc", "cp", 6.0, nil, nil, nil, nil},
		{"rp", nil, 1.0, 5.0, "idle", "k1", nil},
		{"rc", "rp", 6.0, nil, nil, "k1", nil},
		{"mc", "root", 6.0, nil, nil, nil, `{"_reset_from": "root"}`},
		{"sub", "root", 6.0, nil, nil, nil, nil},
	}
	for _, r := range rows {
		mustExec(t, db, "INSERT INTO sessions (id, parent_session_id, started_at, ended_at, end_reason, session_key, model_config, source) VALUES (?, ?, ?, ?, ?, ?, ?, 'cli')", r...)
	}
	ids := func(where string, args ...any) []string {
		out := []string{}
		r, err := db.Query("SELECT s.id FROM sessions s WHERE "+where+" ORDER BY s.id", args...)
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		defer r.Close()
		for r.Next() {
			var id string
			r.Scan(&id)
			out = append(out, id)
		}
		return out
	}
	if got := ids(listableChildSQL); !reflect.DeepEqual(got, []string{"bc", "bp", "cp", "mc", "rc", "root", "rp"}) {
		t.Fatalf("listable %v", got)
	}
	if got := ids(ephemeralChildSQL("s")); !reflect.DeepEqual(got, []string{"sub"}) {
		t.Fatalf("ephemeral %v", got)
	}
	if got := ids("s.parent_session_id = ?\n"+nonContinuationChildSQL("s.", "?"), "root", "root", "root", "root"); !reflect.DeepEqual(got, []string{"sub"}) {
		t.Fatalf("continuations %v", got)
	}
	if !strings.Contains(nonContinuationChildSQL("", "?"), "COALESCE(source, '') != 'tool'") {
		t.Fatal("tool sessions must be rejected")
	}
}

func TestSessionLastActiveSkipsOutOfWindowCells(t *testing.T) {
	db := declaredSchemaDB(t, false)
	mustExec(t, db, "INSERT INTO sessions (id, source, started_at, last_activity_at) VALUES ('a', 'cli', 100, 5e246)")
	mustExec(t, db, "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('a', 'user', 'x', 200)")
	mustExec(t, db, "INSERT INTO sessions (id, source, started_at, last_activity_at) VALUES ('b', 'cli', 'garbage', 'junk')")
	mustExec(t, db, "INSERT INTO sessions (id, source, started_at, last_activity_at) VALUES ('c', 'cli', 50, 300)")
	for id, want := range map[string]any{"a": 200.0, "b": nil, "c": 300.0} {
		var byAlias, byID any
		if err := db.QueryRow("SELECT "+sqlSessionLastActive("s")+", "+sqlSessionLastActiveByID("s.id")+" FROM sessions s WHERE s.id = ?", id).Scan(&byAlias, &byID); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(byAlias, want) || !reflect.DeepEqual(byID, want) {
			t.Fatalf("%s: %v %v want %v", id, byAlias, byID, want)
		}
	}
	if sqlInWindowClause != "BETWEEN 0.0 AND 4200000000.0" {
		t.Fatalf("window %q", sqlInWindowClause)
	}
}

func TestLaunchStubFilterFollowsAdmissionsTable(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "state.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if got, err := d.launchStubFilterSQL(); err != nil || got != notALaunchStubSQL {
		t.Fatalf("with admissions %q %v", got, err)
	}
	if err := d.write(func(tx *Tx) error {
		_, err := tx.Exec("DROP TABLE session_admissions")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := d.launchStubFilterSQL(); err != nil || got != "s.message_count > 0" {
		t.Fatalf("without admissions %q %v", got, err)
	}
}

func TestSkillInvocationDescriptions(t *testing.T) {
	single := skillInvocationPrefix + `"work" skill. ` + singleSkillMarker + "\n\nBODY quoting " + singleSkillInstruction + "not this\n\n"
	dirNote := "Resolve any relative paths in this skill (e.g. `scripts/foo.js`, `templates/config.yaml`) against that directory, " + skillDirNoteEnd
	autoLoad := autoLoadPrefix + `deploy" skill is auto-loaded. Follow its instructions for this session.]` + "\nBODY\n" + dirNote
	cases := []struct {
		content, want string
		ok            bool
	}{
		{single + singleSkillInstruction + "fix  the\ttitle leak\n\n[Runtime note: ignore]", "/work — fix the title leak", true},
		{single + singleSkillInstruction + "fix the" + SkillExcerptJoint + "tail end", "/work — fix the", true},
		{skillInvocationPrefix + `"work" skill. ` + singleSkillMarker + "\nBODY", "/work", true},
		{skillInvocationPrefix + `"/clean /work" skill bundle, two skills.` + bundleUserInstruction + "tidy up" + bundleFirstSkillBlock + "clean" + bundleUserInstruction + "no", "/clean /work — tidy up", true},
		{skillInvocationPrefix + `"" skill. ` + singleSkillMarker + "\n" + singleSkillInstruction + "orphan text", "orphan text", true},
		{skillInvocationPrefix + "nothing quoted", "", false},
		{autoLoad + "\n\n[Skill config (\nkey: v\n]\n\n[Skill setup note: skipped]\n\n  ship   it\nnow ", "ship it now", true},
		{autoLoad + "\n\n[This skill has supporting files under it (e.g. `/scripts/foo.js`).", "/deploy", true},
		{autoLoadPrefix + `deploy" skill is auto-loaded. Follow its instructions for this session.]` + "\nno footer", "/deploy", true},
		{"[IMPORTANT: The \"config\" file is broken]", "/config", true},
		{"plain text", "", false},
	}
	for _, tc := range cases {
		got, ok := DescribeSkillInvocation(tc.content, " — ")
		if got != tc.want || ok != tc.ok {
			t.Fatalf("describe %q -> %q %v want %q %v", tc.content, got, ok, tc.want, tc.ok)
		}
	}
	if got, ok := ExtractUserInstructionFromSkillMessage("not scaffolding"); !ok || got != "not scaffolding" {
		t.Fatalf("passthrough %q %v", got, ok)
	}
	if _, ok := ExtractUserInstructionFromSkillMessage(skillInvocationPrefix + `"x" skill. ` + singleSkillMarker); ok {
		t.Fatal("bare invocation carries no instruction")
	}
	if got, _ := DescribeSkillInvocation(single+singleSkillInstruction+"go", " "); got != "/work go" {
		t.Fatalf("typed separator %q", got)
	}
}

func TestShapePreview(t *testing.T) {
	cases := []struct {
		raw  any
		want string
	}{
		{nil, ""}, {"   \x1c ", ""}, {int64(0), ""}, {0.0, ""}, {false, ""}, {int64(5), "5"}, {[]byte{}, ""},
		{" a\r\nb ", "a  b"}, {"head" + SkillExcerptJoint + "tail", "head"},
		{strings.Repeat("é", 61), strings.Repeat("é", 60) + "..."}, {strings.Repeat("é", 60), strings.Repeat("é", 60)},
	}
	for _, tc := range cases {
		if got := shapePreview(tc.raw); got != tc.want {
			t.Fatalf("shape %q -> %q want %q", tc.raw, got, tc.want)
		}
	}
}

func TestSQLBuildersAndSmallHelpers(t *testing.T) {
	if got := EscapeLike(`a_b%c\d`); got != `a\_b\%c\\d` {
		t.Fatalf("escape %q", got)
	}
	if got := sqlLiteral("it's"); got != "'it''s'" {
		t.Fatalf("literal %q", got)
	}
	if got := sqlJSONExtract("c.model_config", "$.o'k"); got != "json_extract((CASE WHEN json_valid(c.model_config) THEN c.model_config ELSE json_object() END), '$.o''k')" {
		t.Fatalf("extract %q", got)
	}
	if got := sqlStartsWith("x", []string{"é—"}); got != "(SUBSTR(LTRIM(x, "+sqlWhitespace+"), 1, 2) = 'é—')" {
		t.Fatalf("starts with counts code points %q", got)
	}
	if got := placeholders(3); got != "?,?,?" || placeholders(0) != "" {
		t.Fatalf("placeholders %q", got)
	}
	for reason, want := range map[any]bool{"agent_close": true, "lru_evict": true, "tui_shutdown": true, "session_reset": false, "compression": false, nil: false, 3: false} {
		if IsAutomaticEndReason(reason) != want {
			t.Fatalf("automatic %v", reason)
		}
	}
	if !boundaryEndReasons["new_session"] || !boundaryEndReasons["resume_pending_expired"] || boundaryEndReasons["agent_close"] {
		t.Fatal("boundary set")
	}
	if recoverableEndReasonsSQL != "'agent_close', 'ws_orphan_reap', 'superseded_by_resume', 'startup_orphan_reap'" {
		t.Fatalf("recoverable %q", recoverableEndReasonsSQL)
	}
	chunks := idChunks([]string{"a", "b", "c", "d", "e"}, 2)
	if !reflect.DeepEqual(chunks, [][]string{{"a", "b"}, {"c", "d"}, {"e"}}) || idChunks([]string{}, sqlInChunk) != nil {
		t.Fatalf("chunks %v", chunks)
	}
	if !endedByCompression(map[string]any{"ended_at": 1.0, "end_reason": "compression"}) || endedByCompression(map[string]any{"ended_at": nil, "end_reason": "compression"}) || endedByCompression(nil) {
		t.Fatal("ended by compression")
	}
}

func TestSourceMatchClause(t *testing.T) {
	cases := []struct {
		sources, local []string
		sql            string
		args           []any
	}{
		{nil, []string{"desktop"}, "", []any{}},
		{[]string{"cli", "telegram"}, nil, "s.source IN (?,?)", []any{"cli", "telegram"}},
		{[]string{"local"}, []string{"desktop"}, "(s.source = 'local' AND s.created_source IN (?))", []any{"desktop"}},
		{[]string{"cli", "local"}, []string{"desktop", "tui"}, "(s.source IN (?) OR (s.source = 'local' AND s.created_source IN (?,?)))", []any{"cli", "desktop", "tui"}},
	}
	for _, tc := range cases {
		sqlText, args := sourceMatchClause(tc.sources, tc.local)
		if sqlText != tc.sql || !reflect.DeepEqual(args, tc.args) {
			t.Fatalf("%v %v -> %q %v", tc.sources, tc.local, sqlText, args)
		}
	}
}

func TestStatDBFileIdentity(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(a, []byte("x"), 0o644)
	os.WriteFile(b, []byte("y"), 0o644)
	ia, okA := StatDBFileIdentity(a)
	ib, okB := StatDBFileIdentity(b)
	again, _ := StatDBFileIdentity(a)
	if !okA || !okB || ia == ib || ia != again {
		t.Fatalf("identities %v %v %v", ia, ib, again)
	}
	if _, ok := StatDBFileIdentity(filepath.Join(dir, "missing")); ok {
		t.Fatal("missing file has no identity")
	}
}

func TestEmitSessionDeletedPayloadAndProfileRebind(t *testing.T) {
	root := t.TempDir()
	t.Setenv(home.EnvHome, root)
	profileHome := filepath.Join(root, "profiles", "work")
	os.MkdirAll(profileHome, 0o755)
	type call struct {
		hook     string
		payload  map[string]any
		override string
	}
	var mu sync.Mutex
	var calls []call
	SetLifecycleHookInvoker(func(ctx context.Context, hook string, kwargs map[string]any) ([]any, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, call{hook, kwargs, home.Override(ctx)})
		return nil, nil
	})
	defer SetLifecycleHookInvoker(nil)
	d := &DB{sessionCore: &sessionCore{path: filepath.Join(profileHome, "state.db")}}
	d.EmitSessionDeleted(context.Background(), []string{"b", "", "a", "b"}, "prune", "", "/tmp/sessions")
	d.EmitSessionDeleted(context.Background(), nil, "prune", "", "")
	d.EmitSessionDeleted(context.Background(), []string{"x", "y"}, "archive", "y", "")
	plain := &DB{sessionCore: &sessionCore{path: filepath.Join(t.TempDir(), "state.db")}}
	plain.EmitSessionDeleted(context.Background(), []string{"solo"}, "delete", "other", "")
	if len(calls) != 3 {
		t.Fatalf("calls %d", len(calls))
	}
	first := calls[0]
	want := map[string]any{"session_id": "a", "reason": "prune", "profile": "work", "deleted_ids": []string{"a", "b"}, "sessions_dir": "/tmp/sessions"}
	if first.hook != "on_session_delete" || !reflect.DeepEqual(first.payload, want) || first.override != profileHome {
		t.Fatalf("first %+v", first)
	}
	if calls[1].payload["session_id"] != "y" {
		t.Fatalf("primary %+v", calls[1])
	}
	third := calls[2]
	if !reflect.DeepEqual(third.payload, map[string]any{"session_id": "solo", "reason": "delete", "profile": nil}) || third.override != "" {
		t.Fatalf("unprofiled %+v", third)
	}
}

func TestRoutedSessionsSetting(t *testing.T) {
	t.Setenv("ICEBATH_TEST_ROUTED", "env-value")
	if got := RoutedSessionsSetting(context.Background(), "auto_prune", "ICEBATH_TEST_ROUTED"); got != "env-value" {
		t.Fatalf("unscoped %v", got)
	}
	if got := RoutedSessionsSetting(context.Background(), "auto_prune", "ICEBATH_TEST_ROUTED_UNSET"); got != nil {
		t.Fatalf("unset %v", got)
	}
	served := t.TempDir()
	os.WriteFile(home.ConfigPath(served), []byte("sessions:\n  zz_routed_key: 7\n"), 0o644)
	ctx := home.WithOverride(context.Background(), served)
	if got := RoutedSessionsSetting(ctx, "zz_routed_key", "ICEBATH_TEST_ROUTED"); got != 7 {
		t.Fatalf("served profile %#v", got)
	}
	if got := RoutedSessionsSetting(ctx, "zz_absent_key", "ICEBATH_TEST_ROUTED"); got != nil {
		t.Fatalf("absent %#v", got)
	}
}

func TestScaffoldLikePatternsCarryNoWildcards(t *testing.T) {
	for _, pattern := range []string{SkillScaffoldSQLLike, AutoLoadScaffoldSQLLike} {
		literal, found := strings.CutSuffix(pattern, "%")
		if !found || strings.ContainsAny(literal, "%_") {
			t.Fatalf("pattern %q needs an ESCAPE clause", pattern)
		}
	}
	if _, ok := DescribeSkillInvocation("Why does my pool exhaust?", " — "); ok {
		t.Fatal("non scaffolding is untouched")
	}
}

func TestBoundaryEndReasonsIsACopy(t *testing.T) {
	got := BoundaryEndReasons()
	if len(got) != len(boundaryEndReasons) || !got["new_session"] || !got["session_reset"] || got["compression"] {
		t.Fatalf("boundary reasons %v", got)
	}
	got["compression"] = true
	if boundaryEndReasons["compression"] {
		t.Fatal("the exported copy must not alias the package set")
	}
}
