// Named endpoint configurations and the credentials that turn them into providers
package profiles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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
	// Built in profile this one starts from, filling every field left empty
	Template string `json:"template,omitempty"`
	// Kept out of pickers and refused when chosen
	Disabled bool `json:"disabled,omitempty"`
	// Family of the endpoint, naming the registered vendor whose conventions apply
	Vendor   string      `json:"vendor,omitempty"`
	Protocol protocol.ID `json:"protocol,omitempty"`
	BaseURL  string      `json:"base_url,omitempty"`
	Auth     AuthKind    `json:"auth,omitempty"`
	// Header or query parameter name carrying the credential
	AuthName string `json:"auth_name,omitempty"`
	// Where the credential comes from
	Key Source `json:"key,omitzero"`
	// Static headers the endpoint expects on every request
	Header http.Header `json:"header,omitempty"`
	// Static query parameters the endpoint expects on every request
	Query url.Values `json:"query,omitempty"`
	// Model ids the endpoint is known to serve, the first being the default
	Models []string `json:"models,omitempty"`
	// Environment variable whose value replaces the base URL when set
	BaseURLEnv string `json:"base_url_env,omitempty"`
	// Request fields merged into every turn sent through the endpoint
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
	// Base prompt sent in place of the built in one, none sending no prompt at all
	System Source `json:"system,omitzero"`
	// Leaves the environment block about the machine and session out of the system prompt
	NoContext bool `json:"no_context,omitempty"`
	// Asks the endpoint to add no prompt or tools of its own
	Bare bool `json:"bare,omitempty"`
	// Keeps sessions off disk and asks the endpoint to keep nothing of them
	Ghost bool `json:"ghost,omitempty"`
	// Headers and body fields that ask the endpoint to keep nothing, nil taking the template's
	GhostDirective *provider.Directive `json:"ghost_directive,omitempty"`
	// Headers and body fields that ask the endpoint to add nothing of its own, nil taking the template's
	BareDirective *provider.Directive `json:"bare_directive,omitempty"`
	// Settings read by registered auth builders and vendors
	Options map[string]string `json:"options,omitempty"`
}

// Profile with every empty field taken from its template, unchanged without one
func (p Profile) Expand() (Profile, error) {
	if p.Template == "" {
		return p, nil
	}
	t, ok := Lookup(p.Template)
	if !ok {
		return Profile{}, fmt.Errorf("profile %s: no built in profile %q to start from", p.Name, p.Template)
	}
	out := p.Clone()
	if out.Vendor == "" {
		out.Vendor = t.Vendor
	}
	if out.Protocol == "" {
		out.Protocol = t.Protocol
	}
	if out.BaseURL == "" {
		out.BaseURL = t.BaseURL
	}
	if out.Auth == "" {
		out.Auth, out.AuthName = t.Auth, t.AuthName
	}
	if out.Key.Kind == "" {
		out.Key = t.Key
	}
	if out.Header == nil {
		out.Header = t.Header
	}
	if out.Query == nil {
		out.Query = t.Query
	}
	if len(out.Models) == 0 {
		out.Models = t.Models
	}
	if out.BaseURLEnv == "" {
		out.BaseURLEnv = t.BaseURLEnv
	}
	if out.Extra == nil {
		out.Extra = t.Extra
	}
	if out.System.Kind == "" {
		out.System = t.System
	}
	if out.GhostDirective == nil {
		out.GhostDirective = cloneDirective(t.GhostDirective)
	}
	if out.BareDirective == nil {
		out.BareDirective = cloneDirective(t.BareDirective)
	}
	if out.Options == nil {
		out.Options = t.Options
	}
	return out, nil
}

// Default model id, the first listed, or empty
func (p Profile) Model() string {
	if len(p.Models) > 0 {
		return p.Models[0]
	}
	return ""
}

// Reports whether a name starts with a letter or digit followed by letters, digits, underscores, dots or dashes
func ValidName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if alnum || (i > 0 && (c == '_' || c == '.' || c == '-')) {
			continue
		}
		return false
	}
	return true
}

// Checks that the profile names everything a build will need
func (p Profile) Validate() error {
	if !ValidName(p.Name) {
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
	if err := p.Key.validate(p.Name, "key"); err != nil {
		return err
	}
	return p.System.validate(p.Name, "system")
}

// Reports whether the generic auth kinds need a credential value
func (p Profile) needsKey() bool {
	return p.Auth == AuthBearer || p.Auth == AuthHeader || p.Auth == AuthQuery
}

// Where a profile credential stands
type CredentialState string

const (
	CredentialUnneeded CredentialState = "none needed"
	CredentialSet      CredentialState = "set"
	CredentialMissing  CredentialState = "missing"
)

// Checks that the profile can produce its credential without sending a request, naming the source rather than the key
func (p Profile) Credential(ctx context.Context) (CredentialState, string, error) {
	if p.Auth == AuthNone || p.Auth == "" {
		return CredentialUnneeded, "", nil
	}
	source := p.Key.Describe()
	key, err := p.Key.Resolve(ctx)
	if err != nil {
		return CredentialMissing, source, err
	}
	if p.needsKey() {
		if key == "" {
			return CredentialMissing, source, nil
		}
		return CredentialSet, source, nil
	}
	if key == "" {
		source = string(p.Auth)
	}
	auth, err := p.auth(key)
	if err != nil {
		return CredentialMissing, source, err
	}
	if r, ok := auth.(provider.Refresher); ok {
		if err := r.Refresh(ctx); err != nil {
			return CredentialMissing, source, err
		}
	}
	return CredentialSet, source, nil
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
	return &provider.Provider{Name: p.Name, Protocol: proto, BaseURL: p.BaseURL, Auth: auth, Header: p.Header.Clone(), Query: cloneValues(p.Query), Extra: maps.Clone(p.Extra), Directives: p.asked()}, nil
}

// Directives the profile asks of the endpoint, each as the endpoint hears it
func (p Profile) asked() []provider.Directive {
	var out []provider.Directive
	if p.Ghost && p.GhostDirective != nil {
		out = append(out, p.GhostDirective.Clone())
	}
	if p.Bare && p.BareDirective != nil {
		out = append(out, p.BareDirective.Clone())
	}
	return out
}

// Profile with the base URL override from the environment applied when the variable is set
func (p Profile) Resolved() Profile {
	if p.BaseURLEnv != "" {
		if v := strings.TrimSpace(os.Getenv(p.BaseURLEnv)); v != "" {
			p.BaseURL = strings.TrimRight(v, "/")
		}
	}
	return p
}

// Builds a provider with the credential read from the key source and the base URL override applied
func (p Profile) Open(ctx context.Context) (*provider.Provider, error) {
	p = p.Resolved()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	key, err := p.Key.Resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", p.Name, err)
	}
	if p.needsKey() && key == "" {
		if p.Key.Kind == "" || p.Key.Kind == SourceNone {
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

// One model an endpoint serves, with zero for limits the endpoint does not report and nil for metadata it does not know
type Model struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Vendor    string `json:"vendor,omitempty"`
	Context   int    `json:"context,omitempty"`
	MaxOutput int    `json:"max_output,omitempty"`
	// Unix seconds when the endpoint reports a creation time
	Created      int64         `json:"created,omitempty"`
	Pricing      *Pricing      `json:"pricing,omitempty"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
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

// Every auth kind a profile may name, the generic ones first and registered ones sorted after
func AuthKinds() []AuthKind {
	mu.RLock()
	defer mu.RUnlock()
	extra := make([]AuthKind, 0, len(auths))
	for k := range auths {
		extra = append(extra, k)
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	return append([]AuthKind{AuthNone, AuthBearer, AuthHeader, AuthQuery}, extra...)
}

func lookupAuth(kind AuthKind) (AuthBuilder, bool) {
	mu.RLock()
	defer mu.RUnlock()
	b, ok := auths[kind]
	return b, ok
}

// Built in profiles sorted by name, each a copy that leaves the registry untouched
func Defaults() []Profile {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Profile, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, p.Clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Finds a built in profile by name, returning a copy that leaves the registry untouched
func Lookup(name string) (Profile, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := profiles[name]
	if !ok {
		return Profile{}, false
	}
	return p.Clone(), true
}

// Deep copy whose maps and slices share nothing with the original
func (p Profile) Clone() Profile {
	p.Header = p.Header.Clone()
	p.Query = cloneValues(p.Query)
	p.Models = append([]string(nil), p.Models...)
	p.Extra = cloneExtra(p.Extra)
	p.GhostDirective = cloneDirective(p.GhostDirective)
	p.BareDirective = cloneDirective(p.BareDirective)
	p.Options = maps.Clone(p.Options)
	return p
}

func cloneDirective(d *provider.Directive) *provider.Directive {
	if d == nil {
		return nil
	}
	c := d.Clone()
	return &c
}

func cloneExtra(extra map[string]json.RawMessage) map[string]json.RawMessage {
	if extra == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(extra))
	for k, v := range extra {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
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

// Environment variable naming the directory every icebath configuration file lives in
const ConfigDirEnv = "ICEBATH_CONFIG_DIR"

// Directory holding icebath configuration, the environment override, else icebath under the user configuration directory
func ConfigDir() (string, error) {
	if dir := os.Getenv(ConfigDirEnv); dir != "" {
		return filepath.Abs(dir)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "icebath"), nil
}

// Path of the user profile list inside the configuration directory
func UserPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "profiles.json"), nil
}

// Issues a GET through the provider and decodes the JSON body into v
func GetJSON(ctx context.Context, p *provider.Provider, path string, query url.Values, v any) error {
	return DoJSON(ctx, p, protocol.Call{Method: http.MethodGet, Path: path, Query: query, Header: http.Header{"Accept": {"application/json"}}}, v)
}

// Issues any call through the provider and decodes the JSON body into v
func DoJSON(ctx context.Context, p *provider.Provider, call protocol.Call, v any) error {
	resp, err := p.Do(ctx, call)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s %s: %w", call.Method, call.Path, err)
	}
	return nil
}

// Where a text such as a credential or a prompt comes from
type SourceKind string

const (
	// Value names an environment variable
	SourceEnv SourceKind = "env"
	// Value is the text itself
	SourceStatic SourceKind = "static"
	// Value is a path whose trimmed contents are the text
	SourceFile SourceKind = "file"
	// Value is a shell command whose trimmed output is the text
	SourceExec SourceKind = "exec"
	// No text, kept over the source of a template
	SourceNone SourceKind = "none"
)

// Budget for an exec source
const ExecTimeout = 15 * time.Second

// One source of text, the zero value yielding none
type Source struct {
	Kind  SourceKind `json:"kind,omitempty"`
	Value string     `json:"value,omitempty"`
}

func Env(name string) Source { return Source{Kind: SourceEnv, Value: name} }

// Checks that a kind taking a value has one, naming what the source feeds in any error
func (s Source) validate(profile, what string) error {
	switch s.Kind {
	case "", SourceNone:
		return nil
	case SourceEnv, SourceStatic, SourceFile, SourceExec:
		if s.Value == "" {
			return fmt.Errorf("profile %s: %s %s source needs a value", profile, what, s.Kind)
		}
		return nil
	}
	return fmt.Errorf("profile %s: unknown %s source %q", profile, what, s.Kind)
}

// Names the source without revealing static text
func (s Source) Describe() string {
	switch s.Kind {
	case "", SourceNone:
		return "none"
	case SourceStatic:
		return "static"
	}
	return string(s.Kind) + " " + s.Value
}

// Produces the text, empty with no error for the zero source
func (s Source) Resolve(ctx context.Context) (string, error) {
	switch s.Kind {
	case "", SourceNone:
		return "", nil
	case SourceEnv:
		return os.Getenv(s.Value), nil
	case SourceStatic:
		return s.Value, nil
	case SourceFile:
		data, err := os.ReadFile(s.Value)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	case SourceExec:
		ctx, cancel := context.WithTimeout(ctx, ExecTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", s.Value)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("exec source: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(stdout.String()), nil
	}
	return "", fmt.Errorf("unknown key source %q", s.Kind)
}

// Accepts the object form or a string joining kind and value with a colon
func (s *Source) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		kind, value, ok := strings.Cut(text, ":")
		if !ok {
			return errors.New("source string must look like kind:value")
		}
		*s = Source{Kind: SourceKind(kind), Value: value}
		return nil
	}
	var obj struct {
		Kind  SourceKind `json:"kind"`
		Value string     `json:"value"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*s = Source(obj)
	return nil
}
