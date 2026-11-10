package state

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
)

// One shot signal waiters block on until it is set
type event struct {
	once sync.Once
	ch   chan struct{}
}

func newEvent() *event { return &event{ch: make(chan struct{})} }

func (e *event) set() { e.once.Do(func() { close(e.ch) }) }

func (e *event) wait() { <-e.ch }

// Every admitted but unfinished physical close of one path, lifted only by the last to settle
type teardownBarrier struct {
	event   *event
	pending int
}

// One shared database generation with its refcount and file identity
type generation struct {
	path        string
	db          *DB
	identity    FileIdentity
	hasIdentity bool
	refcount    int
	retired     bool
}

// Registry state, every map guarded by regMu and every open and close performed outside it
var (
	regMu              sync.Mutex
	generations        = map[string]*generation{}
	retiredGenerations = map[*DB]*generation{}
	openingPaths       = map[string]*event{}
	tearingDown        = map[string]*teardownBarrier{}
	pathLifecycleLocks = map[string]*sync.Mutex{}
)

// Constructs the database of a registry path, a package var so tests substitute the open
var openSessionDB = func(path string) (*DB, error) { return Open(path, Options{}) }

// Closes a shared instance after clearing its registry owned flag
func teardownDB(d *DB) {
	d.sharedRegistryOwned.Store(false)
	closeQuietly(d, "Error closing shared SessionDB")
}

// Close that never propagates, logging a failed retired generation capture as data at risk
func closeQuietly(d *DB, debugMessage string) {
	err := d.Close()
	if err == nil {
		return
	}
	var capture *RetiredGenerationCaptureError
	if errors.As(err, &capture) {
		slog.Error(fmt.Sprintf("SessionDB for %s did not settle at close: %s", d.Path(), err))
		return
	}
	slog.Debug(debugMessage, "error", err)
}

// Lifecycle mutex of path, created on first use, caller holds regMu
func pathLifecycleLockLocked(path string) *sync.Mutex {
	lock := pathLifecycleLocks[path]
	if lock == nil {
		lock = &sync.Mutex{}
		pathLifecycleLocks[path] = lock
	}
	return lock
}

// Registers one pending physical close of path in the same lock section that removes its generation
func admitTeardownLocked(path string) *teardownBarrier {
	barrier := tearingDown[path]
	if barrier == nil {
		barrier = &teardownBarrier{event: newEvent()}
		tearingDown[path] = barrier
	}
	barrier.pending++
	return barrier
}

// Settles one admitted teardown, only the last one lifting the path barrier
func finishTeardown(path string, barrier *teardownBarrier) {
	regMu.Lock()
	defer regMu.Unlock()
	barrier.pending--
	if barrier.pending > 0 {
		return
	}
	if tearingDown[path] == barrier {
		delete(tearingDown, path)
	}
	barrier.event.set()
}

// Closes d under its path lifecycle mutex, then settles its barrier slot when one was admitted
func teardownGeneration(path string, d *DB, barrier *teardownBarrier) {
	regMu.Lock()
	lifecycle := pathLifecycleLockLocked(path)
	regMu.Unlock()
	defer func() {
		if barrier != nil {
			finishTeardown(path, barrier)
		}
	}()
	lifecycle.Lock()
	defer lifecycle.Unlock()
	teardownDB(d)
}

// Drops the construction marker of path and wakes its waiters, caller holds regMu
func finishOpening(path string, opening *event) {
	if openingPaths[path] == opening {
		delete(openingPaths, path)
	}
	opening.set()
}

// Shared database of dbPath with one more reference, retiring a generation whose file was replaced
func Acquire(ctx context.Context, dbPath string) (*DB, error) {
	if dbPath == "" {
		dbPath = defaultDBPath(ctx)
	}
	path := realpathLax(dbPath)
	for {
		var waitFor *event
		var opening *event
		var lifecycle *sync.Mutex
		regMu.Lock()
		if gen := generations[path]; gen != nil {
			current, ok := StatDBFileIdentity(path)
			if ok && gen.hasIdentity && current != gen.identity {
				gen.retired = true
				delete(generations, path)
				retiredGenerations[gen.db] = gen
			} else {
				gen.refcount++
				regMu.Unlock()
				return gen.db, nil
			}
		}
		if teardown := tearingDown[path]; teardown != nil {
			waitFor = teardown.event
		} else if existing := openingPaths[path]; existing != nil {
			waitFor = existing
		} else {
			opening = newEvent()
			openingPaths[path] = opening
			lifecycle = pathLifecycleLockLocked(path)
		}
		regMu.Unlock()
		if waitFor != nil {
			waitFor.wait()
			continue
		}
		d, identity, hasIdentity, err := openGeneration(path, lifecycle, opening)
		if err != nil {
			regMu.Lock()
			finishOpening(path, opening)
			regMu.Unlock()
			return nil, err
		}
		regMu.Lock()
		teardown := tearingDown[path]
		winner := d
		if teardown == nil {
			if existing := generations[path]; existing != nil {
				existing.refcount++
				winner = existing.db
			} else {
				generations[path] = &generation{path: path, db: d, identity: identity, hasIdentity: hasIdentity, refcount: 1}
			}
		}
		finishOpening(path, opening)
		regMu.Unlock()
		if teardown != nil {
			teardownGeneration(path, d, nil)
			teardown.event.wait()
			continue
		}
		if winner != d {
			teardownGeneration(path, d, nil)
		}
		return winner, nil
	}
}

// Opens the next generation of path under its lifecycle mutex, waking waiters when the open panics
func openGeneration(path string, lifecycle *sync.Mutex, opening *event) (*DB, FileIdentity, bool, error) {
	lifecycle.Lock()
	defer lifecycle.Unlock()
	defer func() {
		if r := recover(); r != nil {
			regMu.Lock()
			finishOpening(path, opening)
			regMu.Unlock()
			panic(r)
		}
	}()
	d, err := openSessionDB(path)
	if err != nil {
		return nil, FileIdentity{}, false, err
	}
	d.sharedRegistryOwned.Store(true)
	identity, ok := StatDBFileIdentity(path)
	return d, identity, ok, nil
}

// Drops one reference of a shared database, false when d is not registry managed and its caller owns the close
func Release(d *DB) bool {
	if d == nil {
		return false
	}
	var barrier *teardownBarrier
	regMu.Lock()
	gen := retiredGenerations[d]
	if gen == nil {
		gen = generations[d.Path()]
		if gen == nil || gen.db != d {
			regMu.Unlock()
			return false
		}
	}
	gen.refcount--
	needsTeardown := gen.refcount <= 0
	if needsTeardown {
		if gen.retired {
			delete(retiredGenerations, d)
		} else if generations[gen.path] == gen {
			delete(generations, gen.path)
		}
		barrier = admitTeardownLocked(gen.path)
	}
	regMu.Unlock()
	if needsTeardown {
		teardownGeneration(gen.path, d, barrier)
	}
	return true
}

// Whether path is root or a file inside it, compared resolved and case folded where the OS folds case
func pathIsUnder(path, root string) bool {
	pathKey, rootKey := normcase(realpathLax(path)), normcase(realpathLax(root))
	return pathKey == rootKey || strings.HasPrefix(pathKey, rootKey+string(filepath.Separator))
}

// Python's os.path.normcase, lower case with forward slashes turned to backslashes on Windows
func normcase(path string) string {
	if runtime.GOOS != "windows" {
		return path
	}
	return strings.ToLower(strings.ReplaceAll(path, "/", `\`))
}

// Closes swept generations outside regMu per path, then waits for teardowns admitted before the sweep
func teardownSweptGenerations(swept []*generation, barriers map[string]*teardownBarrier, active []*teardownBarrier) int {
	byPath := map[string][]*generation{}
	var order []string
	for _, gen := range swept {
		if _, ok := byPath[gen.path]; !ok {
			order = append(order, gen.path)
		}
		byPath[gen.path] = append(byPath[gen.path], gen)
	}
	for _, path := range order {
		closePathGenerations(path, byPath[path], barriers[path])
	}
	for _, barrier := range active {
		barrier.event.wait()
	}
	return len(swept)
}

// Closes every generation of one path under its lifecycle mutex and settles the sweep's barrier slot
func closePathGenerations(path string, gens []*generation, barrier *teardownBarrier) {
	regMu.Lock()
	lifecycle := pathLifecycleLockLocked(path)
	regMu.Unlock()
	defer finishTeardown(path, barrier)
	lifecycle.Lock()
	defer lifecycle.Unlock()
	for _, gen := range gens {
		teardownDB(gen.db)
	}
}

// Live generations followed by retired ones
func allGenerationsLocked() []*generation {
	out := make([]*generation, 0, len(generations)+len(retiredGenerations))
	for _, gen := range generations {
		out = append(out, gen)
	}
	for _, gen := range retiredGenerations {
		out = append(out, gen)
	}
	return out
}

// Closes every shared database regardless of refcount and returns how many, for shutdown after all work finished
func CloseAll() int {
	barriers := map[string]*teardownBarrier{}
	regMu.Lock()
	active := make([]*teardownBarrier, 0, len(tearingDown))
	for _, barrier := range tearingDown {
		active = append(active, barrier)
	}
	swept := allGenerationsLocked()
	for _, gen := range swept {
		if barriers[gen.path] == nil {
			barriers[gen.path] = admitTeardownLocked(gen.path)
		}
	}
	clear(generations)
	clear(retiredGenerations)
	for _, gen := range swept {
		gen.retired = true
	}
	regMu.Unlock()
	return teardownSweptGenerations(swept, barriers, active)
}

// Force closes every shared database whose file lives under directory and returns how many, waiting for pending closes there
func CloseAllUnder(directory string) int {
	root := realpathLax(expandUser(directory))
	barriers := map[string]*teardownBarrier{}
	regMu.Lock()
	var swept []*generation
	for _, gen := range allGenerationsLocked() {
		if pathIsUnder(gen.path, root) {
			swept = append(swept, gen)
		}
	}
	var active []*teardownBarrier
	for path, barrier := range tearingDown {
		if pathIsUnder(path, root) {
			active = append(active, barrier)
		}
	}
	for _, gen := range swept {
		if barriers[gen.path] == nil {
			barriers[gen.path] = admitTeardownLocked(gen.path)
		}
	}
	for _, gen := range swept {
		gen.retired = true
		if generations[gen.path] == gen {
			delete(generations, gen.path)
		}
		delete(retiredGenerations, gen.db)
	}
	regMu.Unlock()
	return teardownSweptGenerations(swept, barriers, active)
}

// Describes every other live generation this process holds for dbPath, retired generations excluded
func OtherGenerationsForPath(dbPath string, exclude *DB) []string {
	path := realpathLax(dbPath)
	regMu.Lock()
	defer regMu.Unlock()
	out := []string{}
	for _, gen := range generations {
		if gen.db != exclude && gen.path == path && !gen.retired {
			out = append(out, fmt.Sprintf("in-process live SessionDB generation (refcount %d)", gen.refcount))
		}
	}
	return out
}

// Snapshot of every live shared database with refcounts untouched, for in process maintenance
func LiveSharedSessionDBs() []*DB {
	regMu.Lock()
	defer regMu.Unlock()
	out := []*DB{}
	for _, gen := range generations {
		if !gen.retired {
			out = append(out, gen.db)
		}
	}
	return out
}

// Runs fn over every live shared database with one registry reference pinned per handle until fn returns
func BorrowLiveSharedSessionDBs(fn func([]*DB) error) error {
	regMu.Lock()
	borrowed := []*DB{}
	for _, gen := range generations {
		if !gen.retired {
			gen.refcount++
			borrowed = append(borrowed, gen.db)
		}
	}
	regMu.Unlock()
	defer func() {
		for _, d := range slices.Backward(borrowed) {
			Release(d)
		}
	}()
	return fn(borrowed)
}

// Registry census of live generations, retired generations and live refcounts
func RegistryStats() map[string]int {
	regMu.Lock()
	defer regMu.Unlock()
	total := 0
	for _, gen := range generations {
		total += gen.refcount
	}
	return map[string]int{
		"live_generations":    len(generations),
		"retired_generations": len(retiredGenerations),
		"total_refcounts":     total,
	}
}

// Releases a shared instance, or closes one the registry does not manage, as a drop in for Close
func ReleaseOrClose(d *DB) {
	if !Release(d) {
		closeQuietly(d, "release_or_close fallback close failed")
	}
}
