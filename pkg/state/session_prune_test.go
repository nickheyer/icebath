package state

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func pruneMk(t *testing.T, db *SessionDB, sid string, ageSeconds float64, title any, messageCount int, cwd string) {
	t.Helper()
	readSession(t, db, NewSession{ID: sid, Source: "cli", Cwd: cwd})
	if err := db.EndSession(sid, "done"); err != nil {
		t.Fatal(err)
	}
	profileExec(t, db, "UPDATE sessions SET started_at = ?, message_count = ?, title = ? WHERE id = ?", now()-ageSeconds, messageCount, title, sid)
}

func pruneMkRich(t *testing.T, db *SessionDB, sid, column, value string) {
	t.Helper()
	readSession(t, db, NewSession{ID: sid, Source: "cli"})
	if err := db.EndSession(sid, "done"); err != nil {
		t.Fatal(err)
	}
	profileExec(t, db, "UPDATE sessions SET "+column+" = ?, started_at = ? WHERE id = ?", value, now()-60, sid)
}

func pruneIDs(t *testing.T, db *SessionDB, olderThanDays *float64, source string, f PruneFilters) map[string]bool {
	t.Helper()
	rows, err := db.ListPruneCandidates(context.Background(), olderThanDays, source, false, f)
	if err != nil {
		t.Fatal(err)
	}
	return projIDs(rows)
}

func pruneRun(t *testing.T, db *SessionDB, o PruneSessionsOptions) int {
	t.Helper()
	n, err := db.PruneSessions(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func pruneSet(ids ...string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func pruneSurvives(t *testing.T, db *SessionDB, alive bool, ids ...string) {
	t.Helper()
	for _, sid := range ids {
		if (autoSession(t, db, sid) != nil) != alive {
			t.Fatalf("%s alive want %v", sid, alive)
		}
	}
}

func TestPruneOldEndedSessions(t *testing.T) {
	db := autoDB(t)
	readSession(t, db, NewSession{ID: "old", Source: "cli"})
	if err := db.EndSession("old", "done"); err != nil {
		t.Fatal(err)
	}
	profileExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", ago(100*24), "old")
	readSession(t, db, NewSession{ID: "new", Source: "cli"})
	if n := pruneRun(t, db, NewPruneSessionsOptions()); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	pruneSurvives(t, db, false, "old")
	if rowValue(autoSession(t, db, "new"), "id") != "new" {
		t.Fatal("new missing")
	}
}

func TestPruneSkipsActiveSessions(t *testing.T) {
	db := autoDB(t)
	readSession(t, db, NewSession{ID: "active", Source: "cli"})
	profileExec(t, db, "UPDATE sessions SET started_at = ? WHERE id = ?", ago(200*24), "active")
	if n := pruneRun(t, db, NewPruneSessionsOptions()); n != 0 {
		t.Fatalf("pruned %d", n)
	}
	pruneSurvives(t, db, true, "active")
	if n, err := db.CountOpenPruneMatches(context.Background(), pruneDays(90), "", PruneFilters{}); err != nil || n != 1 {
		t.Fatalf("open matches %d %v", n, err)
	}
}

func TestOpenPruneMatchCountAppliesOtherFilters(t *testing.T) {
	db := autoDB(t)
	readSession(t, db, NewSession{ID: "matching-open", Source: "cron"})
	readSession(t, db, NewSession{ID: "other-source", Source: "cli"})
	readSession(t, db, NewSession{ID: "ended", Source: "cron"})
	if err := db.EndSession("ended", "completed"); err != nil {
		t.Fatal(err)
	}
	profileExec(t, db, "UPDATE sessions SET started_at = ? WHERE id IN (?, ?, ?)", ago(200*24), "matching-open", "other-source", "ended")
	unarchived := false
	f := PruneFilters{Archived: &unarchived}
	if n, err := db.CountOpenPruneMatches(context.Background(), pruneDays(90), "cron", f); err != nil || n != 1 {
		t.Fatalf("open matches %d %v", n, err)
	}
	if got := pruneIDs(t, db, pruneDays(90), "cron", f); !reflect.DeepEqual(got, pruneSet("ended")) {
		t.Fatalf("candidates %v", got)
	}
}

func TestNegativeOlderThanDaysRejectedAtEveryPruneBoundary(t *testing.T) {
	ctx := context.Background()
	db := autoDB(t)
	readSession(t, db, NewSession{ID: "ended", Source: "cli"})
	if err := db.EndSession("ended", "done"); err != nil {
		t.Fatal(err)
	}
	readSession(t, db, NewSession{ID: "keyed", Source: "telegram", SessionKey: "telegram:dm:1"})
	neg := pruneDays(-1)
	_, errPrune := db.PruneSessions(ctx, PruneSessionsOptions{OlderThanDays: neg})
	_, errList := db.ListPruneCandidates(ctx, neg, "", false, PruneFilters{})
	_, errCount := db.CountPruneMatches(ctx, neg, "", false, PruneFilters{})
	_, errNever := db.ListNeverActiveKeyedSessions(ctx, -1)
	_, _, _, errNeverPrune := db.PruneNeverActiveKeyedSessions(ctx, NeverActivePruneOptions{OlderThanDays: -1})
	for i, err := range []error{errPrune, errList, errCount, errNever, errNeverPrune} {
		if err == nil || !strings.Contains(err.Error(), ">= 0") {
			t.Fatalf("boundary %d error %v", i, err)
		}
	}
	pruneSurvives(t, db, true, "ended", "keyed")
}

func TestStartedAfterWindowPrunesOnlyRecent(t *testing.T) {
	db := autoDB(t)
	pruneMk(t, db, "recent1", 3600, nil, 0, "")
	pruneMk(t, db, "recent2", 2*3600, nil, 0, "")
	pruneMk(t, db, "old", 10*3600, nil, 0, "")
	cutoff := now() - 5*3600
	if n := pruneRun(t, db, PruneSessionsOptions{Filters: PruneFilters{StartedAfter: &cutoff}}); n != 2 {
		t.Fatalf("pruned %d", n)
	}
	pruneSurvives(t, db, true, "old")
	pruneSurvives(t, db, false, "recent1")
}

func TestTitleAndMessageCountFilters(t *testing.T) {
	db := autoDB(t)
	pruneMk(t, db, "smoke1", 60, "Codex Smoke Test 1", 2, "")
	pruneMk(t, db, "smoke2", 60, "codex smoke test 2", 8, "")
	pruneMk(t, db, "real", 60, "Debugging auth", 8, "")
	if got := pruneIDs(t, db, nil, "", PruneFilters{TitleLike: "smoke"}); !reflect.DeepEqual(got, pruneSet("smoke1", "smoke2")) {
		t.Fatalf("candidates %v", got)
	}
	maxMessages := 3.0
	if n := pruneRun(t, db, PruneSessionsOptions{Filters: PruneFilters{TitleLike: "Smoke", MaxMessages: &maxMessages}}); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	pruneSurvives(t, db, false, "smoke1")
	pruneSurvives(t, db, true, "smoke2", "real")
}

func TestTitleLikeUnderscoreIsLiteralNotAWildcard(t *testing.T) {
	db := autoDB(t)
	pruneMk(t, db, "target", 0, "user_auth refactor", 0, "")
	pruneMk(t, db, "bystander1", 0, "user-auth review", 0, "")
	pruneMk(t, db, "bystander2", 0, "userXauth notes", 0, "")
	pruneMk(t, db, "bystander3", 0, "user auth meeting", 0, "")
	if got := pruneIDs(t, db, nil, "", PruneFilters{TitleLike: "user_auth"}); !reflect.DeepEqual(got, pruneSet("target")) {
		t.Fatalf("candidates %v", got)
	}
	if n := pruneRun(t, db, PruneSessionsOptions{Filters: PruneFilters{TitleLike: "user_auth"}}); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	pruneSurvives(t, db, true, "bystander1", "bystander2", "bystander3")
}

func TestPercentInFilterDoesNotSelectEverything(t *testing.T) {
	db := autoDB(t)
	pruneMk(t, db, "a", 0, "alpha", 0, "")
	pruneMk(t, db, "b", 0, "beta", 0, "")
	pruneMk(t, db, "pct", 0, "100% coverage run", 0, "")
	for _, needle := range []string{"%", "100%"} {
		if got := pruneIDs(t, db, nil, "", PruneFilters{TitleLike: needle}); !reflect.DeepEqual(got, pruneSet("pct")) {
			t.Fatalf("%q candidates %v", needle, got)
		}
	}
}

func TestBranchAndModelLikeUnderscoreIsLiteral(t *testing.T) {
	db := autoDB(t)
	pruneMkRich(t, db, "want", "git_branch", "fix/session_prune")
	pruneMkRich(t, db, "other", "git_branch", "fix/session-prune")
	if got := pruneIDs(t, db, nil, "", PruneFilters{BranchLike: "session_prune"}); !reflect.DeepEqual(got, pruneSet("want")) {
		t.Fatalf("branch candidates %v", got)
	}
	model := autoDB(t)
	pruneMkRich(t, model, "want", "model", "vendor/model_mini")
	pruneMkRich(t, model, "other", "model", "vendor/model-mini")
	if got := pruneIDs(t, model, nil, "", PruneFilters{ModelLike: "model_mini"}); !reflect.DeepEqual(got, pruneSet("want")) {
		t.Fatalf("model candidates %v", got)
	}
}

func TestPlainSubstringFiltersStillMatch(t *testing.T) {
	db := autoDB(t)
	pruneMk(t, db, "smoke", 0, "Codex Smoke Test", 0, "")
	pruneMkRich(t, db, "winpath", "title", `build C:\tmp artifacts`)
	if got := pruneIDs(t, db, nil, "", PruneFilters{TitleLike: "smoke"}); !reflect.DeepEqual(got, pruneSet("smoke")) {
		t.Fatalf("smoke candidates %v", got)
	}
	if got := pruneIDs(t, db, nil, "", PruneFilters{TitleLike: `c:\tmp`}); !reflect.DeepEqual(got, pruneSet("winpath")) {
		t.Fatalf("winpath candidates %v", got)
	}
}

func TestCwdPrefixFiltersMatchLiterally(t *testing.T) {
	db := autoDB(t)
	pruneMk(t, db, "target", 0, nil, 0, "/home/me/my_project/src")
	pruneMk(t, db, "sibling", 0, nil, 0, "/home/me/myXproject/src")
	if got := pruneIDs(t, db, nil, "", PruneFilters{CwdPrefix: "/home/me/my_project"}); !reflect.DeepEqual(got, pruneSet("target")) {
		t.Fatalf("underscore candidates %v", got)
	}
	if n := pruneRun(t, db, PruneSessionsOptions{Filters: PruneFilters{CwdPrefix: "/home/me/my_project"}}); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	pruneSurvives(t, db, true, "sibling")
	pct := autoDB(t)
	pruneMk(t, pct, "a", 0, nil, 0, "/home/me/one")
	pruneMk(t, pct, "b", 0, nil, 0, "/home/me/two")
	if got := pruneIDs(t, pct, nil, "", PruneFilters{CwdPrefix: "/home/me/%"}); len(got) != 0 {
		t.Fatalf("percent candidates %v", got)
	}
	tree := autoDB(t)
	pruneMk(t, tree, "root", 0, nil, 0, "/home/me/proj")
	pruneMk(t, tree, "child", 0, nil, 0, "/home/me/proj/src")
	pruneMk(t, tree, "outside", 0, nil, 0, "/home/me/other")
	if got := pruneIDs(t, tree, nil, "", PruneFilters{CwdPrefix: "/home/me/proj"}); !reflect.DeepEqual(got, pruneSet("root", "child")) {
		t.Fatalf("tree candidates %v", got)
	}
	win := autoDB(t)
	pruneMk(t, win, "win_root", 0, nil, 0, `C:\Users\me\my_project`)
	pruneMk(t, win, "win_child", 0, nil, 0, `C:\Users\me\my_project\src`)
	pruneMk(t, win, "win_sibling", 0, nil, 0, `C:\Users\me\myXproject\src`)
	if got := pruneIDs(t, win, nil, "", PruneFilters{CwdPrefix: `C:\Users\me\my_project`}); !reflect.DeepEqual(got, pruneSet("win_root", "win_child")) {
		t.Fatalf("windows candidates %v", got)
	}
}
