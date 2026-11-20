package matrix

import (
	"context"
	"slices"
	"sync"
	"time"
)

const (
	claimWindowSeconds   = 120.0
	settleTimeoutSeconds = 5.0
	maxParkedPerSender   = 4
)

var processStart = time.Now()

// Monotonic seconds, swappable in tests
var monotonic = func() float64 { return time.Since(processStart).Seconds() }

// Voice event parked until a bare mention claims it
type ParkedVoice struct {
	EventID   string
	Content   map[string]any
	RelatesTo map[string]any
}

// Whether the content carries the MSC3245 voice message marker
func hasVoiceMarker(content map[string]any) bool {
	v, ok := content["org.matrix.msc3245.voice"]
	return ok && v != nil
}

// Whether the content is an audio message marked as a voice message
func isVoiceEvent(content map[string]any) bool {
	return content["msgtype"] == "m.audio" && hasVoiceMarker(content)
}

// One voice being gated, seq ordering voices from the same sender by arrival
type VoiceGate struct {
	seq  int
	done chan struct{}
	once sync.Once
}

type senderKey struct{ room, sender string }

type parkedEntry struct {
	at    float64
	seq   int
	voice ParkedVoice
}

// Voices parked per room and sender awaiting a separate mention event within the claim window
type ParkedVoices struct {
	mu       sync.Mutex
	parked   map[senderKey][]parkedEntry
	inflight map[senderKey][]*VoiceGate
	floor    map[senderKey]int
	nextSeq  int
}

// Empty parked voice registry
func NewParkedVoices() *ParkedVoices {
	return &ParkedVoices{parked: map[senderKey][]parkedEntry{}, inflight: map[senderKey][]*VoiceGate{}, floor: map[senderKey]int{}}
}

// Drops parked voices older than the claim window
func (p *ParkedVoices) prune() {
	cutoff := monotonic() - claimWindowSeconds
	for k, v := range p.parked {
		kept := v[:0:0]
		for _, e := range v {
			if e.at >= cutoff {
				kept = append(kept, e)
			}
		}
		if len(kept) == 0 {
			delete(p.parked, k)
		} else {
			p.parked[k] = kept
		}
	}
}

// Whether an unexpired voice is parked or one is still being gated for the sender in the room
func (p *ParkedVoices) Pending(roomID, sender string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := senderKey{roomID, sender}
	if _, ok := p.inflight[key]; ok {
		return true
	}
	cutoff := monotonic() - claimWindowSeconds
	for _, e := range p.parked[key] {
		if e.at >= cutoff {
			return true
		}
	}
	return false
}

// Arrival limit for a bare mention so only voices that began before it may be claimed
func (p *ParkedVoices) Mark() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nextSeq
}

// Marks a parkable voice as gated, always paired with an idempotent release
func (p *ParkedVoices) Begin(roomID, sender string) *VoiceGate {
	p.mu.Lock()
	defer p.mu.Unlock()
	gate := &VoiceGate{seq: p.nextSeq, done: make(chan struct{})}
	p.nextSeq++
	key := senderKey{roomID, sender}
	p.inflight[key] = append(p.inflight[key], gate)
	return gate
}

// Finishes a gate, clearing the sender floor once no older voice can park
func (p *ParkedVoices) Release(roomID, sender string, gate *VoiceGate) {
	gate.once.Do(func() { close(gate.done) })
	p.mu.Lock()
	defer p.mu.Unlock()
	key := senderKey{roomID, sender}
	gates := p.inflight[key]
	if i := slices.Index(gates, gate); i >= 0 {
		gates = slices.Delete(gates, i, i+1)
		if len(gates) == 0 {
			delete(p.inflight, key)
			delete(p.floor, key)
		} else {
			p.inflight[key] = gates
		}
	}
}

// Waits up to the settle timeout for every concurrently gated voice from the sender to park or drop
func (p *ParkedVoices) Settle(ctx context.Context, roomID, sender string) {
	p.mu.Lock()
	gates := slices.Clone(p.inflight[senderKey{roomID, sender}])
	p.mu.Unlock()
	if len(gates) == 0 {
		return
	}
	timer := time.NewTimer(time.Duration(settleTimeoutSeconds * float64(time.Second)))
	defer timer.Stop()
	for _, g := range gates {
		select {
		case <-g.done:
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// Parks the voice unless a newer voice from the sender was already claimed, keeping the newest four
func (p *ParkedVoices) Park(roomID, sender string, gate *VoiceGate, eventID string, content, relatesTo map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := senderKey{roomID, sender}
	floor, ok := p.floor[key]
	if !ok {
		floor = -1
	}
	if gate.seq <= floor {
		return
	}
	p.prune()
	entries := append(p.parked[key], parkedEntry{monotonic(), gate.seq, ParkedVoice{eventID, content, relatesTo}})
	slices.SortStableFunc(entries, func(a, b parkedEntry) int { return a.seq - b.seq })
	if len(entries) > maxParkedPerSender {
		entries = entries[len(entries)-maxParkedPerSender:]
	}
	p.parked[key] = entries
}

// Pops the newest parked voice that began before the mention mark, dropping older ones and keeping later ones
func (p *ParkedVoices) Claim(roomID, sender string, before int) (ParkedVoice, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune()
	key := senderKey{roomID, sender}
	entries := p.parked[key]
	idx := -1
	for i, e := range entries {
		if e.seq < before {
			idx = i
		}
	}
	if idx < 0 {
		return ParkedVoice{}, false
	}
	claimed := entries[idx]
	entries = entries[idx+1:]
	if len(entries) == 0 {
		delete(p.parked, key)
	} else {
		p.parked[key] = entries
	}
	if _, ok := p.inflight[key]; ok {
		floor, ok := p.floor[key]
		if !ok {
			floor = -1
		}
		p.floor[key] = max(floor, claimed.seq)
	}
	return claimed.voice, true
}
