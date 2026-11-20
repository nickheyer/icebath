package tools

import (
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/linktier"
)

// Terminal backends of the plugins tier, every one but local
var pluginTierBackends = map[string]bool{"docker": true, "modal": true, "daytona": true, "vercel_sandbox": true, "ssh": true, "singularity": true}

// TerminalBackendRefusal is the line naming the build that carries a backend this binary lacks, false for a present or unknown name
func TerminalBackendRefusal(name string) (string, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !pluginTierBackends[name] || GetTerminalBackend(name) != nil {
		return "", false
	}
	return linktier.Missing("the "+name+" terminal backend", linktier.Plugins), true
}

// LinkedTerminalBuiltins lists the built in backend names this binary carries, in menu order
func LinkedTerminalBuiltins() []string {
	var names []string
	for _, n := range strings.Split(builtinBackendList, ", ") {
		if builtinBackend(n) != nil {
			names = append(names, n)
		}
	}
	return names
}

// Built in backend names this binary carries as one comma separated line
func linkedBuiltinBackendList() string { return strings.Join(LinkedTerminalBuiltins(), ", ") }

// Scratch sweeps the linked backends run when the process exits
var backendExitSweeps struct {
	sync.Mutex
	fns []func()
}

// Adds a sweep run at process exit
func addBackendExitSweep(fn func()) {
	backendExitSweeps.Lock()
	backendExitSweeps.fns = append(backendExitSweeps.fns, fn)
	backendExitSweeps.Unlock()
}

// SweepBackendScratchAtExit removes the orphaned scratch entries every linked backend left in this process
func SweepBackendScratchAtExit() {
	backendExitSweeps.Lock()
	fns := append([]func(){}, backendExitSweeps.fns...)
	backendExitSweeps.Unlock()
	for _, fn := range fns {
		fn()
	}
}
