package matrix

import (
	"context"
	"testing"
	"time"
)

func TestVoiceMarkerChecks(t *testing.T) {
	if !isVoiceEvent(map[string]any{"msgtype": "m.audio", "org.matrix.msc3245.voice": map[string]any{}}) {
		t.Fatal("voice not detected")
	}
	if isVoiceEvent(map[string]any{"msgtype": "m.audio", "org.matrix.msc3245.voice": nil}) || isVoiceEvent(map[string]any{"msgtype": "m.text", "org.matrix.msc3245.voice": 1}) {
		t.Fatal("non voice detected")
	}
}

func TestParkedVoicesClaimWindowFloorAndCap(t *testing.T) {
	now := 1000.0
	monotonic = func() float64 { return now }
	t.Cleanup(func() { monotonic = func() float64 { return time.Since(processStart).Seconds() } })
	p := NewParkedVoices()
	var gates []*VoiceGate
	for i := 0; i < 6; i++ {
		g := p.Begin("!r", "@u")
		gates = append(gates, g)
	}
	if !p.Pending("!r", "@u") || p.Pending("!r", "@v") {
		t.Fatal("pending wrong")
	}
	for i := 5; i >= 0; i-- {
		p.Park("!r", "@u", gates[i], string(rune('a'+i)), map[string]any{}, map[string]any{})
	}
	mark := p.Mark()
	late := p.Begin("!r", "@u")
	p.Park("!r", "@u", late, "late", nil, nil)
	v, ok := p.Claim("!r", "@u", mark)
	if !ok || v.EventID != "f" {
		t.Fatalf("claimed %+v %v", v, ok)
	}
	if v, ok := p.Claim("!r", "@u", mark); ok {
		t.Fatalf("older voices survived %+v", v)
	}
	p.Park("!r", "@u", gates[0], "stale", nil, nil)
	if v, ok := p.Claim("!r", "@u", p.Mark()); !ok || v.EventID != "late" {
		t.Fatalf("late voice %+v %v", v, ok)
	}
	for _, g := range append(gates, late) {
		p.Release("!r", "@u", g)
		p.Release("!r", "@u", g)
	}
	if p.Pending("!r", "@u") || len(p.floor) != 0 {
		t.Fatal("release left state")
	}
	g := p.Begin("!r", "@u")
	p.Park("!r", "@u", g, "x", nil, nil)
	p.Release("!r", "@u", g)
	now += claimWindowSeconds + 1
	if p.Pending("!r", "@u") {
		t.Fatal("expired voice still pending")
	}
	if _, ok := p.Claim("!r", "@u", p.Mark()); ok {
		t.Fatal("expired voice claimed")
	}
}

func TestParkedVoicesSettleWaitsForRelease(t *testing.T) {
	p := NewParkedVoices()
	g := p.Begin("!r", "@u")
	go func() {
		time.Sleep(20 * time.Millisecond)
		p.Release("!r", "@u", g)
	}()
	start := time.Now()
	p.Settle(context.Background(), "!r", "@u")
	if time.Since(start) > 3*time.Second {
		t.Fatal("settle did not return on release")
	}
}
