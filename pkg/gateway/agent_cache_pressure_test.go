package gateway

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/memory"
)

// test_agent_cache_pressure.py TestBoundsResolution test_absent_section_leaves_lru_bounds_unset
func TestAbsentSectionLeavesLruBoundsUnset(t *testing.T) {
	bounds := ResolveAgentCacheBounds(map[string]any{})
	if bounds.MaxSize != nil || bounds.IdleTTLSecs != nil {
		t.Fatalf("bounds %+v", bounds)
	}
}

// test_agent_cache_pressure.py TestBoundsResolution test_configured_values_are_honoured
func TestConfiguredValuesAreHonoured(t *testing.T) {
	bounds := ResolveAgentCacheBounds(map[string]any{"agent": map[string]any{"agent_cache": map[string]any{
		"max_size": 32, "idle_ttl_secs": 600, "memory_high_mb": 2048, "max_evictions_per_pass": 4, "protect_recent": 2,
	}}})
	if bounds.MaxSize == nil || *bounds.MaxSize != 32 || bounds.IdleTTLSecs == nil || *bounds.IdleTTLSecs != 600.0 ||
		bounds.MemoryHighMB == nil || *bounds.MemoryHighMB != 2048 || bounds.MaxEvictionsPerPass != 4 || bounds.ProtectRecent != 2 {
		t.Fatalf("bounds %+v", bounds)
	}
}

// test_agent_cache_pressure.py TestBoundsResolution test_garbage_values_fall_back_to_defaults
func TestGarbageValuesFallBackToDefaults(t *testing.T) {
	bounds := ResolveAgentCacheBounds(map[string]any{"agent": map[string]any{"agent_cache": map[string]any{"max_size": "lots", "idle_ttl_secs": -5}}})
	if bounds.MaxSize != nil || bounds.IdleTTLSecs != nil || bounds.MaxEvictionsPerPass <= 0 {
		t.Fatalf("bounds %+v", bounds)
	}
}

// test_agent_cache_pressure.py TestBoundsResolution test_protect_recent_zero_is_respected
func TestProtectRecentZeroIsRespected(t *testing.T) {
	if bounds := ResolveAgentCacheBounds(map[string]any{"agent": map[string]any{"agent_cache": map[string]any{"protect_recent": 0}}}); bounds.ProtectRecent != 0 {
		t.Fatalf("protect_recent %d", bounds.ProtectRecent)
	}
}

// test_agent_cache_pressure.py TestMemoryBudgetResolution test_falsy_settings_disable_the_pass
func TestFalsySettingsDisableThePass(t *testing.T) {
	for _, setting := range []any{0, false, nil, "off", "none", ""} {
		if got := ResolveMemoryHighMB(setting); got != nil {
			t.Fatalf("%#v gave %d", setting, *got)
		}
	}
}

// test_agent_cache_pressure.py TestMemoryBudgetResolution test_explicit_budget_is_taken_literally
func TestExplicitBudgetIsTakenLiterally(t *testing.T) {
	for _, setting := range []any{4096, "4096", 4096.0} {
		if got := ResolveMemoryHighMB(setting); got == nil || *got != 4096 {
			t.Fatalf("%#v gave %v", setting, got)
		}
	}
}

// test_agent_cache_pressure.py TestMemoryBudgetResolution test_auto_derives_a_budget_below_the_cgroup_limit
func TestAutoDerivesABudgetBelowTheCgroupLimit(t *testing.T) {
	limitMB := int64(10 * 1024)
	saved := acpCgroupLimitBytes
	acpCgroupLimitBytes = func() *int64 { v := limitMB * 1024 * 1024; return &v }
	t.Cleanup(func() { acpCgroupLimitBytes = saved })
	budget := ResolveMemoryHighMB("auto")
	if budget == nil || *budget <= 0 || int64(*budget) >= limitMB {
		t.Fatalf("budget %v", budget)
	}
}

// test_agent_cache_pressure.py TestMemoryBudgetResolution test_auto_is_disabled_when_no_limit_is_discoverable
func TestAutoIsDisabledWhenNoLimitIsDiscoverable(t *testing.T) {
	savedLimit, savedTotal := acpCgroupLimitBytes, acpTotalMemoryBytes
	acpCgroupLimitBytes = func() *int64 { return nil }
	acpTotalMemoryBytes = func() *int64 { return nil }
	t.Cleanup(func() { acpCgroupLimitBytes, acpTotalMemoryBytes = savedLimit, savedTotal })
	if got := ResolveMemoryHighMB("auto"); got != nil {
		t.Fatalf("budget %d", *got)
	}
}

// Routes the own cgroup and its high limit and stat files to fixed text, a nil stat raising
func installPressureCgroup(t *testing.T, statText *string) {
	savedOwn, savedRead, savedSnap := cgroupOwnPath, acpReadText, acpMemorySnapshot
	cgroupOwnPath = func() (string, error) { return "/hermes.service", nil }
	acpReadText = func(path string) (string, error) {
		switch path {
		case "/sys/fs/cgroup/hermes.service/memory.high":
			return "6979321856\n", nil
		case "/sys/fs/cgroup/hermes.service/memory.stat":
			if statText == nil {
				return "", errors.New("restricted /sys mount")
			}
			return *statText, nil
		}
		return savedRead(path)
	}
	anon := 1_600 * 1024
	acpMemorySnapshot = func() memory.MemorySnapshot { return memory.MemorySnapshot{RSSAnonKiB: &anon} }
	t.Cleanup(func() { cgroupOwnPath, acpReadText, acpMemorySnapshot = savedOwn, savedRead, savedSnap })
}

// test_agent_cache_pressure.py TestPressureSignalScope test_same_cgroup_child_anon_counts_against_the_budget
func TestSameCgroupChildAnonCountsAgainstTheBudget(t *testing.T) {
	stat := "anon 4928307200\nfile 5426061312\nkernel 3629735936\n"
	installPressureCgroup(t, &stat)
	if got := ReadAnonRSSMB(); got == nil || *got != 4_700 {
		t.Fatalf("anon rss %v", got)
	}
}

// test_agent_cache_pressure.py TestPressureSignalScope test_unreadable_cgroup_stat_keeps_the_self_reading
func TestUnreadableCgroupStatKeepsTheSelfReading(t *testing.T) {
	installPressureCgroup(t, nil)
	if got := ReadAnonRSSMB(); got == nil || *got != 1_600 {
		t.Fatalf("anon rss %v", got)
	}
}

// Session keyed planner entries s0 upward
func plannerEntries(n int) []PressureEntry[int] {
	out := make([]PressureEntry[int], n)
	for i := range out {
		out[i] = PressureEntry[int]{Key: fmt.Sprintf("s%d", i), Agent: i}
	}
	return out
}

// Keys of a pressure plan in order
func planKeys(plan []PressureEntry[int]) []string {
	keys := []string{}
	for _, e := range plan {
		keys = append(keys, e.Key)
	}
	return keys
}

func evictAll(string, int) bool { return true }

// test_agent_cache_pressure.py TestEvictionPlanner test_evicts_least_recently_used_first
func TestEvictsLeastRecentlyUsedFirst(t *testing.T) {
	if got := planKeys(PlanPressureEvictions(plannerEntries(6), evictAll, 2, 0)); !reflect.DeepEqual(got, []string{"s0", "s1"}) {
		t.Fatalf("plan %v", got)
	}
}

// test_agent_cache_pressure.py TestEvictionPlanner test_never_touches_the_protected_tail
func TestNeverTouchesTheProtectedTail(t *testing.T) {
	if got := planKeys(PlanPressureEvictions(plannerEntries(10), evictAll, 10, 3)); !reflect.DeepEqual(got, []string{"s0", "s1", "s2", "s3", "s4", "s5", "s6"}) {
		t.Fatalf("plan %v", got)
	}
}

// test_agent_cache_pressure.py TestEvictionPlanner test_a_small_cache_of_large_transcripts_is_still_shedable
func TestASmallCacheOfLargeTranscriptsIsStillShedable(t *testing.T) {
	for _, size := range []int{1, 2, 3, 5} {
		plan := PlanPressureEvictions(plannerEntries(size), evictAll, 10, 8)
		if len(plan) == 0 || len(plan) > size {
			t.Fatalf("size %d plan %v", size, planKeys(plan))
		}
	}
}

// test_agent_cache_pressure.py TestEvictionPlanner test_protection_still_keeps_the_hottest_session
func TestProtectionStillKeepsTheHottestSession(t *testing.T) {
	for _, key := range planKeys(PlanPressureEvictions(plannerEntries(4), evictAll, 10, 8)) {
		if key == "s3" {
			t.Fatal("the hottest session was planned")
		}
	}
}

// test_agent_cache_pressure.py TestEvictionPlanner test_skipped_candidates_do_not_consume_the_batch
func TestSkippedCandidatesDoNotConsumeTheBatch(t *testing.T) {
	plan := PlanPressureEvictions(plannerEntries(6), func(k string, _ int) bool { return k != "s0" }, 2, 0)
	if got := planKeys(plan); !reflect.DeepEqual(got, []string{"s1", "s2"}) {
		t.Fatalf("plan %v", got)
	}
}

// test_agent_cache_pressure.py TestSalvageFollowups test_protect_recent_yaml_false_keeps_default
func TestProtectRecentYamlFalseKeepsDefault(t *testing.T) {
	if bounds := ResolveAgentCacheBounds(map[string]any{"agent": map[string]any{"agent_cache": map[string]any{"protect_recent": false}}}); bounds.ProtectRecent <= 0 {
		t.Fatalf("protect_recent %d", bounds.ProtectRecent)
	}
}
