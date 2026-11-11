package app

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Resources resident payload directory every bundled artifact ships
const bundlePayloadDirName = "agent-payload"

// Executables beside a Linux Electron launcher that are not the launcher
var bundleLinuxNonLaunchers = map[string]bool{"AppRun": true, "chrome-sandbox": true, "chrome_crashpad_handler": true}

// Raised when a tree is not a bundled desktop payload
type notBundledApp struct{ msg string }

func (e *notBundledApp) Error() string { return e.msg }

// App root, resources, payload and launcher of a bundled desktop app, the launcher empty when ambiguous
type bundleLayout struct {
	AppRoot   string
	Resources string
	Payload   string
	Launcher  string
}

// Desktop app holding the payload at projectRoot, a notBundledApp error for any other tree
func resolveBundleLayout(projectRoot, platform string) (*bundleLayout, error) {
	repo, err := pypath.Resolve(projectRoot)
	if err != nil {
		repo = projectRoot
	}
	payload := filepath.Dir(repo)
	if filepath.Base(payload) != bundlePayloadDirName {
		return nil, &notBundledApp{fmt.Sprintf("%s is not inside a %s/ payload — no desktop app contains it", repo, bundlePayloadDirName)}
	}
	resources := filepath.Dir(payload)
	appRoot := filepath.Dir(resources)
	if filepath.Base(resources) == "Resources" && filepath.Base(appRoot) == "Contents" && pypath.PathSuffix(filepath.Dir(appRoot)) == ".app" {
		appRoot = filepath.Dir(appRoot)
	}
	if st, err := os.Stat(appRoot); err != nil || !st.IsDir() {
		return nil, &notBundledApp{fmt.Sprintf("no app directory above the payload at %s", payload)}
	}
	if platform == "" {
		platform = guiPlatform()
	}
	return &bundleLayout{AppRoot: appRoot, Resources: resources, Payload: payload, Launcher: resolveBundleLauncher(appRoot, platform)}, nil
}

// Single launcher among the files of the app that are not helpers, empty when ambiguous
func resolveBundleLauncher(appRoot, platform string) string {
	if platform == "darwin" {
		return bundleOnly(bundleFiles(filepath.Join(appRoot, "Contents", "MacOS")))
	}
	var candidates []string
	for _, p := range bundleFiles(appRoot) {
		name := filepath.Base(p)
		if platform == "win32" {
			if strings.ToLower(pypath.PathSuffix(p)) == ".exe" && !strings.HasPrefix(strings.ToLower(name), "uninstall") {
				candidates = append(candidates, p)
			}
			continue
		}
		if !bundleLinuxNonLaunchers[name] && !strings.Contains(name, ".so") && accessExecutable(p) {
			candidates = append(candidates, p)
		}
	}
	return bundleOnly(candidates)
}

// Regular files directly in a directory sorted by path, none when it is unreadable
func bundleFiles(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			files = append(files, p)
		}
	}
	slices.SortFunc(files, func(a, b string) int { return strings.Compare(pypath.Normcase(a), pypath.Normcase(b)) })
	return files
}

// The only candidate, empty unless exactly one exists
func bundleOnly(candidates []string) string {
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

// Starts argv detached from this process with null stdio and returns its pid
func launchDetached(argv []string, env map[string]string, cwd string) (int, error) {
	return gui.popen(guiRun{Argv: argv, Dir: cwd, Env: env, Stdin: streamDevnull, Stdout: streamDevnull, Stderr: streamDevnull, Attr: detachPopenAttr()})
}
