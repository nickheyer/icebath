// Package commands defines slash commands and dispatches them to bound handlers
package commands

import (
	"context"
	"sort"
	"sync"

	"github.com/nickheyer/icebath/pkg/i18n"
)

// Behaviour of a command typed while the agent is mid turn
type BusyPolicy string

const (
	BusyReject                BusyPolicy = "reject"
	BusyDispatch              BusyPolicy = "dispatch"
	BusyInterruptThenDispatch BusyPolicy = "interrupt_then_dispatch"
)

// Handler executes one parsed slash command
type Handler func(ctx context.Context, inv *Invocation) error

// Def is one slash command with its metadata and bound handler
type Def struct {
	Name         string
	Description  string
	Category     string
	Aliases      []string
	ArgsHint     string
	Subcommands  []string
	CLIOnly      bool
	BusyPolicy   BusyPolicy
	BusyHandler  string
	ArgumentMode string
	Plugin       string
	Handler      Handler
	Meta
}

// Localized slash description from the catalog, the English field when no catalog carries the key
func (d *Def) Describe(ctx context.Context) string {
	key := "slash." + d.Name + ".description"
	if value := i18n.T(ctx, key, "", nil); value != key {
		return value
	}
	return d.Description
}

// Invocation is one typed slash command with its parsed arguments
type Invocation struct {
	Name string
	Raw  string
	Args string
	Argv []string
	Def  *Def
	// Host surface the handler drives, typed by the binding package
	Host any
}

// Registry maps command names and aliases to definitions
type Registry struct {
	mu      sync.RWMutex
	defs    map[string]*Def
	aliases map[string]string
}

// Default is the process wide command registry
var Default = NewRegistry()

// NewRegistry returns an empty registry
func NewRegistry() *Registry {
	return &Registry{defs: map[string]*Def{}, aliases: map[string]string{}}
}

// Register adds a command to the default registry
func Register(d Def) { Default.Register(d) }

// Register adds a command, replacing any earlier definition of the same name
func (r *Registry) Register(d Def) {
	if d.Name == "" {
		panic("commands: register requires a name")
	}
	if d.BusyPolicy == "" {
		d.BusyPolicy = BusyReject
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, ok := r.defs[d.Name]; ok {
		for _, a := range prev.Aliases {
			delete(r.aliases, a)
		}
	}
	copy := d
	r.defs[d.Name] = &copy
	for _, a := range d.Aliases {
		r.aliases[a] = d.Name
	}
}

// Bind attaches a handler to an already registered command and reports whether it exists
func (r *Registry) Bind(name string, h Handler) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.defs[name]
	if ok {
		d.Handler = h
	}
	return ok
}

// Lookup resolves a name or alias to its definition
func (r *Registry) Lookup(name string) *Def {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if canon, ok := r.aliases[name]; ok {
		name = canon
	}
	return r.defs[name]
}

// Names lists canonical command names sorted
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.defs))
	for n := range r.defs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Remove drops a command and its aliases and reports whether it existed
func (r *Registry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.defs[name]
	if ok {
		for _, a := range d.Aliases {
			delete(r.aliases, a)
		}
		delete(r.defs, name)
	}
	return ok
}
