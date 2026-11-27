package credentials

import (
	"cmp"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
)

// Where a credential was found
type Origin int

const (
	// Saved in an account of the provider
	Saved Origin = iota + 1
	// Read from an environment variable, the one the source names
	Environment
	// Taken from the sign in of another program, the one the source names
	External
	// None is needed, the stand in of a server on this machine
	Unneeded
)

type Credential struct {
	APIKey  string
	BaseURL string
	Auth    provider.AuthKind
	Origin  Origin
	// Variable or program the credential came from, empty for one saved in icebath
	Source string
	// Headers its requests carry, such as the ChatGPT account a Codex sign-in is of
	Headers map[string]string
	// When it stops working, zero when it doesn't or that isn't known
	Expires time.Time
	// Who it signs in as, such as an email; never a secret
	Identity string
	// Signs each request, for a credential that signs requests rather than sending a key, such as AWS SigV4
	Sign func(request *http.Request, body []byte) error
	// Region its requests are signed for and served in, such as an AWS region; empty when its provider has none
	Region string
}

// One account of a provider of the profile: the fields it keeps, under the provider's accounts. A provider with no account has one with
// no name and no fields, whose credential can only come from the environment or another program
type Account struct {
	Name     string
	Provider provider.Instance
	Fields   config.View
}

// Kind of the provider the account is of
func (a Account) Spec() string {
	return a.Provider.Spec.Name
}

type Registrant interface {
	Credential(account Account) (Credential, bool)
}

type Registry struct {
	registrants []Registrant
	// What every registry of the host shares, nil for a registry of its own
	pool *pool
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Credential(account Account) (Credential, bool) {
	for _, registrant := range r.registrants {
		if credential, ok := registrant.Credential(account); ok {
			return credential, true
		}
	}
	return Credential{}, false
}

// Account of the provider with the name, the one in use for no name; with none, one that holds nothing and reads the profile's
// environment
func Of(instance provider.Instance, name string) Account {
	name = cmp.Or(name, instance.Account)
	if name == "" {
		return Account{Provider: instance, Fields: instance.Settings.Bare()}
	}
	return Account{Name: name, Provider: instance, Fields: instance.AccountFields(name)}
}

// Credential of a kind no provider of the profile has yet, found in the profile's environment or another program
func (r *Registry) Detect(cfg *config.Config, spec provider.Spec) (Credential, bool) {
	return r.Credential(Account{Provider: provider.Instance{ID: spec.Name, Spec: spec, Known: true}, Fields: cfg.View("").Bare()})
}

// Whether the provider of the account may not take its credential from the source, a variable or another program by name
func (a Account) Ignores(source string) bool {
	return source != "" && slices.Contains(a.Provider.Settings.GetStringSlice(providerconfig.Ignored), source)
}

// Credential kept in the field of the account, else read from the variable the account links to, else from the first of the variables
// set that its provider doesn't ignore
func (a Account) Lookup(field string, variables ...string) (Credential, bool) {
	if linked := strings.TrimSpace(a.Fields.GetString(providerconfig.KeyEnv)); linked != "" && a.Name != "" {
		return Lookup(a.Fields, field, linked)
	}
	return Lookup(a.Fields, field, slices.DeleteFunc(slices.Clone(variables), a.Ignores)...)
}

// Credential kept in the field of the account, else in the first of the variables the account's profile has set, a variable a secret
// store supplied naming the store
func Lookup(fields config.View, field string, variables ...string) (Credential, bool) {
	if field != "" {
		if value := strings.TrimSpace(fields.GetString(field)); Usable(value) {
			return Credential{APIKey: value, Origin: Saved}, true
		}
	}
	for _, variable := range variables {
		value, _ := fields.LookupEnv(variable)
		if value = strings.TrimSpace(value); !Usable(value) {
			continue
		}
		source := variable
		if store := fields.EnvOrigin(variable); store != "" {
			source += " (from " + store + ")"
		}
		return Credential{APIKey: value, Origin: Environment, Source: source}, true
	}
	return Credential{}, false
}

// Values shipped in examples and documentation in place of a key
var placeholders = map[string]bool{"*": true, "**": true, "***": true, "changeme": true, "your_api_key": true, "your_api_key_here": true,
	"your-api-key": true, "placeholder": true, "example": true, "dummy": true, "null": true, "none": true}

// Key families whose placeholder is the family's prefix with x's after it
var placeholderPrefixes = []string{"sk-", "ghp_", "hf_"}

// Whether a secret looks like one that can work rather than nothing or a placeholder of an example file
func Usable(value string) bool {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	if len(value) < 4 || placeholders[lower] || strings.HasPrefix(lower, "your_") && strings.HasSuffix(lower, "_here") {
		return false
	}
	xs := func(s string) bool { return s != "" && strings.Trim(s, "x") == "" }
	for _, prefix := range placeholderPrefixes {
		if rest, ok := strings.CutPrefix(lower, prefix); ok && xs(rest) {
			return false
		}
	}
	return !xs(strings.NewReplacer(" ", "", "-", "", "_", "").Replace(lower))
}

// Credential of a kind whose accounts keep an API key, read from the environment when an account keeps none
type Keyed struct {
	Provider  string
	Variables []string
}

func (k Keyed) Credential(account Account) (Credential, bool) {
	if account.Spec() != k.Provider {
		return Credential{}, false
	}
	return account.Lookup(providerconfig.APIKeyField, k.Variables...)
}

type Credentials struct {
	Registry
}

func New() *Credentials {
	return &Credentials{Registry{pool: newPool()}}
}

// Renews the credentials of the config's profiles under its folder's locks, reading the config again before each renewal so that what
// another process renewed is seen
func (c *Credentials) Keep(cfg *config.Config) {
	c.pool.config = cfg
}

// Adds a registrant, a nested registry sharing what the host keeps of its accounts
func (c *Credentials) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.pool = c.pool
	}
	c.Registry.Register(registrant)
}
