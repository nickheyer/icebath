package tuiserver

import "sync"

// Methods slow enough to run on the worker pool so the reader keeps answering interrupts and approvals
var (
	longHandlersMu sync.RWMutex
	longHandlers   = map[string]bool{
		"session.foreign.list": true, "session.foreign.preview": true, "session.foreign.import": true,
		"billing.state": true, "subscription.state": true, "subscription.preview": true, "subscription.change": true,
		"subscription.resume": true, "subscription.upgrade": true, "usage.bars": true, "session.usage": true,
		"billing.step_up": true, "browser.manage": true, "cli.exec": true, "complete.path": true, "complete.slash": true,
		"llm.oneshot": true, "model.options": true, "pet.cells": true, "pet.gallery": true, "pet.generate": true,
		"pet.hatch": true, "pet.info": true, "pet.select": true, "pet.thumb": true, "learning.frames": true,
		"plugins.manage": true, "plugins.catalog.lookup": true, "reload.mcp": true, "mcp.servers.test": true,
		"mcp.servers.oauth.start": true, "process.list": true, "profiles.configure": true, "profiles.create": true,
		"profiles.describe": true, "profiles.get_asset": true, "profiles.list": true, "profiles.set_asset": true,
		"bot_relay.roster.sync": true, "bot_relay.outbox.drain": true, "bot_relay.deliver": true,
		"bot_relay.reply": true, "image.generate": true, "projects.discover_repos": true,
		"projects.record_repos": true, "projects.for_cwd": true, "projects.tree": true,
		"projects.project_sessions": true, "setup.runtime_check": true, "setup.status": true,
		"free_tier.provision": true, "voice.toggle": true, "voice.record": true, "voice.tts": true,
		"wake.start": true, "wake.status": true, "session.active_list": true, "session.branch": true,
		"session.compress": true, "session.list": true, "session.resume": true, "session.save": true,
		"session.workspace.move": true, "shell.exec": true, "skills.manage": true, "slash.exec": true,
		"onboarding.start": true, "onboarding.state": true, "subagent.roster": true, "command.dispatch": true,
		"shared_metrics.set": true,
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
