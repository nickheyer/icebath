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
	"unicode"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"gopkg.in/yaml.v3"
)

// Env reference lookup load_config expands config templates with, the process env
func EnvRefLookup(name string) (string, bool) { return envRefLookup(name) }

// Process exit status a config command ends with, its message already printed
type ConfigExit struct{ Code int }

func (e *ConfigExit) Error() string { return "exit " + strconv.Itoa(e.Code) }

// Readers and writers config set, get, unset, show and check reach outside this package
type ConfigCommandSources struct {
	RuntimeKeyOwners        RuntimeKeyOwners
	LeftCoreTerminalKeys    map[string]string
	DisplayOverrideableKeys map[string]bool
	IsEnvSettingKey         func(key string) bool
	SaveEnvSetting          func(ctx context.Context, h, key, value string) error
	ReadEnvSetting          func(h, key string) (string, bool)
	RemoveEnvSetting        func(ctx context.Context, h, key string) (bool, error)
	DropStaleModelRoute     func(modelCfg map[string]any, provider string, cfg map[string]any) (map[string]any, bool)
	Language                LanguageCatalog
	Show                    func(ctx context.Context, h string) (ShowSources, error)
	CheckDiagnostics        CheckDiagnosticsSources
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

var (
	openDictTopLevelKeys = setOf("providers", "credential_pool_strategies", "mcp_servers", "hooks", "quick_commands",
		"personalities", "command_allowlist", "model_catalog", "channel_prompts", "server_actions", "secrets", "goals", "loops")
	schemaDefinedDictKeys = setOf("discord", "telegram", "slack", "whatsapp", "signal", "mattermost", "matrix", "feishu",
		"wecom", "weixin", "bluebubbles", "qqbot", "yuanbao", "email", "sms", "dingtalk", "sessions", "checkpoints", "plugins")
	dynamicTopLevelKeys   = setOf("custom_providers")
	platformContainerKeys = setOf("platforms")
)

func setOf(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// Reports whether a root accepts any sub key without a schema walk
func openSubkeyTopLevel(key string) bool {
	return openDictTopLevelKeys[key] || dynamicTopLevelKeys[key] || schemaDefinedDictKeys[key]
}

// Every known top level config key, defaults plus the extra and open roots
func knownTopLevelKeys() map[string]bool {
	out := map[string]bool{}
	for k := range Defaults() {
		out[k] = true
	}
	for _, set := range []map[string]bool{ExtraRootKeys, openDictTopLevelKeys, dynamicTopLevelKeys, schemaDefinedDictKeys} {
		for k := range set {
			out[k] = true
		}
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

// Known flag and suggestion for a CLI key, the schema walk then the runtime read keys
func ValidateConfigKey(key string, src ConfigCommandSources) (bool, string) {
	known, suggestion := validateSchemaKey(key, src)
	if known || key == "" {
		return known, suggestion
	}
	if IsRuntimeReadKey(SplitPath(key), FacadeRuntimeKeyOwners(src.RuntimeKeyOwners)) {
		return true, ""
	}
	return false, suggestion
}

func validateSchemaKey(key string, src ConfigCommandSources) (bool, string) {
	if key == "" {
		return false, ""
	}
	segments := SplitPath(key)
	top := segments[0]
	if strings.HasPrefix(top, "_") || platformContainerKeys[top] || src.LeftCoreTerminalKeys[key] != "" {
		return true, ""
	}
	known := knownTopLevelKeys()
	if !known[top] {
		suggestion := suggestClosestKey(top, known)
		if suggestion == "" {
			return false, ""
		}
		if rest := strings.Join(segments[1:], "."); rest != "" {
			return false, suggestion + "." + rest
		}
		return false, suggestion
	}
	if openSubkeyTopLevel(top) {
		return true, ""
	}
	defaults := Defaults()
	var node any = map[string]any(defaults)[top]
	consumed := []string{top}
	for _, seg := range segments[1:] {
		m, isMap := node.(map[string]any)
		if platformContainerKeys[seg] || !isMap || len(m) == 0 {
			return true, ""
		}
		child, ok := m[seg]
		if !ok {
			rest := strings.Join(segments[len(consumed):], ".")
			head := SplitPath(rest)[0]
			if _, inDefaults := defaults[head]; (inDefaults || openSubkeyTopLevel(head)) && knownKey(rest, src) {
				return false, rest
			}
			names := map[string]bool{}
			for k := range m {
				names[k] = true
			}
			if sibling := suggestClosestKey(seg, names); sibling != "" {
				return false, strings.Join(append(slices.Clone(consumed), sibling), ".")
			}
			return false, ""
		}
		consumed = append(consumed, seg)
		node = child
	}
	return true, ""
}

func knownKey(key string, src ConfigCommandSources) bool {
	ok, _ := ValidateConfigKey(key, src)
	return ok
}

// Reports whether a suggestion proves the key only carries an extra prefix
func isWrongPrefixSuggestion(key, suggestion string, src ConfigCommandSources) bool {
	if suggestion == "" {
		return false
	}
	keySegs, sugSegs := SplitPath(key), SplitPath(suggestion)
	return len(sugSegs) < len(keySegs) && slices.Equal(keySegs[len(keySegs)-len(sugSegs):], sugSegs) &&
		knownKey(suggestion, src)
}

// Leaf default declared for a dotted key, nil for sections and misses
func defaultValueForKey(key string) any {
	node, _ := CfgGet(Defaults(), SplitPath(key)...)
	if _, isMap := node.(map[string]any); isMap {
		return nil
	}
	return node
}

// Integer for a clean Python int literal, signs, whitespace, digit underscores and digits of any script allowed
func coerceInt(value string) (any, bool) {
	n, ok := pyint.Parse(value)
	if !ok {
		return nil, false
	}
	return pyint.Native(n), true
}

// Decimal text of a float literal the way Decimal reads it, digits folded to ASCII and underscores dropped
func decimalLiteral(value string) string {
	var b strings.Builder
	for _, r := range strings.TrimPrefix(strings.TrimFunc(value, isPySpace), "+") {
		switch {
		case r == '_':
		case r > unicode.MaxASCII && unicode.Is(unicode.Nd, r):
			start := r
			for unicode.Is(unicode.Nd, start-1) {
				start--
			}
			b.WriteByte(byte('0' + (r-start)%10))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Float only when the conversion keeps the literal's decimal value, NaN and infinities rejected
func coerceFloat(value string) (any, bool) {
	f, err := pyint.ParseFloat(value)
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

// Coerces a config set string to bool, null, int, float, list or mapping, string settings kept verbatim
func coerceConfigSetValue(key, value string) (any, error) {
	if _, isStr := defaultValueForKey(key).(string); isStr && !(key == "model" && LooksStructuredValue(value)) {
		return value, nil
	}
	stripped := strings.TrimFunc(value, isPySpace)
	if v, ok := scalarWords[pystr.Lower(stripped)]; ok {
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

// List or mapping when the schema, the container table or the value on disk fixes the key to one
func expectedContainerType(key string, userConfig map[string]any) string {
	parts := SplitPath(key)
	schema, _ := CfgGet(Defaults(), parts...)
	if _, isMap := schema.(map[string]any); len(parts) == 1 && isMap {
		schema = nil
	}
	existing, _ := Tree(userConfig).Lookup(key)
	var table any
	if t, ok := KnownContainerTypes[key]; ok {
		table = t
	}
	for _, node := range []any{schema, table, existing} {
		if _, isMap := node.(map[string]any); isMap || node == "mapping" {
			return "mapping"
		}
		if _, isList := node.([]any); isList || node == "list" {
			return "list"
		}
	}
	return ""
}

// Value to store after refusing a wrong shaped value for a list or mapping slot
func refuseContainerTypeMismatch(key string, value any, userConfig map[string]any, force bool) (any, error) {
	expected := expectedContainerType(key, userConfig)
	if expected == "" {
		return value, nil
	}
	if s, isStr := value.(string); expected == "list" && isStr && ScalarAsOneItemListKeys[key] {
		return []any{s}, nil
	}
	_, isList := value.([]any)
	_, isMap := value.(map[string]any)
	if (expected == "list" && isList) || (expected == "mapping" && (isMap || force)) {
		return value, nil
	}
	got := "string"
	if _, isStr := value.(string); !isStr {
		got = PyTypeName(value)
	}
	literal := "{key: value}"
	if expected == "list" {
		literal = "[item, ...]"
	}
	return nil, exitInvalid(fmt.Sprintf("✗ Cannot set '%s': it must be a %s, got a %s — nothing was written.\n"+
		"  Pass a YAML/JSON literal, e.g.:\n    icebath config set %s '%s'\n  or edit config.yaml directly.", key, expected, got, key, literal))
}

// Canonical path for a per platform display key and the note to print when it moved
func redirectPlatformDisplayKey(key string, src ConfigCommandSources) (string, string) {
	segs := SplitPath(key)
	note := ""
	if len(segs) >= 3 && segs[0] == "gateway" && segs[1] == "platforms" {
		segs = segs[1:]
		key = strings.Join(segs, ".")
		note = fmt.Sprintf("  (note: the top-level platforms.%s block outranks gateway.platforms — saved as %s)", segs[1], key)
	}
	if len(segs) != 3 || segs[0] != "platforms" || src.DisplayOverrideableKeys == nil || !src.DisplayOverrideableKeys[segs[2]] {
		return key, note
	}
	canonical := "display.platforms." + segs[1] + "." + segs[2]
	return canonical, "  (note: per-platform display setting — saved as " + canonical + ")"
}

// The gateway.platforms spelling the user typed, empty when they typed another path
func legacyGatewayPlatformsKey(requested string) string {
	segs := SplitPath(requested)
	if len(segs) >= 3 && segs[0] == "gateway" && segs[1] == "platforms" {
		return strings.Join(segs, ".")
	}
	return ""
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
	level, locked, err := backend.Locked(ctx, filepath.Dir(home.ConfigPath(h)), key)
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

// Writes the raw user config, reporting the change to the saved observer when one is installed
func writeUserConfig(ctx context.Context, h, configPath string, userConfig map[string]any) error {
	if err := EnsureIcebathHome(ctx, h); err != nil {
		return err
	}
	facadeMu.RLock()
	observe := savedObserver
	facadeMu.RUnlock()
	var before map[string]any
	snapshot := false
	if observe != nil && ctx.Value(appliedWriteKey{}) == nil {
		if raw, err := ReadUserConfigRaw(ctx, configPath); err == nil {
			before, snapshot = raw, true
		}
	}
	if err := AtomicConfigReplace(ctx, h, configPath, userConfig, ""); err != nil {
		return err
	}
	if snapshot {
		observe(h, before, userConfig)
	}
	return nil
}

func printUnknownKeyNotice(key, suggestion string) {
	fmt.Fprintln(FacadeStdout, colorize("⚠ '"+key+"' is not a recognized config key — it was saved anyway, but Icebath may not read it.", ansiYellow))
	if suggestion != "" {
		fmt.Fprintln(FacadeStdout, colorize("  Did you mean: "+suggestion, ansiYellow))
	}
	if len(SplitPath(key)) == 1 {
		fmt.Fprintln(FacadeStdout, colorize("  (Custom top-level keys are supported and bridged to the environment for skills/external tools. "+
			"Use --force to skip this notice.)", ansiDim))
		return
	}
	fmt.Fprintln(FacadeStdout, colorize("  (Use --force to skip this notice.)", ansiDim))
}

// Sets a dotted config key, routing env shaped keys to .env and refusing wrong shapes, managed and locked keys
func SetConfigValue(ctx context.Context, h, key, value string, force bool, src ConfigCommandSources) error {
	if ManagedSystem(ctx, h) != "" {
		ManagedError(ctx, h, "set configuration values")
		return nil
	}
	if key != strings.TrimFunc(key, isPySpace) || strings.TrimFunc(key, isPySpace) == "" {
		return exitInvalid(fmt.Sprintf("✗ Invalid config key: %s (empty or surrounding whitespace).", pyStringRepr(key)))
	}
	if slices.Contains(SplitPath(key), "") {
		return exitInvalid(fmt.Sprintf("✗ Invalid config key: %s — contains an empty path segment (leading, trailing, or doubled '.').", pyStringRepr(key)))
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
		fmt.Fprintf(FacadeStdout, "✓ Set %s in %s\n", key, home.EnvPath(h))
		return nil
	}
	if src.IsEnvSettingKey(key) {
		if err := src.SaveEnvSetting(ctx, h, key, value); err != nil {
			return exitInvalid("✗ " + err.Error())
		}
		fmt.Fprintf(FacadeStdout, "✓ Set %s in %s\n", strings.ToUpper(key), home.EnvPath(h))
		return nil
	}
	legacy := legacyGatewayPlatformsKey(key)
	key, note := redirectPlatformDisplayKey(key, src)
	if note != "" {
		fmt.Fprintln(FacadeStdout, note)
	}
	known, suggestion := ValidateConfigKey(key, src)
	if !known && !force && isWrongPrefixSuggestion(key, suggestion, src) {
		return exitInvalid(UnknownSubkeyRefusal(key, suggestion))
	}
	if key == "display.language" {
		if msg := DisplayLanguageError(value, src.Language); msg != "" {
			return exitInvalid(msg)
		}
	}
	configPath := home.ConfigPath(h)
	userConfig, err := RequireReadableConfigBeforeWrite(ctx, configPath)
	if err != nil {
		return err
	}
	coerced, err := coerceConfigSetValue(key, value)
	if err != nil {
		return err
	}
	modelVal := userConfig["model"]
	if s, isStr := modelVal.(string); strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "model.") && isStr && s != "" {
		setOrdered(userConfig, "model", map[string]any{"default": s})
	}
	key, err = GuardSectionOverwrite(key, coerced, userConfig, force, FacadeStdout)
	if err != nil {
		var refusal *ConfigSetRefusal
		if errors.As(err, &refusal) {
			return exitInvalid(refusal.Message)
		}
		return err
	}
	coerced, err = refuseContainerTypeMismatch(key, coerced, userConfig, force)
	if err != nil {
		return err
	}
	var oldProvider any
	if m, isMap := modelVal.(map[string]any); isMap {
		oldProvider = m["provider"]
	}
	if err := Tree(userConfig).Set(key, coerced); err != nil {
		var phantom *PhantomKeyError
		var pathErr *PathError
		if errors.As(err, &phantom) || (errors.As(err, &pathErr) && pathErr.Kind == "ValueError") {
			return exitInvalid("✗ " + err.Error())
		}
		return err
	}
	if legacy != "" && Tree(userConfig).Unset(legacy) {
		fmt.Fprintf(FacadeStdout, "  (removed the shadowed %s duplicate)\n", legacy)
	}
	routeNotice := ""
	oldName := strings.TrimSpace(pyOrEmpty(oldProvider))
	if oldName == "" {
		oldName = "the previous provider"
	}
	newName := PyStr(coerced)
	if key == "model.provider" && strings.ToLower(oldName) != strings.ToLower(strings.TrimSpace(newName)) {
		modelCfg, _ := userConfig["model"].(map[string]any)
		popped, unverified := src.DropStaleModelRoute(modelCfg, newName, userConfig)
		if len(popped) > 0 {
			var parts []string
			for _, k := range MappingKeys(popped) {
				parts = append(parts, fmt.Sprintf("model.%s (%s)", k, PyStr(popped[k])))
			}
			routeNotice = "  Cleared " + strings.Join(parts, ", ") + " — that route belonged to " + oldName + ", not " + newName +
				". " + newName + "'s endpoint resolves automatically; set model.base_url again if you meant a custom endpoint."
		} else if unverified {
			routeNotice = colorize(fmt.Sprintf("⚠ model.base_url (%s) was set under %s and still applies to %s — requests go there. "+
				"If it is not %s's endpoint: `icebath config unset model.base_url` (and model.api_mode).",
				PyStr(modelCfg["base_url"]), oldName, newName, newName), ansiYellow)
		}
	}
	if k := strings.ToLower(strings.TrimSpace(key)); k == "model.api_base" || k == "api_base" {
		userConfig = normalizeModelKeys(userConfig)
		key = "model.base_url"
		fmt.Fprintln(FacadeStdout, "  (note: 'api_base' is an alias — saved as model.base_url)")
	}
	if err := writeUserConfig(PinningKeys(ctx, key), h, configPath, userConfig); err != nil {
		return err
	}
	if envVar, ok := TerminalConfigEnvVarForKey(key); ok && key != "terminal.cwd" {
		if err := SaveEnvValueContext(ctx, h, envVar, terminalEnvValueText(coerced)); err != nil {
			return err
		}
	}
	touchSkinFile(h, key, coerced)
	if key == "display.language" && src.Language != nil {
		src.Language.ResetLanguageCache()
	}
	display := coerced
	if s, isStr := coerced.(string); isSecretConfigKey(key) && isStr && s != "" {
		display = MaskSecret(s, "")
	}
	dest, err := ConfigDestination(ctx, configPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(FacadeStdout, "✓ Set %s = %s in %s\n", key, PyStr(display), dest)
	if routeNotice != "" {
		fmt.Fprintln(FacadeStdout, routeNotice)
	}
	if !known && !force {
		printUnknownKeyNotice(key, suggestion)
	}
	return nil
}

// Prints a resolved config value, credentials masked unless raw or redaction is off
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
	default:
		legacy := legacyGatewayPlatformsKey(key)
		key, _ = redirectPlatformDisplayKey(key, src)
		cfg, err := LoadContext(ctx, h, EnvRefLookupContext(ctx))
		if err != nil {
			return err
		}
		value, found = cfg.Lookup(key)
		if !found && legacy != "" {
			value, found = cfg.Lookup(legacy)
		}
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
	if knownTopLevelKeys()[SplitPath(key)[0]] {
		if known, suggestion := ValidateConfigKey(key, src); !known {
			fmt.Fprintln(FacadeNoticeOut, colorize("⚠ '"+key+"' is not a recognized config key — Icebath may not read it; "+
				"the value printed above comes from your config file.", ansiYellow))
			if suggestion != "" {
				fmt.Fprintln(FacadeNoticeOut, colorize("  Did you mean: "+suggestion, ansiYellow))
			}
		}
	}
	return nil
}

// Removes a user set config or .env value, refusing managed and locked keys
func UnsetConfigValue(ctx context.Context, h, key string, src ConfigCommandSources) error {
	if ManagedSystem(ctx, h) != "" {
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
		fmt.Fprintf(FacadeStdout, "✓ Unset %s from %s\n", key, home.EnvPath(h))
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
		fmt.Fprintf(FacadeStdout, "✓ Unset %s from %s\n", key, home.EnvPath(h))
		return nil
	}
	configPath := home.ConfigPath(h)
	userConfig, err := RequireReadableConfigBeforeWrite(ctx, configPath)
	if err != nil {
		return err
	}
	legacy := legacyGatewayPlatformsKey(key)
	key, note := redirectPlatformDisplayKey(key, src)
	if note != "" {
		fmt.Fprintln(FacadeStdout, strings.ReplaceAll(note, "saved as", "resolved as"))
	}
	removed := Tree(userConfig).Unset(key)
	if legacy != "" {
		removed = Tree(userConfig).Unset(legacy) || removed
	}
	envVar, ok := TerminalConfigEnvVarForKey(key)
	if !ok {
		envVar, ok = src.LeftCoreTerminalKeys[key]
	}
	if ok && envVar != "" && key != "terminal.cwd" {
		found, err := RemoveHomeEnvValue(ctx, h, envVar)
		if err != nil {
			return err
		}
		removed = found || removed
	}
	if !removed {
		return exitInvalid("Config key not set: " + key)
	}
	if err := writeUserConfig(ctx, h, configPath, userConfig); err != nil {
		return err
	}
	dest, err := ConfigDestination(ctx, configPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(FacadeStdout, "✓ Unset %s from %s\n", key, dest)
	return nil
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
	Force   bool
}

type configUsage struct {
	usage    string
	examples []string
	extra    []string
}

var (
	usageGet = configUsage{"Usage: icebath config get <key> [--json] [--raw]", []string{"icebath config get model",
		"icebath config get terminal.backend", "icebath config get skills.config --json"}, nil}
	usageSet = configUsage{"Usage: icebath config set [--force] <key> <value>", []string{
		"icebath config set model anthropic/claude-sonnet-4", "icebath config set terminal.backend docker",
		"icebath config set OPENROUTER_API_KEY sk-or-..."}, []string{"", "  --force: skip the unknown-key notice for unrecognized keys,",
		"           and allow a scalar to replace a whole mapping section"}}
	usageUnset = configUsage{"Usage: icebath config unset <key>", []string{"icebath config unset model",
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

// Shows what migrate would change and runs the interactive migration
func cmdConfigMigrate(ctx context.Context, h string) error {
	printBanner("\U0001f504 Checking configuration for updates...")
	missingEnv, err := GetMissingEnvVars(ctx, h, false)
	if err != nil {
		return err
	}
	missingConfig, err := GetMissingConfigFields(ctx, h, EnvRefLookupContext(ctx))
	if err != nil {
		return err
	}
	current, latest, err := CheckConfigVersion(ctx, h, true)
	if err != nil {
		return err
	}
	if len(missingEnv) == 0 && len(missingConfig) == 0 && current >= latest {
		fmt.Fprintln(FacadeStdout, colorize("✓ Configuration is up to date!", ansiGreen))
		fmt.Fprintln(FacadeStdout)
		return nil
	}
	if current < latest {
		fmt.Fprintf(FacadeStdout, "  Config version: %d → %d\n", current, latest)
	}
	if len(missingConfig) > 0 {
		fmt.Fprintf(FacadeStdout, "\n  %d new config option(s) will be added with defaults\n", len(missingConfig))
	}
	var required, optional []MissingEnvVar
	for _, v := range missingEnv {
		if v.IsRequired {
			required = append(required, v)
		} else if !v.Advanced {
			optional = append(optional, v)
		}
	}
	groups := []struct {
		heading string
		group   []MissingEnvVar
		suffix  string
	}{{"⚠️  %d required API key(s) missing:", required, ""}, {"ℹ️  %d optional API key(s) not configured:", optional, " (enables: {})"}}
	for _, g := range groups {
		if len(g.group) == 0 {
			continue
		}
		fmt.Fprintf(FacadeStdout, "\n  "+g.heading+"\n", len(g.group))
		for _, v := range g.group {
			suffix := ""
			if g.suffix != "" {
				suffix = toolsSuffix(v.EnvVar, g.suffix)
			}
			fmt.Fprintf(FacadeStdout, "     • %s%s\n", v.Name, suffix)
		}
	}
	fmt.Fprintln(FacadeStdout)
	results, err := MigrateConfig(ctx, h, true, false)
	if err != nil {
		return err
	}
	fmt.Fprintln(FacadeStdout)
	if len(results.EnvAdded) > 0 || len(results.ConfigAdded) > 0 {
		fmt.Fprintln(FacadeStdout, colorize("✓ Configuration updated!", ansiGreen))
	}
	if len(results.Warnings) > 0 {
		fmt.Fprintln(FacadeStdout)
		for _, w := range results.Warnings {
			fmt.Fprintln(FacadeStdout, colorize("  ⚠️  "+w, ansiYellow))
		}
	}
	fmt.Fprintln(FacadeStdout)
	return nil
}

// Non interactive report of missing and stale configuration
func cmdConfigCheck(ctx context.Context, h string, src ConfigCommandSources) error {
	printBanner("\U0001f4cb Configuration Status")
	current, latest, err := CheckConfigVersion(ctx, h, true)
	if err != nil {
		return err
	}
	if current >= latest {
		fmt.Fprintf(FacadeStdout, "  Config version: %d ✓\n", current)
	} else {
		fmt.Fprintln(FacadeStdout, colorize(fmt.Sprintf("  Config version: %d → %d (update available)", current, latest), ansiYellow))
	}
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
	missingConfig, err := GetMissingConfigFields(ctx, h, EnvRefLookupContext(ctx))
	if err != nil {
		return err
	}
	if len(missingConfig) > 0 {
		fmt.Fprintln(FacadeStdout)
		fmt.Fprintln(FacadeStdout, colorize(fmt.Sprintf("  %d new config option(s) available", len(missingConfig)), ansiYellow))
		fmt.Fprintln(FacadeStdout, "    Run 'icebath config migrate' to add them")
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
  icebath config check     Check for missing, outdated, or inactive config
  icebath config migrate   Update config with new options
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
	return runWriteCommand(SetConfigValue(ctx, h, args.Key, *args.Value, args.Force, src))
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
	return ShowConfig(FacadeStdout, show)
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
		handler = func() error { fmt.Fprintln(FacadeStdout, home.ConfigPath(h)); return nil }
	case "env-path":
		handler = func() error { fmt.Fprintln(FacadeStdout, home.EnvPath(h)); return nil }
	case "migrate":
		handler = func() error { return cmdConfigMigrate(ctx, h) }
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
