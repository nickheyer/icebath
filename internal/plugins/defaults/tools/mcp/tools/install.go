package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/mcp"
	mcpconfig "github.com/nickheyer/icebath/internal/plugins/defaults/tools/mcp/config"
)

// Value a manifest names for the folder its repository was cloned to
const installDirRef = "${INSTALL_DIR}"

// Settings of a catalog entry's server, before credentials and tool filters: its command or address, and how it signs in
func entryConfig(entry mcp.Entry, installDir string) map[string]any {
	expand := func(s string) string { return strings.ReplaceAll(s, installDirRef, installDir) }
	out := map[string]any{}
	switch entry.Launch.Type {
	case "stdio":
		out["command"] = expand(entry.Launch.Command)
		if len(entry.Launch.Args) > 0 {
			args := make([]any, len(entry.Launch.Args))
			for i, a := range entry.Launch.Args {
				args[i] = expand(a)
			}
			out["args"] = args
		}
		if len(entry.Launch.Env) > 0 {
			var env []any
			for _, k := range slices.Sorted(maps.Keys(entry.Launch.Env)) {
				env = append(env, k+"="+expand(entry.Launch.Env[k]))
			}
			out["env"] = env
		}
	case "http":
		out["url"] = entry.Launch.URL
		switch entry.Auth.Type {
		case "oauth":
			out["auth"] = "oauth"
			if len(entry.Auth.OAuth) > 0 {
				out["oauth"] = maps.Clone(entry.Auth.OAuth)
			}
		case "api_key":
			out["headers"] = BearerHeaders(entry.Name)
		}
	}
	return out
}

// Authorization header of a server naming the value its key is kept under
func BearerHeaders(name string) map[string]any {
	return map[string]any{"Authorization": "Bearer ${" + mcp.KeyEnv(name) + "}"}
}

// Token pasted with or without its Bearer word
func StripBearer(token string) string {
	stripped := strings.TrimSpace(token)
	if len(stripped) >= 7 && strings.EqualFold(stripped[:7], "bearer ") {
		return strings.TrimSpace(stripped[7:])
	}
	return stripped
}

// Settings of a server as the profile keeps them, nil when it has none of that name
func (m *Manager) Stored(cfg *config.Config, name string) map[string]any {
	return table(table(m.config.In(cfg.Profile()).Get(mcpconfig.Servers))[name])
}

// Every server the profile keeps settings for, by name
func (m *Manager) StoredNames(cfg *config.Config) []string {
	return slices.Sorted(maps.Keys(table(m.config.In(cfg.Profile()).Get(mcpconfig.Servers))))
}

// Why a server's settings would be refused: a name the settings can't hold, a backdoor shape, or costs that don't parse
func (m *Manager) Check(name string, settings map[string]any) []string {
	if !config.Named(name) {
		return []string{fmt.Sprintf("%q can't name an MCP server: use lower case letters, digits, dashes or underscores", name)}
	}
	issues := validate(name, settings)
	if _, err := parseCosts(name, settings); err != nil {
		issues = append(issues, err.Error())
	}
	return issues
}

// Keeps a server's settings, refusing a backdoor shape; the issues that refused it
func (m *Manager) Save(cfg *config.Config, name string, settings map[string]any) ([]string, error) {
	if !config.Named(name) {
		return nil, fmt.Errorf("%q can't name an MCP server: use lower case letters, digits, dashes or underscores", name)
	}
	if issues := validate(name, settings); len(issues) > 0 {
		return append(issues, fmt.Sprintf("Server '%s' was NOT saved due to suspicious configuration.", name)), nil
	}
	if _, err := parseCosts(name, settings); err != nil {
		return nil, err
	}
	return nil, m.config.In(cfg.Profile()).Set(mcpconfig.Servers+"."+name, settings)
}

// Removes a server's settings and its sign in, stopping it; false when the profile has none of that name
func (m *Manager) Remove(cfg *config.Config, name string) (bool, error) {
	if m.Stored(cfg, name) == nil {
		return false, nil
	}
	p := m.pool(cfg)
	p.drop(name)
	p.forgetProvider(name)
	if err := m.config.In(cfg.Profile()).Set(mcpconfig.Servers+"."+name, nil); err != nil {
		return true, err
	}
	return true, p.store(name).Remove(false)
}

// Turns a server on or off for new sessions
func (m *Manager) Enable(cfg *config.Config, name string, enabled bool) error {
	entry := m.Stored(cfg, name)
	if entry == nil {
		return fmt.Errorf("'%s' is not a configured MCP server", name)
	}
	entry["enabled"] = enabled
	return m.config.In(cfg.Profile()).Set(mcpconfig.Servers+"."+name, entry)
}

// Keeps the tool filter of a server: only the tools of values for include, all but them for exclude; no values drops the filter
func (m *Manager) SetFilter(cfg *config.Config, name, mode string, values []string) error {
	entry := m.Stored(cfg, name)
	if entry == nil {
		return fmt.Errorf("config %s.%s is not a table", mcpconfig.Servers, name)
	}
	if values == nil {
		delete(entry, "tools")
	} else {
		block := table(entry["tools"])
		if block == nil {
			block = map[string]any{}
		}
		list := make([]any, len(values))
		for i, v := range values {
			list[i] = v
		}
		block[mode] = list
		other := "exclude"
		if mode == "exclude" {
			other = "include"
		}
		delete(block, other)
		entry["tools"] = block
	}
	return m.config.In(cfg.Profile()).Set(mcpconfig.Servers+"."+name, entry)
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Keeps a value the servers' settings name as ${NAME}
func (m *Manager) SetValue(cfg *config.Config, name, value string) error {
	if !envName.MatchString(name) {
		return fmt.Errorf("%q can't name a value: use letters, digits and underscores, not starting with a digit", name)
	}
	return m.config.In(cfg.Profile()).Set(mcpconfig.Env+"."+strings.ToLower(name), value)
}

// Value a server's settings name as ${NAME}: the environment's, else the profile's, empty when neither has one
func (m *Manager) Value(cfg *config.Config, name string) string {
	if v := cfg.Getenv(name); v != "" {
		return v
	}
	values := table(m.config.In(cfg.Profile()).Get(mcpconfig.Env))
	if v, ok := values[strings.ToLower(name)]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// Keeps a bearer token for a server and returns the header naming it
func (m *Manager) SaveBearer(cfg *config.Config, name, token string) (map[string]any, error) {
	normalized := StripBearer(token)
	if normalized == "" || strings.EqualFold(normalized, "bearer") {
		return nil, errors.New("Bearer token is required")
	}
	if err := m.SetValue(cfg, mcp.KeyEnv(name), normalized); err != nil {
		return nil, err
	}
	return BearerHeaders(name), nil
}

// Entries of the catalog for the profile; those that can't be installed come with their problem
func (m *Manager) Catalog(cfg *config.Config) []mcp.Entry {
	entries := m.catalog.Catalog(m.config.In(cfg.Profile()))
	slices.SortFunc(entries, func(a, b mcp.Entry) int { return strings.Compare(a.Name, b.Name) })
	return entries
}

// Entry of the catalog that can be installed under the name
func (m *Manager) Entry(cfg *config.Config, name string) (mcp.Entry, bool) {
	return m.catalog.Entry(m.config.In(cfg.Profile()), name)
}

// Values an entry asks for that have none yet
func (m *Manager) Missing(cfg *config.Config, entry mcp.Entry) []mcp.EnvSpec {
	var out []mcp.EnvSpec
	for _, spec := range entry.Auth.Env {
		if m.Value(cfg, spec.Name) == "" {
			out = append(out, spec)
		}
	}
	return out
}

// Settings an entry installs as: its server with the values given that are no secret written in, and the tool filter the profile had for
// it before, else the manifest's
func (m *Manager) InstallConfig(cfg *config.Config, entry mcp.Entry, installDir string, values map[string]string) map[string]any {
	out := entryConfig(entry, installDir)
	for _, spec := range entry.Auth.Env {
		if v, ok := values[spec.Name]; ok && !spec.Secret && v != "" {
			out = replaceRef(out, spec.Name, v).(map[string]any)
		}
	}
	out["enabled"] = true
	prior := table(m.Stored(cfg, entry.Name)["tools"])
	switch {
	case prior["include"] != nil:
		out["tools"] = map[string]any{"include": prior["include"]}
	case prior["exclude"] != nil:
		out["tools"] = map[string]any{"exclude": prior["exclude"]}
	case len(entry.Tools.Excluded) > 0:
		out["tools"] = map[string]any{"exclude": anyList(entry.Tools.Excluded)}
	case len(entry.Tools.Enabled) > 0:
		out["tools"] = map[string]any{"include": anyList(entry.Tools.Enabled)}
	}
	return out
}

func anyList(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Value with every ${NAME} of a value that is no secret written in
func replaceRef(v any, name, value string) any {
	switch x := v.(type) {
	case string:
		return strings.ReplaceAll(x, "${"+name+"}", value)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = replaceRef(item, name, value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = replaceRef(item, name, value)
		}
		return out
	}
	return v
}

var shaRef = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// Clones an entry's repository at its pinned ref into the profile's install folder and runs its bootstrap there, its output written as it
// goes; the folder it was cloned to
func (m *Manager) GitInstall(ctx context.Context, cfg *config.Config, entry mcp.Entry, out *bytes.Buffer) (string, error) {
	install := entry.Install
	if install == nil {
		return "", nil
	}
	root := filepath.Join(m.config.ProfileDir(cfg.Profile()), "mcp-installs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(root, entry.Name)
	git, err := exec.LookPath("git")
	if err != nil {
		return "", &InstallError{Class: "git_missing", Msg: "git is required to install this MCP but was not found on PATH"}
	}
	if _, err := os.Stat(dest); err == nil {
		fmt.Fprintf(out, "  Removing existing install at %s\n", dest)
		if err := os.RemoveAll(dest); err != nil {
			return "", err
		}
	}
	fmt.Fprintf(out, "  Cloning %s (%s) → %s\n", install.URL, install.Ref, dest)
	run := func(args ...string) error {
		cmd := exec.CommandContext(ctx, git, args...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_ASKPASS=", "SSH_ASKPASS=")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil && strings.TrimSpace(stderr.String()) != "" {
			fmt.Fprintln(out, "  "+strings.TrimSpace(stderr.String()))
		}
		return err
	}
	sha := shaRef.MatchString(install.Ref)
	if !sha {
		if run("clone", "--depth", "1", "--branch", install.Ref, install.URL, dest) != nil {
			os.RemoveAll(dest)
			sha = true
		}
	}
	if sha {
		if run("clone", install.URL, dest) != nil {
			return "", &InstallError{Class: "clone_failed", Msg: "git clone failed for " + install.URL}
		}
		if run("-C", dest, "checkout", install.Ref) != nil {
			return "", &InstallError{Class: "clone_failed", Msg: "git checkout " + install.Ref + " failed"}
		}
	}
	for _, step := range install.Bootstrap {
		fmt.Fprintf(out, "  $ %s\n", step)
		cmd := exec.CommandContext(ctx, "sh", "-c", step)
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(ctx, "cmd", "/C", step)
		}
		cmd.Dir, cmd.Stdout, cmd.Stderr = dest, out, out
		if err := cmd.Run(); err != nil {
			code := -1
			if exit, ok := errors.AsType[*exec.ExitError](err); ok {
				code = exit.ExitCode()
			}
			return "", &InstallError{Class: "bootstrap_failed", Msg: fmt.Sprintf("bootstrap step failed (exit %d): %s", code, step)}
		}
	}
	return dest, nil
}

// Install failure and its class: config_invalid, config_rejected, git_missing, clone_failed, bootstrap_failed or missing_credentials
type InstallError struct {
	Class string
	Msg   string
}

func (e *InstallError) Error() string { return e.Msg }

// Removes a server installed from the catalog and its cloned files; whether anything was removed
func (m *Manager) Uninstall(cfg *config.Config, name string) (bool, error) {
	removed, err := m.Remove(cfg, name)
	if err != nil {
		return removed, err
	}
	clone := filepath.Join(m.config.ProfileDir(cfg.Profile()), "mcp-installs", name)
	if _, err := os.Stat(clone); err == nil {
		if err := os.RemoveAll(clone); err != nil {
			return removed, err
		}
		removed = true
	}
	return removed, nil
}
