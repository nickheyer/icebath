package browser

import (
	"fmt"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/vault"
)

// A plugin login backend failed or broke its contract, the fixed text never carrying the plugin's own message
type PluginBackendError struct{ Msg string }

func (e *PluginBackendError) Error() string { return e.Msg }

// Core owned wrapper around a plugin registered login backend, the only form the vault tool ever sees
type PluginLoginBackend struct {
	name, prefix, display string
	plugin                LoginBackend
}

// Wrapper whose namespace is the registry key, never an attribute the plugin can change later
func NewPluginLoginBackend(name string, plugin LoginBackend) *PluginLoginBackend {
	return &PluginLoginBackend{name: name, prefix: name + ":", display: name + " (plugin)", plugin: plugin}
}

func (b *PluginLoginBackend) Name() string        { return b.name }
func (b *PluginLoginBackend) DisplayName() string { return b.display }
func (b *PluginLoginBackend) Prefix() string      { return b.prefix }
func (b *PluginLoginBackend) NeedsUnlock() bool   { return false }
func (b *PluginLoginBackend) IsUnlocked() bool    { return true }

// Ownership decided here, so the plugin's own Owns is never consulted
func (b *PluginLoginBackend) Owns(handle string) bool { return strings.HasPrefix(handle, b.prefix) }

// Runs one plugin call, a returned error or panic replaced by the fixed failure text
func (b *PluginLoginBackend) call(fn func() (any, error)) (result any, err error) {
	failed := false
	func() {
		defer func() {
			if recover() != nil {
				failed = true
			}
		}()
		var callErr error
		result, callErr = fn()
		if callErr != nil {
			failed = true
		}
	}()
	if failed {
		return nil, &PluginBackendError{fmt.Sprintf("The %s login backend failed.", b.name)}
	}
	return result, nil
}

func (b *PluginLoginBackend) requireOwn(handle string) error {
	if !b.Owns(handle) {
		return &PluginBackendError{fmt.Sprintf("Handle is outside the %s login backend's namespace.", b.name)}
	}
	return nil
}

func (b *PluginLoginBackend) require(ok bool) error {
	if !ok {
		return &PluginBackendError{fmt.Sprintf("The %s login backend returned an invalid value.", b.name)}
	}
	return nil
}

// Exact text copy of an optional string field, false for any other value
func pluginText(v any) (any, bool) {
	if v == nil {
		return nil, true
	}
	if s, ok := v.(string); ok {
		return s, true
	}
	return nil, false
}

// Core owned copy of a plugin's item metadata, false when a field has the wrong type or kind
func materializeVaultMeta(meta vault.VaultItemMeta) (vault.VaultItemMeta, bool) {
	origin, okOrigin := pluginText(meta.Origin)
	identifier, okIdentifier := pluginText(meta.Identifier)
	identifierType, okIdentifierType := pluginText(meta.IdentifierType)
	if !okOrigin || !okIdentifier || !okIdentifierType || !slices.Contains(vault.VaultKinds, meta.Kind) {
		return vault.VaultItemMeta{}, false
	}
	return vault.VaultItemMeta{ID: meta.ID, Kind: meta.Kind, Label: meta.Label, Origin: origin, CreatedAt: meta.CreatedAt,
		IdentifierType: identifierType, Identifier: identifier, HasOTP: meta.HasOTP, AllowedOrigins: slices.Clone(meta.AllowedOrigins)}, true
}

// Validated copies of the plugin's items restricted to its own namespace
func (b *PluginLoginBackend) ListItems() ([]vault.VaultItemMeta, error) {
	result, err := b.call(func() (any, error) {
		metas, err := b.plugin.ListItems()
		if err != nil {
			return nil, err
		}
		copied := make([]vault.VaultItemMeta, 0, len(metas))
		valid := true
		for _, m := range metas {
			c, ok := materializeVaultMeta(m)
			valid = valid && ok
			copied = append(copied, c)
		}
		return [2]any{copied, valid}, nil
	})
	if err != nil {
		return nil, err
	}
	pair := result.([2]any)
	if err := b.require(pair[1].(bool)); err != nil {
		return nil, err
	}
	out := []vault.VaultItemMeta{}
	for _, m := range pair[0].([]vault.VaultItemMeta) {
		if b.Owns(m.ID) {
			out = append(out, m)
		}
	}
	return out, nil
}

// Validated copy of one item, nil outside the namespace or when the plugin reports another id
func (b *PluginLoginBackend) GetMeta(handle string) (*vault.VaultItemMeta, error) {
	if !b.Owns(handle) {
		return nil, nil
	}
	result, err := b.call(func() (any, error) {
		meta, err := b.plugin.GetMeta(handle)
		if err != nil {
			return nil, err
		}
		if meta == nil {
			return nil, nil
		}
		c, ok := materializeVaultMeta(*meta)
		return [2]any{c, ok}, nil
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	pair := result.([2]any)
	if err := b.require(pair[1].(bool)); err != nil {
		return nil, err
	}
	meta := pair[0].(vault.VaultItemMeta)
	if meta.ID != handle {
		return nil, nil
	}
	return &meta, nil
}

func (b *PluginLoginBackend) ResolvePassword(handle string) (string, error) {
	if err := b.requireOwn(handle); err != nil {
		return "", err
	}
	result, err := b.call(func() (any, error) { return b.plugin.ResolvePassword(handle) })
	if err != nil {
		return "", err
	}
	return result.(string), nil
}

func (b *PluginLoginBackend) ResolveOTP(handle string) (string, bool, error) {
	if err := b.requireOwn(handle); err != nil {
		return "", false, err
	}
	result, err := b.call(func() (any, error) {
		code, present, err := b.plugin.ResolveOTP(handle)
		if err != nil {
			return nil, err
		}
		return [2]any{code, present}, nil
	})
	if err != nil {
		return "", false, err
	}
	pair := result.([2]any)
	return pair[0].(string), pair[1].(bool), nil
}

// Copied payload of the plugin, a nil mapping reading as an invalid value
func (b *PluginLoginBackend) ResolveSecret(handle string) (map[string]string, error) {
	if err := b.requireOwn(handle); err != nil {
		return nil, err
	}
	result, err := b.call(func() (any, error) {
		value, err := b.plugin.ResolveSecret(handle)
		if err != nil {
			return nil, err
		}
		if value == nil {
			return nil, nil
		}
		copied := make(map[string]string, len(value))
		for k, v := range value {
			copied[k] = v
		}
		return copied, nil
	})
	if err != nil {
		return nil, err
	}
	if err := b.require(result != nil); err != nil {
		return nil, err
	}
	return result.(map[string]string), nil
}

// The plugin's own section around one of its fills, its exit guarded like every other plugin call
func (b *PluginLoginBackend) FillContext() (func() error, error) {
	result, err := b.call(func() (any, error) {
		release, err := b.plugin.FillContext()
		if err != nil {
			return nil, err
		}
		if release == nil {
			return func() error { return nil }, nil
		}
		return release, nil
	})
	if err != nil {
		return nil, err
	}
	release := result.(func() error)
	return func() error {
		_, err := b.call(func() (any, error) { return nil, release() })
		return err
	}, nil
}
