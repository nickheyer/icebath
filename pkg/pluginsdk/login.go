package pluginsdk

import (
	"context"
	"fmt"
	"sync"
)

// Wire fields of one saved login item
func vaultItemFields(item VaultItem) map[string]any {
	origins := item.AllowedOrigins
	if origins == nil {
		origins = []string{}
	}
	return map[string]any{"id": item.ID, "kind": item.Kind, "label": item.Label, "origin": item.Origin, "created_at": item.CreatedAt,
		"identifier_type": item.IdentifierType, "identifier": item.Identifier, "has_otp": item.HasOTP, "allowed_origins": origins}
}

// Provider object of a login backend, one method per vault operation the host runs through login_backend
func loginBackendObject(b LoginBackend) *Object {
	display := b.DisplayName
	if display == "" {
		display = b.Name
	}
	handleOf := func(kwargs map[string]any) string { return text(kwargs["handle"]) }
	var fillMu sync.Mutex
	fills := map[int64]func() error{}
	var fillIDs int64
	methods := map[string]Func{
		"owns": func(ctx context.Context, args []any, _ map[string]any) (any, error) {
			handle := ""
			if len(args) > 0 {
				handle = text(args[0])
			}
			if b.Owns != nil {
				return b.Owns(ctx, handle), nil
			}
			return len(handle) >= len(b.Prefix) && handle[:len(b.Prefix)] == b.Prefix, nil
		},
		"is_unlocked": func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
			if b.IsUnlocked == nil {
				return !b.NeedsUnlock, nil
			}
			return b.IsUnlocked(ctx), nil
		},
		"list_items": func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
			if b.ListItems == nil {
				return []any{}, nil
			}
			items, err := b.ListItems(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]any, len(items))
			for i, item := range items {
				out[i] = vaultItemFields(item)
			}
			return out, nil
		},
		"get_meta": func(ctx context.Context, _ []any, kwargs map[string]any) (any, error) {
			if b.GetMeta == nil {
				return nil, nil
			}
			item, err := b.GetMeta(ctx, handleOf(kwargs))
			if err != nil || item == nil {
				return nil, err
			}
			return vaultItemFields(*item), nil
		},
		"resolve_password": func(ctx context.Context, _ []any, kwargs map[string]any) (any, error) {
			if b.ResolvePassword == nil {
				return nil, fmt.Errorf("login backend %s resolves no passwords", b.Name)
			}
			return b.ResolvePassword(ctx, handleOf(kwargs))
		},
		"resolve_otp": func(ctx context.Context, _ []any, kwargs map[string]any) (any, error) {
			if b.ResolveOTP == nil {
				return nil, nil
			}
			code, ok, err := b.ResolveOTP(ctx, handleOf(kwargs))
			if err != nil || !ok {
				return nil, err
			}
			return code, nil
		},
		"resolve_secret": func(ctx context.Context, _ []any, kwargs map[string]any) (any, error) {
			if b.ResolveSecret == nil {
				return map[string]any{}, nil
			}
			secret, err := b.ResolveSecret(ctx, handleOf(kwargs))
			if err != nil {
				return nil, err
			}
			out := make(map[string]any, len(secret))
			for k, v := range secret {
				out[k] = v
			}
			return out, nil
		},
		"fill_enter": func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
			release := func() error { return nil }
			if b.Fill != nil {
				held, err := b.Fill(ctx)
				if err != nil {
					return nil, err
				}
				if held != nil {
					release = held
				}
			}
			fillMu.Lock()
			fillIDs++
			id := fillIDs
			fills[id] = release
			fillMu.Unlock()
			return id, nil
		},
		"fill_exit": func(_ context.Context, _ []any, kwargs map[string]any) (any, error) {
			id, _ := integer(kwargs["context"])
			fillMu.Lock()
			release := fills[id]
			delete(fills, id)
			fillMu.Unlock()
			if release == nil {
				return nil, fmt.Errorf("login backend %s holds no fill %d", b.Name, id)
			}
			return nil, release()
		},
	}
	return &Object{Type: "LoginBackend", Attrs: map[string]any{"name": b.Name, "prefix": b.Prefix, "display_name": display,
		"needs_unlock": b.NeedsUnlock}, Methods: methods,
		identity: map[string]any{"name": b.Name, "prefix": b.Prefix, "needs_unlock": b.NeedsUnlock}}
}
