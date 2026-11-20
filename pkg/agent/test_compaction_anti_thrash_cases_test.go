package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/state"
)

// Fresh session database at a temp path with the named cli sessions created
func caseSessionDB(t *testing.T, ids ...string) *state.SessionDB {
	t.Helper()
	db, err := state.NewSessionDB(context.Background(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, id := range ids {
		if err := db.CreateSession(state.NewSession{ID: id, Source: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestFutilityGuardModelSwitchResetsAndPersistsFallbackStreak(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := caseSessionDB(t, "s1")
	window := 40960
	o := DefaultContextCompressorOptions("test-model")
	o.ThresholdPercent, o.ProtectFirstN, o.ProtectLastN, o.QuietMode, o.ConfigContextLength, o.Provider = 0.75, 5, 20, true, &window, "test"
	c := NewContextCompressor(context.Background(), o)
	c.setThresholdTokens(24_576)
	c.bindSessionState(db, "s1")
	c.RecordCompletedCompaction(true)
	if streak, _ := db.CompressionFallbackStreak("s1"); c.fallbackCompressionStreak != 1 || streak != 1 {
		t.Fatalf("fallback compaction streak %d persisted %d", c.fallbackCompressionStreak, streak)
	}
	c.UpdateModel("next-model", 100_000, "", "", "", "")
	streak, err := db.CompressionFallbackStreak("s1")
	if c.fallbackCompressionStreak != 0 || err != nil || streak != 0 {
		t.Fatalf("after model switch streak %d persisted %d %v", c.fallbackCompressionStreak, streak, err)
	}
}
