package plugins

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
)

var distNameSplit = regexp.MustCompile(`[<>=!~\[;\s]`)

// Distribution name a requirement names, the text before its first specifier, extra or marker character
func distName(req string) string {
	if loc := distNameSplit.FindStringIndex(req); loc != nil {
		req = req[:loc[0]]
	}
	return strings.TrimSpace(req)
}

var distNormalizeRe = regexp.MustCompile(`[-_.]+`)

// Name importlib metadata keys a distribution lookup by
func distNormalized(name string) string {
	return strings.ReplaceAll(strings.ToLower(distNormalizeRe.ReplaceAllString(name, "-")), "-", "_")
}

// Name an older installer wrote into a dist-info directory, lowercased with dashes as underscores
func distLegacyNormalized(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "-", "_")
}

// Version header of the first distribution on a path matching a name, found false when none is installed
func findDistributionVersion(name string, path []string) (version string, found bool) {
	want, legacy := distNormalized(name), distLegacyNormalized(name)
	for _, entry := range path {
		if entry == "" {
			entry = "."
		}
		children, err := os.ReadDir(entry)
		if err != nil {
			continue
		}
		var eggs []string
		entryBase := strings.ToLower(filepath.Base(entry))
		for _, child := range children {
			low := strings.ToLower(child.Name())
			if ext := filepath.Ext(low); ext == ".dist-info" || ext == ".egg-info" {
				if base, _, _ := strings.Cut(strings.TrimSuffix(low, ext), "-"); distNormalized(base) == want {
					return distributionVersion(filepath.Join(entry, child.Name())), true
				}
			} else if strings.HasSuffix(entryBase, ".egg") && low == "egg-info" {
				if base, _, _ := strings.Cut(strings.TrimSuffix(entryBase, ".egg"), "-"); distLegacyNormalized(base) == legacy {
					eggs = append(eggs, filepath.Join(entry, child.Name()))
				}
			}
		}
		if len(eggs) > 0 {
			return distributionVersion(eggs[0]), true
		}
	}
	return "", false
}

// Version header of an installed distribution's METADATA or PKG-INFO, empty when it has none
func distributionVersion(dir string) string { return distributionHeader(dir, "version") }

// One header of an installed distribution's METADATA or PKG-INFO, matched case insensitively, empty when absent
func distributionHeader(dir, header string) string {
	text := ""
	for _, name := range []string{"METADATA", "PKG-INFO"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			text = string(data)
			break
		}
	}
	if text == "" {
		if data, err := os.ReadFile(dir); err == nil {
			text = string(data)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break
		}
		if key, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(key), header) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// Whether a version satisfies a specifier set under packaging rules with prereleases admitted
func specifierContains(specifiers []string, version string) (bool, error) {
	spec := strings.Join(specifiers, ",")
	probe := pm.PythonDeclaration{PyprojectInstalls: true, RequiresPython: &spec}
	reason, err := probe.PythonError(version)
	return err == nil && reason == "", err
}

// Trees Python plugin code imports from, the path this process hands its Python children
func processImportPath() []string {
	return filepath.SplitList(os.Getenv("PYTHONPATH"))
}

// Whether a distribution the requirement accepts is installed on a path, nil when the requirement names none
func distInstalled(req string, path []string) *bool {
	dist := distName(req)
	if dist == "" {
		return nil
	}
	if path == nil {
		path = processImportPath()
	}
	version, found := findDistributionVersion(dist, path)
	if !found {
		no := false
		return &no
	}
	accepted := true
	if parsed, err := pm.ParseRequirement(req); err == nil {
		specs := make([]string, 0, len(parsed.Specifier))
		for _, s := range parsed.Specifier {
			specs = append(specs, s.String())
		}
		if ok, err := specifierContains(specs, version); err == nil {
			accepted = ok
		}
	}
	return &accepted
}

// Effective Python requirements of a plugin that plugin code cannot import, markers excluding this interpreter skipped
func missingPythonDependencies(ctx context.Context, mf *Manifest) []string {
	var out []string
	for _, req := range declaredRequirements(mf) {
		if installed := distInstalled(req, nil); installed != nil && !*installed && appliesHere(ctx, req) {
			out = append(out, req)
		}
	}
	return out
}

// Requirements PM installs for a plugin, read the way admission reads its declaration
func declaredRequirements(mf *Manifest) []string {
	if mf.Path == "" || !isDir(mf.Path) {
		return append([]string(nil), mf.PythonDeps...)
	}
	declaration, err := pm.ReadPythonDeclaration(mf.Path)
	if err != nil {
		return nil
	}
	reqs, err := declaration.InstallRequirements()
	if err != nil {
		return nil
	}
	return reqs
}

var loaderMarkerEnv struct {
	sync.Mutex
	python string
	env    map[string]string
}

// Prints packaging's default marker environment as computed by the probed interpreter itself
const loaderMarkerProbe = `
import json, os, platform, sys
v = sys.implementation.version
level = "" if v.releaselevel == "final" else f"{v.releaselevel[0]}{v.serial}"
print(json.dumps({
    "implementation_name": sys.implementation.name,
    "implementation_version": f"{v.major}.{v.minor}.{v.micro}{level}",
    "os_name": os.name,
    "platform_machine": platform.machine(),
    "platform_release": platform.release(),
    "platform_system": platform.system(),
    "platform_version": platform.version(),
    "python_full_version": platform.python_version(),
    "platform_python_implementation": platform.python_implementation(),
    "python_version": ".".join(platform.python_version_tuple()[:2]),
    "sys_platform": sys.platform,
}))
`

// Interpreter that runs plugin code, the selected environment's Python or the pinned toolchain one
func pluginInterpreter(ctx context.Context) (string, error) {
	venv, err := pm.SelectedVenv(ctx, pm.RepoRoot())
	if err != nil {
		return "", err
	}
	python := pm.VenvPython(venv)
	if isFile(python) {
		return python, nil
	}
	_, pinned, found, err := pm.Toolchain(ctx, false, false)
	if err != nil {
		return "", err
	}
	if !found {
		return "", &FileNotFoundError{Msg: "no Python interpreter is installed to judge plugin requirement markers"}
	}
	return pinned, nil
}

// Marker environment of the plugin interpreter, probed once per interpreter path
func pluginMarkerEnvironment(ctx context.Context) (map[string]string, error) {
	python, err := pluginInterpreter(ctx)
	if err != nil {
		return nil, err
	}
	loaderMarkerEnv.Lock()
	defer loaderMarkerEnv.Unlock()
	if loaderMarkerEnv.python == python && loaderMarkerEnv.env != nil {
		return loaderMarkerEnv.env, nil
	}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, pm.Native(python), "-I", "-c", loaderMarkerProbe)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	if err := json.Unmarshal(out, &env); err != nil {
		return nil, err
	}
	loaderMarkerEnv.python, loaderMarkerEnv.env = python, env
	return env, nil
}

// Whether a requirement's environment marker admits the plugin interpreter, unparseable markers counting as applying
func appliesHere(ctx context.Context, req string) bool {
	if !strings.Contains(req, ";") {
		return true
	}
	parsed, err := pm.ParseRequirement(req)
	if err != nil || len(parsed.Marker) == 0 {
		return true
	}
	env, err := pluginMarkerEnvironment(ctx)
	if err != nil {
		return true
	}
	ok, err := parsed.MarkerHolds(env, nil)
	return err != nil || ok
}

// Whether every requirement is installed in the dependency environment PM currently selects
func installedInSelectedEnvironment(ctx context.Context, requirements []string) bool {
	venv, err := pm.SelectedVenv(ctx, pm.RepoRoot())
	if err != nil {
		return false
	}
	site, err := pm.SitePackages(venv)
	if err != nil || len(requirements) == 0 {
		return false
	}
	for _, req := range requirements {
		if installed := distInstalled(req, []string{site}); installed == nil || !*installed {
			return false
		}
	}
	return true
}
