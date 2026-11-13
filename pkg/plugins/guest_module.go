package plugins

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Functions a plugin module exports for the host to call
var guestModuleExports = []string{"_initialize", "icebath_alloc", "icebath_call", "icebath_free"}

var errGuestShutdown = errors.New("shutdown")

// One WebAssembly plugin instance, every call into it made by the single goroutine that owns it
type wasmGuest struct {
	spec    *guestSpec
	runtime wazero.Runtime
	module  api.Module
	levels  []guestFuncs
	depth   int
	ctx     context.Context
	cancel  context.CancelCauseFunc
	toHost  *io.PipeWriter
	inbound *io.PipeReader
	logs    *io.PipeWriter
	queue   guestQueue
	budget  runBudget
	mu      sync.Mutex
	replies map[int64]chan []byte
	active  map[int64]bool
	gone    map[int64]bool
	held    map[uint32][]byte
	tokens  uint32
}

// Exported functions of the module for one nesting depth, a call engine serving one call at a time
type guestFuncs struct {
	alloc api.Function
	call  api.Function
	free  api.Function
}

// Host request waiting for the guest goroutine
type guestRequest struct {
	id    int64
	frame []byte
}

// Unbounded queue of host requests with a wake signal for its one consumer
type guestQueue struct {
	mu    sync.Mutex
	items []guestRequest
	ready chan struct{}
}

func (q *guestQueue) push(item guestRequest) {
	q.mu.Lock()
	q.items = append(q.items, item)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

func (q *guestQueue) pop() (guestRequest, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return guestRequest{}, false
	}
	item := q.items[0]
	q.items = q.items[1:]
	return item, true
}

// Run time one host call may spend inside the guest, time waiting in host calls excluded
type runBudget struct {
	mu      sync.Mutex
	limit   time.Duration
	left    time.Duration
	since   time.Time
	timer   *time.Timer
	expired func()
}

func (b *runBudget) begin() {
	b.mu.Lock()
	b.left = b.limit
	b.startLocked()
	b.mu.Unlock()
}

func (b *runBudget) resume() {
	b.mu.Lock()
	b.startLocked()
	b.mu.Unlock()
}

func (b *runBudget) pause() {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
		if b.left -= time.Since(b.since); b.left < 0 {
			b.left = 0
		}
	}
	b.mu.Unlock()
}

func (b *runBudget) startLocked() {
	if b.timer != nil {
		b.timer.Stop()
	}
	b.since = time.Now()
	b.timer = time.AfterFunc(b.left, b.expired)
}

var (
	guestCachesMu sync.Mutex
	guestCaches   = map[string]wazero.CompilationCache{}
)

// Compiled code cache of a cache directory, nil when the directory cannot be made
func guestCompilationCache(dir string) wazero.CompilationCache {
	guestCachesMu.Lock()
	defer guestCachesMu.Unlock()
	if cache, ok := guestCaches[dir]; ok {
		return cache
	}
	cache, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return nil
	}
	guestCaches[dir] = cache
	return cache
}

// Compiles and instantiates the plugin module and joins it to a channel through in memory pipes
func (h *PluginHost) launchWasm(ctx context.Context, s *hostStart, spec *guestSpec) (*Channel, *hostProcess, error) {
	entry, err := guestEntryPath(spec)
	if err != nil {
		return nil, nil, err
	}
	code, err := os.ReadFile(entry)
	if err != nil {
		return nil, nil, err
	}
	g := &wasmGuest{spec: spec, replies: map[int64]chan []byte{}, active: map[int64]bool{}, gone: map[int64]bool{}, held: map[uint32][]byte{}}
	g.queue.ready = make(chan struct{}, 1)
	g.ctx, g.cancel = context.WithCancelCause(context.Background())
	g.budget.limit = spec.callLimit
	g.budget.expired = func() {
		g.cancel(fmt.Errorf("plugin '%s' ran longer than its call limit of %s", spec.name, spec.callLimit))
	}
	config := wazero.NewRuntimeConfig().WithCloseOnContextDone(true).WithMemoryLimitPages(spec.memoryPages)
	if cache := guestCompilationCache(guestCacheDir()); cache != nil {
		config = config.WithCompilationCache(cache)
	}
	g.runtime = wazero.NewRuntimeWithConfig(g.ctx, config)
	logReader, logWriter := io.Pipe()
	g.logs = logWriter
	go pumpHostStderr(logReader, spec.name)
	fail := func(err error) (*Channel, *hostProcess, error) {
		g.cancel(err)
		_ = g.runtime.Close(context.Background())
		_ = logWriter.Close()
		return nil, nil, err
	}
	if _, err := wasi_snapshot_preview1.Instantiate(g.ctx, g.runtime); err != nil {
		return fail(err)
	}
	if _, err := g.runtime.NewHostModuleBuilder("icebath").
		NewFunctionBuilder().WithFunc(g.hostCall).Export("host_call").
		NewFunctionBuilder().WithFunc(g.hostRead).Export("host_read").
		NewFunctionBuilder().WithFunc(g.hostCancelled).Export("cancelled").
		Instantiate(g.ctx); err != nil {
		return fail(err)
	}
	compiled, err := g.runtime.CompileModule(g.ctx, code)
	if err != nil {
		return fail(fmt.Errorf("plugin '%s' module %s does not compile: %w", spec.name, spec.entry[0], err))
	}
	exports := compiled.ExportedFunctions()
	for _, name := range guestModuleExports {
		if _, ok := exports[name]; !ok {
			return fail(fmt.Errorf("plugin '%s' module %s exports no %s; build it from Go that imports pkg/pluginsdk with GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared",
				spec.name, spec.entry[0], name))
		}
	}
	g.budget.begin()
	g.module, err = g.runtime.InstantiateModule(g.ctx, compiled, wazero.NewModuleConfig().WithName(spec.name).
		WithStartFunctions("_initialize").WithStdout(logWriter).WithStderr(logWriter).
		WithSysWalltime().WithSysNanotime().WithSysNanosleep().WithRandSource(rand.Reader))
	g.budget.pause()
	if err != nil {
		if cause := context.Cause(g.ctx); cause != nil {
			err = cause
		}
		return fail(fmt.Errorf("plugin '%s' module %s failed to start: %w", spec.name, spec.entry[0], err))
	}
	hostReader, guestWriter := io.Pipe()
	guestReader, hostWriter := io.Pipe()
	g.toHost, g.inbound = guestWriter, guestReader
	child := &hostProcess{done: make(chan struct{}), stop: func() { g.cancel(errGuestShutdown) }}
	ch, err := h.installGuest(s, child, hostReader, hostWriter, "plugin "+spec.name)
	if err != nil {
		_ = guestWriter.Close()
		_ = guestReader.Close()
		_, _, _ = fail(err)
		return nil, nil, err
	}
	go g.route(guestReader)
	go g.loop(child)
	return ch, child, nil
}

// Reads the frames the host sends, handing replies to their waiting host call and queueing requests for the guest
func (g *wasmGuest) route(reader *io.PipeReader) {
	r := bufio.NewReaderSize(reader, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if frame := bytes.TrimSpace(line); len(frame) > 0 {
			g.accept(frame)
		}
		if err != nil {
			break
		}
	}
	_ = reader.Close()
	g.cancel(errGuestShutdown)
}

func (g *wasmGuest) accept(frame []byte) {
	var head struct {
		ID     json.Number     `json:"id"`
		Method *string         `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(frame, &head); err != nil {
		logWarn(fmt.Sprintf("plugin '%s': dropped a frame that is not JSON: %v", g.spec.name, err))
		return
	}
	id, _ := head.ID.Int64()
	if head.Method == nil {
		g.mu.Lock()
		reply := g.replies[id]
		delete(g.replies, id)
		g.mu.Unlock()
		if reply != nil {
			reply <- frame
		}
		return
	}
	if *head.Method == "cancel" {
		var cancelled struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(head.Params, &cancelled); err == nil {
			gone := cancelled.ID
			g.mu.Lock()
			if g.active[gone] {
				g.gone[gone] = true
			}
			g.mu.Unlock()
		}
		return
	}
	g.mu.Lock()
	g.active[id] = true
	g.mu.Unlock()
	g.queue.push(guestRequest{id: id, frame: frame})
}

// Owns the instance, running queued host requests one at a time until the guest is stopped
func (g *wasmGuest) loop(child *hostProcess) {
	defer close(child.done)
	for g.ctx.Err() == nil {
		if item, ok := g.queue.pop(); ok {
			g.budget.begin()
			g.run(item)
			g.budget.pause()
			continue
		}
		select {
		case <-g.queue.ready:
		case <-g.ctx.Done():
		}
	}
	cause := context.Cause(g.ctx)
	if !errors.Is(cause, errGuestShutdown) {
		child.code = 1
		logWarn(fmt.Sprintf("plugin '%s' WebAssembly instance stopped: %v", g.spec.name, cause))
	}
	_ = g.runtime.Close(context.Background())
	_ = g.toHost.CloseWithError(cause)
	_ = g.inbound.Close()
	_ = g.logs.Close()
}

// Runs one host request in the guest and sends its reply frame
func (g *wasmGuest) run(item guestRequest) {
	reply, err := g.invoke(item.frame)
	g.mu.Lock()
	delete(g.active, item.id)
	delete(g.gone, item.id)
	g.mu.Unlock()
	if err != nil {
		g.cancel(err)
		return
	}
	if reply != nil {
		_, _ = g.toHost.Write(append(reply, '\n'))
	}
}

// Copies a frame into guest memory, calls the guest with it and copies the reply frame out
func (g *wasmGuest) invoke(frame []byte) ([]byte, error) {
	if g.depth == len(g.levels) {
		g.levels = append(g.levels, guestFuncs{alloc: g.module.ExportedFunction("icebath_alloc"),
			call: g.module.ExportedFunction("icebath_call"), free: g.module.ExportedFunction("icebath_free")})
	}
	f := g.levels[g.depth]
	g.depth++
	defer func() { g.depth-- }()
	res, err := f.alloc.Call(g.ctx, uint64(len(frame)))
	if err != nil {
		return nil, err
	}
	ptr := uint32(res[0])
	if !g.module.Memory().Write(ptr, frame) {
		return nil, fmt.Errorf("plugin '%s' returned a buffer outside its memory", g.spec.name)
	}
	res, err = f.call.Call(g.ctx, uint64(ptr), uint64(len(frame)))
	if err != nil {
		return nil, err
	}
	packed := res[0]
	if packed == 0 {
		return nil, nil
	}
	replyPtr := uint32(packed >> 32)
	view, ok := g.module.Memory().Read(replyPtr, uint32(packed))
	if !ok {
		return nil, fmt.Errorf("plugin '%s' returned a reply outside its memory", g.spec.name)
	}
	reply := append([]byte(nil), view...)
	if _, err := f.free.Call(g.ctx, uint64(replyPtr)); err != nil {
		return nil, err
	}
	return reply, nil
}

// Sends one guest request to the host and waits for its reply, running host requests that arrive meanwhile
func (g *wasmGuest) hostCall(_ context.Context, mod api.Module, ptr, size uint32) uint64 {
	view, ok := mod.Memory().Read(ptr, size)
	if !ok {
		return 0
	}
	frame := append(append([]byte(nil), view...), '\n')
	var head struct {
		ID json.Number `json:"id"`
	}
	if err := json.Unmarshal(frame, &head); err != nil {
		return 0
	}
	id, err := head.ID.Int64()
	if err != nil {
		return 0
	}
	reply := make(chan []byte, 1)
	g.mu.Lock()
	g.replies[id] = reply
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.replies, id)
		g.mu.Unlock()
	}()
	g.budget.pause()
	defer g.budget.resume()
	if _, err := g.toHost.Write(frame); err != nil {
		return 0
	}
	for g.ctx.Err() == nil {
		select {
		case data := <-reply:
			return g.hold(data)
		default:
		}
		if item, ok := g.queue.pop(); ok {
			g.budget.resume()
			g.run(item)
			g.budget.pause()
			continue
		}
		select {
		case data := <-reply:
			return g.hold(data)
		case <-g.queue.ready:
		case <-g.ctx.Done():
		}
	}
	return 0
}

// Keeps a reply until the guest copies it and returns its token and length packed
func (g *wasmGuest) hold(data []byte) uint64 {
	g.mu.Lock()
	g.tokens++
	token := g.tokens
	g.held[token] = data
	g.mu.Unlock()
	return uint64(token)<<32 | uint64(uint32(len(data)))
}

// Copies a held reply into the buffer the guest allocated for it
func (g *wasmGuest) hostRead(_ context.Context, mod api.Module, token, ptr uint32) {
	g.mu.Lock()
	data := g.held[token]
	delete(g.held, token)
	g.mu.Unlock()
	mod.Memory().Write(ptr, data)
}

// Reports whether the caller of a host request the guest is serving has gone away
func (g *wasmGuest) hostCancelled(_ context.Context, _ api.Module, id uint64) uint32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gone[int64(id)] {
		return 1
	}
	return 0
}
