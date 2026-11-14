package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Installed node binary of the package manager, false when node or its binary is not installed
var sourceNodeBinary = func(ctx context.Context) (string, bool) {
	installed, err := pm.Installed(ctx, "node", false)
	if err != nil || installed == nil || installed.Binary == "" {
		return "", false
	}
	return installed.Binary, true
}

// Receipt reader of the source builds, the source_product_current hermes tests replace
var sourceProductCurrent = SourceProductCurrent

// Package manager acquisition of a build tool, the runner whose env a source build composes
var sourceEnsure = pm.Ensure

// True when the compiler receipt says the product output matches its inputs, read only under PM node
func SourceProductCurrent(ctx context.Context, projectRoot, product, out string) bool {
	node, ok := sourceNodeBinary(ctx)
	if !ok {
		return false
	}
	host, err := frontendNodeHost(node)
	if err != nil {
		return false
	}
	return frontendProductCurrent(host, projectRoot, product, out, nil, pm.EnvFor([]string{"node"}, nil))
}

// Process environment as Python os environ maps it, keys upper cased on Windows
func sourceEnviron() map[string]string {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			if runtime.GOOS == "windows" {
				k = strings.ToUpper(k)
			}
			env[k] = v
		}
	}
	return env
}

// Interpreter the isolated icon child runs, the selected venv python or this executable
func sourceBuildPython(ctx context.Context) (string, error) {
	root := pm.RepoRoot()
	selected, _, err := pm.RunningFromSelectedEnvironment(ctx, root)
	if err != nil {
		return "", err
	}
	if selected {
		return pm.ProjectPython(ctx, root)
	}
	return os.Executable()
}

// Environment of a JavaScript source build with CI set, the build python named and npm acquired
func SourceBuildEnv(ctx context.Context, baseEnv map[string]string, explicit, verify bool) (map[string]string, error) {
	python, err := sourceBuildPython(ctx)
	if err != nil {
		return nil, err
	}
	env := sourceEnviron()
	for k, v := range baseEnv {
		env[k] = v
	}
	env["CI"], env["ICEBATH_PYTHON"], env["PYTHON"] = "1", python, python
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	vcRuntime := filepath.Join(filepath.Dir(exe), "vcruntime140.dll")
	if info, err := os.Stat(vcRuntime); err == nil && info.Mode().IsRegular() {
		env["ICEBATH_VC_RUNTIME"] = vcRuntime
	}
	delete(env, "ESBUILD_BINARY_PATH")
	npmrc := filepath.Join(home.Dir(ctx), "npmrc")
	if info, err := os.Stat(npmrc); err == nil && info.Mode().IsRegular() {
		if _, ok := env["NPM_CONFIG_USERCONFIG"]; !ok {
			env["NPM_CONFIG_USERCONFIG"] = npmrc
		}
	}
	runner, err := sourceEnsure(ctx, "npm", pm.EnsureOptions{BaseEnv: env, Explicit: explicit, SkipVerify: !verify})
	if err != nil {
		return nil, err
	}
	return runner.Env, nil
}

// Options of a contained build run, the line filter, the tail indent, the working dir and env
type containedRunOptions struct {
	Hide   func(string) bool
	Indent string
	Dir    string
	Env    map[string]string
}

// Runs a checkout writing build command under the output policy inside the update custody
func RunSourceInCustody(ctx context.Context, projectRoot string, command []string, label string, opts containedRunOptions) (string, error) {
	contained, err := ContainedCommand(ctx, command, true, projectRoot)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(contained.Argv[0], contained.Argv[1:]...)
	cmd.Dir = opts.Dir
	if opts.Env != nil {
		cmd.Env = pm.EnvList(opts.Env)
	}
	cmd.ExtraFiles = contained.ExtraFiles
	cmd.SysProcAttr = contained.Attr
	out, runErr := pm.RunContained(cmd, label, pm.ContainOptions{Hide: opts.Hide, Indent: opts.Indent})
	if closeErr := contained.Close(); closeErr != nil {
		return out, closeErr
	}
	return out, runErr
}

// Error Python raises for a None argv entry when node is missing from the build PATH
type sourceNoneArgvError struct{}

func (sourceNoneArgvError) Error() string {
	return "expected str, bytes or os.PathLike object, not NoneType"
}
func (sourceNoneArgvError) PyTypeName() string { return "TypeError" }

// Error of a build env lacking the PATH key
type sourceKeyError struct{ key string }

func (e sourceKeyError) Error() string      { return fmt.Sprintf("'%s'", e.key) }
func (e sourceKeyError) PyTypeName() string { return "KeyError" }

// Node build script runner of the checkout, the run_source_script hermes tests replace
var runSourceScript = runSourceScriptIn

// Runs a node build script of the checkout under custody with npm warnings hidden from the live tail
func runSourceScriptIn(ctx context.Context, projectRoot, script string, args []string, env map[string]string, label string) error {
	path, ok := env["PATH"]
	if !ok {
		return sourceKeyError{"PATH"}
	}
	node, found := pyshutil.Which("node", &path)
	if !found {
		return sourceNoneArgvError{}
	}
	command := append([]string{node, filepath.Join(projectRoot, filepath.FromSlash(script))}, args...)
	_, err := RunSourceInCustody(ctx, projectRoot, command, label, containedRunOptions{
		Hide:   func(line string) bool { return strings.HasPrefix(strings.ToLower(line), "npm warn") },
		Indent: "  ", Dir: projectRoot, Env: env,
	})
	return err
}

// True when dependencies may be installed, an explicit build or lazy installs being on
func sourceInstallAllowed(ctx context.Context, explicit bool) bool {
	return explicit || pm.LazyInstallsAllowedFor(ctx)
}

// Dependency install flags of the node build scripts, no install allowed unless explicit or lazy installs are on
func sourceInstallArgs(ctx context.Context, explicit bool) []string {
	if sourceInstallAllowed(ctx, explicit) {
		return nil
	}
	return []string{"--no-install"}
}

// Prepares node dependencies of the workspace union, reusing a completed identical union
func PrepareSourceDependencies(ctx context.Context, projectRoot string, workspaces []string, env map[string]string, explicit bool) error {
	st := newFrontendStep("Preparing Node dependencies")
	return st.close(frontendPrepareNodeDependencies(ctx, st, projectRoot, workspaces, env, true, sourceInstallAllowed(ctx, explicit), true, nil))
}

// Prepares launch dependencies keeping an installed desktop frontend in the union
func PrepareLaunchDependencies(ctx context.Context, projectRoot string, env map[string]string) error {
	desktopDir := filepath.Join(projectRoot, "apps", "desktop")
	_, packaged := desktopPackagedExecutable(desktopDir)
	workspaces := []string{"web"}
	if desktopDistExists(desktopDir) || packaged {
		workspaces = append(workspaces, "apps/desktop")
	}
	return PrepareSourceDependencies(ctx, projectRoot, workspaces, env, false)
}

// Directory the bundled web UI of a checkout is built into and served from
func WebDistDir(projectRoot string) string {
	return filepath.Join(projectRoot, "cmd", "icebath", "web_dist")
}

// Dashboard compiler of the source builds, the web build step update tests replace with a fixture compiler
var compileSourceWeb = frontendBuildWeb

// Compiles the web UI under bounded CPU and heap with committed icons unless others are given
func BuildSourceWeb(ctx context.Context, projectRoot string, env map[string]string, icons string) error {
	ApplyWebBuildLimits(env)
	if icons == "" {
		icons = projectRoot
	}
	st := newFrontendStep("Building the web UI")
	return st.close(compileSourceWeb(ctx, st, projectRoot, icons, WebDistDir(projectRoot), env))
}

// Webapp compiler of the source builds, the webapp build step tests replace with a fixture compiler
var compileSourceWebapp = frontendBuildSourceWebapp

// Compiles the browser renderer whose dependencies live in a private workspace
func buildSourceWebapp(ctx context.Context, projectRoot string, env map[string]string, explicit bool) error {
	st := newFrontendStep("Building the Webapp renderer")
	return st.close(compileSourceWebapp(ctx, st, projectRoot, "", sourceInstallAllowed(ctx, explicit), env))
}

// Frontend workspaces the checkout carries, the web UI
func sourceFrontends(projectRoot string) []string {
	out := []string{}
	for _, name := range []string{"web"} {
		if info, err := os.Stat(filepath.Join(projectRoot, name, "package.json")); err == nil && info.Mode().IsRegular() {
			out = append(out, name)
		}
	}
	return out
}

// One failed product and its error
type productFailure struct {
	Name string
	Err  error
}

// Failures of the independent products of an update, each still attempted
type ProductBuildError struct{ Failures []productFailure }

func (e *ProductBuildError) Error() string {
	parts := make([]string, len(e.Failures))
	for i, f := range e.Failures {
		parts[i] = f.Name + ": " + failureText(f.Err)
	}
	return strings.Join(parts, "; ")
}

func (e *ProductBuildError) PyTypeName() string { return "ProductBuildError" }

// Argv and exit status of a failed command, else the exception name and message
func failureText(err error) string {
	var pmCalled *pm.CalledProcessError
	if errors.As(err, &pmCalled) {
		return fmt.Sprintf("%s exited %d", strings.Join(pmCalled.Argv, " "), pmCalled.Code)
	}
	var called *CalledProcessError
	if errors.As(err, &called) {
		return fmt.Sprintf("%s exited %d", strings.Join(called.Cmd, " "), called.ReturnCode)
	}
	var subCalled *pysubprocess.CalledProcessError
	if errors.As(err, &subCalled) {
		return fmt.Sprintf("%s exited %d", strings.Join(subCalled.Cmd, " "), subCalled.Returncode)
	}
	name := exceptionName(err)
	if text := err.Error(); text != "" {
		return name + ": " + text
	}
	return name
}

// Python exception class an error stands for
func exceptionName(err error) string {
	var named interface{ PyTypeName() string }
	if errors.As(err, &named) {
		return named.PyTypeName()
	}
	var pyErr *pm.PyError
	if errors.As(err, &pyErr) && pyErr.Class != "" {
		return pyErr.Class
	}
	if pypath.IsOSError(err) {
		return pypath.OSErrorClass(err)
	}
	return "RuntimeError"
}

// True on Linux without an X11 or Wayland display
func headlessLinux() bool {
	return runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == ""
}

// Replaces the dashboard compiler for tests of other packages, the returned function restoring the real one
func SwapWebCompilerForTests(compile func(ctx context.Context, source, icons, out string, env map[string]string) error) func() {
	prev := compileSourceWeb
	compileSourceWeb = func(ctx context.Context, _ *frontendStep, source, icons, out string, env map[string]string) error {
		return compile(ctx, source, icons, out, env)
	}
	return func() { compileSourceWeb = prev }
}

// Swaps the PM tool acquisition and installed node lookup of source builds, returning the restore
func SwapSourceToolsForTests(ensure func(ctx context.Context, name string, opts pm.EnsureOptions) (*pm.Runner, error), node func(ctx context.Context) (string, bool)) func() {
	prevEnsure, prevNode := sourceEnsure, sourceNodeBinary
	sourceEnsure, sourceNodeBinary = ensure, node
	return func() { sourceEnsure, sourceNodeBinary = prevEnsure, prevNode }
}
