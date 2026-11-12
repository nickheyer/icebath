package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/vault"
)

// Error code of every vault failure, validation and store errors alike
const vaultErr = 5095

var vaultRegistry = &handlerRegistry{}

func init() {
	vaultRegistry.scopedMethod("vault.list", vaultList)
	vaultRegistry.scopedMethod("vault.sources", vaultSources)
	vaultRegistry.scopedMethod("vault.source.set", vaultSourceSet)
	vaultRegistry.scopedMethod("vault.unlock", vaultUnlock)
	vaultRegistry.scopedMethod("vault.lock", vaultLock)
	vaultRegistry.scopedMethod("vault.add", vaultAdd)
	vaultRegistry.scopedMethod("vault.remove", vaultRemove)
	vaultRegistry.install()
}

// Answers a raised vault failure with its message under the vault code, a config outage passing through
func vaultFailure(rid any, response *map[string]any) {
	failure := recover()
	if failure == nil {
		return
	}
	if configOutage(failure) {
		panic(failure)
	}
	*response = rpcErr(rid, vaultErr, failureText(failure), nil)
}

// Metadata of every item across the enabled backends a locked manager contributes nothing to
func vaultList(ctx context.Context, rid any, _ map[string]any) (response map[string]any) {
	defer vaultFailure(rid, &response)
	backends, err := browser.EnabledLoginBackends(ctx)
	if err != nil {
		return rpcErr(rid, vaultErr, err.Error(), nil)
	}
	items := []any{}
	for _, backend := range backends {
		if backend.NeedsUnlock() && !backend.IsUnlocked() {
			continue
		}
		metas, err := backend.ListItems()
		if err != nil {
			return rpcErr(rid, vaultErr, err.Error(), nil)
		}
		for _, meta := range metas {
			row := meta.ToDict().Copy()
			row.Set("backend", backend.Name())
			items = append(items, row)
		}
	}
	return ok(rid, map[string]any{"items": pyjson.Plain(items)})
}

// Status of every login source, the local vault first then each external manager
func vaultSources(ctx context.Context, rid any, _ map[string]any) map[string]any {
	backends, err := browser.EnabledLoginBackends(ctx)
	if err != nil {
		panic(err)
	}
	enabled := map[string]browser.LoginBackend{}
	for _, backend := range backends {
		enabled[backend.Name()] = backend
	}
	rows := []any{map[string]any{"name": "local", "display_name": "Icebath vault", "enabled": true, "needs_unlock": false,
		"unlocked": true, "installed": true}}
	for _, cls := range browser.ExternalLoginBackendClasses() {
		live, isLive := enabled[cls[0]]
		installed, err := browser.IsLoginBackendInstalled(ctx, cls[0])
		if err != nil {
			panic(err)
		}
		rows = append(rows, map[string]any{"name": cls[0], "display_name": cls[1], "enabled": isLive, "needs_unlock": true,
			"unlocked": isLive && live.IsUnlocked(), "installed": installed})
	}
	return ok(rid, map[string]any{"sources": rows})
}

// Enables or disables an external manager in config, locking it when disabled
func vaultSourceSet(ctx context.Context, rid any, params map[string]any) map[string]any {
	name := relayString(params["name"])
	known := false
	for _, cls := range browser.ExternalLoginBackendClasses() {
		known = known || cls[0] == name
	}
	if !known {
		return rpcErr(rid, vaultErr, fmt.Sprintf("unknown vault source: %s", name), nil)
	}
	enabled := truthy(params["enabled"])
	h := home.Dir(ctx)
	cfg, err := config.LoadContext(ctx, h, nil)
	if err != nil {
		panic(err)
	}
	section := ensureDict(ensureDict(cfg, "vault"), name)
	if enabled {
		delete(section, "enabled")
	} else {
		section["enabled"] = false
		browser.LockVault(ctx, &name)
	}
	if err := config.SaveConfig(ctx, h, cfg, config.SaveOptions{}); err != nil {
		panic(err)
	}
	return ok(rid, map[string]any{"name": name, "enabled": enabled})
}

// Child mapping of parent at key, a missing or non mapping value replaced by an empty one
func ensureDict(parent map[string]any, key string) map[string]any {
	child, isDict := parent[key].(map[string]any)
	if !isDict {
		child = map[string]any{}
		parent[key] = child
	}
	return child
}

// Unlocks an enabled manager with the master password typed in Settings, never echoing it back
func vaultUnlock(ctx context.Context, rid any, params map[string]any) map[string]any {
	name := relayString(params["name"])
	password := relayString(params["password"])
	backends, err := browser.EnabledLoginBackends(ctx)
	if err != nil {
		panic(err)
	}
	var target interface {
		Unlock(masterPassword string) error
	}
	for _, backend := range backends {
		if backend.Name() == name && backend.NeedsUnlock() {
			unlocker, canUnlock := backend.(interface {
				Unlock(masterPassword string) error
			})
			if !canUnlock {
				panic(fmt.Errorf("'%s' object has no attribute 'unlock'", backend.Name()))
			}
			target = unlocker
			break
		}
	}
	if target == nil {
		return rpcErr(rid, vaultErr, fmt.Sprintf("%s is not an enabled password manager", name), nil)
	}
	if password == "" {
		return rpcErr(rid, vaultErr, "master password is required", nil)
	}
	if err := func() (err error) {
		defer func() {
			if failure := recover(); failure != nil {
				if configOutage(failure) {
					panic(failure)
				}
				err = errors.New(failureText(failure))
			}
		}()
		return target.Unlock(password)
	}(); err != nil {
		return rpcErr(rid, vaultErr, strings.ReplaceAll(err.Error(), password, "[REDACTED]"), nil)
	}
	return ok(rid, map[string]any{"name": name, "unlocked": true})
}

// Forgets one manager's session token, or every one when no name is given
func vaultLock(ctx context.Context, rid any, params map[string]any) map[string]any {
	var name *string
	if truthy(params["name"]) {
		text := relayString(params["name"])
		name = &text
	}
	browser.LockVault(ctx, name)
	return ok(rid, map[string]any{"locked": true})
}

// Adds one item to the encrypted store, any error text scrubbed of the secret's values
func vaultAdd(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	raw, isDict := params["secret"].(map[string]any)
	if !isDict || len(raw) == 0 {
		return rpcErr(rid, vaultErr, "secret payload is required", nil)
	}
	secret := objectOf(raw)
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			response = rpcErr(rid, vaultErr, vault.ScrubSecretFromText(failureText(failure), secret), nil)
		}
	}()
	origin := ""
	if truthy(params["origin"]) {
		origin = relayString(params["origin"])
	}
	meta, err := vault.GetVaultStore(ctx).AddItem(relayString(params["kind"]), relayString(params["label"]), secret, origin)
	if err != nil {
		return rpcErr(rid, vaultErr, vault.ScrubSecretFromText(err.Error(), secret), nil)
	}
	return ok(rid, map[string]any{"id": meta.ID})
}

// Removes one vault item by id
func vaultRemove(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	defer vaultFailure(rid, &response)
	itemID := relayString(params["id"])
	if itemID == "" {
		return rpcErr(rid, vaultErr, "id is required", nil)
	}
	removed, err := vault.GetVaultStore(ctx).RemoveItem(itemID)
	if err != nil {
		return rpcErr(rid, vaultErr, err.Error(), nil)
	}
	return ok(rid, map[string]any{"removed": removed})
}
