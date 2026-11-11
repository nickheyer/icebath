package plugins

import (
	"reflect"
	"testing"
)

func TestEvictModulesSurvivesAConcurrentMappingInsert(t *testing.T) {
	evictionHosts(t, func(t *testing.T, h string, m *Manager) {
		dispatchLive(t, h, "evict_race_setup", nil)
		if err := evictModules(m, "icebath_plugins.discord_platform"); err != nil {
			t.Fatal(err)
		}
		if got := dispatchLive(t, h, "evict_race_handed_off", nil); got != "true" {
			t.Fatalf("the walk never reached the switch point: %s", got)
		}
		got := hostModuleNames(t, h, "icebath_plugins.discord_platform", "race_probe_sdk", "unrelated")
		if !reflect.DeepEqual(got, []string{"race_probe_sdk.client", "unrelated"}) {
			t.Fatalf("modules after eviction: %v", got)
		}
	})
}
