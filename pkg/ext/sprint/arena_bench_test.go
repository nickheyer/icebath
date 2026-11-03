package sprint

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Sprint with a long log registered for a benchmark
func benchSprint(b *testing.B, suffix string) *Sprint {
	snap := probeSnapshot()
	s := newSprint(snap.ID+"-"+suffix, snap.Request, snap.Started, nil)
	s.enter(StageBuild, "building")
	for i := range snap.Members {
		m := snap.Members[i]
		m.Live = &harness.Live{Session: session.New()}
		s.putMember(&m)
	}
	tickets := make([]*Ticket, 0, len(snap.Tickets))
	for i := range snap.Tickets {
		tk := snap.Tickets[i]
		tickets = append(tickets, &tk)
	}
	s.setPlan(Plan{Title: "Probe", Summary: "a plan"}, tickets)
	for i := range 300 {
		s.say(MsgChat, "Ada", "Linus", "T1", strings.Repeat("words of a long exchange between two builders about an interface ", 1+i%3))
	}
	register(s)
	b.Cleanup(func() {
		regMu.Lock()
		sprints = sprints[:len(sprints)-1]
		regMu.Unlock()
	})
	return s
}

// Times a paint of a wide arena with the dock, the inspector and a long log, as the card costs the program loop per frame
func BenchmarkArenaPaint(b *testing.B) {
	s := benchSprint(b, "bench")
	a := newArena(testEnv()).(*arena)
	a.sprintID = s.ID
	a.refresh()
	a.sel, a.inspect = "Ada", true
	buf := uv.NewBuffer(220, 56)
	a.Paint(buf)
	a.advance(30)
	a.settleCamera()
	a.advance(1)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		a.Paint(buf)
	}
}

// Times a frame of a wide floor on the worker under the cinematic camera, where the static layer never settles
func BenchmarkStageMoving(b *testing.B) {
	cfg := Default()
	cfg.Arena.Camera = "cinematic"
	st := newStage(nil, func(tea.Msg) {}, theme.Blackwall(), cfg)
	snap := probeSnapshot()
	st.sync(&snap, probeLive())
	st.resize(270, 62, vxl.ModeOctant, 0.5)
	for range 30 {
		st.sc.update(1.0 / 30)
		st.moveCamera(1.0 / 30)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		st.sc.update(1.0 / 30)
		st.moveCamera(1.0 / 30)
		st.rig.yaw.Kick(5)
		st.render(time.Now())
	}
}

// Times a frame of a wide floor on the worker under the orbit camera, which drifts too slowly to unsettle the springs
func BenchmarkStageDrifting(b *testing.B) {
	cfg := Default()
	cfg.Arena.Camera = "orbit"
	st := newStage(nil, func(tea.Msg) {}, theme.Blackwall(), cfg)
	snap := probeSnapshot()
	st.sync(&snap, probeLive())
	st.resize(270, 62, vxl.ModeOctant, 0.5)
	for range 30 {
		st.sc.update(1.0 / 30)
		st.moveCamera(1.0 / 30)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		st.sc.update(1.0 / 30)
		st.moveCamera(1.0 / 30)
		st.render(time.Now())
	}
}

// Times a frame of a wide floor on the worker with the camera at rest, every frame drawn over the lit layer
func BenchmarkStageSettled(b *testing.B) {
	st := newStage(nil, func(tea.Msg) {}, theme.Blackwall(), Default())
	snap := probeSnapshot()
	st.sync(&snap, probeLive())
	st.resize(270, 62, vxl.ModeOctant, 0.5)
	for range 30 {
		st.sc.update(1.0 / 30)
		st.moveCamera(1.0 / 30)
	}
	st.rig.snap()
	st.render(time.Now())
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		st.sc.update(1.0 / 30)
		st.render(time.Now())
	}
}
