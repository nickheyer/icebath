package terminal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	localconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/local/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

const windows = runtime.GOOS == "windows"

// Terminal temp files idle this long are pruned
const tempIdle = 24 * time.Hour

type registrant struct {
	bash *bash
	// Skills of the sessions, whose variables commands get
	skills *skills.Registry
	// Package manager, whose tools commands find behind the user's own and whose Git Bash commands run with on Windows
	packages *packages.Packages
}

func Register(registry *terminal.Registry, skillset *skills.Registry, pm *packages.Packages) {
	registry.Register(registrant{bash: &bash{packages: pm}, skills: skillset, packages: pm})
}

func (r registrant) Backends() []terminal.Backend {
	return []terminal.Backend{{
		Name:  "local",
		Title: "This machine",
		About: "Runs directly on your computer, with a persistent shell snapshot",
		Where: "the local machine",
		Check: func(*config.Config) error {
			_, err := r.bash.find()
			return err
		},
		Create: func(ctx context.Context, spec *terminal.Spec) (terminal.Environment, error) {
			return create(ctx, spec, r.bash, r.skills, r.packages)
		},
	}}
}

// Bash commands run with, looked for again only once it is gone
type bash struct {
	mu       sync.Mutex
	path     string
	packages *packages.Packages
}

func (b *bash) find() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if st, err := os.Stat(b.path); b.path != "" && err == nil && st.Mode().IsRegular() {
		return b.path, nil
	}
	path, err := findBash(b.packages.GitBash())
	if err == nil {
		b.path = path
	}
	return path, err
}

// Commands run straight on this machine, a fresh bash per command sourcing the session's snapshot
type environment struct {
	*terminal.Shell
	runner *runner
}

func create(ctx context.Context, spec *terminal.Spec, bash *bash, skillset *skills.Registry, pm *packages.Packages) (terminal.Environment, error) {
	r := &runner{spec: spec, bash: bash, skills: skillset, packages: pm}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	r.shell = terminal.NewShell(spec, r, terminal.ShellOptions{
		Backend:   "local",
		Cwd:       initialCwd(spec.Cwd),
		Timeout:   timeout,
		Persist:   spec.Config.GetBool(localconfig.Persistent),
		SudoProbe: true,
		HostFiles: true,
	})
	r.shell.Init(ctx)
	return &environment{Shell: r.shell, runner: r}, nil
}

// Background jobs run under the user's own shell as a login shell, with the environment commands get and unbuffered Python output
func (e *environment) Command(_ context.Context, script, dir string) (terminal.Command, error) {
	shell, err := userShell(e.runner.bash)
	if err != nil {
		return terminal.Command{}, err
	}
	env := e.runner.env()
	env["PYTHONUNBUFFERED"] = "1"
	key, _ := terminal.PathKey(env)
	return terminal.Command{Argv: []string{shell, "-lic", restorePath(script, env[key])}, Dir: safeCwd(dir), Env: terminal.Environ(env)}, nil
}

type runner struct {
	terminal.HostProcs
	spec     *terminal.Spec
	shell    *terminal.Shell
	bash     *bash
	skills   *skills.Registry
	packages *packages.Packages
	pruned   sync.Once
}

// Environment of a command: the host's child environment over the session's client toolchain with the variables its skills need, the
// package manager's tools behind the user's own, Git Bash's folders first on Windows
func (r *runner) env() map[string]string {
	env := r.spec.ChildEnv(r.skills)
	key, ok := terminal.PathKey(env)
	if ok {
		env[key] = r.packages.Path(env[key])
	}
	if ok && windows {
		if bash, err := r.bash.find(); err == nil {
			env[key] = gitBashFirst(bash, env[key])
		}
	}
	return env
}

// Starts bash in a session of its own with the command's environment, its group owned so it dies with icebath
func (r *runner) Run(_ context.Context, script string, login bool, _ time.Duration, stdin *string) (terminal.Proc, error) {
	bash, err := r.bash.find()
	if err != nil {
		return nil, err
	}
	env := r.env()
	args := []string{"-c"}
	if login {
		key, _ := terminal.PathKey(env)
		script = prependInit(restorePath(script, env[key]), r.initFiles())
		args = []string{"-l", "-c"}
	}
	r.recoverCwd()
	cmd := exec.Command(bash, append(args, script)...)
	cmd.Env, cmd.Dir = terminal.Environ(env), r.shell.Cwd()
	terminal.NewSession(cmd)
	p, err := terminal.Start(cmd, stdin)
	if err != nil {
		return nil, err
	}
	p.Own(r.spec.Terminal.Supervisor)
	return p, nil
}

// Removes the snapshot, the cwd file and temp files an interrupted rewrite of the snapshot left
func (r *runner) Cleanup(bool) error {
	snapshot, cwd := r.shell.Snapshot()
	stale, _ := filepath.Glob(snapshot + ".tmp.*")
	for _, path := range append([]string{snapshot, cwd}, stale...) {
		os.Remove(path)
	}
	return nil
}

// A writable temp folder safe to name in a shell: the temp folder setting, then TMPDIR, TMP and TEMP, then the profile's own
func (r *runner) TempDir() string {
	configured := strings.TrimSpace(r.spec.Config.GetString(terminalconfig.TempDir))
	cache := filepath.Join(r.spec.Dir, "cache", "terminal")
	if windows {
		for _, candidate := range []string{configured, os.Getenv("TMPDIR")} {
			if st, err := os.Stat(candidate); candidate != "" && filepath.IsAbs(candidate) && err == nil && st.IsDir() {
				return filepath.ToSlash(candidate)
			}
		}
		os.MkdirAll(cache, 0o755)
		r.prune(cache)
		return filepath.ToSlash(cache)
	}
	trimmed := func(p string) string {
		if t := strings.TrimRight(p, "/"); t != "" {
			return t
		}
		return "/"
	}
	if st, err := os.Stat(configured); strings.HasPrefix(configured, "/") && err == nil && st.IsDir() {
		return trimmed(configured)
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		if value := os.Getenv(name); strings.HasPrefix(value, "/") {
			return trimmed(value)
		}
	}
	if err := os.MkdirAll(cache, 0o755); err == nil && strings.HasPrefix(cache, "/") && terminal.Writable(cache) {
		r.prune(cache)
		return trimmed(cache)
	}
	fallback, _ := filepath.Abs(os.TempDir())
	return trimmed(fallback)
}

// Prunes the profile's terminal temp folder once per environment
func (r *runner) prune(dir string) {
	r.pruned.Do(func() { go terminal.PruneTemp(dir, tempIdle) })
}

// A cd target Git Bash understands on Windows
func (r *runner) QuoteCwd(cwd string) string { return terminal.QuoteCwd(windowsToMsys(cwd)) }

func (r *runner) QuotePath(path string) string {
	return terminal.Quote(strings.ReplaceAll(windowsToMsys(path), `\`, "/"))
}

// Keeps a folder a command ended in when it exists, in its Windows form, else the previous one
func (r *runner) Observe(_ string, cwd string) (string, bool) {
	native := msysToWindows(cwd)
	if st, err := os.Stat(native); native != "" && err == nil && st.IsDir() {
		return native, true
	}
	return "", false
}

// Moves to a usable folder when the current one vanished or can't be entered
func (r *runner) recoverCwd() {
	current := r.shell.Cwd()
	safe := safeCwd(current)
	if safe == current {
		return
	}
	if safe != msysToWindows(current) {
		slog.Warn(fmt.Sprintf("LocalEnvironment cwd %q is missing on disk; falling back to %q so terminal commands keep working.", current, safe))
	}
	r.shell.SetCwd(safe)
}

// Files sourced before a login snapshot: those the settings name, else the usual profile and bashrc files, the ones that exist
func (r *runner) initFiles() []string {
	cfg := r.spec.Config
	candidates := slices.DeleteFunc(cfg.GetStringSlice(localconfig.InitFiles), func(s string) bool { return strings.TrimSpace(s) == "" })
	if len(candidates) == 0 && cfg.GetBool(localconfig.SourceBashrc) && !windows {
		candidates = []string{"~/.profile", "~/.bash_profile", "~/.bashrc"}
	}
	var files []string
	for _, candidate := range candidates {
		if path := terminal.ExpandPath(candidate); path != "" {
			if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
				files = append(files, path)
			}
		}
	}
	return files
}

// Script preceded by quiet guarded sources of the files
func prependInit(script string, files []string) string {
	if len(files) == 0 {
		return script
	}
	prelude := []string{"set +e"}
	for _, file := range files {
		safe := strings.ReplaceAll(file, "'", `'\''`)
		prelude = append(prelude, "[ -r '"+safe+"' ] && . '"+safe+"' 2>/dev/null || true")
	}
	return strings.Join(prelude, "\n") + "\n" + script
}

// Script preceded by a line putting back the parent PATH's folders a login profile dropped
func restorePath(script, parent string) string {
	if windows {
		return script
	}
	var entries []string
	for _, dir := range strings.Split(parent, ":") {
		if dir == "" || slices.Contains(entries, terminal.Quote(dir)) {
			continue
		}
		if filepath.IsAbs(dir) {
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				continue
			}
		}
		entries = append(entries, terminal.Quote(dir))
	}
	if len(entries) == 0 {
		return script
	}
	return "for __icebath_p in " + strings.Join(entries, " ") + "; do " +
		`case ":$PATH:" in *":$__icebath_p:"*) ;; *) PATH="${PATH:+$PATH:}$__icebath_p" ;; esac; ` +
		"done; unset __icebath_p; export PATH\n" + script
}

// The folder when it can be entered, else its nearest ancestor that can, else the system temp folder
func safeCwd(cwd string) string {
	cwd = msysToWindows(cwd)
	usable := func(path string) bool {
		st, err := os.Stat(path)
		return err == nil && st.IsDir() && terminal.Searchable(path)
	}
	if cwd != "" && usable(cwd) {
		return cwd
	}
	if st, err := os.Stat(cwd); cwd != "" && err == nil && st.IsDir() {
		slog.Warn(fmt.Sprintf("Configured terminal cwd %q exists but is not accessible to this user (uid=%s) — falling back to the nearest usable directory.", cwd, uid()))
	}
	if cwd == "" {
		return os.TempDir()
	}
	for parent := filepath.Dir(cwd); ; {
		if usable(parent) {
			return parent
		}
		next := filepath.Dir(parent)
		if next == parent {
			return os.TempDir()
		}
		parent = next
	}
}

func uid() string {
	if windows {
		return os.Getenv("USERNAME")
	}
	return fmt.Sprint(os.Getuid())
}

// The folder a new environment starts in as an absolute path, a relative one that names the tail of the working folder meaning it
func initialCwd(cwd string) string {
	expanded := terminal.ExpandUser(cwd)
	if cwd == "" {
		expanded, _ = os.Getwd()
	}
	if windows {
		if expanded = msysToWindows(expanded); windowsDrive.MatchString(expanded) || strings.HasPrefix(expanded, `\\`) {
			return expanded
		}
	}
	if filepath.IsAbs(expanded) {
		return expanded
	}
	candidate, err := filepath.Abs(expanded)
	if err != nil {
		candidate = expanded
	}
	if st, err := os.Stat(candidate); err != nil || !st.IsDir() {
		current, _ := os.Getwd()
		wanted, have := parts(expanded), parts(current)
		if len(wanted) > 0 && len(wanted) <= len(have) && slices.Equal(have[len(have)-len(wanted):], wanted) {
			return current
		}
	}
	return candidate
}

func parts(path string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part != "." {
			out = append(out, part)
		}
	}
	return out
}

var (
	windowsDrive = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	nativeDrive  = regexp.MustCompile(`^([a-zA-Z]):[\\/]*(.*)$`)
	msysDrive    = regexp.MustCompile(`^/(?:(?:cygdrive|mnt)/)?([a-zA-Z])(/.*)?$`)
)

// A native drive path in Git Bash's form on Windows, unchanged elsewhere
func windowsToMsys(path string) string {
	if !windows || path == "" {
		return path
	}
	m := nativeDrive.FindStringSubmatch(path)
	if m == nil {
		return path
	}
	return "/" + strings.ToLower(m[1]) + "/" + strings.TrimLeft(strings.ReplaceAll(m[2], `\`, "/"), "/")
}

// A Git Bash drive path in its native form on Windows, unchanged elsewhere
func msysToWindows(path string) string {
	if !windows || path == "" {
		return path
	}
	m := msysDrive.FindStringSubmatch(path)
	if m == nil {
		return path
	}
	tail := strings.ReplaceAll(m[2], "/", `\`)
	if tail == "" {
		tail = `\`
	}
	return strings.ToUpper(m[1]) + ":" + tail
}

// Bash commands run with: the one on PATH, else the usual places; on Windows the package manager's Git Bash, else Git for Windows'
func findBash(managed string) (string, error) {
	executable := func(path string) bool {
		st, err := os.Stat(path)
		return err == nil && st.Mode().IsRegular() && (windows || st.Mode()&0o111 != 0)
	}
	onPath, _ := exec.LookPath("bash")
	if windows {
		if managed != "" {
			return managed, nil
		}
		for _, candidate := range windowsBash(onPath) {
			if executable(candidate) && starts(candidate) {
				return candidate, nil
			}
		}
	} else {
		if onPath != "" {
			return onPath, nil
		}
		for _, candidate := range []string{"/usr/bin/bash", "/bin/bash", os.Getenv("SHELL"), "/bin/sh"} {
			if candidate != "" && executable(candidate) {
				return candidate, nil
			}
		}
	}
	if windows {
		return "", errors.New("No shell found. icebath needs bash (Git for Windows on Windows). Run `icebath pm install git` or install Git for Windows.")
	}
	return "", errors.New("No shell found. icebath needs bash (Git for Windows on Windows): install bash so it is on PATH.")
}

// Places Git Bash lives on Windows, the override first, a PATH bash that is the WSL or Store launcher never
func windowsBash(onPath string) []string {
	var candidates []string
	if override := os.Getenv("ICEBATH_GIT_BASH_PATH"); override != "" {
		candidates = append(candidates, override)
	}
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	roots := []string{filepath.Join(programFiles, "Git")}
	if x86 := os.Getenv("ProgramFiles(x86)"); x86 != "" {
		roots = append(roots, filepath.Join(x86, "Git"))
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		roots = append(roots, filepath.Join(local, "icebath", "git"), filepath.Join(local, "Programs", "Git"))
	}
	for _, root := range roots {
		candidates = append(candidates, filepath.Join(root, "bin", "bash.exe"), filepath.Join(root, "usr", "bin", "bash.exe"))
	}
	if lower := strings.ToLower(onPath); onPath != "" && !strings.Contains(lower, "system32") && !strings.Contains(lower, "windowsapps") {
		candidates = append(candidates, onPath)
	}
	var out []string
	for _, candidate := range candidates {
		if !slices.Contains(out, candidate) {
			out = append(out, candidate)
		}
	}
	return out
}

// Whether a bash really starts, a broken install or a launcher failing here
func starts(bash string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, "-c", "exit 0")
	return cmd.Run() == nil
}

// Shells that understand how background jobs are started
var compatible = []string{"bash", "zsh", "sh", "dash", "ksh", "mksh"}

// The user's login shell for background jobs when it is of the sh family, else bash
func userShell(bash *bash) (string, error) {
	if shell := os.Getenv("SHELL"); !windows && shell != "" && slices.Contains(compatible, filepath.Base(shell)) {
		if st, err := os.Stat(shell); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return shell, nil
		}
	}
	return bash.find()
}

// PATH with the folders of the Git Bash at bash first, so its coreutils win over System32's lookalikes
func gitBashFirst(bash, path string) string {
	parent := filepath.Dir(filepath.Dir(bash))
	root := parent
	if strings.EqualFold(filepath.Base(parent), "usr") {
		root = filepath.Dir(parent)
	}
	var dirs []string
	for _, sub := range []string{"ucrt64/bin", "clangarm64/bin", "mingw64/bin", "mingw32/bin", "usr/local/bin", "usr/bin", "bin"} {
		dir := filepath.Join(root, filepath.FromSlash(sub))
		if st, err := os.Stat(dir); err == nil && st.IsDir() && !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	var rest []string
	for _, entry := range strings.Split(path, string(os.PathListSeparator)) {
		if entry != "" && !slices.ContainsFunc(dirs, func(dir string) bool { return strings.EqualFold(dir, entry) }) {
			rest = append(rest, entry)
		}
	}
	return strings.Join(append(dirs, rest...), string(os.PathListSeparator))
}
