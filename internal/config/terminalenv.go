package config

import (
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"unicode/utf16"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

// Variable telling a container backend whether its image was chosen by the user rather than shipped as the default
const EnvTerminalImagePinned = "TERMINAL_DOCKER_IMAGE_PINNED"

// Terminal keys bridged into the environment, each read back as TERMINAL_ and the key in capitals
func terminalBridgedKeys() []string {
	return []string{
		"backend", "modal_mode", "degraded_mode", "cwd", "temp_dir", "timeout", "lifetime_seconds",
		"docker_image", "docker_forward_env", "modal_image",
		"daytona_image", "vercel_runtime", "vercel_image", "ssh_host", "ssh_user", "ssh_port", "ssh_key",
		"container_cpu", "container_memory", "container_disk", "container_persistent",
		"docker_volumes", "docker_env", "docker_mount_cwd_to_workspace", "docker_network",
		"docker_extra_args", "docker_shm_size", "docker_run_as_host_user", "docker_snap_compat",
		"docker_persist_across_processes", "docker_shared_container_key",
		"docker_orphan_reaper", "sandbox_dir", "persistent_shell",
	}
}

// Variable a terminal key is bridged to, backend keeping its historical TERMINAL_ENV name
func terminalEnvName(key string) string {
	if key == "backend" {
		return "TERMINAL_ENV"
	}
	return "TERMINAL_" + strings.ToUpper(key)
}

// Every variable the terminal bridge owns, in key order
func TerminalEnvVars() []string {
	keys := terminalBridgedKeys()
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = terminalEnvName(k)
	}
	return out
}

// Variable a dotted terminal key is mirrored to, ok false for any other key
func TerminalEnvVarForKey(dotted string) (string, bool) {
	key, ok := strings.CutPrefix(dotted, "terminal.")
	if !ok || !slices.Contains(terminalBridgedKeys(), key) {
		return "", false
	}
	return terminalEnvName(key), true
}

// Reports whether a terminal value owns its variable, a cwd stand-in word leaving it to each surface
func terminalBridgeable(key string, value *yaml.Node) bool {
	if key != "cwd" {
		return true
	}
	switch strings.TrimSpace(orText(value)) {
	case ".", "auto", "cwd":
		return false
	}
	return true
}

// Variables a raw terminal section explicitly owns, sorted
func TerminalOwnedEnvVars(terminal *yaml.Node) []string {
	if !isMapping(terminal) {
		return nil
	}
	var out []string
	for _, k := range terminalBridgedKeys() {
		if v := childOf(terminal, k); mappingIndex(terminal, k) >= 0 && terminalBridgeable(k, v) {
			out = append(out, terminalEnvName(k))
		}
	}
	slices.Sort(out)
	return out
}

// Text a terminal value is bridged as, lists and mappings as ASCII JSON and scalars formatted as strings
func TerminalEnvValue(n *yaml.Node) string {
	n = resolveAlias(n)
	if n != nil && (n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode) {
		return jsonText(n, true)
	}
	return displayText(n)
}

// JSON text of a value tree, a space after each separator, keys in order, non-ASCII escaped when ascii is set
func jsonText(n *yaml.Node, ascii bool) string {
	var b strings.Builder
	writeJSON(&b, n, ascii)
	return b.String()
}

// Appends the JSON form of one value, keys written as the text their type formats to
func writeJSON(b *strings.Builder, n *yaml.Node, ascii bool) {
	n = resolveAlias(n)
	switch {
	case n == nil:
		b.WriteString("null")
		return
	case n.Kind == yaml.MappingNode:
		b.WriteByte('{')
		for i := 0; i+1 < len(n.Content); i += 2 {
			if i > 0 {
				b.WriteString(", ")
			}
			writeJSONString(b, jsonKey(n.Content[i]), ascii)
			b.WriteString(": ")
			writeJSON(b, n.Content[i+1], ascii)
		}
		b.WriteByte('}')
		return
	case n.Kind == yaml.SequenceNode:
		b.WriteByte('[')
		for i, it := range n.Content {
			if i > 0 {
				b.WriteString(", ")
			}
			writeJSON(b, it, ascii)
		}
		b.WriteByte(']')
		return
	}
	tag, text := scalarKind(n)
	switch tag {
	case tagNull:
		b.WriteString("null")
		return
	case tagBool:
		v, _ := resolveTagged(tagBool, text)
		b.WriteString(v)
		return
	case tagInt:
		if v, ok := constructInt(text); ok {
			b.WriteString(v.String())
			return
		}
	case tagFloat:
		if f, ok := constructFloat(text); ok {
			switch {
			case math.IsNaN(f):
				b.WriteString("NaN")
			case math.IsInf(f, 1):
				b.WriteString("Infinity")
			case math.IsInf(f, -1):
				b.WriteString("-Infinity")
			default:
				b.WriteString(reprFloat(f))
			}
			return
		}
	case "!!timestamp":
		text = timestampWord(text)
	}
	writeJSONString(b, text, ascii)
}

// Text a mapping key takes as a JSON key, booleans and null in their JSON words and numbers canonical
func jsonKey(k *yaml.Node) string {
	tag, text := scalarKind(orNull(k))
	switch tag {
	case tagNull:
		return "null"
	case tagBool:
		v, _ := resolveTagged(tagBool, text)
		return v
	}
	return displayText(k)
}

// Appends a JSON string, quotes, backslashes and control characters escaped and non-ASCII too when ascii is set
func writeJSONString(b *strings.Builder, s string, ascii bool) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20 || (ascii && r == 0x7f):
			fmt.Fprintf(b, `\u%04x`, r)
		case ascii && r > 0xffff:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(b, `\u%04x\u%04x`, hi, lo)
		case ascii && r > 0x7f:
			fmt.Fprintf(b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}

// Destination of bridged terminal settings, the process environment or a child environment
type EnvTarget interface {
	LookupEnv(name string) (string, bool)
	Setenv(name, value string) error
}

// Process environment as a bridge destination
type ProcessEnv struct{}

// Value of a process variable
func (ProcessEnv) LookupEnv(name string) (string, bool) { return os.LookupEnv(name) }

// Sets a process variable
func (ProcessEnv) Setenv(name, value string) error { return os.Setenv(name, value) }

// Child environment held as a map
type EnvMap map[string]string

// Value of a variable in the map
func (m EnvMap) LookupEnv(name string) (string, bool) {
	v, ok := m[name]
	return v, ok
}

// Sets a variable in the map
func (m EnvMap) Setenv(name, value string) error {
	m[name] = value
	return nil
}

// Terminal section with each value the user or managed file wrote kept as written where the typed value equals it
func asWritten(terminal *yaml.Node, o *loadOrigin) *yaml.Node {
	if o == nil || !isMapping(terminal) {
		return terminal
	}
	for i := 0; i+1 < len(terminal.Content); i += 2 {
		k := terminal.Content[i].Value
		for _, layer := range []*yaml.Node{o.managed, o.user} {
			if w := treeGet(layer, "terminal", k); w != nil && sameValue(w, terminal.Content[i+1]) {
				terminal.Content[i+1] = plainCopy(w)
				break
			}
		}
	}
	return terminal
}

// Reports trees holding the same values, scalars equal as numbers or in their formatted text
func sameValue(a, b *yaml.Node) bool {
	a, b = resolveAlias(a), resolveAlias(b)
	switch {
	case isMapping(a) && isMapping(b):
		if len(a.Content) != len(b.Content) {
			return false
		}
		for i := 0; i+1 < len(a.Content); i += 2 {
			if at := mappingIndex(b, a.Content[i].Value); at < 0 || !sameValue(a.Content[i+1], b.Content[at]) {
				return false
			}
		}
		return true
	case isSequence(a) && isSequence(b):
		if len(a.Content) != len(b.Content) {
			return false
		}
		for i := range a.Content {
			if !sameValue(a.Content[i], b.Content[i]) {
				return false
			}
		}
		return true
	case a != nil && b != nil && a.Kind == yaml.ScalarNode && b.Kind == yaml.ScalarNode:
		return valueEqual(a, b, false) || displayText(a) == displayText(b)
	}
	return false
}

// Bridges the terminal section into the variables terminal tools read, keys the file sets overriding and defaults only filling gaps
func (s *Store) ApplyTerminalEnv(ctx context.Context, target EnvTarget, cfg *Config, override *bool) error {
	raw, err := s.RawDocument(ctx)
	if err != nil {
		return err
	}
	rawTerminal := childOf(raw.root, "terminal")
	fileHasTerminal := isMapping(rawTerminal)
	if !fileHasTerminal {
		rawTerminal = emptyMapping()
	}
	shouldOverride := fileHasTerminal
	if override != nil {
		shouldOverride = *override
	}
	given := cfg != nil
	if !given {
		if cfg, err = s.LoadReadonly(ctx); err != nil {
			return err
		}
	}
	n, err := encodeValue(reflect.ValueOf(cfg.Terminal))
	if err != nil {
		return err
	}
	terminal := asWritten(plainCopy(n), cfg.origin)
	explicit := rawTerminal
	if given {
		explicit = terminal
	}
	isExplicit := func(k string) bool { return mappingIndex(explicit, k) >= 0 }
	envBackend, _ := target.LookupEnv("TERMINAL_ENV")
	sources := []string{orText(childOf(terminal, "backend")), envBackend}
	if !given && mappingIndex(rawTerminal, "backend") < 0 {
		sources[0], sources[1] = sources[1], sources[0]
	}
	backend := sources[0]
	if backend == "" {
		backend = sources[1]
	}
	if _, pinned := target.LookupEnv(EnvTerminalImagePinned); shouldOverride && isExplicit("docker_image") {
		if err := target.Setenv(EnvTerminalImagePinned, "1"); err != nil {
			return err
		}
	} else if !pinned {
		value := "0"
		if _, set := target.LookupEnv("TERMINAL_DOCKER_IMAGE"); set {
			value = "1"
		}
		if err := target.Setenv(EnvTerminalImagePinned, value); err != nil {
			return err
		}
	}
	for _, key := range terminalBridgedKeys() {
		at := mappingIndex(terminal, key)
		if at < 0 {
			continue
		}
		value := terminal.Content[at]
		if !terminalBridgeable(key, value) {
			continue
		}
		if key == "cwd" && isStringNode(value) {
			cwd := strings.TrimSpace(value.Value)
			if !(strings.ToLower(strings.TrimSpace(backend)) == "ssh" && (cwd == "~" || strings.HasPrefix(cwd, "~/"))) {
				value = stringNode(home.ExpandUser(value.Value))
			}
		}
		name := terminalEnvName(key)
		if _, set := target.LookupEnv(name); (shouldOverride && isExplicit(key)) || !set {
			if err := target.Setenv(name, TerminalEnvValue(value)); err != nil {
				return err
			}
		}
	}
	return nil
}
