package sprint

import (
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/vxl"
)

// Collector of the messages a worker sends, safe to read from the test while the worker runs
type sink struct {
	mu   sync.Mutex
	msgs []tea.Msg
}

func (k *sink) send(msg tea.Msg) {
	k.mu.Lock()
	k.msgs = append(k.msgs, msg)
	k.mu.Unlock()
}

// Frames sent so far
func (k *sink) frames() []*frame {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []*frame
	for _, msg := range k.msgs {
		if fm, ok := msg.(frameMsg); ok {
			out = append(out, fm.f)
		}
	}
	return out
}

// Waits until the sink holds at least n frames or the timeout passes, answering the frames
func (k *sink) await(n int, timeout time.Duration) []*frame {
	deadline := time.Now().Add(timeout)
	for {
		if fs := k.frames(); len(fs) >= n || time.Now().After(deadline) {
			return fs
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Worker draws a frame once it has a size, waits for that frame to be painted before the next, and sleeps when none is painted
func TestStagePacesByPaint(t *testing.T) {
	k := &sink{}
	cfg := Default()
	cfg.Arena.FPS = 60
	st := newStage(nil, k.send, theme.Blackwall(), cfg)
	st.start()
	defer st.halt()
	snap := probeSnapshot()
	live := probeLive()
	st.post(func(s *stage) { s.sync(&snap, live) })
	st.post(func(s *stage) { s.resize(80, 24, vxl.ModeShapes, 0.5) })
	fs := k.await(1, 2*time.Second)
	if len(fs) != 1 {
		t.Fatalf("%d frames after a resize", len(fs))
	}
	f := fs[0]
	if f.cols != 80 || f.rows != 24 || len(f.cells) != 80*24 || f.packet != nil || f.gen != 1 {
		t.Fatalf("first frame %dx%d with %d cells gen %d", f.cols, f.rows, len(f.cells), f.gen)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(k.frames()); n != 1 {
		t.Fatalf("%d frames drawn while the first was never painted", n)
	}
	st.painted(f.gen)
	fs = k.await(2, 2*time.Second)
	if len(fs) < 2 {
		t.Fatalf("%d frames after painting the first", len(fs))
	}
	st.painted(fs[len(fs)-1].gen)
	fs = k.await(3, 2*time.Second)
	if len(fs) < 3 || fs[2].gen <= fs[1].gen {
		t.Fatalf("frames did not keep flowing with paints, %d", len(fs))
	}
	if len(f.spots) == 0 {
		t.Fatal("no actor placed on the first frame")
	}
}

// Camera inputs move the camera on the next frame, mark it moving and drop to the bare raster, the settled frame drawing the full passes
func TestStageMotionQuality(t *testing.T) {
	k := &sink{}
	cfg := Default()
	cfg.Arena.Quality = "high"
	st := newStage(nil, k.send, theme.Blackwall(), cfg)
	snap := probeSnapshot()
	st.sync(&snap, probeLive())
	st.resize(120, 30, vxl.ModeShapes, 0.5)
	for range 120 {
		st.sc.update(1.0 / 30)
		st.moveCamera(1.0 / 30)
	}
	st.rig.snap()
	st.render(time.Now())
	rest := k.frames()[0]
	if rest.moving || st.r.AO.Strength <= 0 || !st.layerOK {
		t.Fatalf("settled frame moving %v occlusion %v layer %v", rest.moving, st.r.AO.Strength, st.layerOK)
	}
	yaw := st.rig.yaw.Target
	st.orbit(0.5, 0)
	st.moveCamera(1.0 / 30)
	st.render(time.Now())
	turn := k.frames()[1]
	if !turn.moving || st.r.AO.Strength <= 0 || st.layerOK || st.rig.yaw.Target == yaw {
		t.Fatalf("turning frame moving %v occlusion %v layer %v yaw %v", turn.moving, st.r.AO.Strength, st.layerOK, st.rig.yaw.Target)
	}
	if st.turnCost <= 0 {
		t.Fatal("the cost of the turning frame was not kept")
	}
	st.turnCost = time.Second
	st.orbit(0.5, 0)
	st.moveCamera(1.0 / 30)
	st.render(time.Now())
	if bare := k.frames()[2]; !bare.moving || st.r.AO.Strength != 0 {
		t.Fatalf("a turning frame past the budget kept the passes, occlusion %v", st.r.AO.Strength)
	}
	st.orbit(0.5, 0)
	st.moveCamera(1.0 / 30)
	st.render(time.Now())
	if st.r.AO.Strength != 0 {
		t.Fatal("a turning frame tried the passes again on its own")
	}
	st.zoom(0.9)
	st.moveCamera(1.0 / 30)
	st.render(time.Now())
	if full := k.frames()[4]; !full.moving || st.r.AO.Strength <= 0 {
		t.Fatalf("a turning frame after a zoom did not measure the passes anew, occlusion %v", st.r.AO.Strength)
	}
	k.msgs = k.msgs[:1]
	st.render(time.Now())
	k.msgs = k.msgs[:2]
	if st.rate() != float64(cfg.Arena.FPS) {
		t.Fatalf("rate %v while the camera turns", st.rate())
	}
	st.rig.snap()
	st.render(time.Now())
	if st.rate() != float64(cfg.Arena.FPS) {
		t.Fatalf("rate %v before the refine frame", st.rate())
	}
	st.render(time.Now())
	if again := k.frames()[3]; again.moving || st.r.AO.Strength <= 0 || !st.layerOK {
		t.Fatal("the frame once the camera holds still did not refine")
	}
	st.sc.update(1.0 / 30)
	st.render(time.Now())
	if drift := k.frames()[4]; drift.moving || !st.layerOK {
		t.Fatal("a still camera lost the lit layer")
	}
	st.focus(false)
	if st.rate() != blurredFPS {
		t.Fatalf("rate %v while blurred", st.rate())
	}
}

// A sync with a snapshot of another sprint resets the floor, and the lobby wanderer stands on it with none
func TestStageSyncsSprints(t *testing.T) {
	k := &sink{}
	st := newStage(nil, k.send, theme.Blackwall(), Default())
	st.sync(nil, nil)
	if st.sc.lobby == nil {
		t.Fatal("no lobby wanderer without a sprint")
	}
	snap := probeSnapshot()
	st.sync(&snap, probeLive())
	if st.sc.lobby != nil || len(st.sc.actors) != len(snap.Members) || st.sc.sprintID != snap.ID {
		t.Fatalf("lobby %v actors %d sprint %q", st.sc.lobby != nil, len(st.sc.actors), st.sc.sprintID)
	}
	other := probeSnapshot14()
	st.sync(&other, probeLive14())
	if len(st.sc.actors) != len(other.Members) || st.sc.sprintID != other.ID {
		t.Fatalf("actors %d sprint %q after switching sprints", len(st.sc.actors), st.sc.sprintID)
	}
	st.mark("Ada", "Linus", "T1", "T2")
	if st.selectedActor() == nil || st.selectedActor().name != "Ada" {
		t.Fatal("selection did not reach the floor")
	}
	st.resize(100, 30, vxl.ModeOctant, 0.5)
	for range 120 {
		st.sc.update(1.0 / 30)
		st.moveCamera(1.0 / 30)
	}
	st.render(time.Now())
	f := k.frames()[0]
	if len(f.nodes) == 0 || f.actors["Ada"].gone {
		t.Fatalf("frame carries %d node marks", len(f.nodes))
	}
}

// A settled frame past its budget eases the quality down a step, cheap frames bring it back, and cell buffers are reused only once painted
func TestStageEasesSettledQuality(t *testing.T) {
	k := &sink{}
	cfg := Default()
	cfg.Arena.Quality = "ultra"
	cfg.Arena.FPS = 30
	st := newStage(nil, k.send, theme.Blackwall(), cfg)
	snap := probeSnapshot()
	st.sync(&snap, probeLive())
	st.resize(80, 24, vxl.ModeShapes, 0.5)
	st.rig.snap()
	st.render(time.Now())
	st.render(time.Now())
	if st.ease != 0 || k.frames()[1].quality != "ultra" || st.fr.W != 80*4*2 {
		t.Fatalf("ease %d quality %q width %d before any slow frame", st.ease, k.frames()[1].quality, st.fr.W)
	}
	for range 2 {
		st.noteRest(time.Second)
	}
	if st.ease != 1 {
		t.Fatalf("ease %d after two slow frames", st.ease)
	}
	st.render(time.Now())
	if f := k.frames()[2]; f.quality != "high" || st.fr.W != 80*4 {
		t.Fatalf("eased frame quality %q width %d", f.quality, st.fr.W)
	}
	for range 2 {
		st.noteRest(time.Second)
	}
	for range 2 {
		st.noteRest(time.Second)
	}
	if st.ease != 3 {
		t.Fatalf("ease %d after more slow frames", st.ease)
	}
	st.render(time.Now())
	if f := k.frames()[3]; f.quality != "low" || st.r.AO.Strength != 0 {
		t.Fatalf("bare frame quality %q occlusion %v", f.quality, st.r.AO.Strength)
	}
	for range 200 {
		st.noteRest(time.Millisecond)
	}
	if st.ease != 3 {
		t.Fatalf("ease %d climbed back on its own after cheap frames", st.ease)
	}
	st.zoom(0.9)
	if st.ease != 0 || st.turnCost != 0 {
		t.Fatalf("ease %d turn cost %v after a zoom", st.ease, st.turnCost)
	}
	first := k.frames()[0].cells
	st.render(time.Now())
	if &k.frames()[4].cells[0] == &first[0] {
		t.Fatal("a cell buffer was reused before the card painted past it")
	}
	st.painted(k.frames()[4].gen)
	st.render(time.Now())
	reused := false
	for _, f := range k.frames()[:4] {
		if &f.cells[0] == &k.frames()[5].cells[0] {
			reused = true
		}
	}
	if !reused {
		t.Fatal("no cell buffer was reused once the card painted past it")
	}
}

// Lights settling away from the floor's own fold into the lit floor without lighting it whole, the frame matching one lit whole
func TestStageRelightsSettledLights(t *testing.T) {
	k := &sink{}
	cfg := Default()
	cfg.Arena.Quality = "high"
	st := newStage(nil, k.send, theme.Blackwall(), cfg)
	snap := probeSnapshot()
	st.sync(&snap, probeLive())
	st.resize(120, 30, vxl.ModeShapes, 0.5)
	st.rig.snap()
	now := time.Now()
	st.render(now)
	if !st.layerOK {
		t.Fatal("no lit floor after a settled frame")
	}
	st.sc.beamAt = st.sc.beamAt.Add(vxl.V(3, 0, 1))
	st.sc.beamWant = st.sc.beamAt
	moved := append([]vxl.Light(nil), st.sc.collectLights()...)
	st.render(now.Add(10 * time.Millisecond))
	st.render(now.Add(20 * time.Millisecond))
	if slices.Equal(st.layerLights, moved) {
		t.Fatal("the floor was relit before the lights held still long enough")
	}
	st.render(now.Add(relightAfter + 30*time.Millisecond))
	if !slices.Equal(st.layerLights, moved) {
		t.Fatalf("the floor kept its old lights after they settled, %v", st.layerLights)
	}
	if d := st.out.Dirt(); d.All || d.Count() >= st.fr.W*st.fr.H {
		t.Fatalf("the floor was lit whole for a moved light, %d pixels reworked", d.Count())
	}
	relit := append([]vxl.RGB(nil), st.out.Color...)
	st.layerOK = false
	st.render(now.Add(relightAfter + 40*time.Millisecond))
	for i, c := range st.out.Color {
		if d := math.Sqrt(c.Dist(relit[i])); d > 0.03 {
			t.Fatalf("pixel %d is %v lit whole and %v relit", i, c, relit[i])
		}
	}
}

// The floor keeps its rate while a settled frame changes anything and calms once one changes nothing
func TestStageCalmsOnlyWhenStill(t *testing.T) {
	k := &sink{}
	cfg := Default()
	cfg.Arena.FPS = 30
	st := newStage(nil, k.send, theme.Blackwall(), cfg)
	snap := probeSnapshot()
	st.sync(&snap, probeLive())
	st.resize(80, 24, vxl.ModeShapes, 0.5)
	st.rig.snap()
	rest := func() {
		for i := 0; i < 6000 && st.sc.busy(); i++ {
			st.sc.update(1.0 / 30)
		}
		if st.sc.busy() {
			t.Fatal("the floor never came to rest")
		}
	}
	rest()
	now := time.Now()
	st.render(now)
	st.sc.update(1.0 / 30)
	rest()
	st.render(now.Add(33 * time.Millisecond))
	if st.out.Moved() == 0 || st.rate() != 30 {
		t.Fatalf("rate %v after a settled frame that moved %d pixels", st.rate(), st.out.Moved())
	}
	st.render(now.Add(66 * time.Millisecond))
	if st.out.Moved() != 0 || st.rate() != calmFPS {
		t.Fatalf("rate %v after a settled frame that moved %d pixels", st.rate(), st.out.Moved())
	}
	st.sc.update(1.0 / 30)
	rest()
	st.render(now.Add(99 * time.Millisecond))
	if st.out.Moved() == 0 || st.rate() != 30 {
		t.Fatalf("rate %v once the floor moved again by %d pixels", st.rate(), st.out.Moved())
	}
}
