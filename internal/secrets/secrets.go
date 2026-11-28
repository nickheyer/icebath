// Secret stores API keys and other variables come from, such as Bitwarden Secrets Manager and 1Password, and the environment each profile
// reads through them: its own .env and what its stores supply over this process's environment
package secrets

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cast"

	"github.com/nickheyer/icebath/internal/config"
	secretsconfig "github.com/nickheyer/icebath/internal/secrets/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Longest one fetch of a store may take unless its timeout_seconds says otherwise
const FetchTimeout = 120 * time.Second

// Kind of failure of a fetch, which picks the fix a person is told
type Failure string

const (
	NotConfigured Failure = "not_configured"
	BinaryMissing Failure = "binary_missing"
	AuthFailed    Failure = "auth_failed"
	AuthExpired   Failure = "auth_expired"
	RefInvalid    Failure = "ref_invalid"
	Network       Failure = "network"
	EmptyValue    Failure = "empty_value"
	Timeout       Failure = "timeout"
	Internal      Failure = "internal"
)

// How a store hands its secrets over
const (
	// Variables bound one by one to references in the store, such as op:// ones
	Mapped = "mapped"
	// Every secret of a project, each under its own name
	Bulk = "bulk"
)

// Fix of each kind of failure when a store names none of its own, {name} standing for the store
var remedies = map[Failure]string{
	NotConfigured: "Run icebath secrets {name} setup to finish setting it up.",
	BinaryMissing: "Run icebath secrets {name} setup to install the program it needs.",
	AuthFailed:    "Run icebath secrets {name} setup to sign in again.",
	AuthExpired:   "The sign-in has expired. Run icebath secrets {name} setup to sign in again.",
	Network:       "Check your network connection, then run icebath secrets status --reload.",
	Timeout:       "If the store is only slow, raise its time limit with icebath config set secrets.{name}.timeout_seconds 300.",
}

// What a store is, fixed for it
type Spec struct {
	// Name of its section, secrets.<name>, and of its subcommand
	Name string
	// Name in status lines, and the short one variables are said to come from
	Label, Short string
	Shape        string
	// Scheme of the references it resolves, such as op for op://, owned by one store only, a reference of it a person reads as an example,
	// and a sentence saying where a person finds one
	Scheme, Example, Find string
	// Setting of its section naming the variable that holds its own credential, and that variable unless the setting names another; no
	// store may overwrite it
	TokenSetting, TokenEnv string
	// Fix of a kind of failure, {name} standing for the store and {token_env} for the variable of its credential
	Hints map[Failure]string
}

// One fetch of a store's secrets for a profile
type Fetch struct {
	// Settings of the profile, read without the stores, so a fetch never waits on itself
	Config *config.Config
	// Variables the profile has without any store: its .env over this process's environment
	Env map[string]string
	// Caches are neither read nor written
	Fresh bool
}

// What one fetch got
type Result struct {
	// Values by variable
	Secrets  map[string]string
	Warnings []string
	// Why it failed, empty when it did not
	Error string
	Kind  Failure
	// Helper program it ran
	Binary string
}

func (r Result) OK() bool { return r.Error == "" }

// Result failing with the error of the kind
func Failed(text string, kind Failure) Result {
	return Result{Secrets: map[string]string{}, Error: text, Kind: kind}
}

// Store of secrets a plugin brings
type Source interface {
	Spec() Spec
	// Resolves the store's secrets for the profile; a failure is a Result, never a panic
	Fetch(ctx context.Context, fetch Fetch) Result
}

// Store that fetches with settings of its own beyond its credential, such as the project a bulk store pulls: the key of the first of them
// the profile leaves empty, empty once it is configured
type Configurable interface {
	Unconfigured(cfg *config.Config) string
}

type Registrant interface {
	Sources() []Source
}

// Sources fixed once they are registered
type Sources []Source

func (s Sources) Sources() []Source { return s }

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in, nil for the host's own
	host *Secrets
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Sources() []Source {
	var sources []Source
	for _, registrant := range r.registrants {
		sources = append(sources, registrant.Sources()...)
	}
	return sources
}

// Host the registry is nested in, through which plugins fetch from other stores and read a profile's environment
func (r *Registry) Host() *Secrets {
	return r.host
}

// The secret stores and the environment of every profile
type Secrets struct {
	Registry
	config *config.Config

	once    sync.Once
	sources []Source

	mu sync.Mutex
	// This process's environment carries the profile in use, from Serve on
	serving bool
	active  string
	// What was written into this process's environment for the profile in use, by name
	written map[string]written
	// What the stores supplied each profile, by profile
	scopes map[string]*scope
	// Credentials whose stray characters were already said
	warned map[string]bool
	// Where the status lines of the next fetch of the profile in use go, nil sending them to the log
	out io.Writer
}

func New(cfg *config.Config) *Secrets {
	secretsconfig.Register(cfg)
	h := &Secrets{config: cfg, written: map[string]written{}, scopes: map[string]*scope{}, warned: map[string]bool{}}
	cfg.UseEnvironment(h)
	h.guard(cfg)
	return h
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (h *Secrets) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = h
	}
	h.Registry.Register(registrant)
}

var sourceName = regexp.MustCompile(`^[a-z0-9_]*[a-z0-9][a-z0-9_]*$`)

// Every store in the order they are registered, checked once every plugin registered: a store with a bad name or shape, a name or scheme
// taken twice, is a mistake of the code
func (h *Secrets) all() []Source {
	h.once.Do(func() {
		names, schemes := map[string]bool{}, map[string]string{}
		for _, source := range h.Registry.Sources() {
			spec := source.Spec()
			switch {
			case !sourceName.MatchString(spec.Name):
				panic(fmt.Sprintf("secrets: store name %q is not lower case letters, digits and underscores", spec.Name))
			case spec.Shape != Mapped && spec.Shape != Bulk:
				panic(fmt.Sprintf("secrets: store %s has shape %q, not mapped or bulk", spec.Name, spec.Shape))
			case names[spec.Name]:
				panic("secrets: store " + spec.Name + " is registered twice")
			case spec.Scheme != "" && schemes[spec.Scheme] != "":
				panic(fmt.Sprintf("secrets: scheme %s:// of store %s is owned by store %s", spec.Scheme, spec.Name, schemes[spec.Scheme]))
			}
			names[spec.Name] = true
			if spec.Scheme != "" {
				schemes[spec.Scheme] = spec.Name
			}
			h.sources = append(h.sources, source)
		}
	})
	return h.sources
}

// Store of the name
func (h *Secrets) Source(name string) (Source, bool) {
	for _, source := range h.all() {
		if source.Spec().Name == name {
			return source, true
		}
	}
	return nil, false
}

// Every store: those secrets.sources names first, in its order, then the others in the order they are registered
func (h *Secrets) Ordered(cfg *config.Config) []Source {
	all := h.all()
	var order []Source
	var unknown []string
	for _, name := range cfg.GetStringSlice(secretsconfig.Sources) {
		name = strings.TrimSpace(name)
		source, ok := h.Source(name)
		switch {
		case !ok:
			unknown = append(unknown, name)
		case !slices.Contains(order, source):
			order = append(order, source)
		}
	}
	if len(unknown) > 0 {
		known := make([]string, len(all))
		for i, source := range all {
			known[i] = source.Spec().Name
		}
		slog.Warn(fmt.Sprintf("secrets.sources names unknown source(s): %s (known: %s)", strings.Join(unknown, ", "), cmp.Or(strings.Join(known, ", "), "none")))
	}
	for _, source := range all {
		if !slices.Contains(order, source) {
			order = append(order, source)
		}
	}
	return order
}

// Every store in the order a fetch asks them: those that fill named variables first, so a variable bound to a reference wins over one a store
// holds among all its secrets, then the others, each kind in its place in Ordered
func (h *Secrets) Asked(cfg *config.Config) []Source {
	var out []Source
	for _, shape := range []string{Mapped, Bulk} {
		for _, source := range h.Ordered(cfg) {
			if source.Spec().Shape == shape {
				out = append(out, source)
			}
		}
	}
	return out
}

// Whether the store has every setting it fetches with, its credential aside, and the key of the first one missing when it has not
func (h *Secrets) Configured(cfg *config.Config, source Source) (bool, string) {
	if c, ok := source.(Configurable); ok {
		if missing := c.Unconfigured(cfg); missing != "" {
			return false, missing
		}
	}
	return true, ""
}

// Whether the profile turned the store on
func (h *Secrets) Enabled(cfg *config.Config, name string) bool {
	return cfg.GetBool(secretsconfig.Of(name, secretsconfig.Enabled))
}

// Whether the store's values replace those the shell or the .env set already
func (h *Secrets) Overrides(cfg *config.Config, name string) bool {
	return cfg.GetBool(secretsconfig.Of(name, secretsconfig.Override))
}

// Variable holding the store's own credential for the profile
func (h *Secrets) TokenEnv(cfg *config.Config, spec Spec) string {
	if spec.TokenSetting != "" {
		if named := strings.TrimSpace(cfg.GetString(secretsconfig.Of(spec.Name, spec.TokenSetting))); named != "" {
			return named
		}
	}
	return spec.TokenEnv
}

// Longest a fetch of the store may take for the profile
func (h *Secrets) timeout(cfg *config.Config, name string) time.Duration {
	seconds, err := cast.ToFloat64E(cfg.Get(secretsconfig.Of(name, secretsconfig.Timeout)))
	if err != nil || seconds <= 0 {
		return FetchTimeout
	}
	return time.Duration(seconds * float64(time.Second))
}

// Fix of a failure of the store, empty when there is none to tell
func (h *Secrets) Remedy(cfg *config.Config, spec Spec, kind Failure) string {
	if kind == "" {
		return ""
	}
	hint := cmp.Or(spec.Hints[kind], remedies[kind])
	return strings.TrimSpace(strings.NewReplacer("{name}", spec.Name, "{token_env}", h.TokenEnv(cfg, spec)).Replace(hint))
}

// Fetches the store's secrets for the profile within its timeout, the variables it reads being the profile's without any store
func (h *Secrets) Fetch(ctx context.Context, cfg *config.Config, source Source, fresh bool) Result {
	plain := cfg.Plain()
	return h.fetch(ctx, source, Fetch{Config: plain, Env: h.unscoped(plain, gaps(plain)), Fresh: fresh})
}

func (h *Secrets) fetch(ctx context.Context, source Source, fetch Fetch) Result {
	spec := source.Spec()
	timeout := h.timeout(fetch.Config, spec.Name)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan Result, 1)
	go func() { done <- source.Fetch(ctx, fetch) }()
	select {
	case result := <-done:
		if result.Secrets == nil {
			result.Secrets = map[string]string{}
		}
		return result
	case <-ctx.Done():
		return Failed(fmt.Sprintf("%s did not answer within %s, so icebath went on without it.", spec.Label, strs.Span(timeout)), Timeout)
	}
}

// What the next fetch would do with a variable a store hands over: skip_token for the store's own credential, skip_set for one set
// already that the store does not replace, override for one it does, export for the rest
func Action(name, tokenEnv string, existing map[string]string, override bool) string {
	set := existing[name] != ""
	switch {
	case name == tokenEnv:
		return "skip_token"
	case set && !override:
		return "skip_set"
	case set:
		return "override"
	}
	return "export"
}

// Variables the profile has without its stores, to judge what a store would replace
func (h *Secrets) Existing(cfg *config.Config) map[string]string {
	return h.unscoped(cfg, gaps(cfg))
}
