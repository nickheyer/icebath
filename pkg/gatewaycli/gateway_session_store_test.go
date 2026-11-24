package gatewaycli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
)

func TestNewSessionIsCreatedOnTheGatewayAndItsIdAdopted(t *testing.T) {
	db, client := storeFixture(t)
	sid := mustOpen(t, db, storeCLI("", false), map[string]any{"model": "m1", "toolsets": []any{"file", "web"}, "provider": nil})
	if sid != "local-new" {
		t.Fatalf("sid %q", sid)
	}
	calls := client.all()
	if len(calls) == 0 || calls[0].method != "session.create" {
		t.Fatalf("calls %v", calls)
	}
	params := calls[0].params
	wantEqual(t, params["model"], "m1")
	wantEqual(t, params["toolsets"], []any{"file", "web"})
	wantEqual(t, params["source"], "cli")
	if _, has := params["provider"]; has || params["cwd"] == "" || params["cwd"] == nil {
		t.Fatalf("params %v", params)
	}
	wantEqual(t, db.Connection.State("local-new").Info["model"], "fake-model")
}

func TestAFiniteRunCreatesMainsOneshotSessionSoBareCFindsIt(t *testing.T) {
	db, client := storeFixture(t)
	t.Setenv("ICEBATH_SINGLE_QUERY_SESSION", "1")
	mustOpen(t, db, storeCLI("", false), map[string]any{})
	wantEqual(t, client.last().params["source"], "oneshot")
	t.Setenv("ICEBATH_SESSION_SOURCE", "tool")
	t.Setenv("ICEBATH_SESSION_SOURCE_EXPLICIT", "1")
	mustOpen(t, NewGatewaySessionStore(db.Connection), storeCLI("", false), map[string]any{})
	wantEqual(t, client.last().params["source"], "tool")
	unsetEnv(t, "ICEBATH_SINGLE_QUERY_SESSION", "ICEBATH_SESSION_SOURCE", "ICEBATH_SESSION_SOURCE_EXPLICIT")
	mustOpen(t, NewGatewaySessionStore(db.Connection), storeCLI("", false), map[string]any{})
	wantEqual(t, client.last().params["source"], "cli")
}

func TestResumeAttachesAndUnknownTargetIsLeftToMainsNotFoundPath(t *testing.T) {
	db, client := storeFixture(t)
	if sid := mustOpen(t, db, storeCLI("local-old", true), nil); sid != "local-old" {
		t.Fatalf("sid %q", sid)
	}
	if client.last().method != "session.resume" {
		t.Fatalf("last call %v", client.last())
	}
	if sid := mustOpen(t, db, storeCLI("missing", true), nil); sid != "" {
		t.Fatalf("unknown target adopted %q", sid)
	}
}

func TestTitleRenameAndDetachAreGatewayVerbs(t *testing.T) {
	ctx := context.Background()
	db, client := storeFixture(t)
	mustOpen(t, db, storeCLI("local-old", true), nil)
	if ok, err := db.SetSessionTitle(ctx, "local-old", "Some Title"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	mutate := client.lastOf(t, "session.mutate")
	wantEqual(t, mutate["operation"], "rename")
	wantEqual(t, mutate["payload"], map[string]any{"title": "Some Title"})
	wantEqual(t, mutate["expected_revision"], 3)
	if title := db.Connection.State("local-old").Title; title != "Some Title" {
		t.Fatalf("title %q", title)
	}
	if err := db.EndSession(ctx, "local-old", "cli_close"); err != nil {
		t.Fatal(err)
	}
	last := client.last()
	if last.method != "session.detach" || !reflect.DeepEqual(last.params, map[string]any{"session_id": "local-old",
		"subscription_id": "sub-local-old", "end_reason": "cli_close"}) {
		t.Fatalf("last call %v", last)
	}
}

func wantClientError(t *testing.T, err error, contains string) {
	t.Helper()
	var ce *gateway.GatewayClientError
	if err == nil || !errors.As(err, &ce) || !strings.Contains(err.Error(), contains) {
		t.Fatalf("error %v, want a gateway client error containing %q", err, contains)
	}
}

func TestGatewayOwnedWritesAreNoops(t *testing.T) {
	db, client := storeFixture(t)
	before := len(client.all())
	db.CreateSession("x", "cli")
	db.ReopenSession("x")
	db.AppendMessagesBatch("x", []any{})
	if db.PruneEmptyGhostSessions() != 0 {
		t.Fatal("ghost repair counted rows")
	}
	if len(client.all()) != before {
		t.Fatalf("calls %v", client.all()[before:])
	}
}

func TestConnectionIsFinalOnceClosed(t *testing.T) {
	db, _ := storeFixture(t)
	if db.Connection.Final() {
		t.Fatal("connection final while connected")
	}
	db.Connection.Close(context.Background())
	if !db.Connection.Final() {
		t.Fatal("connection not final after close")
	}
}

func TestReaderOpensOnceTheGatewayCreatedStateDb(t *testing.T) {
	ctx := context.Background()
	db, _ := storeFixture(t)
	if row, err := db.GetSession(ctx, "x"); row != nil || err != nil || db.reader != nil {
		t.Fatalf("row %v err %v reader %v", row, err, db.reader)
	}
	writer, err := state.NewSessionDB(ctx, home.StatePath(home.Dir(ctx)), false)
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	t.Cleanup(func() { db.CloseReader() })
	if row, err := db.GetSession(ctx, "x"); row != nil || err != nil || db.reader == nil {
		t.Fatalf("row %v err %v reader %v", row, err, db.reader)
	}
}

func TestRenameRefusalPrintsMainsTitleInUseText(t *testing.T) {
	ctx := context.Background()
	db, client := storeFixture(t)
	writer, err := state.NewSessionDB(ctx, home.StatePath(home.Dir(ctx)), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.CreateSession(state.NewSession{ID: "local-other", Source: "local"}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.SetSessionTitle(ctx, "local-other", "Alpha Title"); err != nil {
		t.Fatal(err)
	}
	if err := writer.CreateSession(state.NewSession{ID: "local-old", Source: "local"}); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	t.Cleanup(func() { db.CloseReader() })
	mustOpen(t, db, storeCLI("local-old", true), nil)
	client.setRPC(func(ctx context.Context, method string, params map[string]any) (any, error) {
		client.record(method, params)
		if method == "session.mutate" {
			return nil, &gateway.GatewayClientError{Msg: "request_failed"}
		}
		sid, ok := params["session_id"].(string)
		if !ok {
			sid = "local-old"
		}
		return storeSnapshot(sid), nil
	})
	_, err = db.SetSessionTitle(ctx, "local-old", "Alpha Title")
	var inUse *TitleInUseError
	if !errors.As(err, &inUse) || inUse.ErrorKind() != "ValueError" ||
		err.Error() != "Title 'Alpha Title' is already in use by session local-other" {
		t.Fatalf("error %v", err)
	}
	_, err = db.SetSessionTitle(ctx, "local-old", "Free Title")
	if err == nil || err.Error() != "request_failed" {
		t.Fatalf("error %v", err)
	}
	wantClientError(t, err, "request_failed")
}

func storeHistory() []map[string]any {
	return []map[string]any{{"role": "user", "content": "one", "row_id": 11}, {"role": "assistant", "content": "a1", "row_id": 12},
		{"role": "user", "content": "two", "row_id": 13}, {"role": "assistant", "content": "a2", "row_id": 14}}
}

func withHistory(client *fakeConnClient) {
	base := client.rpc
	client.setRPC(func(ctx context.Context, method string, params map[string]any) (any, error) {
		if method == "session.resume" {
			client.record(method, params)
			snapshot := storeSnapshot(params["session_id"].(string))
			var messages []any
			for _, message := range storeHistory() {
				messages = append(messages, message)
			}
			snapshot["messages"] = messages
			snapshot["execution_generation"] = 7
			return snapshot, nil
		}
		if method == "session.mutate" && params["operation"] == "rewind" {
			client.record(method, params)
			return map[string]any{"rewound_count": 2, "target_message": map[string]any{"role": "user", "content": "two"}}, nil
		}
		return base(ctx, method, params)
	})
}

func warmHistory() []map[string]any {
	var warm []map[string]any
	for _, message := range storeHistory() {
		delete(message, "row_id")
		warm = append(warm, message)
	}
	return warm
}

func TestUndoAndRetryRewindByTheDurableRowId(t *testing.T) {
	ctx := context.Background()
	db, client := storeFixture(t)
	withHistory(client)
	mustOpen(t, db, storeCLI("local-old", true), nil)
	warm := warmHistory()
	outcome, err := db.RewindUserTurn(ctx, "local-old", 1, state.RewindUserTurnOptions{WarmHistory: warm, HasWarmHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	mutate := client.lastOf(t, "session.mutate")
	wantEqual(t, mutate["operation"], "rewind")
	wantEqual(t, mutate["payload"], map[string]any{"target_message_id": 13})
	wantEqual(t, mutate["expected_generation"], 7)
	if !reflect.DeepEqual(outcome.Prefix, warm[:2]) || outcome.LiveText != "two" || outcome.RewoundCount != 2 {
		t.Fatalf("outcome %+v", outcome)
	}
	retry, err := db.RewindUserTurn(ctx, "local-old", -1, state.RewindUserTurnOptions{WarmHistory: warm, HasWarmHistory: true,
		RequireRetryable: true})
	if err != nil {
		t.Fatal(err)
	}
	wantEqual(t, client.lastOf(t, "session.mutate")["payload"], map[string]any{"target_message_id": 13})
	if retry.LiveText != "two" || retry.TurnsUndone != 1 {
		t.Fatalf("retry %+v", retry)
	}
}

func TestRewindRefusesWhenTheWarmViewDiverged(t *testing.T) {
	db, client := storeFixture(t)
	withHistory(client)
	mustOpen(t, db, storeCLI("local-old", true), nil)
	before := len(client.of("session.mutate"))
	_, err := db.RewindUserTurn(context.Background(), "local-old", -1, state.RewindUserTurnOptions{
		WarmHistory: []map[string]any{{"role": "user", "content": "other"}}, HasWarmHistory: true})
	var runtime *state.RewindRuntimeError
	if !errors.As(err, &runtime) {
		t.Fatalf("error %v", err)
	}
	if len(client.of("session.mutate")) != before {
		t.Fatal("a refused rewind reached the gateway")
	}
}

func TestCreateIfMissingTitlesAGatewayCreatedSession(t *testing.T) {
	db, client := storeFixture(t)
	sid, err := createTitledSession(context.Background(), db, "Fresh Named")
	if err != nil || sid != "local-new" {
		t.Fatal(sid, err)
	}
	params := client.lastOf(t, "session.create")
	if params["title"] != "Fresh Named" || params["source"] == "" || params["source"] == nil || params["cwd"] == "" || params["cwd"] == nil {
		t.Fatalf("params %v", params)
	}
}

func TestRunBudgetFlagRidesSessionCreate(t *testing.T) {
	unsetEnv(t, "ICEBATH_YOLO_MODE", "ICEBATH_IGNORE_RULES", "ICEBATH_SAFE_MODE", "ICEBATH_IGNORE_USER_CONFIG", "ICEBATH_ACCEPT_HOOKS")
	if got := creationPolicy(map[string]any{"run_budget": 90})["run_budget"]; got != 90.0 {
		t.Fatalf("run_budget %v", got)
	}
	if _, has := creationPolicy(map[string]any{"run_budget": nil})["run_budget"]; has {
		t.Fatal("unset run budget rode the create")
	}
}

func TestVerboseFlagRidesSessionCreate(t *testing.T) {
	unsetEnv(t, "ICEBATH_YOLO_MODE", "ICEBATH_IGNORE_RULES", "ICEBATH_SAFE_MODE", "ICEBATH_IGNORE_USER_CONFIG", "ICEBATH_ACCEPT_HOOKS")
	if got := creationPolicy(map[string]any{"verbose": true})["verbose"]; got != true {
		t.Fatalf("verbose %v", got)
	}
	if _, has := creationPolicy(map[string]any{"verbose": nil})["verbose"]; has {
		t.Fatal("unset verbose rode the create")
	}
}

func resolved(t *testing.T, path string) string {
	t.Helper()
	real, err := pathutil.Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func mustCreate(t *testing.T, db *GatewaySessionStore, recordCwd bool) {
	t.Helper()
	if _, err := db.Create(context.Background(), map[string]any{"toolsets": []any{"file"}}, "", recordCwd); err != nil {
		t.Fatal(err)
	}
}

func TestASandboxBackendSendsTheHostLaunchDirNotItsTerminalCwd(t *testing.T) {
	for _, backend := range []string{"docker", "ssh"} {
		t.Run(backend, func(t *testing.T) {
			db, client := storeFixture(t)
			tmp := t.TempDir()
			t.Chdir(tmp)
			t.Setenv("TERMINAL_CWD", "/project/not-on-this-host")
			if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte("terminal:\n  backend: "+backend+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			mustCreate(t, db, true)
			wantEqual(t, client.lastOf(t, "session.create")["cwd"], resolved(t, tmp))
			if _, err := createTitledSession(context.Background(), db, "Remote Named"); err != nil {
				t.Fatal(err)
			}
			wantEqual(t, client.lastOf(t, "session.create")["cwd"], resolved(t, tmp))
		})
	}
}

func TestAMountingDockerBackendSendsTheWWorktreeNotTheLaunchDir(t *testing.T) {
	db, client := storeFixture(t)
	repo := filepath.Join(t.TempDir(), "repo")
	tree := filepath.Join(repo, ".worktrees", "wt")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	t.Setenv("TERMINAL_CWD", tree)
	if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte("terminal:\n  backend: docker\n  docker_mount_cwd_to_workspace: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, db, true)
	wantEqual(t, client.lastOf(t, "session.create")["cwd"], resolved(t, tree))
}

func recordWorktree(t *testing.T, tree string) {
	t.Helper()
	previous := RecordedWorktree
	RecordedWorktree = func() map[string]string { return map[string]string{"path": tree} }
	t.Cleanup(func() { RecordedWorktree = previous })
}

func TestARemovedWWorktreeIsSentSoTheGatewayRefusesIt(t *testing.T) {
	for _, tc := range []struct{ backend, mount string }{{"local", ""}, {"docker", "true"}} {
		t.Run(tc.backend, func(t *testing.T) {
			db, client := storeFixture(t)
			repo := filepath.Join(t.TempDir(), "repo")
			if err := os.Mkdir(repo, 0o755); err != nil {
				t.Fatal(err)
			}
			tree := filepath.Join(repo, ".worktrees", "wt")
			t.Chdir(repo)
			recordWorktree(t, tree)
			t.Setenv("TERMINAL_CWD", tree)
			doc := "terminal:\n  backend: " + tc.backend + "\n"
			if tc.mount != "" {
				doc += "  docker_mount_cwd_to_workspace: " + tc.mount + "\n"
			}
			if err := os.WriteFile(filepath.Join(os.Getenv("ICEBATH_HOME"), "config.yaml"), []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			mustCreate(t, db, true)
			wantEqual(t, client.lastOf(t, "session.create")["cwd"], resolved(t, tree))
			if _, err := createTitledSession(context.Background(), db, "Named"); err != nil {
				t.Fatal(err)
			}
			wantEqual(t, client.lastOf(t, "session.create")["cwd"], resolved(t, tree))
		})
	}
}

func frozenLaunch(client *fakeConnClient, launchRequest map[string]any) {
	base := client.rpc
	client.setRPC(func(ctx context.Context, method string, params map[string]any) (any, error) {
		result, err := base(ctx, method, params)
		if err == nil && method == "session.resume" {
			frozen := map[string]any{}
			for k, v := range launchRequest {
				frozen[k] = v
			}
			result.(map[string]any)["info"].(map[string]any)["launch_request"] = frozen
		}
		return result, err
	})
}

func TestExplicitFlagsOnResumeAreCheckedAgainstTheFrozenRoute(t *testing.T) {
	ctx := context.Background()
	db, client := storeFixture(t)
	frozenLaunch(client, map[string]any{"model": "m-frozen", "model_requested": "m-alias", "max_turns": 5, "run_budget": 60.0})
	for _, launch := range []map[string]any{{"max_turns": 1}, {"model": "m-other"}, {"checkpoints": true}, {"run_budget": 30},
		{"verbose": true}, {"skills": []any{"arxiv"}}} {
		if _, err := NewGatewaySessionStore(db.Connection).OpenSession(ctx, storeCLI("local-old", true), launch); err != nil {
			t.Fatalf("launch %v: %v", launch, err)
		}
		sent := client.lastOf(t, "session.mutate")
		payload, _ := sent["payload"].(map[string]any)
		for key, value := range launch {
			if sent["operation"] != "launch" || len(payload) != 1 || (!anyutil.Equal(payload[key], value) && pyFloatOf(payload[key]) != pyFloatOf(value)) {
				t.Fatalf("launch %v relaunched with %v", launch, sent)
			}
		}
	}
	relaunches := len(client.of("session.mutate"))
	for _, launch := range []map[string]any{{}, {"max_turns": 5, "model": "m-alias", "run_budget": 60, "toolsets": []any{"web"}}} {
		sid, err := NewGatewaySessionStore(db.Connection).OpenSession(ctx, storeCLI("local-old", true), launch)
		if err != nil || sid != "local-old" || len(client.of("session.mutate")) != relaunches {
			t.Fatalf("launch %v: %q %v", launch, sid, err)
		}
	}
}

func TestExplicitToolsetsOnResumeAreCheckedAgainstTheFrozenRoute(t *testing.T) {
	ctx := context.Background()
	db, client := storeFixture(t)
	frozenLaunch(client, map[string]any{"toolsets": []any{"file"}})
	host := storeCLI("local-old", true)
	host.TypedToolsets = []any{"web"}
	if _, err := NewGatewaySessionStore(db.Connection).OpenSession(ctx, host, map[string]any{"toolsets": []any{"web"}}); err != nil {
		t.Fatal(err)
	}
	sent := client.lastOf(t, "session.mutate")
	if payload, _ := sent["payload"].(map[string]any); sent["operation"] != "launch" || !anyutil.Equal(payload["toolsets"], []any{"web"}) || len(payload) != 1 {
		t.Fatalf("toolsets relaunched with %v", sent)
	}
	relaunches := len(client.of("session.mutate"))
	host.TypedToolsets = []any{"file"}
	if sid, err := NewGatewaySessionStore(db.Connection).OpenSession(ctx, host, map[string]any{"toolsets": []any{"file"}}); err != nil || sid != "local-old" {
		t.Fatal(sid, err)
	}
	host.TypedToolsets = nil
	if sid, err := NewGatewaySessionStore(db.Connection).OpenSession(ctx, host, map[string]any{"toolsets": []any{"web"}}); err != nil || sid != "local-old" {
		t.Fatal(sid, err)
	}
	if len(client.of("session.mutate")) != relaunches {
		t.Fatal("the frozen toolsets again or no typed toolsets must not relaunch")
	}
}

func TestResumeRelaunchesTheClashingFlags(t *testing.T) {
	ctx := context.Background()
	db, client := storeFixture(t)
	frozenLaunch(client, map[string]any{"model": "m-frozen", "max_turns": 5})
	host := storeCLI("local-old", true)
	for _, launch := range []map[string]any{{"model": "m-other"}, {"max_turns": 1}, {"run_budget": 30}} {
		if _, err := NewGatewaySessionStore(db.Connection).OpenSession(ctx, host, launch); err != nil {
			t.Fatal(err)
		}
		sent := client.lastOf(t, "session.mutate")
		payload, _ := sent["payload"].(map[string]any)
		if sent["operation"] != "launch" || len(payload) != 1 {
			t.Fatalf("launch %v sent %v", launch, sent)
		}
		for key, value := range launch {
			if !anyutil.Equal(payload[key], value) && !(key == "run_budget" && payload[key] == 30.0) {
				t.Fatalf("launch %v payload %v", launch, payload)
			}
		}
	}
	calls := len(client.of("session.mutate"))
	if _, err := NewGatewaySessionStore(db.Connection).OpenSession(ctx, host, map[string]any{"model": "m-frozen", "max_turns": 5}); err != nil {
		t.Fatal(err)
	}
	if len(client.of("session.mutate")) != calls {
		t.Fatal("the frozen flags again must not relaunch")
	}
	overrides, err := ResumeOverrides(ctx, map[string]any{"provider": "anthropic"},
		map[string]any{"info": map[string]any{"launch_request": map[string]any{"provider": "openrouter"}}})
	if err != nil || len(overrides) != 1 || overrides["provider"] != "anthropic" {
		t.Fatalf("overrides %v %v", overrides, err)
	}
}

func TestCliListingsIncludeGatewayRowsCreatedByTheCli(t *testing.T) {
	ctx := context.Background()
	db, _ := storeFixture(t)
	writer, err := state.NewSessionDB(ctx, home.StatePath(home.Dir(ctx)), false)
	if err != nil {
		t.Fatal(err)
	}
	rows := [][3]string{{"local-cli", "local", "cli"}, {"local-tui", "local", "tui"}, {"legacy", "cli", "cli"}, {"one", "oneshot", "oneshot"}}
	for _, row := range rows {
		if err := writer.CreateSession(state.NewSession{ID: row[0], Source: row[1]}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.AppendMessage(ctx, row[0], map[string]any{"role": "user", "content": "hi " + row[0]}, state.AppendGuards{}); err != nil {
			t.Fatal(err)
		}
	}
	err = writer.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		for _, row := range rows {
			if _, err := conn.ExecContext(ctx, "UPDATE sessions SET created_source=? WHERE id=?", row[2], row[0]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	t.Cleanup(func() { db.CloseReader() })
	sortedIDs := func(ids []string) []string {
		sort.Strings(ids)
		return ids
	}
	listed, err := state.QuerySessionListing(ctx, db, state.SessionListingQuery{Source: "cli", IncludeUnnamed: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var listedIDs []string
	for _, row := range listed {
		listedIDs = append(listedIDs, row.Value("id").(string))
	}
	wantIDs(t, sortedIDs(listedIDs), "legacy", "local-cli")
	found, err := db.SearchSessions(ctx, SearchSessionsOptions{Sources: []string{"cli", "oneshot"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var foundIDs []string
	for _, row := range found {
		foundIDs = append(foundIDs, row["id"].(string))
	}
	wantIDs(t, sortedIDs(foundIDs), "legacy", "local-cli", "one")
	tui, err := db.SearchSessions(ctx, SearchSessionsOptions{Sources: []string{"tui"}, Limit: 10})
	if err != nil || len(tui) != 1 || tui[0]["id"] != "local-tui" {
		t.Fatal(tui, err)
	}
	ten := 10
	all, err := db.ListSessionsRich(ctx, state.ListSessionsRichOptions{Limit: &ten})
	if err != nil || len(all) != 4 {
		t.Fatal(len(all), err)
	}
}

func TestOpenSessionRecordsTheLaunchKeyAndWhetherItWasTyped(t *testing.T) {
	db, client := storeFixture(t)
	host := storeCLI("", false)
	host.TypedAPIKey = "typed-key"
	mustOpen(t, db, host, map[string]any{"api_key": "typed-key", "base_url": "http://127.0.0.1:9/v1"})
	if key, typed := db.Connection.LaunchKey(); key != "typed-key" || !typed {
		t.Fatalf("key %q typed %v", key, typed)
	}
	wantEqual(t, client.lastOf(t, "session.create")["api_key"], "typed-key")
	host.TypedAPIKey = nil
	mustOpen(t, db, host, map[string]any{"api_key": "alias-key", "base_url": "http://127.0.0.1:9/v1"})
	if key, typed := db.Connection.LaunchKey(); key != "alias-key" || typed {
		t.Fatalf("key %q typed %v", key, typed)
	}
	resumed := storeCLI("local-old", true)
	if sid := mustOpen(t, NewGatewaySessionStore(db.Connection), resumed, map[string]any{"api_key": "alias-key"}); sid != "local-old" {
		t.Fatalf("sid %q", sid)
	}
	wantEqual(t, client.lastOf(t, "session.resume"), map[string]any{"session_id": "local-old", "api_key": "alias-key"})
	mustOpen(t, db, storeCLI("", false), map[string]any{})
	if key, typed := db.Connection.LaunchKey(); key != "" || typed {
		t.Fatalf("key %q typed %v", key, typed)
	}
}
