package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	warnedUnavailableProviders     = map[[3]string]bool{}
	warnedUnavailableProvidersMu   sync.Mutex
	recoverMemoryProviderAtStartup = memory.RecoverMemoryProviderAtStartup
)

// Once key per profile home, provider and kind of trouble
func unavailableWarningKey(ctx context.Context, name, kind string) [3]string {
	return [3]string{home.Key(home.Dir(ctx)), name, kind}
}

func memoryProviderWarned(key [3]string) bool {
	warnedUnavailableProvidersMu.Lock()
	defer warnedUnavailableProvidersMu.Unlock()
	return warnedUnavailableProviders[key]
}

// Logs the message and hands it to the user sink once per home, provider and kind
func warnOnce(ctx context.Context, name, message string, say func(string), kind string) {
	key := unavailableWarningKey(ctx, name, kind)
	warnedUnavailableProvidersMu.Lock()
	seen := warnedUnavailableProviders[key]
	warnedUnavailableProviders[key] = true
	warnedUnavailableProvidersMu.Unlock()
	if seen {
		return
	}
	slog.Warn(message)
	if say != nil {
		say(message)
	}
}

// Warns once per home and provider that a selected memory provider reports unavailable, with its own hint
func warnMemoryProviderUnavailable(ctx context.Context, name, reason string, say func(string)) {
	suffix := ""
	if reason != "" {
		suffix = " " + reason
	}
	warnOnce(ctx, name, fmt.Sprintf("⚠ Memory provider %s is selected but reports unavailable — external memory "+
		"is disabled for this session (built-in memory still works). Check the "+
		"provider's credentials/config with 'icebath memory status'. Note: "+
		"systemd/gateway services do not inherit ~/.icebath/.env automatically; set "+
		"any required variables in the service environment.%s", pyrepr.StrRepr(name), suffix), say, "unavailable")
}

// Configured provider ready to add, or nil after telling the user once per home why it will not run
func resolveMemoryProvider(ctx context.Context, env *tools.Env, name string, say func(string)) (memory.Provider, error) {
	p := memory.LoadMemoryProvider(ctx, env, name)
	if p == nil {
		if parked, ok := memory.ParkedProviderNotice(ctx, env, name); ok && parked != "" {
			warnOnce(ctx, name, parked, say, "parked")
			return nil, nil
		}
		if memory.FindProviderDir(ctx, env, name) == "" {
			recovered, err := recoverMemoryProviderAtStartup(ctx, name, say)
			if err != nil {
				return nil, err
			}
			if recovered {
				p = memory.LoadMemoryProvider(ctx, env, name)
			}
		}
		if p == nil && memory.FindProviderDir(ctx, env, name) != "" {
			warnOnce(ctx, name, fmt.Sprintf("⚠ Memory provider '%s' is installed but failed to load, so external memory is off "+
				"for this session (built-in memory still works). The log names the error; "+
				"`icebath memory status` shows the plugin's state.", name), say, "load")
		}
	}
	if p == nil || p.Available(ctx) {
		return p, nil
	}
	if !memoryProviderWarned(unavailableWarningKey(ctx, name, "unavailable")) {
		warnMemoryProviderUnavailable(ctx, name, providerUnavailableReason(ctx, p, name), say)
	}
	return nil, nil
}

// Provider hint for the unavailable warning, empty when the provider itself fails
func providerUnavailableReason(ctx context.Context, p memory.Provider, name string) (reason string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("unavailable_reason() failed for %s", name), "panic", r)
			reason = ""
		}
	}()
	return p.UnavailableReason(ctx)
}
