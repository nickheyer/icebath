package plugins

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Requirements the package manager installs into the shared environment for a plugin
func declaredRequirements(mf *Manifest) []string {
	if mf == nil {
		return nil
	}
	if mf.Path == "" || !isDir(mf.Path) {
		return mf.PythonDeps
	}
	declaration, err := pm.ReadPythonDeclaration(mf.Path)
	if err != nil {
		return nil
	}
	requirements, err := declaration.InstallRequirements()
	if err != nil {
		return nil
	}
	return requirements
}

// Declared Python requirements of a plugin that apply here and are not installed in the selected environment
func MissingPythonDependencies(ctx context.Context, mf *Manifest) []string {
	site, err := pm.SelectedSitePackages(ctx)
	if err != nil {
		return nil
	}
	var missing []string
	for _, req := range declaredRequirements(mf) {
		if !pm.RequirementAppliesHere(ctx, req) {
			continue
		}
		if installed, known := pm.DistInstalled(req, site); known && !installed {
			missing = append(missing, req)
		}
	}
	return missing
}

// Whether every requirement is installed in the dependency environment the install currently selects
func InstalledInSelectedEnvironment(ctx context.Context, requirements []string) bool {
	site, err := pm.SelectedSitePackages(ctx)
	if err != nil || len(requirements) == 0 {
		return false
	}
	for _, req := range requirements {
		if installed, known := pm.DistInstalled(req, site); !known || !installed {
			return false
		}
	}
	return true
}

// Warns about declared Python dependencies that are missing, discovery never installing them
func (m *Manager) warnPythonDependencies(ctx context.Context, mf *Manifest) {
	missing := MissingPythonDependencies(ctx, mf)
	if len(missing) == 0 {
		return
	}
	key := mf.ID()
	if InstalledInSelectedEnvironment(ctx, missing) {
		logWarn(fmt.Sprintf("Plugin %s's Python dependencies were installed after this process started: %s. "+
			"A running process never switches dependency environments; restart Icebath "+
			"(icebath gateway restart for the gateway) to load them.", key, strings.Join(missing, ", ")))
		return
	}
	logWarn(fmt.Sprintf("Plugin %s declares Python dependencies that are not "+
		"installed: %s. For an enabled plugin, run icebath pm repair, "+
		"then restart Icebath. Discovery does not install dependencies.", key, strings.Join(missing, ", ")))
}

// Plugins that failed to load while a declared Python dependency is missing, as activation entries
func DependencyBlockedSummaries(ctx context.Context, m *Manager) []map[string]any {
	out := []map[string]any{}
	keys, loaded := m.loadedSnapshot()
	for i, key := range keys {
		if loaded[i].Error == "" {
			continue
		}
		missing := MissingPythonDependencies(ctx, loaded[i].Manifest)
		if len(missing) == 0 {
			continue
		}
		name := key
		if loaded[i].Manifest != nil && loaded[i].Manifest.Name != "" {
			name = loaded[i].Manifest.Name
		}
		out = append(out, map[string]any{"name": name, "key": key, "activated_now": map[string]any{},
			"deferred": map[string]any{}, "missing_dependencies": anyList(missing)})
	}
	return out
}

// Restart line of each process kind that can report a dependency gap
var restartLines = map[string]string{
	"gateway": "  icebath gateway restart",
	"backend": "  quit and reopen the Icebath desktop app (or restart icebath dashboard)",
}

// What to do when running processes lack the dependencies of a plugin, one restart line per reporting process
func dependencyRestartHint(ctx context.Context, missing, processes []string) (string, error) {
	lines := make([]string, 0, len(processes))
	for _, process := range processes {
		line, ok := restartLines[process]
		if !ok {
			return "", fmt.Errorf("KeyError: %s", config.PyRepr(process))
		}
		lines = append(lines, line)
	}
	deps, restart := strings.Join(missing, ", "), strings.Join(lines, "\n")
	if InstalledInSelectedEnvironment(ctx, missing) {
		return fmt.Sprintf("Its Python dependencies (%s) were installed after the running gateway or dashboard "+
			"started, and a running process never switches dependency environments. Restart them to "+
			"load the plugin:\n%s", deps, restart), nil
	}
	return fmt.Sprintf("Its Python dependencies (%s) are not installed. Run icebath pm repair, then restart:\n%s", deps, restart), nil
}

// Sorted union of the dependency gaps the gateway and the backend report with the processes that have one
func dependencyGaps(gateway, backend any) (missing []string, in []string, err error) {
	seen := map[string]bool{}
	for _, gap := range []struct {
		process string
		raw     any
	}{{"gateway", gateway}, {"backend", backend}} {
		deps, err := pyStrItems(gap.raw)
		if err != nil {
			return nil, nil, err
		}
		if len(deps) > 0 {
			in = append(in, gap.process)
		}
		for _, dep := range deps {
			seen[dep] = true
		}
	}
	for dep := range seen {
		missing = append(missing, dep)
	}
	sort.Strings(missing)
	return missing, in, nil
}
