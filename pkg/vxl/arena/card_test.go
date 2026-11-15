package arena

import (
	"slices"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/ui/keys"
	"github.com/nickheyer/icebath/pkg/ui/wm"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Card over a journal of its own holding the probe turn, pinned to that turn, with the harness it calls
func probeCard(t *testing.T) (*arena, *probeHarness, *Journal) {
	t.Helper()
	j := NewJournal()
	for _, r := range probeReports("card", time.Now().Add(-10*time.Second)) {
		j.Observe(r)
	}
	h := &probeHarness{}
	a := New(testEnv(), j, h, Default(), nil).(*arena)
	a.sprintID = j.Sprints("card")[0]
	a.refresh()
	return a, h, j
}

// Text of one row of painted cells
func rowText(buf *uv.Buffer, y int) string {
	var sb strings.Builder
	for x := range buf.Width() {
		if c := buf.CellAt(x, y); c != nil {
			sb.WriteString(c.Content)
		}
	}
	return sb.String()
}

// Toolbar on the first row, the floor under it, a dock on the left from ninety columns
func TestArenaLayoutRules(t *testing.T) {
	a := newArena(testEnv()).(*arena)
	for _, w := range []int{154, 120, 90} {
		buf := paintInto(a, w, 43)
		l := a.lay
		if l.bar != (rect{0, 0, w, 1}) || !strings.Contains(rowText(buf, 0), "Dock") {
			t.Fatalf("width %d toolbar %+v row %q", w, l.bar, rowText(buf, 0))
		}
		if l.dock.w == 0 || l.dock.x != 0 || l.dock.w > w/2 || l.world.x != l.dock.w || l.world.x+l.world.w != w || l.world.y != 1 || l.world.h != 42 {
			t.Fatalf("width %d dock %+v world %+v", w, l.dock, l.world)
		}
	}
	paintInto(a, 89, 43)
	if a.lay.dock.w != 0 || a.lay.world != (rect{0, 1, 89, 42}) {
		t.Fatalf("narrow card dock %+v world %+v", a.lay.dock, a.lay.world)
	}
	c, _, _ := probeCard(t)
	paintInto(c, 154, 43)
	if c.lay.tape != (rect{c.lay.world.x, 42, c.lay.world.w, 1}) || c.lay.world.h != 41 {
		t.Fatalf("turn shown with timeline %+v over world %+v", c.lay.tape, c.lay.world)
	}
}

// The d key toggles the dock, the toolbar button and the left half of the card, and toggles them back
func TestArenaDockKey(t *testing.T) {
	km := testKeys()
	if ids := km.Lookup([]string{Context}, keys.Press("d")); !slices.Contains(ids, ActSidebar) {
		t.Fatalf("d fires %v in the arena", ids)
	}
	a := newArena(testEnv()).(*arena)
	a.env.Keys = km
	before := paintInto(a, 154, 43)
	dock := a.lay.dock
	a.Update(wm.ActionMsg{ID: ActSidebar})
	hidden := paintInto(a, 154, 43)
	if a.dock || a.lay.dock.w != 0 || a.lay.world.x != 0 {
		t.Fatalf("dock %v at %+v world %+v after d", a.dock, a.lay.dock, a.lay.world)
	}
	changed := 0
	for y := dock.y; y < dock.y+dock.h; y++ {
		for x := dock.x; x < dock.x+dock.w; x++ {
			if !before.CellAt(x, y).Equal(hidden.CellAt(x, y)) {
				changed++
			}
		}
	}
	if changed == 0 || before.CellAt(2, 0).Equal(hidden.CellAt(2, 0)) {
		t.Fatalf("hiding the dock changed %d dock cells, toolbar button the same", changed)
	}
	a.Update(wm.ActionMsg{ID: ActSidebar})
	back := paintInto(a, 154, 43)
	if !a.dock || a.lay.dock != dock || rowText(back, 0) != rowText(before, 0) || rowText(back, 3) != rowText(before, 3) {
		t.Fatalf("second d left dock %+v", a.lay.dock)
	}
}

// Selecting a member or a ticket turns the camera onto it until the selection clears
func TestArenaSelectionFocusesCamera(t *testing.T) {
	a, _, _ := probeCard(t)
	paintInto(a, 154, 43)
	a.advance(60)
	a.press("member:Linus")
	if a.sel != "Linus" {
		t.Fatalf("selected %q", a.sel)
	}
	a.advance(90)
	s := a.stage
	act := s.sc.byName["Linus"]
	target := vxl.V(s.rig.tx.Target, s.rig.ty.Target, s.rig.tz.Target)
	if s.focusOn != "member" || target.Sub(act.pos.Add(vxl.V(0, 1.5, 0))).Len() > followSlack+0.01 {
		t.Fatalf("camera aims at %v, Linus stands at %v", target, act.pos)
	}
	a.press("ticket:D3")
	a.advance(30)
	n := s.sc.nodes["D3"]
	target = vxl.V(s.rig.tx.Target, s.rig.ty.Target, s.rig.tz.Target)
	if s.focusOn != "ticket" || target.Sub(n.core(s.sc.t)).Len() > 0.5 || a.sel != "Grace" {
		t.Fatalf("camera aims at %v, core D3 at %v, selected %q", target, n.core(s.sc.t), a.sel)
	}
	a.act(ActLeave)
	a.act(ActLeave)
	a.advance(2)
	if s.focusOn != "" || s.rig.tx.Target != s.sc.lay.center().X {
		t.Fatalf("camera kept focus %q after clearing the selection", s.focusOn)
	}
}

// Turn controls and member actions reach the harness only where a real operation stands behind them
func TestArenaActionsReachHarness(t *testing.T) {
	a, h, j := probeCard(t)
	paintInto(a, 154, 43)
	a.act(ActKill)
	if len(h.calls) != 0 {
		t.Fatalf("interrupt without a selection called %v", h.calls)
	}
	a.selectMember("Linus")
	a.act(ActKill)
	a.act(ActOpen)
	a.selectMember("Grace")
	a.act(ActKill)
	if cmd := a.act(ActSteer); cmd == nil {
		t.Fatal("steering a finished member answered nothing")
	}
	a.act(ActPause)
	a.act(ActStop)
	a.act(ActRestart)
	if want := []string{"interrupt:Linus", "open:Linus", "pause", "stop"}; !slices.Equal(h.calls, want) {
		t.Fatalf("calls %v want %v", h.calls, want)
	}
	for _, b := range a.buttons() {
		if b.ID == ActRestart {
			t.Fatal("retry offered on a running turn")
		}
	}
	last := probeReports("card", time.Now())[4]
	last.At, last.Busy, last.Outcome, last.Failure = time.Now(), false, OutcomeFailed, "provider refused"
	for i := range last.Agents {
		last.Agents[i].Status = "interrupted"
	}
	j.Observe(last)
	a.refresh()
	a.act(ActRestart)
	a.act(ActPause)
	if h.calls[len(h.calls)-1] != "retry" {
		t.Fatalf("calls %v after a failed turn", h.calls)
	}
	retry := false
	for _, b := range a.buttons() {
		retry = retry || b.ID == ActRestart
		if b.ID == ActPause || b.ID == ActStop {
			t.Fatalf("button %s offered on an ended turn", b.ID)
		}
	}
	if !retry {
		t.Fatal("no retry button on a failed newest turn")
	}
}

// A replay rebuilds the turn from its start, steps along its changes and returns to live
func TestArenaReplay(t *testing.T) {
	a, _, j := probeCard(t)
	paintInto(a, 154, 43)
	live := a.snap
	a.act(ActReplay)
	a.refresh()
	if a.replay == nil || !a.replay.playing || a.snap.ID == live.ID || !strings.HasPrefix(a.snap.ID, live.Sprint+"@") {
		t.Fatalf("replay %+v shows %q", a.replay, a.snap.ID)
	}
	if len(a.snap.Members) != 1 || a.snap.Since != len(a.snap.Messages) {
		t.Fatalf("replay start shows %d members and plays messages from %d of %d", len(a.snap.Members), a.snap.Since, len(a.snap.Messages))
	}
	a.act(ActReplay)
	if a.replay.playing {
		t.Fatal("space did not pause the replay")
	}
	tp, _ := j.Tape(live.Sprint)
	steps := 0
	for a.replay.cursor.Before(tp.end) && steps < 100 {
		epoch := a.replay.epoch
		a.act(ActStepForward)
		a.refresh()
		if a.replay.epoch != epoch {
			t.Fatal("a step forward reset the floor")
		}
		steps++
	}
	if steps < 4 || len(a.snap.Members) != len(live.Members) || len(a.snap.Messages) != len(live.Messages) {
		t.Fatalf("%d steps reached %d members and %d messages of %d and %d", steps, len(a.snap.Members), len(a.snap.Messages), len(live.Members), len(live.Messages))
	}
	epoch := a.replay.epoch
	a.act(ActStepBack)
	a.refresh()
	if a.replay.epoch == epoch || a.snap.Since != len(a.snap.Messages) || len(a.snap.Messages) >= len(live.Messages) {
		t.Fatalf("step back epoch %d since %d of %d messages", a.replay.epoch, a.snap.Since, len(a.snap.Messages))
	}
	paintInto(a, 154, 43)
	tr := a.track()
	a.seekTape(tr.x)
	a.refresh()
	if !a.replay.cursor.Equal(tp.start) || len(a.snap.Members) != 1 {
		t.Fatalf("seek to the start of the track put the cursor at %v of %v", a.replay.cursor, tp.start)
	}
	a.act(ActSpeed)
	if speeds[a.replay.speed] != 2 {
		t.Fatalf("speed %v after one press", speeds[a.replay.speed])
	}
	a.act(ActLive)
	a.refresh()
	if a.replay != nil || a.snap.ID != live.Sprint {
		t.Fatalf("live view shows %q", a.snap.ID)
	}
}

// Playing squeezes every silence of the tape to the gap and stops at its end
func TestReplaySqueezesSilence(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	at := func(s float64) time.Time { return start.Add(time.Duration(s * float64(time.Second))) }
	tp := tape{start: start, end: at(30), times: []time.Time{at(0), at(20), at(21)}}
	r := &replay{cursor: start, playing: true}
	r.advance(tp, 100*time.Millisecond)
	if !r.cursor.Equal(at(20).Add(-replayGap)) {
		t.Fatalf("cursor %v after a step into a long silence", r.cursor.Sub(start))
	}
	r.advance(tp, time.Second)
	r.advance(tp, time.Second)
	if !r.cursor.Equal(at(20.5)) {
		t.Fatalf("cursor %v after two seconds of play", r.cursor.Sub(start))
	}
	r.speed = 3
	r.advance(tp, 100*time.Millisecond)
	if !r.cursor.Equal(at(21.3)) {
		t.Fatalf("cursor %v at eight times the speed", r.cursor.Sub(start))
	}
	r.speed = 0
	if r.advance(tp, time.Second) || !r.cursor.Equal(at(30).Add(-replayGap)) {
		t.Fatalf("cursor %v in the silence before the end", r.cursor.Sub(start))
	}
	r.advance(tp, time.Second)
	if !r.advance(tp, time.Second) || r.playing || !r.cursor.Equal(tp.end) {
		t.Fatalf("replay playing %v at %v past the end", r.playing, r.cursor.Sub(start))
	}
}
