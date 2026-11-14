package dedupe

import (
	"os"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

func at(ms int) time.Time { return time.UnixMilli(int64(ms)) }

func TestCollapsesTheSameKeyInsideTheWindowTwoWindowsOneEvent(t *testing.T) {
	isDup := NewEventDeduper(time.Second)
	if isDup("input:s1", at(0)) || !isDup("input:s1", at(5)) {
		t.Fatal("second window was not deduped")
	}
}

func TestDistinctKeysAreIndependent(t *testing.T) {
	isDup := NewEventDeduper(time.Second)
	if isDup("input:s1", at(0)) || isDup("approval:s1", at(0)) || isDup("input:s2", at(0)) {
		t.Fatal("distinct keys collided")
	}
}

func TestReFiresOnceTheWindowElapses(t *testing.T) {
	isDup := NewEventDeduper(time.Second)
	if isDup("turnDone:s1", at(0)) || !isDup("turnDone:s1", at(999)) || isDup("turnDone:s1", at(1000)) {
		t.Fatal("window")
	}
}

func TestASpokenReplyStaysClaimedLongAfterTheTickWindowABeepDoesNot(t *testing.T) {
	owns := NewAmbientClaimArbiter(time.Second, SpeechClaimTTL)
	if !owns("speak:m1", at(0)) || owns("speak:m1", at(5000)) {
		t.Fatal("speech claim")
	}
	if !owns("sound:turnDone:s1", at(0)) || !owns("sound:turnDone:s1", at(5000)) {
		t.Fatal("beep keys still re-fire after the tick window")
	}
}

func TestASpokenReplyCanBeClaimedAgainOnceTheSpeechTtlElapses(t *testing.T) {
	owns := NewAmbientClaimArbiter(time.Second, time.Minute)
	if !owns("speak:m1", at(0)) || !owns("speak:m1", at(60000)) {
		t.Fatal("speech TTL")
	}
}
