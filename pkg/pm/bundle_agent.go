package pm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nickheyer/icebath/pkg/internal/fnmatch"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Payload relative destinations of the planted frontend products
var frontendDestinations = map[string]string{
	"tui": "cmd/icebath/tui_dist", "web": "cmd/icebath/web_dist", "webapp": "apps/desktop/dist-webapp",
}

// Copies a file or tree into destination, links kept and caches skipped, refusing nested input and output
func copyInput(source, destination string, replace bool) error {
	resolvedSource, resolvedDestination := resolvePath(source), resolvePath(destination)
	if resolvedSource == resolvedDestination {
		return nil
	}
	if isRelativeTo(resolvedDestination, resolvedSource) || isRelativeTo(resolvedSource, resolvedDestination) {
		return &PyError{Class: "ValueError", Message: "output must not be inside copied input: " + source}
	}
	if replace && pathExists(destination) {
		if err := pyfs.RmTree(destination, nil); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o777); err != nil {
		return err
	}
	if isDirectory(source) {
		return pyfs.CopyTree(source, destination, true, true, func(dir string, names []string) map[string]bool {
			skipped := map[string]bool{}
			for _, name := range names {
				for _, pattern := range []string{"__pycache__", ".git"} {
					if fnmatch.Fnmatch(name, pattern) {
						skipped[name] = true
					}
				}
			}
			return skipped
		})
	}
	return pyfs.Copy2(source, destination)
}

// Plants only the explicit built frontend products into the repo snapshot
func plantSurfaces(repo string, frontends map[string]string) error {
	if err := validateFrontends(frontends); err != nil {
		return err
	}
	if tui, ok := frontends["tui"]; ok {
		destination := filepath.Join(repo, filepath.FromSlash(frontendDestinations["tui"]))
		if err := copyInput(filepath.Join(tui, "dist"), destination, true); err != nil {
			return err
		}
		if err := copyInput(filepath.Join(tui, "package.json"), filepath.Join(destination, "package.json"), false); err != nil {
			return err
		}
	}
	for _, name := range []string{"web", "webapp"} {
		if source, ok := frontends[name]; ok {
			if err := copyInput(source, filepath.Join(repo, filepath.FromSlash(frontendDestinations[name])), true); err != nil {
				return err
			}
		}
	}
	return nil
}

// Table value of a TOML document key, an empty table when absent
func tomlTable(table *OrderedObject, key string) (*OrderedObject, error) {
	value, ok := table.Get(key)
	if !ok {
		return NewOrderedObject(), nil
	}
	sub, ok := value.(*OrderedObject)
	if !ok {
		return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("%s object has no attribute 'items'", pyrepr.StrRepr(tomlTypeName(value)))}
	}
	return sub, nil
}

// Writes the static PEP 621 metadata and entry points of a source layout install beside its code
func writeMetadata(projectPath, repo string) error {
	project, err := loadProjectTable(projectPath)
	if err != nil {
		return err
	}
	if dynamic, ok := project.Get("dynamic"); ok && pyrepr.Truthy(dynamic) {
		return &PyError{Class: "ValueError", Message: "agent assembly requires static project metadata"}
	}
	field := func(key string) (string, error) {
		value, ok := project.Get(key)
		if !ok {
			return "", &PyError{Class: "KeyError", Message: pyrepr.StrRepr(key)}
		}
		return pyrepr.Str(value), nil
	}
	name, err := field("name")
	if err != nil {
		return err
	}
	version, err := field("version")
	if err != nil {
		return err
	}
	info := filepath.Join(repo, fmt.Sprintf("%s-%s.dist-info", projectDistSeparators.ReplaceAllString(name, "_"), version))
	if err := os.MkdirAll(info, 0o777); err != nil {
		return err
	}
	lines := []string{"Metadata-Version: 2.3", "Name: " + name, "Version: " + version}
	for _, pair := range [][2]string{{"description", "Summary"}, {"requires-python", "Requires-Python"}} {
		if value, ok := project.Get(pair[0]); ok {
			lines = append(lines, pair[1]+": "+pyrepr.Str(value))
		}
	}
	list := func(value any) ([]any, error) {
		items, ok := value.([]any)
		if !ok {
			return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("%s object is not iterable", pyrepr.StrRepr(tomlTypeName(value)))}
		}
		return items, nil
	}
	if value, ok := project.Get("dependencies"); ok {
		items, err := list(value)
		if err != nil {
			return err
		}
		for _, req := range items {
			lines = append(lines, "Requires-Dist: "+pyrepr.Str(req))
		}
	}
	optional, err := tomlTable(project, "optional-dependencies")
	if err != nil {
		return err
	}
	for _, extraName := range optional.Keys {
		extra := strings.ToLower(projectDistSeparators.ReplaceAllString(extraName, "-"))
		lines = append(lines, "Provides-Extra: "+extra)
		items, err := list(optional.Values[extraName])
		if err != nil {
			return err
		}
		for _, item := range items {
			requirement, marker, _ := strings.Cut(pyrepr.Str(item), ";")
			condition := ""
			if marker != "" {
				condition = "(" + strings.TrimSpace(marker) + ") and "
			}
			lines = append(lines, fmt.Sprintf("Requires-Dist: %s; %sextra == '%s'", strings.TrimSpace(requirement), condition, extra))
		}
	}
	if err := writeTextFile(filepath.Join(info, "METADATA"), strings.Join(lines, "\n")+"\n"); err != nil {
		return err
	}
	declared, err := tomlTable(project, "entry-points")
	if err != nil {
		return err
	}
	groups := NewOrderedObject()
	for _, group := range declared.Keys {
		groups.Set(group, declared.Values[group])
	}
	entries, err := projectEntries(projectPath)
	if err != nil {
		return err
	}
	groups.Set("console_scripts", entries)
	if gui, ok := project.Get("gui-scripts"); ok && pyrepr.Truthy(gui) {
		groups.Set("gui_scripts", gui)
	}
	var sections []string
	for _, group := range groups.Keys {
		values, ok := groups.Values[group].(*OrderedObject)
		if !ok {
			return &PyError{Class: "AttributeError", Message: fmt.Sprintf("%s object has no attribute 'items'", pyrepr.StrRepr(tomlTypeName(groups.Values[group])))}
		}
		var rows []string
		for _, name := range values.Keys {
			rows = append(rows, name+" = "+pyrepr.Str(values.Values[name]))
		}
		sections = append(sections, "["+group+"]\n"+strings.Join(rows, "\n"))
	}
	return writeTextFile(filepath.Join(info, "entry_points.txt"), strings.Join(sections, "\n")+"\n")
}

// Absolute form of a path the way Path.absolute renders it, dot dot components kept
func pathAbsolute(path string) (string, error) {
	if filepath.IsAbs(path) {
		return pypath.PathStr(path), nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return pypath.PathStr(cwd + string(filepath.Separator) + path), nil
}

// JSON object holding an ordered object's string valued entries in order
func orderedJSONObject(o *OrderedObject) *pyjson.Object {
	obj := pyjson.NewObject()
	for _, key := range o.Keys {
		obj.Set(key, o.Values[key])
	}
	return obj
}

// Writes value as indented JSON text with a trailing newline
func writeJSONText(path string, value any) error {
	encoded, err := pyjson.Dumps(value, pyjson.Indent(2))
	if err != nil {
		return err
	}
	return writeTextFile(path, encoded+"\n")
}

// Removes a file the way unlink with missing_ok does, refusing a real directory
func unlinkMissingOK(path string) error {
	if info, err := os.Lstat(path); err == nil && info.IsDir() {
		return &os.PathError{Op: "unlink", Path: path, Err: syscall.EISDIR}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Assembles a runnable agent from prepared inputs into out and writes its completion manifest
func assembleAgent(ctx context.Context, inputs AgentInputs, out string) error {
	_, err := assembleAgentManifest(ctx, inputs, out)
	return err
}

// Assembles the agent and returns the manifest it published
func assembleAgentManifest(ctx context.Context, inputs AgentInputs, out string) (*pyjson.Object, error) {
	out, err := pathAbsolute(out)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(out, 0o777); err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(out, "manifest.json")
	var previous any = pyjson.NewObject()
	if isRegularFile(manifestPath) {
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			return nil, err
		}
		text, err := decodeUTF8Sig(raw)
		if err != nil {
			return nil, err
		}
		if previous, err = pyjson.Loads(text); err != nil {
			return nil, &PyError{Class: "JSONDecodeError", Message: err.Error()}
		}
	}
	for _, stale := range []string{manifestPath, filepath.Join(out, "command-map.json")} {
		if err := unlinkMissingOK(stale); err != nil {
			return nil, err
		}
	}
	if err := inputs.validate(out); err != nil {
		return nil, err
	}
	entries, err := projectEntries(inputs.Project)
	if err != nil {
		return nil, err
	}
	repo := pypath.PathStr(filepath.Join(out, inputs.Repo))
	referenced := inputs.Placement == "references"
	if err := os.MkdirAll(repo, 0o777); err != nil {
		return nil, err
	}
	if !referenced {
		if resolvePath(inputs.Code) != resolvePath(repo) {
			if repo == out {
				return nil, &PyError{Class: "ValueError", Message: "root-level code must be prepared in place"}
			}
			if err := copyInput(inputs.Code, repo, true); err != nil {
				return nil, err
			}
		}
		for _, name := range orderedKeys(inputs.Resources, resourceEnvNames()) {
			if err := copyInput(inputs.Resources[name], filepath.Join(repo, name), true); err != nil {
				return nil, err
			}
		}
		if err := plantSurfaces(repo, inputs.Frontends); err != nil {
			return nil, err
		}
		if err := writeMetadata(inputs.Project, repo); err != nil {
			return nil, err
		}
		if !isRelativeTo(resolvePath(inputs.SitePackages), out) {
			return nil, &PyError{Class: "ValueError", Message: "source binding requires an output-owned dependency directory"}
		}
		relativeCode, err := filepath.Rel(inputs.SitePackages, repo)
		if err != nil {
			return nil, err
		}
		if err := writeTextFile(filepath.Join(inputs.SitePackages, "icebath-agent.pth"), filepath.ToSlash(relativeCode)+"\n"); err != nil {
			return nil, err
		}
	}
	for _, pair := range [][2]string{{inputs.Stamp, filepath.Join(repo, "install-stamp.json")}, {inputs.Features, filepath.Join(out, "enabled-features.json")}} {
		if pair[0] != "" {
			if err := copyInput(pair[0], pair[1], false); err != nil {
				return nil, err
			}
		}
	}
	if inputs.Placement == "contained" {
		if err := relativizeLinks(out); err != nil {
			return nil, err
		}
	}
	location := func(path string) string {
		if pathIsRelativeTo(path, out) && !referenced {
			return preparedName(path, out)
		}
		return pypath.PathStr(path)
	}
	python, site := location(inputs.Python), location(inputs.SitePackages)
	tools := "tools"
	if inputs.Tools != "" {
		tools = location(inputs.Tools)
	}
	var commands *OrderedObject
	if referenced {
		if commands, err = writeCommandMap(inputs, entries, out, repo); err != nil {
			return nil, err
		}
	} else {
		if err := retirePreviousLaunchers(previous, entries, inputs, out); err != nil {
			return nil, err
		}
		launchPython := python
		if strings.HasSuffix(inputs.Target, "-bionic") {
			launchPython = location(inputs.Environment) + "/bin/python"
		}
		if commands, err = writeLaunchers(ctx, out, entries, launchPython, inputs.Repo, site, inputs.Target, inputs.binDir()); err != nil {
			return nil, err
		}
	}
	launchers := make([]any, len(entries.Keys))
	for i, name := range entries.Keys {
		launchers[i] = name
	}
	runtimeRecord := pyjson.NewObject()
	if referenced {
		runtimeRecord.Set("repoDir", pypath.PathStr(inputs.Code))
	} else {
		runtimeRecord.Set("repoDir", inputs.Repo)
	}
	runtimeRecord.Set("toolsDir", tools)
	runtimeRecord.Set("storePython", python)
	runtimeRecord.Set("sitePackages", site)
	runtimeRecord.Set("commands", orderedJSONObject(commands))
	manifest := pyjson.NewObject()
	manifest.Set("schema", 1)
	manifest.Set("target", inputs.Target)
	manifest.Set("repo", inputs.Repo)
	manifest.Set("venv", location(inputs.Environment))
	manifest.Set("store", tools)
	manifest.Set("launchers", launchers)
	manifest.Set("runtime", runtimeRecord)
	if inputs.Ref != nil {
		manifest.Set("ref", *inputs.Ref)
	}
	return manifest, writeJSONText(manifestPath, manifest)
}

// Links referenced resources and frontends into the output and writes the derived command map
func writeCommandMap(inputs AgentInputs, entries *OrderedObject, out, repo string) (*OrderedObject, error) {
	commands := NewOrderedObject()
	for _, name := range entries.Keys {
		commands.Set(name, pypath.PathStr(filepath.Join(inputs.CommandDir, name)))
	}
	for _, name := range commands.Keys {
		if command := commands.Values[name].(string); !isRegularFile(command) {
			return nil, &PyError{Class: "FileNotFoundError", Message: "prepared command missing: " + command}
		}
	}
	env := pyjson.NewObject()
	if inputs.Env != nil {
		for _, key := range inputs.Env.Keys {
			env.Set(key, inputs.Env.Vals[key])
		}
	}
	env.Set("ICEBATH_PYTHON", pypath.PathStr(inputs.Python))
	env.Set("ICEBATH_INSTALL_ROOT", repo)
	type link struct{ key, source, destination string }
	var links []link
	for _, name := range orderedKeys(inputs.Resources, resourceEnvNames()) {
		links = append(links, link{resourceEnvVar(name), inputs.Resources[name], filepath.Join(repo, name)})
	}
	frontendLinks := map[string][2]string{
		"tui": {"ICEBATH_TUI_DIR", filepath.Join(out, "ui-tui")}, "web": {"ICEBATH_WEB_DIST", filepath.Join(repo, "web_dist")},
		"webapp": {"ICEBATH_WEBAPP_DIST", filepath.Join(repo, "webapp_dist")},
	}
	for _, name := range orderedKeys(inputs.Frontends, frontendNames) {
		links = append(links, link{frontendLinks[name][0], inputs.Frontends[name], frontendLinks[name][1]})
	}
	for _, l := range links {
		if err := unlinkMissingOK(l.destination); err != nil {
			return nil, err
		}
		if err := os.Symlink(l.source, l.destination); err != nil {
			return nil, err
		}
		env.Set(l.key, l.destination)
	}
	mapped := pyjson.NewObject()
	for _, name := range entries.Keys {
		command := pyjson.NewObject()
		command.Set("source", commands.Values[name])
		command.Set("destination", inputs.binDir()+"/"+name)
		command.Set("entry", entries.Values[name])
		mapped.Set(name, command)
	}
	mapping := pyjson.NewObject()
	mapping.Set("commands", mapped)
	mapping.Set("env", env)
	return commands, writeJSONText(filepath.Join(out, "command-map.json"), mapping)
}

// Removes launchers the previous manifest minted for commands the project no longer declares
func retirePreviousLaunchers(previous any, entries *OrderedObject, inputs AgentInputs, out string) error {
	get := func(value any, key string) (any, error) {
		obj, ok := value.(*pyjson.Object)
		if !ok {
			return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("%s object has no attribute 'get'", pyrepr.StrRepr(pyrepr.TypeName(value)))}
		}
		if v, ok := obj.Get(key); ok {
			return v, nil
		}
		return pyjson.NewObject(), nil
	}
	runtimeRecord, err := get(previous, "runtime")
	if err != nil {
		return err
	}
	recorded, err := get(runtimeRecord, "commands")
	if err != nil {
		return err
	}
	commands, ok := recorded.(*pyjson.Object)
	if !ok {
		return &PyError{Class: "AttributeError", Message: fmt.Sprintf("%s object has no attribute 'items'", pyrepr.StrRepr(pyrepr.TypeName(recorded)))}
	}
	suffix := ""
	if strings.HasPrefix(inputs.Target, "win32") {
		suffix = ".exe"
	}
	for _, name := range commands.Keys {
		if _, declared := entries.Get(name); declared {
			continue
		}
		if command, ok := commands.Vals[name].(string); ok && command == inputs.binDir()+"/"+name+suffix {
			if err := unlinkMissingOK(filepath.Join(out, filepath.FromSlash(command))); err != nil {
				return err
			}
		}
	}
	return nil
}

// Assembles from an inputs document, clearing any completion claim when assembly fails
func AgentMain(ctx context.Context, inputsPath, out string) error {
	err := func() error {
		raw, err := os.ReadFile(inputsPath)
		if err != nil {
			return err
		}
		text, err := decodeUTF8Sig(raw)
		if err != nil {
			return err
		}
		document, err := pyjson.Loads(text)
		if err != nil {
			return &PyError{Class: "JSONDecodeError", Message: err.Error()}
		}
		inputs, err := agentInputsFromDict(document)
		if err != nil {
			return err
		}
		return assembleAgent(ctx, inputs, out)
	}()
	if err != nil {
		for _, stale := range []string{"manifest.json", "command-map.json"} {
			if cleanup := unlinkMissingOK(filepath.Join(out, stale)); cleanup != nil {
				return cleanup
			}
		}
	}
	return err
}
