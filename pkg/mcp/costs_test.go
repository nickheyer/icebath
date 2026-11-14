package mcp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Cost table of the upstream fixture with a single figure and a noted range
func costedConfig() map[string]any {
	return map[string]any{"trust": "full", "spend_limit": 100, "tool_costs": map[string]any{
		"paid_render": 40, "batch": map[string]any{"credits": []any{5, 20}, "note": "per item"}}}
}

// Registers the fixture's policy under a key
func registerPolicy(t *testing.T, key ServerKey, cfg map[string]any) *CostPolicy {
	t.Helper()
	p, err := ConfigCostPolicy(key.Name, cfg)
	if err != nil || p == nil {
		t.Fatalf("policy %v %v", p, err)
	}
	mcpMu.Lock()
	serverCostPolicies[key] = p
	mcpMu.Unlock()
	return p
}

func TestParseCostPolicyValidation(t *testing.T) {
	where := "MCP server 'srv'"
	bad := []struct {
		costs, limit, unit any
		want               string
	}{
		{map[string]any{"paid_render": -1}, nil, nil, "cost of 'paid_render' must be a non-negative number, not -1"},
		{map[string]any{"a": true}, nil, nil, "must be a non-negative number, not True"},
		{map[string]any{"a": []any{1}}, nil, nil, "range must be [min, max]"},
		{map[string]any{"a": []any{5, 1}}, nil, nil, "range must be [min, max] with min <= max"},
		{map[string]any{"a": map[string]any{"credits": 1, "x": 2}}, nil, nil, `must be a number, [min, max], or {"credits": ..., "note": "..."}`},
		{map[string]any{"a": map[string]any{"credits": 1, "note": 3}}, nil, nil, "note must be a string"},
		{[]any{1}, nil, nil, "costs must be an object mapping MCP tool names to costs"},
		{map[string]any{"": 1}, nil, nil, "costs keys must be non-empty MCP tool names"},
		{nil, "lots", nil, "spend_limit must be a non-negative number, not 'lots'"},
		{map[string]any{"a": 1}, nil, "   ", "cost_unit must be a short non-empty label, not '   '"},
		{map[string]any{"a": 1}, nil, strings.Repeat("x", 33), "cost_unit must be a short non-empty label"},
	}
	for _, c := range bad {
		_, err := ParseCostPolicy(c.costs, c.limit, c.unit, where)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v %v %v: got %v want %q", c.costs, c.limit, c.unit, err, c.want)
		}
	}
	if p, err := ParseCostPolicy(map[string]any{"free": 0}, 50, nil, where); p != nil || err != nil {
		t.Fatalf("zero cost table must be no policy, got %v %v", p, err)
	}
	if p, err := ParseCostPolicy(nil, 50, nil, where); p != nil || err != nil {
		t.Fatalf("a limit alone must be no policy, got %v %v", p, err)
	}
	p, err := ParseCostPolicy(map[string]any{"a": map[string]any{"credits": []any{1, 2.5}, "note": " n "}}, nil, " tokens ", where)
	if err != nil || p.Unit != "tokens" || p.Limit != 0 || p.Costs["a"] != (ToolCost{1, 2.5, "n"}) {
		t.Fatalf("parsed %+v %v", p, err)
	}
}

func TestCostSentencesAndRefusal(t *testing.T) {
	resetLedgers(t)
	key := ServerKey{Name: "srv"}
	registerPolicy(t, key, costedConfig())
	ctx := context.Background()
	paid := PendingCharge(ctx, key, "srv", "paid_render", "chat-a")
	batch := PendingCharge(ctx, key, "srv", "batch", "chat-a")
	if PendingCharge(ctx, key, "srv", "free_status", "chat-a") != nil {
		t.Fatal("undeclared tool must not charge")
	}
	if got := paid.Lines(0); len(got) != 2 || got[0] != "Costs about 40 credits." || got[1] != "Spend limit for this chat on 'srv': 100 credits." {
		t.Fatalf("lines %q", got)
	}
	if got := batch.Lines(40); strings.Join(got, " ") != "Costs 5-20 credits (per item). This chat has used about 40 credits on 'srv' (limit 100)." {
		t.Fatalf("batch lines %q", got)
	}
	refusal := paid.Refusal(80)
	for _, want := range []string{"MCP tool 'paid_render' on server 'srv' was NOT run", "used about 80 credits", "spend limit is 100 credits", "Do not retry"} {
		if !strings.Contains(refusal, want) {
			t.Fatalf("refusal %q lacks %q", refusal, want)
		}
	}
	if qty(1, "credits") != "1 credit" || fmtAmount(1234567) != "1,234,567" || fmtAmount(2.5) != "2.5" || fmtAmount(1234.567) != "1,234.57" {
		t.Fatalf("formatting %q %q %q %q", qty(1, "credits"), fmtAmount(1234567), fmtAmount(2.5), fmtAmount(1234.567))
	}
}

func TestReserveWithinLimitAndRefund(t *testing.T) {
	resetLedgers(t)
	key := ServerKey{Name: "srv"}
	registerPolicy(t, key, costedConfig())
	c := PendingCharge(context.Background(), key, "srv", "paid_render", "chat-a")
	for i := 0; i < 2; i++ {
		if ok, _ := c.ReserveWithinLimit(); !ok {
			t.Fatalf("reservation %d refused", i)
		}
	}
	ok, before := c.ReserveWithinLimit()
	if ok || before != 80 || !c.OverLimit(before) {
		t.Fatalf("third reservation %v %v", ok, before)
	}
	c.Refund()
	c.Refund()
	c.Refund()
	if c.Spent() != 0 {
		t.Fatalf("refunds must floor at zero, spent %v", c.Spent())
	}
}

func TestChatsAndProfilesNeverShareATally(t *testing.T) {
	resetLedgers(t)
	ctx := context.Background()
	bare := ServerKey{Name: "srv"}
	policy := registerPolicy(t, bare, costedConfig())
	for i := 0; i < 3; i++ {
		PendingCharge(ctx, bare, "srv", "paid_render", "chat-a").Book()
	}
	if PendingCharge(ctx, bare, "srv", "paid_render", "chat-b").Spent() != 0 {
		t.Fatal("chat-b saw chat-a's spend")
	}
	pa, pb := ServerKey{"profile-a", "srv"}, ServerKey{"profile-b", "srv"}
	mcpMu.Lock()
	serverCostPolicies[pa], serverCostPolicies[pb] = policy, policy
	mcpMu.Unlock()
	a := PendingCharge(ctx, pa, "srv", "paid_render", "chat-a")
	b := PendingCharge(ctx, pb, "srv", "paid_render", "chat-a")
	a.Book()
	if a.Spent() != 40 || b.Spent() != 0 {
		t.Fatalf("profiles shared a tally %v %v", a.Spent(), b.Spent())
	}
	inFlight := PendingCharge(ctx, bare, "srv", "paid_render", "chat-a")
	rekeyUnscopedCosts("launch")
	inFlight.Book()
	costsMu.Lock()
	got := spent[tallyKey{Policy: ServerKey{"launch", "srv"}, Session: "chat-a"}]
	costsMu.Unlock()
	if got != 160 {
		t.Fatalf("moved total %v want 160", got)
	}
}

func TestTotalsFollowRotationAndCronRunsAreSeparate(t *testing.T) {
	resetLedgers(t)
	key := ServerKey{Name: "srv"}
	registerPolicy(t, key, costedConfig())
	ctx := context.Background()
	PendingCharge(ctx, key, "srv", "paid_render", "parent").Book()
	PendingCharge(ctx, key, "srv", "paid_render", "parent").Book()
	TransferSession("parent", "child")
	if got := PendingCharge(ctx, key, "srv", "paid_render", "child").Spent(); got != 80 {
		t.Fatalf("rotated total %v", got)
	}
	for _, job := range []string{"job-a", "job-b"} {
		runCtx := BindRun(ctx, job+"-run-1")
		for i := 0; i < 2; i++ {
			if ok, _ := PendingCharge(runCtx, key, "srv", "paid_render", "").ReserveWithinLimit(); !ok {
				t.Fatalf("%s call %d refused", job, i)
			}
		}
	}
	if got := PendingCharge(ctx, key, "srv", "paid_render", "").Spent(); got != 0 {
		t.Fatalf("cron runs leaked into the shared empty session %v", got)
	}
}

func TestUnknownCostKeysNamesTypos(t *testing.T) {
	cfg := costedConfig()
	cfg["tool_costs"].(map[string]any)["mcp_srv_typo"] = 3
	p, _ := ConfigCostPolicy("srv", cfg)
	if got := UnknownCostKeys(p, []string{"paid_render", "batch", "free_status"}); len(got) != 1 || got[0] != "mcp_srv_typo" {
		t.Fatalf("unknown keys %v", got)
	}
	if UnknownCostKeys(nil, []string{"x"}) != nil || UnknownCostKeys(p, nil) != nil {
		t.Fatal("no policy or no tools must report nothing")
	}
}

func TestParallelOverLimitPromptsRunOneAtATime(t *testing.T) {
	resetLedgers(t)
	key := ServerKey{Name: "srv"}
	registerPolicy(t, key, costedConfig())
	ctx := context.Background()
	PendingCharge(ctx, key, "srv", "paid_render", "chat-a").Book()
	PendingCharge(ctx, key, "srv", "paid_render", "chat-a").Book()
	var mu sync.Mutex
	active, peak := 0, 0
	var seen []float64
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := PendingCharge(ctx, key, "srv", "paid_render", "chat-a")
			release := c.Asking()
			defer release()
			mu.Lock()
			active++
			peak = max(peak, active)
			seen = append(seen, c.Spent())
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			c.Book()
			mu.Lock()
			active--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if peak != 1 || len(seen) != 2 || seen[0] == seen[1] {
		t.Fatalf("peak %d seen %v", peak, seen)
	}
}

func TestFilterSuspiciousDropsMalformedBudgets(t *testing.T) {
	logs := captureLogs(t)
	kept := filterSuspiciousServers(map[string]any{"bad": map[string]any{"command": "x", "spend_limit": "lots"}, "ok": map[string]any{"command": "x"}})
	if len(kept) != 1 || kept["ok"] == nil {
		t.Fatalf("kept %v", kept)
	}
	if !strings.Contains(logs.text(), "spend_limit must be a non-negative number") {
		t.Fatalf("logs %q", logs.text())
	}
}

func TestPortableCostsSurviveANativeTrustOverride(t *testing.T) {
	isolateHome(t)
	captureLogs(t)
	safe := map[string]*Rendered{"srv": {Config: map[string]any{"command": "python3", "trust": "full", "spend_limit": 0}}}
	portable := map[string]map[string]any{"srv": {"command": "python3", "args": []any{"s.py"}, "trust": "untrusted",
		"tool_costs": map[string]any{"paid_render": []any{5, 20}}, "spend_limit": 50}}
	mergePortableServers(portable, safe, map[string]bool{})
	got := safe["srv"].Config
	if got["trust"] != "full" || got["spend_limit"] != 0 || got["tool_costs"] == nil {
		t.Fatalf("merged %v", got)
	}
	rejected := map[string]*Rendered{}
	mergePortableServers(portable, rejected, map[string]bool{"srv": true})
	if _, revived := rejected["srv"]; revived {
		t.Fatal("a rejected native entry was revived by its package")
	}
	fresh := map[string]*Rendered{}
	mergePortableServers(map[string]map[string]any{"pkg_srv": {"command": "x"}}, fresh, map[string]bool{})
	if fresh["pkg_srv"] == nil || fresh["pkg_srv"].Config["command"] != "x" {
		t.Fatalf("portable server not added %v", fresh)
	}
}
