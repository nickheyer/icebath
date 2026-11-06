package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

type fakeRead struct {
	id      string
	timeout time.Duration
}

// Scripted gateway, flips name the account read that first answers active and rows script each read
type gatewayFake struct {
	mu               sync.Mutex
	connected        map[string]bool
	flips            map[string]int
	rows             map[string][]any
	mintStatus       string
	mintOverrides    map[string]string
	statusReason     string
	noConnectionID   bool
	lists            int
	mints            []map[string]any
	reads            []fakeRead
	slugOf           map[string]string
	listed           []string
	instruction      string
	afterAccountRead func(id string, row map[string]any) map[string]any
}

func newGatewayFake() *gatewayFake {
	return &gatewayFake{connected: map[string]bool{}, flips: map[string]int{}, rows: map[string][]any{}, mintStatus: "initiated",
		mintOverrides: map[string]string{}, slugOf: map[string]string{}, listed: []string{"gmail", "notion"}}
}

func (g *gatewayFake) ListConnectors(ctx context.Context, timeout time.Duration) ([]map[string]any, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lists++
	var out []map[string]any
	for _, s := range g.listed {
		out = append(out, map[string]any{"connector": s, "enabled": true, "connected": g.connected[s]})
	}
	return out, nil
}

func (g *gatewayFake) Connections(ctx context.Context, req ConnectionsRequest) (map[string]any, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mints = append(g.mints, map[string]any{"connectors": strings.Join(req.Connectors, ","), "reinitiate": req.Reinitiate,
		"alias": req.Alias, "return_to": req.ReturnTo, "op": req.Op})
	var results []any
	for _, slug := range req.Connectors {
		status := g.mintStatus
		if s, ok := g.mintOverrides[slug]; ok {
			status = s
		}
		row := map[string]any{"connector": slug, "status": status, "reinitiated": req.Reinitiate}
		if status == "initiated" {
			row["connect_url"] = fmt.Sprintf("https://connect.example/%s/%d", slug, len(g.mints))
		}
		if !g.noConnectionID && (status == "initiated" || status == "active") {
			id := fmt.Sprintf("ca_%s_%d", slug, len(g.mints))
			g.slugOf[id] = slug
			row["connection_id"] = id
		}
		if g.statusReason != "" {
			row["status_reason"] = g.statusReason
		}
		if g.instruction != "" {
			row["instruction"] = g.instruction
		}
		results = append(results, row)
	}
	return map[string]any{"results": results, "summary": map[string]any{"total": len(req.Connectors)}}, nil
}

func (g *gatewayFake) AccountStatus(ctx context.Context, id string, timeout time.Duration) (map[string]any, error) {
	g.mu.Lock()
	g.reads = append(g.reads, fakeRead{id, timeout})
	slug := g.slugOf[id]
	nth := 0
	for _, r := range g.reads {
		if r.id == id {
			nth++
		}
	}
	status := g.statusOf(slug, nth)
	hook := g.afterAccountRead
	g.mu.Unlock()
	if err, ok := status.(error); ok {
		return nil, err
	}
	if status == nil {
		return nil, nil
	}
	row := map[string]any{"connectionId": id, "connector": slug, "status": status, "statusReason": g.statusReason,
		"label": slug + "_a", "active": status == "active", "createdAt": "2026-09-14T10:00:00.000Z", "updatedAt": "2026-09-14T10:00:00.000Z"}
	if hook != nil {
		row = hook(id, row)
	}
	return row, nil
}

func (g *gatewayFake) statusOf(slug string, nth int) any {
	if script := g.rows[slug]; len(script) > 0 {
		return script[min(nth, len(script))-1]
	}
	if flip, ok := g.flips[slug]; ok && nth >= flip {
		return "active"
	}
	if g.connected[slug] && len(g.flips) == 0 {
		return "active"
	}
	return "pending"
}

func (g *gatewayFake) readIDs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.reads))
	for i, r := range g.reads {
		out[i] = r.id
	}
	return out
}

// Card callback recording each payload and answering at once
type desktopCard struct {
	mu   sync.Mutex
	seen []map[string]any
}

func (c *desktopCard) cb(payload *pyjson.Object) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(connDumps(payload)), &m)
	c.mu.Lock()
	c.seen = append(c.seen, m)
	c.mu.Unlock()
	return ""
}

// Resets the live registry and the provider hooks, catalog and hosted seams for one managed test
func managedTestSetup(t *testing.T) *Env {
	LiveResetForTests()
	oldCat, oldHosted, oldNote, oldSeen, oldTick, oldDeadline := managedCatalogNames, managedHostedNames, NoteAppConnected, NoteConnectedAppsSeen, ManagedWatchTickSeconds, OperationDeadlineSeconds
	managedCatalogNames = func() map[string]bool { return map[string]bool{} }
	managedHostedNames = func(context.Context, *Env) (map[string]bool, bool) { return nil, false }
	NoteAppConnected = func(context.Context, *Env) {}
	NoteConnectedAppsSeen = func(context.Context, *Env, []any) {}
	t.Cleanup(func() {
		LiveResetForTests()
		managedCatalogNames, managedHostedNames, NoteAppConnected, NoteConnectedAppsSeen = oldCat, oldHosted, oldNote, oldSeen
		ManagedWatchTickSeconds, OperationDeadlineSeconds = oldTick, oldDeadline
	})
	return &Env{Home: t.TempDir()}
}

func patchCatalog(t *testing.T, catalog []string, hosted []string, hostedKnown bool) {
	managedCatalogNames = func() map[string]bool { return connSet(catalog) }
	managedHostedNames = func(context.Context, *Env) (map[string]bool, bool) { return connSet(hosted), hostedKnown }
}

func connSet(names []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[n] = true
	}
	return out
}

func runManaged(t *testing.T, ctx context.Context, env *Env, args map[string]any, gw ManagedClient, cb ConnCallback, tick float64, platform string) map[string]any {
	t.Setenv("ICEBATH_SESSION_PLATFORM", platform)
	ManagedWatchTickSeconds = tick
	raw := ManageConnections(ctx, env, args, ManageOptions{ClientFactory: func() (ManagedClient, error) { return gw, nil }, Callback: cb, SessionID: "s1"})
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("result %q: %v", raw, err)
	}
	return out
}

func connectArgs(action string, names ...any) map[string]any {
	return map[string]any{"action": action, "connectors": names}
}

func targetsOf(out map[string]any) map[string]map[string]any {
	by := map[string]map[string]any{}
	for _, raw := range out["targets"].([]any) {
		t := raw.(map[string]any)
		by[t["name"].(string)] = t
	}
	return by
}

func TestManagedDesktopConnectMintsOnceEmitsTheCardAndReturnsOutcomesWithoutURLs(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	gw.flips = map[string]int{"gmail": 2, "notion": 3}
	card := &desktopCard{}
	out := runManaged(t, context.Background(), env, connectArgs("connect", "gmail", "notion"), gw, card.cb, 0, "desktop")
	if len(gw.mints) != 1 || gw.mints[0]["connectors"] != "gmail,notion" {
		t.Fatalf("mints %v", gw.mints)
	}
	if len(card.seen) != 1 || card.seen[0]["op_id"] != out["op_id"] {
		t.Fatalf("card %v out %v", card.seen, out)
	}
	for _, raw := range card.seen[0]["targets"].([]any) {
		row := raw.(map[string]any)
		if row["kind"] != "connector" || row["action"] != "connect" {
			t.Fatalf("payload row %v", row)
		}
	}
	if out["status"] != "settled" || out["settled_by"] != "all_resolved" {
		t.Fatalf("out %v", out)
	}
	for _, row := range targetsOf(out) {
		if row["state"] != "connected" {
			t.Fatalf("row %v", row)
		}
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "connect_url") || LiveCurrent(OperationSessionKey(env, "s1"), "") != nil {
		t.Fatalf("url leaked or op left open: %s", raw)
	}
}

func TestManagedDesktopConnectURLStaysOnTheLiveOperationForThePanel(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	gw.flips = map[string]int{"gmail": 2}
	var captured *ConnectionOperation
	cb := func(payload *pyjson.Object) string {
		id, _ := payload.Get("op_id")
		captured = LiveGet(OperationSessionKey(env, "s1"), id.(string), "")
		return ""
	}
	runManaged(t, context.Background(), env, connectArgs("connect", "gmail"), gw, cb, 0, "desktop")
	targets, _ := captured.Result(context.Background(), env, true).Get("targets")
	url, _ := targets.([]any)[0].(*pyjson.Object).Get("connect_url")
	if s, _ := url.(string); !strings.HasPrefix(s, "https://connect.example/gmail/") {
		t.Fatalf("connect_url %v", url)
	}
}

func TestManagedWatcherTransitionsOnFlipAndSettlesByDeadlineWhenNothingFlips(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	OperationDeadlineSeconds = 0.05
	out := runManaged(t, context.Background(), env, connectArgs("connect", "gmail"), gw, (&desktopCard{}).cb, 0.01, "desktop")
	if out["settled_by"] != "deadline" || targetsOf(out)["gmail"]["state"] != "not_connected" || len(gw.readIDs()) < 2 {
		t.Fatalf("out %v reads %v", out, gw.readIDs())
	}
}

func TestManagedWatcherReadsOneAccountPerTargetPerTickAndNeverTheList(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	gw.flips = map[string]int{"gmail": 3, "notion": 2}
	runManaged(t, context.Background(), env, connectArgs("connect", "gmail", "notion"), gw, (&desktopCard{}).cb, 0, "desktop")
	reads := gw.readIDs()
	count := func(id string) int {
		n := 0
		for _, r := range reads {
			if r == id {
				n++
			}
		}
		return n
	}
	if gw.lists != 0 || count("ca_gmail_1") != 3 || count("ca_notion_1") != 2 || count("ca_gmail_1")+count("ca_notion_1") != len(reads) {
		t.Fatalf("lists %d reads %v", gw.lists, reads)
	}
}

func TestManagedRespondFromTheCardSkipsATargetAndWakesTheLoop(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	gw.flips = map[string]int{"gmail": 2}
	done := make(chan struct{})
	cb := func(payload *pyjson.Object) string {
		id, _ := payload.Get("op_id")
		time.AfterFunc(20*time.Millisecond, func() {
			op := LiveGet(OperationSessionKey(env, "s1"), id.(string), "")
			if _, err := op.Transition("notion", TargetSkipped, ActorUser, TransitionFields{}); err != nil {
				t.Error(err)
			}
			close(done)
		})
		return ""
	}
	out := runManaged(t, context.Background(), env, connectArgs("connect", "gmail", "notion"), gw, cb, 0.01, "desktop")
	<-done
	by := targetsOf(out)
	if by["gmail"]["state"] != "connected" || by["notion"]["state"] != "skipped" || out["settled_by"] != "all_resolved" {
		t.Fatalf("out %v", out)
	}
}

func TestManagedMintFailureDetailSurvivesAndOnlyAnUnlistedCatalogNameIsMisrouted(t *testing.T) {
	env := managedTestSetup(t)
	ctx := context.Background()
	gw := newGatewayFake()
	gw.mintStatus, gw.statusReason = "failed", "vendor: bad scope"
	OperationDeadlineSeconds = 0.05
	patchCatalog(t, []string{"notion"}, []string{"gmail", "notion"}, true)
	by := targetsOf(runManaged(t, ctx, env, connectArgs("connect", "gmail", "notion"), gw, (&desktopCard{}).cb, 0.01, "desktop"))
	if by["gmail"]["state"] != "not_connected" || by["gmail"]["detail"] != "vendor: bad scope" || by["notion"]["detail"] != "vendor: bad scope" {
		t.Fatalf("first %v", by)
	}

	gw = newGatewayFake()
	gw.flips, gw.mintOverrides = map[string]int{"gmail": 1}, map[string]string{"notion": "failed"}
	card := &desktopCard{}
	OperationDeadlineSeconds = 0.2
	patchCatalog(t, []string{"notion"}, []string{"gmail"}, true)
	by = targetsOf(runManaged(t, ctx, env, connectArgs("connect", "gmail", "notion"), gw, card.cb, 0.01, "desktop"))
	if len(gw.mints) != 1 || len(card.seen) == 0 || by["gmail"]["state"] != "connected" ||
		!strings.HasPrefix(by["notion"]["detail"].(string), "notion is a local MCP server") {
		t.Fatalf("second %v", by)
	}

	gw = newGatewayFake()
	gw.mintStatus = "failed"
	card = &desktopCard{}
	OperationDeadlineSeconds = 30
	patchCatalog(t, []string{"notion"}, nil, true)
	out := runManaged(t, ctx, env, connectArgs("connect", "notion"), gw, card.cb, 0.01, "desktop")
	if len(card.seen) != 0 || out["settled_by"] != "all_resolved" || !strings.HasPrefix(targetsOf(out)["notion"]["detail"].(string), "notion is a local MCP server") {
		t.Fatalf("third %v", out)
	}

	gw = newGatewayFake()
	gw.mintStatus, gw.statusReason = "failed", "gateway hiccup"
	OperationDeadlineSeconds = 0.05
	patchCatalog(t, []string{"notion"}, nil, false)
	out = runManaged(t, ctx, env, connectArgs("connect", "notion"), gw, (&desktopCard{}).cb, 0.01, "desktop")
	if targetsOf(out)["notion"]["detail"] != "gateway hiccup" {
		t.Fatalf("fourth %v", out)
	}
}

func TestManagedAGuessedSlugFailsNamingTheListedOneAndStatusNamesItToo(t *testing.T) {
	env := managedTestSetup(t)
	ctx := context.Background()
	fake := func() *gatewayFake {
		gw := newGatewayFake()
		gw.listed, gw.mintStatus = []string{"gmail", "granola_mcp"}, "failed"
		gw.instruction = "This connector is not allowed by your organization's policy."
		return gw
	}
	card := &desktopCard{}
	OperationDeadlineSeconds = 30
	patchCatalog(t, nil, nil, false)
	out := runManaged(t, ctx, env, connectArgs("connect", "granola"), fake(), card.cb, 0.01, "desktop")
	if len(card.seen) != 0 || out["settled_by"] != "all_resolved" || targetsOf(out)["granola"]["detail"] != "'granola' is not a connector slug; use granola_mcp" {
		t.Fatalf("granola %v", out)
	}
	OperationDeadlineSeconds = 0.05
	out = runManaged(t, ctx, env, connectArgs("connect", "gmail"), fake(), (&desktopCard{}).cb, 0.01, "desktop")
	if targetsOf(out)["gmail"]["detail"] != "This connector is not allowed by your organization's policy." {
		t.Fatalf("gmail %v", out)
	}
	old := PortalAccounts
	PortalAccounts = func(context.Context, *Env) ([]map[string]any, error) { return nil, nil }
	defer func() { PortalAccounts = old }()
	out = runManaged(t, ctx, env, connectArgs("status", "granola"), fake(), nil, 0, "desktop")
	unknown, _ := out["unknown"].(map[string]any)
	if len(out["connectors"].([]any)) != 0 || len(unknown) != 1 || unknown["granola"] != "'granola' is not a connector slug; use granola_mcp" {
		t.Fatalf("status %v", out)
	}
}

func TestManagedReconnectOnAnActiveTargetMakesNoGatewayMint(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	gw.connected["gmail"] = true
	out := runManaged(t, context.Background(), env, connectArgs("reconnect", "gmail"), gw, (&desktopCard{}).cb, 0, "desktop")
	if len(gw.mints) != 0 || targetsOf(out)["gmail"]["state"] != "connected" || out["settled_by"] != "all_resolved" {
		t.Fatalf("mints %v out %v", gw.mints, out)
	}
}

func TestManagedReconnectForceAlwaysReinitiatesEvenWhenActive(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	gw.connected["gmail"], gw.flips = true, map[string]int{"gmail": 1}
	OperationDeadlineSeconds = 0.05
	args := connectArgs("reconnect", "gmail")
	args["force"] = true
	runManaged(t, context.Background(), env, args, gw, (&desktopCard{}).cb, 0.01, "desktop")
	if len(gw.mints) != 1 || gw.mints[0]["connectors"] != "gmail" || gw.mints[0]["reinitiate"] != true {
		t.Fatalf("mints %v", gw.mints)
	}
}

func TestManagedReconnectOnADisconnectedTargetReinitiates(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	gw.flips = map[string]int{"gmail": 2}
	runManaged(t, context.Background(), env, connectArgs("reconnect", "gmail"), gw, (&desktopCard{}).cb, 0, "desktop")
	if len(gw.mints) != 1 || gw.mints[0]["connectors"] != "gmail" || gw.mints[0]["reinitiate"] != true {
		t.Fatalf("mints %v", gw.mints)
	}
}

func TestManagedOffDesktopConnectReturnsLinksAndDoesNotBlock(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	out := runManaged(t, context.Background(), env, connectArgs("connect", "gmail"), gw, nil, 0, "cli")
	url, _ := targetsOf(out)["gmail"]["connect_url"].(string)
	if out["status"] != "initiated" || !strings.HasPrefix(url, "https://connect.example/gmail/") || out["op_id"] == nil {
		t.Fatalf("out %v", out)
	}
	if gw.lists != 0 || len(gw.readIDs()) != 0 || LiveCurrent(OperationSessionKey(env, "s1"), "") != nil {
		t.Fatalf("watched without a card")
	}
}

func TestManagedCallbackPresenceDecidesTheCardPathOnTUI(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	gw.flips = map[string]int{"gmail": 1}
	card := &desktopCard{}
	out := runManaged(t, context.Background(), env, connectArgs("connect", "gmail"), gw, card.cb, 0, "tui")
	row := targetsOf(out)["gmail"]
	if _, has := row["connect_url"]; len(card.seen) != 1 || row["state"] != "connected" || has {
		t.Fatalf("card %v out %v", card.seen, out)
	}
}

func TestManagedSecondConnectWhileAnOperationIsOpenIsRefused(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	op := NewConnectionOperation([]*ConnTarget{NewConnTarget("gmail", "connector", "connect")}, OperationSessionKey(env, "s1"), "", nil)
	if err := LiveOpen(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	out := runManaged(t, context.Background(), env, connectArgs("connect", "notion"), gw, (&desktopCard{}).cb, 0, "desktop")
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "already open") || !strings.Contains(msg, op.OpID) || len(gw.mints) != 0 {
		t.Fatalf("out %v mints %v", out, gw.mints)
	}
}

func TestManagedConnectedReadOnAFailedTargetIsIgnoredNotAnError(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	gw.mintStatus, gw.statusReason, gw.flips = "failed", "denied", map[string]int{"gmail": 1}
	OperationDeadlineSeconds = 0.05
	out := runManaged(t, context.Background(), env, connectArgs("connect", "gmail"), gw, (&desktopCard{}).cb, 0.01, "desktop")
	row := targetsOf(out)["gmail"]
	if _, has := out["error"]; has || !slices.Contains([]any{"failed", "not_connected"}, row["state"]) || row["detail"] != "denied" {
		t.Fatalf("out %v", out)
	}
}

func TestManagedContinueDuringAConnectedReadKeepsTheSettledResult(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	var opID string
	settled := false
	gw.afterAccountRead = func(id string, row map[string]any) map[string]any {
		LiveGet(OperationSessionKey(env, "s1"), opID, "").Settle(SettleContinue)
		settled = true
		row["status"], row["active"] = "active", true
		return row
	}
	cb := func(payload *pyjson.Object) string {
		id, _ := payload.Get("op_id")
		opID = id.(string)
		return ""
	}
	out := runManaged(t, context.Background(), env, connectArgs("connect", "gmail"), gw, cb, 0.01, "desktop")
	if _, has := out["error"]; !settled || has || out["settled_by"] != "continue" || targetsOf(out)["gmail"]["state"] != "not_connected" {
		t.Fatalf("out %v", out)
	}
}

func TestManagedInterruptWakesTheLoopAndSettlesBeforeTheNextTick(t *testing.T) {
	env := managedTestSetup(t)
	gw := newGatewayFake()
	worker := NewWorkerID()
	ctx := WithWorker(context.Background(), worker)
	defer SetInterrupt(worker, false, "")
	cb := func(*pyjson.Object) string {
		time.AfterFunc(20*time.Millisecond, func() { SetInterrupt(worker, true, "") })
		return ""
	}
	OperationDeadlineSeconds = 10
	started := time.Now()
	out := runManaged(t, ctx, env, connectArgs("connect", "gmail"), gw, cb, 5.0, "desktop")
	if out["settled_by"] != "interrupt" || time.Since(started) >= 2*time.Second {
		t.Fatalf("out %v after %s", out, time.Since(started))
	}
}
