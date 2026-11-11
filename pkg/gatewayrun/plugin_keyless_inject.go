package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

const keylessWarnWindowS = 300.0

type keylessWarnKey struct{ pluginID, kind string }

var (
	keylessWarnedMu sync.Mutex
	keylessWarned   = map[keylessWarnKey]float64{}
)

// Warns once per plugin and refusal kind per window that a key less inject was dropped, debug on repeats
func keylessRefuse(pluginID, kind, detail string) {
	keylessRefuseAt(pythread.Monotonic(), pluginID, kind, detail)
}

// Refusal at a monotonic instant, marks older than the window dropped first
func keylessRefuseAt(now float64, pluginID, kind, detail string) {
	keylessWarnedMu.Lock()
	for key, at := range keylessWarned {
		if now-at >= keylessWarnWindowS {
			delete(keylessWarned, key)
		}
	}
	mark := keylessWarnKey{pluginID, kind}
	_, repeated := keylessWarned[mark]
	if !repeated {
		keylessWarned[mark] = now
	}
	keylessWarnedMu.Unlock()
	if repeated {
		runLog().Debug(fmt.Sprintf("inject_message from plugin %s refused again: %s", pluginID, detail))
		return
	}
	runLog().Warn(fmt.Sprintf("inject_message from plugin %s has no session key and %s; nothing was injected. Pass session_key= (or origin=) to choose the chat.", pluginID, detail))
}

// Whether a live session is a local session its profile's local adapter authorizes
func keylessLocal(r *GatewayRunner, live *gateway.LiveSession) (bool, error) {
	source := live.Source
	if source == nil || source.Platform != gateway.PlatformLocal {
		return false, nil
	}
	verdict, err := gateway.AuthorizeLocalSource(r, source)
	if err != nil {
		return false, err
	}
	return verdict != nil && *verdict, nil
}

// Whether a live local session was launched by the classic CLI, false when its frozen policy cannot be read
func keylessLaunchedByClassicCLI(ctx context.Context, r *GatewayRunner, live *gateway.LiveSession) (bool, error) {
	policy, err := gateway.LocalPolicyForSource(ctx, r, live.Source)
	if err != nil {
		var refusal *state.RuntimeStoreError
		if errors.As(err, &refusal) {
			return false, nil
		}
		return false, err
	}
	return policy != nil && policy.Source == "cli", nil
}

// Authority and live session a key less inject goes to, nil logged when nothing may receive it
func keylessResolve(ctx context.Context, r *GatewayRunner, pluginHome, callingSessionKey, pluginID string) (*gateway.SessionAuthority, *gateway.LiveSession, error) {
	var authority *gateway.SessionAuthority
	if pluginHome != "" {
		found, err := gateway.AuthorityForHome(r, pluginHome)
		if err != nil {
			return nil, nil, err
		}
		authority = found
	}
	if authority == nil {
		keylessRefuse(pluginID, "unserved", fmt.Sprintf("its profile home %s is not served by this gateway", homeOrNone(pluginHome)))
		return nil, nil, nil
	}
	sessions := authority.LiveSessionViews()
	if callingSessionKey != "" {
		for _, s := range sessions {
			if s.Live.Route != callingSessionKey {
				continue
			}
			local, err := keylessLocal(r, s.Live)
			if err != nil {
				return nil, nil, err
			}
			if local {
				return authority, s.Live, nil
			}
		}
		for _, other := range gateway.AllAuthorities(r) {
			if other == nil || other == authority {
				continue
			}
			for _, s := range other.LiveSessionViews() {
				if s.Live.Route == callingSessionKey && s.Live.Source != nil && s.Live.Source.Platform == gateway.PlatformLocal {
					runLog().Warn(fmt.Sprintf("Plugin message injection refused: plugin=%s session=%s is not a live local session of the plugin's profile home %s",
						pluginID, callingSessionKey, homeOrNone(pluginHome)))
					return nil, nil, nil
				}
			}
		}
		runLog().Warn("inject_message: gateway mode requires an existing session_key or an origin")
		return nil, nil, nil
	}
	var attached []*gateway.LiveSession
	for _, s := range sessions {
		if !s.Subscribed {
			continue
		}
		local, err := keylessLocal(r, s.Live)
		if err != nil {
			return nil, nil, err
		}
		if !local {
			continue
		}
		cli, err := keylessLaunchedByClassicCLI(ctx, r, s.Live)
		if err != nil {
			return nil, nil, err
		}
		if cli {
			attached = append(attached, s.Live)
		}
	}
	switch len(attached) {
	case 1:
		return authority, attached[0], nil
	case 0:
		keylessRefuse(pluginID, "none", "no classic CLI session is attached in its profile")
	default:
		keylessRefuse(pluginID, "several", fmt.Sprintf("%d classic CLI sessions are attached in its profile", len(attached)))
	}
	return nil, nil, nil
}

// Commits the key less text to the resolved session's FIFO under the plugin profile whose authority resolved it
func keylessAdmit(ctx context.Context, r *GatewayRunner, authority *gateway.SessionAuthority, live *gateway.LiveSession, content, pluginID string) (bool, error) {
	if !r.pluginInjectionAccepting() {
		return false, nil
	}
	entry := r.asyncSessionStore.Store().LookupBySessionKey(ctx, live.Route)
	if entry == nil || !r.pluginInjectionAccepting() {
		return false, nil
	}
	scoped := ctx
	if r.multiplexOn() {
		scoped = gateway.ProfileRuntimeScope(ctx, authority.ProfileID, true)
	}
	active, err := gateway.ActiveAuthority(scoped, r)
	if err != nil || active != authority {
		return false, err
	}
	adapter, err := r.deliveryAdapterFor(scoped, live.Source)
	if err != nil {
		return false, err
	}
	admitted, err := r.admitPluginInjection(scoped, adapter, live.Source, live.Route, entry.SessionID, content, pluginID)
	return admitted != nil && *admitted, err
}
