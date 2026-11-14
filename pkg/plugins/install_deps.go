package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Refusal reason of a dependency consent with the closed install failure class it reports
type ConsentRefusal struct{ Text, FailureClass string }

func (r *ConsentRefusal) Error() string { return r.Text }

// Consent gate for the Node and Python dependencies a plugin declares, nothing installed on a decline
func installPluginPythonDeps(ctx context.Context, manifest map[string]any, target string, console *Console, assumeYes bool) (bool, *ConsentRefusal) {
	declaration, err := pm.ReadPythonDeclaration(target)
	var deps []string
	hasPython := false
	if err == nil {
		deps, err = declaration.InstallRequirements()
	}
	if err == nil {
		hasPython, err = declaration.InstallsPackages()
	}
	if err != nil {
		return false, &ConsentRefusal{"invalid Python dependency declaration: " + pyrepr.ExceptionMessage(err), "manifest_invalid"}
	}
	hasPackageJSON := isFile(filepath.Join(target, "package.json"))
	if !hasPython && !hasPackageJSON {
		return true, nil
	}
	name := manifestText(manifest, "name", "this plugin")
	if hasPackageJSON {
		console.Print(fmt.Sprintf("\n[bold]%s[/bold] declares Node dependencies (package.json).", name))
		if isTTY() && askYesNo(ctx, []any{"node", manifest["name"]}, "  Install them into the plugin's own node_modules now? [y/N]: ", console) {
			if reason := pm.InstallNodeSidecar(ctx, target, true); reason != "" {
				console.Print("[yellow]⚠[/yellow] Node deps: " + reason)
			}
		} else {
			console.Print("[dim]Skipped Node deps — run `icebath plugins install` again to retry.[/dim]\n")
		}
	}
	if !hasPython {
		return true, nil
	}
	return consentPythonDeps(ctx, name, deps, console, assumeYes)
}

// The y or N gate every Python dependency passes before it enters the shared environment
func consentPythonDeps(ctx context.Context, pluginName string, deps []string, console *Console, assumeYes bool) (bool, *ConsentRefusal) {
	console.Print(fmt.Sprintf("\n[bold]%s[/bold] declares Python dependencies:", pluginName))
	if len(deps) > 0 {
		for _, dep := range deps {
			console.Print("  - " + dep)
		}
	} else {
		console.Print("  - (declared in its pyproject.toml)")
	}
	if assumeYes {
		console.Print("[dim]Consent given up front — preparing them without prompting.[/dim]\n")
		return true, nil
	}
	if !isTTY() {
		console.Print("[dim]Non-interactive install — skipping dependency install. " +
			"Run `icebath plugins enable` when ready to prepare them.[/dim]\n")
		return false, &ConsentRefusal{"dependency install skipped (non-interactive)", "non_interactive"}
	}
	key := []any{"python", pluginName, anyList(deps)}
	if !askYesNo(ctx, key, "  Prepare these with Icebath through PM now? [y/N]: ", console) {
		console.Print("[dim]Skipped — run `icebath plugins enable` when ready " +
			"to prepare them.[/dim]\n")
		return false, &ConsentRefusal{"dependency install declined", "deps_declined"}
	}
	return true, nil
}

// Python requirements of an installed tree for dashboard details, a read failure added to the warnings
func pythonDependencySummary(target string, warnings *[]string) []string {
	declaration, err := pm.ReadPythonDeclaration(target)
	var deps []string
	if err == nil {
		deps, err = declaration.InstallRequirements()
	}
	if err != nil {
		*warnings = append(*warnings, "Could not read Python dependencies: "+pyrepr.ExceptionMessage(err))
		return []string{}
	}
	return append([]string{}, deps...)
}

// Refreshes a rebuilt Node sidecar and gates newly declared Python requirements of an update
func refreshDeclaredDependencies(ctx context.Context, target, staged string, manifest map[string]any, interactive bool) error {
	if isDirPath(filepath.Join(target, "node_modules")) && isFilePath(filepath.Join(staged, "package.json")) {
		moved := !isDirPath(filepath.Join(staged, "node_modules"))
		for _, name := range []string{"package.json", "package-lock.json"} {
			moved = moved || string(bytesOrNil(filepath.Join(target, name))) != string(bytesOrNil(filepath.Join(staged, name))) ||
				(bytesOrNil(filepath.Join(target, name)) == nil) != (bytesOrNil(filepath.Join(staged, name)) == nil)
		}
		if moved {
			if reason := pm.InstallNodeSidecar(ctx, staged, true); reason != "" {
				return &PluginOperationError{Msg: fmt.Sprintf("Node dependencies could not be refreshed: %s. "+
					"The installed plugin and active environment are unchanged.", reason)}
			}
		}
	}
	active, err := activePluginTree(ctx, target, true)
	if err != nil || !active {
		return err
	}
	before, err := pm.ReadPythonDeclaration(target)
	if err != nil {
		return err
	}
	after, err := pm.ReadPythonDeclaration(staged)
	if err != nil {
		return err
	}
	had, err := before.InstallRequirements()
	if err != nil {
		return err
	}
	wants, err := after.InstallRequirements()
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, spec := range had {
		known[spec] = true
	}
	var added []string
	for _, spec := range wants {
		if !known[spec] {
			added = append(added, spec)
		}
	}
	installsNow, err := after.InstallsPackages()
	if err != nil {
		return err
	}
	installedBefore, err := before.InstallsPackages()
	if err != nil {
		return err
	}
	if len(added) == 0 && !(installsNow && !installedBefore) {
		return nil
	}
	if !interactive {
		listed := strings.Join(added, ", ")
		if listed == "" {
			listed = "in its pyproject.toml"
		}
		return &PluginOperationError{Msg: fmt.Sprintf("The update declares new Python dependencies (%s); "+
			"run `icebath plugins update %s` in a terminal to review them.", listed, filepath.Base(target))}
	}
	consented, reason := consentPythonDeps(ctx, manifestText(manifest, "name", filepath.Base(target)), added, NewConsole(os.Stdout), false)
	if !consented {
		return &PluginOperationError{Msg: fmt.Sprintf("Update declined: %s. The installed plugin and active environment are unchanged.", reason.Text)}
	}
	return nil
}
