package pm

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
)

var (
	registryMu sync.RWMutex
	packages   = map[string]Package{}
)

// UnknownPackageError reports a name no registered definition carries
type UnknownPackageError struct{ Name string }

func (e *UnknownPackageError) Error() string { return "unknown package: " + e.Name }

// Registers a package definition under its name, a later registration of the same name winning
func Register(p Package) error {
	base := p.Base()
	if base.Name == "" {
		return fmt.Errorf("package has no name")
	}
	base.bind(p)
	registryMu.Lock()
	packages[base.Name] = p
	registryMu.Unlock()
	return nil
}

// Registers a package from an init function, panicking on a definition without a name
func MustRegister(p Package) {
	if err := Register(p); err != nil {
		panic(err)
	}
}

// Registered definition of a package
func GetPackage(name string) (Package, error) {
	registryMu.RLock()
	p, ok := packages[name]
	registryMu.RUnlock()
	if !ok {
		return nil, &UnknownPackageError{Name: name}
	}
	return p, nil
}

// Sorted names of every registered package
func AllPackages() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return sortedKeys(packages)
}

// Supported runtime roots for a source install, internal tools entering only through dependencies
func SourceInstallPackages(names []string) ([]string, error) {
	target, err := CurrentTarget()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		p, err := GetPackage(name)
		if err != nil {
			return nil, err
		}
		base := p.Base()
		if !base.Internal && (name == "python" || !base.Optional) && p.MissingReason(target) == "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// Packages to publish before a venv sync, which is not one of them
func ToolRoots(names []string) ([]string, error) {
	roots, err := SourceInstallPackages(names)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range roots {
		p, err := GetPackage(name)
		if err != nil {
			return nil, err
		}
		if _, isState := p.(StatePackage); !isState {
			out = append(out, name)
		}
	}
	return out, nil
}

// PackageDefinition is the identity of a non built in registration a fresh worker restores by name
type PackageDefinition struct {
	Name     string `json:"name"`
	Module   string `json:"module"`
	Qualname string `json:"qualname"`
}

// Go package path of this package, whose definitions every worker of this binary already carries
var builtinModule = reflect.TypeOf(BasePackage{}).PkgPath()

// Type identity of a registered definition
func definitionIdentity(p Package) (string, string) {
	t := reflect.TypeOf(p)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.PkgPath(), t.Name()
}

// Identities of the registrations a fresh worker must restore, all of them or only a closure
func PackageDefinitions(names []string) ([]PackageDefinition, error) {
	var selected []Package
	if names != nil {
		walked, err := Walk(names)
		if err != nil {
			return nil, err
		}
		selected = walked
	} else {
		for _, name := range AllPackages() {
			p, _ := GetPackage(name)
			selected = append(selected, p)
		}
	}
	var out []PackageDefinition
	for _, p := range selected {
		module, qualname := definitionIdentity(p)
		if module == builtinModule {
			continue
		}
		if module == "" || qualname == "" || module == "main" {
			return nil, NewInstallError(p.Base().Name, fmt.Sprintf("package definition %s.%s is not importable", module, qualname),
				"define and register a module-level Package subclass in an importable file")
		}
		out = append(out, PackageDefinition{Name: p.Base().Name, Module: module, Qualname: qualname})
	}
	return out, nil
}

// Restores explicit registrations by identity, refusing any this binary does not carry
func LoadPackageDefinitions(definitions []PackageDefinition) error {
	for _, d := range definitions {
		p, err := GetPackage(d.Name)
		if err == nil {
			module, qualname := definitionIdentity(p)
			if module == d.Module && qualname == d.Qualname {
				continue
			}
			err = fmt.Errorf("definition now registers %s.%s", module, qualname)
		}
		return NewInstallError(d.Name, fmt.Sprintf("cannot load package definition %s.%s: %v", d.Module, d.Qualname, err),
			"keep the package definition and its dependencies importable in the isolated PM runtime")
	}
	return nil
}

// Dependencies first topological order over the requested packages, refusing cycles
func Walk(names []string) ([]Package, error) {
	seen := map[string]bool{}
	var order []Package
	var visit func(name string, chain []string) error
	visit = func(name string, chain []string) error {
		for _, c := range chain {
			if c == name {
				return fmt.Errorf("dependency cycle: %s", strings.Join(append(append([]string{}, chain...), name), " -> "))
			}
		}
		if seen[name] {
			return nil
		}
		p, err := GetPackage(name)
		if err != nil {
			return err
		}
		next := append(append([]string{}, chain...), name)
		for _, dep := range p.Base().Deps {
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
