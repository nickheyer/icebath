package pty

import "sync"

// Terminal exit a renderer receives, a null code and signal where the platform reports none
type ExitPayload struct {
	Code   *int    `json:"code"`
	Signal *string `json:"signal"`
}

// Characters of shell output kept while no renderer is attached
const maxPendingOutput = 1024 * 1024

// Buffers shell output and the exit until the renderer attaches, then streams both
type OutputGate struct {
	onExitFlushed func()
	sendData      func(data string)
	sendExit      func(payload ExitPayload)

	mu          sync.Mutex
	attached    bool
	pendingData []rune
	pendingExit *ExitPayload
	exited      bool
}

// Detached gate over the three deliveries of one terminal session
func NewOutputGate(onExitFlushed func(), sendData func(string), sendExit func(ExitPayload)) *OutputGate {
	return &OutputGate{onExitFlushed: onExitFlushed, sendData: sendData, sendExit: sendExit}
}

func (g *OutputGate) flushExit(payload ExitPayload) {
	g.sendExit(payload)
	g.onExitFlushed()
}

// Marks the renderer attached, delivering the buffered output and then a buffered exit
func (g *OutputGate) Attach() {
	g.mu.Lock()
	if g.attached {
		g.mu.Unlock()
		return
	}
	g.attached = true
	data := string(g.pendingData)
	g.pendingData = nil
	exit := g.pendingExit
	g.pendingExit = nil
	g.mu.Unlock()
	if data != "" {
		g.sendData(data)
	}
	if exit != nil {
		g.flushExit(*exit)
	}
}

// Streams a chunk to an attached renderer, else keeps the newest megabyte of characters
func (g *OutputGate) Data(chunk string) {
	g.mu.Lock()
	if g.attached {
		g.mu.Unlock()
		g.sendData(chunk)
		return
	}
	g.pendingData = append(g.pendingData, []rune(chunk)...)
	if over := len(g.pendingData) - maxPendingOutput; over > 0 {
		g.pendingData = append([]rune(nil), g.pendingData[over:]...)
	}
	g.mu.Unlock()
}

// Drops any exit so an app quit never reads to the renderer as the shell exiting
func (g *OutputGate) Silence() {
	g.mu.Lock()
	g.exited = true
	g.pendingExit = nil
	g.mu.Unlock()
}

// Records the first exit only, delivering it now when attached and holding it otherwise
func (g *OutputGate) Exit(payload ExitPayload) {
	g.mu.Lock()
	if g.exited {
		g.mu.Unlock()
		return
	}
	g.exited = true
	if g.attached {
		g.mu.Unlock()
		g.flushExit(payload)
		return
	}
	g.pendingExit = &payload
	g.mu.Unlock()
}
