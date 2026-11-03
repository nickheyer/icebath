package termlab

import (
	"os"
	"sort"
	"testing"
	"time"
)

// Key latencies of a count of presses with a pause between them, sorted
func (r *rig) pressMany(n int) []time.Duration {
	var lats []time.Duration
	for i := 0; i < n; i++ {
		r.s.Type("d")
		lat, err := r.s.Latency(2 * time.Second)
		if err != nil {
			lat = 2 * time.Second
		}
		lats = append(lats, lat)
		time.Sleep(150 * time.Millisecond)
	}
	sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
	return lats
}

// Bytes the program wrote between two moments
func (r *rig) wrote(from, to time.Time) int {
	n := 0
	for _, c := range r.s.Chunks() {
		if c.At.After(from) && c.At.Before(to) {
			n += len(c.Data)
		}
	}
	return n
}

// Keys keep answering within a frame while the floor draws, while the camera is dragged and right after, in glyph and image mode
func TestKonsoleInputLatency(t *testing.T) {
	for _, mode := range []string{"glyphs", "image"} {
		t.Run(mode, func(t *testing.T) {
			env := map[string]string{"sprint.arena.display": mode, "sprint.arena.world": "den", "sprint.arena.quality": "ultra"}
			if profile := os.Getenv("TERMLAB_PROFILE"); profile != "" {
				env["ICEBATH_CPUPROFILE"] = profile + "-" + mode + ".prof"
			}
			r := newRigWithEnv(t, Konsole, konsoleScreen, env)
			r.waitBar()
			frame := r.openArena()
			time.Sleep(3 * time.Second)
			start := time.Now()
			idle := r.pressMany(20)
			world := worldBox(frame)
			cx, cy := (world.Min.X+world.Max.X)/2, (world.Min.Y+world.Max.Y)/2
			r.s.Mouse(cx, cy, 0, true)
			dragStart := time.Now()
			var during []time.Duration
			for i := 1; i <= 60; i++ {
				r.s.Write(MouseSeq(cx+i, cy+i/4, 32, true))
				if i%10 == 0 {
					r.s.Type("d")
					lat, err := r.s.Latency(2 * time.Second)
					if err != nil {
						lat = 2 * time.Second
					}
					during = append(during, lat)
				}
				time.Sleep(33 * time.Millisecond)
			}
			r.s.Mouse(cx+60, cy+15, 0, false)
			dragEnd := time.Now()
			after := r.pressMany(10)
			sort.Slice(during, func(i, j int) bool { return during[i] < during[j] })
			t.Logf("%s: %.0f KB/s idle, %.0f KB/s dragging, latency idle p90 %v max %v, dragging max %v, after max %v", mode, float64(r.wrote(start, dragStart))/1024/dragStart.Sub(start).Seconds(), float64(r.wrote(dragStart, dragEnd))/1024/dragEnd.Sub(dragStart).Seconds(), idle[len(idle)*9/10], idle[len(idle)-1], during[len(during)-1], after[len(after)-1])
			r.check(idle[len(idle)*9/10] < 60*time.Millisecond, "idle", nil, "keys answered in %v at the ninth decile while idle", idle[len(idle)*9/10])
			r.check(during[len(during)-1] < 100*time.Millisecond, "dragging", nil, "a key answered in %v while the camera was dragged", during[len(during)-1])
			r.check(after[len(after)-1] < 100*time.Millisecond, "after", nil, "a key answered in %v right after the drag", after[len(after)-1])
		})
	}
}
