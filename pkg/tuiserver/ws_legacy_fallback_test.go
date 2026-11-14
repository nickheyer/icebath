package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Transport of the legacy fallback cases that records what a pooled handler writes
type wsLegacyTransport struct {
	mu      sync.Mutex
	written []*pyjson.Object
	done    chan struct{}
	once    sync.Once
}

func newWSLegacyTransport() *wsLegacyTransport { return &wsLegacyTransport{done: make(chan struct{})} }

func (t *wsLegacyTransport) Write(obj *pyjson.Object) bool {
	t.mu.Lock()
	t.written = append(t.written, obj)
	t.mu.Unlock()
	t.once.Do(func() { close(t.done) })
	return true
}

func (t *wsLegacyTransport) Close() {}

// Principal holding the interactive grant on a profile
func wsLegacyActor(profileID string) gateway.Principal {
	return gateway.Principal{Capabilities: gateway.PurposeCapabilities("interactive"), ProfileID: profileID}
}

// One legacy fallback request, the reply a pooled handler writes through the transport when none is returned
func wsLegacyCall(t *testing.T, ctx context.Context, actor gateway.Principal, method string, params map[string]any) map[string]any {
	t.Helper()
	transport := newWSLegacyTransport()
	reply := dispatchLegacy(ctx, map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, transport, actor)
	if reply != nil {
		return reply
	}
	select {
	case <-transport.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s was neither answered nor written", method)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	written, _ := pyjson.Plain(transport.written[0]).(map[string]any)
	return written
}

// Sets the import time launch home under the lock its watchers read it with and returns the previous one
func wsTestSwapLaunchHome(h string) string {
	servedProfileMu.Lock()
	defer servedProfileMu.Unlock()
	prev := hermesHome
	hermesHome = h
	return prev
}

// Launch home with a bee profile under it, the launch store reopened for the case
func wsLegacyHomes(t *testing.T) (launch, bee string) {
	t.Helper()
	launch = filepath.Join(t.TempDir(), "root")
	bee = filepath.Join(launch, "profiles", "bee")
	mkdirAll(t, bee)
	t.Setenv("ICEBATH_HOME", launch)
	prevHome := wsTestSwapLaunchHome(launch)
	dbMu.Lock()
	prevDB, prevErr := sharedDB, dbError
	sharedDB, dbError = nil, ""
	dbMu.Unlock()
	prevOpen := OpenSessionDBAtPath
	OpenSessionDBAtPath = func(ctx context.Context, dbPath string, readOnly bool) (*state.DB, error) {
		return state.NewSessionDB(ctx, dbPath, readOnly)
	}
	t.Cleanup(func() {
		wsTestSwapLaunchHome(prevHome)
		OpenSessionDBAtPath = prevOpen
		dbMu.Lock()
		sharedDB, dbError = prevDB, prevErr
		dbMu.Unlock()
	})
	return launch, bee
}

// Session row with a title in the store of a home
func wsLegacySeedSession(t *testing.T, h, sid, title string) {
	t.Helper()
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetSessionTitle(ctx, sid, title); err != nil {
		t.Fatal(err)
	}
}

func TestProfileBoundTicketReadsOnlyItsOwnProfile(t *testing.T) {
	launch, bee := wsLegacyHomes(t)
	wsLegacySeedSession(t, launch, "launch-sid", "LAUNCH_PROFILE_SECRET_TITLE")
	wsLegacySeedSession(t, bee, "bee-sid", "BEE_TITLE")
	for sid, profileHome := range map[string]any{"live-launch": nil, "live-bee": bee} {
		tuigateway.SetSessionRecord(sid, tuigateway.NewSession(map[string]any{
			"session_key": sid, "history": []any{map[string]any{"role": "user", "content": "PRIVATE " + sid}},
			"history_lock": &sync.Mutex{}, "profile_home": profileHome, "created_at": 1, "last_active": 1}))
		t.Cleanup(func() { tuigateway.SetSessionRecord(sid, nil) })
	}
	call := func(profileID, method string) map[string]any {
		actor := wsLegacyActor(profileID)
		if !legacyFallbackAllowed(principalCapabilities(actor), method) {
			t.Fatalf("%s is not allowed through the fallback", method)
		}
		reply := wsLegacyCall(t, context.Background(), actor, method, map[string]any{})
		result, isDict := reply["result"].(map[string]any)
		if !isDict {
			t.Fatalf("%s on %s answered %v", method, profileID, reply)
		}
		return result
	}
	liveIDs := func(result map[string]any) []string {
		var ids []string
		rows, _ := result["sessions"].([]any)
		for _, row := range rows {
			if m, isDict := row.(map[string]any); isDict {
				ids = append(ids, m["id"].(string))
			}
		}
		return ids
	}
	recent := call(bee, "session.most_recent")
	if recent["session_id"] != "bee-sid" || recent["title"] != "BEE_TITLE" {
		t.Fatalf("the bee ticket read %v", recent)
	}
	if ids := liveIDs(call(bee, "session.active_list")); !reflect.DeepEqual(ids, []string{"live-bee"}) {
		t.Fatalf("the bee ticket listed %v", ids)
	}
	if got := call(launch, "session.most_recent")["session_id"]; got != "launch-sid" {
		t.Fatalf("the launch ticket read %v", got)
	}
	ids := liveIDs(call(launch, "session.active_list"))
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"live-bee", "live-launch"}) {
		t.Fatalf("the launch ticket listed %v", ids)
	}
}

func TestTicketWhoseHomeVanishedIsRefusedNotTheLaunchProfile(t *testing.T) {
	_, bee := wsLegacyHomes(t)
	gone := filepath.Join(filepath.Dir(bee), "gone")
	reply := wsLegacyCall(t, context.Background(), wsLegacyActor(gone), "billing.charge", map[string]any{"amount_usd": 5})
	failure, _ := reply["error"].(map[string]any)
	if failure["code"] != 4064 {
		t.Fatalf("reply %v", reply)
	}
}

func TestSecondaryProfilePasteLandsInItsOwnHome(t *testing.T) {
	tmp := t.TempDir()
	launch, secondary := filepath.Join(tmp, "launch"), filepath.Join(tmp, "profiles", "coder")
	mkdirAll(t, launch)
	mkdirAll(t, secondary)
	prevHome := wsTestSwapLaunchHome(launch)
	t.Cleanup(func() { wsTestSwapLaunchHome(prevHome) })
	reply := dispatchLegacy(context.Background(), map[string]any{"jsonrpc": "2.0", "id": 1, "method": "paste.collapse",
		"params": map[string]any{"text": "secret\npaste"}}, nil, wsLegacyActor(secondary))
	result, _ := reply["result"].(map[string]any)
	path, _ := result["path"].(string)
	if filepath.Dir(path) != filepath.Join(secondary, "pastes") {
		t.Fatalf("the paste landed at %q (reply %v)", path, reply)
	}
	written, err := os.ReadFile(path)
	if err != nil || string(written) != "secret\npaste" {
		t.Fatalf("paste content %q, %v", written, err)
	}
	if _, err := os.Stat(filepath.Join(launch, "pastes")); !os.IsNotExist(err) {
		t.Fatalf("the launch home got a pastes directory: %v", err)
	}
}
