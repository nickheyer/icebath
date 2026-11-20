package gatewayrun

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	hygieneBase = 300.0
	hygieneKey  = "agent:main:telegram:private:123"
)

// Bare runner whose session database resolves under a per test home, the object new runner of the cases
func hygieneBareRunner(t *testing.T) (*GatewayRunner, context.Context) {
	t.Helper()
	return allocGatewayRunner(), home.WithOverride(context.Background(), t.TempDir())
}

// State database at a temp path closed when the test ends
func hygieneStateDB(t *testing.T) *state.SessionDB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Runner pinned to db, a gateway restarted over the same state database
func hygieneRunnerOver(db *state.SessionDB) *GatewayRunner {
	r := allocGatewayRunner()
	r.pinSessionDB(state.NewAsyncSessionDB(db))
	return r
}

func TestCooldownLadderFirstFailureUsesTheConfiguredBase(t *testing.T) {
	r, ctx := hygieneBareRunner(t)
	if got := hygieneCooldownForFailure(ctx, r, hygieneKey, hygieneBase); got != hygieneBase {
		t.Fatalf("got %v", got)
	}
}

func TestCooldownLadderConsecutiveFailuresEscalate(t *testing.T) {
	r, ctx := hygieneBareRunner(t)
	for i, m := range hygieneCooldownLadderMultipliers {
		if got := hygieneCooldownForFailure(ctx, r, hygieneKey, hygieneBase); got != hygieneBase*m {
			t.Fatalf("rung %d got %v", i, got)
		}
	}
}

func TestCooldownLadderConsecutiveFailuresEscalateAcrossGatewayRestart(t *testing.T) {
	ctx := context.Background()
	db := hygieneStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "before-rotation", Source: "telegram", SessionKey: hygieneKey}); err != nil {
		t.Fatal(err)
	}
	if got := hygieneCooldownForFailure(ctx, hygieneRunnerOver(db), hygieneKey, hygieneBase); got != hygieneBase {
		t.Fatalf("first runner got %v", got)
	}
	if err := db.CreateSession(state.NewSession{ID: "after-rotation", Source: "telegram", SessionKey: hygieneKey, ParentSessionID: "before-rotation"}); err != nil {
		t.Fatal(err)
	}
	if got := hygieneCooldownForFailure(ctx, hygieneRunnerOver(db), hygieneKey, hygieneBase); got != hygieneBase*3 {
		t.Fatalf("restarted runner got %v", got)
	}
	if got := hygieneCooldownForFailure(ctx, hygieneRunnerOver(db), "agent:main:telegram:private:999", hygieneBase); got != hygieneBase {
		t.Fatalf("other chat got %v", got)
	}
}

func TestCooldownLadderLadderSaturatesAtTheTopRung(t *testing.T) {
	r, ctx := hygieneBareRunner(t)
	for range 3 {
		hygieneCooldownForFailure(ctx, r, hygieneKey, hygieneBase)
	}
	top := hygieneBase * hygieneCooldownLadderMultipliers[len(hygieneCooldownLadderMultipliers)-1]
	for range 10 {
		if got := hygieneCooldownForFailure(ctx, r, hygieneKey, hygieneBase); got != top {
			t.Fatalf("got %v want %v", got, top)
		}
	}
}

func TestCooldownLadderResetReturnsToTheFirstRung(t *testing.T) {
	r, ctx := hygieneBareRunner(t)
	for range 3 {
		hygieneCooldownForFailure(ctx, r, hygieneKey, hygieneBase)
	}
	resetHygieneFailureStreak(ctx, r, hygieneKey)
	if got := r.sessionState(hygieneKey).persistent.hygieneFailureStreak; got != 0 {
		t.Fatalf("streak %d", got)
	}
	if got := hygieneCooldownForFailure(ctx, r, hygieneKey, hygieneBase); got != hygieneBase {
		t.Fatalf("got %v", got)
	}
}

func TestCooldownLadderResetReturnsRestartedGatewayToFirstRung(t *testing.T) {
	ctx := context.Background()
	db := hygieneStateDB(t)
	r := hygieneRunnerOver(db)
	if got := hygieneCooldownForFailure(ctx, r, hygieneKey, hygieneBase); got != hygieneBase {
		t.Fatalf("got %v", got)
	}
	resetHygieneFailureStreak(ctx, r, hygieneKey)
	if got := hygieneCooldownForFailure(ctx, hygieneRunnerOver(db), hygieneKey, hygieneBase); got != hygieneBase {
		t.Fatalf("restarted got %v", got)
	}
}

func TestCooldownLadderStreaksArePerSession(t *testing.T) {
	r, ctx := hygieneBareRunner(t)
	for range 3 {
		hygieneCooldownForFailure(ctx, r, hygieneKey, hygieneBase)
	}
	if got := hygieneCooldownForFailure(ctx, r, "agent:main:telegram:private:999", hygieneBase); got != hygieneBase {
		t.Fatalf("got %v", got)
	}
}

func TestCooldownLadderAbsoluteCapBoundsALargeOperatorBase(t *testing.T) {
	r, ctx := hygieneBareRunner(t)
	highest := 0.0
	for range 4 {
		v := hygieneCooldownForFailure(ctx, r, hygieneKey, 3600.0)
		if v > hygieneCooldownMaxSeconds {
			t.Fatalf("over cap %v", v)
		}
		highest = math.Max(highest, v)
	}
	if highest != hygieneCooldownMaxSeconds {
		t.Fatalf("max %v", highest)
	}
}

func TestCooldownLadderCapDoesNotShrinkTheConfiguredBase(t *testing.T) {
	r, ctx := hygieneBareRunner(t)
	if got := hygieneCooldownForFailure(ctx, r, hygieneKey, hygieneCooldownMaxSeconds*2); math.Abs(got-hygieneCooldownMaxSeconds) > 1e-6*hygieneCooldownMaxSeconds {
		t.Fatalf("got %v", got)
	}
}

func TestCooldownLadderZeroBaseStaysZero(t *testing.T) {
	r, ctx := hygieneBareRunner(t)
	for range 2 {
		if got := hygieneCooldownForFailure(ctx, r, hygieneKey, 0.0); got != 0.0 {
			t.Fatalf("got %v", got)
		}
	}
}

// Stand in whose session state accessors blow up and which has no state database
type explodingHygieneGateway struct{}

func (explodingHygieneGateway) sessionState(string) *SessionState {
	panic("no sessions map")
}
func (explodingHygieneGateway) peekSessionState(string) *SessionState {
	panic("'_Exploding' object has no attribute '_peek_session_state'")
}
func (explodingHygieneGateway) SessionDB(context.Context) *state.SessionDB { return nil }

func TestDegradedRunnersRunnerWhoseSessionStateRaisesStillCoolsDown(t *testing.T) {
	ctx := context.Background()
	if got := hygieneCooldownForFailure(ctx, explodingHygieneGateway{}, hygieneKey, hygieneBase); got != hygieneBase {
		t.Fatalf("got %v", got)
	}
	resetHygieneFailureStreak(ctx, explodingHygieneGateway{}, hygieneKey)
}

func TestFailureReasonForwardedReasonIsForwardedWhenSupplied(t *testing.T) {
	db := hygieneStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "sid-1", Source: "telegram"}); err != nil {
		t.Fatal(err)
	}
	reason := "summary model timed out"
	recordHygieneCooldown(context.Background(), hygieneRunnerOver(db), "sid-1", 300.0, &reason)
	cooldown, err := db.GetCompressionFailureCooldown("sid-1")
	if err != nil || cooldown == nil {
		t.Fatalf("cooldown %v %v", cooldown, err)
	}
	if got := cooldown.GetOr("error", nil); got != reason {
		t.Fatalf("error %v", got)
	}
}

// Recovery predicate over the base rotation case with the given overrides applied
func recoveredWith(over map[string]any) bool {
	args := map[string]any{"aborted": false, "rotated": true, "in_place": false,
		"msg_count": 220, "new_count": 100, "approx_tokens": 50_000, "new_tokens": 30_000}
	for k, v := range over {
		args[k] = v
	}
	return hygieneCompactionRecovered(args["aborted"].(bool), args["rotated"].(bool), args["in_place"].(bool),
		args["msg_count"].(int), args["new_count"].(int), args["approx_tokens"].(int), args["new_tokens"].(int))
}

func TestHygieneCompactionRecoveredRealRotationWithReductionIsRecovery(t *testing.T) {
	if !recoveredWith(nil) {
		t.Fatal("not recovered")
	}
}

func TestHygieneCompactionRecoveredAbortIsNeverRecovery(t *testing.T) {
	if recoveredWith(map[string]any{"aborted": true}) {
		t.Fatal("recovered")
	}
}

func TestHygieneCompactionRecoveredNoRewriteIsNeverRecoveryEvenWhenCountsLookGood(t *testing.T) {
	if !agent.CompressionMadeProgress(220, 100, 50_000, 30_000) {
		t.Fatal("counts do not read as progress")
	}
	if recoveredWith(map[string]any{"rotated": false, "in_place": false}) {
		t.Fatal("recovered")
	}
}

func TestHygieneCompactionRecoveredInPlaceCompactionCounts(t *testing.T) {
	if !recoveredWith(map[string]any{"rotated": false, "in_place": true}) {
		t.Fatal("not recovered")
	}
}

func TestHygieneCompactionRecoveredRowDropWithFlatTokensIsRecovery(t *testing.T) {
	if !recoveredWith(map[string]any{"new_count": 100, "new_tokens": 50_000}) {
		t.Fatal("not recovered")
	}
}

func TestHygieneCompactionRecoveredSizeOnlyWinIsRecovery(t *testing.T) {
	if !recoveredWith(map[string]any{"msg_count": 220, "new_count": 220, "approx_tokens": 288_000, "new_tokens": 183_000}) {
		t.Fatal("not recovered")
	}
}

func TestHygieneCompactionRecoveredSubFivePercentWobbleIsNotRecovery(t *testing.T) {
	if recoveredWith(map[string]any{"msg_count": 220, "new_count": 220, "approx_tokens": 50_000, "new_tokens": 49_900}) {
		t.Fatal("recovered")
	}
}

func TestHygieneWaitShouldExtendExtendsWhileIdleAndUnderCeiling(t *testing.T) {
	if !hygieneWaitShouldExtend(1.0, 30.0, 10.0, 600.0, false) {
		t.Fatal("stopped")
	}
}

func TestHygieneWaitShouldExtendStopsWhenIdleBudgetExhausted(t *testing.T) {
	if hygieneWaitShouldExtend(30.0, 30.0, 10.0, 600.0, false) {
		t.Fatal("extended")
	}
}

func TestHygieneWaitShouldExtendStopsAtCeiling(t *testing.T) {
	if hygieneWaitShouldExtend(1.0, 30.0, 600.0, 600.0, false) {
		t.Fatal("extended")
	}
}

func TestHygieneWaitShouldExtendFenceCancelStopsEvenWithFreshProgress(t *testing.T) {
	if hygieneWaitShouldExtend(0.0, 30.0, 0.1, 600.0, true) {
		t.Fatal("extended")
	}
}

func TestRecordedCooldownEscalatesPersistedDeadlinesGrow(t *testing.T) {
	ctx := context.Background()
	db := hygieneStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "sess-1", Source: "telegram"}); err != nil {
		t.Fatal(err)
	}
	r := allocGatewayRunner()
	r.pinSessionDB(nil)
	recorder := hygieneRunnerOver(db)
	now := float64(time.Now().UnixNano()) / 1e9
	var waits []float64
	for range 3 {
		recordHygieneCooldown(ctx, recorder, "sess-1", hygieneCooldownForFailure(ctx, r, hygieneKey, hygieneBase), nil)
		cooldown, err := db.GetCompressionFailureCooldown("sess-1")
		if err != nil || cooldown == nil {
			t.Fatalf("cooldown %v %v", cooldown, err)
		}
		until, _ := pyint.Float(cooldown.GetOr("cooldown_until", 0))
		waits = append(waits, until-now)
	}
	if !(waits[0] < waits[1] && waits[1] < waits[2]) {
		t.Fatalf("waits %v", waits)
	}
}
