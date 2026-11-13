package nemorelay

import (
	"context"

	"github.com/google/uuid"
)

// Registers a global event subscriber under a unique name
func RegisterSubscriber(name string, fn EventSubscriberFn) error {
	return writeGlobal(func(state *runtimeState) error {
		if _, exists := state.eventSubscribers[name]; exists {
			return alreadyExistsError(name + " subscriber already exists")
		}
		state.eventSubscribers[name] = fn
		return nil
	})
}

// Removes a global event subscriber and reports whether it existed
func DeregisterSubscriber(name string) (bool, error) {
	return writeGlobal(func(state *runtimeState) bool {
		_, existed := state.eventSubscribers[name]
		delete(state.eventSubscribers, name)
		return existed
	}), nil
}

func addScopeSubscriber(local *scopeLocalRegistries, name string, fn EventSubscriberFn) error {
	if _, exists := local.eventSubscribers[name]; exists {
		return alreadyExistsError(name + " subscriber already exists")
	}
	local.eventSubscribers[name] = fn
	return nil
}

// Registers a subscriber that only sees events emitted while the scope is open
func ScopeRegisterSubscriber(ctx context.Context, scopeUUID uuid.UUID, name string, fn EventSubscriberFn) error {
	return CurrentScopeStack(ctx).withLocalRegistries(scopeUUID, func(local *scopeLocalRegistries) error {
		return addScopeSubscriber(local, name, fn)
	})
}

// Registers a scope subscriber without waiting when the scope stack is busy
func TryScopeRegisterSubscriber(ctx context.Context, scopeUUID uuid.UUID, name string, fn EventSubscriberFn) error {
	return CurrentScopeStack(ctx).tryWithLocalRegistries(scopeUUID, func(local *scopeLocalRegistries) error {
		return addScopeSubscriber(local, name, fn)
	})
}

// Removes a scope subscriber and reports whether it existed
func ScopeDeregisterSubscriber(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	removed := false
	err := CurrentScopeStack(ctx).withLocalRegistries(scopeUUID, func(local *scopeLocalRegistries) error {
		_, removed = local.eventSubscribers[name]
		delete(local.eventSubscribers, name)
		return nil
	})
	return removed, err
}

// Removes a scope subscriber without waiting when the scope stack is busy
func TryScopeDeregisterSubscriber(ctx context.Context, scopeUUID uuid.UUID, name string) (bool, error) {
	removed := false
	err := CurrentScopeStack(ctx).tryWithLocalRegistries(scopeUUID, func(local *scopeLocalRegistries) error {
		_, removed = local.eventSubscribers[name]
		delete(local.eventSubscribers, name)
		return nil
	})
	return removed, err
}
