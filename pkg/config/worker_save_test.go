package config

import (
	"reflect"
	"testing"
)

func TestWorkerRebaseKeepsLiveDeletions(t *testing.T) {
	server := func() map[string]any {
		return map[string]any{"command": "stale-cmd", "args": []any{"--x"}, "enabled": false}
	}
	enabled := func() map[string]any { s := server(); s["enabled"] = true; return s }
	liveCmd := func(on bool) map[string]any { s := server(); s["command"] = "live-cmd"; s["enabled"] = on; return s }
	cases := []struct{ live, base, desired, want map[string]any }{
		{map[string]any{"mcp_servers": map[string]any{}}, map[string]any{"mcp_servers": map[string]any{"revoked": server()}},
			map[string]any{"mcp_servers": map[string]any{"revoked": enabled()}}, map[string]any{"mcp_servers": map[string]any{}}},
		{map[string]any{"model": "m"}, map[string]any{"model": "m", "mcp_servers": map[string]any{"revoked": server()}},
			map[string]any{"model": "m", "mcp_servers": map[string]any{"revoked": enabled()}}, map[string]any{"model": "m"}},
		{map[string]any{"mcp_servers": map[string]any{"revoked": "off"}}, map[string]any{"mcp_servers": map[string]any{"revoked": server()}},
			map[string]any{"mcp_servers": map[string]any{"revoked": enabled()}}, map[string]any{"mcp_servers": map[string]any{"revoked": "off"}}},
		{map[string]any{}, map[string]any{"command_allowlist": []any{"a *"}}, map[string]any{"command_allowlist": []any{"a *", "b *"}}, map[string]any{}},
		{map[string]any{"mcp_servers": map[string]any{"revoked": liveCmd(false)}}, map[string]any{"mcp_servers": map[string]any{"revoked": server()}},
			map[string]any{"mcp_servers": map[string]any{"revoked": enabled()}}, map[string]any{"mcp_servers": map[string]any{"revoked": liveCmd(true)}}},
		{map[string]any{"mcp_servers": map[string]any{}}, map[string]any{"mcp_servers": map[string]any{}},
			map[string]any{"mcp_servers": map[string]any{"new": enabled()}}, map[string]any{"mcp_servers": map[string]any{"new": enabled()}}},
	}
	for i, c := range cases {
		if got := threeWay(c.live, c.desired, c.base); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("case %d: %v", i, got)
		}
	}
}

func TestThreeWayListEditsAndDefaultOnlyBranches(t *testing.T) {
	got := threeWay([]any{"a", "live", "b"}, []any{"a", "new"}, []any{"a", "b"})
	if !reflect.DeepEqual(got, []any{"a", "live", "new"}) {
		t.Fatalf("list %v", got)
	}
	base := map[string]any{"approvals": map[string]any{"always": []any{}}}
	desired := map[string]any{"approvals": map[string]any{"always": []any{"ls"}}}
	if got := threeWayFrozen(map[string]any{}, desired, base, map[string]any{}); !reflect.DeepEqual(got, desired) {
		t.Fatalf("default only branch %v", got)
	}
	if got := threeWayFrozen(map[string]any{}, desired, base, base); !reflect.DeepEqual(got, map[string]any{}) {
		t.Fatalf("operator deleted branch %v", got)
	}
}

func TestRebaseForSaveOutsideAWorkerMergesPartialSaves(t *testing.T) {
	freshWorkerPolicy(t)
	live := map[string]any{"model": map[string]any{"default": "a", "provider": "p"}, "keep": 1, "agent": map[string]any{"x": 1}}
	cfg := map[string]any{"model": map[string]any{"default": "b"}, "agent": nil}
	loader := func() map[string]any { t.Fatal("loader consulted outside a worker"); return nil }
	got := RebaseForSave(cfg, live, true, loader)
	want := map[string]any{"model": map[string]any{"default": "b", "provider": "p"}, "keep": 1, "agent": nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged %v", got)
	}
	if got := RebaseForSave(cfg, live, false, loader); !reflect.DeepEqual(got, cfg) {
		t.Fatalf("whole save %v", got)
	}
	if got := RebaseForSave(cfg, nil, true, loader); !reflect.DeepEqual(got, cfg) {
		t.Fatalf("empty live %v", got)
	}
}

func TestRebaseForSaveInAFrozenWorkerLandsOnlyItsEdits(t *testing.T) {
	freshWorkerPolicy(t)
	snapshot := map[string]any{"model": "m", "mcp_servers": map[string]any{"revoked": map[string]any{"enabled": false}}}
	if _, err := BindWorkerConfig(snapshot); err != nil {
		t.Fatal(err)
	}
	loaded := map[string]any{"model": "m", "mcp_servers": map[string]any{"revoked": map[string]any{"enabled": false}}, "approvals": map[string]any{"always": []any{}}}
	desired := map[string]any{"model": "m", "mcp_servers": map[string]any{"revoked": map[string]any{"enabled": true}}, "approvals": map[string]any{"always": []any{"ls"}}}
	live := map[string]any{"model": "edited-since"}
	got := RebaseForSave(desired, live, false, func() map[string]any { return loaded })
	want := map[string]any{"model": "edited-since", "approvals": map[string]any{"always": []any{"ls"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rebased %v", got)
	}
}

func TestBypassWorkerSchemaDefaultBranchesAreNotOperatorState(t *testing.T) {
	freshWorkerPolicy(t)
	defaults := map[string]any(Defaults())
	snapshot := map[string]any{"agent": Clone(defaults["agent"]), "custom": map[string]any{"a": 1}}
	snapshot["agent"].(map[string]any)["max_turns"] = 7
	if _, err := BindSafeWorkerPolicy(true, true, snapshot); err != nil {
		t.Fatal(err)
	}
	got := frozenForDeletion(WorkerConfigSnapshot())
	want := map[string]any{"agent": map[string]any{"max_turns": 7}, "custom": map[string]any{"a": 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("frozen %v", got)
	}
}
