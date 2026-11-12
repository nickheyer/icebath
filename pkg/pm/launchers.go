package pm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pylaunch"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Steward value of a desktop bundle install
const stewardDesktop = "desktop-app"

// Install stamp updateMechanism values a build lane may write
var updateMechanisms = []string{"self", "app-installer", "electron-updater", "external", "microsoft-store"}

// Install stamp updateMechanism values a build lane may write, as a fresh slice
func UpdateMechanisms() []string { return append([]string(nil), updateMechanisms...) }

// Commands every install publishes, in publication order
var windowsBinLaunchers = []string{"icebath", "icebath-acp"}

// WindowsBinLaunchers lists the launcher names the managed bin directory publishes
func WindowsBinLaunchers() []string { return append([]string(nil), windowsBinLaunchers...) }

// LegacyAgentEntry is the argv entry that runs the legacy single query runner the icebath-agent command opens
const LegacyAgentEntry = "__icebath_run_agent__"

// Subcommand each published command runs on the install binary
var entryPoints = map[string][]string{"icebath": nil, "icebath-acp": {"acp"}}

// Number of commands every install publishes
func EntryPointCount() int { return len(entryPoints) }

var isWindows = func() bool { return runtime.GOOS == "windows" }

// Result of a PATH exposure pass
type ExposeResult struct {
	OK      bool
	Skipped string
	Error   string
	Path    string
	Mode    string
	Written []string
}

func installBinaryName() string {
	if isWindows() {
		return "icebath.exe"
	}
	return "icebath"
}

// Install stamp of a tree as a JSON object, empty when missing, unreadable or not an object
func ReadInstallStamp(projectRoot string) *pyjson.Object {
	data, err := os.ReadFile(InstallStampPath(projectRoot))
	if err != nil {
		return &pyjson.Object{}
	}
	if strictUTF8(data) != nil {
		return &pyjson.Object{}
	}
	text := string(data)
	doc, err := pyjson.Loads(strings.TrimPrefix(text, "\ufeff"))
	if obj, ok := doc.(*pyjson.Object); err == nil && ok {
		return obj
	}
	return &pyjson.Object{}
}

func stampString(stamp *pyjson.Object, key string) (string, bool) {
	value, _ := stamp.Get(key)
	s, ok := value.(string)
	return s, ok
}

// Reports a sealed tree that ships its own binary, a stamp without a valid mechanism being a build lane error
func isSealedTree(projectRoot string) (bool, error) {
	if lexistsFollow(filepath.Join(projectRoot, ".git")) {
		return false, nil
	}
	stamp := ReadInstallStamp(projectRoot)
	if stamp.Len() == 0 {
		return false, nil
	}
	mechanism, _ := stamp.Get("updateMechanism")
	for _, known := range updateMechanisms {
		if s, ok := mechanism.(string); ok && s == known {
			return true, nil
		}
	}
	return false, &PyError{Class: "RuntimeError", Message: fmt.Sprintf(
		"install-stamp.json at %s is missing a valid 'updateMechanism' (one of %s). The build lane that wrote this stamp must pass --update-mechanism to scripts/write_install_stamp.py.",
		filepath.Dir(InstallStampPath(projectRoot)), strings.Join(updateMechanisms, ", "))}
}

// IsSealedTree exports isSealedTree
func IsSealedTree(projectRoot string) (bool, error) { return isSealedTree(projectRoot) }

func lexistsFollow(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Reports a desktop bundle payload by its stamp, a git tree never counting
func IsBundledPayload(root string) bool {
	if lexistsFollow(filepath.Join(root, ".git")) {
		return false
	}
	stamp := ReadInstallStamp(root)
	if stamp.Len() == 0 {
		return false
	}
	payload, _ := stamp.Get("payload")
	distribution, _ := stampString(stamp, "distribution")
	return truthyOrdered(payload) || distribution == stewardDesktop
}

// Install binary of a tree when present as a regular file, empty otherwise
func resolveInstallBinary(root string) string {
	path := filepath.Join(root, installBinaryName())
	if isRegularFile(path) {
		return path
	}
	return ""
}

// Install binary of a tree when present as a regular file, empty for an externally owned runtime
func ResolveInstallBinary(root string) string { return resolveInstallBinary(root) }

// Writes through a pid suffixed staging name and renames so a concurrent start never sees a torn launcher
func writeAtomic(target string, write func(staging string) error) string {
	staging := filepath.Join(filepath.Dir(target), fmt.Sprintf("%s.stage.%d", filepath.Base(target), os.Getpid()))
	if err := write(staging); err == nil {
		if err = os.Rename(staging, target); err == nil {
			return target
		}
	}
	_ = os.Remove(staging)
	return ""
}

// Directory of the checkout's own commands, the ones every home's conveniences forward to
func checkoutLauncherDir(root string) string { return filepath.Join(root, ".icebath", "bin") }

// Prefix of the line naming the tool store a checkout launcher was published for
const storeRecordPrefix = "icebath-store "

// Publishes an executable sh wrapper that execs command with the caller's arguments, recording store when set
func writeShell(target string, command []string, store string) string {
	quoted := make([]string, len(command))
	for i, part := range command {
		quoted[i] = pyshlex.Quote(part)
	}
	record := ""
	if store != "" {
		record = "# " + storeRecordPrefix + pyshlex.Quote(store) + "\n"
	}
	body := "#!/bin/sh\n" + record + "exec " + strings.Join(quoted, " ") + " \"$@\"\n"
	if st, err := os.Lstat(target); err == nil && st.Mode()&os.ModeSymlink == 0 {
		if data, err := os.ReadFile(target); err == nil && string(data) == body && executable(target) {
			return target
		}
	}
	return writeAtomic(target, func(staging string) error {
		if err := os.WriteFile(staging, []byte(body), 0o666); err != nil {
			return err
		}
		return os.Chmod(staging, 0o755)
	})
}

// Publishes a cmd wrapper running the install binary with the caller's arguments, recording store when set
func writeCmd(target, binary string, subcommand []string, store string) string {
	line := `"` + binary + `"`
	for _, part := range subcommand {
		line += " " + part
	}
	record := ""
	if store != "" {
		record = "@rem " + storeRecordPrefix + `"` + store + "\"\r\n"
	}
	body := "@echo off\r\n" + record + line + " %*\r\n"
	if data, err := os.ReadFile(target); err == nil && string(data) == body {
		return target
	}
	return writeAtomic(target, func(staging string) error { return os.WriteFile(staging, []byte(body), 0o666) })
}

// Interpreter a tool store's facts record for its python entry, empty when none is recorded or present
func storePython(runtime string) (string, error) {
	rel := filepath.Join("bin", "python3")
	if isWindows() {
		rel = "python.exe"
	}
	facts := filepath.Join(runtime, "facts.json")
	if !isRegularFile(facts) {
		return "", nil
	}
	data, err := os.ReadFile(facts)
	if err != nil {
		return "", nil
	}
	text, err := pycodecs.Decode(data, "utf-8-sig")
	if err != nil {
		return "", nil
	}
	doc, err := pyjson.Loads(text)
	if err != nil {
		return "", nil
	}
	get := func(value any, key string, fallback any) (any, error) {
		obj, ok := value.(*pyjson.Object)
		if !ok {
			return nil, &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(value))}
		}
		if got, present := obj.Get(key); present {
			return got, nil
		}
		return fallback, nil
	}
	packages, err := get(doc, "packages", &pyjson.Object{})
	if err != nil {
		return "", err
	}
	python, err := get(packages, "python", nil)
	if err != nil {
		return "", err
	}
	if !pyrepr.Truthy(python) {
		python = &pyjson.Object{}
	}
	entry, err := get(python, "entry", nil)
	if err != nil || !pyrepr.Truthy(entry) {
		return "", err
	}
	name, ok := entry.(string)
	if !ok {
		flavour := "PosixPath"
		if isWindows() {
			flavour = "WindowsPath"
		}
		return "", &PyError{Class: "TypeError", Message: fmt.Sprintf("unsupported operand type(s) for /: '%s' and '%s'", flavour, pyrepr.TypeName(entry))}
	}
	candidate := pypath.Join(runtime, name, rel)
	if isRegularFile(candidate) {
		return candidate, nil
	}
	return "", nil
}

// Store a published launcher records, the tree's own unless only the runtime store records an interpreter
func publicationStore(ctx context.Context, root string) (string, error) {
	own, err := EnvironmentStoreRoot(ctx, root, false)
	if err != nil {
		return "", err
	}
	if python, err := storePython(own); err != nil || python != "" {
		return own, err
	}
	runtime, err := EnvironmentStoreRoot(ctx, root, true)
	if err != nil {
		return "", err
	}
	if python, err := storePython(runtime); err != nil || python != "" {
		return runtime, err
	}
	return own, nil
}

// Writes one command's wrapper recording its store, the bare command in its own dir being the binary itself
func mintLauncher(name, root, outDir, binary, store string) string {
	if len(entryPoints[name]) == 0 && resolvePath(outDir) == resolvePath(root) {
		return binary
	}
	if isWindows() {
		return writeCmd(filepath.Join(outDir, name+".cmd"), binary, entryPoints[name], store)
	}
	return writeShell(filepath.Join(outDir, name), append([]string{binary}, entryPoints[name]...), store)
}

// Publishes one command bound to the binary and own store, empty when the binary is missing or writing failed
func stageLauncher(ctx context.Context, name, root, outDir string) (string, error) {
	binary := resolveInstallBinary(root)
	if binary == "" {
		return "", nil
	}
	store, err := publicationStore(ctx, root)
	if err != nil {
		return "", err
	}
	path := mintLauncher(name, root, outDir, binary, store)
	if path == "" || path == binary {
		return path, nil
	}
	exe := filepath.Join(outDir, name+".exe")
	collectRetired(exe)
	if strings.EqualFold(filepath.Ext(path), ".cmd") && !retireExe(exe) {
		return "", nil
	}
	return path, nil
}

// StageLauncher exports stageLauncher
func StageLauncher(ctx context.Context, name, root, outDir string) (string, error) {
	return stageLauncher(ctx, name, root, outDir)
}

// Removes copies an earlier publication renamed aside
func collectRetired(exe string) {
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), globEscape(filepath.Base(exe))+"*.deleteme"))
	for _, leftover := range matches {
		_ = os.Remove(leftover)
	}
}

// Removes a stale exe so the cmd wrapper wins, renaming a held exe aside first
func retireExe(exe string) bool {
	err := os.Remove(exe)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return true
	}
	aside := filepath.Join(filepath.Dir(exe), fmt.Sprintf("%s.%d.deleteme", filepath.Base(exe), os.Getpid()))
	if err := os.Rename(exe, aside); err != nil {
		return false
	}
	_ = os.Remove(aside)
	return true
}

// First command token of a cmd line, quoted or bare
func firstToken(line string) string {
	text := pystr.Strip(line)
	if text == "" {
		return ""
	}
	if text[0] == '"' || text[0] == '\'' {
		end := strings.IndexByte(text[1:], text[0])
		if end == -1 {
			return ""
		}
		return text[1 : end+1]
	}
	fields := pystr.SplitWhitespace(text, 1)
	return fields[0]
}

// Binary a published command runs, the exe itself for a binary and empty when unreadable
func launcherBinary(target string) string {
	data, err := os.ReadFile(target)
	if err != nil {
		return ""
	}
	if strings.EqualFold(filepath.Ext(target), ".exe") {
		interpreter, _ := pylaunch.ExeInterpreter(data)
		return interpreter
	}
	if strictUTF8(data) != nil {
		return ""
	}
	text := string(data)
	lines := pystr.Splitlines(strings.TrimPrefix(text, "\ufeff"), false)
	if strings.EqualFold(filepath.Ext(target), ".cmd") {
		for _, entry := range lines[min(1, len(lines)):] {
			if pystr.Strip(entry) != "" && !strings.HasPrefix(entry, "@rem "+storeRecordPrefix) {
				return firstToken(entry)
			}
		}
		return ""
	}
	for _, entry := range lines {
		if strings.HasPrefix(entry, "exec ") {
			tokens, err := pyshlex.SplitPOSIX(strings.TrimPrefix(entry, "exec "), false)
			if err != nil || len(tokens) == 0 {
				return ""
			}
			return tokens[0]
		}
	}
	return ""
}

// Tool store a checkout launcher records, a legacy exe's interpreter path, empty when it records none
func launcherStore(target string) string {
	data, err := os.ReadFile(target)
	if err != nil {
		return ""
	}
	if strings.EqualFold(filepath.Ext(target), ".exe") {
		interpreter, _ := pylaunch.ExeInterpreter(data)
		return interpreter
	}
	if strictUTF8(data) != nil {
		return ""
	}
	cmd := strings.EqualFold(filepath.Ext(target), ".cmd")
	for _, line := range pystr.Splitlines(strings.TrimPrefix(string(data), "\ufeff"), false) {
		if cmd {
			if rest, ok := strings.CutPrefix(line, "@rem "+storeRecordPrefix); ok {
				return firstToken(rest)
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "# "+storeRecordPrefix); ok {
			tokens, err := pyshlex.SplitPOSIX(rest, false)
			if err != nil || len(tokens) == 0 {
				return ""
			}
			return tokens[0]
		}
	}
	return ""
}

// Reports a record still naming what exists, a store directory or a legacy exe's interpreter file
func launcherRecordLive(target, record string) bool {
	if strings.EqualFold(filepath.Ext(target), ".exe") {
		return isRegularFile(record)
	}
	return isDirectory(record)
}

// Reports a recorded path that is the store or lies under it, both resolved
func recordUnder(record, store string) bool {
	want := resolvePath(store)
	for path := record; ; {
		if resolvePath(path) == want {
			return true
		}
		next := filepath.Dir(path)
		if next == path {
			return false
		}
		path = next
	}
}

// Install directory of the binary a published command runs
func launcherInstall(target string) string {
	binary := launcherBinary(target)
	if binary == "" {
		return ""
	}
	return filepath.Dir(binary)
}

// A command in directory that starts another install which still exists, empty when none does
func launcherOfAnotherInstall(directory, root string) string {
	for _, name := range windowsBinLaunchers {
		runs := ""
		for _, candidate := range []string{filepath.Join(directory, name+".exe"), filepath.Join(directory, name+".cmd")} {
			if isRegularFile(candidate) {
				runs = candidate
				break
			}
		}
		if runs == "" {
			continue
		}
		install, binary := launcherInstall(runs), launcherBinary(runs)
		if install != "" && binary != "" && resolvePath(install) != resolvePath(root) && isDirectory(install) && isRegularFile(binary) {
			return runs
		}
	}
	return ""
}

// Reports the install the default home sits over, uses the tool store of and owns
func isHomeInstall(ctx context.Context, root, store string) bool {
	homeRoot := resolvePath(home.Root())
	return filepath.Dir(resolvePath(root)) == homeRoot && resolvePath(store) == resolvePath(filepath.Join(homeRoot, "tools")) &&
		OwningHomeRoot(ctx, root) == ""
}

// Recognizes wrappers and links this install wrote, never a mere mention in a comment
func ownsLauncher(target, root string) bool {
	if st, err := os.Lstat(target); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return isRelativeTo(resolvePath(target), root)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return false
	}
	if strictUTF8(data) != nil {
		return false
	}
	text := string(data)
	tokens, err := pyshlex.SplitPOSIX(strings.TrimPrefix(text, "\ufeff"), true)
	if err != nil {
		return false
	}
	owned := map[string]bool{}
	for _, name := range []string{"icebath", "icebath.exe", "icebath-acp", filepath.Join(".icebath", "bin", "icebath"), filepath.Join(".icebath", "bin", "icebath-acp")} {
		owned[filepath.Join(root, name)] = true
	}
	for _, token := range tokens {
		if owned[token] {
			return true
		}
	}
	if filepath.Base(target) == "icebath-acp" {
		sibling := filepath.Join(filepath.Dir(target), "icebath")
		return len(tokens) == 4 && tokens[0] == "exec" && tokens[1] == sibling && tokens[2] == "acp" && tokens[3] == "$@" &&
			ownsLauncher(sibling, root)
	}
	return false
}

type publishedLauncher struct {
	path    string
	changed bool
}

// User bin commands forwarding to the install's own commands, each reported with whether it changed
func publishConveniences(root, outDir string, names []string, create bool) ([]publishedLauncher, error) {
	if create {
		if err := os.MkdirAll(outDir, 0o777); err != nil {
			return nil, err
		}
	} else if !isDirectory(outDir) {
		return nil, nil
	}
	var published []publishedLauncher
	for _, name := range names {
		target := filepath.Join(outDir, name)
		present := lexistsFollow(target) || isSymlink(target)
		if (!create || present) && !ownsLauncher(target, root) {
			continue
		}
		var before int64
		hadBefore := false
		if present {
			if st, err := os.Lstat(target); err == nil {
				before, hadBefore = st.ModTime().UnixNano(), true
			}
		}
		command := []string{filepath.Join(checkoutLauncherDir(root), name)}
		if name == "icebath-agent" {
			command = []string{filepath.Join(checkoutLauncherDir(root), "icebath"), LegacyAgentEntry}
		}
		if writeShell(target, command, "") == "" {
			return published, fmt.Errorf("could not publish launcher %s", target)
		}
		st, err := os.Lstat(target)
		if err != nil {
			return published, err
		}
		published = append(published, publishedLauncher{target, !hadBefore || before != st.ModTime().UnixNano()})
	}
	return published, nil
}

// Checkout launcher recording another live store than this home's, kept so a borrowing launch never rebinds it
func keptSharedLauncher(name, local, store, own string) string {
	if own == "" {
		return ""
	}
	candidates := []string{filepath.Join(local, name)}
	if isWindows() {
		candidates = []string{filepath.Join(local, name+".exe"), filepath.Join(local, name+".cmd")}
	}
	var dead []string
	for _, target := range candidates {
		if !isRegularFile(target) && !isSymlink(target) {
			continue
		}
		bound := launcherStore(target)
		if bound == "" || bound == store {
			continue
		}
		if !launcherRecordLive(target, bound) {
			dead = append(dead, target)
			continue
		}
		if recordUnder(bound, store) {
			continue
		}
		for _, stale := range dead {
			_ = os.Remove(stale)
		}
		return target
	}
	return ""
}

// Publishes the install's own commands then conveniences in outDir, empty when any own command failed
func EnsureInstallLaunchers(ctx context.Context, repoRoot, outDir string) ([]string, error) {
	root := resolvePath(repoRoot)
	local := checkoutLauncherDir(root)
	if err := os.MkdirAll(local, 0o777); err != nil {
		return nil, err
	}
	store, err := EnvironmentStoreRoot(ctx, root, true)
	if err != nil {
		return nil, err
	}
	own := resolveInstallBinary(root)
	owner := isHomeInstall(ctx, root, store)
	var written []string
	for _, name := range windowsBinLaunchers {
		if !owner {
			if kept := keptSharedLauncher(name, local, store, own); kept != "" {
				written = append(written, kept)
				continue
			}
		}
		path, err := stageLauncher(ctx, name, root, local)
		if err != nil {
			return nil, err
		}
		if path != "" {
			written = append(written, path)
		}
	}
	if resolvePath(outDir) == local {
		return written, nil
	}
	if len(written) != len(windowsBinLaunchers) {
		return nil, nil
	}
	if !isWindows() {
		published, err := publishConveniences(root, outDir, windowsBinLaunchers, true)
		if err != nil {
			return nil, err
		}
		paths := make([]string, len(published))
		for i, p := range published {
			paths[i] = p.path
		}
		return paths, nil
	}
	if err := os.MkdirAll(outDir, 0o777); err != nil {
		return nil, err
	}
	var staged []string
	for _, name := range windowsBinLaunchers {
		path, err := stageLauncher(ctx, name, root, outDir)
		if err != nil {
			return nil, err
		}
		if path != "" {
			staged = append(staged, path)
		}
	}
	return staged, nil
}

func exposeOnPathDisabled(ctx context.Context) (bool, error) {
	h := home.Dir(ctx)
	tree, err := config.LoadReadonly(ctx, h, config.NewSecrets(h).Lookup())
	if err != nil {
		return false, err
	}
	value, found := tree.Lookup("cli.expose_on_path")
	return found && !truthyOrdered(value), nil
}

// Repairs PATH conveniences without taking over another installation's files
func ExposeCLI(ctx context.Context, projectRoot string, create bool) (ExposeResult, error) {
	if projectRoot == "" {
		projectRoot = InstallRoot()
	}
	root := resolvePath(projectRoot)
	if isWindows() {
		return exposeUserBin(ctx, root, create), nil
	}
	if create {
		disabled, err := exposeOnPathDisabled(ctx)
		if err != nil {
			return ExposeResult{}, err
		}
		if disabled {
			return ExposeResult{OK: true, Skipped: "config-disabled"}, nil
		}
	}
	if IsBundledPayload(root) {
		if create && runtime.GOOS == "darwin" {
			return symlinkSealedLaunchers(filepath.Join(filepath.Dir(root), "bin")), nil
		}
		return ExposeResult{OK: true, Skipped: "bundle-owns-launchers"}, nil
	}
	if mechanism, _ := stampString(ReadInstallStamp(root), "updateMechanism"); mechanism == "external" {
		return ExposeResult{OK: true, Skipped: "externally-owned"}, nil
	}
	if resolveInstallBinary(root) == "" {
		return ExposeResult{OK: true, Skipped: "no-install-binary"}, nil
	}
	local, err := EnsureInstallLaunchers(ctx, root, checkoutLauncherDir(root))
	if err != nil {
		return ExposeResult{OK: false, Error: pypath.OSErrorText(err)}, nil
	}
	if len(local) != len(windowsBinLaunchers) {
		return ExposeResult{OK: false, Error: "source launcher publication failed"}, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ExposeResult{}, err
	}
	dirs := []string{filepath.Join(userHome, ".local", "bin")}
	for _, directory := range []string{filepath.Join(home.Root(), "bin"), "/usr/local/bin"} {
		if !containsString(dirs, directory) && (!create || ownsLauncher(filepath.Join(directory, "icebath"), root)) {
			dirs = append(dirs, directory)
		}
	}
	written := []string{}
	for _, directory := range dirs {
		published, err := publishConveniences(root, directory, append(append([]string{}, windowsBinLaunchers...), "icebath-agent"), create)
		if err != nil {
			return ExposeResult{OK: false, Error: pypath.OSErrorText(err)}, nil
		}
		for _, p := range published {
			if p.changed {
				written = append(written, filepath.Base(p.path))
			}
		}
	}
	return ExposeResult{OK: true, Written: written}, nil
}

// Links a sealed bundle's commands into the user bin, never clobbering a file or live foreign link
func symlinkSealedLaunchers(payloadBin string) ExposeResult {
	userHome, err := os.UserHomeDir()
	if err != nil {
		return ExposeResult{OK: false, Error: err.Error()}
	}
	linkDir := filepath.Join(userHome, ".local", "bin")
	payloadRoot := filepath.Dir(payloadBin)
	written := []string{}
	if err := os.MkdirAll(linkDir, 0o777); err != nil {
		return ExposeResult{OK: false, Error: pypath.OSErrorText(err)}
	}
	for _, name := range []string{"icebath", "icebath-agent", "icebath-acp"} {
		source := filepath.Join(payloadBin, name)
		if !isRegularFile(source) {
			continue
		}
		target := filepath.Join(linkDir, name)
		if isSymlink(target) {
			current, err := os.Readlink(target)
			if err != nil {
				return ExposeResult{OK: false, Error: pypath.OSErrorText(err)}
			}
			if current == source {
				continue
			}
			intoPayload := strings.HasPrefix(filepath.Clean(current), payloadRoot+string(os.PathSeparator))
			if !intoPayload && lexistsFollow(target) {
				continue
			}
		} else if lexistsFollow(target) {
			continue
		}
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ExposeResult{OK: false, Error: pypath.OSErrorText(err)}
		}
		if err := os.Symlink(source, target); err != nil {
			return ExposeResult{OK: false, Error: pypath.OSErrorText(err)}
		}
		written = append(written, name)
	}
	return ExposeResult{OK: true, Written: written, Mode: "sealed-symlinks"}
}

// Puts entry first on a semicolon PATH, false when the PATH already names it
func mergeUserPath(existing, entry string) (string, bool) {
	wanted := strings.TrimRight(entry, `\/`)
	parts := []string{entry}
	for _, part := range strings.Split(existing, ";") {
		if part == "" {
			continue
		}
		if pystr.Casefold(strings.TrimRight(part, `\/`)) == pystr.Casefold(wanted) {
			return "", false
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ";"), true
}

// Refreshes durable commands, existing PATH exposure only being repaired before dependency sync succeeds
func publishLaunchers(ctx context.Context, projectRoot string, create bool) error {
	root := projectRoot
	sealed, err := isSealedTree(root)
	if err != nil {
		return err
	}
	if sealed {
		slog.Info(fmt.Sprintf("launchers: sealed tree at %s keeps its own", root))
		return nil
	}
	if mechanism, _ := stampString(ReadInstallStamp(root), "updateMechanism"); mechanism == "external" {
		slog.Info(fmt.Sprintf("launchers: external runtime at %s keeps its own", root))
		return nil
	}
	if resolveInstallBinary(root) == "" {
		slog.Warn(fmt.Sprintf("launchers: no install binary under %s; %s not published", root, checkoutLauncherDir(root)))
		return nil
	}
	written, err := publishOwnLaunchers(ctx, root, checkoutLauncherDir(root))
	if err != nil {
		return err
	}
	if len(written) != len(entryPoints) {
		return NewInstallError("launchers", "source launcher publication failed", "retry the source update")
	}
	result, err := exposeCLI(ctx, root, create)
	if err != nil {
		return err
	}
	if !result.OK {
		slog.Warn(fmt.Sprintf("CLI exposure failed: %s", result.Error))
	}
	return nil
}

// Own command publication the source launcher refresh runs, the attribute hermes tests replace
var publishOwnLaunchers = EnsureInstallLaunchers

// Replaces the own command publication of the launcher refresh and returns the previous one
func SwapPublishOwnLaunchers(fn func(ctx context.Context, repoRoot, outDir string) ([]string, error)) func(ctx context.Context, repoRoot, outDir string) ([]string, error) {
	prev := publishOwnLaunchers
	publishOwnLaunchers = fn
	return prev
}

// PATH exposure the source launcher refresh runs, the attribute hermes tests replace
var exposeCLI = ExposeCLI

// Replaces the PATH exposure of the launcher refresh and returns the previous one
func SwapExposeCLI(fn func(ctx context.Context, projectRoot string, create bool) (ExposeResult, error)) func(ctx context.Context, projectRoot string, create bool) (ExposeResult, error) {
	prev := exposeCLI
	exposeCLI = fn
	return prev
}

// PublishLaunchers exports publishLaunchers
func PublishLaunchers(ctx context.Context, projectRoot string, create bool) error {
	return publishLaunchers(ctx, projectRoot, create)
}

// Stages the user facing commands into the default home bin and registers it on the User PATH
func exposeWindowsUserBin(ctx context.Context, root string, create bool) ExposeResult {
	directory := filepath.Join(home.Root(), "bin")
	fail := func(err error) ExposeResult { return ExposeResult{OK: false, Error: pypath.OSErrorText(err)} }
	if !create {
		published, err := publishConveniences(root, directory, windowsBinLaunchers, false)
		if err != nil {
			return fail(err)
		}
		written := []string{}
		for _, p := range published {
			if p.changed {
				written = append(written, filepath.Base(p.path))
			}
		}
		return ExposeResult{OK: true, Written: written}
	}
	store, err := EnvironmentStoreRoot(ctx, root, true)
	if err != nil {
		return fail(err)
	}
	if !isHomeInstall(ctx, root, store) {
		if other := launcherOfAnotherInstall(directory, root); other != "" {
			return ExposeResult{OK: true, Skipped: other + " starts another checkout"}
		}
	}
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return fail(err)
	}
	written, err := EnsureInstallLaunchers(ctx, root, directory)
	if err != nil {
		return fail(err)
	}
	if len(written) != len(windowsBinLaunchers) {
		return ExposeResult{OK: false, Error: "source launcher publication failed"}
	}
	names := make([]string, len(written))
	for i, path := range written {
		names[i] = filepath.Base(path)
	}
	if !ownsUserPath(filepath.Dir(directory)) {
		return ExposeResult{OK: true, Skipped: "User PATH belongs to the home new consoles use", Written: names}
	}
	state, err := registerUserPath(directory)
	if err != nil {
		return fail(err)
	}
	return ExposeResult{OK: true, Path: state, Written: names}
}

// Windows user bin exposure and User PATH registration, swapped by tests
var (
	exposeUserBin    = exposeWindowsUserBin
	registerUserPath = registerWindowsUserPath
)

// InlineStringLiteral keeps inline Python source intact through Windows PowerShell's native argv quoting, repr elsewhere
func InlineStringLiteral(value string) string {
	if !isWindows() {
		return pyrepr.StrRepr(value)
	}
	escaped := pycodecs.UnicodeEscape(value)
	return "'" + strings.NewReplacer("'", `\x27`, `"`, `\x22`).Replace(escaped) + "'"
}

// ExeIsVenvBound reports a launcher exe embedding the venv interpreter of the pre pm installer, which must be replaced
func ExeIsVenvBound(exe, venvDir string) bool {
	if venvDir == "" {
		return false
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		return false
	}
	for _, interpreter := range []string{
		filepath.Join(venvDir, "Scripts", "icebath.exe"), filepath.Join(venvDir, "Scripts", "icebath-acp.exe"),
		filepath.Join(venvDir, "Scripts", "python.exe"), filepath.Join(venvDir, "bin", "python3"),
		filepath.Join(venvDir, "bin", "icebath"), filepath.Join(venvDir, "bin", "icebath-acp"),
	} {
		if bytes.Contains(data, []byte(interpreter)) || bytes.Contains(data, utf16LEBytes(interpreter)) {
			return true
		}
	}
	return false
}

// UTF-16 little endian bytes of s
func utf16LEBytes(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 0, len(units)*2)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// Reports whether new consoles resolve this home so its commands may go on the User PATH
func ownsUserPath(root string) bool {
	consoleRoot, known := newConsoleRoot()
	if known && pathNormcase(realpath(root)) == pathNormcase(realpath(consoleRoot)) {
		return true
	}
	if !known {
		consoleRoot = "an unknown icebath home"
	}
	slog.Info(fmt.Sprintf("User PATH left unchanged: new consoles use %s, not %s", consoleRoot, root))
	return false
}
