package pm

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ActivationInputs are the checkout files deciding the dependency set, a changed mtime triggering a resync
var ActivationInputs = []string{"uv.lock", "pyproject.toml", "pkg/pm/lock.json"}

// Stamp directory beside facts.json holding the input mtimes activation was verified against
func ActivationInputsDir(ctx context.Context, projectRoot string) string {
	return filepath.Join(InstallStateDir(ctx, projectRoot), "inputs")
}

// Nanosecond mtimes of the activation inputs present in the checkout, taken before an install
func ActivationInputMtimes(projectRoot string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, name := range ActivationInputs {
		path := filepath.Join(projectRoot, filepath.FromSlash(name))
		if !isRegularFile(path) {
			continue
		}
		st, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		out[name] = st.ModTime().UnixNano()
	}
	return out, nil
}

// Rewrites the stamps so each carries the exact mtime of the input the install was verified against
func RecordActivationInputs(stamps string, mtimes map[string]int64, projectRoot string, testEnvironment bool) error {
	removeTreeQuiet(stamps)
	if err := os.MkdirAll(stamps, 0o777); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stamps, ".project-root"), []byte(resolvePath(projectRoot)), 0o666); err != nil {
		return err
	}
	if testEnvironment {
		if err := touchFile(filepath.Join(stamps, ".test-environment")); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(mtimes) {
		stamp := filepath.Join(stamps, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(stamp), 0o777); err != nil {
			return err
		}
		if err := touchFile(stamp); err != nil {
			return err
		}
		at := time.Unix(0, mtimes[name])
		if err := os.Chtimes(stamp, at, at); err != nil {
			return err
		}
	}
	return nil
}

// Environment a sealed payload ships beside its tree, empty when there is none
func PayloadVenv(projectRoot string) (string, error) {
	manifest, payloadRoot, ok := payloadManifest(resolvePath(projectRoot))
	if !ok {
		return "", nil
	}
	venv, _ := manifest["venv"].(string)
	if venv == "" {
		return "", nil
	}
	return payloadPath(payloadRoot, venv, "environment")
}

// Launcher a sealed payload records for the icebath command, empty when none is recorded
func payloadIcebathCommand(root string) (string, error) {
	manifest, payloadRoot, ok := payloadManifest(root)
	if !ok {
		return "", nil
	}
	rt, _ := manifest["runtime"].(map[string]any)
	commands, _ := rt["commands"].(map[string]any)
	command, _ := commands["icebath"].(string)
	if command == "" {
		return "", nil
	}
	return payloadPath(payloadRoot, command, "launcher")
}

// Directory holding a sealed payload's own launchers, empty for every other tree
func PayloadCommandDir(projectRoot string) (string, error) {
	command, err := payloadIcebathCommand(resolvePath(projectRoot))
	if err != nil || command == "" {
		return "", err
	}
	return filepath.Dir(command), nil
}

// Name a person types to run this install's CLI, a channel build's qualified launcher name included
func CLICommandName(projectRoot string) (string, error) {
	command, err := payloadIcebathCommand(resolvePath(projectRoot))
	if err != nil {
		return "", err
	}
	if command == "" {
		return "icebath", nil
	}
	return strings.TrimSuffix(filepath.Base(command), ".exe"), nil
}

// In tree venv or .venv directory of a checkout, venv winning, empty when neither exists
func projectVenvDir(root string) string {
	for _, name := range []string{"venv", ".venv"} {
		if candidate := filepath.Join(root, name); isDirectory(candidate) {
			return candidate
		}
	}
	return ""
}

// Payload environment, then the checkout's in tree venv, then the venv path a first install creates
func BaseVenv(projectRoot string) (string, error) {
	payload, err := PayloadVenv(projectRoot)
	if err != nil || payload != "" {
		return payload, err
	}
	root := resolvePath(projectRoot)
	if dir := projectVenvDir(root); dir != "" {
		return dir, nil
	}
	return filepath.Join(root, "venv"), nil
}

// Flushes every finished file to disk before a selection record may name the tree
func flushBeforeSelecting() error {
	if filesystemSync == nil {
		return nil
	}
	return filesystemSync()
}

// Committed environment of the install, or its original environment before the first sync
func SelectedVenv(ctx context.Context, projectRoot string) (string, error) {
	recorded, err := recordedVenv(ctx, projectRoot)
	if err != nil || recorded != "" {
		return recorded, err
	}
	return BaseVenv(projectRoot)
}

// Environment PM committed for the install or a sealed payload's own, never the pre PM in tree venv
func CommittedVenv(ctx context.Context, projectRoot string) (string, error) {
	recorded, err := recordedVenv(ctx, projectRoot)
	if err != nil || recorded != "" {
		return recorded, err
	}
	return PayloadVenv(projectRoot)
}

// Generation the install's facts select, refusing a damaged record or a tree outside the install
func recordedVenv(ctx context.Context, projectRoot string) (string, error) {
	path := EnvironmentRuntimeFactsPath(ctx, projectRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	unreadable := &PyError{Class: "RuntimeError", Message: "cannot read dependency environment: " + path}
	if err != nil {
		return "", unreadable
	}
	data = trimBOM(data)
	if utf8Strict(data) != nil {
		return "", unreadable
	}
	doc, err := loadJSON(data)
	if err != nil {
		return "", unreadable
	}
	invalid := &PyError{Class: "RuntimeError", Message: "invalid dependency environment record: " + path}
	record, ok := doc.(map[string]any)
	if !ok {
		return "", invalid
	}
	var packages any = map[string]any{}
	if v, present := record["packages"]; present {
		packages = v
	}
	packageMap, ok := packages.(map[string]any)
	if !ok {
		return "", invalid
	}
	var fact any = map[string]any{}
	if v, present := packageMap["venv"]; present {
		fact = v
	}
	factMap, ok := fact.(map[string]any)
	if !ok {
		return "", invalid
	}
	value := factMap["environment"]
	if value == nil {
		return "", nil
	}
	text, ok := value.(string)
	if !ok {
		return "", &PyError{Class: "RuntimeError", Message: "invalid dependency environment path"}
	}
	environment := resolvePath(text)
	generations := resolvePath(filepath.Join(InstallStateDir(ctx, projectRoot), "environments"))
	if !isRelativeTo(environment, generations) || !isRegularFile(filepath.Join(environment, "pyvenv.cfg")) {
		return "", &PyError{Class: "RuntimeError", Message: "dependency environment is missing or outside this install: " + environment}
	}
	return environment, nil
}

// Interpreter of the install's selected dependency environment
func ProjectPython(ctx context.Context, projectRoot string) (string, error) {
	venv, err := SelectedVenv(ctx, projectRoot)
	if err != nil {
		return "", err
	}
	return VenvPython(venv), nil
}

// Sealed payload interpreter that enters venvs through the venv entry script, empty for Termux and other trees
func payloadStorePython(root string) (string, error) {
	manifest, payloadRoot, ok := payloadManifest(root)
	if !ok {
		return "", nil
	}
	target := ""
	if v, present := manifest["target"]; present {
		target = pyStr(v)
	}
	if strings.HasSuffix(target, "-bionic") {
		return "", nil
	}
	rt, _ := manifest["runtime"].(map[string]any)
	relative, _ := rt["storePython"].(string)
	if relative == "" {
		return "", nil
	}
	return payloadPath(payloadRoot, relative, "interpreter")
}

// Argv prefix running Python inside a venv, a sealed payload attaching the venv to its own interpreter
func VenvCommand(ctx context.Context, projectRoot, venv string, options []string) ([]string, error) {
	storePython, err := payloadStorePython(resolvePath(projectRoot))
	if err != nil {
		return nil, err
	}
	if storePython == "" {
		return append([]string{VenvPython(venv)}, options...), nil
	}
	site, err := SitePackages(venv)
	if err != nil {
		return nil, err
	}
	return VenvEntryCommand(ctx, storePython, options, site)
}

// Reports Python str isdigit over a whole non empty string
func pyIsDigitString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !pyIsDigit(r) {
			return false
		}
	}
	return true
}

// Decimal text Python int gives a digit string, a ValueError for digit forms int refuses
func pyIntText(s string) (string, error) {
	for _, r := range s {
		if !isDecimalRune(r) {
			return "", &PyError{Class: "ValueError", Message: "invalid literal for int() with base 10: " + pyRepr(s)}
		}
	}
	return digitsValue(s).String(), nil
}

// Major and minor version a POSIX venv holds from pyvenv.cfg or its lib directory, ok false when neither says
func VenvPythonVersion(venv string) (major, minor string, ok bool, err error) {
	parse := func(text string) (string, string, bool, error) {
		head, rest, _ := strings.Cut(text, ".")
		tail, _, _ := strings.Cut(rest, ".")
		if !pyIsDigitString(head) || !pyIsDigitString(tail) {
			return "", "", false, nil
		}
		a, err := pyIntText(head)
		if err != nil {
			return "", "", false, err
		}
		b, err := pyIntText(tail)
		if err != nil {
			return "", "", false, err
		}
		return a, b, true, nil
	}
	if data, readErr := os.ReadFile(filepath.Join(venv, "pyvenv.cfg")); readErr == nil {
		data = trimBOM(data)
		if err := utf8Strict(data); err != nil {
			return "", "", false, err
		}
		for _, line := range pySplitLines(string(data)) {
			key, value, _ := strings.Cut(line, "=")
			if k := pyStrip(key); k != "version" && k != "version_info" {
				continue
			}
			if a, b, found, err := parse(pyStrip(value)); err != nil || found {
				return a, b, found, err
			}
		}
	}
	entries, readErr := os.ReadDir(filepath.Join(venv, "lib"))
	if readErr != nil {
		return "", "", false, nil
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "python3") || (runtime.GOOS == "windows" && strings.HasPrefix(strings.ToLower(name), "python3")) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if a, b, found, err := parse(strings.TrimPrefix(name, "python")); err != nil || found {
			return a, b, found, err
		}
	}
	return "", "", false, nil
}

// Reports a Unicode decimal digit, the only digit form Python int accepts
func isDecimalRune(r rune) bool { return unicode.Is(unicode.Nd, r) }

// Major and minor version of the pinned Python PM builds environments with, this process's interpreter
func pinnedPythonVersion() (string, string, error) {
	version, ok := currentLockfile().Version("python")
	major, rest, _ := strings.Cut(version, ".")
	minor, _, _ := strings.Cut(rest, ".")
	if !ok || major == "" || minor == "" {
		return "", "", &PyError{Class: "RuntimeError", Message: fmt.Sprintf("the lock pins no python version: %s", pyRepr(version))}
	}
	return major, minor, nil
}

// Site packages directory of a venv for the interpreter version the tree actually holds
func SitePackages(venv string) (string, error) {
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Lib", "site-packages"), nil
	}
	major, minor, ok, err := VenvPythonVersion(venv)
	if err != nil {
		return "", err
	}
	if !ok {
		if major, minor, err = pinnedPythonVersion(); err != nil {
			return "", err
		}
	}
	return filepath.Join(venv, "lib", "python"+major+"."+minor, "site-packages"), nil
}

// Trees this process resolves Python imports against, the path its Python children inherit
var importPath struct {
	sync.Mutex
	entries []string
}

// Snapshot of this process's import path
func importPathEntries() []string {
	importPath.Lock()
	defer importPath.Unlock()
	return append([]string{}, importPath.entries...)
}

// Directory of the resolved running executable, what this process runs from
var processPrefix = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(resolvePath(exe)), nil
}

// Reports whether this process resolves imports from the install's selected environment, with what it runs from
func runningFromSelectedEnvironment(ctx context.Context, projectRoot string) (bool, string, error) {
	prefix, err := processPrefix()
	if err != nil {
		return false, "", err
	}
	selected, err := SelectedVenv(ctx, projectRoot)
	if err != nil {
		return false, prefix, nil
	}
	site, err := SitePackages(selected)
	if err != nil {
		return false, prefix, nil
	}
	resolved := resolvePath(site)
	for _, entry := range importPathEntries() {
		if entry != "" && resolvePath(entry) == resolved {
			return true, prefix, nil
		}
	}
	return false, prefix, nil
}

// NoCommittedEnvironment refuses PM's store binary with nothing committed, only a repair fixes it
type NoCommittedEnvironment struct{ Message string }

func (e *NoCommittedEnvironment) Error() string { return e.Message }

// RuntimeError class callers catch it as
func (e *NoCommittedEnvironment) Unwrap() error {
	return &PyError{Class: "RuntimeError", Message: e.Message}
}

// Refuses a boot from inside the tool store with no committed environment, other binaries keep their own
func requireOwnDependencies(ctx context.Context, projectRoot string) error {
	prefix, err := processPrefix()
	if err != nil {
		return err
	}
	store, err := EnvironmentStoreRoot(ctx, projectRoot, true)
	if err != nil {
		return err
	}
	if isRelativeTo(resolvePath(prefix), resolvePath(store)) {
		return &NoCommittedEnvironment{Message: "no dependency environment is committed for this install"}
	}
	return nil
}

// Selects the committed generation at boot, leasing it and pointing child imports and PATH at it
func ActivateDependencies(ctx context.Context, projectRoot string) error {
	var environment, selected string
	if isDirectory(InstallStateDir(ctx, projectRoot)) {
		stop := false
		err := func() error {
			held, release, err := RuntimeLock(ctx, projectRoot, InstallLockTimeout)
			if err != nil {
				return err
			}
			defer release()
			if held {
				if err := recoverPublication(ctx, projectRoot); err != nil {
					return err
				}
			}
			if environment, err = CommittedVenv(ctx, projectRoot); err != nil {
				return err
			}
			if environment == "" {
				stop = true
				return requireOwnDependencies(ctx, projectRoot)
			}
			releaseLease, err := LeaseGeneration(environment)
			if err != nil {
				return err
			}
			for !held {
				current, err := CommittedVenv(ctx, projectRoot)
				if err != nil {
					return err
				}
				if current == "" || current == environment {
					break
				}
				releaseLease()
				environment = current
				if releaseLease, err = LeaseGeneration(current); err != nil {
					return err
				}
			}
			if selected, err = SitePackages(environment); err != nil {
				return err
			}
			stop = !isDirectory(selected) && !isRegularFile(EnvironmentRuntimeFactsPath(ctx, projectRoot))
			return nil
		}()
		if err != nil || stop {
			return err
		}
	} else {
		var err error
		if environment, err = PayloadVenv(projectRoot); err != nil {
			return err
		}
		if environment == "" {
			return requireOwnDependencies(ctx, projectRoot)
		}
		if selected, err = SitePackages(environment); err != nil {
			return err
		}
		if !isDirectory(selected) {
			return nil
		}
	}
	if !isDirectory(selected) {
		return &PyError{Class: "RuntimeError", Message: "dependency environment has no site-packages: " + selected}
	}
	root := resolvePath(projectRoot)
	importPath.Lock()
	entries := []string{root, selected}
	for _, entry := range importPath.entries {
		base := filepath.Base(entry)
		if base == "site-packages" || base == "dist-packages" || resolvePath(entry) == root || resolvePath(entry) == resolvePath(selected) {
			continue
		}
		entries = append(entries, entry)
	}
	importPath.entries = entries
	importPath.Unlock()
	sep := string(os.PathListSeparator)
	os.Setenv("PYTHONPATH", root+sep+selected)
	os.Unsetenv("VIRTUAL_ENV")
	commandDir, err := PayloadCommandDir(projectRoot)
	if err != nil {
		return err
	}
	if commandDir == "" {
		commandDir = filepath.Join(root, ".icebath", "bin")
	}
	directories := []string{commandDir}
	if _, _, sealed := payloadManifest(root); !(runtime.GOOS == "windows" && sealed) {
		directories = append(directories, VenvBinDir(environment, runtime.GOOS == "windows"))
	}
	var prefix []string
	for _, directory := range directories {
		if isDirectory(directory) {
			prefix = append(prefix, directory)
		}
	}
	if len(prefix) > 0 {
		os.Setenv("PATH", strings.Join(append(prefix, os.Getenv("PATH")), sep))
	}
	return nil
}

// Ordered environment, names matched case insensitively on Windows the way os environ matches them
type environList struct {
	keys   []string
	values map[string]string
}

// Index of a name in the list, minus one when absent
func (e *environList) find(name string) int {
	for i, key := range e.keys {
		if key == name || runtime.GOOS == "windows" && strings.EqualFold(key, name) {
			return i
		}
	}
	return -1
}

// Sets a name in place or appends it
func (e *environList) set(name, value string) {
	if i := e.find(name); i >= 0 {
		e.values[e.keys[i]] = value
		return
	}
	e.keys = append(e.keys, name)
	e.values[name] = value
}

// Removes a name when present
func (e *environList) pop(name string) {
	if i := e.find(name); i >= 0 {
		delete(e.values, e.keys[i])
		e.keys = append(e.keys[:i], e.keys[i+1:]...)
	}
}

// Entries as NAME=value strings in order
func (e *environList) entries() []string {
	out := make([]string, len(e.keys))
	for i, key := range e.keys {
		out[i] = key + "=" + e.values[key]
	}
	return out
}

// Composed map ordered by the process environ, keys the environ lacks following in sorted order
func orderedEnv(env map[string]string) *environList {
	list := &environList{values: map[string]string{}}
	seen := map[string]bool{}
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if value, ok := env[key]; ok && !seen[key] {
			seen[key] = true
			list.keys = append(list.keys, key)
			list.values[key] = value
		}
	}
	for _, key := range sortedKeys(env) {
		if !seen[key] {
			list.keys = append(list.keys, key)
			list.values[key] = env[key]
		}
	}
	return list
}

// Environment of the installed PM packages with the committed site packages and the activation sentinel
func ActivationEnvironment(ctx context.Context, projectRoot string) ([]string, error) {
	composed, err := envFor(ctx, AllPackages(), nil)
	if err != nil {
		return nil, err
	}
	environment, err := CommittedVenv(ctx, projectRoot)
	if err != nil {
		return nil, err
	}
	env := orderedEnv(composed)
	env.pop("PYTHONHOME")
	env.pop("VIRTUAL_ENV")
	pythonPath := resolvePath(projectRoot)
	if environment != "" {
		site, err := SitePackages(environment)
		if err != nil {
			return nil, err
		}
		pythonPath += string(os.PathListSeparator) + site
	}
	env.set("PYTHONPATH", pythonPath)
	env.set("__ICEBATH_ACTIVATED", EnvironmentRuntimeFactsPath(ctx, projectRoot))
	testPython, err := TestenvPython(ctx, projectRoot)
	if err != nil {
		return nil, err
	}
	if testPython != "" {
		env.set("__ICEBATH_TEST_PYTHON", testPython)
	}
	return env.entries(), nil
}

// Quotes a value for fish, where only backslash and the single quote are special inside quotes
func fishQuote(value string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", `\'`) + "'"
}

// Names fish refuses to assign, each inherited unchanged or owned by fish itself
var fishReadOnly = map[string]bool{
	"PWD": true, "SHLVL": true, "_": true, "status": true, "version": true, "hostname": true, "fish_pid": true,
	"history": true, "pipestatus": true, "status_generation": true, "umask": true, "FISH_VERSION": true,
}

// Export statement and unassignable names of each shell dialect
var shellDialects = map[string]struct {
	statement func(name, value string) string
	readOnly  map[string]bool
}{
	"sh":   {func(name, value string) string { return "export " + name + "=" + shellQuote(value) }, map[string]bool{}},
	"fish": {func(name, value string) string { return "set -gx " + name + " " + fishQuote(value) }, fishReadOnly},
}

var shellIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Environment entries as a script the dialect's shell evaluates, unassignable names passed over
func ShellExports(env []string, dialect string) (string, error) {
	shell, ok := shellDialects[dialect]
	if !ok {
		return "", &PyError{Class: "KeyError", Message: pyRepr(dialect)}
	}
	var lines []string
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		if shellIdentifier.MatchString(name) && !shell.readOnly[name] {
			lines = append(lines, shell.statement(name, value))
		}
	}
	return strings.Join(lines, "\n"), nil
}

// Usage line of the activation environment printer
func environmentsUsage(prog string, t argTheme) string {
	return fmt.Sprintf("%susage: %s%s%s%s [%s-h%s] [%s--format%s %s{json,sh,fish}%s]\n", t.usage, t.reset, t.prog, prog, t.reset,
		t.summaryShort, t.reset, t.summaryLong, t.reset, t.summaryLabel, t.reset)
}

// Help text of the activation environment printer in argparse layout
func environmentsHelp(prog string, t argTheme) string {
	return environmentsUsage(prog, t) + "\nPrint the composed activation environment.\n\n" +
		fmt.Sprintf("%soptions:%s\n", t.heading, t.reset) +
		fmt.Sprintf("  %s-h%s, %s--help%s            show this help message and exit\n", t.shortOption, t.reset, t.longOption, t.reset) +
		fmt.Sprintf("  %s--format%s %s{json,sh,fish}%s\n", t.longOption, t.reset, t.label, t.reset)
}

// Parses the printer's arguments with argparse semantics, returning the format or an exit code
func parseEnvironmentsArgs(argv []string, prog string, stdout, stderr io.Writer) (string, int, bool) {
	t := argparseTheme(stdout)
	fail := func(message string) (string, int, bool) {
		io.WriteString(stderr, environmentsUsage(prog, argparseTheme(stderr)))
		fmt.Fprintf(stderr, "%s: error: %s\n", prog, message)
		return "", 2, false
	}
	format := "json"
	var unknown []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			unknown = append(unknown, argv[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			unknown = append(unknown, arg)
			continue
		}
		name, value, explicit := strings.Cut(arg, "=")
		option := ""
		switch {
		case arg == "-h" || name == "--help" || strings.HasPrefix(arg, "--h") && strings.HasPrefix("--help", name):
			option = "help"
		case strings.HasPrefix(arg, "-h"):
			return fail(fmt.Sprintf("argument -h/--help: ignored explicit argument %s", pyRepr(arg[2:])))
		case strings.HasPrefix(name, "--f") && strings.HasPrefix("--format", name):
			option = "format"
		}
		switch option {
		case "help":
			if explicit {
				return fail(fmt.Sprintf("argument -h/--help: ignored explicit argument %s", pyRepr(value)))
			}
			io.WriteString(stdout, environmentsHelp(prog, t))
			return "", 0, false
		case "format":
			if !explicit {
				if i+1 >= len(argv) || len(argv[i+1]) > 1 && argv[i+1][0] == '-' && !negativeNumber.MatchString(argv[i+1]) {
					return fail("argument --format: expected one argument")
				}
				i++
				value = argv[i]
			}
			if _, ok := shellDialects[value]; !ok && value != "json" {
				return fail(fmt.Sprintf("argument --format: invalid choice: %s (choose from json, sh, fish)", pyRepr(value)))
			}
			format = value
		default:
			unknown = append(unknown, arg)
		}
	}
	if len(unknown) > 0 {
		return fail("unrecognized arguments: " + strings.Join(unknown, " "))
	}
	return format, 0, true
}

// Prints the composed activation environment as JSON or a shell script, the pm env entry the CLI binds
func EnvironmentsMain(ctx context.Context, argv []string, prog string, stdout, stderr io.Writer) int {
	format, code, ok := parseEnvironmentsArgs(argv, prog, stdout, stderr)
	if !ok {
		return code
	}
	env, err := ActivationEnvironment(ctx, RepoRoot())
	if err != nil {
		fmt.Fprintln(stderr, pyTraceback(err))
		return 1
	}
	if format != "json" {
		script, err := ShellExports(env, format)
		if err != nil {
			fmt.Fprintln(stderr, pyTraceback(err))
			return 1
		}
		fmt.Fprintln(stdout, script)
		return 0
	}
	object := NewOrderedObject()
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		object.Set(name, value)
	}
	data, err := dumpsJSON(object, false)
	if err != nil {
		fmt.Fprintln(stderr, pyTraceback(err))
		return 1
	}
	fmt.Fprintln(stdout, string(data))
	return 0
}

// Value of a journal row key the way subscripting a decoded object reads it
func rowItem(row any, key string) (any, error) {
	m, ok := row.(map[string]any)
	if !ok {
		return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("'%s' object is not subscriptable", pyTypeName(row))}
	}
	v, present := m[key]
	if !present {
		return nil, &PyError{Class: "KeyError", Message: pyRepr(key)}
	}
	return v, nil
}

// Path text of a journal value, a TypeError for anything but a string
func rowPath(row any, key string) (string, error) {
	v, err := rowItem(row, key)
	if err != nil {
		return "", err
	}
	s, ok := v.(string)
	if !ok {
		return "", &PyError{Class: "TypeError", Message: fmt.Sprintf("argument should be a str or an os.PathLike object, not '%s'", pyTypeName(v))}
	}
	return s, nil
}

// Bytes of a strict base64 journal value, nil for JSON null
func rowBase64(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, &PyError{Class: "TypeError", Message: fmt.Sprintf("argument should be a bytes-like object or ASCII string, not '%s'", pyTypeName(v))}
	}
	out, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, &PyError{Class: "ValueError", Message: err.Error()}
	}
	if out == nil {
		out = []byte{}
	}
	return out, nil
}

// Path with empty and dot components dropped the way pathlib spells it
func purePath(p string) string {
	if p == "" {
		return "."
	}
	volume := filepath.VolumeName(p)
	rest := p[len(volume):]
	rooted := rest != "" && os.IsPathSeparator(rest[0])
	var parts []string
	for _, part := range splitPathComponents(rest) {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	joined := strings.Join(parts, string(filepath.Separator))
	if rooted {
		joined = string(filepath.Separator) + joined
	}
	if volume+joined == "" {
		return "."
	}
	return volume + joined
}

// Hex digest of a file as a JSON value, nil when it does not exist
func digestValue(path string) (any, error) {
	digest, ok, err := FileDigest(path)
	if err != nil || !ok {
		return nil, err
	}
	return digest, nil
}

// Removes a tree, making read only entries writable and retrying each refused removal once
func rmtreeForce(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if st.Mode()&fs.ModeSymlink != 0 || IsJunction(path) {
		_ = os.Chmod(path, 0o200)
		return nil
	}
	return removeForce(path)
}

// Depth first removal whose failed steps chmod the entry writable and run again
func removeForce(dir string) error {
	retry := func(p string, op func() error) error {
		if err := op(); err != nil {
			_ = os.Chmod(p, 0o200)
			return op()
		}
		return nil
	}
	var entries []os.DirEntry
	if err := retry(dir, func() (err error) { entries, err = os.ReadDir(dir); return err }); err != nil {
		return err
	}
	for _, entry := range entries {
		child := filepath.Join(dir, entry.Name())
		if entry.IsDir() && !IsJunction(child) {
			if err := removeForce(child); err != nil {
				return err
			}
			continue
		}
		if err := retry(child, func() error { return os.Remove(child) }); err != nil {
			return err
		}
	}
	return retry(dir, func() error { return os.Remove(dir) })
}

// Rolls back or settles an interrupted plugin code publication from its journal row
func recoverPluginPublication(ctx context.Context, project string, row map[string]any, journal string) error {
	target, err := rowPath(row, "target")
	if err != nil {
		return err
	}
	backup, err := rowPath(row, "backup")
	if err != nil {
		return err
	}
	metadata, err := rowPath(row, "metadata")
	if err != nil {
		return err
	}
	target, backup, metadata = purePath(target), purePath(backup), purePath(metadata)
	homeRoot := resolvePath(DependencyHomeRoot(ctx))
	parent := filepath.Dir(target)
	if !isRelativeTo(resolvePath(target), homeRoot) || filepath.Base(parent) != "plugins" || filepath.Dir(backup) != parent ||
		!strings.HasPrefix(filepath.Base(backup), ".previous-") || metadata != filepath.Join(parent, ".install-metadata.json") {
		return &PyError{Class: "ValueError", Message: "plugin publication paths escape their home"}
	}
	committed := truthy(row["committed"])
	if !committed {
		before, err := rowItem(row, "facts_before")
		if err != nil {
			return err
		}
		digest, err := digestValue(EnvironmentRuntimeFactsPath(ctx, project))
		if err != nil {
			return err
		}
		committed = !jsonEquals(digest, before)
	}
	if committed {
		if pathExists(backup) {
			if err := rmtreeForce(backup); err != nil {
				return err
			}
		}
	} else {
		rawBefore, err := rowItem(row, "metadata_before")
		if err != nil {
			return err
		}
		old, err := rowBase64(rawBefore)
		if err != nil {
			return err
		}
		current, _, err := ReadBytesOrNone(metadata)
		if err != nil {
			return err
		}
		rawAfter, err := rowItem(row, "metadata_after")
		if err != nil {
			return err
		}
		if rawAfter == nil {
			return &PyError{Class: "TypeError", Message: "argument should be a bytes-like object or ASCII string, not 'NoneType'"}
		}
		updated, err := rowBase64(rawAfter)
		if err != nil {
			return err
		}
		sameBytes := func(a, b []byte) bool { return (a == nil) == (b == nil) && string(a) == string(b) }
		if !sameBytes(current, old) && !sameBytes(current, updated) {
			return &PyError{Class: "ValueError", Message: "plugin metadata changed after publication; preserve it for manual recovery"}
		}
		if pathExists(backup) {
			if pathExists(target) {
				if err := rmtreeForce(target); err != nil {
					return err
				}
			}
			if err := renameFile(backup, target); err != nil {
				return err
			}
		} else {
			existed, err := rowItem(row, "target_existed")
			if err != nil {
				return err
			}
			if !truthy(existed) && pathExists(target) {
				if err := rmtreeForce(target); err != nil {
					return err
				}
			}
		}
		if old == nil {
			if err := os.Remove(metadata); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		} else if err := DurableWriteBytes(metadata, old); err != nil {
			return err
		}
	}
	return os.Remove(journal)
}

// Configs one journal row restores, each with its previous bytes and the digest written after publication
type journalConfig struct {
	path     string
	previous []byte
	after    any
}

// Rolls back or settles the publication journal row of one checkout
func recoverJournal(ctx context.Context, project, journal string, data []byte) error {
	doc, err := loadJSON(data)
	if err != nil {
		return &PyError{Class: "ValueError", Message: err.Error()}
	}
	row, ok := doc.(map[string]any)
	if !ok {
		return &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(doc))}
	}
	if kind, _ := row["kind"].(string); kind == "plugin" {
		return recoverPluginPublication(ctx, project, row, journal)
	}
	entries := []any{row}
	if raw, present := row["configs"]; present {
		if entries, err = pyIterate(raw); err != nil {
			return err
		}
	}
	homeRoot := resolvePath(DependencyHomeRoot(ctx))
	var configs []journalConfig
	for _, entry := range entries {
		config, err := rowPath(entry, "config")
		if err != nil {
			return err
		}
		config = purePath(config)
		if filepath.Base(config) != "config.yaml" || !isRelativeTo(resolvePath(config), homeRoot) {
			return &PyError{Class: "ValueError", Message: "config path is outside icebath state"}
		}
		raw, err := rowItem(entry, "previous")
		if err != nil {
			return err
		}
		previous, err := rowBase64(raw)
		if err != nil {
			return err
		}
		configs = append(configs, journalConfig{path: config, previous: previous, after: entry.(map[string]any)["config_after"]})
	}
	if !truthy(row["committed"]) {
		before, err := rowItem(row, "facts_before")
		if err != nil {
			return err
		}
		digest, err := digestValue(EnvironmentRuntimeFactsPath(ctx, project))
		if err != nil {
			return err
		}
		if jsonEquals(digest, before) {
			for _, c := range configs {
				var prior any
				if c.previous != nil {
					sum := sha256.Sum256(c.previous)
					prior = hex.EncodeToString(sum[:])
				}
				current, err := digestValue(c.path)
				if err != nil {
					return err
				}
				if !jsonEquals(current, prior) && !jsonEquals(current, c.after) {
					return &PyError{Class: "ValueError", Message: "config changed after publication began; preserve it for manual recovery"}
				}
			}
			for _, c := range configs {
				if c.previous == nil {
					if err := os.Remove(c.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
						return err
					}
				} else if err := DurableWriteBytes(c.path, c.previous); err != nil {
					return err
				}
			}
		}
	}
	return os.Remove(journal)
}

// Text Python's str gives a recovery failure, OS errors spelled the way OSError prints them
func pyExceptionText(err error) string {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) {
		return pyOSErrorText(err)
	}
	return err.Error()
}

// Recovers an interrupted publication under the held dependency lock, before activation or another write
func recoverPublication(ctx context.Context, project string) error {
	journal := filepath.Join(InstallStateDir(ctx, project), "publication.json")
	data, ok, err := ReadBytesOrNone(journal)
	if err != nil || !ok {
		return err
	}
	if err := recoverJournal(ctx, project, journal, data); err != nil {
		if class := errorClass(err); class == "AttributeError" || class == "RuntimeError" {
			return err
		}
		return &PyError{Class: "RuntimeError", Message: fmt.Sprintf("cannot recover dependency publication: %s: %s", journal, pyExceptionText(err))}
	}
	return nil
}

// Marks the journal committed so recovery keeps a publication that changed code or config without a generation
func finishPublication(ctx context.Context, project string) error {
	journal := filepath.Join(InstallStateDir(ctx, project), "publication.json")
	data, err := os.ReadFile(journal)
	if err != nil {
		return err
	}
	doc, err := loadJSON(data)
	if err != nil {
		return &PyError{Class: "ValueError", Message: err.Error()}
	}
	row, ok := doc.(map[string]any)
	if !ok {
		return &PyError{Class: "TypeError", Message: fmt.Sprintf("'%s' object does not support item assignment", pyTypeName(doc))}
	}
	row["committed"] = true
	encoded, err := dumpsJSON(row, false)
	if err != nil {
		return err
	}
	if err := publicationWrite(journal, encoded); err != nil {
		return err
	}
	return recoverPublication(ctx, project)
}

// Removes unselected lease managed generations older than minAge once no reader holds them
func collectGenerations(ctx context.Context, project string, minAge time.Duration) ([]string, error) {
	var removed []string
	root := InstallStateDir(ctx, project)
	if _, err := os.Lstat(root); err != nil {
		return removed, nil
	}
	held, release, err := RuntimeLock(ctx, project, InstallLockTimeout)
	if err != nil {
		return nil, err
	}
	defer release()
	if !held {
		return removed, nil
	}
	if err := recoverPublication(ctx, project); err != nil {
		return nil, err
	}
	venv, err := SelectedVenv(ctx, project)
	if err != nil {
		return nil, err
	}
	selected := resolvePath(filepath.Dir(venv))
	generations := filepath.Join(root, "environments")
	if !isDirectory(generations) {
		return removed, nil
	}
	entries, err := os.ReadDir(generations)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		generation := filepath.Join(generations, entry.Name())
		if st, err := os.Lstat(generation); err != nil || st.Mode()&fs.ModeSymlink != 0 || !isDirectory(generation) || resolvePath(generation) == selected {
			continue
		}
		marker, err := os.Stat(filepath.Join(generation, ".lease-managed"))
		if err != nil || !marker.Mode().IsRegular() || time.Since(marker.ModTime()) < minAge {
			continue
		}
		leased, err := LeasesHeld(generation)
		if err != nil {
			return nil, err
		}
		if !leased {
			if err := RemoveTree(generation); err != nil {
				return nil, err
			}
			removed = append(removed, generation)
		}
	}
	return removed, nil
}

// Reports an error Python raises as OSError or ValueError
func isOSOrValueError(err error) bool {
	var pathErr *fs.PathError
	var linkErr *os.LinkError
	var syscallErr *os.SyscallError
	if errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &syscallErr) {
		return true
	}
	switch errorClass(err) {
	case "OSError", "FileNotFoundError", "FileExistsError", "PermissionError", "ValueError", "UnicodeDecodeError", "TOMLDecodeError":
		return true
	}
	return false
}

// Collects the dependency and PM runtime generations a publish superseded, never failing the committed update
func CollectSupersededGenerations(ctx context.Context, projectRoot string) error {
	removed, err := collectGenerations(ctx, projectRoot, 24*time.Hour)
	if err == nil {
		var runtimes []string
		runtimes, err = CollectRuntimeGenerations(filepath.Join(InstallStateDir(ctx, projectRoot), "pm-runtime"))
		removed = append(removed, runtimes...)
	}
	if err != nil {
		if isOSOrValueError(err) {
			slog.Warn(fmt.Sprintf("dependency generation cleanup skipped: %s", pyExceptionText(err)))
			return nil
		}
		return err
	}
	if len(removed) > 0 {
		slog.Info(fmt.Sprintf("collected %d unused dependency generations", len(removed)))
	}
	return nil
}
