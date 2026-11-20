package browser

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/vault"
)

// Base reference of the objects register_login_backend receives from an in process plugin in the host
var hostedLoginBackendBase = plugins.HostInProcessObjectBases["register_login_backend"]

// Value of the wrong type a hosted backend returned, judged in the plugin host
var errHostedLoginInvalid = errors.New("login backend returned a value of the wrong type")

// Python login backend an in process plugin registered, living in the plugin host and called by reference
type hostedLoginBackend struct{ obj *plugins.HostedObject }

// Plugin host object handed to register_login_backend as a LoginBackend, any other value unchanged
func adaptHostedLoginBackend(p any) any {
	obj, ok := p.(*plugins.HostedObject)
	if !ok || obj == nil || obj.Base != hostedLoginBackendBase {
		return p
	}
	return hostedLoginBackend{obj: obj}
}

// Name, prefix and needs_unlock the child read from the class, or its fixed refusal
func (h hostedLoginBackend) identity() (name, prefix string, needsUnlock bool, refusal string) {
	judged := h.obj.Identity
	if message, ok := judged["error"].(string); ok {
		return "", "", false, message
	}
	name, okName := judged["name"].(string)
	prefix, okPrefix := judged["prefix"].(string)
	needsUnlock, okUnlock := judged["needs_unlock"].(bool)
	if !okName || !okPrefix || !okUnlock {
		return "", "", false, "login backend must subclass LoginBackend"
	}
	return name, prefix, needsUnlock, ""
}

func (h hostedLoginBackend) Name() string {
	name, _, _, _ := h.identity()
	return name
}

func (h hostedLoginBackend) Prefix() string {
	_, prefix, _, _ := h.identity()
	return prefix
}

func (h hostedLoginBackend) NeedsUnlock() bool {
	_, _, needsUnlock, _ := h.identity()
	return needsUnlock
}

// Attribute read in the host, raising like the plugin attribute does
func (h hostedLoginBackend) DisplayName() string {
	value, err := h.obj.Get(context.Background(), "display_name")
	if err != nil {
		panic(err)
	}
	return config.PyStr(value)
}

// Plugin's own ownership answer from the host, raising like the plugin method does
func (h hostedLoginBackend) Owns(handle string) bool {
	value, err := h.obj.Call(context.Background(), "owns", []any{handle}, nil)
	if err != nil {
		panic(err)
	}
	return config.PyTruthy(value)
}

// Plugin's own unlock state from the host, raising like the plugin method does
func (h hostedLoginBackend) IsUnlocked() bool {
	value, err := h.obj.Call(context.Background(), "is_unlocked", nil, nil)
	if err != nil {
		panic(err)
	}
	return config.PyTruthy(value)
}

// Runs one backend operation in the host, a wrong typed value reported as errHostedLoginInvalid
func (h hostedLoginBackend) call(op string, params map[string]any) (any, error) {
	raw, err := h.obj.LoginBackendCall(context.Background(), op, params)
	if err != nil {
		return nil, err
	}
	reply, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("login backend operation %s answered %T", op, raw)
	}
	if config.PyTruthy(reply["invalid"]) {
		return nil, errHostedLoginInvalid
	}
	return reply["value"], nil
}

// Enters the plugin's fill context in the host, the returned function leaving it
func (h hostedLoginBackend) FillContext() (func() error, error) {
	ref, err := h.call("fill_enter", nil)
	if err != nil {
		return nil, err
	}
	return func() error {
		_, err := h.call("fill_exit", map[string]any{"context": ref})
		return err
	}, nil
}

// Item metadata the host copied from one VaultItemMeta
func hostedVaultMeta(raw any) (vault.VaultItemMeta, error) {
	fields, ok := raw.(map[string]any)
	if !ok {
		return vault.VaultItemMeta{}, fmt.Errorf("login backend item answered %T", raw)
	}
	text := func(key string) string {
		s, _ := fields[key].(string)
		return s
	}
	meta := vault.VaultItemMeta{ID: text("id"), Kind: text("kind"), Label: text("label"), Origin: fields["origin"], CreatedAt: text("created_at"),
		IdentifierType: fields["identifier_type"], Identifier: fields["identifier"], AllowedOrigins: []string{}}
	meta.HasOTP, _ = fields["has_otp"].(bool)
	origins, _ := fields["allowed_origins"].([]any)
	for _, origin := range origins {
		s, _ := origin.(string)
		meta.AllowedOrigins = append(meta.AllowedOrigins, s)
	}
	return meta, nil
}

func (h hostedLoginBackend) ListItems() ([]vault.VaultItemMeta, error) {
	value, err := h.call("list_items", nil)
	if err != nil {
		return nil, err
	}
	raw, _ := value.([]any)
	items := make([]vault.VaultItemMeta, 0, len(raw))
	for _, item := range raw {
		meta, err := hostedVaultMeta(item)
		if err != nil {
			return nil, err
		}
		items = append(items, meta)
	}
	return items, nil
}

func (h hostedLoginBackend) GetMeta(handle string) (*vault.VaultItemMeta, error) {
	value, err := h.call("get_meta", map[string]any{"handle": handle})
	if err != nil || value == nil {
		return nil, err
	}
	meta, err := hostedVaultMeta(value)
	if err != nil {
		return nil, err
	}
	return &meta, nil
}

func (h hostedLoginBackend) ResolvePassword(handle string) (string, error) {
	value, err := h.call("resolve_password", map[string]any{"handle": handle})
	if err != nil {
		return "", err
	}
	password, _ := value.(string)
	return password, nil
}

func (h hostedLoginBackend) ResolveOTP(handle string) (string, bool, error) {
	value, err := h.call("resolve_otp", map[string]any{"handle": handle})
	if err != nil || value == nil {
		return "", false, err
	}
	code, _ := value.(string)
	return code, true, nil
}

func (h hostedLoginBackend) ResolveSecret(handle string) (map[string]string, error) {
	value, err := h.call("resolve_secret", map[string]any{"handle": handle})
	if err != nil {
		return nil, err
	}
	raw, _ := value.(map[string]any)
	secret := make(map[string]string, len(raw))
	for k, v := range raw {
		secret[k], _ = v.(string)
	}
	return secret, nil
}
