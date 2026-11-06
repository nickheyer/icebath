package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/skills"
)

// Slow install step ids the desktop catalog card words in its own language
type InstallPhase string

const (
	InstallPhaseDownloading    InstallPhase = "downloading"
	InstallPhasePythonPackages InstallPhase = "python_packages"
	InstallPhaseLoadingTools   InstallPhase = "loading_tools"
)

type sharedAnswersKey struct{}

type sharedAnswers struct {
	mu      sync.Mutex
	answers map[string]bool
}

// Context in which each dependency question is asked once and repeats reuse the first answer
func WithSharedDependencyAnswers(ctx context.Context) context.Context {
	return context.WithValue(ctx, sharedAnswersKey{}, &sharedAnswers{answers: map[string]bool{}})
}

// Asks one y or N dependency question, reusing a shared answer for the same key
func askYesNo(ctx context.Context, key []any, prompt string, console *Console) bool {
	shared, _ := ctx.Value(sharedAnswersKey{}).(*sharedAnswers)
	id := pyrepr.Repr(key)
	if shared != nil {
		shared.mu.Lock()
		answer, ok := shared.answers[id]
		shared.mu.Unlock()
		if ok {
			word := "no"
			if answer {
				word = "yes"
			}
			console.Print("  [dim]Reusing your answer for the first profile: " + word + "[/dim]")
			return answer
		}
	}
	accepted := false
	if line, err := inputLine(prompt); err == nil {
		answer := strings.ToLower(strings.TrimSpace(line))
		accepted = answer == "y" || answer == "yes"
	}
	if shared != nil {
		shared.mu.Lock()
		shared.answers[id] = accepted
		shared.mu.Unlock()
	}
	return accepted
}

// Refusal reason with the closed extension install failure class publication reports
type consentRefusal struct {
	Text         string
	FailureClass string
}

// Manifest value at the key as text, the default when the key is absent
func manifestText(manifest map[string]any, key, def string) string {
	if v, ok := manifest[key]; ok {
		return pyrepr.Str(v)
	}
	return def
}

// Consent gate for declared Node and Python dependencies, never failing, refusals carry their reason
func installPluginPythonDeps(ctx context.Context, manifest map[string]any, target string, console *Console, assumeYes bool) (bool, *consentRefusal) {
	declaration, err := pm.ReadPythonDeclaration(target)
	var deps []string
	if err == nil {
		deps, err = declaration.InstallRequirements()
	}
	if err != nil {
		return false, &consentRefusal{Text: fmt.Sprintf("invalid Python dependency declaration: %v", err), FailureClass: "manifest_invalid"}
	}
	hasPython, err := declaration.InstallsPackages()
	if err != nil {
		return false, &consentRefusal{Text: fmt.Sprintf("invalid Python dependency declaration: %v", err), FailureClass: "manifest_invalid"}
	}
	hasPackageJSON := isFilePath(filepath.Join(target, "package.json"))
	if !hasPython && !hasPackageJSON {
		return true, nil
	}
	if hasPackageJSON {
		console.Print(fmt.Sprintf("\n[bold]%s[/bold] declares Node dependencies (package.json).", manifestText(manifest, "name", "this plugin")))
		nodeOK := isTTY() && askYesNo(ctx, []any{"node", manifest["name"]}, "  Install them into the plugin's own node_modules now? [y/N]: ", console)
		if nodeOK {
			if reason := installNodeSidecar(ctx, target, true); reason != "" {
				console.Print("[yellow]⚠[/yellow] Node deps: " + reason)
			}
		} else {
			console.Print("[dim]Skipped Node deps — run `icebath plugins install` again to retry.[/dim]\n")
		}
	}
	if !hasPython {
		return true, nil
	}
	return consentPythonDeps(ctx, manifestText(manifest, "name", "this plugin"), deps, console, assumeYes)
}

// The y or N gate for Python dependencies entering the shared environment
func consentPythonDeps(ctx context.Context, pluginName string, deps []string, console *Console, assumeYes bool) (bool, *consentRefusal) {
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
		return false, &consentRefusal{Text: "dependency install skipped (non-interactive)", FailureClass: "non_interactive"}
	}
	if !askYesNo(ctx, []any{"python", pluginName, stringsAny(deps)}, "  Prepare these with Icebath through PM now? [y/N]: ", console) {
		console.Print("[dim]Skipped — run `icebath plugins enable` when ready to prepare them.[/dim]\n")
		return false, &consentRefusal{Text: "dependency install declined", FailureClass: "deps_declined"}
	}
	return true, nil
}

// Declared Python requirements for the dashboard, a warning instead when unreadable
func pythonDependencySummary(target string, warnings *[]string) []string {
	declaration, err := pm.ReadPythonDeclaration(target)
	var deps []string
	if err == nil {
		deps, err = declaration.InstallRequirements()
	}
	if err != nil {
		*warnings = append(*warnings, fmt.Sprintf("Could not read Python dependencies: %v", err))
		return []string{}
	}
	return append([]string{}, deps...)
}

// Prompts for unset requires_env variables and saves each answer to the home env file
func promptPluginEnvVars(ctx context.Context, h string, manifest map[string]any, console *Console) error {
	missing, err := missingEnvSpecs(ctx, h, manifest)
	if err != nil || len(missing) == 0 {
		return err
	}
	shownHome := home.Display(h)
	console.Print(fmt.Sprintf("\n[bold]%s[/bold] requires the following environment variables:\n", manifestText(manifest, "name", "this plugin")))
	for _, spec := range missing {
		name := pyrepr.Str(spec["name"])
		line := "  " + name
		if desc := spec["description"]; jsonTruthy(desc) {
			line += " — " + pyrepr.Str(desc)
		}
		console.Print(line)
		if url := spec["url"]; jsonTruthy(url) {
			console.Print(fmt.Sprintf("  [dim]Get yours at: %s[/dim]", pyrepr.Str(url)))
		}
		read := inputLine
		if jsonTruthy(spec["secret"]) {
			svc := installedServices()
			if svc == nil || svc.MaskedSecretPrompt == nil {
				return &ServiceMissingError{Name: "MaskedSecretPrompt"}
			}
			read = svc.MaskedSecretPrompt
		}
		value, err := read("  " + name + ": ")
		if err != nil {
			console.Print(fmt.Sprintf("\n[dim]  Skipped (you can set these later in %s/.env)[/dim]", shownHome))
			return nil
		}
		if value = strings.TrimSpace(value); value != "" {
			if err := config.SaveEnvValue(h, name, value); err != nil {
				return err
			}
			os.Setenv(name, value)
			console.Print(fmt.Sprintf("  [green]✓[/green] Saved to %s/.env", shownHome))
		} else {
			console.Print(fmt.Sprintf("  [dim]  Skipped (set %s in %s/.env later)[/dim]", name, shownHome))
		}
	}
	console.Print("")
	return nil
}

// Refuses a manifest needing a newer manifest version or core than this installer
func checkManifestVersion(h string, manifest map[string]any, pluginName string) error {
	reason, err := pm.ManifestVersionError(manifest, pluginName)
	if err != nil || reason == "" {
		return err
	}
	update, err := config.RecommendedUpdateCommand(h)
	if err != nil {
		return err
	}
	return &PluginOperationError{Msg: fmt.Sprintf("%s Run %s to update Icebath.", reason, update), FailureClass: "incompatible"}
}

// Fails unless a dir can be listed or a file opened for reading
func probeReadable(path string) error {
	if isDirPath(path) {
		_, err := os.ReadDir(path)
		return err
	}
	f, err := os.Open(path)
	if err == nil {
		f.Close()
	}
	return err
}

// C library wording of an OS error, the errno text with its first letter raised
func osStrerror(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		text := []rune(errno.Error())
		if len(text) > 0 {
			text[0] = unicode.ToUpper(text[0])
		}
		return string(text)
	}
	return err.Error()
}

// Repairs or refuses a tree with entries the installer could not read back
func ensureTreeReadable(root, pluginsDir string) error {
	paths := []string{root}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if path != root && d != nil {
			paths = append(paths, path)
		}
		if err != nil && d != nil && d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
	for _, path := range paths {
		if probeReadable(path) == nil {
			continue
		}
		if runtime.GOOS != "windows" {
			if st, err := os.Stat(path); err == nil {
				add := os.FileMode(0o400)
				if st.IsDir() {
					add = 0o500
				}
				os.Chmod(path, st.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid|os.ModeSticky)|add)
			}
		}
		if err := probeReadable(path); err != nil {
			fix := "chmod -R u+rX " + pluginsDir
			if runtime.GOOS == "windows" {
				fix = fmt.Sprintf(`icacls "%s" /grant:r "%%USERNAME%%":(OI)(CI)F /T`, pluginsDir)
			}
			rel, _ := filepath.Rel(root, path)
			return &PluginOperationError{Msg: fmt.Sprintf("Installed file %s is not readable (%s); nothing was installed. "+
				"Fix permissions on %s (e.g. `%s`) and retry.", rel, osStrerror(err), pluginsDir, fix), FailureClass: "permission"}
		}
	}
	return nil
}

// Refuses a portable plugin whose app is unavailable here in the runtime surfaces' own sentence
func refuseUnavailablePortablePlugin(ctx context.Context, pluginName, tree, appTitle string) error {
	if !isFilePath(filepath.Join(tree, "plugin.json")) {
		return nil
	}
	pkg, err := LoadAgentPlugin(tree, filepath.Join(filepath.Dir(tree), ".icebath-install-data"))
	if err != nil {
		return &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' is unavailable: %v.", pluginName, err), FailureClass: "manifest_invalid"}
	}
	names := make([]string, 0, len(pkg.ServerDeclarations))
	for name := range pkg.ServerDeclarations {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		svc := installedServices()
		if svc == nil || svc.ServerAvailabilityReason == nil {
			return &ServiceMissingError{Name: "ServerAvailabilityReason"}
		}
		reason, offerable, err := svc.ServerAvailabilityReason(ctx, pkg.ServerDeclarations[name], appTitle)
		if err != nil {
			return err
		}
		if !offerable {
			return &PluginOperationError{Msg: fmt.Sprintf("Can't install plugin '%s': %s", pluginName, reason), FailureClass: "incompatible"}
		}
	}
	return nil
}

// Catalog known issues surfaced as warnings, never as a gate
func knownIssueWarnings(entry *PluginCatalogEntry) []string {
	out := []string{}
	for _, issue := range entry.KnownIssues {
		out = append(out, "Known issue: "+issue)
	}
	return out
}

// Terminal y or N accepting a caution verdict, shared by install and update
func InteractiveScanDecision(console *Console, verb string) func(scan any) bool {
	return func(scan any) bool {
		console.Print("")
		console.Print("[yellow]⚠ Security scan flagged this plugin:[/yellow]")
		if result, ok := scan.(*skills.ScanResult); ok && result != nil {
			console.Print(FormatPluginScanReport(*result))
		}
		return isTTY() && askYes(fmt.Sprintf("  %s anyway? Only continue if you trust the source. [y/N]: ", verb), nil)
	}
}

// Non interactive answer to a scan block with the verdict and findings a client can show
func ScanBlockedResult(blocked *PluginScanBlocked) map[string]any {
	verdict := "dangerous"
	findings := []any{}
	if blocked.ScanResult != nil {
		verdict = blocked.ScanResult.Verdict
		for _, f := range blocked.ScanResult.Findings {
			findings = append(findings, map[string]any{"pattern_id": f.PatternID, "severity": f.Severity, "category": f.Category,
				"file": f.File, "line": f.Line, "description": f.Description})
		}
	}
	return map[string]any{"ok": false, "error": blocked.Error(), "scan_blocked": true, "scan_verdict": verdict, "scan_findings": findings}
}

func prefix(s string, n int) string { return s[:min(n, len(s))] }

// Clones and returns the checked out sha, refusing a source that moved since the reviewed commit
func cloneAtReviewedCommit(ctx context.Context, h, tmpClone, gitURL, requestedRevision, inspectedSha, subdir string, catalogPin bool) (string, error) {
	if inspectedSha == "" {
		return clonePluginRepo(ctx, h, tmpClone, gitURL, requestedRevision, subdir)
	}
	sha, err := normalizeExactRevision(inspectedSha)
	if err != nil {
		return "", err
	}
	if !catalogPin && requestedRevision != "" && requestedRevision != sha {
		return "", &PluginOperationError{Msg: fmt.Sprintf("This plugin is pinned to commit %s, not the commit %s you reviewed. "+
			"Reinstall it with that pin, or re-open the install dialog.", prefix(requestedRevision, 12), prefix(sha, 12)), FailureClass: "already_installed"}
	}
	head, err := clonePluginRepo(ctx, h, tmpClone, gitURL, requestedRevision, subdir)
	if err != nil {
		return "", err
	}
	if gitExe := resolveGitExecutable(); gitExe != "" {
		sha = gitResolveCommit(ctx, tmpClone, gitExe, sha)
	}
	if head != sha {
		return "", newPluginSourceMoved(fmt.Sprintf("The source changed since you reviewed it: it is now at commit %s, not %s. "+
			"Re-open the install dialog to review the new version.", prefix(head, 12), prefix(sha, 12)))
	}
	return head, nil
}

// Install inputs, an empty Ref or ReviewedPin or InspectedSha means none was given
type installCoreOptions struct {
	Force             bool
	AcceptCaution     *bool
	Ref               string
	ScanDecision      func(scan any) bool
	ReviewedPin       string
	PythonDeps        bool
	AssumeDepsConsent bool
	Catalog           map[string]any
	AllowRemoved      bool
	BeforeSwap        func(manifest map[string]any, tree string) ([]string, error)
	AppTitle          string
	InspectedSha      string
}

// Clones, scans and atomically publishes a git plugin with its source and exact revision recorded
func installPluginCore(ctx context.Context, identifier string, opts installCoreOptions) (string, map[string]any, string, error) {
	h := home.Dir(ctx)
	requestedRevision := ""
	if opts.Ref != "" {
		var err error
		if requestedRevision, err = normalizeExactRevision(opts.Ref); err != nil {
			return "", nil, "", err
		}
	}
	gitURL, subdir, err := resolveGitURL(identifier)
	if err != nil {
		return "", nil, "", &PluginOperationError{Msg: err.Error(), FailureClass: "invalid_source"}
	}
	dir, err := pluginsDir(h)
	if err != nil {
		return "", nil, "", err
	}
	source, err := canonicalSource(gitURL, subdir)
	if err != nil {
		return "", nil, "", err
	}
	oldMetadata, err := readInstallMetadata(h)
	if err != nil {
		return "", nil, "", err
	}
	if requestedRevision == "" {
		var pins []map[string]any
		for _, v := range oldMetadata {
			if e, ok := v.(map[string]any); ok && e["source"] == source && e["pinned"] == true {
				pins = append(pins, e)
			}
		}
		if len(pins) == 1 {
			if revision, ok := pins[0]["revision"].(string); ok {
				if requestedRevision, err = normalizeExactRevision(revision); err != nil {
					return "", nil, "", err
				}
			}
		}
	}
	tmp, err := os.MkdirTemp(dir, ".install-")
	if err != nil {
		return "", nil, "", err
	}
	target, err := placeInstallClone(ctx, h, dir, filepath.Join(tmp, "plugin"), identifier, gitURL, subdir, source, requestedRevision, oldMetadata, opts)
	os.RemoveAll(tmp)
	if err != nil {
		return "", nil, "", err
	}
	if looks, err := looksLikePluginDir(target); err != nil {
		return "", nil, "", err
	} else if !looks {
		logWarn(fmt.Sprintf("%s has no plugin.yaml / __init__.py; may not be a valid plugin", filepath.Base(target)))
	}
	copyExampleFiles(target, NewConsole(os.Stdout))
	installed, err := readManifest(target)
	if err != nil {
		return "", nil, "", err
	}
	name := filepath.Base(target)
	if jsonTruthy(installed["name"]) {
		name = pyrepr.Str(installed["name"])
	}
	return target, installed, name, nil
}

// Clone, check, scan and publication of one install inside its temporary directory
func placeInstallClone(ctx context.Context, h, dir, tmpClone, identifier, gitURL, subdir, source, requestedRevision string,
	oldMetadata map[string]any, opts installCoreOptions) (string, error) {
	installedRevision, err := cloneAtReviewedCommit(ctx, h, tmpClone, gitURL, requestedRevision, opts.InspectedSha, subdir, opts.Catalog != nil)
	if err != nil {
		return "", err
	}
	atReviewedPin := false
	if opts.ReviewedPin != "" {
		pin := opts.ReviewedPin
		if gitExe := resolveGitExecutable(); gitExe != "" {
			pin = gitResolveCommit(ctx, tmpClone, gitExe, opts.ReviewedPin)
		}
		atReviewedPin = installedRevision == pin
	}
	tmpTarget := tmpClone
	if subdir != "" {
		if tmpTarget, err = resolveSubdirWithin(tmpClone, subdir); err != nil {
			return "", err
		}
	}
	if err := ensureTreeReadable(tmpTarget, dir); err != nil {
		return "", err
	}
	manifest, err := ReadManifestForInstall(tmpTarget)
	if err != nil {
		return "", err
	}
	pluginName := repoNameFromURL(gitURL)
	if jsonTruthy(manifest["name"]) {
		pluginName = pyrepr.Str(manifest["name"])
	} else if subdir != "" {
		trimmed := strings.TrimRight(subdir, "/")
		pluginName = trimmed[strings.LastIndex(trimmed, "/")+1:]
	}
	link := filepath.Join(dir, pluginName)
	if st, lerr := os.Lstat(link); !strings.Contains(pluginName, "/") && ((lerr == nil && st.Mode()&os.ModeSymlink != 0) || pm.IsJunction(link)) {
		return "", &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' is already installed outside the catalog: %s is a link to %s. "+
			"Delete that link to install the catalog version.", pluginName, link, resolvePath(link)), FailureClass: "already_installed"}
	}
	target, err := sanitizePluginName(pluginName, dir, false)
	if err != nil {
		return "", &PluginOperationError{Msg: err.Error(), FailureClass: "manifest_invalid"}
	}
	if err := checkManifestVersion(h, manifest, pluginName); err != nil {
		return "", err
	}
	prior, _ := oldMetadata[pluginName].(map[string]any)
	beforeSwap := opts.BeforeSwap
	var trackedEdits []string
	if st, lerr := os.Lstat(target); beforeSwap == nil && opts.Force && lerr == nil && st.IsDir() && prior != nil && prior["source"] == source {
		var local []string
		if local, trackedEdits, err = localChanges(ctx, target); err != nil {
			return "", err
		}
		beforeSwap = func(_ map[string]any, tree string) ([]string, error) {
			return carryUserFiles(target, tree, local, local != nil)
		}
	}
	var merged []string
	if beforeSwap != nil {
		if merged, err = beforeSwap(manifest, tmpTarget); err != nil {
			return "", err
		}
	}
	force := opts.Force
	if opts.AcceptCaution != nil {
		force = *opts.AcceptCaution
	}
	if _, err := scanMergedTree(h, tmpTarget, identifier, merged, scanOptions{Force: force, ScanDecision: opts.ScanDecision, ReviewedPin: atReviewedPin}); err != nil {
		return "", err
	}
	if !opts.PythonDeps {
		active, err := activePluginTree(ctx, target, true)
		if err != nil {
			return "", err
		}
		if active {
			return "", &PluginOperationError{Msg: "--no-deps cannot replace an active plugin. Retry without --no-deps; " +
				"PM must prepare its dependencies before publication.", FailureClass: "already_installed"}
		}
	}
	if err := refuseUnavailablePortablePlugin(ctx, pluginName, tmpTarget, opts.AppTitle); err != nil {
		return "", err
	}
	_, statErr := os.Stat(target)
	exists := statErr == nil
	if exists && !opts.Force {
		command, err := pm.CLICommandName(pm.RepoRoot())
		if err != nil {
			return "", err
		}
		return "", &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' already exists. Use force reinstall or run `%s plugins update %s`.",
			pluginName, command, pluginName), FailureClass: "already_installed"}
	}
	if exists && requestedRevision == "" && prior != nil && prior["pinned"] == true {
		return "", &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' is pinned. Reinstall it with an explicit "+
			"--ref <40-character commit SHA> to change its source or revision.", pluginName), FailureClass: "already_installed"}
	}
	record := map[string]any{"pinned": requestedRevision != "", "revision": installedRevision, "source": source}
	if jsonTruthy(manifest["update_url"]) {
		url, err := httpsUpdateURL(manifest["update_url"])
		if err != nil {
			return "", &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' %v", pluginName, err), FailureClass: "manifest_invalid"}
		}
		record["update_url"] = url
	}
	if opts.Catalog != nil {
		block := map[string]any{}
		for k, v := range opts.Catalog {
			block[k] = v
		}
		block["sha"], block["pin"] = installedRevision, ""
		if atReviewedPin {
			block["pin"] = opts.ReviewedPin
		}
		record["catalog"] = block
		writeCatalogSidecarRecord(tmpTarget, opts.Catalog, installedRevision)
	}
	if opts.AllowRemoved {
		record["allow_removed"] = true
	}
	newMetadata := map[string]any{}
	for k, v := range oldMetadata {
		newMetadata[k] = v
	}
	newMetadata[pluginName] = record
	if len(trackedEdits) > 0 {
		old := "old"
		if jsonTruthy(prior["revision"]) {
			old = pyrepr.Str(prior["revision"])
		}
		backup := filepath.Join(filepath.Dir(dir), "plugins-backup", filepath.Base(target)+"-"+string([]rune(old)[:min(8, len([]rune(old)))]))
		if err := stashLocalFiles(target, trackedEdits, backup); err != nil {
			return "", err
		}
		NewConsole(os.Stdout).Print(fmt.Sprintf("[yellow]Local edits to %d tracked file(s) were not carried over; "+
			"copies are under %s (re-apply by hand).[/yellow]", len(trackedEdits), backup))
	}
	if err := publishPlugin(ctx, tmpTarget, target, oldMetadata, newMetadata, publishOptions{RequireConsent: true, AssumeConsent: opts.AssumeDepsConsent}); err != nil {
		return "", &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' was not published: %v", pluginName, err), FailureClass: publishFailureClass(err)}
	}
	return target, nil
}

// Failure class of a publication from the error type at the raise site
func publishFailureClass(err error) string {
	var opErr *PluginOperationError
	if errors.As(err, &opErr) && opErr.FailureClass != "" && opErr.FailureClass != "other" {
		return opErr.FailureClass
	}
	var errno syscall.Errno
	isOS := errors.As(err, &errno)
	if !isOS {
		var pathErr *fs.PathError
		var linkErr *os.LinkError
		isOS = errors.As(err, &pathErr) || errors.As(err, &linkErr)
	}
	if errno == syscall.EACCES || errno == syscall.EPERM || errors.Is(err, fs.ErrPermission) {
		return "permission"
	}
	switch errno {
	case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, syscall.ESHUTDOWN, syscall.ETIMEDOUT:
		return "deps_failed"
	}
	if isOS {
		return "filesystem_error"
	}
	return "deps_failed"
}

// Runs one install attempt and records it as a shared metrics extension install
func recordedInstall(ctx context.Context, h string, install func() (string, map[string]any, string, error), catalogName, identifier string) (string, map[string]any, string, error) {
	source := "url"
	var name any
	if catalogName != "" {
		source, name = "catalog", catalogName
	} else if strings.HasPrefix(identifier, "file://") {
		source = "local"
	}
	before, err := readInstallMetadata(h)
	if err != nil {
		return "", nil, "", err
	}
	target, manifest, installedName, err := install()
	if err != nil {
		observability.RecordExtensionInstall(ctx, "plugin", source, name, "failed", nil, nil, err)
		return "", nil, "", err
	}
	if _, ok := before[installedName]; !ok {
		observability.RecordExtensionInstall(ctx, "plugin", source, name, "success", nil, nil, nil)
	}
	return target, manifest, installedName, nil
}

// Catalog entry or nil and the identifier to clone, announcing the catalog pin or the custom source warning
func announceSource(ctx context.Context, identifier string, console *Console, brief bool) (*PluginCatalogEntry, string, error) {
	if !LooksLikeCatalogName(identifier) {
		console.Print("[yellow]Warning:[/yellow] custom (unreviewed) source — not from the Icebath catalog.")
		return nil, identifier, nil
	}
	entry, err := resolveCatalogName(ctx, identifier, console)
	if err != nil {
		return nil, "", err
	}
	console.Print(fmt.Sprintf("[bold]%s[/bold] [cyan]\\[%s][/cyan] [dim]pinned @ %s[/dim]", entry.Name, entry.Tier, prefix(entry.Sha, 8)))
	if !brief {
		console.Print(EntryCapabilitySummary(entry))
	}
	return entry, entry.InstallIdentifier(), nil
}

// Flags of the plugins install command, a nil Enable prompts for it
type InstallCommandOptions struct {
	Identifier   string
	Force        bool
	Enable       *bool
	Ref          string
	AllowRemoved bool
	NoDeps       bool
	YesDeps      bool
	Brief        bool
}

// Installs a plugin from the catalog, a git URL or owner/repo shorthand, then prompts, enables and reports
func CmdInstall(ctx context.Context, opts InstallCommandOptions, out io.Writer) error {
	h := home.Dir(ctx)
	console := NewConsole(out)
	entry, identifier, err := announceSource(ctx, opts.Identifier, console, opts.Brief)
	if err != nil {
		return err
	}
	if opts.AllowRemoved {
		console.Print("[bold red]WARNING:[/bold red] [red]--allow-removed set — skipping the catalog kill-list check. " +
			"This plugin may have been removed for security reasons.[/red]")
	}
	gitURL, subdir, err := resolveGitURL(identifier)
	if err == nil && !opts.AllowRemoved {
		candidates := []string{identifier, gitURL}
		if entry != nil {
			candidates = append(candidates, entry.Name)
		}
		err = raiseIfRemoved(ctx, candidates...)
	}
	if err != nil {
		return fail(console, fmt.Sprintf("[red]Error:[/red] %v", err))
	}
	if strings.HasPrefix(gitURL, "http://") || strings.HasPrefix(gitURL, "file://") {
		console.Print("[yellow]Warning:[/yellow] Using insecure/local URL scheme. Consider using https:// or git@ for production installs.")
	}
	if !opts.Brief {
		shownSubdir := ""
		if subdir != "" {
			shownSubdir = " (subdir: " + subdir + ")"
		}
		console.Print(fmt.Sprintf("[dim]Cloning %s%s...[/dim]", gitURL, shownSubdir))
	}
	decide := InteractiveScanDecision(console, "Install")
	catalogName := ""
	if entry != nil {
		catalogName = entry.Name
	}
	target, installedManifest, installedName, err := recordedInstall(ctx, h, func() (string, map[string]any, string, error) {
		if entry != nil {
			return installCatalogEntry(ctx, entry, CatalogInstallOptions{Force: opts.Force, Ref: opts.Ref, AllowRemoved: opts.AllowRemoved,
				ScanDecision: decide, PythonDeps: !opts.NoDeps, AssumeDepsConsent: opts.YesDeps})
		}
		return installPluginCore(ctx, identifier, installCoreOptions{Force: opts.Force, Ref: opts.Ref, ScanDecision: decide,
			PythonDeps: !opts.NoDeps, AllowRemoved: opts.AllowRemoved, AssumeDepsConsent: opts.YesDeps})
	}, catalogName, identifier)
	var opErr *PluginOperationError
	if errors.As(err, &opErr) {
		label := "Error"
		var blocked *PluginScanBlocked
		if errors.As(err, &blocked) {
			label = "Blocked"
		}
		return fail(console, fmt.Sprintf("[red]%s:[/red] %v", label, err))
	}
	if err != nil {
		return err
	}
	return finishInstall(ctx, h, console, opts, identifier, target, installedManifest, installedName)
}

// Env prompts, enable decision, after install display, selection and capability consent of a placed install
func finishInstall(ctx context.Context, h string, console *Console, opts InstallCommandOptions, identifier, target string,
	installedManifest map[string]any, installedName string) error {
	if looks, err := looksLikePluginDir(target); err != nil {
		return err
	} else if !looks {
		console.Print(fmt.Sprintf("[yellow]Warning:[/yellow] %s doesn't contain plugin.yaml, plugin.json, or __init__.py. "+
			"It may not be a valid Icebath plugin.", installedName))
	}
	if err := promptPluginEnvVars(ctx, h, installedManifest, console); err != nil {
		return err
	}
	alreadyActive, err := activePluginTree(ctx, target, false)
	if err != nil {
		return err
	}
	shouldEnable := opts.Enable
	if opts.NoDeps {
		no := false
		shouldEnable = &no
		console.Print("[dim]--no-deps: skipping dependency consent; the plugin stays disabled.[/dim]")
	}
	svc := installedServices()
	if svc == nil || svc.DefinesMemoryProvider == nil {
		return &ServiceMissingError{Name: "DefinesMemoryProvider"}
	}
	isMemoryProvider := svc.DefinesMemoryProvider(target)
	if shouldEnable == nil && !alreadyActive {
		prompt := fmt.Sprintf("  Enable '%s' now? [y/N]: ", installedName)
		if isMemoryProvider {
			prompt = fmt.Sprintf("  Use '%s' as the memory provider now? [y/N]: ", installedName)
		}
		answer := isTTY() && askYes(prompt, nil)
		shouldEnable = &answer
	}
	enable := shouldEnable != nil && *shouldEnable
	depsOK, depsReason := true, (*consentRefusal)(nil)
	if enable && !alreadyActive {
		depsOK, depsReason = installPluginPythonDeps(ctx, installedManifest, target, console, opts.YesDeps)
	}
	if err := displayAfterInstall(console, target, identifier, opts.Brief); err != nil {
		return err
	}
	if enable && !depsOK {
		console.Print(fmt.Sprintf("[red]✗[/red] Cannot enable [bold]%s[/bold]: %s", installedName, depsReason.Text))
		console.Print("[dim]The plugin stays installed but disabled; re-enable after resolving the conflict.[/dim]")
		enable = false
	}
	activate := opts.Enable != nil && *opts.Enable
	explicitEnable := activate
	switch {
	case alreadyActive && !(isMemoryProvider && explicitEnable):
		console.Print("[dim]Replacement installed; plugin selection was not changed.[/dim]")
	case isMemoryProvider:
		if err := selectMemoryProvider(ctx, h, filepath.Base(target), console, enable); err != nil {
			return err
		}
	case enable:
		err := setPluginEnabled(ctx, h, installedName, true, nil, console)
		var refused *AdmissionRefused
		if errors.As(err, &refused) {
			console.Print("[dim]The plugin stays installed but disabled; re-enable after resolving the conflict.[/dim]")
		} else if err != nil {
			return err
		} else {
			activate = true
			console.Print(fmt.Sprintf("[green]✓[/green] Plugin [bold]%s[/bold] enabled.", installedName))
		}
	default:
		console.Print(fmt.Sprintf("[dim]Plugin installed but not enabled. Run `icebath plugins enable %s` to activate.[/dim]", installedName))
	}
	if svc.RequiresAuthNotice == nil {
		return &ServiceMissingError{Name: "RequiresAuthNotice"}
	}
	if notice := svc.RequiresAuthNotice(installedManifest); notice != "" {
		console.Print("[yellow]" + notice + "[/yellow]")
	}
	if declared := declaredCapabilitiesFromManifest(installedManifest, installedName); len(declared) > 0 {
		if err := runCapabilityConsent(ctx, console, installedName, declared, "install"); err != nil {
			return err
		}
	}
	if activate && !isMemoryProvider {
		if err := reportActivation(ctx, console, installedName); err != nil {
			return err
		}
	}
	console.Print("")
	return nil
}

// Makes a memory provider live after preparing its dependencies, or says how to
func selectMemoryProvider(ctx context.Context, h, name string, console *Console, selectNow bool) error {
	if !selectNow {
		console.Print(fmt.Sprintf("[dim]Memory provider installed but not active. Run `icebath memory setup` "+
			"(or set memory.provider: %s) to use it.[/dim]", name))
		return nil
	}
	svc := installedServices()
	if svc == nil || svc.MemoryProviderDir == nil {
		return &ServiceMissingError{Name: "MemoryProviderDir"}
	}
	if svc.PrepareMemoryProviderDependencies == nil {
		return &ServiceMissingError{Name: "PrepareMemoryProviderDependencies"}
	}
	if _, ok := svc.MemoryProviderDir(h, name); !ok {
		console.Print(fmt.Sprintf("[red]✗[/red] %s does not resolve as a memory provider; memory.provider is unchanged.", name))
		return nil
	}
	if err := svc.PrepareMemoryProviderDependencies(ctx, name); err != nil {
		logDebug(fmt.Sprintf("memory provider %s dependency preparation failed: %v", name, err))
		console.Print(fmt.Sprintf("[red]✗[/red] Could not prepare %s's dependencies: %v", name, err))
		console.Print("[dim]memory.provider is unchanged; run `icebath memory setup` after resolving it.[/dim]")
		return nil
	}
	expected, err := pluginSelectionVersion(h)
	if err != nil {
		return err
	}
	disabled := getDisabledSet(h)
	aliases, err := pluginAliases(h, name)
	if err != nil {
		return err
	}
	lifted := map[string]bool{}
	overlap := false
	for key := range disabled {
		if aliases[key] {
			overlap = true
		} else {
			lifted[key] = true
		}
	}
	if overlap {
		err := admitAndSavePluginSets(ctx, h, getEnabledSet(h), lifted, admitSetOptions{Console: console,
			Action: fmt.Sprintf("Re-enable '%s'", name), ExpectedConfig: expected})
		var refused *AdmissionRefused
		if errors.As(err, &refused) {
			console.Print("[dim]memory.provider is unchanged.[/dim]")
			return nil
		}
		if err != nil {
			return err
		}
	}
	previous := getCurrentMemoryProvider(h)
	if err := saveMemoryProvider(h, name); err != nil {
		return err
	}
	replacing := ""
	if previous != "" && previous != name {
		replacing = " (replacing " + previous + ")"
	}
	console.Print(fmt.Sprintf("[green]✓[/green] [bold]%s[/bold] set as memory.provider%s. "+
		"Run `icebath memory setup %s` to configure it; new sessions use it.", name, replacing, name))
	return nil
}

// Installed, not enabled tree of a catalog entry at the approved commit, ok false otherwise
func catalogInstallOnDisk(ctx context.Context, h, catalogName, ref, inspectedSha string) (string, map[string]any, string, bool, error) {
	target := catalogInstalledDir(h, catalogName)
	if target == "" {
		return "", nil, "", false, nil
	}
	record := catalogInstallRecord(h, target)
	installedSha := strings.ToLower(pyStrOr(record["sha"]))
	if ref != "" && installedSha != strings.ToLower(ref) {
		return "", nil, "", false, nil
	}
	if inspectedSha != "" {
		inspected, pin := strings.ToLower(inspectedSha), strings.ToLower(pyStrOr(record["pin"]))
		if inspected == "" || (inspected != installedSha && inspected != pin) {
			return "", nil, "", false, nil
		}
	}
	manifest, err := readManifest(target)
	if err != nil {
		return "", nil, "", false, err
	}
	installedName := filepath.Base(target)
	if jsonTruthy(manifest["name"]) {
		installedName = pyrepr.Str(manifest["name"])
	}
	enabled := getEnabledSet(h)
	if enabled[installedName] || enabled[filepath.Base(target)] {
		return "", nil, "", false, nil
	}
	return target, manifest, installedName, true, nil
}

// Catalog entry or nil, the identifier to clone, the warnings so far and a refusal result
func resolveSource(ctx context.Context, identifier, catalogName string) (*PluginCatalogEntry, string, []string, map[string]any) {
	warnings := []string{}
	var entry *PluginCatalogEntry
	if catalogName != "" {
		if entry = GetLiveCatalogEntry(ctx, catalogName); entry == nil {
			return nil, identifier, warnings, map[string]any{"ok": false, "error": fmt.Sprintf("'%s' is not in the Icebath plugin catalog.", catalogName)}
		}
		warnings = append(warnings, knownIssueWarnings(entry)...)
		identifier = entry.InstallIdentifier()
	} else {
		warnings = append(warnings, "Custom (unreviewed) source — not from the Icebath catalog.")
	}
	gitURL, _, err := resolveGitURL(identifier)
	if err != nil {
		return entry, identifier, warnings, nil
	}
	if strings.HasPrefix(gitURL, "http://") || strings.HasPrefix(gitURL, "file://") {
		warnings = append(warnings, "Insecure URL scheme; prefer https:// or git@ for production installs.")
	}
	candidates := []string{identifier, gitURL}
	if entry != nil {
		candidates = append(candidates, entry.Name)
	}
	if err := raiseIfRemoved(ctx, candidates...); err != nil {
		return entry, identifier, warnings, map[string]any{"ok": false, "error": err.Error()}
	}
	return entry, identifier, warnings, nil
}

// Tree to enable, reusing a refused catalog install on disk, else the error result of a fresh install
func placeTree(ctx context.Context, h string, entry *PluginCatalogEntry, identifier string, force bool, ref string,
	assumeDepsConsent bool, step func(InstallPhase), inspectedSha string) (string, map[string]any, string, map[string]any, error) {
	ref = strings.TrimSpace(ref)
	if ref != "" {
		inspectedSha = ""
	}
	if entry != nil && !force {
		target, manifest, name, ok, err := catalogInstallOnDisk(ctx, h, entry.Name, ref, inspectedSha)
		if err != nil || ok {
			return target, manifest, name, nil, err
		}
	}
	step(InstallPhaseDownloading)
	catalogName := ""
	if entry != nil {
		catalogName = entry.Name
	}
	target, manifest, name, err := recordedInstall(ctx, h, func() (string, map[string]any, string, error) {
		if entry != nil {
			return installCatalogEntry(ctx, entry, CatalogInstallOptions{Force: force, Ref: ref, PythonDeps: true,
				AssumeDepsConsent: assumeDepsConsent, InspectedSha: inspectedSha})
		}
		return installPluginCore(ctx, identifier, installCoreOptions{Force: force, Ref: ref, PythonDeps: true,
			AssumeDepsConsent: assumeDepsConsent, InspectedSha: inspectedSha})
	}, catalogName, identifier)
	var blocked *PluginScanBlocked
	var moved *PluginSourceMoved
	var opErr *PluginOperationError
	switch {
	case errors.As(err, &blocked):
		return "", nil, "", ScanBlockedResult(blocked), nil
	case errors.As(err, &moved):
		return "", nil, "", map[string]any{"ok": false, "error": err.Error(), "source_moved": true}, nil
	case errors.As(err, &opErr):
		return "", nil, "", map[string]any{"ok": false, "error": err.Error()}, nil
	}
	return target, manifest, name, nil, err
}

// Enables a placed plugin through admission, the refusal result when admission refuses
func enablePlaced(ctx context.Context, h, installedName string, deps []string, step func(InstallPhase)) (map[string]any, error) {
	if len(deps) > 0 {
		step(InstallPhasePythonPackages)
	}
	err := setPluginEnabled(ctx, h, installedName, true, nil, nil)
	var refused *AdmissionRefused
	if errors.As(err, &refused) {
		return map[string]any{"ok": false, "error": fmt.Sprintf("enable refused: %s", refused.Error()), "plugin_name": installedName, "enabled": false}, nil
	}
	return nil, err
}

// Inputs of a dashboard or TUI install, a nil AssumeDepsConsent takes the fresh enable as consent
type DashboardInstallOptions struct {
	Identifier        string
	Force             bool
	Enable            bool
	CatalogName       string
	Ref               string
	AssumeDepsConsent *bool
	OnStep            func(InstallPhase)
	InspectedSha      string
}

// Non interactive install for the dashboard and TUI with the kill list enforced on every path
func DashboardInstallPlugin(ctx context.Context, opts DashboardInstallOptions) (map[string]any, error) {
	h := home.Dir(ctx)
	assume := opts.Enable && !opts.Force
	if opts.AssumeDepsConsent != nil {
		assume = *opts.AssumeDepsConsent
	}
	step := opts.OnStep
	if step == nil {
		step = func(InstallPhase) {}
	}
	if opts.InspectedSha != "" {
		forgetInspection(ctx, opts.Identifier, opts.CatalogName)
	}
	entry, identifier, warnings, refusal := resolveSource(ctx, opts.Identifier, opts.CatalogName)
	if refusal != nil {
		return refusal, nil
	}
	target, manifest, installedName, result, err := placeTree(ctx, h, entry, identifier, opts.Force, opts.Ref, assume, step, opts.InspectedSha)
	if err != nil || result != nil {
		return result, err
	}
	deps := pythonDependencySummary(target, &warnings)
	if opts.Enable {
		refused, err := enablePlaced(ctx, h, installedName, deps, step)
		if err != nil || refused != nil {
			return refused, err
		}
	}
	activated := map[string]any{"gateway_reloaded": false, "activation": nil, "restart_required": false}
	if opts.Enable {
		step(InstallPhaseLoadingTools)
		if activated, err = activatePluginNow(ctx, installedName); err != nil {
			return nil, err
		}
	}
	missing, err := missingEnvSpecs(ctx, h, manifest)
	if err != nil {
		return nil, err
	}
	missingNames := []any{}
	for _, spec := range missing {
		missingNames = append(missingNames, spec["name"])
	}
	var afterInstall any
	if ap := filepath.Join(target, "after-install.md"); fileExists(ap) {
		afterInstall = ap
	}
	knownIssues := []any{}
	if entry != nil {
		knownIssues = stringsAny(entry.KnownIssues)
	}
	out := map[string]any{"ok": true, "plugin_name": installedName, "warnings": stringsAny(warnings),
		"python_dependencies": stringsAny(deps), "missing_env": missingNames, "after_install_path": afterInstall, "enabled": opts.Enable}
	for k, v := range activated {
		out[k] = v
	}
	out["known_issues"] = knownIssues
	return out, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
