package termlab

import (
	"runtime"
	"slices"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Sleep between samples, its overshoot read as the wake delay of a waiting process
const gaugeNap = 40 * time.Millisecond

// Wall time a sample spins, the share the thread ran read as the CPU share a busy process gets
const gaugeSpin = 10 * time.Millisecond

// Machine load sampled beside a test phase, a sleeper's wake delay and a spinning thread's CPU share
type Gauge struct {
	mu     sync.Mutex
	wakes  []time.Duration
	shares []float64
	stop   chan struct{}
	done   chan struct{}
}

// Load of a phase, how late a sleeper woke and how many times longer busy work took than on a free core
type Load struct {
	Wake     time.Duration
	Slowdown float64
}

// Starts sampling the machine until Stop
func StartGauge() *Gauge {
	g := &Gauge{stop: make(chan struct{}), done: make(chan struct{})}
	go g.run()
	return g
}

// Takes a wake and a spin sample after another on one thread until stopped
func (g *Gauge) run() {
	defer close(g.done)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	for {
		select {
		case <-g.stop:
			return
		default:
		}
		start := time.Now()
		time.Sleep(gaugeNap)
		wake := max(time.Since(start)-gaugeNap, 0)
		share := spinShare(gaugeSpin)
		g.mu.Lock()
		g.wakes, g.shares = append(g.wakes, wake), append(g.shares, share)
		g.mu.Unlock()
	}
}

// Share of a core the calling thread ran while spinning, its thread CPU time over the wall time
func spinShare(window time.Duration) float64 {
	var before, after unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &before); err != nil {
		panic("termlab gauge: thread CPU clock: " + err.Error())
	}
	start := time.Now()
	for time.Since(start) < window {
	}
	wall := time.Since(start)
	if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &after); err != nil {
		panic("termlab gauge: thread CPU clock: " + err.Error())
	}
	return min(1, float64(after.Nano()-before.Nano())/float64(wall))
}

// Load so far, the ninth decile wake delay and the inverse median CPU share, one without samples
func (g *Gauge) Load() Load {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.shares) == 0 {
		return Load{Slowdown: 1}
	}
	wakes, shares := slices.Clone(g.wakes), slices.Clone(g.shares)
	slices.Sort(wakes)
	slices.Sort(shares)
	return Load{Wake: wakes[len(wakes)*9/10], Slowdown: 1 / max(shares[len(shares)/2], 0.01)}
}

// Stops sampling and gives the load of the whole phase
func (g *Gauge) Stop() Load {
	close(g.stop)
	<-g.done
	return g.Load()
}

// Wall clock limit under the load, the base stretched by the slowdown plus a wake delay per process woken
func (l Load) Limit(base time.Duration, wakes int) time.Duration {
	return time.Duration(float64(base)*l.Slowdown) + time.Duration(wakes)*l.Wake
}
