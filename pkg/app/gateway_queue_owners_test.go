package app

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Transport of an owner side viewer connection that drops every frame
type droppingTransport struct{}

func (droppingTransport) Write(*pyjson.Object) bool { return true }
func (droppingTransport) Close()                    {}

// Owner the authority boots over, the store, an empty adapter map and no cached agent
type queueOwnerRunner struct {
	store     *gateway.SessionStore
	adapters  *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]
	authority *gateway.SessionAuthority
}

func (r *queueOwnerRunner) SessionRuntimeDescriptor() map[string]any { return nil }
func (r *queueOwnerRunner) Draining() bool                           { return false }
func (r *queueOwnerRunner) Running() bool                            { return true }
func (r *queueOwnerRunner) SessionStore() *gateway.SessionStore      { return r.store }
func (r *queueOwnerRunner) MultiplexProfiles() bool                  { return false }
func (r *queueOwnerRunner) CachedAgentFor(string) any                { return nil }
func (r *queueOwnerRunner) EvictCachedAgent(string)                  {}
func (r *queueOwnerRunner) SetSessionAuthority(a *gateway.SessionAuthority) {
	r.authority = a
}
func (r *queueOwnerRunner) Adapters() *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	return r.adapters
}
func (r *queueOwnerRunner) ProfileAdapterMap(string) *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	return pydict.New[gateway.Platform, gateway.PlatformAdapter]()
}
func (r *queueOwnerRunner) AdapterForSource(_ context.Context, source *gateway.SessionSource) (gateway.PlatformAdapter, error) {
	adapter, _ := r.adapters.Get(source.Platform)
	return adapter, nil
}

// One viewer socket whose calls go through the owner's real connection dispatch over the wire encoding
type ownerLife struct {
	connection *gateway.AuthorityConnection
	events     *gateway.EventQueue
}

func (l *ownerLife) RPCTimeout(ctx context.Context, method string, params map[string]any, _ time.Duration) (any, error) {
	if params == nil {
		params = map[string]any{}
	}
	raw, err := pyjson.Dumps(map[string]any{"id": 1, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	request, err := pyjson.LoadsDict(raw)
	if err != nil {
		return nil, err
	}
	reply, err := l.connection.Dispatch(ctx, request)
	if err != nil {
		return nil, err
	}
	if member, failed := reply.Get("error"); failed {
		return nil, gateway.RPCError(member)
	}
	result, _ := reply.Get("result")
	text, err := pyjson.Dumps(result)
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

func (l *ownerLife) EventQueue() *gateway.EventQueue { return l.events }
func (l *ownerLife) SendFrame(string) error          { return nil }
func (l *ownerLife) Close() error                    { return nil }

// Real owners over one state database, a boot again is the owner restart and the first owner never drains
type queueOwners struct {
	t       *testing.T
	ctx     context.Context
	profile string
	db      *state.SessionDB
	store   *gateway.SessionStore
	first   *gateway.SessionAuthority
	sid     string
	mu      sync.Mutex
	current *gateway.SessionAuthority
	lives   []*ownerLife
}

func startQueueOwners(t *testing.T) *queueOwners {
	t.Helper()
	ctx := context.Background()
	profile := t.TempDir()
	t.Setenv("ICEBATH_HOME", profile)
	if err := os.WriteFile(filepath.Join(profile, "config.yaml"), []byte("platform_toolsets:\n  cli: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := state.NewSessionDB(ctx, filepath.Join(profile, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	store := gateway.NewSessionStore(ctx, filepath.Join(profile, "sessions"), gateway.DefaultGatewayConfig(ctx), nil)
	store.PinDB(db)
	o := &queueOwners{t: t, ctx: ctx, profile: profile, db: db, store: store}
	prevResolve, prevLoad := gateway.ResolvePrefillMessagesFile, gateway.LoadPrefillMessages
	gateway.ResolvePrefillMessagesFile, gateway.LoadPrefillMessages = ResolvePrefillMessagesFile, LoadPrefillMessages
	t.Cleanup(func() { gateway.ResolvePrefillMessagesFile, gateway.LoadPrefillMessages = prevResolve, prevLoad })
	var previous gateway.SessionSchedule
	previous = gateway.SwapSessionSchedule(func(ctx context.Context, a *gateway.SessionAuthority, ref gateway.SessionRef) error {
		if a == o.first {
			return nil
		}
		return previous(ctx, a, ref)
	})
	t.Cleanup(func() {
		gateway.SwapSessionSchedule(previous)
		o.mu.Lock()
		lives := append([]*ownerLife{}, o.lives...)
		o.mu.Unlock()
		for _, life := range lives {
			_ = life.connection.Close(ctx)
		}
		db.Close()
	})
	o.first = o.boot("first")
	owner, err := gateway.NewAuthorityConnection(o.first, droppingTransport{}, map[string]any{"user_id": "owner"}, false)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := gateway.CreateLocalSession(ctx, o.first, owner.Actor(), map[string]any{"cwd": profile, "model": "m", "toolsets": []any{}, "request_id": "n5"})
	if err != nil {
		t.Fatal(err)
	}
	o.sid = ref.SessionID
	return o
}

// Boots a new owner on the shared store and database and makes it the one new viewers dial
func (o *queueOwners) boot(instanceID string) *gateway.SessionAuthority {
	o.t.Helper()
	runner := &queueOwnerRunner{store: o.store, adapters: pydict.New[gateway.Platform, gateway.PlatformAdapter]()}
	a, err := gateway.InitializeSessionAuthority(o.ctx, runner, o.profile, instanceID, o.db, true)
	if err != nil {
		o.t.Fatalf("boot %s: %v", instanceID, err)
	}
	o.mu.Lock()
	o.current = a
	o.mu.Unlock()
	return a
}

// Opens one viewer life on the current owner
func (o *queueOwners) dial(context.Context) (gatewaycli.ConnectionClient, error) {
	o.mu.Lock()
	current := o.current
	o.mu.Unlock()
	connection, err := gateway.NewAuthorityConnection(current, droppingTransport{}, map[string]any{"user_id": "owner"}, false)
	if err != nil {
		return nil, err
	}
	life := &ownerLife{connection: connection, events: gateway.NewEventQueue(0)}
	o.mu.Lock()
	o.lives = append(o.lives, life)
	o.mu.Unlock()
	return life, nil
}

// Connection dialing the current owner, closed when the case ends
func (o *queueOwners) connection() *gatewaycli.GatewayConnection {
	conn := gatewaycli.NewGatewayConnection(o.dial)
	o.t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// Admission rows of the session carrying the queued text
func (o *queueOwners) rows() []*pyjson.Object {
	o.t.Helper()
	all, err := state.ListSessionAdmissions(o.ctx, o.db, o.sid, false)
	if err != nil {
		o.t.Fatal(err)
	}
	var out []*pyjson.Object
	for _, row := range all {
		payload, _ := row.Get("payload")
		if text, _ := objectField(payload, "text"); text == fifoText {
			out = append(out, row)
		}
	}
	return out
}

// Statuses of the rows carrying the queued text
func (o *queueOwners) statuses() []string {
	var out []string
	for _, row := range o.rows() {
		status, _ := row.Get("status")
		out = append(out, status.(string))
	}
	return out
}

// Field of a decoded JSON object or plain map
func objectField(value any, key string) (any, bool) {
	switch v := value.(type) {
	case *pyjson.Object:
		return v.Get(key)
	case map[string]any:
		field, ok := v[key]
		return field, ok
	}
	return nil, false
}

// Busy CLI built without its constructor, viewing the session through the gateway store
func busyQueueCLI(t *testing.T, store *gatewaycli.GatewaySessionStore, sid string) *CLI {
	t.Helper()
	c := bareCLI()
	c.sessionID = sid
	c.pendingInput, c.interruptQueue = &inputQueue{}, &inputQueue{}
	c.sessionDB = store
	c.agentRunning = true
	return c
}

// Runs one slash command and returns what it printed joined by spaces
func queueCommand(t *testing.T, c *CLI, rec *printRecorder, text string) string {
	t.Helper()
	before := len(rec.plain())
	if !c.processCommand(text) {
		t.Fatalf("%s ended the CLI", text)
	}
	return strings.Join(rec.plain()[before:], " ")
}

// Attached store and busy CLI over a fresh connection to the owners
func attachedQueueCLI(t *testing.T, o *queueOwners) (*CLI, *gatewaycli.GatewaySessionStore) {
	t.Helper()
	store := gatewaycli.NewGatewaySessionStore(o.connection())
	if _, err := store.Attach(context.Background(), o.sid, false); err != nil {
		t.Fatalf("attach: %v", err)
	}
	return busyQueueCLI(t, store, o.sid), store
}

func TestQueueWhileBusyIsADurableAdmissionThatSurvivesAnOwnerRestart(t *testing.T) {
	rec := recordPrints(t)
	o := startQueueOwners(t)
	c, _ := attachedQueueCLI(t, o)
	queueCommand(t, c, rec, "/queue "+fifoText)
	if _, held := c.pendingInput.GetNoWait(); held {
		t.Fatal("the prompt was held client side")
	}
	if got := o.statuses(); len(got) != 1 || got[0] != "queued" {
		t.Fatalf("statuses %v", got)
	}
	if listed := queueCommand(t, c, rec, "/queue list"); !strings.Contains(listed, fifoText) {
		t.Fatalf("list %q", listed)
	}
	o.boot("restarted")
	if got := o.statuses(); len(got) != 1 || got[0] != "queued" {
		t.Fatalf("statuses after restart %v", got)
	}
	ids := map[any]bool{}
	for _, row := range o.rows() {
		id, _ := row.Get("admission_id")
		ids[id] = true
	}
	if len(ids) != 1 {
		t.Fatalf("admissions %v", ids)
	}
	snapshot, err := gatewaycli.NewGatewaySessionStore(o.connection()).Attach(context.Background(), o.sid, false)
	if err != nil {
		t.Fatalf("fresh attach: %v", err)
	}
	pending, _ := snapshot.Get("pending")
	rows, _ := pending.([]any)
	if len(rows) != 1 {
		t.Fatalf("pending %v", pending)
	}
	text, _ := objectField(rows[0], "text")
	status, _ := objectField(rows[0], "status")
	if text != fifoText || status != "queued" {
		t.Fatalf("pending row %v", rows[0])
	}
}

func TestQueueRmAndClearRetireTheGatewayAdmissions(t *testing.T) {
	rec := recordPrints(t)
	o := startQueueOwners(t)
	c, _ := attachedQueueCLI(t, o)
	queueCommand(t, c, rec, "/queue "+fifoText)
	queueCommand(t, c, rec, "/queue second item")
	queueCommand(t, c, rec, "/queue rm 1")
	if got := o.statuses(); len(got) != 1 || got[0] != "terminal" {
		t.Fatalf("statuses %v", got)
	}
	if listed := queueCommand(t, c, rec, "/queue list"); !strings.Contains(listed, "second item") || strings.Contains(listed, fifoText) {
		t.Fatalf("list %q", listed)
	}
	queueCommand(t, c, rec, "/queue clear")
	if listed := queueCommand(t, c, rec, "/queue list"); !strings.Contains(strings.ToLower(listed), "empty") {
		t.Fatalf("list after clear %q", listed)
	}
}

// Enter typed while the turn runs in the given busy mode, the busy input hint already seen
func busyEnter(t *testing.T, c *CLI, mode, text string, images ...string) {
	t.Helper()
	SetCLIConfig(map[string]any{"onboarding": map[string]any{"seen": map[string]any{"busy_input_prompt": true}}})
	t.Cleanup(func() { SetCLIConfig(nil) })
	c.busyInputMode = mode
	var payload any = text
	if len(images) > 0 {
		payload = submittedInput{Text: text, Images: append([]string{}, images...)}
	}
	c.tuiEnterWhileBusy(text, append([]string{}, images...), payload, "")
}

func TestProbeQueueModeEnterThenQueueKeepEntryOrder(t *testing.T) {
	rec := recordPrints(t)
	o := startQueueOwners(t)
	c, _ := attachedQueueCLI(t, o)
	busyEnter(t, c, "queue", "typed first")
	queueCommand(t, c, rec, "/queue "+fifoText)
	if _, held := c.pendingInput.GetNoWait(); held {
		t.Fatal("an input was held client side")
	}
	listed := queueCommand(t, c, rec, "/queue list")
	first, second := strings.Index(listed, "typed first"), strings.Index(listed, fifoText)
	if first < 0 || second < 0 || first >= second {
		t.Fatalf("entry order lost: %s", listed)
	}
}

// One input the replaced dispatch step received with the time it arrived
type processedInput struct {
	at   time.Time
	item any
}

// Runs the input loop with a recording dispatch until the given count and returns the records and a stop
func runInputLoop(c *CLI, until int) (func() []processedInput, func(timeout time.Duration)) {
	var mu sync.Mutex
	var processed []processedInput
	one := func(item any) {
		mu.Lock()
		processed = append(processed, processedInput{time.Now(), item})
		reached := len(processed) >= until
		mu.Unlock()
		if reached {
			c.uiMu.Lock()
			c.shouldExit = true
			c.uiMu.Unlock()
		}
	}
	c.uiMu.Lock()
	c.shouldExit = false
	c.uiMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.tuiProcessLoopWith(one, func() { c.drainProcessNotifications("cli-idle") })
	}()
	records := func() []processedInput {
		mu.Lock()
		defer mu.Unlock()
		return append([]processedInput{}, processed...)
	}
	stop := func(timeout time.Duration) {
		select {
		case <-done:
		case <-time.After(timeout):
		}
		c.uiMu.Lock()
		c.shouldExit = true
		c.uiMu.Unlock()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
	return records, stop
}

// Text an input renders as, the str of the Python item
func inputText(item any) string {
	if seeded, ok := item.(*seededQueryMessage); ok {
		return seeded.String()
	}
	if text, ok := item.(string); ok {
		return text
	}
	return ""
}

func TestProbeSlashCommandTypedAfterQueueRunsAfterIt(t *testing.T) {
	c, client, conn, _ := idleNotifyCLI(t)
	state := rememberQueued(conn, "adm-q", fifoText)
	state.ApplyEvent(context.Background(), "session.info", map[string]any{"running": true, "pending": []any{
		map[string]any{"admission_id": "adm-1", "status": "started", "sequence": 1, "text": "turn"},
		map[string]any{"admission_id": "adm-q", "status": "queued", "sequence": 2, "text": fifoText}}})
	c.pendingInput.Put("/new")
	processed, stop := runInputLoop(c, 2)
	waitUntil(t, func() bool { return c.pendingInput.Empty() })
	for _, frame := range []map[string]any{
		scriptFrame("session.info", map[string]any{"running": false, "pending": []any{
			map[string]any{"admission_id": "adm-q", "status": "queued", "sequence": 2, "text": fifoText}}}, ""),
		scriptFrame("message.start", map[string]any{}, "adm-q"),
	} {
		client.events.PutNowait(wireValue(t, frame))
	}
	stop(5 * time.Second)
	records := processed()
	var order []string
	for _, record := range records {
		order = append(order, inputText(record.item))
	}
	if len(order) != 2 || order[0] != fifoText || order[1] != "/new" {
		t.Fatalf("input loop runs %q before the /queue prompt", order)
	}
	if seeded, ok := records[0].item.(*seededQueryMessage); !ok || seeded.AdmissionID != "adm-q" {
		t.Fatalf("first input %#v", records[0].item)
	}
}

func TestTypingStaysResponsiveWhenTheOwnerQueueIsPaused(t *testing.T) {
	if ownQueueHoldSeconds < 2.0 {
		t.Fatalf("hold %v", ownQueueHoldSeconds)
	}
	queued := map[string]any{"admission_id": "adm-q", "status": "queued", "sequence": 2, "text": fifoText}
	cases := []struct {
		name    string
		pending []any
		running bool
	}{
		{"unknown_head_idle", []any{map[string]any{"admission_id": "adm-0", "status": "unknown", "sequence": 1, "text": "lost"}, queued}, false},
		{"own_head_idle", []any{queued}, false},
		{"own_head_running", []any{queued}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, conn, _ := idleNotifyCLI(t)
			state := rememberQueued(conn, "adm-q", fifoText)
			state.ApplyEvent(context.Background(), "session.info", map[string]any{"running": tc.running, "pending": tc.pending})
			started := time.Now()
			c.pendingInput.Put("/help")
			processed, stop := runInputLoop(c, 1)
			stop(time.Duration((ownQueueHoldSeconds + 2) * float64(time.Second)))
			records := processed()
			if len(records) != 1 || inputText(records[0].item) != "/help" {
				t.Fatalf("processed %v", records)
			}
			limit := ownQueueHoldSeconds + 1.0
			if status, _ := tc.pending[0].(map[string]any)["status"].(string); status == "unknown" {
				limit = 0.5
			}
			if waited := records[0].at.Sub(started).Seconds(); waited >= limit {
				t.Fatalf("waited %v", waited)
			}
			state.Lock()
			keys := state.QueuedInputs.Keys
			value, _ := state.QueuedInputs.Get("adm-q")
			state.Unlock()
			if len(keys) != 1 || value != fifoText {
				t.Fatalf("queued inputs %v", keys)
			}
		})
	}
}

// Gateway agent whose running turn refuses every steer
type steerRefusingAgent struct{ *GatewayAgent }

func (steerRefusingAgent) Steer(string) any { return false }

func TestASteerThatFallsBackToQueueIsAGatewayAdmissionAndImagesStayLocal(t *testing.T) {
	c, client, conn, _ := idleNotifyCLI(t)
	c.agentRunning = true
	c.agent = steerRefusingAgent{&GatewayAgent{ctx: context.Background()}}
	busyEnter(t, c, "steer", "steer me")
	state := conn.State(scriptSID)
	state.Lock()
	var queued []any
	for _, key := range state.QueuedInputs.Keys {
		value, _ := state.QueuedInputs.Get(key)
		queued = append(queued, value)
	}
	state.Unlock()
	if !c.pendingInput.Empty() || len(queued) != 1 || queued[0] != "steer me" {
		t.Fatalf("pending empty %v queued %v", c.pendingInput.Empty(), queued)
	}
	busyEnter(t, c, "queue", "look", "/img.png")
	item, _ := c.pendingInput.GetNoWait()
	if want := (submittedInput{Text: "look", Images: []string{"/img.png"}}); !reflect.DeepEqual(item, want) {
		t.Fatalf("held %#v", item)
	}
	if submits := len(client.callsOf("prompt.submit")); submits != 1 {
		t.Fatalf("submits %d", submits)
	}
	conn.Close(context.Background())
	local := bareCLI()
	local.sessionID = scriptSID
	local.pendingInput, local.interruptQueue = &inputQueue{}, &inputQueue{}
	local.agentRunning = true
	busyEnter(t, local, "queue", "plain")
	if item, _ := local.pendingInput.GetNoWait(); item != "plain" {
		t.Fatalf("held %#v", item)
	}
}
