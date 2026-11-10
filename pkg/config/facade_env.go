package config

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/home"
)

// Terminal config keys in declaration order with the env var each one mirrors
var TerminalConfigEnvMap = func() [][2]string {
	out := [][2]string{{"backend", "TERMINAL_ENV"}}
	for _, key := range []string{
		"modal_mode", "degraded_mode", "cwd", "temp_dir", "timeout", "lifetime_seconds",
		"docker_image", "docker_forward_env", "modal_image",
		"daytona_image", "vercel_runtime", "vercel_image", "ssh_host", "ssh_user", "ssh_port", "ssh_key",
		"container_cpu", "container_memory", "container_disk", "container_persistent",
		"docker_volumes", "docker_env", "docker_mount_cwd_to_workspace", "docker_network",
		"docker_extra_args", "docker_shm_size", "docker_run_as_host_user", "docker_snap_compat",
		"docker_persist_across_processes", "docker_shared_container_key",
		"docker_orphan_reaper", "sandbox_dir", "persistent_shell",
	} {
		out = append(out, [2]string{key, "TERMINAL_" + strings.ToUpper(key)})
	}
	return out
}()

// Env var a terminal config key mirrors, false when the key mirrors none
func terminalEnvVar(key string) (string, bool) {
	for _, pair := range TerminalConfigEnvMap {
		if pair[0] == key {
			return pair[1], true
		}
	}
	return "", false
}

// Env var mirrored by a dotted terminal config key, false outside the terminal section
func TerminalConfigEnvVarForKey(key string) (string, bool) {
	if !strings.HasPrefix(key, "terminal.") {
		return "", false
	}
	return terminalEnvVar(key[len("terminal."):])
}

// Python json.dumps text of a value with the default separators and ASCII escaping
func pyJSONDumps(v any) string {
	var b strings.Builder
	var write func(v any)
	write = func(v any) {
		switch c := v.(type) {
		case nil:
			b.WriteString("null")
		case bool:
			if c {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
		case string:
			b.WriteString(pyJSONString(c))
		case float64:
			switch {
			case math.IsNaN(c):
				b.WriteString("NaN")
			case math.IsInf(c, 1):
				b.WriteString("Infinity")
			case math.IsInf(c, -1):
				b.WriteString("-Infinity")
			default:
				b.WriteString(pyFloatRepr(c))
			}
		case map[string]any:
			b.WriteString("{")
			for i, k := range MappingKeys(c) {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(pyJSONString(k))
				b.WriteString(": ")
				write(c[k])
			}
			b.WriteString("}")
		case Tree:
			write(map[string]any(c))
		case []any:
			b.WriteString("[")
			for i, item := range c {
				if i > 0 {
					b.WriteString(", ")
				}
				write(item)
			}
			b.WriteString("]")
		case []string:
			items := make([]any, len(c))
			for i, s := range c {
				items[i] = s
			}
			write(items)
		default:
			b.WriteString(PyStr(c))
		}
	}
	write(v)
	return b.String()
}

// JSON string literal with every non ASCII rune escaped the way ensure_ascii does
func pyJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20 || (r >= 0x7f && r < 0x10000):
				fmt.Fprintf(&b, `\u%04x`, r)
			case r >= 0x10000:
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Env text of a terminal config value, lists and mappings as JSON
func terminalEnvValueText(v any) string {
	switch v.(type) {
	case []any, map[string]any, Tree, []string:
		return pyJSONDumps(v)
	}
	return PyStr(v)
}

// Python str of a value when truthy, empty otherwise
func pyOrEmpty(v any) string {
	if !PyTruthy(v) {
		return ""
	}
	return PyStr(v)
}

// Reports whether a terminal config value owns its mirrored env var, a dot, auto or cwd value never does
func terminalConfigValueIsBridgeable(key string, value any) bool {
	if key != "cwd" {
		return true
	}
	switch strings.TrimSpace(pyOrEmpty(value)) {
	case ".", "auto", "cwd":
		return false
	}
	return true
}

// Env vars a raw terminal section explicitly owns
func TerminalConfigOwnedEnvVars(terminalConfig any) map[string]bool {
	out := map[string]bool{}
	section, ok := terminalConfig.(map[string]any)
	if !ok {
		return out
	}
	for _, pair := range TerminalConfigEnvMap {
		if value, has := section[pair[0]]; has && terminalConfigValueIsBridgeable(pair[0], value) {
			out[pair[1]] = true
		}
	}
	return out
}

// Reports whether the remote SSH shell must expand a tilde cwd itself
func IsSSHRemoteTildeCwd(backend, cwd string) bool {
	return strings.ToLower(strings.TrimSpace(backend)) == "ssh" && (cwd == "~" || strings.HasPrefix(cwd, "~/"))
}

// Where the terminal bridge reads and writes env values, the process environment when Env is nil
type TerminalEnvOptions struct {
	Env      map[string]string
	Config   map[string]any
	Override *bool
}

// Bridges terminal config keys into the env vars terminal tools read, explicit file keys overriding env values
func ApplyTerminalConfigToEnv(ctx context.Context, h string, opts TerminalEnvOptions) error {
	get := func(name string) (string, bool) {
		if opts.Env != nil {
			v, ok := opts.Env[name]
			return v, ok
		}
		return os.LookupEnv(name)
	}
	set := func(name, value string) error {
		if opts.Env != nil {
			opts.Env[name] = value
			return nil
		}
		return os.Setenv(name, value)
	}
	raw, err := ReadRawConfigReadonly(ctx, h)
	if err != nil {
		return err
	}
	rawTerminal, fileHasTerminal := raw["terminal"].(map[string]any)
	if !fileHasTerminal {
		rawTerminal = map[string]any{}
	}
	shouldOverride := fileHasTerminal
	if opts.Override != nil {
		shouldOverride = *opts.Override
	}
	cfg := opts.Config
	if cfg == nil {
		loaded, lerr := LoadReadonly(ctx, h, EnvRefLookupContext(ctx))
		if lerr != nil {
			return lerr
		}
		cfg = loaded
	}
	var terminalAny any = map[string]any{}
	if value, has := cfg["terminal"]; has {
		terminalAny = value
	}
	terminal, ok := terminalAny.(map[string]any)
	if !ok {
		return nil
	}
	explicit := rawTerminal
	if opts.Config != nil {
		explicit = terminal
	}
	envBackend, _ := get("TERMINAL_ENV")
	sources := []any{terminal["backend"], envBackend}
	if _, rawBackend := rawTerminal["backend"]; !(opts.Config != nil || rawBackend) {
		sources[0], sources[1] = sources[1], sources[0]
	}
	backend := ""
	if PyTruthy(sources[0]) {
		backend = PyStr(sources[0])
	} else if PyTruthy(sources[1]) {
		backend = PyStr(sources[1])
	}
	if _, pinned := explicit["docker_image"]; shouldOverride && pinned {
		if err := set("TERMINAL_DOCKER_IMAGE_PINNED", "1"); err != nil {
			return err
		}
	} else if _, has := get("TERMINAL_DOCKER_IMAGE_PINNED"); !has {
		flag := "0"
		if _, image := get("TERMINAL_DOCKER_IMAGE"); image {
			flag = "1"
		}
		if err := set("TERMINAL_DOCKER_IMAGE_PINNED", flag); err != nil {
			return err
		}
	}
	for _, pair := range TerminalConfigEnvMap {
		key, envVar := pair[0], pair[1]
		value, has := terminal[key]
		if !has || !terminalConfigValueIsBridgeable(key, value) {
			continue
		}
		if key == "cwd" {
			if s, isStr := value.(string); isStr && !IsSSHRemoteTildeCwd(backend, strings.TrimSpace(pyOrEmpty(value))) {
				value = pyExpandUser(s)
			}
		}
		_, explicitKey := explicit[key]
		_, present := get(envVar)
		if (shouldOverride && explicitKey) || !present {
			if err := set(envVar, terminalEnvValueText(value)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Parsed .env of a home, empty for a frozen worker whose secrets arrive through its scope
func LoadEnv(h string) map[string]string {
	if WorkerConfigSnapshot() != nil {
		return map[string]string{}
	}
	env := LoadEnvFile(home.EnvPath(h))
	var policy any
	if v, ok := env["ICEBATH_REDACT_SECRETS"]; ok {
		policy = v
	}
	notePolicy("note_env_policy", h, policy)
	return env
}

// Re-reads the home .env into the process env, removing known keys it dropped unless the file is unreadable
func ReloadEnv(h string) (int, error) {
	env := LoadEnv(h)
	count := 0
	for key, value := range env {
		if cur, ok := os.LookupEnv(key); !ok || cur != value {
			if err := os.Setenv(key, value); err != nil {
				return count, err
			}
			count++
		}
	}
	hk := hooks()
	known := map[string]bool{}
	if hk.DotenvUnreadable == nil || !hk.DotenvUnreadable(home.EnvPath(h)) {
		for _, name := range OptionalEnvVars.Names() {
			known[name] = true
		}
		if hk.ExtraEnvKeys != nil {
			for _, name := range hk.ExtraEnvKeys() {
				known[name] = true
			}
		}
	}
	for key := range known {
		if _, kept := env[key]; kept {
			continue
		}
		if _, present := os.LookupEnv(key); present {
			if err := os.Unsetenv(key); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, nil
}
