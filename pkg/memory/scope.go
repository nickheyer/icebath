package memory

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"unsafe"
	"weak"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Check code logged when a memory call names no profile or the wrong one
const CallerProfileRefusal = "memory_caller_profile_unresolved"

// Check code logged when one provider instance is offered to a second profile
const SharedInstanceRefusal = "memory_provider_instance_shared"

// What the model sees for a refused memory tool call, without check names or home paths
func RefusedToolMessage(toolName string) string {
	return fmt.Sprintf("Memory tool '%s' refused. Ask the operator to check `icebath logs errors`.", toolName)
}

// Stable key of a home, empty when no home is given
func homeKey(h string) string {
	if h == "" {
		return ""
	}
	return home.Key(h)
}

// Profile the caller context names, its home override else the home its secret scope was built for
func explicitCallerHome(ctx context.Context) string {
	if h := home.Override(ctx); h != "" {
		return h
	}
	return provider.CurrentSecretScopeHome(ctx)
}

// Python repr of a home for refusal logs, None when absent
func homeRepr(h string) string {
	if h == "" {
		return "None"
	}
	return pyRepr(h)
}

func refuse(label, reason, callerHome, boundHome string, multiplexed bool) {
	slog.Default().Warn(fmt.Sprintf("Memory %s refused (%s): %s; caller profile home %s, provider bound to %s, multiplexed=%s",
		label, CallerProfileRefusal, reason, homeRepr(callerHome), homeRepr(boundHome), pyStr(multiplexed)))
}

// BoundScope is the profile a memory manager was initialized for, its home key resolved once
type BoundScope struct {
	Home          string
	AgentIdentity string
	HomeKey       string
}

// Records the bound home and identity with the home key resolved up front
func NewBoundScope(icebathHome, agentIdentity string) *BoundScope {
	return &BoundScope{Home: icebathHome, AgentIdentity: agentIdentity, HomeKey: homeKey(icebathHome)}
}

// ProviderScope is the profile a memory provider call runs for
type ProviderScope struct {
	Home          string
	AgentIdentity string
}

// Provider scope naming the calling profile, false when the call or hook must be refused
func CallerScope(ctx context.Context, bound *BoundScope, label string) (ProviderScope, bool) {
	if bound == nil {
		bound = &BoundScope{}
	}
	boundHome := bound.Home
	multiplexed := provider.IsMultiplexActive(ctx)
	callerHome := explicitCallerHome(ctx)
	if callerHome == "" {
		if multiplexed {
			refuse(label, "the caller names no profile on a process serving several", "", boundHome, multiplexed)
			return ProviderScope{}, false
		}
		callerHome = home.Dir(ctx)
	}
	if boundHome == "" {
		if multiplexed {
			refuse(label, "the memory manager recorded no profile", callerHome, "", multiplexed)
			return ProviderScope{}, false
		}
		return ProviderScope{Home: callerHome}, true
	}
	callerKey := homeKey(callerHome)
	boundKey := bound.HomeKey
	if boundKey == "" {
		boundKey = homeKey(boundHome)
	}
	if callerKey == "" || callerKey != boundKey {
		refuse(label, "the caller's profile is not the one its provider was initialized for", callerHome, boundHome, multiplexed)
		return ProviderScope{}, false
	}
	return ProviderScope{Home: boundHome, AgentIdentity: bound.AgentIdentity}, true
}

// One provider instance claim, a weak reference to the instance and the profile home key it serves
type instanceClaim struct {
	ref weak.Pointer[byte]
	key string
}

var (
	claimsMu sync.Mutex
	claims   = map[uintptr]instanceClaim{}
)

// Address of a pointer shaped provider instance, false when it cannot be weakly tracked
func instanceAddress(p any) (*byte, bool) {
	rv := reflect.ValueOf(p)
	if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() {
		return nil, false
	}
	return (*byte)(rv.UnsafePointer()), true
}

// Drops claims whose instance has been collected, so a reused address starts unclaimed
func sweepDeadClaims() {
	for addr, c := range claims {
		if c.ref.Value() == nil {
			delete(claims, addr)
		}
	}
}

// Provider name for refusal logs, a question mark when it has none
func providerName(p any) string {
	if named, ok := p.(interface{ Name() string }); ok {
		return named.Name()
	}
	return "?"
}

// Binds the instance to a home, refused on a multiplexed process when another profile holds it or it is untrackable
func ClaimInstance(ctx context.Context, p any, h string) bool {
	key := homeKey(h)
	if key == "" {
		key = h
	}
	multiplexed := provider.IsMultiplexActive(ctx)
	ptr, trackable := instanceAddress(p)
	claimsMu.Lock()
	defer claimsMu.Unlock()
	sweepDeadClaims()
	owner, owned := "", false
	if trackable {
		if c, ok := claims[uintptr(unsafe.Pointer(ptr))]; ok && c.ref.Value() == ptr {
			owner, owned = c.key, true
		}
	}
	if owned && owner != key && multiplexed {
		slog.Default().Warn(fmt.Sprintf("Memory provider '%s' refused for profile home %s (%s): this same provider instance "+
			"is already bound to %s; a provider must give every agent its own instance",
			providerName(p), pyRepr(h), SharedInstanceRefusal, pyRepr(owner)))
		return false
	}
	if !trackable {
		if multiplexed {
			slog.Default().Warn(fmt.Sprintf("Memory provider '%s' refused for profile home %s (%s): the instance can't be "+
				"tracked (no weak reference support), so sharing it can't be ruled out",
				providerName(p), pyRepr(h), SharedInstanceRefusal))
			return false
		}
		return true
	}
	claims[uintptr(unsafe.Pointer(ptr))] = instanceClaim{ref: weak.Make(ptr), key: key}
	return true
}

// Runs fn under the owning profile home override, set explicitly so a background worker never relies on the submitter
func InOwnerHome[T any](bound *BoundScope, fn func(ctx context.Context) T) func(ctx context.Context) T {
	if bound == nil || bound.Home == "" {
		return fn
	}
	h := bound.Home
	return func(ctx context.Context) T {
		return fn(home.WithOverride(ctx, h))
	}
}

// Memory caller scope registers nothing at import
func registerMemoryCallerScope() {}
