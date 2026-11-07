package state

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func preparedFixture(t *testing.T) (context.Context, *SessionDB, string) {
	t.Helper()
	db := mustSessionDB(t, filepath.Join(t.TempDir(), "state.db"), false)
	t.Cleanup(func() { db.Close() })
	return context.Background(), db, seedLocalSession(t, db, t.TempDir())
}

func preparedSnapshot(t *testing.T, ctx context.Context, db *SessionDB, sid string) *pyjson.Object {
	t.Helper()
	var snapshot *pyjson.Object
	if err := db.readCtx(ctx, false, func(conn SQLiteConn) error {
		var err error
		snapshot, err = db.LocalSnapshot(ctx, conn, sid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func runPolicyHandler(ctx context.Context, db *SessionDB, operation, sid string, payload, prepared *pyjson.Object) ([]string, *pyjson.Object, error) {
	var affected []string
	var result *pyjson.Object
	err := db.executeWrite(ctx, func(conn SQLiteConn) error {
		var err error
		affected, result, err = policyHandlers[operation](db, ctx, conn, sid, payload, prepared)
		return err
	}, nil)
	return affected, result, err
}

func storedValue(t *testing.T, db *SessionDB, query string, args ...any) any {
	t.Helper()
	var value any
	if err := db.readCtx(context.Background(), false, func(conn SQLiteConn) error {
		var err error
		value, _, err = fetchScalar(context.Background(), conn, query, args...)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestModelPickCommitsOverItsSnapshotAndSupersedesTheDeferredModel(t *testing.T) {
	ctx, db, sid := preparedFixture(t)
	snapshot := preparedSnapshot(t, ctx, db, sid)
	if got := pyStr(rowValue(snapshot, "target")); got != sid {
		t.Fatalf("target %q", got)
	}
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", DeferredPickPrefix+sid, `{"model":"later","reasoning":"high"}`)
	policy := pyjson.ObjectOf("source", "cli", "cwd", "/w", "model", "m2", "config_json", `{"model":{"provider":"p2"}}`)
	prepared := pyjson.ObjectOf("snapshot", snapshot, "policy", policy)
	affected, result, err := runPolicyHandler(ctx, db, "model", sid, pyjson.ObjectOf("model", "m2"), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(affected, []string{sid}) {
		t.Fatalf("affected %v", affected)
	}
	want := pyjson.ObjectOf("model", "m2", "provider", "p2", "execution_generation", int64(1))
	if !pyjson.Equal(result, want) {
		t.Fatalf("result %v", result)
	}
	if got := storedValue(t, db, "SELECT model_config FROM sessions WHERE id=?", sid); got != `{"provider":"p2"}` {
		t.Fatalf("model_config %v", got)
	}
	if got := storedValue(t, db, "SELECT value FROM state_meta WHERE key=?", DeferredPickPrefix+sid); got != `{"reasoning":"high"}` {
		t.Fatalf("deferred pick %v", got)
	}
	receipt, err := loadsStoredObject(storedValue(t, db, "SELECT value FROM state_meta WHERE key=?", LocalPolicyPrefix+sid))
	if err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(rowValue(receipt, "policy"), policy) {
		t.Fatalf("policy %v", rowValue(receipt, "policy"))
	}
	if _, _, err := runPolicyHandler(ctx, db, "model", sid, pyjson.ObjectOf("model", "m2"), prepared); refusalReason(t, err) != "revision_conflict" {
		t.Fatal(err)
	}
	if _, _, err := runPolicyHandler(ctx, db, "model", sid, pyjson.ObjectOf("model", "m2"), pyjson.ObjectOf("policy", policy)); refusalReason(t, err) != "runtime_coordination_required" {
		t.Fatal(err)
	}
	withLevel := pyjson.ObjectOf("snapshot", preparedSnapshot(t, ctx, db, sid), "policy", policy)
	_, result, err = runPolicyHandler(ctx, db, "model", sid, pyjson.ObjectOf("model", "m2", "reasoning", "low"), withLevel)
	if err != nil {
		t.Fatal(err)
	}
	if got := rowValue(result, "effort"); got != "low" {
		t.Fatalf("effort %v", got)
	}
	if got := storedValue(t, db, "SELECT value FROM state_meta WHERE key=?", DeferredPickPrefix+sid); got != nil {
		t.Fatalf("deferred level survived a selection carrying its own: %v", got)
	}
}

func TestReasoningPickDropsEveryDeferredLevel(t *testing.T) {
	ctx, db, sid := preparedFixture(t)
	policy := pyjson.ObjectOf("source", "cli", "cwd", "/w", "model", "m", "reasoning", "high")
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", DeferredPickPrefix+sid,
		`{"application_id":"a","payload":{"model":"x","reasoning":"low"},"reasoning":"low"}`)
	prepared := pyjson.ObjectOf("snapshot", preparedSnapshot(t, ctx, db, sid), "policy", policy)
	_, result, err := runPolicyHandler(ctx, db, "reasoning", sid, pyjson.ObjectOf("effort", "high"), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(result, pyjson.ObjectOf("effort", "high", "execution_generation", int64(1))) {
		t.Fatalf("result %v", result)
	}
	if got := storedValue(t, db, "SELECT value FROM state_meta WHERE key=?", DeferredPickPrefix+sid); got != `{"application_id":"a","payload":{"model":"x"}}` {
		t.Fatalf("deferred pick %v", got)
	}
	profileExec(t, db, "UPDATE state_meta SET value=? WHERE key=?", `{"reasoning":"low"}`, DeferredPickPrefix+sid)
	prepared = pyjson.ObjectOf("snapshot", preparedSnapshot(t, ctx, db, sid), "policy", policy)
	if _, result, err = runPolicyHandler(ctx, db, "reasoning", sid, pyjson.ObjectOf("effort", nil), prepared); err != nil {
		t.Fatal(err)
	}
	if got, has := result.Get("effort"); !has || got != nil {
		t.Fatalf("reset effort %v", got)
	}
	if got := storedValue(t, db, "SELECT value FROM state_meta WHERE key=?", DeferredPickPrefix+sid); got != nil {
		t.Fatalf("level-only deferred pick kept %v", got)
	}
}

func TestWorkspaceMoveReplacesTheGitIdentityOfEveryRow(t *testing.T) {
	ctx, db, sid := preparedFixture(t)
	profileExec(t, db, "UPDATE sessions SET git_branch='old', git_repo_root='/old' WHERE id=?", sid)
	policy := pyjson.ObjectOf("source", "cli", "cwd", "/new", "model", "m")
	prepared := pyjson.ObjectOf("snapshot", preparedSnapshot(t, ctx, db, sid), "policy", policy, "git", pyjson.ObjectOf("branch", "", "root", "/new"))
	_, result, err := runPolicyHandler(ctx, db, "workspace", sid, pyjson.ObjectOf("cwd", "/new"), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if !pyjson.Equal(result, pyjson.ObjectOf("cwd", "/new", "branch", nil, "git_repo_root", "/new", "execution_generation", int64(1))) {
		t.Fatalf("result %v", result)
	}
	if got := storedValue(t, db, "SELECT cwd || '|' || COALESCE(git_branch,'NULL') || '|' || git_repo_root || '|' || git_metadata_generation FROM sessions WHERE id=?", sid); got != "/new|NULL|/new|1" {
		t.Fatalf("row %v", got)
	}
	for _, op := range []string{"editor_mcp", "edit_policy"} {
		prepared = pyjson.ObjectOf("snapshot", preparedSnapshot(t, ctx, db, sid), "policy", policy, "tools_changed", true, "mcp_servers", []any{"s"})
		_, result, err = runPolicyHandler(ctx, db, op, sid, pyjson.ObjectOf("policy", "session"), prepared)
		if err != nil {
			t.Fatal(err)
		}
		if got := rowValue(result, "execution_generation"); !pyjson.Equal(got, map[string]int64{"editor_mcp": 2, "edit_policy": 3}[op]) {
			t.Fatalf("%s generation %v", op, got)
		}
	}
	if got := rowValue(result, "edit_approval_policy"); got != "session" {
		t.Fatalf("edit policy %v", got)
	}
}

func TestPolicyPicksReadCommittedReceiptsAndTheBranchLineage(t *testing.T) {
	ctx, db, sid := preparedFixture(t)
	receipt := func(key, result string) {
		profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", "gateway.mutation.v1."+key, `{"digest":"d","result":`+result+`}`)
	}
	receipt("a", `{"operation":"model","session_id":"`+sid+`","revision":1}`)
	receipt("b", `{"operation":"workspace","session_id":"`+sid+`","revision":3}`)
	receipt("c", `{"operation":"reasoning","status":"deferred","session_id":"`+sid+`","revision":4}`)
	receipt("d", `{"operation":"branch","session_id":"`+sid+`","revision":2,"branched_session_id":"old-child"}`)
	picks, err := db.PolicyPicks(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(picks, []string{"model", "workspace"}) {
		t.Fatalf("parent picks %v", picks)
	}
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", LocalPolicyPrefix+"old-child", `{"policy":{}}`)
	if picks, err = db.PolicyPicks(ctx, "old-child"); err != nil || !reflect.DeepEqual(picks, []string{"model"}) {
		t.Fatalf("pre picks branch %v %v", picks, err)
	}
	profileExec(t, db, "INSERT INTO state_meta(key,value) VALUES(?,?)", LocalPolicyPrefix+"new-child", `{"policy":{},"picks":["reasoning"]}`)
	if picks, err = db.PolicyPicks(ctx, "new-child"); err != nil || !reflect.DeepEqual(picks, []string{"reasoning"}) {
		t.Fatalf("branch picks %v %v", picks, err)
	}
}

func TestCommitRefreshedPolicyReplacesOnlyTheExactOldPolicy(t *testing.T) {
	ctx, db, sid := preparedFixture(t)
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	old := rowValue(rowValue(preparedSnapshot(t, ctx, db, sid), "receipt").(*pyjson.Object), "policy")
	fresh := pyjson.ObjectOf("source", "cli", "cwd", "/fresh", "model", "m")
	if _, err := db.CommitRefreshedPolicy(ctx, epoch, sid, fresh, fresh, 0); refusalReason(t, err) != "revision_conflict" {
		t.Fatal(err)
	}
	if _, err := db.CommitRefreshedPolicy(ctx, epoch, sid, old, fresh, 1); refusalReason(t, err) != "revision_conflict" {
		t.Fatal(err)
	}
	if _, err := db.CommitRefreshedPolicy(ctx, epoch+1, sid, old, fresh, 0); refusalReason(t, err) != "stale_epoch" {
		t.Fatal(err)
	}
	next, err := db.CommitRefreshedPolicy(ctx, epoch, sid, old, fresh, 0)
	if err != nil || !pyjson.Equal(next, int64(1)) {
		t.Fatalf("generation %v %v", next, err)
	}
	stored := rowValue(rowValue(preparedSnapshot(t, ctx, db, sid), "receipt").(*pyjson.Object), "policy")
	if !pyjson.Equal(stored, fresh) {
		t.Fatalf("policy %v", stored)
	}
}
