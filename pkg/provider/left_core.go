package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"gopkg.in/yaml.v3"
)

// LeftCoreFeature is one feature that moved out of core into a catalog plugin
type LeftCoreFeature struct {
	Plugin             string
	Label              string
	Toolsets           []string
	Platform           string
	EnableEnv          []string
	ChannelEnvPrefixes []string
	CLI                string
	Command            string
	Providers          []string
	ProfileEnv         []string
	LaunchEnv          []string
	Hosts              []string
}

var leftCoreTable []LeftCoreFeature

func init() { leftCoreTable = buildLeftCoreTable() }

// Every feature that left core with its plugin, predicates, env names, toolsets, commands, providers and hosts
func buildLeftCoreTable() []LeftCoreFeature {
	rows := []LeftCoreFeature{
		{
			Plugin: "homeassistant", Label: "Home Assistant",
			Toolsets: []string{"homeassistant"}, Platform: "homeassistant",
			EnableEnv: []string{"HASS_TOKEN"}, ChannelEnvPrefixes: []string{"HASS_"},
		},
		{
			Plugin: "spotify", Label: "Spotify",
			CLI: "spotify",
		},
		{
			Plugin: "langfuse", Label: "Langfuse",
		},
		{
			Plugin: "mcp-serve", Label: "Icebath as an MCP server",
			Command: "mcp-serve",
		},
		{
			Plugin: "camofox", Label: "Camofox browser backend",
			Toolsets: []string{"camofox"},
		},
		{
			Plugin: "agent-importers", Label: "`icebath import-agent`",
			Command: "import-agent",
		},
	}
	return rows
}

// LeftCore returns a copy of every feature row that left core
func LeftCore() []LeftCoreFeature { return append([]LeftCoreFeature(nil), leftCoreTable...) }

func leftCoreFeatureByPlugin(plugin string) *LeftCoreFeature {
	for i := range leftCoreTable {
		if leftCoreTable[i].Plugin == plugin {
			return &leftCoreTable[i]
		}
	}
	panic(fmt.Sprintf("no left core row for plugin %q", plugin))
}

// Python str of a config value
func pyStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	}
	return leftCorePyRepr(v)
}

// Python str of value or else, the value counting only when truthy
func pyOrStr(v any) string {
	if !pyTruthy(v) {
		return ""
	}
	return pyStr(v)
}

func anyMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func isTruthyValue(v any) bool {
	if s, ok := v.(string); ok {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyTruthy(v)
}

// Reports whether a config or env value spells a truthy flag the way is_truthy_value does
func IsTruthyValue(v any) bool { return isTruthyValue(v) }

// Raw config.yaml of a home, empty when missing, unreadable or not a mapping
func leftCoreReadConfig(h string) map[string]any {
	doc, err := config.ReadDocument(home.ConfigPath(h))
	if err != nil || doc == nil {
		return map[string]any{}
	}
	return doc
}

// Raw config.yaml for a write back, empty when missing or not a mapping, failing on malformed YAML
func leftCoreReadUserConfigRaw(path string) (map[string]any, error) {
	doc, err := config.ReadDocument(path)
	if err == nil {
		return doc, nil
	}
	var invalid *config.InvalidUserConfigError
	if errors.As(err, &invalid) {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, readErr
		}
		var probe any
		if yaml.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &probe) == nil {
			return map[string]any{}, nil
		}
	}
	return nil, err
}

// User config.yaml with env references expanded and the managed overlay applied, as the gateway loader reads it
func leftCoreYAMLLayers(h string) (map[string]any, error) {
	path := home.ConfigPath(h)
	doc := map[string]any{}
	if _, err := os.Stat(path); err == nil {
		read, err := config.ReadDocument(path)
		if err != nil {
			return nil, err
		}
		doc = read
	}
	expanded, _ := config.Expand(doc, os.LookupEnv).(map[string]any)
	if expanded == nil {
		expanded = map[string]any{}
	}
	return config.ApplyManagedOverlay(expanded), nil
}

// PlatformConfiguredOn reports a platform switched on, or holding a token without an enabled key, in the merged gateway config
func PlatformConfiguredOn(h, name string) bool {
	gwData := gatewayLegacyJSON(h)
	if gwData == nil {
		gwData = map[string]any{}
	}
	yamlCfg, err := leftCoreYAMLLayers(h)
	if err != nil {
		yamlCfg = map[string]any{}
	}
	block, ok := gatewayMergePlatformSections(yamlCfg, yamlCfg["gateway"], gwData, []string{name})[name].(map[string]any)
	if !ok {
		return false
	}
	enabled, token := gatewayPlatformEnabled(block)
	_, explicit := block["enabled"]
	return enabled || (!explicit && strings.TrimSpace(token) != "")
}

// Reports a toolset of names selected for another platform or in the top level toolsets list
func toolsetListed(cfg map[string]any, names map[string]bool, ownPlatform string) bool {
	var selections []any
	if rows, ok := cfg["platform_toolsets"].(map[string]any); ok {
		for platform, sel := range rows {
			if platform != ownPlatform {
				selections = append(selections, sel)
			}
		}
	}
	selections = append(selections, cfg["toolsets"])
	for _, sel := range selections {
		if list, ok := sel.([]any); ok {
			for _, item := range list {
				if names[pyStr(item)] {
					return true
				}
			}
		}
	}
	return false
}

// auth.json of a home, empty when missing, unreadable or invalid
func leftCoreAuthStore(h string) map[string]any {
	data, err := os.ReadFile(home.AuthPath(h))
	if err != nil {
		return map[string]any{}
	}
	var store any
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &store) != nil {
		return map[string]any{}
	}
	return anyMap(store)
}

// Whether the plugin key or name sits in plugins.enabled and plugins.disabled with the managed overlay applied
func leftCorePluginLists(h, plugin string) (bool, bool) {
	feature := leftCoreFeatureByPlugin(plugin)
	cfg, err := leftCoreYAMLLayers(h)
	if err != nil {
		pkgLog().Debug("left-core: unreadable config", "home", h, "error", err)
		return false, false
	}
	plugins := anyMap(cfg["plugins"])
	listed := func(key string) bool {
		names, ok := plugins[key].([]any)
		if !ok {
			return false
		}
		for _, n := range names {
			if name := pyStr(n); name == feature.Plugin {
				return true
			}
		}
		return false
	}
	return listed("enabled"), listed("disabled")
}

func isLaunchHome(h string) bool { return home.Key(h) == home.Key(home.Process()) }

// Env name from the home dotenv layers, else the launch home process env when asked
func leftCoreHomeEnv(h, name string, processEnv bool) string {
	value := strings.TrimSpace(resolveHomeDotenv(h)[name])
	if value == "" && processEnv && isLaunchHome(h) {
		value = strings.TrimSpace(os.Getenv(name))
	}
	return value
}

func leftCoreInstallSentence(ctx context.Context, f *LeftCoreFeature) string {
	return fmt.Sprintf("%s moved out of core into the '%s' plugin: install it with `%s`", f.Label, f.Plugin,
		PluginInstallCommand(f.Plugin, home.Dir(ctx)))
}

// PlatformInstallHint names the install command for an unknown platform that left core, else empty
func PlatformInstallHint(ctx context.Context, platform string) string {
	for i := range leftCoreTable {
		if f := &leftCoreTable[i]; f.Platform != "" && f.Platform == platform {
			return ". " + leftCoreInstallSentence(ctx, f)
		}
	}
	return ""
}

// ProviderInstallHint names the install, enable or update command for a provider a left core plugin supplies
func ProviderInstallHint(ctx context.Context, name string) string {
	f, ok := leftCoreProviderFeature(name)
	if !ok {
		return ""
	}
	h := home.Dir(ctx)
	if !leftCorePresent(f, h) {
		return leftCoreInstallSentence(ctx, f) + "."
	}
	command := PluginInstallCommand(f.Plugin, h)
	if _, disabled := leftCorePluginLists(h, f.Plugin); disabled {
		return fmt.Sprintf("%s moved out of core into the '%s' plugin, which is installed but disabled: run `%s`.",
			f.Label, f.Plugin, strings.ReplaceAll(command, " install ", " enable "))
	}
	cause := ""
	if loadErr := leftCoreLoadError(f); loadErr != "" {
		cause = " (" + loadErr + ")"
	}
	return fmt.Sprintf("%s moved out of core into the '%s' plugin, which is installed but failed to load%s: run `%s`, "+
		"or see `icebath logs` for the cause.", f.Label, f.Plugin, cause, strings.ReplaceAll(command, " install ", " update "))
}

// Error the feature's provider plugin raised on import in the active home, else empty
func leftCoreLoadError(f *LeftCoreFeature) string { return leftCoreHooks().LoadError(f.Plugin) }

func leftCoreProviderInstallHint(name string) string {
	return ProviderInstallHint(context.Background(), name)
}

// Every row's profile managed env names
func leftCoreProfileEnv() []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range leftCoreTable {
		for _, name := range f.ProfileEnv {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// Every row's launch env names the generic env writers refuse
func leftCoreLaunchEnvNames() map[string]bool {
	out := map[string]bool{}
	for _, f := range leftCoreTable {
		for _, name := range f.LaunchEnv {
			out[name] = true
		}
	}
	return out
}

// JSON string literal escaping every non ASCII rune as Python json.dumps does
func pyJSONQuote(s string) string {
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
				if r == 0x7f {
					b.WriteRune(r)
				} else {
					fmt.Fprintf(&b, `\u%04x`, r)
				}
			case r >= 0x10000:
				r -= 0x10000
				fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			default:
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Whether a row's plugin is installed in the home, enabled or not
func leftCorePresent(f *LeftCoreFeature, h string) bool {
	return PluginPresent(f.Plugin, h)
}

// PluginPresent reports the plugin installed in the home, enabled or not
func PluginPresent(plugin, h string) bool {
	info, err := os.Stat(filepath.Join(home.PluginsDir(h), plugin))
	return err == nil && info.IsDir()
}

// Python int of a config scalar
func pyInt(v any) (int, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int:
		return t, true
	case int64:
		return int(t), true
	case uint64:
		return int(t), true
	case float64:
		return int(t), true
	case json.Number:
		n, err := strconv.Atoi(t.String())
		return n, err == nil
	case string:
		n, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(t), "_", ""))
		return n, err == nil
	}
	return 0, false
}

// One line cause of an install error, git's own fatal or error line when present
func leftCoreFirstCause(errText string) string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(errText, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	cause := ""
	found := false
	for _, line := range lines {
		if strings.HasPrefix(line, "fatal:") || strings.HasPrefix(line, "error:") {
			cause = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			found = true
			break
		}
	}
	if !found {
		if len(lines) > 0 {
			cause = lines[0]
		} else {
			cause = "unknown error"
		}
	}
	return strings.TrimRight(cause, ". ")
}
