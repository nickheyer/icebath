package plugins

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	xterm "github.com/charmbracelet/x/term"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Source no longer at the commit the install dialog reviewed
type PluginSourceMoved struct{ *PluginOperationError }

func (e *PluginSourceMoved) Unwrap() error { return e.PluginOperationError }

func newPluginSourceMoved(msg string) *PluginSourceMoved {
	return &PluginSourceMoved{&PluginOperationError{Msg: msg, FailureClass: "clone_failed"}}
}

// Security scan refusal carrying the scan result it was decided on
type PluginScanBlocked struct {
	*PluginOperationError
	ScanResult *skills.ScanResult
}

func (e *PluginScanBlocked) Unwrap() error { return e.PluginOperationError }

func newPluginScanBlocked(msg string, result *skills.ScanResult) *PluginScanBlocked {
	return &PluginScanBlocked{&PluginOperationError{Msg: msg, FailureClass: "scan_blocked"}, result}
}

// Whether stdin and stdout are both terminals
var isTTY = func() bool { return xterm.IsTerminal(os.Stdin.Fd()) && xterm.IsTerminal(os.Stdout.Fd()) }

var stdinLines = bufio.NewReader(os.Stdin)

// Writes the prompt to stdout and reads one line from stdin without its newline, io.EOF at end of input
var inputLine = func(prompt string) (string, error) {
	fmt.Fprint(os.Stdout, prompt)
	line, err := stdinLines.ReadString('\n')
	if err != nil && line == "" {
		return "", io.EOF
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

// One y or N question where end of input counts as no
func askYes(prompt string, reader func(prompt string) (string, error)) bool {
	if reader == nil {
		reader = inputLine
	}
	answer, err := reader(prompt)
	if err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

// Install and update time scanning, on unless plugins.scan_on_install is falsy
var scanOnInstallEnabled = func(h string) bool {
	return jsonTruthy(configValue(h, true, "plugins", "scan_on_install"))
}

// How a caution scan verdict may be accepted
type scanOptions struct {
	Force        bool
	ScanDecision func(scan any) bool
	ReviewedPin  bool
}

// Scans a tree and enforces the install policy, nil result when scanning is disabled
var scanPluginTree = func(h, pluginDir, identifier string, opts scanOptions) (*skills.ScanResult, error) {
	if !scanOnInstallEnabled(h) {
		return nil, nil
	}
	result := ScanPlugin(pluginDir, identifier)
	allowed, reason := ShouldAllowPluginInstall(result, opts.Force)
	if allowed == nil && opts.ReviewedPin {
		yes := true
		allowed, reason = &yes, "Caution verdict accepted: reviewed catalog pin"
	}
	if allowed == nil && opts.ScanDecision != nil && opts.ScanDecision(&result) {
		yes := true
		allowed, reason = &yes, "Caution verdict accepted by user"
	}
	if allowed == nil || !*allowed {
		return nil, newPluginScanBlocked(fmt.Sprintf("Security scan blocked plugin install: %s\n\n%s\n"+
			"Review the findings above. Install only plugins from sources you trust. (Scanning can be configured via "+
			"plugins.scan_on_install in config.yaml.)", reason, FormatPluginScanReport(result)), &result)
	}
	logInfo(fmt.Sprintf("plugin scan passed for %s: %s", filepath.Base(pluginDir), reason))
	return &result, nil
}

// Scan block text for a tree that also holds user files carried from the installed copy
func preservedFilesNote(blocked *PluginScanBlocked, merged []string) string {
	preserved := toSet(merged)
	hitSet := map[string]bool{}
	if blocked.ScanResult != nil {
		for _, f := range blocked.ScanResult.Findings {
			if preserved[f.File] {
				hitSet[f.File] = true
			}
		}
	}
	note := "The scanned tree included user files preserved from the installed copy."
	if hits := sortedSet(hitSet); len(hits) > 0 {
		note = fmt.Sprintf("These findings come from user files preserved from the installed copy: %s. "+
			"Move or remove them and retry the update.", strings.Join(hits, ", "))
	}
	return blocked.Error() + "\n\n" + note
}

// Scan of a candidate tree whose block names the findings sitting in carried user files
func scanMergedTree(h, pluginDir, identifier string, merged []string, opts scanOptions) (*skills.ScanResult, error) {
	result, err := scanPluginTree(h, pluginDir, identifier, opts)
	var blocked *PluginScanBlocked
	if err != nil && len(merged) > 0 && errors.As(err, &blocked) {
		return nil, newPluginScanBlocked(preservedFilesNote(blocked, merged), blocked.ScanResult)
	}
	return result, err
}

var githubBrowserSegments = map[string]bool{
	"actions": true, "blob": true, "commit": true, "commits": true, "issues": true, "pull": true,
	"pulls": true, "releases": true, "tree": true, "wiki": true,
}

// Cloneable git URL and optional subdirectory of a URL or owner/repo identifier
func resolveGitURL(identifier string) (string, string, error) {
	if hasURLScheme(identifier) {
		if rest, ok := strings.CutPrefix(identifier, "https://github.com/"); ok {
			rest, _, _ = strings.Cut(rest, "?")
			rest, _, _ = strings.Cut(rest, "#")
			parts := strings.Split(strings.Trim(rest, "/"), "/")
			if len(parts) >= 3 && parts[0] != "" && parts[1] != "" && githubBrowserSegments[parts[2]] {
				subdir := ""
				if parts[2] == "tree" && len(parts) >= 5 {
					var kept []string
					for _, p := range parts[4:] {
						if p != "" {
							kept = append(kept, p)
						}
					}
					subdir = strings.Trim(strings.Join(kept, "/"), "/")
				}
				return "https://github.com/" + parts[0] + "/" + strings.TrimSuffix(parts[1], ".git") + ".git", subdir, nil
			}
		}
		if gitURL, frag, ok := strings.Cut(identifier, "#"); ok {
			return gitURL, strings.Trim(frag, "/"), nil
		}
		if gitURL, subdir, ok := strings.Cut(identifier, ".git/"); ok {
			return gitURL + ".git", strings.Trim(subdir, "/"), nil
		}
		return identifier, "", nil
	}
	identifier, fragment, _ := strings.Cut(identifier, "#")
	var parts []string
	for _, p := range strings.Split(strings.Trim(identifier, "/"), "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) >= 2 {
		subdir := strings.Trim(strings.Join(append(append([]string{}, parts[2:]...), strings.Split(fragment, "/")...), "/"), "/")
		return "https://github.com/" + parts[0] + "/" + parts[1] + ".git", subdir, nil
	}
	return "", "", fmt.Errorf("Invalid plugin identifier: '%s'. Use a Git URL or 'owner/repo' shorthand (optionally with a "+
		"subdirectory: 'owner/repo/path/to/plugin').", identifier)
}

// Subdirectory resolved inside a clone, refused when it escapes, is missing or is a file
func resolveSubdirWithin(cloneRoot, subdir string) (string, error) {
	root := resolvePath(cloneRoot)
	joined := subdir
	if !filepath.IsAbs(subdir) {
		joined = filepath.Join(root, subdir)
	}
	candidate := resolvePath(joined)
	if candidate != root && !strings.HasPrefix(candidate, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator)) {
		return "", &PluginOperationError{Msg: fmt.Sprintf("Plugin subdirectory '%s' escapes the repository.", subdir), FailureClass: "invalid_source"}
	}
	st, err := os.Stat(candidate)
	if err != nil {
		return "", &PluginOperationError{Msg: fmt.Sprintf("Plugin subdirectory '%s' does not exist in the repository.", subdir), FailureClass: "invalid_source"}
	}
	if !st.IsDir() {
		return "", &PluginOperationError{Msg: fmt.Sprintf("Plugin subdirectory '%s' is not a directory.", subdir), FailureClass: "invalid_source"}
	}
	return candidate, nil
}

// Repository name of a git URL, the last path part with an ssh host prefix dropped
func repoNameFromURL(url string) string {
	name := strings.TrimSuffix(strings.TrimRight(url, "/"), ".git")
	name = name[strings.LastIndex(name, "/")+1:]
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name = name[i+1:]
		name = name[strings.LastIndex(name, "/")+1:]
	}
	return name
}

// Native manifest path, empty when absent, an invalid manifest refusal when ambiguous
func nativeManifestFile(pluginDir string) (string, error) {
	path, err := pm.NativeManifestFile(pluginDir)
	if err != nil {
		return "", &PluginOperationError{Msg: err.Error(), FailureClass: "manifest_invalid"}
	}
	return path, nil
}

// Native or portable manifest preferring native YAML, empty with a warning when unreadable
func readManifest(pluginDir string) (map[string]any, error) {
	native, err := nativeManifestFile(pluginDir)
	if err != nil {
		return nil, err
	}
	if native == "" {
		if !hasPortableManifest(pluginDir) {
			return map[string]any{}, nil
		}
		manifest, _, err := ReadAgentPluginManifest(pluginDir)
		if err != nil {
			logWarn(fmt.Sprintf("Failed to read plugin.json in %s: %v", pluginDir, err))
			return map[string]any{}, nil
		}
		return manifest, nil
	}
	manifest, err := pm.ReadNativeManifest(native)
	if err != nil {
		logWarn(fmt.Sprintf("Failed to read plugin.yaml in %s: %v", pluginDir, err))
		return map[string]any{}, nil
	}
	if manifest == nil {
		return map[string]any{}, nil
	}
	return manifest, nil
}

// Whether a dir has a native or portable manifest
func looksLikePluginDir(target string) (bool, error) {
	native, err := nativeManifestFile(target)
	if err != nil {
		return false, err
	}
	if native != "" {
		return true, nil
	}
	_, err = os.Stat(filepath.Join(target, "plugin.json"))
	return err == nil, nil
}

// Copies each example file to its real name without overwriting an existing file
func copyExampleFiles(pluginDir string, console *Console) {
	examples, _ := filepath.Glob(filepath.Join(pluginDir, "*.example"))
	for _, example := range examples {
		base := filepath.Base(example)
		realName := strings.TrimSuffix(base, ".example")
		if realName == "" {
			realName = base
		}
		realPath := filepath.Join(pluginDir, realName)
		if _, err := os.Lstat(realPath); err == nil {
			continue
		}
		if err := pyfs.Copy2(example, realPath); err != nil {
			console.Print(fmt.Sprintf("[yellow]Warning:[/yellow] Failed to copy %s: %s", base, pyfs.OSErrorText(err)))
			continue
		}
		console.Print(fmt.Sprintf("[dim]  Created %s from %s[/dim]", realName, base))
	}
}

// Declared requires_env entries normalised to mappings whose variable is unset for the home
func missingEnvSpecs(ctx context.Context, h string, manifest map[string]any) ([]map[string]any, error) {
	entries, _ := manifest["requires_env"].([]any)
	var specs []map[string]any
	for _, entry := range entries {
		switch v := entry.(type) {
		case string:
			specs = append(specs, map[string]any{"name": v})
		case map[string]any:
			if jsonTruthy(v["name"]) {
				specs = append(specs, v)
			}
		}
	}
	missing := []map[string]any{}
	for _, spec := range specs {
		value, _, err := config.GetEnvValue(ctx, h, pyrepr.Str(spec["name"]))
		if err != nil {
			return nil, err
		}
		if value == "" {
			missing = append(missing, spec)
		}
	}
	return missing, nil
}

// Explicit deny list that wins over the allow list
func getDisabledSet(h string) map[string]bool { return configNameSet(h, "plugins", "disabled") }

// Allow list of enabled plugin keys
func getEnabledSet(h string) map[string]bool { return configNameSet(h, "plugins", "enabled") }

// Digest of the home config the admission compares against, missing when absent
func pluginSelectionVersion(h string) (string, error) {
	digest, ok, err := pm.FileDigest(filepath.Join(h, "config.yaml"))
	if err != nil {
		return "", err
	}
	if !ok {
		return "missing", nil
	}
	return digest, nil
}

// Proposed selection change and where a refusal is reported
type admitSetOptions struct {
	ExtraDirs      []string
	Console        *Console
	Action         string
	ExpectedConfig string
	Plugin         string
	Settled        []string
}

// Admits proposed enabled and disabled sets through the one PM transaction, reporting a refusal
func admitAndSavePluginSets(ctx context.Context, h string, enabled, disabled map[string]bool, opts admitSetOptions) error {
	dir, err := pluginsDir(h)
	if err != nil {
		return err
	}
	action := opts.Action
	if action == "" {
		action = "enable"
	}
	err = admitPluginSetChange(ctx, enabled, disabled, admitOptions{ActivePluginsDir: dir, ExtraDirs: opts.ExtraDirs,
		ExpectedConfig: opts.ExpectedConfig, Plugin: opts.Plugin, Settled: opts.Settled})
	var conflict *DependencyConflict
	var refused *AdmissionRefused
	switch {
	case err == nil || opts.Console == nil:
	case errors.As(err, &conflict):
		opts.Console.Print("[red]✗[/red] " + pyrich.Escape(conflict.Error()))
		opts.Console.Print("[dim]config.yaml and the active environment are unchanged.[/dim]")
	case errors.As(err, &refused):
		opts.Console.Print(fmt.Sprintf("[red]✗[/red] %s refused: %s", action, refused.Error()))
		opts.Console.Print("[dim]config.yaml and the active environment are unchanged. " +
			"Run `icebath pm install` to resolve dependencies, then retry.[/dim]")
	}
	return err
}
