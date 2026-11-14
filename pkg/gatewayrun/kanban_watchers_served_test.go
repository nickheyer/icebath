package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	servedSession       = "20260918_033413_0665eb"
	servedWorkerSession = "20260918_034333_945a5a"
)

// Stateless adapter recording in process wake turns and the home and profile each ran under
type recordingAPIServerAdapter struct {
	*gateway.BasePlatformAdapter
	mu       sync.Mutex
	turns    []map[string]string
	homes    []string
	profiles []string
	server   *selfPostServer
}

func (a *recordingAPIServerAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *recordingAPIServerAdapter) Disconnect(context.Context) error            { return nil }
func (a *recordingAPIServerAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: false, Error: "API server uses HTTP request/response, not send()"}, nil
}
func (a *recordingAPIServerAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}
func (a *recordingAPIServerAdapter) RunInternalSessionTurn(ctx context.Context, sessionID, text, profile, category string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.homes = append(a.homes, home.Dir(ctx))
	a.profiles = append(a.profiles, profile)
	a.turns = append(a.turns, map[string]string{"session_id": sessionID, "text": text, "category": category})
	return nil
}
func (a *recordingAPIServerAdapter) SelfPostTarget() (string, int, string, string) {
	if a.server == nil {
		return "", 0, "", ""
	}
	return a.server.host, a.server.port, strings.Repeat("k", 20), "icebath"
}

// Recording API server adapter, posting HTTP wakes to server when one is given
func newRecordingAPIServerAdapter(server *selfPostServer) *recordingAPIServerAdapter {
	a := &recordingAPIServerAdapter{server: server}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.PlatformAPIServer, a)
	a.BasePlatformAdapter.Traits.SupportsAsyncDelivery = false
	return a
}

// Multiplex home serving the route only builder profile beside atlas
type servedHome struct {
	root, builder, atlas string
}

// Default multiplex home with builder and atlas profiles and the kanban board beside it
func setupServed(t *testing.T) servedHome {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	s := servedHome{root: root, builder: filepath.Join(root, "profiles", "builder"), atlas: filepath.Join(root, "profiles", "atlas")}
	for _, d := range []string{s.builder, s.atlas} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{filepath.Join(root, "config.yaml"): "gateway:\n  multiplex_profiles: true\n", filepath.Join(root, ".env"): "",
		filepath.Join(s.builder, "config.yaml"): "{}\n", filepath.Join(s.builder, ".env"): ""}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(tmp, "board.db"))
	return s
}

// Records a session in the state store of a home stamped with the profile
func ownSession(t *testing.T, h, sessionID, profile string) {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: sessionID, Source: "webui", ProfileName: profile}); err != nil {
		t.Fatal(err)
	}
}

// Multiplexing runner whose primary api_server adapter is adapter and whose builder profile holds builderAdapters
func makeServedRunner(t *testing.T, adapter gateway.PlatformAdapter, builderAdapters *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]) *GatewayRunner {
	t.Helper()
	if adapter == nil {
		adapter = newRecordingAPIServerAdapter(nil)
	}
	r := makeKanbanRunner(t, adapter)
	r.adapters = adapterDict(gateway.PlatformAPIServer, adapter)
	if builderAdapters == nil {
		builderAdapters = adapterDict()
	}
	r.profileAdapters = pydict.New[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]]()
	r.profileAdapters.Set("builder", builderAdapters)
	r.profileAdapters.Set("atlas", adapterDict())
	r.primaryProfileName = "default"
	r.kanbanNotifierProfile = "default"
	r.config = gateway.DefaultGatewayConfig(context.Background())
	on := true
	r.config.MultiplexProfiles = &on
	r.config.ProfileRoutes = nil
	return r
}

// Completed card whose origin is a raw api_server session id
func servedSubscription(t *testing.T, chatID, profile string) string {
	t.Helper()
	conn, err := kanban.Connect(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tid := kanbanCreate(t, conn, "relay origin", "builder", kstr(servedWorkerSession))
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "api_server", ChatID: chatID, ChatType: kstr("dm"),
		NotifierProfile: kstr(profile), DeliveryMode: kstr("notify+wake")})
	kanbanComplete(t, conn, tid, "done once")
	return tid
}

// Subscription row of an api_server destination
func apiSub(chatID, profile string) *pyjson.Object {
	return kobj("task_id", "t_owned", "platform", "api_server", "chat_id", chatID, "chat_type", "dm", "thread_id", "",
		"notifier_profile", profile, "delivery_metadata", nil)
}

func TestServedProfileWakeRunsInProcessOnlyForTheSessionItOwns(t *testing.T) {
	s := setupServed(t)
	ownSession(t, s.builder, servedSession, "builder")
	server := newSelfPostServer(t, 0)
	if _, err := kanban.InitDB(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}
	adapter := newRecordingAPIServerAdapter(nil)
	r := makeServedRunner(t, adapter, nil)
	task := servedSubscription(t, servedSession, "builder")
	runOneNotifierTick(t, r)
	if len(adapter.turns) != 1 || adapter.turns[0]["session_id"] != servedSession {
		t.Fatalf("turns %v", adapter.turns)
	}
	if !strings.Contains(adapter.turns[0]["text"], task) || !strings.Contains(adapter.turns[0]["text"], "done once") {
		t.Fatalf("text %q", adapter.turns[0]["text"])
	}
	if strings.Join(adapter.homes, ",") != s.builder || strings.Join(adapter.profiles, ",") != "builder" || len(server.posts) != 0 {
		t.Fatalf("homes %v profiles %v posts %v", adapter.homes, adapter.profiles, server.posts)
	}
	if a, _ := r.adapters.Get(gateway.PlatformAPIServer); a != gateway.PlatformAdapter(adapter) {
		t.Fatal("primary adapter replaced")
	}
	if b, _ := r.profileAdapters.Get("builder"); b.Len() != 0 {
		t.Fatal("builder gained adapters")
	}
	if n := len(unseenOnCompleted(t, task, servedSession)); n != 0 {
		t.Fatalf("unseen %d", n)
	}

	ctx := context.Background()
	resolve := func(sub *pyjson.Object, profile string, builder *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]) gateway.PlatformAdapter {
		a, err := adapterForSubscription(ctx, makeServedRunner(t, nil, builder), gateway.PlatformAPIServer, sub, profile)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	if resolve(apiSub("20260918_051500_deadbe", "builder"), "builder", nil) != nil {
		t.Fatal("unknown session authorized")
	}
	if resolve(apiSub(servedSession, "builder"), "atlas", nil) != nil {
		t.Fatal("non owner authorized")
	}
	if resolve(apiSub(servedSession, "builder"), "ghost", nil) != nil {
		t.Fatal("unserved profile authorized")
	}
	if resolve(apiSub(servedSession, "builder"), "builder", adapterDict(gateway.PlatformDiscord, newKanbanRecordingAdapterOn(gateway.PlatformDiscord))) == nil {
		t.Fatal("other platform adapter gated api_server")
	}
	ownAPI := newRecordingAPIServerAdapter(nil)
	if resolve(apiSub(servedSession, "builder"), "builder", adapterDict(gateway.PlatformAPIServer, ownAPI)) != gateway.PlatformAdapter(ownAPI) {
		t.Fatal("own api_server adapter not used")
	}
	ownSession(t, s.atlas, "stamped-elsewhere", "builder")
	if resolve(apiSub("stamped-elsewhere", "builder"), "builder", nil) != nil {
		t.Fatal("foreign store authorized")
	}
	ownSession(t, s.builder, "atlas-row", "atlas")
	if resolve(apiSub("atlas-row", "builder"), "builder", nil) != nil {
		t.Fatal("foreign stamp authorized")
	}

	ownSession(t, s.root, "default-origin", "default")
	defaultAdapter := newRecordingAPIServerAdapter(server)
	r = makeServedRunner(t, defaultAdapter, nil)
	task = servedSubscription(t, "default-origin", "default")
	runOneNotifierTick(t, r)
	if len(defaultAdapter.turns) != 0 || strings.Join(server.sessions(), ",") != "default-origin" {
		t.Fatalf("turns %v posts %v", defaultAdapter.turns, server.sessions())
	}
	if n := len(unseenOnCompleted(t, task, "default-origin")); n != 0 {
		t.Fatalf("unseen %d", n)
	}
}

// Unseen completed events of an api_server destination
func unseenOnCompleted(t *testing.T, tid, chatID string) []*kanban.Event {
	t.Helper()
	conn := kanbanTestConn(t)
	_, events, err := kanban.UnseenEventsForSub(context.Background(), conn, tid, "api_server", chatID, nil, []string{"completed"})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// Telegram adapter with a chat to workspace map for scope lookups
type scopeMapAdapter struct {
	*kanbanRecordingAdapter
	channelTeam map[string]string
	explode     bool
}

func (a *scopeMapAdapter) ScopeIDForChat(chatID string) *string {
	if a.explode {
		panic("adapter state gone")
	}
	if team, ok := a.channelTeam[chatID]; ok && team != "" {
		return &team
	}
	return nil
}

func TestUnscopedPlatformWakeKeyIsByteIdentical(t *testing.T) {
	kanbanTestDB(t, "wake-scope-telegram.db")
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "wake scope", "worker", kstr("origin-session"))
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "chat-dm", ChatType: kstr("dm"), DeliveryMode: kstr("notify+wake")})
	kanbanComplete(t, conn, tid, "done")
	adapter := newKanbanRecordingAdapter()
	runOneNotifierTick(t, makeKanbanRunner(t, adapter))
	if len(adapter.handled) != 1 {
		t.Fatalf("handled %d", len(adapter.handled))
	}
	wake := adapter.handled[0].Source
	if wake.ScopeID != nil {
		t.Fatalf("scope %v", *wake.ScopeID)
	}
	key, err := gateway.BuildSessionKey(context.Background(), wake, true, false, "")
	if err != nil || key != "agent:main:telegram:dm:chat-dm" {
		t.Fatalf("key %s %v", key, err)
	}
}

func TestWakeScopeIDPrefersPersistedMetadataOverTheAdapterMap(t *testing.T) {
	const team, channel = "T0B8U2M6NRE", "C0BCDG3H66P"
	adapter := &scopeMapAdapter{kanbanRecordingAdapter: newKanbanRecordingAdapter(), channelTeam: map[string]string{channel: "T_STALE"}}
	if got := wakeScopeID(adapter, kobj("chat_id", channel, "delivery_metadata", kobj("slack_team_id", team))); deref(got) != team {
		t.Fatalf("got %v", got)
	}
	if got := wakeScopeID(adapter, kobj("chat_id", channel)); deref(got) != "T_STALE" {
		t.Fatalf("got %v", got)
	}
	if got := wakeScopeID(adapter, kobj("chat_id", "C_OTHER")); got != nil {
		t.Fatalf("got %v", *got)
	}
}

func TestWakeScopeIDDegradesWhenTheAdapterLookupRaises(t *testing.T) {
	adapter := &scopeMapAdapter{kanbanRecordingAdapter: newKanbanRecordingAdapter(), explode: true}
	if got := wakeScopeID(adapter, kobj("chat_id", "C0BCDG3H66P")); got != nil {
		t.Fatalf("got %v", *got)
	}
}
