package config

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/home"
	"gopkg.in/yaml.v3"
)

// Env reference lookup load_config expands config templates with, the process env
func EnvRefLookup(name string) (string, bool) { return envRefLookup(name) }

// Process exit status a config command ends with, its message already printed
type ConfigExit struct{ Code int }

func (e *ConfigExit) Error() string { return "exit " + strconv.Itoa(e.Code) }

// Readers and writers config set, get, unset, show and check reach outside this package
type ConfigCommandSources struct {
	// Registers the settings blocks plugins declare, so their keys are known before a lookup
	LoadPluginSettings  func()
	IsEnvSettingKey     func(key string) bool
	SaveEnvSetting      func(ctx context.Context, h, key, value string) error
	ReadEnvSetting      func(h, key string) (string, bool)
	RemoveEnvSetting    func(ctx context.Context, h, key string) (bool, error)
	DropStaleModelRoute func(modelCfg map[string]any, provider string, cfg map[string]any) (map[string]any, bool)
	Language            LanguageCatalog
	Show                func(ctx context.Context, h string) (ShowSources, error)
	CheckDiagnostics    CheckDiagnosticsSources
}

const (
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiCyan   = "\033[36m"
	ansiBold   = "\033[1m"
)

// Prints a message to standard error and ends the command with status 1
func exitInvalid(msg string) error {
	fmt.Fprintln(FacadeNoticeOut, msg)
	return &ConfigExit{Code: 1}
}

// Prints the managed install refusal for an action to standard error
func ManagedError(ctx context.Context, h, action string) {
	fmt.Fprintln(FacadeNoticeOut, FormatManagedMessage(ctx, h, action))
}

func setOf(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// Closest candidate to a typed key above the difflib cutoff, empty when none
func suggestClosestKey(key string, candidates map[string]bool) string {
	names := make([]string, 0, len(candidates))
	for k := range candidates {
		names = append(names, k)
	}
	slices.Sort(names)
	if m := closeMatches(key, names, 1, 0.6); len(m) > 0 {
		return m[0]
	}
	return ""
}

// Best fuzzy matches above a cutoff ranked the way difflib get_close_matches ranks them
func closeMatches(word string, possibilities []string, n int, cutoff float64) []string {
	type scored struct {
		score float64
		text  string
	}
	var results []scored
	for _, x := range possibilities {
		if s := seqRatio([]rune(x), []rune(word)); s >= cutoff {
			results = append(results, scored{s, x})
		}
	}
	slices.SortStableFunc(results, func(a, b scored) int {
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		return -strings.Compare(a.text, b.text)
	})
	out := []string{}
	for i := 0; i < len(results) && i < n; i++ {
		out = append(out, results[i].text)
	}
	return out
}

// Difflib similarity of two rune sequences, twice the matched characters over the total length
func seqRatio(a, b []rune) float64 {
	total := len(a) + len(b)
	if total == 0 {
		return 1
	}
	b2j := map[rune][]int{}
	for j, r := range b {
		b2j[r] = append(b2j[r], j)
	}
	type span struct{ alo, ahi, blo, bhi int }
	queue := []span{{0, len(a), 0, len(b)}}
	matches := 0
	for len(queue) > 0 {
		s := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		besti, bestj, best := s.alo, s.blo, 0
		j2len := map[int]int{}
		for i := s.alo; i < s.ahi; i++ {
			next := map[int]int{}
			for _, j := range b2j[a[i]] {
				if j < s.blo {
					continue
				}
				if j >= s.bhi {
					break
				}
				k := j2len[j-1] + 1
				next[j] = k
				if k > best {
					besti, bestj, best = i-k+1, j-k+1, k
				}
			}
			j2len = next
		}
		if best == 0 {
			continue
		}
		matches += best
		if s.alo < besti && s.blo < bestj {
			queue = append(queue, span{s.alo, besti, s.blo, bestj})
		}
		if besti+best < s.ahi && bestj+best < s.bhi {
			queue = append(queue, span{besti + best, s.ahi, bestj + best, s.bhi})
		}
	}
	return 2 * float64(matches) / float64(total)
}

// Settings registry calls the CLI makes, installed by the settings package
type SettingsHooks struct {
	// Whether the registry holds a key
	Known func(key string) bool
	// Writes the text typed for a registered key through the settings writer, the stored value back; known false for any other key
	Set func(ctx context.Context, key, text string) (stored any, known bool, err error)
	// Removes the value a registered key holds in config.yaml, whether one was there; known false for any other key
	Reset func(ctx context.Context, key string) (wasSet, known bool, err error)
	// Value in effect for a registered key, known false for any other key
	Get func(ctx context.Context, key string) (value any, known bool)
	// Settings groups config show prints for the home in ctx
	Show func(ctx context.Context) []ShowGroup
	// Every registered key, for the closest-match suggestion
	Keys func() []string
}

var settingsHooks SettingsHooks

// Installs the settings registry lookups the CLI writer consults
func RegisterSettingsHooks(h SettingsHooks) { settingsHooks = h }

// Leaf default declared for a dotted key, nil for sections and misses
func defaultValueForKey(key string) any {
	node, _ := CfgGet(Defaults(), SplitPath(key)...)
	if _, isMap := node.(map[string]any); isMap {
		return nil
	}
	return node
}

// Integer of a decimal literal with an optional sign and digits that single underscores may separate, as YAML writes one; an int when it fits, else a *big.Int
func coerceInt(value string) (any, bool) {
	s := strings.TrimSpace(value)
	sign := ""
	if s != "" && (s[0] == '+' || s[0] == '-') {
		sign, s = s[:1], s[1:]
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") || strings.Trim(s, "0123456789_") != "" {
		return nil, false
	}
	n, ok := new(big.Int).SetString(sign+strings.ReplaceAll(s, "_", ""), 10)
	if !ok {
		return nil, false
	}
	if n.IsInt64() && n.Int64() >= math.MinInt && n.Int64() <= math.MaxInt {
		return int(n.Int64()), true
	}
	return n, true
}

// Exact decimal text of a float literal strconv.ParseFloat accepted, the plus sign and digit underscores dropped
func decimalLiteral(value string) string {
	return strings.ReplaceAll(strings.TrimPrefix(strings.TrimSpace(value), "+"), "_", "")
}

// Float only when the conversion keeps the literal's decimal value, NaN and infinities rejected
func coerceFloat(value string) (any, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, false
	}
	literal, ok := new(big.Rat).SetString(decimalLiteral(value))
	shortest, ok2 := new(big.Rat).SetString(strconv.FormatFloat(f, 'g', -1, 64))
	if !ok || !ok2 || literal.Cmp(shortest) != 0 {
		return nil, false
	}
	return f, true
}

var scalarWords = map[string]any{"true": true, "yes": true, "on": true, "false": false, "no": false, "off": false,
	"null": nil, "none": nil, "~": nil}

// Value the CLI writes for typed text: bool, null, int, float, list or mapping, text of string settings kept verbatim
func ParseCLIValue(key, value string) (any, error) { return coerceConfigSetValue(key, value) }

// Coerces a config set string to bool, null, int, float, list or mapping, string settings kept verbatim
func coerceConfigSetValue(key, value string) (any, error) {
	if _, isStr := defaultValueForKey(key).(string); isStr {
		return value, nil
	}
	stripped := strings.TrimFunc(value, isPySpace)
	if v, ok := scalarWords[strings.ToLower(stripped)]; ok {
		return v, nil
	}
	if v, ok := coerceInt(stripped); ok {
		return v, nil
	}
	if v, ok := coerceFloat(stripped); ok {
		return v, nil
	}
	if !LooksStructuredValue(value) {
		return value, nil
	}
	parsed, err := FastSafeLoad([]byte(value))
	if err != nil {
		detail := strings.SplitN(YAMLErrorDetails(err), "\n", 2)[0]
		return nil, exitInvalid(fmt.Sprintf("✗ Value for '%s' looks like a list/mapping but is not valid YAML/JSON "+
			"(%s) — nothing was written.\n  Fix the literal, or quote it (e.g. \"'[text'\") to store a plain string.", key, detail))
	}
	switch parsed.(type) {
	case []any, map[string]any:
		return parsed, nil
	}
	return value, nil
}

// Refuses a key pinned by the managed layer or locked by a remote config level
func exitIfKeyManaged(ctx context.Context, h, key, action string) error {
	if IsKeyManaged(key) {
		fmt.Fprintf(FacadeNoticeOut, "Cannot %s '%s': it is managed by your administrator (%s) and cannot be changed. "+
			"Contact your administrator to modify it.\n", action, key, managedSource("config.yaml"))
		return &ConfigExit{Code: 1}
	}
	backend, err := GetConfigBackend(ctx)
	if err != nil {
		return err
	}
	level, locked, err := backend.Locked(ctx, filepath.Dir(GetConfigPath(h)), key)
	if err != nil {
		return err
	}
	if locked && level != "" {
		fmt.Fprintf(FacadeNoticeOut, "Cannot %s '%s': it is locked by the %s level of Remote Config and cannot be "+
			"changed from this agent.\n", action, key, level)
		return &ConfigExit{Code: 1}
	}
	return nil
}

// Bumps the named skin file mtime so the gateway watcher applies a display.skin set at once
func touchSkinFile(h, key string, value any) {
	s, isStr := value.(string)
	if key != "display.skin" || !isStr || s == "" {
		return
	}
	path := filepath.Join(h, "skins", s+".yaml")
	if _, err := os.Stat(path); err == nil {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
	}
}

// Writes the raw user config, replacing the whole document
func writeUserConfig(ctx context.Context, h, configPath string, userConfig map[string]any) error {
	if err := EnsureIcebathHome(ctx, h); err != nil {
		return err
	}
	return AtomicConfigReplace(ctx, h, configPath, userConfig, "")
}

// Writes a registry key through the settings writer and reports it, false when the registry does not know the key
func setRegisteredValue(ctx context.Context, h, key, value string, src ConfigCommandSources) (bool, error) {
	if settingsHooks.Set == nil || settingsHooks.Known == nil || !settingsHooks.Known(key) {
		return false, nil
	}
	if key == DisplayLanguageKey {
		if msg := DisplayLanguageError(value, src.Language); msg != "" {
			return true, exitInvalid(msg)
		}
	}
	ctx = home.WithOverride(ctx, h)
	configPath := GetConfigPath(h)
	userConfig, err := RequireReadableConfigBeforeWrite(ctx, configPath)
	if err != nil {
		return true, err
	}
	modelCfg, _ := Clone(userConfig["model"]).(map[string]any)
	oldProvider := strings.TrimSpace(pyOrEmpty(modelCfg["provider"]))
	stored, _, err := settingsHooks.Set(ctx, key, value)
	var printed *ConfigExit
	if errors.As(err, &printed) {
		return true, err
	}
	if err != nil {
		return true, exitInvalid("✗ " + err.Error())
	}
	routeNotice := ""
	if newProvider := PyStr(stored); key == "model.provider" && !strings.EqualFold(oldProvider, strings.TrimSpace(newProvider)) {
		if oldProvider == "" {
			oldProvider = "the previous provider"
		}
		if routeNotice, err = dropStaleRoute(ctx, modelCfg, oldProvider, newProvider, userConfig, src); err != nil {
			return true, err
		}
	}
	touchSkinFile(h, key, stored)
	if key == DisplayLanguageKey && src.Language != nil {
		src.Language.ResetLanguageCache()
	}
	display := stored
	if s, isStr := stored.(string); isSecretConfigKey(key) && isStr && s != "" {
		display = MaskSecret(s, "")
	}
	dest, err := ConfigDestination(ctx, configPath)
	if err != nil {
		return true, err
	}
	fmt.Fprintf(FacadeStdout, "✓ Set %s = %s in %s\n", key, PyStr(display), dest)
	if routeNotice != "" {
		fmt.Fprintln(FacadeStdout, routeNotice)
	}
	return true, nil
}

// Clears the model endpoint a provider change leaves behind through the settings writer, the notice to print
func dropStaleRoute(ctx context.Context, modelCfg map[string]any, oldName, newName string, userConfig map[string]any, src ConfigCommandSources) (string, error) {
	baseURL := PyStr(modelCfg["base_url"])
	popped, unverified := src.DropStaleModelRoute(modelCfg, newName, userConfig)
	if unverified {
		return colorize(fmt.Sprintf("⚠ model.base_url (%s) was set under %s and still applies to %s — requests go there. "+
			"If it is not %s's endpoint: `icebath config unset model.base_url` (and model.api_mode).",
			baseURL, oldName, newName, newName), ansiYellow), nil
	}
	if len(popped) == 0 {
		return "", nil
	}
	var parts []string
	for _, k := range SortedKeys(popped) {
		if _, _, err := settingsHooks.Reset(ctx, "model."+k); err != nil {
			return "", err
		}
		parts = append(parts, fmt.Sprintf("model.%s (%s)", k, PyStr(popped[k])))
	}
	return "  Cleared " + strings.Join(parts, ", ") + " — that route belonged to " + oldName + ", not " + newName +
		". " + newName + "'s endpoint resolves automatically; set model.base_url again if you meant a custom endpoint.", nil
}

// Command line text of a config value, JSON or YAML block style for containers
func formatConfigGetValue(value any, asJSON bool) string {
	if asJSON {
		return pyJSONUnicode(pyJSONDumps(value))
	}
	switch v := value.(type) {
	case bool:
		if v {
			return "true"
		}
		return "false"
	case nil:
		return "null"
	case map[string]any, []any:
		return strings.TrimRight(pySafeDump(v), " \t\n\r")
	}
	return PyStr(value)
}

// Undoes non ASCII escapes in Python JSON text, giving the ensure_ascii False form
func pyJSONUnicode(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '\\' || i+1 >= len(text) {
			b.WriteByte(c)
			continue
		}
		if text[i+1] != 'u' || i+6 > len(text) {
			b.WriteString(text[i : i+2])
			i++
			continue
		}
		code, err := strconv.ParseUint(text[i+2:i+6], 16, 32)
		if err != nil || code < 0x7f {
			b.WriteString(text[i : i+6])
			i += 5
			continue
		}
		r := rune(code)
		consumed := 6
		if utf16.IsSurrogate(r) && i+12 <= len(text) && text[i+6:i+8] == `\u` {
			if lo, err := strconv.ParseUint(text[i+8:i+12], 16, 32); err == nil {
				r = utf16.DecodeRune(r, rune(lo))
				consumed = 12
			}
		}
		b.WriteRune(r)
		i += consumed - 1
	}
	return b.String()
}

// Block style YAML in the shape PyYAML safe_dump writes with sort_keys off
func pySafeDump(value any) string {
	var b strings.Builder
	dumpBlock(&b, value, 0)
	return b.String()
}

func dumpScalar(v any) string {
	out, err := yaml.Marshal(v) // config-writer ok renders a value for display and never writes it to disk
	if err != nil {
		return PyStr(v)
	}
	return strings.TrimSuffix(string(out), "\n")
}

func dumpBlock(b *strings.Builder, value any, indent int) {
	pad := strings.Repeat(" ", indent)
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			b.WriteString(pad + "{}\n")
			return
		}
		for _, k := range MappingKeys(v) {
			child := v[k]
			b.WriteString(pad + dumpScalar(k) + ":")
			switch c := child.(type) {
			case map[string]any:
				if len(c) == 0 {
					b.WriteString(" {}\n")
					continue
				}
				b.WriteString("\n")
				dumpBlock(b, c, indent+2)
			case []any:
				if len(c) == 0 {
					b.WriteString(" []\n")
					continue
				}
				b.WriteString("\n")
				dumpBlock(b, c, indent)
			default:
				b.WriteString(" " + dumpScalar(child) + "\n")
			}
		}
	case []any:
		if len(v) == 0 {
			b.WriteString(pad + "[]\n")
			return
		}
		for _, item := range v {
			switch c := item.(type) {
			case map[string]any, []any:
				var inner strings.Builder
				dumpBlock(&inner, c, indent+2)
				text := inner.String()
				b.WriteString(pad + "- " + strings.TrimPrefix(text, pad+"  "))
			default:
				b.WriteString(pad + "- " + dumpScalar(item) + "\n")
			}
		}
	default:
		b.WriteString(pad + dumpScalar(value) + "\n")
	}
}

// Parsed config command line, an empty command meaning show
type ConfigArgs struct {
	Command string
	Key     string
	Value   *string
	JSON    bool
	Raw     bool
}

type configUsage struct {
	usage    string
	examples []string
	extra    []string
}

var (
	usageGet = configUsage{"Usage: icebath config get <key> [--json] [--raw]", []string{"icebath config get model.default",
		"icebath config get terminal.backend", "icebath config get skills.config --json"}, nil}
	usageSet = configUsage{"Usage: icebath config set <key> <value>", []string{
		"icebath config set model.default anthropic/claude-sonnet-4", "icebath config set terminal.backend docker",
		"icebath config set OPENROUTER_API_KEY sk-or-..."}, nil}
	usageUnset = configUsage{"Usage: icebath config unset <key>", []string{"icebath config unset model.default",
		"icebath config unset terminal.backend", "icebath config unset OPENROUTER_API_KEY"}, nil}
)

// Prints a usage block with examples and ends the command with status 1
func usageExit(u configUsage) error {
	fmt.Fprintln(FacadeStdout, u.usage)
	fmt.Fprintln(FacadeStdout)
	fmt.Fprintln(FacadeStdout, "Examples:")
	for _, line := range u.examples {
		fmt.Fprintf(FacadeStdout, "  %s\n", line)
	}
	for _, line := range u.extra {
		fmt.Fprintln(FacadeStdout, line)
	}
	return &ConfigExit{Code: 1}
}

// Runs a config writer, only a RuntimeError class refusal becoming a clean status 1 message
func runWriteCommand(err error) error {
	if err != nil && isRuntimeError(err) {
		return exitInvalid("✗ " + err.Error())
	}
	return err
}

func toolsSuffix(info EnvVar, format string) string {
	if len(info.Tools) == 0 {
		return ""
	}
	return strings.Replace(format, "{}", strings.Join(info.Tools[:min(2, len(info.Tools))], ", "), 1)
}

func printBanner(text string) {
	fmt.Fprintln(FacadeStdout)
	fmt.Fprintln(FacadeStdout, colorize(text, ansiCyan, ansiBold))
	fmt.Fprintln(FacadeStdout)
}

// Non interactive report of missing and stale configuration
func cmdConfigCheck(ctx context.Context, h string, src ConfigCommandSources) error {
	printBanner("\U0001f4cb Configuration Status")
	groups := []struct {
		title   string
		table   map[string]EnvVar
		names   []string
		missing func(name string, info EnvVar) string
	}{
		{"Required", RequiredEnvVars, SortedEnvNames(RequiredEnvVars), func(n string, _ EnvVar) string {
			return colorize("    ✗ "+n+" (missing)", ansiRed)
		}},
		{"Optional", OptionalEnvVars.Copy(), OptionalEnvVars.Names(), func(n string, i EnvVar) string {
			return colorize("    ○ "+n+toolsSuffix(i, " → {}"), ansiDim)
		}},
	}
	for _, g := range groups {
		fmt.Fprintln(FacadeStdout)
		fmt.Fprintln(FacadeStdout, colorize("  "+g.title+":", ansiBold))
		for _, name := range g.names {
			v, _, err := GetEnvValue(ctx, h, name)
			if err != nil {
				return err
			}
			if v != "" {
				fmt.Fprintf(FacadeStdout, "    ✓ %s\n", name)
			} else {
				fmt.Fprintln(FacadeStdout, g.missing(name, g.table[name]))
			}
		}
	}
	raw, err := ReadRawConfigReadonly(ctx, h)
	if err != nil {
		return err
	}
	var envErr error
	getEnv := func(name string) string {
		v, _, err := GetEnvValue(ctx, h, name)
		if err != nil && envErr == nil {
			envErr = err
		}
		return v
	}
	diagnostics, err := ConfigCheckDiagnostics(raw, getEnv, src.CheckDiagnostics)
	if err != nil {
		return err
	}
	if envErr != nil {
		return envErr
	}
	if len(diagnostics) > 0 {
		fmt.Fprintln(FacadeStdout)
		fmt.Fprintln(FacadeStdout, colorize("  Saved configuration:", ansiBold))
		for _, d := range diagnostics {
			fmt.Fprintln(FacadeStdout, colorize("    ⚠ "+d, ansiYellow))
		}
	}
	fmt.Fprintln(FacadeStdout)
	return nil
}

const configUsageText = `Available commands:
  icebath config           Show current configuration
  icebath config edit      Open config in editor
  icebath config get <key>          Print a resolved config value
  icebath config set <key> <value>   Set a config value
  icebath config unset <key>        Remove a config value
  icebath config check     Check which keys are set and what the saved config leaves inactive
  icebath config path      Show config file path
  icebath config env-path  Show .env file path`

// Prints a resolved config value, the get usage when no key is given
func cmdConfigGet(ctx context.Context, h string, args ConfigArgs, src ConfigCommandSources) error {
	if args.Key == "" {
		return usageExit(usageGet)
	}
	return GetConfigValue(ctx, h, args.Key, args.JSON, args.Raw, src)
}

// Sets a config value, the set usage when the key or value is missing
func cmdConfigSet(ctx context.Context, h string, args ConfigArgs, src ConfigCommandSources) error {
	if args.Key == "" || args.Value == nil {
		return usageExit(usageSet)
	}
	return runWriteCommand(SetConfigValue(ctx, h, args.Key, *args.Value, src))
}

// Removes a config value, the unset usage when no key is given
func cmdConfigUnset(ctx context.Context, h string, args ConfigArgs, src ConfigCommandSources) error {
	if args.Key == "" {
		return usageExit(usageUnset)
	}
	return runWriteCommand(UnsetConfigValue(ctx, h, args.Key, src))
}

// Prints every config section through the show screen
func cmdConfigShow(ctx context.Context, h string, src ConfigCommandSources) error {
	show, err := src.Show(ctx, h)
	if err != nil {
		return err
	}
	return ShowConfig(home.WithOverride(ctx, h), FacadeStdout, show)
}

// Runs one config subcommand, a backend write refusal printed as an error with status 1
func ConfigCommand(ctx context.Context, h string, args ConfigArgs, src ConfigCommandSources) error {
	var handler func() error
	switch args.Command {
	case "", "show":
		handler = func() error { return cmdConfigShow(ctx, h, src) }
	case "edit":
		handler = func() error { return EditConfig(ctx, h) }
	case "get":
		handler = func() error { return cmdConfigGet(ctx, h, args, src) }
	case "set":
		handler = func() error { return cmdConfigSet(ctx, h, args, src) }
	case "unset":
		handler = func() error { return cmdConfigUnset(ctx, h, args, src) }
	case "path":
		handler = func() error { fmt.Fprintln(FacadeStdout, GetConfigPath(h)); return nil }
	case "env-path":
		handler = func() error { fmt.Fprintln(FacadeStdout, GetEnvPath(h)); return nil }
	case "check":
		handler = func() error { return cmdConfigCheck(ctx, h, src) }
	default:
		fmt.Fprintf(FacadeStdout, "Unknown config command: %s\n\n%s\n", args.Command, configUsageText)
		return &ConfigExit{Code: 1}
	}
	err := handler()
	var writeErr *ConfigWriteError
	if errors.As(err, &writeErr) {
		fmt.Fprintf(FacadeNoticeOut, "Error: %s\n", err.Error())
		return &ConfigExit{Code: 1}
	}
	return err
}

// Whether the registry knows a key, plugin declared blocks registered first
func knownSetting(key string, src ConfigCommandSources) bool {
	if settingsHooks.Known == nil {
		return false
	}
	if settingsHooks.Known(key) {
		return true
	}
	if src.LoadPluginSettings != nil {
		src.LoadPluginSettings()
	}
	return settingsHooks.Known(key)
}

// Refusal for a key the settings registry does not know, naming the closest setting when one is near
func unknownKeyRefusal(key string) error {
	msg := "✗ " + key + " is not a setting icebath knows; nothing was written."
	if settingsHooks.Keys != nil {
		if s := suggestClosestKey(key, setOf(settingsHooks.Keys()...)); s != "" {
			msg += "\n  Did you mean: " + s
		}
	}
	return exitInvalid(msg)
}

// Checks a typed key: no surrounding whitespace and no empty path segment
func checkKeySpelling(key string) error {
	if key != strings.TrimFunc(key, isPySpace) || strings.TrimFunc(key, isPySpace) == "" {
		return exitInvalid(fmt.Sprintf("✗ Invalid config key: %s (empty or surrounding whitespace).", pyStringRepr(key)))
	}
	if slices.Contains(SplitPath(key), "") {
		return exitInvalid(fmt.Sprintf("✗ Invalid config key: %s — contains an empty path segment (leading, trailing, or doubled '.').", pyStringRepr(key)))
	}
	return nil
}

// Sets a setting through the settings writer, env shaped keys going to .env; any other key is refused
func SetConfigValue(ctx context.Context, h, key, value string, src ConfigCommandSources) error {
	if IsManaged(ctx, h) {
		ManagedError(ctx, h, "set configuration values")
		return nil
	}
	if err := checkKeySpelling(key); err != nil {
		return err
	}
	if err := exitIfKeyManaged(ctx, h, key, "set"); err != nil {
		return err
	}
	if IsEnvConfigKey(key) {
		save := hooks().SaveProviderEnvCredential
		if save == nil {
			return errors.New("provider credential lifecycle is not registered")
		}
		if err := save(ctx, h, strings.ToUpper(key), value); err != nil {
			return exitInvalid("✗ " + err.Error())
		}
		fmt.Fprintf(FacadeStdout, "✓ Set %s in %s\n", key, GetEnvPath(h))
		return nil
	}
	if src.IsEnvSettingKey(key) {
		if err := src.SaveEnvSetting(ctx, h, key, value); err != nil {
			return exitInvalid("✗ " + err.Error())
		}
		fmt.Fprintf(FacadeStdout, "✓ Set %s in %s\n", strings.ToUpper(key), GetEnvPath(h))
		return nil
	}
	if !knownSetting(key, src) {
		return unknownKeyRefusal(key)
	}
	_, err := setRegisteredValue(ctx, h, key, value, src)
	return err
}

// Prints the value in effect for a setting or an env shaped key, credentials masked unless raw or redaction is off
func GetConfigValue(ctx context.Context, h, key string, asJSON, raw bool, src ConfigCommandSources) error {
	var value any
	found := false
	switch {
	case IsEnvConfigKey(key):
		v, ok, err := GetEnvValue(ctx, h, strings.ToUpper(key))
		if err != nil {
			return err
		}
		value, found = v, ok
	case src.IsEnvSettingKey(key):
		v, ok := src.ReadEnvSetting(h, key)
		value, found = v, ok
	case knownSetting(key, src) && settingsHooks.Get != nil:
		value, found = settingsHooks.Get(home.WithOverride(ctx, h), key)
	default:
		return unknownKeyRefusal(key)
	}
	if !found {
		return exitInvalid("Config key not set: " + key)
	}
	if !raw && redactEnabled(ctx) {
		if s, isStr := value.(string); isStr {
			if isSecretConfigKey(key) && !envTemplateRe.MatchString(s) {
				value = MaskSecret(s, "")
			}
		} else {
			value = RedactConfigValue(value)
		}
	}
	fmt.Fprintln(FacadeStdout, formatConfigGetValue(value, asJSON))
	return nil
}

// Removes a setting's value through the settings writer or an env shaped key from .env, refusing managed, locked and unknown keys
func UnsetConfigValue(ctx context.Context, h, key string, src ConfigCommandSources) error {
	if IsManaged(ctx, h) {
		ManagedError(ctx, h, "unset configuration values")
		return nil
	}
	if err := exitIfKeyManaged(ctx, h, key, "unset"); err != nil {
		return err
	}
	if IsEnvConfigKey(key) {
		remove := hooks().RemoveProviderEnvCredential
		if remove == nil {
			return errors.New("provider credential lifecycle is not registered")
		}
		found, err := remove(ctx, h, strings.ToUpper(key))
		if err != nil {
			return exitInvalid("✗ " + err.Error())
		}
		if !found {
			return exitInvalid("Config key not set: " + key)
		}
		fmt.Fprintf(FacadeStdout, "✓ Unset %s from %s\n", key, GetEnvPath(h))
		return nil
	}
	if src.IsEnvSettingKey(key) {
		found, err := src.RemoveEnvSetting(ctx, h, key)
		if err != nil {
			return exitInvalid("✗ " + err.Error())
		}
		if !found {
			return exitInvalid("Config key not set: " + key)
		}
		fmt.Fprintf(FacadeStdout, "✓ Unset %s from %s\n", key, GetEnvPath(h))
		return nil
	}
	if !knownSetting(key, src) || settingsHooks.Reset == nil {
		return unknownKeyRefusal(key)
	}
	ctx = home.WithOverride(ctx, h)
	if _, err := RequireReadableConfigBeforeWrite(ctx, GetConfigPath(h)); err != nil {
		return err
	}
	wasSet, _, err := settingsHooks.Reset(ctx, key)
	if err != nil {
		return exitInvalid("✗ " + err.Error())
	}
	if !wasSet {
		return exitInvalid("Config key not set: " + key)
	}
	dest, err := ConfigDestination(ctx, GetConfigPath(h))
	if err != nil {
		return err
	}
	fmt.Fprintf(FacadeStdout, "✓ Unset %s from %s\n", key, dest)
	return nil
}
