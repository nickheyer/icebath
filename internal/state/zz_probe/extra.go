package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/hostinfo"
	"github.com/nickheyer/icebath/internal/state"
)

func extra(dir string) {
	eng := state.New(home.New(hostinfo.New()))
	path := filepath.Join(dir, "extra.db")
	s, err := state.Open(ctx, eng, state.Options{Path: path})
	if err != nil {
		panic(err)
	}
	t0 := float64(time.Now().Unix())
	for i, id := range []string{"seg1", "seg2", "seg3", "pinned1"} {
		check("mk", s.CreateSession(ctx, state.NewSession{ID: id, Source: "local", ChatID: str("seg1"), SessionKey: str("route-1")}))
		addMsg(s, id, "user", "hello "+id, t0+float64(i))
	}
	exec1(s, "UPDATE sessions SET ended_at = 5, end_reason = 'session_reset' WHERE id IN ('seg1','seg2')")
	exec1(s, "UPDATE sessions SET parent_session_id = 'seg1' WHERE id = 'seg2'")
	exec1(s, "UPDATE sessions SET parent_session_id = 'seg2' WHERE id = 'seg3'")
	exec1(s, `INSERT INTO state_meta VALUES ('gateway.local_policy.v1:seg1', '{"session_id": "seg1", "lineage": ["seg1", "seg2", "seg3"], "entry": {"session_id": "seg3"}}')`)
	exec1(s, "UPDATE sessions SET archived = 1, pinned = 1 WHERE id = 'pinned1'")
	rows, err := s.ListSessionsRich(ctx, state.SessionListOptions{IncludePinned: true})
	for _, r := range rows {
		fmt.Printf("local row %s lineage=%v archived=%d\n", r.ID, r.LineageIDs, r.Archived)
	}
	fmt.Println("local list err", err)
	n, _ := s.SessionCount(ctx, state.SessionFilter{}, true)
	targets, _ := s.GetSessionDeleteTargets(ctx, "seg2")
	fmt.Println("local count", n, "delete targets of seg2", targets)
	var vocab string
	s.Read(ctx, func(c state.Conn) error {
		c.ExecContext(ctx, "CREATE VIRTUAL TABLE temp.v USING fts5vocab('main', 'messages_fts', 'row')")
		return c.QueryRowContext(ctx, "SELECT group_concat(term, ' ') FROM (SELECT term FROM temp.v ORDER BY term)").Scan(&vocab)
	})
	fmt.Println("base vocab", vocab)
	addMsg(s, "seg3", "user", "새캘린더앱을 만들자 日本語 x中文y", t0+9)
	s.Read(ctx, func(c state.Conn) error {
		c.ExecContext(ctx, "CREATE VIRTUAL TABLE temp.cv USING fts5vocab('main', 'messages_fts_cjk', 'row')")
		return c.QueryRowContext(ctx, "SELECT group_concat(term, ' ') FROM (SELECT term FROM temp.cv ORDER BY term)").Scan(&vocab)
	})
	fmt.Println("cjk vocab", vocab)
	fmt.Println("cjk phrase", q1(s, "SELECT COUNT(*) FROM messages_fts_cjk WHERE messages_fts_cjk MATCH '\"캘린\"'"), q1(s, "SELECT COUNT(*) FROM messages_fts_cjk WHERE messages_fts_cjk MATCH '\"앱을만\"'"), q1(s, "SELECT COUNT(*) FROM messages_fts_cjk WHERE messages_fts_cjk MATCH '\"中文\"'"))
	opt, err := s.OptimizeFTS(ctx)
	reb, err2 := s.RebuildFTS(ctx)
	lock, _ := os.ReadFile(path + ".fts_rebuild.lock")
	fmt.Println("optimize", opt, err, "rebuild", reb, err2, "lock file bytes after release", len(lock))
	for i := 0; i < 1000; i++ {
		check("touch", s.SetMeta(ctx, "k", fmt.Sprint(i)))
	}
	fmt.Println("after 1000 writes usermerge", q1(s, "SELECT v FROM messages_fts_config WHERE k='usermerge'"))
	// FTS scoped corruption fails open on the next write
	exec1(s, "UPDATE messages_fts_data SET block = x'ffffffffffffffffffff' WHERE id = 10")
	werr := s.Write(ctx, state.WritePatience, func(c state.Conn) error {
		_, err := c.ExecContext(ctx, "INSERT INTO messages (session_id, role, content, timestamp) VALUES ('seg3','user','after corruption words',?)", t0+20)
		return err
	})
	fmt.Println("write over corrupt index:", werr, s.FullText(), "stale key", q1(s, "SELECT value FROM state_meta WHERE key='fts_stale'"))
	lf, _ := os.OpenFile(path+".fts_rebuild.lock", os.O_RDWR|os.O_CREATE, 0o666)
	fmt.Println("hold lock:", fileutil.TryLockFile(lf))
	fmt.Println("retry while held:", s.RetryDeferredFTSRecovery(ctx), s.FullText())
	fileutil.UnlockFile(lf)
	lf.Close()
	s.Close(ctx)
	s, err = state.Open(ctx, eng, state.Options{Path: path})
	fmt.Println("reopen recovers:", err, s.FullText(), "match", q1(s, "SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'corruption'"), "stale", q1(s, "SELECT COUNT(*) FROM state_meta WHERE key='fts_stale'"))
	s.Close(ctx)

	// ids, keys and the identity codec
	id, _ := state.NewSessionID(time.Date(2026, 11, 13, 9, 8, 7, 0, time.Local), 6)
	fmt.Println("id", id, state.IsKnownSessionID(id), state.IsKnownSessionID("cron_job-1_20260101_120000"), state.IsKnownSessionID("bg_123456_abcdef"),
		state.IsKnownSessionID("api_1700000000_deadbeef"), state.IsKnownSessionID("nope"), state.IsKnownSessionID("cron_x_20260101_120000\n"))
	j, e, ok := state.CronRunFromRequest("cron:job:exec1", "")
	j2, e2, ok2 := state.CronRunFromRequest("cron:a:b:c", "a:b")
	lo, hi := state.CronRequestRange("job")
	lj, lok := state.LegacyCronRunJob("cron_daily_digest_20260101_010101")
	fmt.Println("cron", j, e, ok, "|", j2, e2, ok2, "|", lo, hi, "|", lj, lok)
	li, _ := state.LocalIdentity("default", "cron-owner", "req-é")
	sum := sha256.Sum256([]byte(`["default","cron-owner","req-\u00e9"]`))
	fmt.Println("local identity matches", li == "local-"+hex.EncodeToString(sum[:]))
	fp, _ := state.AdmissionFingerprint("t", map[string]any{"b": 1.0, "a": "<é>", "n": []any{1, nil}})
	sum = sha256.Sum256([]byte(`{"payload":{"a":"<é>","b":1.0,"n":[1,null]},"target":"t"}`))
	fmt.Println("fingerprint matches", fp == hex.EncodeToString(sum[:]))
	p, okp := state.ProfileFromSessionKey("agent:main~:telegram")
	fmt.Println("profile", p, okp)
	m := state.DecodeToolCallUIDs(`{"c1": "u1", "c2": ["u2", "u3"], "c3": "", "": "x", "c4": [1]}`)
	enc, _, _ := state.EncodeToolCallUIDs(m)
	restored := state.RestoreToolCallUIDs(state.ToolCallUIDs{"c2": {Many: []string{"u2", "u3", "u4"}}}, m)
	renc, _, _ := state.EncodeToolCallUIDs(restored)
	ab, _, _ := state.EncodeAbsorbedUIDs([]string{"x", "", "x", "y"})
	k, okk := state.StableToolKeyOf("assistant", `[{"id":"call_1|item"},{"call_id":" c2 "}]`, 3, nil, str("t"))
	fmt.Println("uids", enc, "|", renc, "|", ab, "|", state.DecodeUIDList(`["a","a",1,"b"]`), "|", k.CallIDs, okk)
	probes, err := state.SchemaReadProbeStatements()
	fmt.Println("probe statements", len(probes), err)
	cur, _ := state.CurrentDeclaredColumns("sessions")
	fmt.Println("history sessions columns", len(cur), cur[0], cur[1], cur[len(cur)-1])
	layouts, _ := state.ReachablePhysicalLayouts("session_model_usage", nil)
	count := 0
	for range layouts {
		count++
	}
	fmt.Println("usage layouts", count)
}
