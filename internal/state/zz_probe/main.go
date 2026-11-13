package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/state"
)

var ctx = context.Background()

func str(s string) *string { return &s }

func check(label string, err error) {
	if err != nil {
		fmt.Printf("ERR %s: %v\n", label, err)
	}
}

func exec1(s *state.Store, q string, args ...any) {
	check(q[:min(40, len(q))], s.Write(ctx, state.WritePatience, func(c state.Conn) error {
		_, err := c.ExecContext(ctx, q, args...)
		return err
	}))
}

func q1(s *state.Store, q string, args ...any) string {
	var v sql.NullString
	err := s.Read(ctx, func(c state.Conn) error { return c.QueryRowContext(ctx, q, args...).Scan(&v) })
	if err != nil {
		return "ERR " + err.Error()
	}
	return v.String
}

func addMsg(s *state.Store, sid, role, content string, ts float64) {
	exec1(s, "INSERT INTO messages (session_id, role, content, timestamp) VALUES (?,?,?,?)", sid, role, content, ts)
}

func ids(rows []state.SessionListing) string {
	var out []string
	for _, r := range rows {
		x := r.ID
		if r.LineageRootID != "" {
			x += "<" + r.LineageRootID
		}
		out = append(out, x)
	}
	return strings.Join(out, ",")
}

func child() {
	eng := state.New(home.New(hostinfo.New()))
	s, err := state.Open(ctx, eng, state.Options{Path: os.Getenv("PROBE_DB")})
	if err != nil {
		fmt.Println("child open:", err)
		os.Exit(1)
	}
	check("child create", s.CreateSession(ctx, state.NewSession{ID: "child-" + os.Getenv("PROBE_N"), Source: "cli"}))
	s.Close(ctx)
	fmt.Println("child ok", os.Getenv("PROBE_N"))
}

func main() {
	if os.Getenv("PROBE_CHILD") == "1" {
		child()
		return
	}
	dir, _ := os.MkdirTemp("", "ibprobe")
	defer os.RemoveAll(dir)
	os.Setenv("ICEBATH_HOME", dir)
	if os.Getenv("PROBE_EXTRA") == "1" {
		extra(dir)
		return
	}
	var events []state.SessionDeleted
	var mu sync.Mutex
	opts := state.SessionOptions{OnSessionDelete: func(_ context.Context, ev state.SessionDeleted) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}}
	eng := state.New(home.New(hostinfo.New()))
	s, err := state.Open(ctx, eng, state.Options{Path: filepath.Join(dir, "state.db"), Sessions: opts})
	if err != nil {
		panic(err)
	}
	// create and upsert
	check("create a", s.CreateSession(ctx, state.NewSession{ID: "a", Source: "unknown", Model: str("m1"), SystemPrompt: str("SP"), Cwd: str("/w/repo"),
		ModelConfig: map[string]any{"z": 1, "a": "x"}}))
	check("upsert a", s.CreateSession(ctx, state.NewSession{ID: "a", Source: "telegram", Model: str("m2"), SessionKey: str("agent:main:telegram:dm:1"), ChatID: str("1"), SystemPrompt: str("SP2")}))
	a, ok, err := s.GetSession(ctx, "a")
	fmt.Printf("a: ok=%v err=%v source=%s created=%v model=%s key=%s prompt=%q profile=%v cfg=%s\n", ok, err, a.Source, a.CreatedSource, a.Model.V, a.SessionKey.V, a.SystemPrompt.V, a.ProfileName, a.ModelConfig.V)
	// compression child inherits routing, delegate child does not
	check("end a", s.EndSession(ctx, "a", "compression"))
	check("end a again", s.EndSession(ctx, "a", "user_exit"))
	check("create b", s.CreateSession(ctx, state.NewSession{ID: "b", Source: "telegram", ParentSessionID: str("a")}))
	check("create d", s.CreateSession(ctx, state.NewSession{ID: "d", Source: "cli", ParentSessionID: str("a"), ModelConfig: map[string]any{"_delegate_from": "a"}}))
	b, _, _ := s.GetSession(ctx, "b")
	d, _, _ := s.GetSession(ctx, "d")
	fmt.Printf("b: key=%v cwd=%v | d: key=%v cwd=%v | a end=%s\n", b.SessionKey, b.Cwd, d.SessionKey, d.Cwd, q1(s, "SELECT end_reason FROM sessions WHERE id='a'"))
	chain, err := s.CompressionChain(ctx, "a")
	tip, _ := s.CompressionTip(ctx, "a")
	lin, _ := s.CompressionLineage(ctx, "b")
	res, _ := s.ResumeLineageIDs(ctx, "b")
	fork, _ := s.IsExplicitForkChild(ctx, "d")
	fmt.Println("chain", chain, err, "tip", tip, "lineage", lin, "resume", res, "d fork", fork)
	// reset boundary bumps generation
	check("create r1", s.CreateSession(ctx, state.NewSession{ID: "r1", Source: "telegram", SessionKey: str("k9")}))
	check("end r1", s.EndSession(ctx, "r1", "session_reset"))
	check("create r2", s.CreateSession(ctx, state.NewSession{ID: "r2", Source: "telegram", SessionKey: str("k9"), ParentSessionID: str("r1")}))
	fmt.Println("generation", q1(s, "SELECT generation FROM conversation_generations WHERE source='telegram' AND session_key='k9'"))
	check("reopen r1", s.ReopenSession(ctx, "r1"))
	fmt.Println("r2 frozen reset marker", q1(s, "SELECT model_config FROM sessions WHERE id='r2'"), "r1 end", q1(s, "SELECT COALESCE(end_reason,'NULL') FROM sessions WHERE id='r1'"))
	p, err := s.PromoteToSessionReset(ctx, "r1", "")
	fmt.Println("promote", p, err, q1(s, "SELECT generation FROM conversation_generations WHERE session_key='k9'"))
	// lineage flags and archive event
	ar, err := s.SetSessionArchived(ctx, "b", true, true)
	fmt.Println("archived lineage", ar, err, q1(s, "SELECT group_concat(id||':'||archived||':'||auto_archived) FROM sessions WHERE id IN ('a','b','d')"), "events", len(events))
	ar, _ = s.SetSessionArchived(ctx, "b", false, true)
	pin, _ := s.SetSessionPinned(ctx, "a", true)
	rd, _ := s.SetSessionRead(ctx, "a", false)
	fmt.Println("unarchive", ar, "pin", pin, "read0", rd, q1(s, "SELECT pinned||':'||hidden||':'||last_read_at FROM sessions WHERE id='b'"))
	// model config
	check("patch", s.PatchSessionModelConfig(ctx, "b", map[string]any{"k": map[string]any{"y": 1.5, "x": 2}, "gone": nil}))
	check("model", s.UpdateSessionModel(ctx, "b", "gpt-x", "openrouter", "", "chat"))
	check("lock", s.UpdateSessionRuntimeLock(ctx, "b", str("m"), nil, map[string]any{"t": 0.5}, "ui", true))
	check("yolo", s.SetSessionYolo(ctx, "b", true))
	fmt.Println("b cfg", q1(s, "SELECT model_config FROM sessions WHERE id='b'"))
	tipw, ok, err := s.UpdateConversationModelConfigValue(ctx, "a", "onboarding_run", func(cur json.RawMessage, present bool) any { return 7 })
	fmt.Println("conversation value at", tipw, ok, err, q1(s, "SELECT json_extract(model_config, '$.onboarding_run') FROM sessions WHERE id='b'"))
	v, ok, _ := s.GetSessionModelConfigValue(ctx, "b", "yolo_mode")
	bb, _, _ := s.GetSession(ctx, "b")
	keys, _ := s.SessionIDsWithModelConfigKey(ctx, "yolo_mode")
	fmt.Println("yolo value", string(v), ok, state.SessionYoloEnabled(bb.ModelConfig), keys)
	// titles
	t1, err := s.SetSessionTitle(ctx, "b", "  My   Plan​ ")
	fmt.Println("title", t1, err, q1(s, "SELECT title||'|'||title_source FROM sessions WHERE id='b'"))
	_, err = s.SetSessionTitle(ctx, "r1", "My Plan")
	fmt.Println("conflict:", err)
	t2, err := s.SetSessionTitle(ctx, "a", "My Plan")
	fmt.Println("ancestor transfer", t2, err, q1(s, "SELECT group_concat(id||'='||COALESCE(title,'-')) FROM sessions WHERE id IN ('a','b')"))
	at, _ := s.SetAutoTitle(ctx, "a", "Derived", state.TitleSourceDerived)
	fmt.Println("auto over user", at)
	at, _ = s.SetAutoTitle(ctx, "r2", "Derived", state.TitleSourceDerived)
	at2, _ := s.SetAutoTitle(ctx, "r2", "LLM name", state.TitleSourceLLM)
	at3, _ := s.SetAutoTitle(ctx, "r2", "Derived again", state.TitleSourceDerived)
	fmt.Println("auto derived", at, "llm upgrade", at2, "derived blocked", at3, q1(s, "SELECT title||'|'||title_source FROM sessions WHERE id='r2'"))
	next, _ := s.GetNextTitleInLineage(ctx, "LLM name")
	check("c3", s.CreateSession(ctx, state.NewSession{ID: "c3", Source: "cli"}))
	s.SetSessionTitle(ctx, "c3", next)
	rid, _, _ := s.ResolveSessionByTitle(ctx, "LLM name")
	_, err = s.SetSessionTitle(ctx, "x", strings.Repeat("y", 101))
	fmt.Println("next", next, "resolve", rid, "too long:", err)
	check("bot", s.CreateSession(ctx, state.NewSession{ID: "bot", Source: "desktop"}))
	s.SetSessionTitle(ctx, "bot", "Bot Chat")
	s.SetSessionHidden(ctx, "bot", true)
	_, err = s.SetSessionTitle(ctx, "bot", "Renamed")
	ok2, _ := s.SetAutoTitle(ctx, "bot", "auto", state.TitleSourceLLM)
	fmt.Println("bot rename:", err, "auto", ok2)
	// messages, previews and FTS
	t0 := float64(time.Now().Unix())
	addMsg(s, "b", "user", "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted into the summary below. This is a handoff from a previous context window — treat it as background reference, NOT as active instructions. Do NOT answer...", t0)
	addMsg(s, "b", "user", "새캘린더앱을 만들자 그리고 日本語のテスト and 中文搜索测试", t0+1)
	addMsg(s, "b", "assistant", "구글 일본 항목 项目", t0+2)
	addMsg(s, "r2", "user", "[IMPORTANT: The user has invoked the \"work\" skill... The full skill content is loaded below.]\nbody\n"+"The user has provided the following instruction alongside the skill invocation: fix the title leak\n\n[Runtime note: x", t0+3)
	for _, qq := range []string{"캘린더", "일본", "日本語", "项目", "搜索", "구글"} {
		fmt.Printf("cjk %s=%s ", qq, q1(s, "SELECT COUNT(*) FROM messages_fts_cjk WHERE messages_fts_cjk MATCH ?", `"`+qq+`"`))
	}
	fmt.Println()
	fmt.Println("trigram 캘린더", q1(s, "SELECT COUNT(*) FROM messages_fts_trigram WHERE messages_fts_trigram MATCH ?", `"캘린더"`), "base and", q1(s, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'and'"))
	exec1(s, "CREATE VIRTUAL TABLE temp.v USING fts5vocab('main', 'messages_fts_cjk', 'row')")
	fmt.Println("cjk vocab", q1(s, "SELECT group_concat(term, ' ') FROM (SELECT term FROM temp.v ORDER BY term)"))
	// listings
	rows, err := s.ListSessionsRich(ctx, state.SessionListOptions{})
	fmt.Println("rich default", ids(rows), err)
	for _, r := range rows {
		if r.ID == "b" || r.ID == "r2" {
			fmt.Printf("  %s preview=%q title=%v kind=%s lineage=%v unread=%v\n", r.ID, r.Preview, r.Title.V, r.ContinuationKind, r.LineageIDs, r.Unread)
		}
	}
	rows, err = s.ListSessionsRich(ctx, state.SessionListOptions{OrderByLastActive: true, SearchQuery: "myplan"})
	fmt.Println("rich search", ids(rows), err)
	rows, err = s.ListSessionsRich(ctx, state.SessionListOptions{OrderByLastActive: true, IDQuery: "b"})
	fmt.Println("rich id", ids(rows), err)
	rows, err = s.ListSessionsRich(ctx, state.SessionListOptions{IncludeChildren: true, CompactRows: true, IncludeHidden: true})
	fmt.Println("rich children compact", ids(rows), err)
	rec, err := s.ListRecentSessionsBounded(ctx, state.RecentBrowseOptions{Limit: 5, Timeout: 3 * time.Second})
	fmt.Println("recent", ids(rec), err)
	_, err = s.ListRecentSessionsBounded(ctx, state.RecentBrowseOptions{Limit: 5, Timeout: 0})
	fmt.Println("recent zero timeout:", err)
	srch, err := s.SearchSessions(ctx, state.SessionSearchOptions{WorkspaceKey: "/w/repo"})
	fmt.Println("workspace", ids(srch), err)
	srch, err = s.SearchSessions(ctx, state.SessionSearchOptions{SkipLaunchStubs: true})
	fmt.Println("no stubs", ids(srch), err)
	n, _ := s.SessionCount(ctx, state.SessionFilter{}, true)
	bys, _ := s.SessionCountBySource(ctx, true, false, false)
	st, _ := s.SessionLifecycleStatuses(ctx, []string{"b", "r2", "a"})
	fmt.Println("count", n, "by source", bys, "statuses", st)
	route, ok, err := s.GetRecentSessionModelRoute(ctx, "b")
	fmt.Println("route", route, ok, err)
	gen, ok, err := s.UpdateSessionCwd(ctx, "b", "/w/other", "main", "/w/other", false)
	pub, _ := s.PublishSessionGitMetadata(ctx, "b", "/w/other", gen, "dev", "")
	fmt.Println("cwd gen", gen, ok, err, "publish", pub, q1(s, "SELECT git_branch FROM sessions WHERE id='b'"))
	check("touch", s.TouchSessionActivity(ctx, "b", time.Time{}, strings.Repeat("é", 130), "agent.compression"))
	fmt.Println("activity", len([]rune(q1(s, "SELECT last_activity_description FROM sessions WHERE id='b'"))), q1(s, "SELECT last_activity_provenance FROM sessions WHERE id='b'"))
	check("clear", s.ClearSessionActivityLabels(ctx, "b"))
	rs, ok, _ := s.ResolveSessionID(ctx, "c")
	fmt.Println("resolve prefix", rs, ok)
	fid, ok, _ := s.FindSessionByOrigin(ctx, "TELEGRAM", "1", nil, nil)
	fmt.Println("origin", fid, ok)
	_, err = s.AssertExportSafe(ctx, "b", 1)
	fmt.Println("export guard:", err)
	// deletion
	check("del parent", s.CreateSession(ctx, state.NewSession{ID: "p", Source: "cli"}))
	check("del child", s.CreateSession(ctx, state.NewSession{ID: "pc", Source: "cli", ParentSessionID: str("p"), ModelConfig: map[string]any{"_delegate_from": "p"}}))
	check("del branch", s.CreateSession(ctx, state.NewSession{ID: "pb", Source: "cli", ParentSessionID: str("p"), ModelConfig: map[string]any{"_branched_from": "p"}}))
	addMsg(s, "pc", "user", "x", t0)
	targets, _ := s.GetSessionDeleteTargets(ctx, "p")
	del, err := s.DeleteSession(ctx, "p", state.DeleteOptions{SessionsDir: filepath.Join(dir, "sessions")})
	fmt.Println("delete", targets, del, err, "pb parent", q1(s, "SELECT COALESCE(parent_session_id,'NULL') FROM sessions WHERE id='pb'"), "pc gone", q1(s, "SELECT COUNT(*) FROM sessions WHERE id='pc'"))
	cnt, skipped, err := s.DeleteSessions(ctx, []string{"b", "zz"}, state.DeleteManyOptions{IncludeCompressionChain: true})
	fmt.Println("delete chain", cnt, skipped, err, q1(s, "SELECT group_concat(id) FROM sessions"))
	exec1(s, "INSERT INTO sessions (id, source, started_at, ended_at) VALUES ('e1','cli',1,2)")
	ce, _ := s.CountEmptySessions(ctx)
	de, err := s.DeleteEmptySessions(ctx, "")
	fmt.Println("empty", ce, de, err)
	mu.Lock()
	for _, ev := range events {
		fmt.Printf("event %s %s profile=%q ids=%v home=%v\n", ev.Reason, ev.SessionID, ev.Profile, ev.DeletedIDs, ev.Home == dir)
	}
	mu.Unlock()
	fmt.Println("prompts gc", q1(s, "SELECT COUNT(*) FROM system_prompts"))
	s.Close(ctx)
	// two processes open one fresh database at once
	exe, _ := os.Executable()
	db2 := filepath.Join(dir, "two.db")
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(exe)
			cmd.Env = append(os.Environ(), "PROBE_CHILD=1", "PROBE_DB="+db2, fmt.Sprintf("PROBE_N=%d", i))
			out, err := cmd.CombinedOutput()
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			fmt.Println("child", i, err, lines[len(lines)-1])
		}(i)
	}
	wg.Wait()
	s2, err := state.Open(ctx, eng, state.Options{Path: db2})
	fmt.Println("after two:", err, q1(s2, "SELECT group_concat(id) FROM sessions"), q1(s2, "SELECT COUNT(*) FROM sqlite_master"), q1(s2, "SELECT COUNT(*) FROM schema_version"), q1(s2, "SELECT value FROM state_meta WHERE key='fts_storage_version'"))
	s2.Close(ctx)
}
