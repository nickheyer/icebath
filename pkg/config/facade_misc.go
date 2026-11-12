package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
)

// Source tree the running code was built from, else the directory holding the running binary
func GetProjectRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if ok && filepath.IsAbs(file) {
		root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			return resolved
		}
	}
	exe, exeErr := os.Executable()
	if exeErr == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
	}
	wd, _ := os.Getwd()
	if ok && !filepath.IsAbs(file) {
		var starts []string
		if exeErr == nil {
			starts = append(starts, filepath.Dir(exe))
		}
		if wd != "" {
			starts = append(starts, wd)
		}
		for _, start := range starts {
			if root, found := trimmedSourceRoot(start, filepath.ToSlash(file)); found {
				return root
			}
		}
	}
	if exeErr != nil {
		return wd
	}
	return filepath.Dir(exe)
}

// Nearest ancestor whose go.mod module joined with the trimmed path names an existing source file
func trimmedSourceRoot(start, trimmed string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for {
		if module := goModModulePath(filepath.Join(dir, "go.mod")); module != "" && strings.HasPrefix(trimmed, module+"/") {
			source := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(trimmed, module+"/")))
			if info, err := os.Stat(source); err == nil && info.Mode().IsRegular() {
				return dir, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// Module path a go.mod file declares, empty when the file is unreadable or declares none
func goModModulePath(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			if unquoted, err := strconv.Unquote(fields[1]); err == nil {
				return unquoted
			}
			return fields[1]
		}
	}
	return ""
}

// Container mode metadata the NixOS activation wrote, nil when off, inside the container or in dev mode
type ContainerExecInfo struct {
	Backend       string
	ContainerName string
	ExecUser      string
	IcebathBin    string
}

// Reads the home container mode file, only a missing file reads as container mode off
func GetContainerExecInfo(h string) (*ContainerExecInfo, error) {
	if os.Getenv("ICEBATH_DEV") == "1" || hostinfo.IsContainer() {
		return nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(h, ".container-mode"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info := map[string]string{}
	for _, line := range readEnvTextLines(raw) {
		line = strings.TrimFunc(line, isPySpace)
		if strings.Contains(line, "=") && !strings.HasPrefix(line, "#") {
			key, value, _ := strings.Cut(line, "=")
			info[strings.TrimFunc(key, isPySpace)] = strings.TrimFunc(value, isPySpace)
		}
	}
	get := func(key, def string) string {
		if v, ok := info[key]; ok {
			return v
		}
		return def
	}
	return &ContainerExecInfo{
		Backend:       get("backend", "docker"),
		ContainerName: get("container_name", "icebath-agent"),
		ExecUser:      get("exec_user", "icebath"),
		IcebathBin:    get("icebath_bin", "/data/current-package/bin/icebath"),
	}, nil
}

// Refuses an existing invalid config before a non interactive run, missing and empty files pass
func RequireParseableUserConfig(ctx context.Context, h string, ignoreUserConfig bool) error {
	if ignoreUserConfig || os.Getenv("ICEBATH_IGNORE_USER_CONFIG") == "1" {
		return nil
	}
	configPath := home.ConfigPath(h)
	data, parseErr := ReadConfigDoc(ctx, configPath)
	if errors.Is(parseErr, fs.ErrNotExist) {
		return nil
	}
	if parseErr == nil {
		if _, isMap := data.(map[string]any); data == nil || isMap {
			return nil
		}
		parseErr = fmt.Errorf("top-level YAML value must be a mapping, got %s", PyTypeName(data))
	}
	backup, berr := BackupConfig(ctx, configPath, "corrupt", DefaultBackupKeep)
	if berr != nil {
		return berr
	}
	where := YAMLErrorLocation(parseErr)
	at := ""
	if where != "" {
		at = " at " + where
	}
	message := fmt.Sprintf("Icebath stopped because your settings file (%s) has a formatting error%s. Fix it with "+
		"`icebath config edit` and check with `icebath config check`, or add --ignore-user-config to run once with default settings.",
		configPath, at)
	if backup != "" {
		message += " A copy of the broken file is at " + backup + "."
	}
	message += " Details: " + YAMLErrorDetails(parseErr)
	slog.Error(message)
	return &InvalidUserConfigError{Path: configPath, Err: parseErr, Message: message}
}

// Creates a missing config by copying the example template, else writing the stripped defaults
func SeedConfigFile(ctx context.Context, h, configPath, template string) (bool, error) {
	if template == "" {
		template = filepath.Join(GetProjectRoot(), "cli-config.yaml.example")
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return false, err
	}
	if info, err := os.Stat(template); err == nil {
		data, err := os.ReadFile(template)
		if err != nil {
			return false, err
		}
		if err := os.WriteFile(configPath, data, info.Mode().Perm()); err != nil {
			return false, err
		}
		if err := os.Chmod(configPath, info.Mode().Perm()); err != nil {
			return false, err
		}
		if err := os.Chtimes(configPath, info.ModTime(), info.ModTime()); err != nil {
			return false, err
		}
		SecureFile(ctx, h, configPath)
		return true, nil
	}
	return false, SaveConfig(ctx, h, map[string]any(Defaults()), SaveOptions{})
}

// Process runner the editor launch goes through
var runEditor = func(editor, path string) error {
	cmd := exec.Command(editor, path)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil
		}
		return err
	}
	return nil
}

// Opens the config file in the user editor, seeding a missing file first
func EditConfig(ctx context.Context, h string) error {
	if ManagedSystem(ctx, h) != "" {
		ManagedError(ctx, h, "edit configuration")
		return nil
	}
	files, err := SupportsFileTooling(ctx)
	if err != nil {
		return err
	}
	if !files {
		return exitInvalid("`icebath config edit` opens the local config file, and this config backend has none. " +
			"Use `icebath config set <key> <value>` instead.")
	}
	configPath := home.ConfigPath(h)
	if _, err := os.Stat(configPath); err != nil {
		if _, err := SeedConfigFile(ctx, h, configPath, ""); err != nil {
			return err
		}
		fmt.Fprintf(FacadeStdout, "Created %s\n", configPath)
	}
	candidates := []string{"nano", "vim", "vi", "code", "notepad"}
	if runtime.GOOS == "windows" {
		candidates = []string{"notepad", "code", "vim", "vi", "nano"}
	}
	editor := firstNonEmpty(os.Getenv("EDITOR"), os.Getenv("VISUAL"))
	for _, c := range candidates {
		if editor != "" {
			break
		}
		if _, err := exec.LookPath(c); err == nil {
			editor = c
		}
	}
	if editor == "" {
		fmt.Fprintln(FacadeStdout, "No editor found. Config file is at:")
		fmt.Fprintf(FacadeStdout, "  %s\n", configPath)
		return nil
	}
	fmt.Fprintf(FacadeStdout, "Opening %s in %s...\n", configPath, editor)
	return runEditor(editor, configPath)
}

// Child mapping at key, replacing a missing or non mapping value with an empty one
func ensureDict(parent map[string]any, key string) map[string]any {
	if child, ok := parent[key].(map[string]any); ok {
		return child
	}
	child := map[string]any{}
	setOrdered(parent, key, child)
	return child
}

// Persists one field under platforms.<platform>, raw edits only the user file
func WritePlatformConfigField(ctx context.Context, h, platformKey, fieldKey string, value any, raw bool) error {
	var cfg map[string]any
	if raw {
		doc, err := ReadRawConfig(ctx, h)
		if err != nil {
			return err
		}
		cfg = doc
	} else {
		tree, err := LoadContext(ctx, h, EnvRefLookupContext(ctx))
		if err != nil {
			return err
		}
		cfg = map[string]any(tree)
	}
	setOrdered(ensureDict(ensureDict(cfg, "platforms"), platformKey), fieldKey, value)
	return SaveConfig(ctx, h, cfg, SaveOptions{})
}

// Writes config structure warnings to stderr, the loaded config checked when cfg is nil
func PrintConfigWarnings(ctx context.Context, h string, cfg map[string]any, removedNote func(section, name string) string) {
	var issues []ConfigIssue
	if cfg != nil {
		issues = ValidateConfigStructure(cfg, removedNote)
	} else {
		issues = ValidateLoadedConfigStructure(ctx, h, EnvRefLookupContext(ctx), removedNote)
	}
	w := FacadeNoticeOut
	if len(issues) == 0 {
		return
	}
	lines := []string{"\033[33m⚠ Config issues detected in config.yaml:\033[0m"}
	for _, ci := range issues {
		marker := "\033[33m⚠\033[0m"
		if ci.Severity == "error" {
			marker = "\033[31m✗\033[0m"
		}
		lines = append(lines, "  "+marker+" "+ci.Message)
	}
	lines = append(lines, "  \033[2mRun 'icebath doctor' for fix suggestions.\033[0m")
	fmt.Fprint(w, strings.Join(lines, "\n")+"\n\n")
}

// Warns about MESSAGING_CWD and TERMINAL_CWD left in the home .env instead of terminal.cwd
func WarnDeprecatedCwdEnvVars(w io.Writer, h string) {
	envMap := LoadEnv(h)
	var lines []string
	for _, name := range []string{"MESSAGING_CWD", "TERMINAL_CWD"} {
		if val := strings.TrimFunc(envMap[name], isPySpace); val != "" {
			lines = append(lines, fmt.Sprintf("  \033[33m⚠\033[0m %s=%s found in .env — this is deprecated.", name, val))
		}
	}
	if len(lines) == 0 {
		return
	}
	hint := home.Display(h)
	lines = append([]string{"\033[33m⚠ Deprecated .env settings detected:\033[0m"}, lines...)
	lines = append(lines, "  \033[2mMove to config.yaml instead:  terminal:\\n    cwd: /your/project/path\033[0m")
	lines = append(lines, "  \033[2mThen remove the old entries from "+hint+"/.env\033[0m")
	fmt.Fprint(w, strings.Join(lines, "\n")+"\n\n")
}
