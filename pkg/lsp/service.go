package lsp

import (
	"fmt"
	"log/slog"
	"sync"
)

var (
	registryMu sync.Mutex
	services   = map[string]*Service{}
)

// ServiceFor returns the active service of a profile home, building it once on first use, nil when disabled
func ServiceFor(homeKey string, build func() (*Service, error)) (*Service, error) {
	registryMu.Lock()
	svc, ok := services[homeKey]
	if !ok {
		var err error
		if svc, err = build(); err != nil {
			registryMu.Unlock()
			return nil, err
		}
		services[homeKey] = svc
	}
	registryMu.Unlock()
	if svc == nil || !svc.IsActive() {
		return nil, nil
	}
	return svc, nil
}

// Every service started so far
func startedServices() []*Service {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]*Service, 0, len(services))
	for _, svc := range services {
		if svc != nil {
			out = append(out, svc)
		}
	}
	return out
}

// ReleaseWorkspace shuts down the clients serving a worktree about to be removed in every started service
func ReleaseWorkspace(path string) int {
	released := 0
	for _, svc := range startedServices() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					svc.rt.logf(slog.LevelDebug, "agent.lsp", "LSP workspace release failed for %s: %s", path, fmt.Sprint(r))
				}
			}()
			n, err := svc.ReleaseWorkspace(path)
			if err != nil {
				svc.rt.logf(slog.LevelDebug, "agent.lsp", "LSP workspace release failed for %s: %s", path, errText(err))
				return
			}
			released += n
		}()
	}
	return released
}

// ShutdownServices tears down every started service, idempotent, for the process exit path
func ShutdownServices() {
	registryMu.Lock()
	all := make([]*Service, 0, len(services))
	for _, svc := range services {
		all = append(all, svc)
	}
	services = map[string]*Service{}
	registryMu.Unlock()
	for _, svc := range all {
		if svc == nil {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					svc.rt.logf(slog.LevelDebug, "agent.lsp", "LSP shutdown error: %s", fmt.Sprint(r))
				}
			}()
			svc.Shutdown()
		}()
	}
}

// AtExitShutdown runs ShutdownServices for an exit hook, logging instead of propagating any failure
func AtExitShutdown() {
	defer func() {
		if r := recover(); r != nil {
			slog.Default().Debug(fmt.Sprintf("atexit LSP shutdown failed: %s", fmt.Sprint(r)), "logger", "agent.lsp")
		}
	}()
	shutdownServicesHook()
}

// Shutdown entry the exit hook runs, swapped by tests
var shutdownServicesHook = ShutdownServices
