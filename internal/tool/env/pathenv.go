package env

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/shell"
)

const sanePath = "/opt/homebrew/bin:/opt/homebrew/sbin:" +
	"/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func normcase(p string) string {
	if windowsHost {
		return strings.ToLower(strings.ReplaceAll(p, "/", `\`))
	}
	return p
}

func splitPathList(p string) []string {
	var out []string
	for _, e := range strings.Split(p, string(os.PathListSeparator)) {
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}

func prependMissingPathEntries(existing string, dirs []string) string {
	entries := splitPathList(existing)
	have := map[string]bool{}
	for _, e := range entries {
		have[e] = true
	}
	var missing []string
	for _, d := range dirs {
		if !have[d] {
			missing = append(missing, d)
		}
	}
	if len(missing) == 0 {
		return existing
	}
	return strings.Join(append(missing, entries...), string(os.PathListSeparator))
}

func (h *Host) gitBashBinDirs(ctx context.Context) []string {
	h.bashOnce.Do(func() {
		if !windowsHost {
			return
		}
		bash, err := h.FindBash(ctx)
		if err != nil {
			return
		}
		parent := filepath.Dir(filepath.Dir(bash))
		root := parent
		if strings.ToLower(filepath.Base(parent)) == "usr" {
			root = filepath.Dir(parent)
		}
		seen := map[string]bool{}
		for _, sub := range []string{"mingw64/bin", "mingw32/bin", "usr/local/bin", "usr/bin", "bin"} {
			d := filepath.Join(append([]string{root}, strings.Split(sub, "/")...)...)
			if isDir(d) && !seen[d] {
				seen[d] = true
				h.bashDirs = append(h.bashDirs, d)
			}
		}
	})
	return h.bashDirs
}

func (h *Host) userToolsFirst(ctx context.Context, existing string) (string, error) {
	path, err := h.toolchain.UserFirstPath(ctx, existing)
	if err != nil {
		return "", err
	}
	bashDirs := h.gitBashBinDirs(ctx)
	if len(bashDirs) == 0 {
		return path, nil
	}
	keys := map[string]bool{}
	for _, d := range bashDirs {
		keys[normcase(d)] = true
	}
	out := append([]string{}, bashDirs...)
	for _, p := range strings.Split(path, string(os.PathListSeparator)) {
		if p != "" && !keys[normcase(p)] {
			out = append(out, p)
		}
	}
	return strings.Join(out, string(os.PathListSeparator)), nil
}

func (h *Host) resolveBinDir() string {
	h.binOnce.Do(func() {
		candidate := ""
		if which, err := exec.LookPath("icebath"); err == nil {
			candidate = filepath.Dir(which)
		} else if exe, err := os.Executable(); err == nil {
			base := strings.ToLower(filepath.Base(exe))
			if base == "icebath" || strings.HasPrefix(base, "icebath.") {
				candidate = filepath.Dir(exe)
			}
		}
		if candidate != "" && isDir(candidate) {
			h.binDir = candidate
		}
	})
	return h.binDir
}

func (h *Host) prependBinDir(existing string) string {
	dir := h.resolveBinDir()
	if dir == "" {
		return prependMissingPathEntries(existing, nil)
	}
	return prependMissingPathEntries(existing, []string{dir})
}

func (h *Host) managedRuntimeEntries(ctx context.Context) []string {
	dirs, err := h.toolchain.RuntimeDirs(ctx)
	if err != nil {
		slog.Debug("managed runtime directories unavailable for the terminal PATH", "err", err)
		return nil
	}
	homeDir, err := h.homes.Home(ctx)
	if err != nil {
		slog.Debug("managed runtime directories unavailable for the terminal PATH", "err", err)
		return nil
	}
	var out []string
	for _, d := range append(dirs, home.Bin.In(homeDir)) {
		if isDir(d) {
			out = append(out, d)
		}
	}
	return out
}

func userLocalBinEntries() []string {
	userHome, err := home.UserHome()
	if err != nil {
		return nil
	}
	local := filepath.Join(userHome, ".local", "bin")
	if isDir(local) {
		return []string{local}
	}
	return nil
}

func (h *Host) appendMissingSanePath(ctx context.Context, existing string) string {
	if windowsHost {
		return existing
	}
	var ordered []string
	seen := map[string]bool{}
	add := func(e string) {
		if e != "" && !seen[e] {
			seen[e] = true
			ordered = append(ordered, e)
		}
	}
	for _, e := range strings.Split(existing, ":") {
		add(e)
	}
	for _, e := range strings.Split(sanePath, ":") {
		add(e)
	}
	for _, e := range h.managedRuntimeEntries(ctx) {
		add(e)
	}
	for _, e := range userLocalBinEntries() {
		add(e)
	}
	return strings.Join(ordered, ":")
}

func (h *Host) shellInitConfig(ctx context.Context) ([]string, bool) {
	cfg, err := h.cfg.Load(ctx)
	if err != nil {
		return nil, true
	}
	var files []string
	for _, f := range cfg.Terminal.ShellInitFiles {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, cfg.Terminal.AutoSourceBashrc
}

var posixVarRef = regexp.MustCompile(`\$(\w+|\{[^}]*\})`)

var windowsVarRef = regexp.MustCompile(`%([^%]+)%`)

func expandVars(p string) string {
	if !strings.Contains(p, "$") && !(windowsHost && strings.Contains(p, "%")) {
		return p
	}
	p = posixVarRef.ReplaceAllStringFunc(p, func(ref string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(ref, "$"), "{"), "}")
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return ref
	})
	if windowsHost {
		p = windowsVarRef.ReplaceAllStringFunc(p, func(ref string) string {
			if v, ok := os.LookupEnv(strings.Trim(ref, "%")); ok {
				return v
			}
			return ref
		})
	}
	return p
}

func (h *Host) shellInitFiles(ctx context.Context) []string {
	explicit, auto := h.shellInitConfig(ctx)
	candidates := explicit
	if len(candidates) == 0 && auto && !windowsHost {
		candidates = []string{"~/.profile", "~/.bash_profile", "~/.bashrc"}
	}
	var out []string
	for _, raw := range candidates {
		if p := expandVars(home.ExpandUser(raw)); p != "" && isFile(p) {
			out = append(out, p)
		}
	}
	return out
}

func prependShellInit(command string, files []string) string {
	if len(files) == 0 {
		return command
	}
	prelude := []string{"set +e"}
	for _, p := range files {
		safe := strings.ReplaceAll(p, "'", `'\''`)
		prelude = append(prelude, "[ -r '"+safe+"' ] && . '"+safe+"' 2>/dev/null || true")
	}
	return strings.Join(prelude, "\n") + "\n" + command
}

func DroppedPathPrelude(command, parentPath string) string {
	if windowsHost {
		return command
	}
	var entries []string
	seen := map[string]bool{}
	for _, d := range strings.Split(parentPath, ":") {
		if seen[d] {
			continue
		}
		seen[d] = true
		if d != "" && (!filepath.IsAbs(d) || isDir(d)) {
			entries = append(entries, d)
		}
	}
	if len(entries) == 0 {
		return command
	}
	quoted := make([]string, len(entries))
	for i, d := range entries {
		quoted[i] = shell.Quote(d)
	}
	restore := "for __icebath_p in " + strings.Join(quoted, " ") + "; do " +
		`case ":$PATH:" in *":$__icebath_p:"*) ;; *) PATH="${PATH:+$PATH:}$__icebath_p" ;; esac; ` +
		"done; unset __icebath_p; export PATH"
	return restore + "\n" + command
}
