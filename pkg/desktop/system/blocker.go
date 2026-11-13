package system

import "sync"

// Hold is one native sleep inhibition, released exactly once
type Hold interface {
	Release()
}

// Native inhibition for a blocker kind, nil hold when the platform has nothing to inhibit with
type inhibitFunc func(kind KeepAwakeType, reason string) Hold

// NativeBlocker numbers each request like Electron powerSaveBlocker and keeps it started until stopped
type NativeBlocker struct {
	mu      sync.Mutex
	next    int
	held    map[int]Hold
	reason  string
	inhibit inhibitFunc
}

// Blocker over the platform sleep inhibitors, reason naming the app to the OS
func NewNativeBlocker(reason string) *NativeBlocker {
	return &NativeBlocker{held: map[int]Hold{}, reason: reason, inhibit: platformInhibit}
}

// Takes a native inhibition of kind and returns its id
func (b *NativeBlocker) Start(kind KeepAwakeType) int {
	hold := b.inhibit(kind, b.reason)
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.next
	b.next++
	b.held[id] = hold
	return id
}

// Releases the inhibition of id
func (b *NativeBlocker) Stop(id int) {
	b.mu.Lock()
	hold, ok := b.held[id]
	delete(b.held, id)
	b.mu.Unlock()
	if ok && hold != nil {
		hold.Release()
	}
}

// True while id is held
func (b *NativeBlocker) IsStarted(id int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.held[id]
	return ok
}
