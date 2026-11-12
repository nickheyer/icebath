package gatewayrun

import (
	"context"
	"math/big"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Earlier queued release holding the single release worker until told to finish
type releaseStuckOnTheRegistry struct {
	started, finish chan struct{}
	once            sync.Once
}

func (s *releaseStuckOnTheRegistry) Release() error {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.finish:
	case <-time.After(10 * time.Second):
	}
	return nil
}

// Marker whose release signals the worker reached it
type drainMarker struct{ done chan struct{} }

func (m drainMarker) Release() error { close(m.done); return nil }

// Runner with an active session cap and empty running state
func leaseRunner(cap int64) *GatewayRunner {
	r := allocGatewayRunner()
	r.config = &gateway.GatewayConfig{}
	if cap > 0 {
		r.config.MaxConcurrentSessions = big.NewInt(cap)
	}
	return r
}

// Claims a gateway slot for the session and ends its turn
func endGatewayTurn(t *testing.T, r *GatewayRunner, sessionKey string, source *gateway.SessionSource) *activesessions.Lease {
	t.Helper()
	ctx := context.Background()
	lease, refusal := r.claimActiveSessionSlot(ctx, sessionKey, source)
	if refusal != nil || lease == nil {
		t.Fatalf("claim refused %v", refusal)
	}
	r.sessionState(sessionKey).turn.lease = lease
	r.releaseRunningAgentState(ctx, sessionKey, nil)
	return lease
}

// Releases the stuck worker and waits for the ended turn's release to land
func drainLeaseWorker(t *testing.T, lease *activesessions.Lease, stuck *releaseStuckOnTheRegistry) {
	t.Helper()
	close(stuck.finish)
	done := make(chan struct{})
	gateway.ReleaseSoon(drainMarker{done})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the release worker never drained")
	}
	if !lease.Released() {
		t.Fatal("ended turn lease not released")
	}
}

func TestTheNextSessionsClaimAtTheCapSeesTheEndedTurnsSlotFree(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	r := leaseRunner(1)
	src := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "c1", UserID: strp("u1")}
	stuck := &releaseStuckOnTheRegistry{started: make(chan struct{}), finish: make(chan struct{})}
	gateway.ReleaseSoon(stuck)
	select {
	case <-stuck.started:
	case <-time.After(10 * time.Second):
		t.Fatal("stuck release never started")
	}
	leaseA := endGatewayTurn(t, r, "agent:main:telegram:dm:a", src)
	leaseB, refusal := r.claimActiveSessionSlot(context.Background(), "agent:main:telegram:dm:b", src)
	if refusal != nil {
		t.Fatalf("the next session was refused with nothing running: %v", refusal)
	}
	drainLeaseWorker(t, leaseA, stuck)
	entries, err := activesessions.RegistrySnapshot(context.Background(), tmp, true)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.GetOr("session_id", nil).(string))
	}
	if !slices.Equal(ids, []string{"agent:main:telegram:dm:b"}) {
		t.Fatalf("registry %v", ids)
	}
	if err := leaseB.Release(); err != nil {
		t.Fatal(err)
	}
}

// Runner over a real session database with an agent cache
func rebaselineRunner(t *testing.T) (*GatewayRunner, *state.SessionDB) {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(t.TempDir(), "sessions.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.CreateSession(state.NewSession{ID: "s1", Source: "telegram"}); err != nil {
		t.Fatal(err)
	}
	r := allocGatewayRunner()
	r.agentCache = pydict.New[string, agentCacheEntry]()
	r.agentCacheLock = &sync.Mutex{}
	r.pinSessionDB(state.NewAsyncSessionDB(db))
	return r, db
}

// Stored message count of the session, nil when it cannot be read
func liveMessageCount(t *testing.T, db *state.SessionDB, sessionID string) *int {
	t.Helper()
	row, err := db.GetSession(context.Background(), sessionID)
	if err != nil || row == nil {
		return nil
	}
	n, ok := pythonIntField(row.GetOr("message_count", int64(0)))
	if !ok {
		return nil
	}
	return &n
}

// Reuse decision of the cache hit guard, reuse unless the stored snapshot and the live count differ
func guardWouldReuse(t *testing.T, r *GatewayRunner, db *state.SessionDB, sessionKey, sessionID string) bool {
	t.Helper()
	live := liveMessageCount(t, db, sessionID)
	r.agentCacheLock.Lock()
	cached, _ := r.agentCache.Get(sessionKey)
	r.agentCacheLock.Unlock()
	return !(cached.msgCount != nil && live != nil && *live != *cached.msgCount)
}

// Appends one message row to the session
func appendRow(t *testing.T, db *state.SessionDB, role, content string) {
	t.Helper()
	if _, err := db.AppendMessage(context.Background(), "s1", map[string]any{"role": role, "content": content}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
}

func TestCrossProcessWriteStillInvalidates(t *testing.T) {
	r, db := rebaselineRunner(t)
	build := liveMessageCount(t, db, "s1")
	r.agentCache.Set("telegram:s1", agentCacheEntry{agent: &agent.AIAgent{}, signature: "sig", msgCount: build})
	appendRow(t, db, "user", "u")
	appendRow(t, db, "assistant", "a")
	r.refreshAgentCacheMessageCount(context.Background(), "telegram:s1", "s1")
	if !guardWouldReuse(t, r, db, "telegram:s1", "s1") {
		t.Fatal("own turn rebuilt the agent after the rebaseline")
	}
	appendRow(t, db, "user", "external from dashboard")
	if guardWouldReuse(t, r, db, "telegram:s1", "s1") {
		t.Fatal("cross process write did not invalidate the cached agent")
	}
}

func TestInBandFollowupReusesCachedAgent(t *testing.T) {
	r, db := rebaselineRunner(t)
	cachedAgent := &agent.AIAgent{}
	r.agentCache.Set("telegram:s1", agentCacheEntry{agent: cachedAgent, signature: "sig", msgCount: liveMessageCount(t, db, "s1")})
	appendRow(t, db, "user", "u")
	appendRow(t, db, "assistant", "a")
	if guardWouldReuse(t, r, db, "telegram:s1", "s1") {
		t.Fatal("follow up reused before the rebaseline")
	}
	r.refreshAgentCacheMessageCount(context.Background(), "telegram:s1", "s1")
	if !guardWouldReuse(t, r, db, "telegram:s1", "s1") {
		t.Fatal("follow up rebuilt the warm agent after the rebaseline")
	}
	r.agentCacheLock.Lock()
	cached, _ := r.agentCache.Get("telegram:s1")
	r.agentCacheLock.Unlock()
	if cached.agent != cachedAgent {
		t.Fatal("cached agent replaced")
	}
}

// Discord adapter reporting a fixed voice channel context
type vcAdapter struct {
	*gateway.BasePlatformAdapter
	value string
}

func (a *vcAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *vcAdapter) Disconnect(context.Context) error            { return nil }
func (a *vcAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: true}, nil
}
func (a *vcAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}
func (a *vcAdapter) GetVoiceChannelContext(int64) (string, error) { return a.value, nil }

// Discord adapter answering with the given voice context
func newVCAdapter(value string) *vcAdapter {
	a := &vcAdapter{value: value}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(&gateway.PlatformConfig{Enabled: true}, gateway.PlatformDiscord, a)
	return a
}

// Slash interaction raw payload with a guild id
type vcInteraction struct{ guildID any }

func (i vcInteraction) InteractionGuildID() any { return i.guildID }

// Discord channel source of the voice note cases
func vcSource() *gateway.SessionSource {
	return &gateway.SessionSource{Platform: gateway.PlatformDiscord, ChatID: "c1", ChatType: "channel", UserID: strp("u1")}
}

// Runner whose Discord adapter reports the voice context
func vcRunner(value string) (*GatewayRunner, *vcAdapter) {
	adapter := newVCAdapter(value)
	r := allocGatewayRunner()
	r.adapters = adapterDict(gateway.PlatformDiscord, adapter)
	return r, adapter
}

// Voice note of one event, failing on an error
func vcNote(t *testing.T, r *GatewayRunner, source *gateway.SessionSource) (string, bool) {
	t.Helper()
	note, ok, err := r.voiceChannelSidecarNote(context.Background(), &gateway.MessageEvent{RawMessage: vcInteraction{"777"}}, source, "sk")
	if err != nil {
		t.Fatal(err)
	}
	return note, ok
}

func TestFirstSightingInjects(t *testing.T) {
	r, _ := vcRunner("**Voice:** dev-vc (2 members)")
	if note, ok := vcNote(t, r, vcSource()); !ok || !strings.Contains(note, "dev-vc (2 members)") {
		t.Fatalf("note %q", note)
	}
}

func TestUnchangedStateInjectsNothing(t *testing.T) {
	r, _ := vcRunner("**Voice:** dev-vc (2 members)")
	if _, ok := vcNote(t, r, vcSource()); !ok {
		t.Fatal("first sighting injected nothing")
	}
	if note, ok := vcNote(t, r, vcSource()); ok {
		t.Fatalf("unchanged state injected %q", note)
	}
}

func TestMemberChangeInjectsAgain(t *testing.T) {
	r, adapter := vcRunner("**Voice:** dev-vc (2 members)")
	vcNote(t, r, vcSource())
	adapter.value = "**Voice:** dev-vc (3 members)"
	if note, ok := vcNote(t, r, vcSource()); !ok || !strings.Contains(note, "dev-vc (3 members)") {
		t.Fatalf("note %q", note)
	}
}

func TestLeavingChannelInjectsDisconnectNote(t *testing.T) {
	r, adapter := vcRunner("**Voice:** dev-vc (2 members)")
	vcNote(t, r, vcSource())
	adapter.value = ""
	if note, ok := vcNote(t, r, vcSource()); !ok || note == "" {
		t.Fatal("leaving the channel injected nothing")
	}
}

func TestNeverInChannelInjectsNothing(t *testing.T) {
	r, _ := vcRunner("")
	if note, ok := vcNote(t, r, vcSource()); ok {
		t.Fatalf("note %q", note)
	}
}

func TestSecondaryProfileReadsItsOwnBotVoiceContext(t *testing.T) {
	primary, secondary := newVCAdapter(""), newVCAdapter("**Voice:** dev-vc (2 members)")
	r := allocGatewayRunner()
	r.adapters = adapterDict(gateway.PlatformDiscord, primary)
	r.profileAdapters = profileAdapterDict("otto", adapterDict(gateway.PlatformDiscord, secondary))
	source := vcSource()
	source.SetTransportAdapterRef(secondary)
	if note, ok := vcNote(t, r, source); !ok || !strings.Contains(note, "dev-vc (2 members)") {
		t.Fatalf("note %q", note)
	}
}

// Multiplexed runner with a primary and an otto Discord bot reporting their own calls
func multiplexVCRunner(primaryVC, secondaryVC string) *GatewayRunner {
	r, _ := vcRunner(primaryVC)
	r.profileAdapters = profileAdapterDict("otto", adapterDict(gateway.PlatformDiscord, newVCAdapter(secondaryVC)))
	multiplex := true
	r.config = &gateway.GatewayConfig{MultiplexProfiles: &multiplex}
	return r
}

// Discord channel source answered by the otto profile
func secondaryVCSource() *gateway.SessionSource {
	source := vcSource()
	source.Profile = strp("otto")
	return source
}

func TestSecondaryNeverBorrowsThePrimaryCall(t *testing.T) {
	r := multiplexVCRunner("**Voice:** ops-vc (5 members)", "")
	if note, ok := vcNote(t, r, secondaryVCSource()); ok {
		t.Fatalf("secondary borrowed the primary call: %q", note)
	}
}

func TestDisconnectedSecondaryFailsClosed(t *testing.T) {
	r := multiplexVCRunner("**Voice:** ops-vc (5 members)", "")
	r.profileAdapters = profileAdapterDict()
	if note, ok := vcNote(t, r, secondaryVCSource()); ok {
		t.Fatalf("disconnected secondary read a call: %q", note)
	}
}

func TestADesktopClaimOfTheSessionAGatewayTurnJustEndedIsNotAlreadyOwned(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", tmp)
	r := leaseRunner(0)
	src := &gateway.SessionSource{Platform: gateway.PlatformTelegram, ChatID: "c1", UserID: strp("u1")}
	key := "agent:main:telegram:dm:shared"
	stuck := &releaseStuckOnTheRegistry{started: make(chan struct{}), finish: make(chan struct{})}
	gateway.ReleaseSoon(stuck)
	select {
	case <-stuck.started:
	case <-time.After(10 * time.Second):
		t.Fatal("stuck release never started")
	}
	leaseGW := endGatewayTurn(t, r, key, src)
	leaseDesk, refusal := tuigateway.ClaimActiveSessionSlot(context.Background(), key, "desktop-view", "desktop", tmp)
	if refusal != nil {
		t.Fatalf("Desktop saw the ended gateway turn as the session's owner: %s", *refusal)
	}
	drainLeaseWorker(t, leaseGW, stuck)
	entries, err := activesessions.RegistrySnapshot(context.Background(), tmp, true)
	if err != nil {
		t.Fatal(err)
	}
	var got [][2]any
	for _, e := range entries {
		got = append(got, [2]any{e.GetOr("session_id", nil), e.GetOr("surface", nil)})
	}
	if len(got) != 1 || got[0] != [2]any{key, "desktop"} {
		t.Fatalf("registry %v", got)
	}
	if err := leaseDesk.Release(); err != nil {
		t.Fatal(err)
	}
}

// Releasing a session's running agent state while the running agents view reads that session stays race free
func TestReleaseRunningAgentStateWhileRunningAgentsViewReads(t *testing.T) {
	ctx := context.Background()
	r := leaseRunner(0)
	const sessionKey = "agent:main:telegram:dm:42"
	running := &agent.AIAgent{}
	claim := func() {
		r.withSessionState(sessionKey, func(s *SessionState) {
			s.turn.agent, s.turn.event, s.turn.startedTS = running, gateway.NewMessageEvent("hi"), wallClock()
		})
	}
	claim()
	stop, first := make(chan struct{}), make(chan struct{})
	released := make(chan int)
	go func() {
		count := 0
		for {
			select {
			case <-stop:
				released <- count
				return
			default:
			}
			r.releaseRunningAgentState(ctx, sessionKey, nil)
			if count++; count == 1 {
				close(first)
			}
			claim()
		}
	}()
	<-first
	for range 20000 {
		got, ok := r.runningAgents().Get(sessionKey)
		if ok != (got == running) || (!ok && got != nil) {
			t.Fatalf("running agents view read %p present=%v", got, ok)
		}
		if resident := r.ResidentAgentFor(sessionKey); resident != nil && resident != any(running) {
			t.Fatalf("resident agent %v", resident)
		}
	}
	close(stop)
	if count := <-released; count == 0 {
		t.Fatal("the releasing goroutine never released the running agent state")
	}
	if _, ok := r.runningAgents().Get(sessionKey); !ok {
		t.Fatal("the final claim is not visible through the running agents view")
	}
	if !r.releaseRunningAgentState(ctx, sessionKey, nil) {
		t.Fatal("release refused")
	}
	if got, ok := r.runningAgents().Get(sessionKey); ok || got != nil {
		t.Fatalf("running agent %p survived the release", got)
	}
}
