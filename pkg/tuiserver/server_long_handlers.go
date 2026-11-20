package tuiserver

import "sync"

// Methods slow enough to run on the worker pool so the reader keeps answering interrupts and approvals
var (
	longHandlersMu sync.RWMutex
	longHandlers   = map[string]bool{
		"session.usage": true, "browser.manage": true, "complete.path": true, "complete.slash": true,
		"model.options": true, "pet.cells": true, "pet.gallery": true, "pet.generate": true,
		"pet.hatch": true, "pet.info": true, "pet.select": true, "learning.frames": true, "learning.graph": true,
		"plugins.manage": true, "reload.mcp": true, "mcp.servers.test": true,
		"mcp.servers.oauth.start": true, "process.list": true,
		"profiles.list": true,
		"setup.status":  true,
		"voice.toggle":  true, "voice.record": true,
		"wake.start": true, "wake.status": true, "session.active_list": true, "session.branch": true,
		"session.compress": true, "session.list": true, "session.resume": true, "session.save": true,
		"session.workspace.move": true, "shell.exec": true, "skills.manage": true, "slash.exec": true,
		"command.dispatch": true,
	}
)

// Adds methods to the pool dispatched set, the union a methods module applies when it installs
func addLongHandlers(names ...string) {
	longHandlersMu.Lock()
	defer longHandlersMu.Unlock()
	next := make(map[string]bool, len(longHandlers)+len(names))
	for name := range longHandlers {
		next[name] = true
	}
	for _, name := range names {
		next[name] = true
	}
	longHandlers = next
}
