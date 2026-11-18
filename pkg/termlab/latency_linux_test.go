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

// Keys keep answering while the floor draws, while dragged and after, in both displays, limits stretched by the load
func TestKonsoleInputLatency(t *testing.T) {
	for _, mode := range []string{"glyphs", "image"} {
		t.Run(mode, func(t *testing.T) {
			env := map[string]string{"arena.display": mode, "arena.world": "den", "arena.quality": "ultra"}
			if profile := os.Getenv("TERMLAB_PROFILE"); profile != "" {
				env["ICEBATH_CPUPROFILE"] = profile + "-" + mode + ".prof"
			}
			r := newRigWithEnv(t, Konsole, konsoleScreen, env)
			r.waitBar()
			frame := r.openArena()
			time.Sleep(3 * time.Second)
			start := time.Now()
			gauge := StartGauge()
			idle := r.pressMany(20)
			idleLoad := gauge.Stop()
			world := worldBox(frame)
			cx, cy := (world.Min.X+world.Max.X)/2, (world.Min.Y+world.Max.Y)/2
			gauge = StartGauge()
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
			dragLoad := gauge.Stop()
			gauge = StartGauge()
			after := r.pressMany(10)
			afterLoad := gauge.Stop()
			sort.Slice(during, func(i, j int) bool { return during[i] < during[j] })
			t.Logf("%s: %.0f KB/s idle, %.0f KB/s dragging, latency idle p90 %v max %v, dragging max %v, after max %v", mode, float64(r.wrote(start, dragStart))/1024/dragStart.Sub(start).Seconds(), float64(r.wrote(dragStart, dragEnd))/1024/dragEnd.Sub(dragStart).Seconds(), idle[len(idle)*9/10], idle[len(idle)-1], during[len(during)-1], after[len(after)-1])
			t.Logf("%s: load idle %s wake %.2fx, dragging %s wake %.2fx, after %s wake %.2fx", mode, idleLoad.Wake, idleLoad.Slowdown, dragLoad.Wake, dragLoad.Slowdown, afterLoad.Wake, afterLoad.Slowdown)
			idleLimit, dragLimit, afterLimit := idleLoad.Limit(60*time.Millisecond, outputWakes), dragLoad.Limit(100*time.Millisecond, outputWakes), afterLoad.Limit(100*time.Millisecond, outputWakes)
			r.check(idle[len(idle)*9/10] < idleLimit, "idle", nil, "keys answered in %v at the ninth decile while idle, limit %v", idle[len(idle)*9/10], idleLimit)
			r.check(during[len(during)-1] < dragLimit, "dragging", nil, "a key answered in %v while the camera was dragged, limit %v", during[len(during)-1], dragLimit)
			r.check(after[len(after)-1] < afterLimit, "after", nil, "a key answered in %v right after the drag, limit %v", after[len(after)-1], afterLimit)
		})
	}
}
