package gateway

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
)

// Compression chain length one walk of the lineage covers, hermes_state_compression _CHAIN_CAP
const recoverChainCap = 1000

func recoverTUIParams(cwd string) map[string]any {
	return map[string]any{"request_id": "r", "source": "tui", "cwd": cwd, "model": "m", "toolsets": []any{}}
}

// Another owner of the same store on a fresh epoch, its boot recovery not yet run
func recoverColdOwner(t *testing.T, ctx context.Context, db *state.SessionDB, instanceID string) *SessionAuthority {
	t.Helper()
	epoch, err := db.BeginRuntimeEpoch(ctx, instanceID)
	if err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(ctx, filepath.Join(t.TempDir(), "sessions"), DefaultGatewayConfig(ctx), nil)
	store.PinDB(db)
	key, err := pypath.Resolve(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	store.bindLocalAuthorityEpoch(key, epoch, true)
	runner := &recoverTestRunner{store: store, adapters: pydict.New[Platform, PlatformAdapter](), profiles: pydict.New[string, *pydict.Dict[Platform, PlatformAdapter]]()}
	a := NewSessionAuthority(runner, "fixture", instanceID, db, epoch)
	runner.launch = a
	return a
}

// Another owner of the same store after the boot recovery initialize_session_authority runs
func recoverSecondOwner(t *testing.T, ctx context.Context, db *state.SessionDB) *SessionAuthority {
	t.Helper()
	a := recoverColdOwner(t, ctx, db, "second")
	if err := adoptLocalCompressions(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := recoverLocalSessions(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	return a
}

// One compaction step as 0.22.2 wrote it, the parent ended and the child copying its route columns
func compactStepLike0222(ctx context.Context, conn state.SQLiteConn, parentID, stepID, childID string, routeColumns bool) error {
	var model, cwd, profile, user, key, chat, chatType, origin any
	if err := conn.QueryRowContext(ctx, "SELECT model,cwd,profile_name,user_id,session_key,chat_id,chat_type,origin_json FROM sessions WHERE id=?",
		parentID).Scan(&model, &cwd, &profile, &user, &key, &chat, &chatType, &origin); err != nil {
		return err
	}
	if !routeColumns {
		user, key, chat, chatType, origin = nil, nil, nil, nil, nil
	}
	now := float64(time.Now().UnixNano()) / 1e9
	if _, err := conn.ExecContext(ctx, "UPDATE sessions SET ended_at=?, end_reason='compression' WHERE id=?", now, stepID); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, `INSERT INTO sessions(id,source,model,parent_session_id,cwd,profile_name,
            user_id,session_key,chat_id,chat_type,origin_json,started_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		childID, "tui", model, stepID, cwd, profile, user, key, chat, chatType, origin, now)
	return err
}

func compactLike0222(t *testing.T, ctx context.Context, db *state.SessionDB, parentID, childID string, routeColumns bool) {
	t.Helper()
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		return compactStepLike0222(ctx, conn, parentID, parentID, childID, routeColumns)
	}); err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{"role": "user", "content": "after compression into " + childID}
	if _, err := db.AppendMessage(ctx, childID, fields, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

func compactChainLike0222(t *testing.T, ctx context.Context, db *state.SessionDB, parentID string, childIDs []string) {
	t.Helper()
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		step := parentID
		for _, child := range childIDs {
			if err := compactStepLike0222(ctx, conn, parentID, step, child, true); err != nil {
				return err
			}
			step = child
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func recoverReceipt(t *testing.T, ctx context.Context, db *state.SessionDB, sid string) *pyjson.Object {
	t.Helper()
	receipt, err := db.LocalReceipt(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func recoverPlain(t *testing.T, v any) any {
	t.Helper()
	text, err := pyjson.Dumps(v)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		t.Fatal(err)
	}
	return pyjson.Plain(decoded)
}

func recoverNested(t *testing.T, receipt *pyjson.Object, keys ...string) any {
	t.Helper()
	var value any = receipt
	for _, key := range keys {
		object, ok := value.(*pyjson.Object)
		if !ok {
			t.Fatalf("%v is not an object at %s", keys, key)
		}
		value, _ = object.Get(key)
	}
	return value
}

func recoverLive(a *SessionAuthority, sid string) bool {
	_, ok := a.live(sid)
	return ok
}

func recoverRefused(t *testing.T, err error, reason string) {
	t.Helper()
	var refusal *state.RuntimeStoreError
	if !errors.As(err, &refusal) || refusal.Reason != reason {
		t.Fatalf("error %v, want refusal %s", err, reason)
	}
}

func TestRestoringACurrentLiveRouteDoesNotWaitOnTheStoreLock(t *testing.T) {
	ctx, a, runner := newRecoverAuthority(t)
	actor := Principal{Subject: "human", ProfileID: a.ProfileID, TransportID: "sock", Capabilities: map[string]bool{"session:create": true}}
	ref, _ := seedRecoverSession(t, ctx, a, actor, map[string]any{"request_id": "r", "source": "cli", "cwd": t.TempDir(), "model": "m", "toolsets": []any{}})
	if _, err := restoreLocalSession(ctx, a, ref.SessionID); err != nil {
		t.Fatal(err)
	}
	if !recoverLive(a, ref.SessionID) {
		t.Fatal("the session is not live")
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		runner.store.mu.Lock()
		defer runner.store.mu.Unlock()
		close(held)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}()
	defer func() {
		close(release)
		<-done
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the writer never took the store lock")
	}
	started := time.Now()
	type outcome struct {
		ref SessionRef
		err error
	}
	result := make(chan outcome, 1)
	go func() {
		restored, err := restoreLocalSession(ctx, a, ref.SessionID)
		result <- outcome{restored, err}
	}()
	select {
	case got := <-result:
		if got.err != nil || got.ref.SessionID != ref.SessionID {
			t.Fatalf("restored %v, %v", got.ref, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the restore waited on the store lock")
	}
	if time.Since(started) >= 2*time.Second {
		t.Fatalf("the restore took %s", time.Since(started))
	}
}

func TestASessionCompactedBy0222ResumesOnItsCompressedTip(t *testing.T) {
	ctx, first, _ := newRecoverAuthority(t)
	ref, _ := seedRecoverSession(t, ctx, first, recoverActor(), recoverTUIParams(t.TempDir()))
	if _, err := first.DB.AppendMessage(ctx, ref.SessionID, map[string]any{"role": "user", "content": "before the downgrade"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	compactLike0222(t, ctx, first.DB, ref.SessionID, "classic-child-1", true)
	compactLike0222(t, ctx, first.DB, "classic-child-1", "classic-child-2", true)
	second := recoverSecondOwner(t, ctx, first.DB)
	if !recoverLive(second, ref.SessionID) {
		t.Fatal("the receipt was paused on cold recovery")
	}
	receipt := recoverReceipt(t, ctx, second.DB, ref.SessionID)
	if lineage, _ := receipt.Get("lineage"); !reflect.DeepEqual(lineage, []any{ref.SessionID, "classic-child-1", "classic-child-2"}) {
		t.Fatalf("lineage %v", lineage)
	}
	if target := recoverNested(t, receipt, "entry", "session_id"); target != "classic-child-2" {
		t.Fatalf("entry target %v", target)
	}
	rows, err := localHistory(ctx, second, ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if last := rows[len(rows)-1]["content"]; last != "after compression into classic-child-2" {
		t.Fatalf("last row %v", last)
	}
}

func TestAChildWithoutTheReceiptsRouteIsNotAdopted(t *testing.T) {
	ctx, first, _ := newRecoverAuthority(t)
	ref, _ := seedRecoverSession(t, ctx, first, recoverActor(), recoverTUIParams(t.TempDir()))
	before := recoverPlain(t, recoverReceipt(t, ctx, first.DB, ref.SessionID))
	compactLike0222(t, ctx, first.DB, ref.SessionID, "unrouted-child", false)
	second := recoverSecondOwner(t, ctx, first.DB)
	if recoverLive(second, ref.SessionID) {
		t.Fatal("the unrouted child was adopted")
	}
	_, err := restoreLocalSession(ctx, second, ref.SessionID)
	recoverRefused(t, err, "storage_unavailable")
	if after := recoverPlain(t, recoverReceipt(t, ctx, second.DB, ref.SessionID)); !reflect.DeepEqual(after, before) {
		t.Fatalf("receipt changed to %v", after)
	}
}

func TestACompactedReceiptWithAForeignOriginStaysRefused(t *testing.T) {
	ctx, first, _ := newRecoverAuthority(t)
	ref, _ := seedRecoverSession(t, ctx, first, recoverActor(), recoverTUIParams(t.TempDir()))
	compactLike0222(t, ctx, first.DB, ref.SessionID, "classic-child", true)
	receipt := recoverReceipt(t, ctx, first.DB, ref.SessionID)
	recoverNested(t, receipt, "entry", "origin").(*pyjson.Object).Set("profile", "other-profile")
	text, err := pyjson.Dumps(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", text, state.LocalPolicyPrefix+ref.SessionID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	second := recoverSecondOwner(t, ctx, first.DB)
	if recoverLive(second, ref.SessionID) {
		t.Fatal("a foreign origin was replaced by the catch-up")
	}
	if after := recoverPlain(t, recoverReceipt(t, ctx, second.DB, ref.SessionID)); !reflect.DeepEqual(after, recoverPlain(t, receipt)) {
		t.Fatalf("receipt changed to %v", after)
	}
}

func TestACompressionChainLongerThanOneWalkIsAdoptedWhole(t *testing.T) {
	ctx, first, _ := newRecoverAuthority(t)
	ref, _ := seedRecoverSession(t, ctx, first, recoverActor(), recoverTUIParams(t.TempDir()))
	children := make([]string, recoverChainCap+1)
	want := []any{ref.SessionID}
	for n := range children {
		children[n] = fmt.Sprintf("classic-child-%d", n)
		want = append(want, children[n])
	}
	compactChainLike0222(t, ctx, first.DB, ref.SessionID, children)
	second := recoverSecondOwner(t, ctx, first.DB)
	if !recoverLive(second, ref.SessionID) {
		t.Fatal("the long chain was paused on cold recovery")
	}
	receipt := recoverReceipt(t, ctx, second.DB, ref.SessionID)
	if lineage, _ := receipt.Get("lineage"); !reflect.DeepEqual(lineage, want) {
		t.Fatalf("lineage holds %d ids", len(lineage.([]any)))
	}
	if target := recoverNested(t, receipt, "entry", "session_id"); target != children[len(children)-1] {
		t.Fatalf("entry target %v", target)
	}
}

func TestAChainThatLoopsBackPastOneWalkIsNotAdopted(t *testing.T) {
	ctx, first, _ := newRecoverAuthority(t)
	ref, _ := seedRecoverSession(t, ctx, first, recoverActor(), recoverTUIParams(t.TempDir()))
	before := recoverPlain(t, recoverReceipt(t, ctx, first.DB, ref.SessionID))
	children := make([]string, recoverChainCap+1)
	for n := range children {
		children[n] = fmt.Sprintf("classic-child-%d", n)
	}
	compactChainLike0222(t, ctx, first.DB, ref.SessionID, children)
	last := children[len(children)-1]
	if err := first.DB.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		if _, err := conn.ExecContext(ctx, "UPDATE sessions SET end_reason='compression' WHERE id=?", last); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, "UPDATE sessions SET parent_session_id=? WHERE id=?", last, ref.SessionID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	second := recoverSecondOwner(t, ctx, first.DB)
	if recoverLive(second, ref.SessionID) {
		t.Fatal("a looping chain was adopted")
	}
	if after := recoverPlain(t, recoverReceipt(t, ctx, second.DB, ref.SessionID)); !reflect.DeepEqual(after, before) {
		t.Fatalf("receipt changed to %v", after)
	}
}

func TestARestoreOnTheOwnerLoopHandsTheCatchUpToAWorker(t *testing.T) {
	ctx, first, _ := newRecoverAuthority(t)
	ref, _ := seedRecoverSession(t, ctx, first, recoverActor(), recoverTUIParams(t.TempDir()))
	compactLike0222(t, ctx, first.DB, ref.SessionID, "classic-child", true)
	cold := recoverColdOwner(t, ctx, first.DB, "cold")
	_, err := restoreLocalSession(withOwnerLoop(ctx), cold, ref.SessionID)
	recoverRefused(t, err, "storage_unavailable")
	var pending *catchUpPending
	if !errors.As(err, &pending) {
		t.Fatalf("the restore refused with %T, the catch-up ran on the owner loop", err)
	}
	cold.localAdoptionsMu.Lock()
	worker := cold.localAdoptions[ref.SessionID]
	cold.localAdoptionsMu.Unlock()
	if worker == nil {
		t.Fatal("no tracked worker took the catch-up")
	}
	if _, err := worker.wait(); err != nil {
		t.Fatal(err)
	}
	for _, task := range mutationTasks(cold) {
		if _, err := task.wait(); err != nil {
			t.Fatal(err)
		}
	}
	if target := recoverNested(t, recoverReceipt(t, ctx, cold.DB, ref.SessionID), "entry", "session_id"); target != "classic-child" {
		t.Fatalf("entry target %v", target)
	}
	if !recoverLive(cold, ref.SessionID) {
		t.Fatal("the committed catch-up was not published")
	}
}

// Registry naming one served secondary profile beside the authorities it holds
type recoverRegistry struct {
	names map[*SessionAuthority]string
	all   []*SessionAuthority
}

func (r *recoverRegistry) ProfileName(a *SessionAuthority) (string, bool) {
	name, served := r.names[a]
	return name, served
}
func (r *recoverRegistry) Authorities() []*SessionAuthority { return r.all }
func (r *recoverRegistry) Active() *SessionAuthority        { return r.all[0] }
func (r *recoverRegistry) Serving() []*SessionAuthority     { return r.all }
func (r *recoverRegistry) LaunchKey() string                { return "" }
func (r *recoverRegistry) ForHome(string) *SessionAuthority { return nil }

// Marks the authority as the served secondary profile of its runner, as a multiplex gateway builds one
func recoverServe(a *SessionAuthority) *SessionAuthority {
	a.Runner.(*recoverTestRunner).registry = &recoverRegistry{names: map[*SessionAuthority]string{a: "secondary"}, all: []*SessionAuthority{a}}
	return a
}

func recoverGUIParams(requestID, cwd string) map[string]any {
	return map[string]any{"request_id": requestID, "source": "gui", "cwd": cwd, "model": "frozen", "toolsets": []any{}}
}

func recoverStoreReceipt(t *testing.T, ctx context.Context, db *state.SessionDB, sid string, mutate func(stored string) string) {
	t.Helper()
	stored, found, err := db.GetMeta(ctx, state.LocalPolicyPrefix+sid)
	if err != nil || !found {
		t.Fatalf("stored receipt: %v %v", found, err)
	}
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", mutate(stored), state.LocalPolicyPrefix+sid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Receipt text mutation that sets the nested field at path to value
func recoverWithField(t *testing.T, value any, path ...string) func(string) string {
	return func(stored string) string {
		receipt, err := loadsObject(stored)
		if err != nil {
			t.Fatal(err)
		}
		node := receipt
		for _, key := range path[:len(path)-1] {
			next, _ := node.Get(key)
			node = next.(*pyjson.Object)
		}
		node.Set(path[len(path)-1], value)
		text, err := pyjson.Dumps(receipt)
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
}

func recoverLogLines(logs string, sid string) []string {
	var lines []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, sid) {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestLiveSessionRebuildsARouteEntryMissingFromTheStore(t *testing.T) {
	ctx, a, runner := newRecoverAuthority(t)
	served := recoverServe(a)
	actor := Principal{Subject: "owner", ProfileID: served.ProfileID, TransportID: "socket",
		Capabilities: map[string]bool{"session:create": true, "session:read": true}}
	ref, _ := seedRecoverSession(t, ctx, served, actor, recoverGUIParams("live", t.TempDir()))
	live, _ := served.live(ref.SessionID)
	route := live.Route
	if live.Source.Profile == nil || *live.Source.Profile != "secondary" {
		t.Fatalf("served source profile %v", live.Source.Profile)
	}
	store := runner.store
	store.mu.Lock()
	store.entries.delete(route)
	store.mu.Unlock()
	restored, err := restoreLocalSession(ctx, served, ref.SessionID)
	if err != nil || restored != ref {
		t.Fatalf("restored %v %v", restored, err)
	}
	target, err := receiptEntryTarget(ctx, served.DB, ref.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if got := apiRouteTarget(served, route); got != target {
		t.Fatalf("rebuilt route names %q, receipt %q", got, target)
	}
}

func TestARefusedReceiptNamesItsFailingCheckInTheLog(t *testing.T) {
	terminal, err := pyjson.Dumps(pyjson.ObjectOf("TERMINAL_ENV", "local", "TERMINAL_CWD", "/elsewhere"))
	if err != nil {
		t.Fatal(err)
	}
	damages := []struct {
		name, clause string
		apply        func(t *testing.T, ctx context.Context, db *state.SessionDB, sid string)
	}{
		{"foreign_origin", "entry mismatch", func(t *testing.T, ctx context.Context, db *state.SessionDB, sid string) {
			recoverStoreReceipt(t, ctx, db, sid, recoverWithField(t, "other-profile", "entry", "origin", "profile"))
		}},
		{"lineage", "ValidateLocalLineage", func(t *testing.T, ctx context.Context, db *state.SessionDB, sid string) {
			recoverStoreReceipt(t, ctx, db, sid, recoverWithField(t, []any{sid, "gone"}, "lineage"))
		}},
		{"terminal_policy", "ValueError: terminal policy mismatch", func(t *testing.T, ctx context.Context, db *state.SessionDB, sid string) {
			recoverStoreReceipt(t, ctx, db, sid, recoverWithField(t, terminal, "policy", "terminal_json"))
		}},
		{"malformed", "JSONDecodeError", func(t *testing.T, ctx context.Context, db *state.SessionDB, sid string) {
			recoverStoreReceipt(t, ctx, db, sid, func(stored string) string { return stored[:len(stored)-1] })
		}},
	}
	for _, damage := range damages {
		t.Run(damage.name, func(t *testing.T) {
			ctx, a, _ := newRecoverAuthority(t)
			served := recoverServe(a)
			actor := Principal{Subject: "owner", ProfileID: served.ProfileID, TransportID: "socket",
				Capabilities: map[string]bool{"session:create": true, "session:read": true}}
			ref, _ := seedRecoverSession(t, ctx, served, actor, recoverGUIParams(damage.name, t.TempDir()))
			damage.apply(t, ctx, served.DB, ref.SessionID)
			logs := captureLogs(t)
			cold := recoverServe(recoverColdOwner(t, ctx, served.DB, "second"))
			if err := adoptLocalCompressions(ctx, cold); err != nil {
				t.Fatal(err)
			}
			if err := recoverLocalSessions(ctx, cold, false); err != nil {
				t.Fatal(err)
			}
			if recoverLive(cold, ref.SessionID) {
				t.Fatal("the damaged receipt was restored")
			}
			recovery := recoverLogLines(logs.String(), ref.SessionID)
			if len(recovery) != 1 || !strings.Contains(recovery[0], damage.clause) {
				t.Fatalf("one boot warning per refused receipt naming %q: %v", damage.clause, recovery)
			}
			logs.Reset()
			_, err := restoreLocalSession(ctx, cold, ref.SessionID)
			recoverRefused(t, err, "storage_unavailable")
			resumed := recoverLogLines(logs.String(), ref.SessionID)
			if len(resumed) == 0 || !strings.Contains(resumed[0], damage.clause) {
				t.Fatalf("the later restore did not name %q: %v", damage.clause, resumed)
			}
		})
	}
}

// Served secondary authority named alpha on a multiplexing store over db, with a separate launch store beside it
func servedAlphaOwner(t *testing.T, ctx context.Context, db *state.SessionDB, instanceID string) *SessionAuthority {
	t.Helper()
	installPrefillLoaders(t)
	epoch, err := db.BeginRuntimeEpoch(ctx, instanceID)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultGatewayConfig(ctx)
	multiplex := true
	cfg.MultiplexProfiles = &multiplex
	store := NewSessionStore(ctx, filepath.Join(t.TempDir(), "sessions"), cfg, nil)
	store.PinDB(db)
	key, err := pypath.Resolve(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	store.bindLocalAuthorityEpoch(key, epoch, true)
	runner := &recoverTestRunner{store: store, adapters: pydict.New[Platform, PlatformAdapter](), profiles: pydict.New[string, *pydict.Dict[Platform, PlatformAdapter]]()}
	a := NewSessionAuthority(runner, filepath.Dir(db.Path()), instanceID, db, epoch)
	runner.registry = &recoverRegistry{names: map[*SessionAuthority]string{a: "alpha"}, all: []*SessionAuthority{a}}
	return a
}

func servedAlphaBoot(t *testing.T, ctx context.Context, db *state.SessionDB) *SessionAuthority {
	t.Helper()
	a := servedAlphaOwner(t, ctx, db, "second")
	if err := adoptLocalCompressions(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := recoverLocalSessions(ctx, a, false); err != nil {
		t.Fatal(err)
	}
	return a
}

func servedAlphaSession(t *testing.T) (context.Context, *SessionAuthority, SessionRef, *state.SessionDB) {
	t.Helper()
	ctx, db := newTestStateDB(t)
	launch, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launch.Close() })
	first := servedAlphaOwner(t, ctx, db, "first")
	actor := Principal{Subject: "owner", ProfileID: first.ProfileID, TransportID: "socket",
		Capabilities: map[string]bool{"session:create": true, "session:read": true, "session:control": true}}
	ref, err := createLocalSession(ctx, first, actor, recoverGUIParams("old-desktop", t.TempDir()), localCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, first, ref, launch
}

func servedAlphaStore(t *testing.T, ctx context.Context, db *state.SessionDB, sid string, receipt *pyjson.Object) {
	t.Helper()
	text, err := pyjson.Dumps(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ExecuteWrite(ctx, func(conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE state_meta SET value=? WHERE key=?", text, state.LocalPolicyPrefix+sid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSecondaryReceiptRestoresWithoutLosingItsOwner(t *testing.T) {
	origin := func(t *testing.T, receipt *pyjson.Object) *pyjson.Object {
		return recoverNested(t, receipt, "entry", "origin").(*pyjson.Object)
	}
	aged := map[string]func(t *testing.T, receipt *pyjson.Object){
		"missing": func(t *testing.T, receipt *pyjson.Object) {},
		"null":    func(t *testing.T, receipt *pyjson.Object) { origin(t, receipt).Set("profile", nil) },
		"foreign": func(t *testing.T, receipt *pyjson.Object) { origin(t, receipt).Set("profile", "beta") },
		"route": func(t *testing.T, receipt *pyjson.Object) {
			route, _ := receipt.Get("route")
			receipt.Set("route", strings.Replace(route.(string), "agent:alpha:", "agent:main:", 1))
		},
		"home": func(t *testing.T, receipt *pyjson.Object) { receipt.Set("profile_id", t.TempDir()) },
		"user": func(t *testing.T, receipt *pyjson.Object) { origin(t, receipt).Set("user_id", "another") },
	}
	for _, variant := range []string{"current", "missing", "null", "foreign", "route", "home", "user"} {
		t.Run(variant, func(t *testing.T) {
			ctx, first, ref, launch := servedAlphaSession(t)
			receipt := recoverReceipt(t, ctx, first.DB, ref.SessionID)
			originalRoute, _ := receipt.Get("route")
			if !strings.HasPrefix(originalRoute.(string), "agent:alpha:") {
				t.Fatalf("served route %v", originalRoute)
			}
			if variant != "current" {
				origin(t, receipt).Delete("profile")
				aged[variant](t, receipt)
				servedAlphaStore(t, ctx, first.DB, ref.SessionID, receipt)
			}
			restored := servedAlphaBoot(t, ctx, first.DB)
			absent := func() {
				t.Helper()
				row, err := launch.GetSession(ctx, ref.SessionID)
				if err != nil || row != nil {
					t.Fatalf("the launch store holds the session: %v %v", row, err)
				}
			}
			if variant != "current" && variant != "missing" {
				if recoverLive(restored, ref.SessionID) {
					t.Fatal("an aged receipt of another owner was restored")
				}
				_, err := restoreLocalSession(ctx, restored, ref.SessionID)
				var refusal *state.RuntimeStoreError
				if !errors.As(err, &refusal) {
					t.Fatalf("restore answered %v", err)
				}
				if after := recoverPlain(t, recoverReceipt(t, ctx, restored.DB, ref.SessionID)); !reflect.DeepEqual(after, recoverPlain(t, receipt)) {
					t.Fatalf("receipt changed to %v", after)
				}
				absent()
				return
			}
			if !recoverLive(restored, ref.SessionID) {
				t.Fatal("owned legacy receipt was paused on cold recovery")
			}
			again, err := restoreLocalSession(ctx, restored, ref.SessionID)
			if err != nil || again != ref {
				t.Fatalf("restored %v %v", again, err)
			}
			live, _ := restored.live(ref.SessionID)
			if live.Source.Profile == nil || *live.Source.Profile != "alpha" || live.Route != originalRoute {
				t.Fatalf("live profile %v route %q", live.Source.Profile, live.Route)
			}
			absent()
		})
	}
}

func TestACompactedSecondaryReceiptMissingItsOriginProfileRestores(t *testing.T) {
	ctx, first, ref, _ := servedAlphaSession(t)
	receipt := recoverReceipt(t, ctx, first.DB, ref.SessionID)
	recoverNested(t, receipt, "entry", "origin").(*pyjson.Object).Delete("profile")
	servedAlphaStore(t, ctx, first.DB, ref.SessionID, receipt)
	compactLike0222(t, ctx, first.DB, ref.SessionID, "classic-child", true)
	restored := servedAlphaBoot(t, ctx, first.DB)
	if !recoverLive(restored, ref.SessionID) {
		t.Fatal("the compacted receipt was paused on cold recovery")
	}
	live, _ := restored.live(ref.SessionID)
	if live.Source.Profile == nil || *live.Source.Profile != "alpha" {
		t.Fatalf("live profile %v", live.Source.Profile)
	}
	after := recoverReceipt(t, ctx, restored.DB, ref.SessionID)
	if lineage, _ := after.Get("lineage"); !reflect.DeepEqual(lineage, []any{ref.SessionID, "classic-child"}) {
		t.Fatalf("lineage %v", lineage)
	}
	want := recoverNested(t, receipt, "entry").(*pyjson.Object)
	want.Set("session_id", "classic-child")
	if entry, _ := after.Get("entry"); !reflect.DeepEqual(recoverPlain(t, entry), recoverPlain(t, want)) {
		t.Fatalf("entry %v", recoverPlain(t, entry))
	}
}
