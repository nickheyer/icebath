// Package delegation is what decides where the subagents an agent delegates tasks to run: backends that route each task to the model, the
// credentials and the profile its subagent runs on, replaceable by plugins, and the settings of delegation
package delegation

import (
	"cmp"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	delegationconfig "github.com/nickheyer/icebath/internal/delegation/config"
)

// Backend a blank or unset delegation.backend selects
const Default = "default"

// One task of a delegate_task call as a backend routes it
type Task struct {
	// Place of the task in its call
	Index   int
	Goal    string
	Context string
	// Values the model gave the parameters the backend declares, by name
	Params map[string]any
}

// Where one task's subagent runs; every field left empty takes what the agent that delegated runs on
type Route struct {
	Model    string
	Provider string
	// Endpoint of its own and its key and request format, for a model served outside the profile's providers
	BaseURL string
	APIKey  string
	APIMode string
	// Fields added to every request of the subagent
	RequestOverrides map[string]any
	// How hard it thinks, none turning reasoning off
	ReasoningEffort string
	// Speed tier its requests ask for, such as fast
	ServiceTier string
	// Models it falls back to, each provider:model; nil takes delegation.fallback_providers, empty none
	Fallbacks []string
	// Profile whose own main model, keys, reasoning and fallbacks it runs on
	Profile string
	// Name of the route in the results, such as the name of a pool
	Label string
}

// Parameter a backend adds to the tasks of delegate_task
type Parameter struct {
	Name string
	// JSON schema of its value, an enum of which bounds the values a task may give
	Schema map[string]any
}

// Refusal of one task by a backend, its siblings still started: the text the model reads
type Refusal struct {
	Message string
}

func (r *Refusal) Error() string { return r.Message }

// Strategy turning a task into the route of its subagent, chosen by delegation.backend
type Backend struct {
	// Value of delegation.backend that selects it
	Name  string
	Title string
	// Line describing it in pickers
	Summary string
	// Whether it can run here; nil for always
	Available func(cfg *config.Config) bool
	// Parameters it adds to each task of delegate_task; nil for none
	Parameters func(cfg *config.Config) []Parameter
	// What it adds to delegate_task's description; nil for nothing
	Description func(cfg *config.Config) string
	// Route of one task, a *Refusal refusing that task alone
	Route func(cfg *config.Config, task Task) (Route, error)
}

// Settings of the backend under delegation.backends, by its name; none when unset
func (b Backend) Settings(cfg *config.Config) map[string]any {
	backends, _ := cfg.Get(delegationconfig.Backends).(map[string]any)
	own, _ := backends[b.Name].(map[string]any)
	return own
}

// Task fields delegate_task owns, which no backend may declare as its own
var Reserved = map[string]bool{"goal": true, "context": true, "index": true, "output_schema": true, "images": true, "group": true, "role": true,
	"toolsets": true, "acp_command": true, "acp_args": true, "provider": true, "model": true, "base_url": true, "api_key": true, "api_mode": true}

var parameterName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// Parameters the backend declares, in its order, without those named badly or reserved, which are logged
func (b Backend) Declared(cfg *config.Config) []Parameter {
	if b.Parameters == nil {
		return nil
	}
	var out []Parameter
	for _, p := range b.Parameters(cfg) {
		if !parameterName.MatchString(p.Name) || Reserved[p.Name] {
			slog.Warn(fmt.Sprintf("delegation provider %s: task parameter %q refused (reserved or invalid name)", b.Name, p.Name))
			continue
		}
		if p.Schema == nil {
			continue
		}
		out = append(out, p)
	}
	return out
}

// What the backend adds to delegate_task's description, trimmed
func (b Backend) Addendum(cfg *config.Config) string {
	if b.Description == nil {
		return ""
	}
	return strings.TrimSpace(b.Description(cfg))
}

func (b Backend) available(cfg *config.Config) bool {
	return b.Available == nil || b.Available(cfg)
}

type Registrant interface {
	Backends() []Backend
}

type Registry struct {
	registrants []Registrant
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Backends() []Backend {
	var backends []Backend
	for _, registrant := range r.registrants {
		backends = append(backends, registrant.Backends()...)
	}
	return backends
}

// Backend of the name, false when no plugin brings one
func (r *Registry) Backend(name string) (Backend, bool) {
	for _, backend := range r.Backends() {
		if strings.EqualFold(backend.Name, name) {
			return backend, true
		}
	}
	return Backend{}, false
}

// Backend delegation.backend names, the default when blank
func Configured(cfg *config.Config) string {
	return cmp.Or(strings.ToLower(strings.TrimSpace(cfg.GetString(delegationconfig.Backend))), Default)
}

// The backend of the name and, when it can't run here, why with what to do about it
func (r *Registry) Status(cfg *config.Config, name string) (Backend, string) {
	backend, ok := r.Backend(name)
	switch {
	case ok && backend.available(cfg):
		return backend, ""
	case !ok && name == Default:
		return backend, "Subagents can't run, because this build of icebath leaves out the usual way to run them."
	case !ok:
		return backend, fmt.Sprintf("No plugin that is turned on runs subagents with %s. Install or turn on the plugin that does, or go back to "+
			"the usual way with `/delegation default` or `icebath config set delegation.backend default`.", name)
	}
	return backend, fmt.Sprintf("Subagents can't run with %s here, because its plugin says it can't. Fix its setup, or go back to the usual "+
		"way with `/delegation default` or `icebath config set delegation.backend default`.", name)
}

// Backend the settings select, a refusal naming it and the fix when it can't run
func (r *Registry) Active(cfg *config.Config) (Backend, error) {
	backend, reason := r.Status(cfg, Configured(cfg))
	if reason != "" {
		return backend, &Refusal{Message: reason + " No subagent was started."}
	}
	return backend, nil
}

// A backend as pickers list it, unavailable ones with why
type Info struct {
	Name, Title, Summary string
	Available            bool
	Reason               string
}

// Every backend, the default first and the rest by name
func (r *Registry) List(cfg *config.Config) []Info {
	names := []string{Default}
	for _, backend := range r.Backends() {
		if name := strings.ToLower(backend.Name); !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names[1:])
	out := make([]Info, 0, len(names))
	for _, name := range names {
		backend, reason := r.Status(cfg, name)
		out = append(out, Info{Name: name, Title: cmp.Or(backend.Title, name), Summary: backend.Summary, Available: reason == "", Reason: reason})
	}
	return out
}

type Delegation struct {
	Registry
}

func New(cfg *config.Config) *Delegation {
	d := &Delegation{}
	delegationconfig.Register(cfg, func() []config.Option {
		var options []config.Option
		for _, info := range d.List(cfg) {
			about := info.Summary
			if _, installed := d.Backend(info.Name); !installed {
				about = "Not installed"
			} else if !info.Available {
				about = "Can't run here"
			}
			options = append(options, config.Option{Value: info.Name, Title: info.Title, About: about})
		}
		return options
	})
	return d
}
