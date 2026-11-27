// Packages icebath downloads and keeps on this machine for its features: pinned tools and runtimes in a hash checked store, and the
// Python environments features run their code in
package packages

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/config"
	packagesconfig "github.com/nickheyer/icebath/internal/packages/config"
)

// Packages the host itself runs on, declared by it
const (
	Python = "python"
	UV     = "uv"
	Node   = "node"
	NPM    = "npm"
	Git    = "git"
	GH     = "gh"
)

// Environment variable pointing a process at another tool store, such as the one an image ships
const RuntimeDirEnv = "ICEBATH_RUNTIME_DIR"

// What a feature needs on this machine: the tools it runs and the Python dependency sets it installs on request
type Registrant interface {
	Packages() []Package
	Extras() []Extra
}

// Declarations fixed once they are registered
type Declared struct {
	Tools []Package
	Sets  []Extra
}

func (d Declared) Packages() []Package { return d.Tools }

func (d Declared) Extras() []Extra { return d.Sets }

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in, nil for the host's own
	host *Packages
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Packages() []Package {
	var packages []Package
	for _, registrant := range r.registrants {
		packages = append(packages, registrant.Packages()...)
	}
	return packages
}

func (r *Registry) Extras() []Extra {
	var extras []Extra
	for _, registrant := range r.registrants {
		extras = append(extras, registrant.Extras()...)
	}
	return extras
}

// Host the registry is nested in, through which features install, find and run their packages
func (r *Registry) Host() *Packages {
	return r.host
}

// The store of pinned tools and the Python environments of this machine
type Packages struct {
	Registry
	config *config.Config

	mu sync.Mutex
	// Declarations by name, built on first use once every plugin registered
	byName map[string]*Package
	extras map[string]*Extra
	// Answer of the one libatomic install this process tries
	libatomic *attempt
	// Interpreters probed for being Python 3.8 or newer
	probes probes
}

func New(cfg *config.Config) *Packages {
	h := &Packages{config: cfg}
	packagesconfig.Register(cfg)
	h.Register(runtimes())
	return h
}

// Adds a registrant; a registry nested in the host reaches it through Host
func (h *Packages) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = h
	}
	h.Registry.Register(registrant)
}

// Declarations by name, a name declared twice being a mistake of the code
func (h *Packages) index() (map[string]*Package, map[string]*Extra) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byName != nil {
		return h.byName, h.extras
	}
	byName, extras := map[string]*Package{}, map[string]*Extra{}
	for _, p := range h.Registry.Packages() {
		if p.Name == "" {
			panic("packages: a package has no name")
		}
		if _, taken := byName[p.Name]; taken {
			panic("packages: package " + p.Name + " is declared twice")
		}
		byName[p.Name] = &p
	}
	for _, e := range h.Registry.Extras() {
		e.Name = extraKey(e.Name)
		if _, taken := extras[e.Name]; taken {
			panic("packages: extra " + e.Name + " is declared twice")
		}
		extras[e.Name] = &e
	}
	h.byName, h.extras = byName, extras
	return byName, extras
}

// Declaration of a package
func (h *Packages) Package(name string) (*Package, error) {
	byName, _ := h.index()
	if p, ok := byName[name]; ok {
		return p, nil
	}
	return nil, &UnknownError{Name: name}
}

// Names of every declared package, sorted
func (h *Packages) Names() []string {
	byName, _ := h.index()
	return slices.Sorted(func(yield func(string) bool) {
		for name := range byName {
			if !yield(name) {
				return
			}
		}
	})
}

// A name no declaration carries
type UnknownError struct{ Name string }

func (e *UnknownError) Error() string { return "unknown package: " + e.Name }

// Packages of the names with the ones each needs on the target ahead of it, refusing a cycle
func (h *Packages) walk(names []string, target string) ([]*Package, error) {
	seen := map[string]bool{}
	var order []*Package
	var visit func(name string, chain []string) error
	visit = func(name string, chain []string) error {
		if slices.Contains(chain, name) {
			return fmt.Errorf("These programs depend on each other in a circle: %s.", strings.Join(append(slices.Clone(chain), name), " → "))
		}
		if seen[name] {
			return nil
		}
		p, err := h.Package(name)
		if err != nil {
			return err
		}
		next := append(slices.Clone(chain), name)
		for _, dep := range p.depsOn(target) {
			if err := visit(dep, next); err != nil {
				return err
			}
		}
		if !seen[name] {
			seen[name] = true
			order = append(order, p)
		}
		return nil
	}
	for _, name := range names {
		if err := visit(name, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// Folder icebath keeps its machine wide state in
func (h *Packages) dir() string {
	return h.config.Dir()
}

// Store of this process: the one ICEBATH_RUNTIME_DIR names, else tools under the config folder
func (h *Packages) storeRoot() string {
	if override := os.Getenv(RuntimeDirEnv); override != "" {
		if abs, err := filepath.Abs(override); err == nil {
			return abs
		}
		return override
	}
	return filepath.Join(h.dir(), "tools")
}

// Downloads not finished yet, kept to resume
func (h *Packages) partials() string {
	return filepath.Join(h.dir(), "cache", "partials")
}

// Cache uv keeps its downloads and builds in, shared by every environment
func (h *Packages) uvCache() string {
	return filepath.Join(h.dir(), "cache", "uv")
}

// Machine wide state of the package manager: user pins, declines, the extras' environments
func (h *Packages) stateDir() string {
	return filepath.Join(h.dir(), "packages")
}
