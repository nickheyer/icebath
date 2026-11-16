package app

import (
	"io"
	"sync"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
)

// Message repainting the monitor
type monitorRepaintMsg struct{}

// Full height subagent and process monitor, header, roster or tail, steer input, notice and footer
type monitorApp struct {
	monitor *subagentMonitor
	shell   *ptApplication
	roster  *ptBuffer
	tail    *ptBuffer
	steer   *ptBuffer
	style   *ptMergedStyle
	in      io.Reader
	out     io.Writer
	program atomic.Pointer[tea.Program]
	done    atomic.Bool

	mu        sync.Mutex
	focus     string
	steering  bool
	confirm   bool
	notice    string
	target    any
	cols      int
	rows      int
	rosterTop int
	tailTop   int
	tailTop2  int
	steerLeft int
}

// Steer or stop call the monitor sends off the UI loop
var monitorControl = (*subagentMonitor).control

// Panic value carried as an error
type panicError struct{ text string }

// Repaints the monitor from any goroutine
func (a *monitorApp) invalidate() {
	p := a.program.Load()
	if a.done.Load() || p == nil {
		return
	}
	go p.Send(monitorRepaintMsg{})
}

// monitorModel adapts the monitor to the bubbletea model contract
type monitorModel struct{ app *monitorApp }
