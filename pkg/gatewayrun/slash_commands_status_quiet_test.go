package gatewayrun

import (
	"errors"
	"testing"

	"github.com/nickheyer/icebath/pkg/agent"
)

func TestQuietYieldsTheFallbackWhenTheCallFails(t *testing.T) {
	if got := quiet(func() (int, error) { return 7, nil }, -1); got != 7 {
		t.Fatalf("clean call gave %d", got)
	}
	if got := quiet(func() (int, error) { return 7, errors.New("locked") }, -1); got != -1 {
		t.Fatalf("failed call gave %d", got)
	}
	if got := quiet(func() (int, error) { panic("broken") }, -1); got != -1 {
		t.Fatalf("broken call gave %d", got)
	}
}

func TestQuietSyncYieldsTheFallbackWhenTheCallBreaks(t *testing.T) {
	if got := quietSync(func() []string { return []string{"a"} }, nil); len(got) != 1 || got[0] != "a" {
		t.Fatalf("clean call gave %v", got)
	}
	if got := quietSync(func() []string { panic("broken") }, []string{}); got == nil || len(got) != 0 {
		t.Fatalf("broken call gave %v", got)
	}
}

func TestQuietBoolIsFalseWhenTheProbeBreaks(t *testing.T) {
	if !quietBool(func() bool { return true }) || quietBool(func() bool { return false }) {
		t.Fatal("clean probe was not passed through")
	}
	if quietBool(func() bool { panic("broken") }) {
		t.Fatal("broken probe gave true")
	}
}

func TestAgentCountIsZeroWithoutAnAgent(t *testing.T) {
	if got := agentCount(nil, (*agent.AIAgent).SessionAPICalls); got != 0 {
		t.Fatalf("no agent gave %d", got)
	}
	if got := agentCount(&agent.AIAgent{}, func(*agent.AIAgent) int { return 12 }); got != 12 {
		t.Fatalf("agent gave %d", got)
	}
}

func TestTaskCardCompactCollapsesEveryWhitespaceRunAndCuts(t *testing.T) {
	if got := taskCardCompact("  run the\t\n tests\v now ", 120); got != "run the tests now" {
		t.Fatalf("got %q", got)
	}
	if got := taskCardCompact(nil, 120); got != "" {
		t.Fatalf("nil gave %q", got)
	}
	if got := taskCardCompact("abcdefg hijkl", 10); got != "abcdefg..." {
		t.Fatalf("cut gave %q", got)
	}
	if got := taskCardCompact("abcdef ghijkl", 10); got != "abcdef..." {
		t.Fatalf("cut at a space gave %q", got)
	}
}

func TestLeaseTokenAndFieldViewDebugText(t *testing.T) {
	token := &TurnLeaseToken{sessionID: "s1", ownerKey: "agent:main:local:dm:1", generation: 3}
	if got, want := token.String(), "TurnLeaseToken(session_id='s1', owner_key='agent:main:local:dm:1', generation=3, released=False)"; got != want {
		t.Fatalf("got %q", got)
	}
	r := allocGatewayRunner()
	r.busyAckTS().Set("k1", 2.5)
	if got, want := r.busyAckTS().String(), "SessionFieldView(turn.busy_ack_ts, map[k1:2.5])"; got != want {
		t.Fatalf("got %q", got)
	}
}

func TestLoginRegistryCreatesTheAttemptTableOnce(t *testing.T) {
	r := allocGatewayRunner()
	lock, attempts := r.loginRegistry()
	lock.Lock()
	attempts[loginSingle] = &signInAttempt{attemptID: "a1"}
	lock.Unlock()
	againLock, again := r.loginRegistry()
	if againLock != lock || again[loginSingle] == nil || again[loginSingle].attemptID != "a1" {
		t.Fatal("the registry was created twice")
	}
}
