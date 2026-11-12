package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestHealCountersAreLruBoundedAndNoSessionIsEverRenotified(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("agent:\n  sanitizer_heal_escalation_threshold: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyHealLogLock.Lock()
	saved := []any{emptyHealLogState, emptyHealLogOrder, emptyHealUserNotified, emptyHealPendingNotice}
	emptyHealLogState, emptyHealLogOrder = map[string]*emptyHealState{}, nil
	emptyHealUserNotified, emptyHealPendingNotice = map[string]bool{}, map[string]string{}
	emptyHealLogLock.Unlock()
	t.Cleanup(func() {
		emptyHealLogLock.Lock()
		emptyHealLogState = saved[0].(map[string]*emptyHealState)
		emptyHealLogOrder, _ = saved[1].([]string)
		emptyHealUserNotified = saved[2].(map[string]bool)
		emptyHealPendingNotice = saved[3].(map[string]string)
		emptyHealLogLock.Unlock()
	})
	ctx := context.Background()
	for _, sid := range []string{"busy", "quiet"} {
		logEmptyNonFinalHeal(ctx, sid, 1)
		if consumePendingSanitizerHealNotice(sid) == "" {
			t.Fatalf("%s got no first notice", sid)
		}
	}
	for i := range 3000 {
		sid := fmt.Sprintf("s%d", i)
		logEmptyNonFinalHeal(ctx, sid, 1)
		consumePendingSanitizerHealNotice(sid)
		if i%100 == 0 {
			logEmptyNonFinalHeal(ctx, "busy", 1)
		}
	}
	emptyHealLogLock.Lock()
	size, busy, quiet := len(emptyHealLogState), emptyHealLogState["busy"] != nil, emptyHealLogState["quiet"] != nil
	emptyHealLogLock.Unlock()
	if size > 1024 || !busy || quiet {
		t.Fatalf("size %d busy %v quiet %v", size, busy, quiet)
	}
	for _, sid := range []string{"busy", "quiet"} {
		logEmptyNonFinalHeal(ctx, sid, 1)
		if notice := consumePendingSanitizerHealNotice(sid); notice != "" {
			t.Fatalf("%s was notified twice: %q", sid, notice)
		}
	}
	if got := getSanitizerHealStats()["busy"]["heal_events"]; got != 32 {
		t.Fatalf("busy heal_events %v", got)
	}
}
