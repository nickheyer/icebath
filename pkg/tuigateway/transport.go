package tuigateway

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Peer a JSON RPC frame is forwarded to, Write false once the peer is gone
type Transport interface {
	Write(obj *pyjson.Object) bool
	Close()
}

// Transport that aborts its socket on overflow instead of closing it
type abortingTransport interface {
	Abort()
}

// Transport sending a whole batch and awaiting the real send while its generation stays current
type batchTransport interface {
	WriteManyIfCurrentAsync(frames []*pyjson.Object, isCurrent func() bool) bool
}

// JSON text of a frame, an internal error frame carrying the original id when it cannot be serialized
func SerializeFrame(obj *pyjson.Object, peer string, log *slog.Logger) string {
	text, err := pyjson.Dumps(obj, pyjson.EnsureASCII(false))
	if err == nil {
		return text
	}
	rid, _ := obj.Get("id")
	log.Error(fmt.Sprintf("frame serialization failed peer=%s id=%v error_type=%T error=%v", peer, rid, err, err))
	fallback := pyjson.ObjectOf("jsonrpc", "2.0", "id", rid,
		"error", pyjson.ObjectOf("code", -32603, "message", "response serialization error: "+err.Error()))
	text, err = pyjson.Dumps(fallback, pyjson.EnsureASCII(false))
	if err != nil {
		panic(err)
	}
	return text
}

type pendingFrame struct {
	frame *pyjson.Object
	size  int
}

type fanoutPeer struct {
	transport    Transport
	pending      []pendingFrame
	pendingBytes int
	writing      bool
	attached     bool
	generation   int
}

// Ordered bounded per peer session event mailboxes, a slow peer losing its subscription on overflow
type FanoutTransport struct {
	mu    sync.Mutex
	peers []*fanoutPeer
}

// Bounds of one peer mailbox, the frame count and the serialized bytes it may hold unsent
var (
	fanoutMaxPendingFrames = 16384
	fanoutMaxPendingBytes  = 4 * 1024 * 1024
)

// Fanout over the given transports
func NewFanoutTransport(transports ...Transport) *FanoutTransport {
	f := &FanoutTransport{}
	for _, t := range transports {
		f.Attach(t)
	}
	return f
}

// Adds a peer, reusing a detached peer's writer for the same transport, false when already attached
func (f *FanoutTransport) Attach(transport Transport) bool {
	if transport == nil {
		return false
	}
	if self, ok := transport.(*FanoutTransport); ok && self == f {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, peer := range f.peers {
		if peer.transport == transport {
			if peer.attached {
				return false
			}
			peer.pending = nil
			peer.pendingBytes = 0
			peer.attached = true
			peer.generation++
			return true
		}
	}
	f.peers = append(f.peers, &fanoutPeer{transport: transport, attached: true})
	return true
}

// Revokes a peer's queue and captured batches, dropping it once no writer holds it
func (f *FanoutTransport) remove(peer *fanoutPeer) {
	peer.attached = false
	peer.generation++
	peer.pending = nil
	peer.pendingBytes = 0
	if peer.writing {
		return
	}
	for i, p := range f.peers {
		if p == peer {
			f.peers = append(f.peers[:i], f.peers[i+1:]...)
			return
		}
	}
}

// Detaches a transport, true when it was attached
func (f *FanoutTransport) Detach(transport Transport) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, peer := range f.peers {
		if peer.transport == transport {
			attached := peer.attached
			f.remove(peer)
			return attached
		}
	}
	return false
}

// True when the transport is an attached peer
func (f *FanoutTransport) Contains(transport Transport) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, peer := range f.peers {
		if peer.attached && peer.transport == transport {
			return true
		}
	}
	return false
}

// Attached peer transports
func (f *FanoutTransport) Transports() []Transport {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Transport
	for _, peer := range f.peers {
		if peer.attached {
			out = append(out, peer.transport)
		}
	}
	return out
}

// True when any attached transport other than excluding remains
func (f *FanoutTransport) HasTransports(excluding Transport) bool {
	for _, t := range f.Transports() {
		if t != excluding {
			return true
		}
	}
	return false
}

// True while the peer still carries the generation a batch was captured under
func (f *FanoutTransport) isCurrent(peer *fanoutPeer, generation int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return peer.generation == generation
}

// Sends a peer's queued frames batch by batch, pruning the peer on a failed write
func (f *FanoutTransport) drain(peer *fanoutPeer) {
	for {
		f.mu.Lock()
		if len(peer.pending) == 0 {
			peer.writing = false
			if !peer.attached {
				f.remove(peer)
			}
			f.mu.Unlock()
			return
		}
		generation := peer.generation
		batch := peer.pending
		peer.pending = nil
		peer.pendingBytes = 0
		f.mu.Unlock()
		ok := f.writeBatch(peer, generation, batch)
		if !ok {
			f.mu.Lock()
			if peer.generation != generation {
				f.mu.Unlock()
				continue
			}
			peer.writing = false
			f.remove(peer)
			f.mu.Unlock()
			return
		}
	}
}

// Writes one batch, a panicking transport counting as a failed write
func (f *FanoutTransport) writeBatch(peer *fanoutPeer, generation int, batch []pendingFrame) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("fanout write failed; pruning peer: %v", r))
			ok = false
		}
	}()
	if writer, many := peer.transport.(batchTransport); many {
		frames := make([]*pyjson.Object, len(batch))
		for i, item := range batch {
			frames[i] = item.frame
		}
		return writer.WriteManyIfCurrentAsync(frames, func() bool { return f.isCurrent(peer, generation) })
	}
	for _, item := range batch {
		if f.isCurrent(peer, generation) && !peer.transport.Write(item.frame) {
			return false
		}
	}
	return true
}

// Aborts or closes an overflowed transport outside the membership lock
func (f *FanoutTransport) signalOverflowDetach(transport Transport) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("fanout overflow close failed; membership already dropped: %v", r))
		}
	}()
	if aborting, ok := transport.(abortingTransport); ok {
		aborting.Abort()
		return
	}
	transport.Close()
}

// Fans a frame out to every attached peer, true while any peer stays attached
func (f *FanoutTransport) Write(obj *pyjson.Object) bool {
	return f.WriteOverflow(obj, nil)
}

// Fans a frame out, an overflowed peer getting the notice overflow returns or its transport closed
func (f *FanoutTransport) WriteOverflow(obj *pyjson.Object, overflow func(Transport) *pyjson.Object) bool {
	encoded := SerializeFrame(obj, "fanout", slog.Default())
	size := len(encoded)
	decoded, err := pyjson.Loads(encoded)
	if err != nil {
		panic(err)
	}
	frame := decoded.(*pyjson.Object)
	var overflowed []Transport
	f.mu.Lock()
	for _, peer := range append([]*fanoutPeer(nil), f.peers...) {
		if !peer.attached {
			continue
		}
		if len(peer.pending) >= fanoutMaxPendingFrames || peer.pendingBytes+size > fanoutMaxPendingBytes {
			slog.Warn("fanout subscriber backlog full; detaching peer")
			f.remove(peer)
			var notice *pyjson.Object
			if overflow != nil {
				notice = overflow(peer.transport)
			}
			if notice == nil {
				overflowed = append(overflowed, peer.transport)
				continue
			}
			peer.pending = append(peer.pending, pendingFrame{frame: notice})
			present := false
			for _, p := range f.peers {
				present = present || p == peer
			}
			if !present {
				f.peers = append(f.peers, peer)
			}
			f.startWriter(peer)
			continue
		}
		peer.pending = append(peer.pending, pendingFrame{frame: frame, size: size})
		peer.pendingBytes += size
		f.startWriter(peer)
	}
	remaining := false
	for _, peer := range f.peers {
		remaining = remaining || peer.attached
	}
	f.mu.Unlock()
	for _, t := range overflowed {
		f.signalOverflowDetach(t)
	}
	return remaining
}

// Starts the peer's single writer goroutine when none runs
func (f *FanoutTransport) startWriter(peer *fanoutPeer) {
	if !peer.writing {
		peer.writing = true
		go f.drain(peer)
	}
}

// Detaches every peer without closing their sockets
func (f *FanoutTransport) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, peer := range append([]*fanoutPeer(nil), f.peers...) {
		f.remove(peer)
	}
}
