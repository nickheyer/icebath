package gatewayrun

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

func TestStopReachesResidentAgentAfterRealCacheEviction(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	const key = "agent:main:telegram:dm:106963"
	db, err := state.NewSessionDB(ctx, filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "test"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "current")
	if err != nil {
		t.Fatal(err)
	}
	resident := &agent.AIAgent{}
	r := allocGatewayRunner()
	r.config = gateway.DefaultGatewayConfig(ctx)
	r.agentCache = pydict.New[string, agentCacheEntry]()
	r.agentCache.Set(key, agentCacheEntry{agent: resident, signature: "signature"})
	r.runningAgents().Set(key, resident)
	authority := gateway.NewSessionAuthority(r, "owned", "current", db, epoch)
	user := "human"
	authority.Sessions["s"] = gateway.NewLiveSession(&gateway.SessionSource{UserID: &user}, key)
	actor := gateway.Principal{Subject: "human", ProfileID: "owned", TransportID: "cli",
		Capabilities: map[string]bool{"session:submit": true, "session:control": true}}
	if _, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: actor.Subject, SessionID: "s",
		RequestID: "running", Payload: pyjson.ObjectOf("text", "work")}); err != nil {
		t.Fatal(err)
	}
	row, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: epoch, SessionID: "s"})
	if err != nil || row == nil {
		t.Fatalf("claim %v %v", row, err)
	}
	r.evictCachedAgent(ctx, key, false, false)
	if found := r.CachedAgentFor(key); found != nil {
		t.Fatalf("cached agent after eviction %v", found)
	}
	if found := r.ResidentAgentFor(key); found != resident {
		t.Fatalf("resident agent %v", found)
	}
	generation := gateway.RowInt(row, "generation")
	if _, err := authority.Interrupt(ctx, actor, gateway.SessionRef{ProfileID: "owned", SessionID: "s"}, *generation); err != nil {
		t.Fatal(err)
	}
	if !resident.IsInterrupted() {
		t.Fatal("stop did not reach the resident agent")
	}
	if pending := authority.PendingStops(); len(pending) != 0 {
		t.Fatalf("pending stops %v", pending)
	}
}
