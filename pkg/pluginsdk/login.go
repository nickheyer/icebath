package pluginsdk

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Kinds of saved item a login backend may list
var vaultKinds = map[string]bool{"login": true, "payment": true, "address": true}

// Operations the host runs on a login backend through the login_backend request
var loginOps = map[string]bool{"list_items": true, "get_meta": true, "resolve_password": true, "resolve_otp": true,
	"resolve_secret": true, "fill_enter": true, "fill_exit": true}

// Metadata of one saved item a login backend lists, secrets excluded
type VaultItem struct {
	// Handle of the item, starting with the backend name and a colon
	ID string
	// One of login, payment or address
	Kind  string
	Label string
	// Site the item belongs to, empty for none
	Origin    string
	CreatedAt string
	// What the identifier is, such as username or email, empty for none
	IdentifierType string
	Identifier     string
	HasOTP         bool
	AllowedOrigins []string
}

// Browser vault backend resolving saved logins by handle, its handles all starting with the name and a colon
type LoginBackend struct {
	// Lowercase identifier the vault settings and every handle of the backend carry
	Name        string
	DisplayName string
	ListItems   func(ctx context.Context) ([]VaultItem, error)
	// Item of a handle, nil when the backend holds none
	GetMeta         func(ctx context.Context, handle string) (*VaultItem, error)
	ResolvePassword func(ctx context.Context, handle string) (string, error)
	// Current one time code of a login that stores a seed, ok false when it stores none
	ResolveOTP func(ctx context.Context, handle string) (code string, ok bool, err error)
	// Full payload of a payment or address item, the password alone when unset
	ResolveSecret func(ctx context.Context, handle string) (map[string]string, error)
	// Holds whatever one fill needs until the returned release runs
	Fill func(ctx context.Context) (release func() error, err error)
}

// Wire fields of one saved item, an empty optional text crossing as none
func vaultItemFields(item VaultItem) map[string]any {
	optional := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	origins := make([]any, len(item.AllowedOrigins))
	for i, origin := range item.AllowedOrigins {
		origins[i] = origin
	}
	return map[string]any{"id": item.ID, "kind": item.Kind, "label": item.Label, "origin": optional(item.Origin), "created_at": item.CreatedAt,
		"identifier_type": optional(item.IdentifierType), "identifier": optional(item.Identifier), "has_otp": item.HasOTP, "allowed_origins": origins}
}

// Provider object of a login backend, one method per vault operation the host runs
func loginBackendObject(b LoginBackend) *Object {
	prefix := b.Name + ":"
	display := b.DisplayName
	if display == "" {
		display = b.Name
	}
	handleOf := func(kwargs map[string]any) string { return text(kwargs["handle"]) }
	var fillMu sync.Mutex
	fills := map[int64]func() error{}
	var fillIDs int64
	methods := map[string]Func{
		"owns": func(_ context.Context, args []any, _ map[string]any) (any, error) {
			return strings.HasPrefix(argText(args, 0), prefix), nil
		},
		"is_unlocked": func(context.Context, []any, map[string]any) (any, error) { return true, nil },
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
				if b.ResolvePassword == nil {
					return nil, fmt.Errorf("login backend %s resolves no secrets", b.Name)
				}
				password, err := b.ResolvePassword(ctx, handleOf(kwargs))
				if err != nil {
					return nil, err
				}
				return map[string]any{"password": password}, nil
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
				return nil, &Error{Type: "LookupError", Message: fmt.Sprintf("login backend %s holds no fill %d", b.Name, id)}
			}
			return nil, release()
		},
	}
	return &Object{Type: "LoginBackend", Attrs: map[string]any{"name": b.Name, "prefix": prefix, "display_name": display, "needs_unlock": false},
		Methods: methods}
}

// Name, prefix and unlock need the host judges a login backend by before it runs any of its code, read off the object attributes
func loginIdentity(o *Object) map[string]any {
	name, okName := o.Attrs["name"].(string)
	prefix, okPrefix := o.Attrs["prefix"].(string)
	if !okName || !okPrefix {
		return map[string]any{"error": "login backend name and prefix must be plain string attributes"}
	}
	needsUnlock, _ := o.Attrs["needs_unlock"].(bool)
	return map[string]any{"name": name, "prefix": prefix, "needs_unlock": needsUnlock}
}

// Runs one vault operation on a login backend object and answers its value, or that the value has the wrong type
func (p *peer) loginBackend(ctx context.Context, params map[string]any) (any, error) {
	obj, err := p.requireObject(params["ref"])
	if err != nil {
		return nil, err
	}
	op := text(params["op"])
	if !loginOps[op] {
		return nil, &Error{Type: "ValueError", Message: fmt.Sprintf("unknown login backend operation %q", op)}
	}
	fn := obj.Methods[op]
	if fn == nil {
		return nil, &Error{Type: "AttributeError", Message: op}
	}
	kwargs := p.decodeMap(params)
	delete(kwargs, "ref")
	delete(kwargs, "op")
	value, err := fn(context.WithValue(ctx, selfKey{}, obj), []any{}, kwargs)
	if err != nil {
		return nil, err
	}
	plain := p.encode(value)
	if !loginValueFits(op, plain) {
		return map[string]any{"invalid": true}, nil
	}
	return map[string]any{"value": plain}, nil
}

// Whether the wire value of a vault operation has the type the vault takes
func loginValueFits(op string, value any) bool {
	switch op {
	case "list_items":
		items, ok := value.([]any)
		for _, item := range items {
			ok = ok && vaultItemFits(item)
		}
		return ok
	case "get_meta":
		return value == nil || vaultItemFits(value)
	case "resolve_password":
		_, ok := value.(string)
		return ok
	case "resolve_otp":
		_, ok := value.(string)
		return ok || value == nil
	case "resolve_secret":
		secret, ok := value.(map[string]any)
		for _, v := range secret {
			_, isText := v.(string)
			ok = ok && isText
		}
		return ok
	}
	return true
}

// Whether a wire value is the metadata of one saved item
func vaultItemFits(value any) bool {
	fields, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, name := range []string{"id", "kind", "label", "created_at"} {
		if _, isText := fields[name].(string); !isText {
			return false
		}
	}
	for _, name := range []string{"origin", "identifier", "identifier_type"} {
		if _, isText := fields[name].(string); !isText && fields[name] != nil {
			return false
		}
	}
	origins, isList := fields["allowed_origins"].([]any)
	for _, origin := range origins {
		_, isText := origin.(string)
		isList = isList && isText
	}
	_, isFlag := fields["has_otp"].(bool)
	return isList && isFlag && vaultKinds[text(fields["kind"])]
}

// Registers a login backend as the provider object the vault drives
func (h *host) RegisterLoginBackend(backend LoginBackend) (*Handle, error) {
	if backend.Name == "" {
		return nil, fmt.Errorf("login backend needs a name")
	}
	return h.RegisterProvider("login_backend", loginBackendObject(backend))
}
