package sprint

import (
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"

	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/session"
	"github.com/nickheyer/icebath/pkg/ui/anim"
)

// Times a paint of a wide arena under the cinematic camera, where the static layer never settles
func BenchmarkArenaPaintMoving(b *testing.B) {
	snap := probeSnapshot()
	s := newSprint(snap.ID+"-benchmove", snap.Request, snap.Started, nil)
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
		s.say(MsgChat, "Ada", "Linus", "T1", strings.Repeat("words ", 1+i%3))
	}
	register(s)
	defer func() {
		regMu.Lock()
		sprints = sprints[:len(sprints)-1]
		regMu.Unlock()
	}()
	a := newArena(testEnv()).(*arena)
	a.sprintID = s.ID
	a.cfg.Arena.Camera, a.rig.preset = "cinematic", "cinematic"
	a.refresh()
	a.sel, a.inspect = "Ada", true
	buf := uv.NewBuffer(270, 62)
	a.Paint(buf)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		a.sc.update(1.0 / 30)
		a.moveCamera(1.0 / 30)
		a.Paint(buf)
	}
}

// Times a paint of a wide arena with the camera at rest, every frame drawn over the lit layer
func BenchmarkArenaPaintSettled(b *testing.B) {
	snap := probeSnapshot()
	s := newSprint(snap.ID+"-benchsettled", snap.Request, snap.Started, nil)
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
	register(s)
	defer func() {
		regMu.Lock()
		sprints = sprints[:len(sprints)-1]
		regMu.Unlock()
	}()
	a := newArena(testEnv()).(*arena)
	a.sprintID = s.ID
	a.refresh()
	a.sel, a.inspect = "Ada", true
	buf := uv.NewBuffer(270, 62)
	a.Paint(buf)
	for _, v := range []*anim.Value{&a.rig.tx, &a.rig.ty, &a.rig.tz, &a.rig.yaw, &a.rig.pitch, &a.rig.dist} {
		v.Snap()
	}
	a.Paint(buf)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		a.sc.update(1.0 / 30)
		a.Paint(buf)
	}
}
