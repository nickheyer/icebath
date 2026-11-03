package vxl

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// Share of work one helper goroutine takes, indices pulled from a counter until none are left
type job struct {
	fn   func(i int)
	n    int
	next *atomic.Int32
	wg   *sync.WaitGroup
}

// Runs indices off the shared counter until every one is taken
func (j job) run() {
	for {
		i := int(j.next.Add(1)) - 1
		if i >= j.n {
			return
		}
		j.fn(i)
	}
}

var (
	poolOnce sync.Once
	poolJobs chan job
)

// Starts one parked helper per CPU, kept for the life of the process
func startPool() {
	poolJobs = make(chan job, 4*runtime.NumCPU())
	for range runtime.NumCPU() {
		go func() {
			for j := range poolJobs {
				j.run()
				j.wg.Done()
			}
		}()
	}
}

// Calls fn for every index below n across at most a count of goroutines, the caller taking a share
func parallel(n, workers int, fn func(i int)) {
	if n <= 0 {
		return
	}
	workers = min(workers, n)
	if workers <= 1 {
		for i := range n {
			fn(i)
		}
		return
	}
	poolOnce.Do(startPool)
	j := job{fn: fn, n: n, next: new(atomic.Int32), wg: new(sync.WaitGroup)}
	j.wg.Add(workers - 1)
	for range workers - 1 {
		select {
		case poolJobs <- j:
		default:
			j.wg.Done()
		}
	}
	j.run()
	j.wg.Wait()
}

// Runs fn over bands of rows side by side, each band at least eight rows
func parallelRows(h int, fn func(y0, y1 int)) { parallelBands(h, 8, fn) }

// Runs fn over bands of rows side by side, each band at least a grain of rows
func parallelBands(h, grain int, fn func(y0, y1 int)) {
	if h <= 0 {
		return
	}
	workers := min(runtime.NumCPU(), max(h/max(grain, 1), 1))
	band := (h + workers - 1) / workers
	parallel((h+band-1)/band, workers, func(b int) { fn(b*band, min((b+1)*band, h)) })
}

// Runs fn over bands of rows across at most a count of goroutines, each band at least a grain of rows
func parallelBandsWith(h, grain, workers int, fn func(y0, y1 int)) {
	if h <= 0 {
		return
	}
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	workers = min(workers, max(h/max(grain, 1), 1))
	band := (h + workers - 1) / workers
	parallel((h+band-1)/band, workers, func(b int) { fn(b*band, min((b+1)*band, h)) })
}
