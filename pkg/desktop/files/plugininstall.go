package files

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var githubBrowserSegments = map[string]bool{"tree": true, "blob": true, "commit": true}

var (
	gitURLSchemeRE = regexp.MustCompile(`^(https?://|git@|ssh://|file://)`)
	commitShaRE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	jsWS           = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`
	yamlNameRE     = regexp.MustCompile(`(?m)^name:` + jsWS + `*['"]?([^'"\n]+)['"]?` + jsWS + `*$`)
)

// gitTimeout matches the backend's default plugins.clone_timeout_seconds
const gitTimeout = 300 * time.Second

// ResolvedGitURL is a clone URL and the subdirectory the plugin sits in
type ResolvedGitURL struct {
	GitURL string
	Subdir string
}

func trimSlashes(s string) string { return strings.Trim(s, "/") }

// Clone URL and subdirectory of a plugin identifier, a URL or owner/repo with an optional path
func ResolvePluginGitURL(identifier string) (ResolvedGitURL, error) {
	trimmed := jsTrim(identifier)
	if trimmed == "" {
		return ResolvedGitURL{}, errors.New("Plugin identifier is required.")
	}
	if gitURLSchemeRE.MatchString(trimmed) {
		if strings.HasPrefix(trimmed, "https://github.com/") {
			rest := trimmed[len("https://github.com/"):]
			if i := strings.IndexAny(rest, "?#"); i >= 0 {
				rest = rest[:i]
			}
			rest = strings.TrimRight(rest, "/")
			var parts []string
			for _, p := range strings.Split(rest, "/") {
				if p != "" {
					parts = append(parts, p)
				}
			}
			if len(parts) >= 3 && parts[2] != "" && githubBrowserSegments[parts[2]] {
				repo := strings.TrimSuffix(parts[1], ".git")
				subdir := ""
				if parts[2] == "tree" && len(parts) >= 5 {
					subdir = strings.TrimRight(strings.Join(parts[4:], "/"), "/")
				}
				return ResolvedGitURL{GitURL: "https://github.com/" + parts[0] + "/" + repo + ".git", Subdir: subdir}, nil
			}
		}
		if i := strings.IndexByte(trimmed, '#'); i >= 0 {
			return ResolvedGitURL{GitURL: trimmed[:i], Subdir: trimSlashes(trimmed[i+1:])}, nil
		}
		if i := strings.Index(trimmed, ".git/"); i >= 0 {
			return ResolvedGitURL{GitURL: trimmed[:i+len(".git")], Subdir: trimSlashes(trimmed[i+len(".git/"):])}, nil
		}
		return ResolvedGitURL{GitURL: trimmed}, nil
	}
	var parts []string
	for _, p := range strings.Split(trimmed, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) >= 2 {
		return ResolvedGitURL{
			GitURL: "https://github.com/" + parts[0] + "/" + parts[1] + ".git",
			Subdir: strings.TrimRight(strings.Join(parts[2:], "/"), "/"),
		}, nil
	}
	return ResolvedGitURL{}, errors.New("Invalid plugin identifier. Use a Git URL or 'owner/repo' (optionally with a subdirectory).")
}

func lastNonEmpty(parts []string, fallback string) string {
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return fallback
	}
	return parts[len(parts)-1]
}

// Repository name of a clone URL, the last path or scp segment without .git
func RepoNameFromURL(u string) string {
	name := strings.TrimRight(u, "/")
	name = strings.TrimSuffix(name, ".git")
	name = lastNonEmpty(strings.Split(name, "/"), name)
	if strings.Contains(name, ":") {
		name = lastNonEmpty(strings.Split(name, ":"), name)
		name = lastNonEmpty(strings.Split(name, "/"), name)
	}
	return name
}

// The name when it can be one folder directly under the desktop-plugins root, an error otherwise
func CheckedDesktopFolderName(name string) (string, error) {
	candidate := StripLikePython(name)
	bad := candidate == "" || strings.HasPrefix(candidate, ".") || strings.HasSuffix(candidate, ".") ||
		strings.ContainsAny(candidate, `/\:`) || utf8.RuneCountInString(candidate) > 128
	for _, r := range candidate {
		if r < 32 {
			bad = true
		}
	}
	if !bad {
		stem := strings.SplitN(candidate, ".", 2)[0]
		bad = winReserved(strings.TrimRight(stem, " "))
	}
	if bad {
		return "", fmt.Errorf("'%s' is not a desktop plugin folder name: it must be a plain folder name, not a path.", name)
	}
	return candidate, nil
}

// Stable folder name of a desktop plugin, the last real subdirectory segment else the repository name
func DesktopPluginFolderName(gitURL, subdir string) string {
	if subdir != "" {
		var last string
		for _, part := range regexp.MustCompile(`[/\\]`).Split(subdir, -1) {
			if part != "" && part != "." && part != "desktop" {
				last = part
			}
		}
		if last != "" {
			return last
		}
	}
	return RepoNameFromURL(gitURL)
}

// Subdirectory of the clone, refused when it escapes the repository
func ResolveSubdirWithin(cloneRoot, subdir string) (string, error) {
	root := resolvePath(cloneRoot)
	candidate := resolveAgainst(root, subdir)
	if candidate != root && !strings.HasPrefix(candidate, root+string(filepath.Separator)) {
		return "", fmt.Errorf("Plugin subdirectory '%s' escapes the repository.", subdir)
	}
	return candidate, nil
}

func accessible(p string) bool {
	_, err := os.Lstat(p)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

func pathIsDirectory(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func pathIsFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// DesktopEntry is the plugin.js of a package and the folder it sits in relative to the package
type DesktopEntry struct {
	EntryFile    string
	SourceSubdir string
}

// Desktop entry by the rule the reconcile applies, a root plugin.js before desktop/plugin.js
func FindDesktopEntry(pluginRoot string) *DesktopEntry {
	sourceDir := DesktopHalfSource(pluginRoot)
	if sourceDir == "" {
		return nil
	}
	sub, err := filepath.Rel(pluginRoot, sourceDir)
	if err != nil || sub == "" {
		sub = "."
	}
	return &DesktopEntry{EntryFile: filepath.Join(sourceDir, "plugin.js"), SourceSubdir: sub}
}

var agentPayloadFiles = []string{"__init__.py", filepath.Join("dashboard", "manifest.json")}

// True when a plugin.yaml folder ships something the agent side loads
func yamlShipsAgentPayload(pluginRoot, yamlPath string) bool {
	for _, rel := range agentPayloadFiles {
		if accessible(filepath.Join(pluginRoot, rel)) {
			return true
		}
	}
	if !pathIsDirectory(filepath.Join(pluginRoot, "locales")) {
		return false
	}
	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		return false
	}
	var manifest any
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		return false
	}
	m, ok := manifest.(map[string]any)
	if !ok {
		return false
	}
	switch locales := m["provides_locales"].(type) {
	case nil:
		return false
	case map[string]any:
		return len(locales) > 0
	case []any:
		return len(locales) > 0
	default:
		return jsTruthy(normalizeYAMLScalar(locales))
	}
}

func normalizeYAMLScalar(v any) any {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case float32:
		return float64(x)
	}
	return v
}

// PluginComponentDetection is what a plugin folder carries for the agent and the desktop
type PluginComponentDetection struct {
	Agent               bool
	Desktop             bool
	AgentName           any
	DesktopName         string
	DesktopSourceSubdir string
}

// Agent and desktop halves of a plugin folder and their names
func DetectPluginComponents(pluginRoot string) PluginComponentDetection {
	yamlPath := ""
	for _, name := range []string{"plugin.yaml", "plugin.yml"} {
		if accessible(filepath.Join(pluginRoot, name)) {
			yamlPath = filepath.Join(pluginRoot, name)
			break
		}
	}
	hasYAML := yamlPath != ""
	hasPortable := accessible(filepath.Join(pluginRoot, "plugin.json"))
	agent := (hasYAML && yamlShipsAgentPayload(pluginRoot, yamlPath)) || hasPortable
	entry := FindDesktopEntry(pluginRoot)
	d := PluginComponentDetection{Agent: agent, Desktop: entry != nil}
	if agent {
		d.AgentName = filepath.Base(pluginRoot)
		if hasYAML {
			if raw, err := os.ReadFile(yamlPath); err == nil {
				if m := yamlNameRE.FindStringSubmatch(string(raw)); m != nil && m[1] != "" {
					d.AgentName = jsTrim(m[1])
				}
			}
		} else if hasPortable {
			if raw, err := os.ReadFile(filepath.Join(pluginRoot, "plugin.json")); err == nil {
				if parsed, err := jsonParse(string(raw)); err == nil {
					if obj, ok := parsed.(map[string]any); ok && jsTruthy(obj["name"]) {
						d.AgentName = obj["name"]
					}
				}
			}
		}
	}
	if entry != nil {
		if entry.SourceSubdir == "." {
			d.DesktopName = filepath.Base(pluginRoot)
		} else {
			d.DesktopName = filepath.Base(filepath.Dir(entry.EntryFile))
		}
		d.DesktopSourceSubdir = entry.SourceSubdir
	}
	return d
}

func noninteractiveGitEnv() []string {
	env := map[string]string{}
	var order []string
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			if _, seen := env[kv[:i]]; !seen {
				order = append(order, kv[:i])
			}
			env[kv[:i]] = kv[i+1:]
		}
	}
	for _, k := range []string{"GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "SSH_ASKPASS"} {
		if _, seen := env[k]; !seen {
			order = append(order, k)
		}
	}
	env["GIT_TERMINAL_PROMPT"], env["GIT_ASKPASS"], env["SSH_ASKPASS"] = "0", "echo", "echo"
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Node spawn failure text, spawn then the command and the error code
func spawnError(command string, err error) error {
	code := ErrorCode(err)
	if errors.Is(err, exec.ErrNotFound) {
		code = "ENOENT"
	}
	if code == "" {
		return err
	}
	return &NodeError{Code: code, Message: fmt.Sprintf("spawn %s %s", command, code), Err: err}
}

// Runs a hidden git command keeping stderr, killed at the clone timeout
func runGit(gitBin string, args []string, cwd string) (int, string, error) {
	plan := PlanNoConsoleGitSpawn(gitBin, args, hostOS == "windows", noninteractiveGitEnv())
	cmd := exec.Command(plan.Command, plan.Args...)
	cmd.Dir = cwd
	cmd.Env = plan.Env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	configureGitProcess(cmd, plan, false)
	if err := cmd.Start(); err != nil {
		return 0, "", spawnError(plan.Command, err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		code := 0
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return 0, "", err
			}
			code = exitErr.ExitCode()
			if code < 0 {
				code = 1
			}
		}
		return code, stderr.String(), nil
	case <-time.After(gitTimeout):
		cmd.Process.Kill()
		return 0, "", fmt.Errorf("Git %s timed out after %d seconds.", args[0], int(gitTimeout/time.Second))
	}
}

func runGitOrThrow(gitBin string, args []string, cwd string) error {
	code, stderr, err := runGit(gitBin, args, cwd)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("Git %s failed:\n%s", args[0], jsTrim(stderr))
	}
	return nil
}

var sparseEscapeRE = regexp.MustCompile(`([\\*?\[])`)

// Classic sparse checkout pattern of one subdirectory
func sparseCheckoutPattern(subdir string) string {
	return "/" + sparseEscapeRE.ReplaceAllString(trimSlashes(subdir), `\$1`) + "/\n"
}

// Shallow clone into a temp folder, a blobless sparse checkout when only a subdirectory is wanted
func cloneToTemp(gitBin, gitURL, subdir string) (string, error) {
	tmpRoot, err := os.MkdirTemp("", "hermes-plugin-")
	if err != nil {
		return "", nodeFsError(err, "mkdtemp", filepath.Join(os.TempDir(), "hermes-plugin-XXXXXX"))
	}
	fail := func(err error) (string, error) {
		os.RemoveAll(tmpRoot)
		return "", err
	}
	if subdir == "" {
		if err := runGitOrThrow(gitBin, []string{"clone", "--depth", "1", gitURL, tmpRoot}, ""); err != nil {
			return fail(err)
		}
		return tmpRoot, nil
	}
	if err := runGitOrThrow(gitBin, []string{"clone", "--depth", "1", "--filter=blob:none", "--no-checkout", gitURL, tmpRoot}, ""); err != nil {
		return fail(err)
	}
	if err := runGitOrThrow(gitBin, []string{"config", "core.sparseCheckout", "true"}, tmpRoot); err != nil {
		return fail(err)
	}
	info := filepath.Join(tmpRoot, ".git", "info")
	if err := os.MkdirAll(info, 0o777); err != nil {
		return fail(nodeFsError(err, "mkdir", info))
	}
	sparse := filepath.Join(info, "sparse-checkout")
	if err := os.WriteFile(sparse, []byte(sparseCheckoutPattern(subdir)), 0o666); err != nil {
		return fail(nodeFsError(err, "open", sparse))
	}
	if err := runGitOrThrow(gitBin, []string{"checkout", "HEAD"}, tmpRoot); err != nil {
		return fail(err)
	}
	return tmpRoot, nil
}

func headSha(gitBin, cloneRoot, revision string) (string, error) {
	res, err := ExecGit(gitBin, []string{"rev-parse", revision}, cloneRoot, noninteractiveGitEnv(), gitTimeout)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", fmt.Errorf("Git rev-parse failed:\n%s", jsTrim(res.Stderr))
	}
	return strings.ToLower(jsTrim(res.Stdout)), nil
}

// ReviewedCommitUnavailableError is the refusal of a reviewed commit the source no longer has
type ReviewedCommitUnavailableError struct{ Message string }

func (e *ReviewedCommitUnavailableError) Error() string { return e.Message }

// Detaches the clone at the commit the install dialog reviewed, refusing a source that lost it
func checkoutReviewedCommit(gitBin, cloneRoot, sha string) error {
	reviewed := strings.ToLower(jsTrim(sha))
	if !commitShaRE.MatchString(reviewed) {
		return errors.New("Installing a desktop plugin needs the full commit the install dialog reviewed.")
	}
	err := func() error {
		head, err := headSha(gitBin, cloneRoot, "HEAD")
		if err != nil {
			return err
		}
		if head != reviewed {
			if err := runGitOrThrow(gitBin, []string{"fetch", "--depth", "1", "origin", reviewed}, cloneRoot); err != nil {
				return err
			}
			if err := runGitOrThrow(gitBin, []string{"checkout", "--detach", reviewed}, cloneRoot); err != nil {
				return err
			}
		}
		head, err = headSha(gitBin, cloneRoot, "HEAD")
		if err != nil {
			return err
		}
		peeled, err := headSha(gitBin, cloneRoot, reviewed+"^{commit}")
		if err != nil {
			return err
		}
		if head != peeled {
			return fmt.Errorf("HEAD is not %s.", reviewed)
		}
		return nil
	}()
	if err != nil {
		return &ReviewedCommitUnavailableError{Message: fmt.Sprintf(
			"The commit you reviewed (%s) could not be checked out; the source may have changed since you reviewed it. Re-open the install dialog to review it again. %s",
			reviewed[:12], err.Error())}
	}
	return nil
}

func resolvePluginRoot(cloneRoot, subdir string) (string, error) {
	if subdir == "" {
		return cloneRoot, nil
	}
	resolved, err := ResolveSubdirWithin(cloneRoot, subdir)
	if err != nil {
		return "", err
	}
	if !pathIsDirectory(resolved) {
		return "", fmt.Errorf("Plugin subdirectory '%s' does not exist in the repository.", subdir)
	}
	return resolved, nil
}

func insecureSchemeWarnings(gitURL string) ([]string, bool) {
	if strings.HasPrefix(gitURL, "http://") || strings.HasPrefix(gitURL, "file://") {
		return []string{"This URL uses an insecure or local scheme. Prefer https:// or git@ for production installs."}, true
	}
	return []string{}, false
}

// Clones a plugin repository and reports its halves and the commit read, never throwing
func ProbePluginRepo(gitBin, identifier string) *OrderedObject {
	result, err := func() (*OrderedObject, error) {
		resolved, err := ResolvePluginGitURL(identifier)
		if err != nil {
			return nil, err
		}
		warnings, insecure := insecureSchemeWarnings(resolved.GitURL)
		cloneRoot, err := cloneToTemp(gitBin, resolved.GitURL, resolved.Subdir)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(cloneRoot)
		pluginRoot, err := resolvePluginRoot(cloneRoot, resolved.Subdir)
		if err != nil {
			return nil, err
		}
		detected := DetectPluginComponents(pluginRoot)
		if !detected.Agent && !detected.Desktop {
			return NewOrderedObject("ok", false, "agent", false, "desktop", false, "warnings", warnings,
				"insecure", insecure, "error", "No agent or desktop plugin artifacts found in this repository."), nil
		}
		var agentName any = detected.AgentName
		if agentName == nil && detected.Agent {
			agentName = RepoNameFromURL(resolved.GitURL)
		}
		var desktopName any
		if detected.Desktop {
			desktopName = DesktopPluginFolderName(resolved.GitURL, resolved.Subdir)
		}
		sha, err := headSha(gitBin, cloneRoot, "HEAD")
		if err != nil {
			return nil, err
		}
		out := NewOrderedObject("ok", true, "agent", detected.Agent, "desktop", detected.Desktop)
		out.Set("agentName", jsonNull(agentName))
		out.Set("desktopName", jsonNull(desktopName))
		out.Set("warnings", warnings)
		out.Set("insecure", insecure)
		out.Set("sha", sha)
		return out, nil
	}()
	if err != nil {
		return NewOrderedObject("ok", false, "agent", false, "desktop", false, "warnings", []string{}, "insecure", false, "error", err.Error())
	}
	return result
}

// jsonNullValue serializes as JSON null inside an ordered object
type jsonNullValue struct{}

func (jsonNullValue) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

func jsonNull(v any) any {
	if v == nil {
		return jsonNullValue{}
	}
	return v
}

// Installs a repository's desktop half under the desktop-plugins root from exactly the reviewed commit
func InstallDesktopPluginFromGit(gitBin, identifier, desktopPluginsRoot string, force bool, sha *string) *OrderedObject {
	result, err := func() (*OrderedObject, error) {
		resolved, err := ResolvePluginGitURL(identifier)
		if err != nil {
			return nil, err
		}
		cloneRoot, err := cloneToTemp(gitBin, resolved.GitURL, resolved.Subdir)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(cloneRoot)
		if sha != nil {
			if err := checkoutReviewedCommit(gitBin, cloneRoot, *sha); err != nil {
				return nil, err
			}
		}
		pluginRoot, err := resolvePluginRoot(cloneRoot, resolved.Subdir)
		if err != nil {
			return nil, err
		}
		detected := DetectPluginComponents(pluginRoot)
		if !detected.Desktop || detected.DesktopSourceSubdir == "" {
			return NewOrderedObject("ok", false, "error", "No desktop plugin.js found in this repository."), nil
		}
		sourceDir := pluginRoot
		if detected.DesktopSourceSubdir != "." {
			sourceDir = filepath.Join(pluginRoot, detected.DesktopSourceSubdir)
		}
		var packageName any
		if detected.Agent {
			packageName = detected.AgentName
			if packageName == nil {
				packageName = DesktopPluginFolderName(resolved.GitURL, resolved.Subdir)
			}
		}
		rawName := DesktopPluginFolderName(resolved.GitURL, resolved.Subdir)
		if packageName != nil {
			name, ok := packageName.(string)
			if !ok {
				return nil, errors.New("text.replace is not a function")
			}
			rawName = name
		}
		pluginName, err := CheckedDesktopFolderName(rawName)
		if err != nil {
			return nil, err
		}
		targetDir := filepath.Join(desktopPluginsRoot, pluginName)
		targetPlugin := filepath.Join(targetDir, "plugin.js")
		return WithDesktopPluginsRoot(desktopPluginsRoot, func() (*OrderedObject, error) {
			if !force && (pathIsDirectory(targetDir) || pathIsFile(targetPlugin)) {
				return NewOrderedObject("ok", false, "error",
					fmt.Sprintf("Desktop plugin '%s' already exists. Enable force reinstall to replace it.", pluginName)), nil
			}
			var filter func(string) bool
			if detected.DesktopSourceSubdir == "." {
				filter = RootLayoutFilter
			}
			err := PublishDesktopTree(DesktopTreeSource{Dir: sourceDir, PlainWithin: cloneRoot}, targetDir, func(staged string) error {
				if packageName == nil {
					if err := os.Remove(filepath.Join(staged, PackageMarker)); err != nil && !isMissingErr(err) {
						return nodeFsError(err, "rm", filepath.Join(staged, PackageMarker))
					}
					return nil
				}
				st, err := os.Stat(filepath.Join(staged, "plugin.js"))
				if err != nil {
					return nodeFsError(err, "stat", filepath.Join(staged, "plugin.js"))
				}
				return WriteDesktopHalfMarker(staged, NewOrderedObject("package", pluginName, "repo", resolved.GitURL,
					"source", targetDir, "sourceMtimeMs", mtimeMs(st)))
			}, filter)
			if err != nil {
				return nil, err
			}
			if !pathIsFile(targetPlugin) {
				return NewOrderedObject("ok", false, "error", fmt.Sprintf("Install completed but %s is missing.", targetPlugin)), nil
			}
			return NewOrderedObject("ok", true, "pluginName", pluginName, "path", targetDir), nil
		})
	}()
	if err != nil {
		out := NewOrderedObject("ok", false, "error", err.Error())
		var moved *ReviewedCommitUnavailableError
		if errors.As(err, &moved) {
			out.Set("sourceMoved", true)
		}
		return out
	}
	return result
}
