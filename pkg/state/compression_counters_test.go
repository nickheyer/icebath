package state

import (
	"strings"
	"testing"
)

func TestCompressionOverloadStreakAndRecoveryDeadlineRoundTrip(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "s", Source: "cli"})
	if v, err := db.CompressionOverloadStreak("s"); v != 0 || err != nil {
		t.Fatalf("fresh streak %d %v", v, err)
	}
	if err := db.SetCompressionOverloadStreak("s", -4); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.CompressionOverloadStreak("s"); v != 0 {
		t.Fatalf("negative streak stored %d", v)
	}
	for want := 1; want <= 2; want++ {
		got, err := db.IncrementCompressionOverloadStreak("s")
		if err != nil || got == nil || *got != want {
			t.Fatalf("increment %v %v want %d", got, err, want)
		}
	}
	if got, err := db.IncrementCompressionOverloadStreak("ghost"); got != nil || err != nil {
		t.Fatalf("missing row increment %v %v", got, err)
	}
	if got, _ := db.IncrementCompressionOverloadStreak(""); got != nil {
		t.Fatal("empty session id incremented")
	}
	if v, _ := db.CompressionOverloadStreak("s"); v != 2 {
		t.Fatalf("streak %d", v)
	}
	if err := db.SetCompressionRecoveryDeadline("s", 1234.5); err != nil {
		t.Fatal(err)
	}
	if v, _ := db.CompressionRecoveryDeadline("s"); v != 1234.5 {
		t.Fatalf("deadline %v", v)
	}
	if err := db.SetCompressionRecoveryDeadline("s", -3); err != nil {
		t.Fatal(err)
	}
	if v := repairScalar(t, db, "SELECT compression_recovery_deadline FROM sessions WHERE id = 's'"); v != nil {
		t.Fatalf("disarmed deadline stored %v", v)
	}
	if v, _ := db.CompressionRecoveryDeadline("ghost"); v != 0 {
		t.Fatalf("missing deadline %v", v)
	}
	profileExec(t, db, "UPDATE sessions SET compression_fallback_streak = 'junk', compression_ineffective_count = '7' WHERE id = 's'")
	if v, err := db.CompressionFallbackStreak("s"); v != 0 || err != nil {
		t.Fatalf("unparsable streak %d %v", v, err)
	}
	if v, _ := db.CompressionIneffectiveCount("s"); v != 7 {
		t.Fatalf("text count %d", v)
	}
	if err := db.SetCompressionIneffectiveCount("s", 3); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCompressionFallbackStreak("s", 2); err != nil {
		t.Fatal(err)
	}
	if a, _ := db.CompressionIneffectiveCount("s"); a != 3 {
		t.Fatalf("count %d", a)
	}
	if b, _ := db.CompressionFallbackStreak("s"); b != 2 {
		t.Fatalf("streak %d", b)
	}
}

func TestCompressionFailureCooldownMergeMaxAndRowRestore(t *testing.T) {
	db := usageDB(t)
	readSession(t, db, NewSession{ID: "s", Source: "cli"})
	far := now() + 600
	first := "first"
	db.RecordCompressionFailureCooldown("s", far, &first)
	second := "second"
	db.RecordCompressionFailureCooldown("s", now()+10, &second)
	live, err := db.GetCompressionFailureCooldown("s")
	if err != nil || live == nil {
		t.Fatalf("live cooldown %v %v", live, err)
	}
	if rowValue(live, "cooldown_until") != far || rowValue(live, "error") != "second" {
		t.Fatalf("merge max lost the longer deadline %v", live)
	}
	agentView, _ := db.CompressionCooldown("s")
	if agentView.Until != far || agentView.Error != "second" || agentView.RemainingSeconds <= 0 {
		t.Fatalf("agent view %+v", agentView)
	}
	snapshot, err := db.GetCompressionFailureCooldownRow("s")
	if err != nil || !snapshot.SessionExists || snapshot.CooldownUntil == nil || *snapshot.CooldownUntil != far {
		t.Fatalf("snapshot %+v %v", snapshot, err)
	}
	db.ClearCompressionFailureCooldown("s")
	if live, _ := db.GetCompressionFailureCooldown("s"); live != nil {
		t.Fatal("clear left a cooldown")
	}
	if err := db.RestoreCompressionFailureCooldownRow("s", snapshot); err != nil {
		t.Fatal(err)
	}
	if again, _ := db.GetCompressionFailureCooldownRow("s"); *again.CooldownUntil != far || again.Error != "second" {
		t.Fatalf("restored %+v", again)
	}
	expired := now() - 5
	db.ClearCompressionFailureCooldown("s")
	db.RecordCompressionFailureCooldown("s", expired, nil)
	if live, _ := db.GetCompressionFailureCooldown("s"); live != nil {
		t.Fatal("expired cooldown reported live")
	}
	if row, _ := db.GetCompressionFailureCooldownRow("s"); row.CooldownUntil == nil || *row.CooldownUntil != expired || row.Error != nil {
		t.Fatalf("expired row %+v", row)
	}
	absent, _ := db.GetCompressionFailureCooldownRow("ghost")
	if absent.SessionExists || absent.CooldownUntil != nil {
		t.Fatalf("absent row %+v", absent)
	}
	if err := db.RestoreCompressionFailureCooldownRow("ghost", absent); err != nil {
		t.Fatal(err)
	}
	err = db.RestoreCompressionFailureCooldownRow("s", absent)
	if err == nil || !strings.Contains(err.Error(), "cannot restore absent compression cooldown row: session now exists") {
		t.Fatalf("absent snapshot over a live row %v", err)
	}
	if err := db.RestoreCompressionFailureCooldownRow("vanished", snapshot); err != nil {
		t.Fatalf("vanished row restore %v", err)
	}
	if err := db.SetCompressionCooldown("s", far, ""); err != nil {
		t.Fatal(err)
	}
	if row, _ := db.GetCompressionFailureCooldownRow("s"); row.Error != nil {
		t.Fatalf("empty reason stored %v", row.Error)
	}
	if err := db.ClearCompressionCooldown("s"); err != nil {
		t.Fatal(err)
	}
}
