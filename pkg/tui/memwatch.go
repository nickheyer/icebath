package tui

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

const (
	// Gap between two samples of the memory watchdog
	memorySampleEvery = 10 * time.Second
	// Live heap from which fast growth is worth a warning, lowered to half of the high level when the budget is small
	memoryWarnFloor = 600 << 20
	// Share of the heap budget at which the watchdog sheds the rendered blocks and half of the pictures and writes a dump
	memoryHighShare = 0.70
	// Share of the heap budget at which the watchdog sheds every render cache, dumps and ends the process
	memoryCriticalShare = 0.88
	// Wait between two automatic dumps of the high level, unless the settings give another
	memoryDumpCooldown = 10 * time.Minute
	// Exit status of a process that ended itself before the system ran out of memory, the one a kill by the system leaves
	memoryExitStatus = 137
	// Longest the watchdog waits for the UI loop to shed its caches before it dumps anyway
	memoryShedWait = 2 * time.Second
	// Longest the critical exit waits for the UI loop to leave before the watchdog ends the process itself
	memoryExitFailsafe = 4 * time.Second
	// Memory assumed when neither a control group limit nor the physical memory can be read
	memoryAssumedCeiling = 8 << 30
	// Levels of the watchdog as its notices name them
	levelHigh, levelCritical = "high", "critical"
)

// Live heap sizes the memory watchdog acts at, with the limits they were derived from
type memoryThresholds struct {
	warn, step, high, critical uint64
	budget                     uint64
	source                     string
	goLimit, cgroup, physical  uint64
	// GOGC of the collector, negative when it is off
	percent int64
}

// Thresholds of this process on its live heap: the budget is the Go memory limit when one is set within what the machine allows, since the collector
// then holds the whole process under it, else the control group limit or the physical memory divided by the growth the collector allows between
// two collections (1 + GOGC/100), since those bound what the process holds and not what is live; critical is 88% and high 70% of the budget,
// and the fast growth warning starts at 600 MiB, or half of high when that is less, and asks for a quarter of that floor gained between two samples
func resolveMemoryThresholds() memoryThresholds {
	t := memoryThresholds{cgroup: cgroupMemoryLimit(), physical: physicalMemory(), percent: 100}
	if limit := debug.SetMemoryLimit(-1); limit > 0 && limit < math.MaxInt64 {
		t.goLimit = uint64(limit)
	}
	sample := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() == metrics.KindUint64 {
		// A collector switched off reads as minus one widened to the whole range
		if t.percent = -1; sample[0].Value.Uint64() <= math.MaxInt32 {
			t.percent = int64(sample[0].Value.Uint64())
		}
	}
	hard, source := t.cgroup, "control group limit"
	if hard == 0 || (t.physical > 0 && t.physical < hard) {
		hard, source = t.physical, "physical memory"
	}
	if hard == 0 {
		hard, source = memoryAssumedCeiling, "assumed ceiling"
	}
	if t.goLimit > 0 && t.goLimit <= hard {
		t.budget, t.source = t.goLimit, "Go memory limit"
	} else {
		growth := 1.0
		if t.percent >= 0 {
			growth += float64(t.percent) / 100
		}
		t.budget, t.source = uint64(float64(hard)/growth), source
	}
	t.critical, t.high = uint64(float64(t.budget)*memoryCriticalShare), uint64(float64(t.budget)*memoryHighShare)
	t.warn = min(memoryWarnFloor, t.high/2)
	t.step = max(t.warn/4, 1)
	return t
}

// Watchdog that samples the live heap, warns once on fast growth, sheds caches and dumps at its high level and ends the process at its critical one
type memoryWatch struct {
	limits   memoryThresholds
	cooldown time.Duration
	// Heap of the sample before, negative until the first so a process that starts large is not taken for one that grew
	lastHeap int64
	warned   bool
	// Levels that acted since the heap was last below the high level
	dumped   map[string]bool
	lastAuto time.Time
	warn     func(memorySnapshot)
	shed     func(all bool)
	high     func(memorySnapshot, heapDumpResult)
	critical func(memorySnapshot, heapDumpResult)
}

// Samples on every tick until stop closes
func (w *memoryWatch) run(stop <-chan struct{}) {
	ticker := time.NewTicker(memorySampleEvery)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			w.tick(now)
		}
	}
}

// Takes one sample and acts on the level it shows
func (w *memoryWatch) tick(now time.Time) {
	snap := sampleMemory()
	heap := int64(min(snap.heap, math.MaxInt64))
	if snap.heap < w.limits.high && w.lastHeap >= 0 {
		switch {
		case !w.warned && snap.heap >= w.limits.warn && heap-w.lastHeap >= int64(w.limits.step):
			w.warned = true
			w.warn(snap)
		case snap.heap < w.limits.warn:
			w.warned = false
		}
	}
	w.lastHeap = heap
	level := ""
	switch {
	case snap.heap >= w.limits.critical:
		level = levelCritical
	case snap.heap >= w.limits.high:
		level = levelHigh
	}
	if level == "" {
		clear(w.dumped)
		return
	}
	if w.dumped[level] {
		return
	}
	// The cooldown keeps a heap that swings around the high level from dumping at every swing, it never holds the critical exit back
	if level == levelHigh && now.Sub(w.lastAuto) < w.cooldown {
		return
	}
	w.lastAuto = now
	w.shed(level == levelCritical)
	w.dumped[level] = true
	if level == levelCritical {
		w.critical(snap, performHeapDump(dumpAutoCritical))
		return
	}
	w.high(snap, performHeapDump(dumpAutoHigh))
}

// Drops laid out pictures, the least recently used half or all, each asked from the gateway anew when it comes into view
func (s *inlineImages) evict(all bool) {
	drop := (len(s.order) + 1) / 2
	if all {
		drop = len(s.order)
	}
	for _, key := range s.order[:drop] {
		delete(s.renders, key)
	}
	s.order = slices.Clone(s.order[drop:])
	s.gen++
}

// Notes the memory the UI starts with, writes the dump the settings ask for at the start, starts the memory watchdog beside the UI and returns what stops it
func (a *app) startMemoryWatch() func() {
	markMemoryBaseline()
	if prefs.HeapDumpOnStart {
		go func() {
			dump := performHeapDump(dumpManual)
			a.post(asyncDoneMsg{run: func() tea.Cmd { return a.onceSessionStands(sessionWaitTries, func() { a.showHeapDump(dump) }) }})
		}()
	}
	w := &memoryWatch{limits: resolveMemoryThresholds(), cooldown: max(prefs.HeapDumpCooldown, 0), lastHeap: -1, dumped: map[string]bool{},
		warn: a.memoryWarned, shed: a.shedRenderCaches, high: a.memoryHigh, critical: a.memoryCritical}
	stop := make(chan struct{})
	go w.run(stop)
	return func() { close(stop) }
}

// Has the UI loop shed its render caches and waits for it, no longer than the shed wait so a stuck loop cannot hold the dump back: the rendered
// transcript blocks go whole, the rows in view painted anew at the next frame, and the laid out pictures by half or all
func (a *app) shedRenderCaches(all bool) {
	done := make(chan struct{})
	a.post(asyncDoneMsg{run: func() tea.Cmd {
		a.card.view.invalidate()
		a.images.evict(all)
		close(done)
		return nil
	}})
	select {
	case <-done:
	case <-time.After(memoryShedWait):
	}
}

// Line that names the level, the heap and where the automatic dump went
func memoryDumpNotice(level string, snap memorySnapshot, dump heapDumpResult) string {
	return tr(txt.ChatBitsEntryMemoryDump, level, render.Bytes(int64(snap.heap)), dumpPathOr(dump, tr(txt.ChatBitsEntryDumpFailed)))
}

// Profile of a dump, its facts file when no profile was written, the fallback when the dump failed
func dumpPathOr(dump heapDumpResult, fallback string) string {
	for _, path := range []string{dump.heapPath, dump.diagPath} {
		if path != "" {
			return path
		}
	}
	return fallback
}

// Records fast heap growth in the crash log and warns in the transcript, the stderr of the process being held until the terminal is given back
func (a *app) memoryWarned(snap memorySnapshot) {
	heap := render.Bytes(int64(snap.heap))
	recordParentLifecycle(fmt.Sprintf("memory-warning fast heap growth heap=%s rss=%s", heap, render.Bytes(int64(snap.rss))))
	a.post(asyncDoneMsg{run: func() tea.Cmd {
		a.sys(tr(txt.ChatBitsEntryHeapClimbing, heap))
		return nil
	}})
}

// Tells in the transcript that the high level shed caches and where its dump went
func (a *app) memoryHigh(snap memorySnapshot, dump heapDumpResult) {
	a.post(asyncDoneMsg{run: func() tea.Cmd {
		a.sys(memoryDumpNotice(levelHigh, snap, dump))
		return nil
	}})
}

// Records the critical level in the crash log and ends the terminal UI with the memory exit status, its last lines written once the terminal is given back
func (a *app) memoryCritical(snap memorySnapshot, dump heapDumpResult) {
	heap, rss := render.Bytes(int64(snap.heap)), render.Bytes(int64(snap.rss))
	// The exit closes the gateway socket like any other, so this line is what tells a memory exit from a quit afterwards
	recordParentLifecycle(fmt.Sprintf("memory-critical exit(%d) heap=%s rss=%s dump=%s", memoryExitStatus, heap, rss, dumpPathOr(dump, "failed")))
	lines := []string{
		fmt.Sprintf("icebath lifecycle: memory critical exit heap=%s rss=%s", heap, rss),
		memoryDumpNotice(levelCritical, snap, dump),
		tr(txt.ChatBitsEntryExitingOom),
	}
	var once sync.Once
	say := func() {
		once.Do(func() {
			for _, line := range lines {
				fmt.Fprintln(os.Stderr, line)
			}
		})
	}
	a.post(asyncDoneMsg{run: func() tea.Cmd {
		a.lastWords = say
		return a.dieFor(memoryExitStatus, "memory-critical")
	}})
	// A loop too starved to leave is not waited for: the lines go to the held stderr, which the terminal guard shows after it restored the terminal
	time.AfterFunc(memoryExitFailsafe, func() {
		say()
		os.Exit(memoryExitStatus)
	})
}
