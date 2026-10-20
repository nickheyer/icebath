// Named endpoint configurations and the credentials that turn them into providers
package profiles

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"sync"

	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/provider"
)

// How a credential travels on the request
type AuthKind string

const (
	AuthNone   AuthKind = "none"
	AuthBearer AuthKind = "bearer"
	AuthHeader AuthKind = "header"
	AuthQuery  AuthKind = "query"
)

// Builds the credential carrier for an auth kind the generic layer does not know
type AuthBuilder func(p Profile, key string) (provider.Auth, error)

// Declarative description of one endpoint and where its credential comes from
type Profile struct {
	Name string `json:"name"`
	// Family of the endpoint, naming the registered vendor whose conventions apply
	Vendor   string      `json:"vendor,omitempty"`
	Protocol protocol.ID `json:"protocol"`
	BaseURL  string      `json:"base_url"`
	Auth     AuthKind    `json:"auth,omitempty"`
	// Header or query parameter name carrying the credential
	AuthName string `json:"auth_name,omitempty"`
	// Where the credential comes from
	Key Source `json:"key,omitempty"`
	// Static headers the endpoint expects on every request
	Header http.Header `json:"header,omitempty"`
	// Static query parameters the endpoint expects on every request
	Query url.Values `json:"query,omitempty"`
	// Model ids the endpoint is known to serve, the first being the default
	Models []string `json:"models,omitempty"`
	// Request fields merged into every turn sent through the endpoint
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
	// Settings read by registered auth builders and vendors
	Options map[string]string `json:"options,omitempty"`
}

// Default model id, the first listed, or empty
func (p Profile) Model() string {
	if len(p.Models) > 0 {
		return p.Models[0]
	}
	return ""
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Checks that the profile names everything a build will need
func (p Profile) Validate() error {
	if !namePattern.MatchString(p.Name) {
		return fmt.Errorf("profile %q: invalid name", p.Name)
	}
	if p.Protocol == "" {
		return fmt.Errorf("profile %s: protocol is required", p.Name)
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("profile %s: invalid base url %q", p.Name, p.BaseURL)
	}
	switch p.Auth {
	case AuthNone, "", AuthBearer:
	case AuthHeader, AuthQuery:
		if p.AuthName == "" {
			return fmt.Errorf("profile %s: auth kind %s needs a name", p.Name, p.Auth)
		}
	default:
		if _, ok := lookupAuth(p.Auth); !ok {
			return fmt.Errorf("profile %s: unknown auth kind %q", p.Name, p.Auth)
		}
	}
	return p.Key.validate(p.Name)
}

// Reports whether the generic auth kinds need a credential value
func (p Profile) needsKey() bool {
	return p.Auth == AuthBearer || p.Auth == AuthHeader || p.Auth == AuthQuery
}

// Builds the credential carrier for key
func (p Profile) auth(key string) (provider.Auth, error) {
	if p.needsKey() && key == "" {
		return nil, fmt.Errorf("profile %s: credential is required", p.Name)
	}
	switch p.Auth {
	case AuthNone, "":
		return provider.None{}, nil
	case AuthBearer:
		return provider.Bearer(key), nil
	case AuthHeader:
		return provider.Header{Name: p.AuthName, Value: key}, nil
	case AuthQuery:
		return provider.Query{Name: p.AuthName, Value: key}, nil
	}
	build, ok := lookupAuth(p.Auth)
	if !ok {
		return nil, fmt.Errorf("profile %s: unknown auth kind %q", p.Name, p.Auth)
	}
	return build(p, key)
}

// Builds a provider from the profile and a credential, resolving the protocol from the registry
func (p Profile) Build(key string) (*provider.Provider, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	proto, ok := protocol.Lookup(p.Protocol)
	if !ok {
		return nil, fmt.Errorf("profile %s: protocol %q is not registered", p.Name, p.Protocol)
	}
	auth, err := p.auth(key)
	if err != nil {
		return nil, err
	}
	return &provider.Provider{Name: p.Name, Protocol: proto, BaseURL: p.BaseURL, Auth: auth, Header: p.Header.Clone(), Query: cloneValues(p.Query), Extra: maps.Clone(p.Extra), Retry: provider.DefaultRetry}, nil
}

// Builds a provider with the credential read from the key source
func (p Profile) Open(ctx context.Context) (*provider.Provider, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	key, err := p.Key.Resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", p.Name, err)
	}
	if p.needsKey() && key == "" {
		if p.Key.Kind == "" {
			return nil, fmt.Errorf("profile %s: credential is required and no key source is set", p.Name)
		}
		return nil, fmt.Errorf("profile %s: %s yielded no credential", p.Name, p.Key.Describe())
	}
	return p.Build(key)
}

// Lists the models the endpoint serves through the vendor the profile names
func (p Profile) ListModels(ctx context.Context, prov *provider.Provider) ([]Model, error) {
	v, ok := LookupVendor(p.Vendor)
	if !ok {
		return nil, fmt.Errorf("profile %s: no vendor %q is registered for listing models", p.Name, p.Vendor)
	}
	return v.Models(ctx, prov)
}

// One model an endpoint serves, with zero for limits the endpoint does not report
type Model struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Context   int    `json:"context,omitempty"`
	MaxOutput int    `json:"max_output,omitempty"`
	// Unix seconds when the endpoint reports a creation time
	Created int64 `json:"created,omitempty"`
}

// Conventions of one endpoint family registered by its package
type Vendor struct {
	Name string
	// Lists the models an endpoint of this family serves
	Models func(ctx context.Context, p *provider.Provider) ([]Model, error)
}

var (
	mu       sync.RWMutex
	profiles = map[string]Profile{}
	vendors  = map[string]Vendor{}
	auths    = map[AuthKind]AuthBuilder{}
)

// Makes a built in profile available to Lookup, panicking on an invalid or duplicate one
func Register(p Profile) {
	if err := p.Validate(); err != nil {
		panic("profiles: " + err.Error())
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := profiles[p.Name]; dup {
		panic("profiles: duplicate registration of " + p.Name)
	}
	profiles[p.Name] = p
}

// Makes a vendor available to profiles naming it, panicking on a duplicate
func RegisterVendor(v Vendor) {
	if v.Name == "" || v.Models == nil {
		panic("profiles: vendor needs a name and a model lister")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := vendors[v.Name]; dup {
		panic("profiles: duplicate vendor " + v.Name)
	}
	vendors[v.Name] = v
}

// Makes an auth kind buildable, panicking on a duplicate or a generic kind
func RegisterAuth(kind AuthKind, b AuthBuilder) {
	switch kind {
	case AuthNone, "", AuthBearer, AuthHeader, AuthQuery:
		panic("profiles: auth kind " + string(kind) + " is built in")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := auths[kind]; dup {
		panic("profiles: duplicate auth kind " + string(kind))
	}
	auths[kind] = b
}

func lookupAuth(kind AuthKind) (AuthBuilder, bool) {
	mu.RLock()
	defer mu.RUnlock()
	b, ok := auths[kind]
	return b, ok
}

// Built in profiles sorted by name
func Defaults() []Profile {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Profile, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Finds a built in profile by name
func Lookup(name string) (Profile, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := profiles[name]
	return p, ok
}

func LookupVendor(name string) (Vendor, bool) {
	mu.RLock()
	defer mu.RUnlock()
	v, ok := vendors[name]
	return v, ok
}

// Registered vendor names sorted
func Vendors() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(vendors))
	for name := range vendors {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func cloneValues(v url.Values) url.Values {
	if v == nil {
		return nil
	}
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}
