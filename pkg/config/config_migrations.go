package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"
)

// Lowest on disk config version the ladder still migrates
const SupportFloorVersion = 12

// Explanation shown when a config predates the support floor
func SupportFloorMessage(h string) string {
	return fmt.Sprintf("This config predates version %d (~2 years old) and can no longer be auto-migrated. Back up "+
		"%s/config.yaml and run `icebath setup` to regenerate, or manually set _config_version: %d "+
		"after reviewing the changelog.", SupportFloorVersion, home.Display(h), SupportFloorVersion)
}

// What one migration pass added, warned about and must surface to the user
type MigrationResults struct {
	EnvAdded    []string
	ConfigAdded []string
	Warnings    []string
	Notices     []string
}

// Profile directory the bot mode roster names
type BotProfile struct {
	Name string
	Dir  string
}

// Personality helpers the v34 reset compares system prompts with
type PersonalitySources struct {
	Normalize  func(value any) string
	PromptText func(value any) string
	Render     func(definition any) string
	Available  func(cfg map[string]any) map[string]any
}

// Collaborators the ladder reaches outside this package, wired by the caller
type MigrationSources struct {
	Out                                 io.Writer
	ReadRaw                             func(h string) (map[string]any, error)
	Persist                             func(h string, cfg map[string]any) error
	GetEnvValue                         func(name string) string
	GetEnvValuePreferDotenv             func(name string) string
	SaveEnvValue                        func(name, value string) error
	RemoveEnvValue                      func(name string) (bool, error)
	CustomProviderEntryToProviderConfig func(entry any, providerKey string) map[string]any
	Personality                         PersonalitySources
	LegacyRelayPluginKeys               func(enabled any) []string
	BotProtocolHeading                  string
	BotRoster                           func(h string) []BotProfile
	StripLegacyBotProtocol              func(text string) string
	ParseConfigStringList               func(value any) []string
	ExplicitToolsetKeys                 func() map[string]bool
	ToolsetAllowedForPlatform           func(key, platform string) bool
	ParseEnabledFlag                    func(value any, def bool) bool
	RecordCoreUse                       func(h string, cfg map[string]any, version int) bool
	Browser                             BrowserEnvSources
}

// How a pass treats the config, unversioned files get only legacy key steps
type MigrationOptions struct {
	Unversioned bool
	ConfigOnly  bool
}

type migrationRun struct {
	h          string
	res        *MigrationResults
	quiet      bool
	src        MigrationSources
	configOnly bool
}

type migrationStep struct {
	target int
	run    func(r *migrationRun) error
}

// Prints a line to the migration output unless the pass is quiet
func (r *migrationRun) say(msg string) {
	if r.quiet {
		return
	}
	out := r.src.Out
	if out == nil {
		out = os.Stdout
	}
	fmt.Fprintln(out, msg)
}

// Raw user config through the selected backend, an empty document when missing or unparseable
func (r *migrationRun) read() map[string]any {
	read := r.src.ReadRaw
	if read == nil {
		read = func(h string) (map[string]any, error) { return ReadRawConfig(context.Background(), h) }
	}
	doc, err := read(r.h)
	if err != nil || doc == nil {
		return map[string]any{}
	}
	return doc
}

// Persists the config, records the added entry and prints the message unless quiet
func (r *migrationRun) commit(cfg map[string]any, added, message string) error {
	if err := r.src.Persist(r.h, cfg); err != nil {
		return err
	}
	if added != "" {
		r.res.ConfigAdded = append(r.res.ConfigAdded, added)
	}
	if message != "" {
		r.say(message)
	}
	return nil
}

// Mapping at a key, the same object when present so writes alias, else a fresh one
func dictAt(cfg map[string]any, key string) map[string]any {
	if m, ok := cfg[key].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

type rewriteSpec struct {
	section, key   string
	match          func(cur any, present bool) bool
	newValue       any
	remove         bool
	added, message string
	extraGuard     func(raw map[string]any) bool
	createSection  bool
}

// Rewrites or deletes one section key when its current value matches, skipping a missing section unless asked
func rewriteKey(spec rewriteSpec) func(r *migrationRun) error {
	return func(r *migrationRun) error {
		cfg := r.read()
		raw, ok := cfg[spec.section].(map[string]any)
		if !ok {
			if !spec.createSection {
				return nil
			}
			raw = map[string]any{}
		}
		cur, present := raw[spec.key]
		if !spec.match(cur, present) || (spec.extraGuard != nil && !spec.extraGuard(raw)) {
			return nil
		}
		if spec.remove {
			delete(raw, spec.key)
		} else {
			raw[spec.key] = spec.newValue
		}
		cfg[spec.section] = raw
		return r.commit(cfg, spec.added, spec.message)
	}
}

// Rewrite that fires only while the key still equals the old default
func rewriteStaleDefault(old any, spec rewriteSpec) func(r *migrationRun) error {
	spec.match = func(cur any, _ bool) bool { return pyEq(cur, old) }
	return rewriteKey(spec)
}

// Matches a string whose trimmed lowercase form is the word
func lowerIs(word string) func(cur any, present bool) bool {
	return func(cur any, _ bool) bool {
		s, ok := cur.(string)
		return ok && strings.ToLower(strings.TrimSpace(s)) == word
	}
}

// Numeric value of a bool or number for Python style equality
func pyNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// Python equality where bools and numbers compare by value through nested lists and mappings
func pyEq(a, b any) bool {
	if af, ok := pyNumber(a); ok {
		bf, ok := pyNumber(b)
		return ok && af == bf
	}
	switch x := a.(type) {
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !pyEq(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, has := y[k]
			if !has || !pyEq(v, w) {
				return false
			}
		}
		return true
	}
	if _, ok := pyNumber(b); ok {
		return false
	}
	return reflect.DeepEqual(a, b)
}

// Reports whether a list holds a value equal to the target
func pyContains(list []any, target any) bool {
	for _, v := range list {
		if pyEq(v, target) {
			return true
		}
	}
	return false
}

// TypeError Python raises when an unhashable value meets a set
func pyUnhashable(v any) error {
	switch v.(type) {
	case []any:
		return errors.New("unhashable type: 'list'")
	case map[string]any:
		return errors.New("unhashable type: 'dict'")
	}
	return nil
}

// Python int() of a config value, false on TypeError or ValueError, error on overflow
func pyIntOf(v any) (int, bool, error) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true, nil
		}
		return 0, true, nil
	case int:
		return x, true, nil
	case int64:
		return int(x), true, nil
	case uint64:
		return int(x), true, nil
	case float64:
		if math.IsInf(x, 0) {
			return 0, false, errors.New("cannot convert float infinity to integer")
		}
		if math.IsNaN(x) {
			return 0, false, nil
		}
		return int(x), true, nil
	case string:
		s := strings.TrimFunc(x, unicode.IsSpace)
		body := strings.TrimLeft(s, "+-")
		if len(s)-len(body) > 1 || body == "" || strings.HasPrefix(body, "_") || strings.HasSuffix(body, "_") || strings.Contains(body, "__") {
			return 0, false, nil
		}
		n, err := strconv.Atoi(strings.ReplaceAll(s, "_", ""))
		if err != nil {
			return 0, false, nil
		}
		return n, true, nil
	}
	return 0, false, nil
}

// Mapping keys in document order, keys the document did not list appended sorted
func orderedKeys(m map[string]any, order []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range order {
		if _, ok := m[k]; ok && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	for _, k := range SortedKeys(m) {
		if !seen[k] {
			out = append(out, k)
		}
	}
	return out
}

var (
	urlSchemeRe    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*$`)
	ipvFutureRe    = regexp.MustCompile(`^[vV][a-fA-F0-9]+\..+$`)
	repeatedDashRe = regexp.MustCompile(`-{2,}`)
)

// Hostname urlparse reports for a URL, its ValueError cases returned as errors
func pyURLHostname(raw string) (string, error) {
	rest := strings.TrimLeft(raw, "\x00\x01\x02\x03\x04\x05\x06\x07\x08\t\n\x0b\x0c\r\x0e\x0f\x10\x11\x12\x13\x14\x15\x16\x17\x18\x19\x1a\x1b\x1c\x1d\x1e\x1f ")
	rest = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(rest)
	if i := strings.IndexByte(rest, ':'); i > 0 && urlSchemeRe.MatchString(rest[:i]) {
		rest = rest[i+1:]
	}
	if !strings.HasPrefix(rest, "//") {
		return "", nil
	}
	end := len(rest)
	if j := strings.IndexAny(rest[2:], "/?#"); j >= 0 {
		end = j + 2
	}
	netloc := rest[2:end]
	open, closed := strings.Contains(netloc, "["), strings.Contains(netloc, "]")
	if open != closed {
		return "", errors.New("Invalid IPv6 URL")
	}
	hostPort := netloc[strings.LastIndex(netloc, "@")+1:]
	var hostname string
	if before, bracketed, ok := strings.Cut(hostPort, "["); ok {
		var port string
		hostname, port, _ = strings.Cut(bracketed, "]")
		if before != "" || (port != "" && !strings.HasPrefix(port, ":")) {
			return "", errors.New("Invalid IPv6 URL")
		}
		if strings.HasPrefix(hostname, "v") || strings.HasPrefix(hostname, "V") {
			if !ipvFutureRe.MatchString(hostname) {
				return "", errors.New("IPvFuture address is invalid")
			}
		} else if addr, err := netip.ParseAddr(hostname); err != nil || addr.Is4() {
			return "", errors.New("invalid bracketed host")
		}
	} else if open {
		return "", errors.New("Invalid IPv6 URL")
	} else {
		hostname, _, _ = strings.Cut(hostPort, ":")
	}
	if !isASCIIText(netloc) {
		n := strings.NewReplacer("@", "", ":", "", "#", "", "?", "").Replace(netloc)
		if normalized := norm.NFKC.String(n); n != normalized && strings.ContainsAny(normalized, "/?#@:") {
			return "", errors.New("netloc contains invalid characters under NFKC normalization")
		}
	}
	if name, zone, ok := strings.Cut(hostname, "%"); ok {
		return strings.ToLower(name) + "%" + zone, nil
	}
	return strings.ToLower(hostname), nil
}

// Python str of an OSError, errno and strerror and the failing path
func pyOSErrorString(err error) string {
	var pathErr *os.PathError
	var errno syscall.Errno
	if errors.As(err, &pathErr) && errors.As(pathErr.Err, &errno) {
		msg := errno.Error()
		if msg != "" {
			msg = strings.ToUpper(msg[:1]) + msg[1:]
		}
		return fmt.Sprintf("[Errno %d] %s: %s", int(errno), msg, pyStringRepr(pathErr.Path))
	}
	return err.Error()
}

// UnicodeDecodeError text Python reports for the first invalid UTF-8 sequence, nil when the bytes decode
func pyUTF8DecodeError(data []byte) error {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		b := data[i]
		need := 0
		switch {
		case b >= 0xC2 && b <= 0xDF:
			need = 1
		case b >= 0xE0 && b <= 0xEF:
			need = 2
		case b >= 0xF0 && b <= 0xF4:
			need = 3
		default:
			return fmt.Errorf("'utf-8' codec can't decode byte 0x%02x in position %d: invalid start byte", b, i)
		}
		tail := data[i+1:]
		if len(tail) > need {
			tail = tail[:need]
		}
		valid := 0
		for valid < len(tail) && tail[valid]&0xC0 == 0x80 {
			valid++
		}
		if valid == len(tail) && i+1+need > len(data) {
			if len(data)-i == 1 {
				return fmt.Errorf("'utf-8' codec can't decode byte 0x%02x in position %d: unexpected end of data", b, i)
			}
			return fmt.Errorf("'utf-8' codec can't decode bytes in position %d-%d: unexpected end of data", i, len(data)-1)
		}
		return fmt.Errorf("'utf-8' codec can't decode byte 0x%02x in position %d: invalid continuation byte", b, i)
	}
	return nil
}

func isASCIIText(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// Moves the legacy custom_providers list into the providers mapping
func migrateTo12(r *migrationRun) error {
	cfg := r.read()
	list, ok := cfg["custom_providers"].([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	providers := dictAt(cfg, "providers")
	migrated := 0
	var added []string
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		oldName, _ := entry["name"].(string)
		var oldURL any = ""
		for _, field := range []string{"base_url", "url", "api"} {
			if v := entry[field]; PyTruthy(v) {
				oldURL = v
				break
			}
		}
		if !PyTruthy(oldURL) {
			continue
		}
		key := strings.NewReplacer(" ", "-", "(", "", ")", "").Replace(strings.ToLower(strings.TrimFunc(oldName, unicode.IsSpace)))
		key = strings.Trim(repeatedDashRe.ReplaceAllString(key, "-"), "-")
		if key == "" {
			key = fmt.Sprintf("endpoint-%d", migrated)
			if s, isStr := oldURL.(string); isStr {
				if host, err := pyURLHostname(s); err == nil {
					if host == "" {
						host = "endpoint"
					}
					key = strings.ReplaceAll(host, ".", "-")
				}
			}
		}
		base, suffix := key, migrated
		for {
			if _, taken := providers[key]; !taken {
				break
			}
			key = fmt.Sprintf("%s-%d", base, suffix)
			suffix++
		}
		newEntry := r.src.CustomProviderEntryToProviderConfig(entry, key)
		if newEntry == nil {
			continue
		}
		if oldName == "" {
			delete(newEntry, "name")
		}
		if apiKey, has := newEntry["api_key"]; has {
			if err := pyUnhashable(apiKey); err != nil {
				return err
			}
			if s, ok := apiKey.(string); ok && (s == "no-key" || s == "no-key-required" || s == "") {
				delete(newEntry, "api_key")
			}
		}
		providers[key] = newEntry
		added = append(added, key)
		migrated++
	}
	if migrated == 0 {
		return nil
	}
	cfg["providers"] = providers
	delete(cfg, "custom_providers")
	if err := r.src.Persist(r.h, cfg); err != nil {
		return err
	}
	r.say(fmt.Sprintf("  \u2713 Migrated %d custom provider(s) to providers: section", migrated))
	for _, key := range added {
		api := ""
		if entry, ok := providers[key].(map[string]any); ok {
			if v, has := entry["api"]; has {
				api = PyStr(v)
			}
		}
		r.say(fmt.Sprintf("    \u2192 %s: %s", key, api))
	}
	return nil
}

// Clears the dead LLM_MODEL and OPENAI_MODEL entries from the home env file
func migrateTo13(r *migrationRun) error {
	if r.configOnly {
		return nil
	}
	for _, dead := range []string{"LLM_MODEL", "OPENAI_MODEL"} {
		if r.src.GetEnvValue(dead) == "" {
			continue
		}
		if r.src.SaveEnvValue(dead, "") == nil {
			r.say(fmt.Sprintf("  \u2713 Cleared %s from .env (no longer used \u2014 config.yaml is source of truth)", dead))
		}
	}
	return nil
}

var localWhisperModels = map[string]bool{
	"tiny.en": true, "tiny": true, "base.en": true, "base": true, "small.en": true, "small": true,
	"medium.en": true, "medium": true, "large-v1": true, "large-v2": true, "large-v3": true,
	"large": true, "distil-large-v2": true, "distil-medium.en": true,
	"distil-small.en": true, "distil-large-v3": true, "distil-large-v3.5": true,
	"large-v3-turbo": true, "turbo": true,
}

// Moves a flat stt.model into the section of its provider, dropping cloud names for local whisper
func migrateTo14(r *migrationRun) error {
	rawSTT, ok := r.read()["stt"].(map[string]any)
	if !ok {
		return nil
	}
	legacy, has := rawSTT["model"]
	if !has {
		return nil
	}
	provider := "local"
	if v, present := rawSTT["provider"]; present {
		provider, _ = v.(string)
		if _, isStr := v.(string); !isStr {
			provider = "local"
		}
	}
	cfg := r.read()
	stt, ok := cfg["stt"].(map[string]any)
	if !ok {
		stt = map[string]any{}
	}
	delete(stt, "model")
	place := func(section string) {
		existing, present := rawSTT[section]
		if !present {
			existing = map[string]any{}
		}
		if m, isMap := existing.(map[string]any); isMap {
			if _, hasModel := m["model"]; hasModel {
				return
			}
		}
		target, isMap := stt[section].(map[string]any)
		if !isMap {
			target = map[string]any{}
			stt[section] = target
		}
		target["model"] = legacy
	}
	if provider == "local" || provider == "local_command" {
		if s, isStr := legacy.(string); isStr && localWhisperModels[s] {
			place("local")
		}
	} else {
		place(provider)
	}
	cfg["stt"] = stt
	return r.commit(cfg, "", "  \u2713 Migrated legacy stt.model to provider-specific config")
}

// Moves display.tool_progress_overrides into display.platforms tool_progress entries
func migrateTo16(r *migrationRun) error {
	cfg := r.read()
	display := dictAt(cfg, "display")
	overrides, ok := display["tool_progress_overrides"].(map[string]any)
	if !ok || len(overrides) == 0 {
		return nil
	}
	platforms := dictAt(display, "platforms")
	var pairs []string
	for _, plat := range MappingKeys(overrides) {
		mode := overrides[plat]
		target, isMap := platforms[plat].(map[string]any)
		if !isMap {
			target = map[string]any{}
			platforms[plat] = target
		}
		if _, set := target["tool_progress"]; !set {
			target["tool_progress"] = mode
		}
		pairs = append(pairs, plat+"="+PyStr(mode))
	}
	display["platforms"] = platforms
	cfg["display"] = display
	return r.commit(cfg, "display.platforms (migrated from tool_progress_overrides)",
		"  \u2713 Migrated tool_progress_overrides \u2192 display.platforms: "+strings.Join(pairs, ", "))
}

// Drops compression.summary keys, carrying explicit values to auxiliary.compression
func migrateTo17(r *migrationRun) error {
	cfg := r.read()
	comp, ok := cfg["compression"].(map[string]any)
	if !ok {
		if _, present := cfg["compression"]; present {
			return nil
		}
		comp = map[string]any{}
	}
	fields := []string{"model", "provider", "base_url"}
	legacy := map[string]any{}
	anySet := false
	for _, k := range fields {
		v, has := comp["summary_"+k]
		delete(comp, "summary_"+k)
		legacy[k] = v
		if has && v != nil {
			anySet = true
		}
	}
	var migrated []string
	for _, k := range fields {
		raw := legacy[k]
		val := ""
		if PyTruthy(raw) {
			val = strings.TrimFunc(PyStr(raw), unicode.IsSpace)
		}
		if val == "" || (k == "provider" && val == "auto") {
			continue
		}
		aux, isMap := cfg["auxiliary"].(map[string]any)
		if !isMap {
			aux = map[string]any{}
			cfg["auxiliary"] = aux
		}
		auxComp, isMap := aux["compression"].(map[string]any)
		if !isMap {
			auxComp = map[string]any{}
			aux["compression"] = auxComp
		}
		cur := auxComp[k]
		if !PyTruthy(cur) || (k == "provider" && pyEq(cur, "auto")) {
			auxComp[k] = val
			migrated = append(migrated, k+"="+PyStr(raw))
		}
	}
	if len(migrated) == 0 && !anySet {
		return nil
	}
	cfg["compression"] = comp
	message := "  \u2713 Removed unused compression.summary_* keys"
	if len(migrated) > 0 {
		message = "  \u2713 Migrated compression.summary_* \u2192 auxiliary.compression: " + strings.Join(migrated, ", ")
	}
	return r.commit(cfg, "", message)
}

// Names of plugins under the home plugins directory with a manifest, minus the disabled ones
func installedUserPlugins(h string, disabled []any) []any {
	entries, err := os.ReadDir(home.PluginsDir(h))
	if err != nil {
		return []any{}
	}
	found := []any{}
	for _, child := range entries {
		dir := filepath.Join(home.PluginsDir(h), child.Name())
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		manifestFile := filepath.Join(dir, "plugin.yaml")
		if _, err := os.Stat(manifestFile); err != nil {
			manifestFile = filepath.Join(dir, "plugin.yml")
		}
		if _, err := os.Stat(manifestFile); err != nil {
			continue
		}
		var manifest any = map[string]any{}
		if data, err := os.ReadFile(manifestFile); err == nil {
			var doc any
			if yaml.Unmarshal([]byte(strings.TrimPrefix(string(data), "\ufeff")), &doc) == nil && PyTruthy(doc) {
				manifest = normalizeMaps(doc)
			}
		}
		m, ok := manifest.(map[string]any)
		if !ok {
			return []any{}
		}
		var name any = child.Name()
		if v := m["name"]; PyTruthy(v) {
			name = v
		}
		if pyUnhashable(name) != nil {
			return []any{}
		}
		if !pyContains(disabled, name) {
			found = append(found, name)
		}
	}
	return found
}

// Makes plugins opt in, grandfathering installed user plugins that are not disabled
func migrateTo21(r *migrationRun) error {
	cfg := r.read()
	plugins := dictAt(cfg, "plugins")
	if _, set := plugins["enabled"]; set {
		return nil
	}
	disabled, _ := plugins["disabled"].([]any)
	if raw, present := plugins["disabled"]; present && PyTruthy(raw) {
		if _, isList := raw.([]any); isList {
			for _, v := range disabled {
				if err := pyUnhashable(v); err != nil {
					return err
				}
			}
		}
	}
	grandfathered := installedUserPlugins(r.h, disabled)
	plugins["enabled"] = grandfathered
	cfg["plugins"] = plugins
	message := "  \u2713 Plugins now opt-in: no existing plugins to grandfather. Use `icebath plugins enable <name>` to activate."
	if len(grandfathered) > 0 {
		message = fmt.Sprintf("  \u2713 Plugins now opt-in: grandfathered %d existing plugin(s) into plugins.enabled", len(grandfathered))
	}
	return r.commit(cfg, fmt.Sprintf("plugins.enabled (opt-in allow-list, %d grandfathered)", len(grandfathered)), message)
}

// Default key order of the curator section and the auxiliary curator task
var (
	curatorDefaultOrder    = []string{"enabled", "interval_hours", "min_idle_hours", "stale_after_days", "archive_after_days", "consolidate", "prune_builtins", "archive_ttl_days", "backup"}
	auxCuratorDefaultOrder = []string{"provider", "model", "base_url", "api_key", "timeout", "extra_body", "reasoning_effort"}
)

// Copies default keys the section lacks into it, returning the added keys in default order
func seedMissing(section, defaults map[string]any, order []string) []string {
	var added []string
	for _, k := range orderedKeys(defaults, order) {
		if _, has := section[k]; !has {
			section[k] = Clone(defaults[k])
			added = append(added, k)
		}
	}
	return added
}

// Seeds unset curator defaults into the config and creates the curator log directory
func migrateTo23(r *migrationRun) error {
	defaults := Defaults()
	if !r.configOnly {
		dir := filepath.Join(home.LogsDir(r.h), "curator")
		if err := os.MkdirAll(dir, 0o777); err != nil {
			r.res.Warnings = append(r.res.Warnings, fmt.Sprintf("Could not create %s: %s", dir, pyOSErrorString(err)))
		}
	}
	cfg := r.read()
	rawCurator := dictAt(cfg, "curator")
	addedCurator := seedMissing(rawCurator, AsMap(defaults["curator"]), curatorDefaultOrder)
	if len(addedCurator) > 0 {
		cfg["curator"] = rawCurator
	}
	rawAux := dictAt(cfg, "auxiliary")
	rawAuxCurator := dictAt(rawAux, "curator")
	addedAux := seedMissing(rawAuxCurator, AsMap(AsMap(defaults["auxiliary"])["curator"]), auxCuratorDefaultOrder)
	if len(addedAux) > 0 {
		rawAux["curator"] = rawAuxCurator
		cfg["auxiliary"] = rawAux
	}
	if len(addedCurator) == 0 && len(addedAux) == 0 {
		return nil
	}
	if err := r.src.Persist(r.h, cfg); err != nil {
		return err
	}
	for _, part := range []struct {
		label string
		added []string
	}{{"curator", addedCurator}, {"auxiliary.curator", addedAux}} {
		if len(part.added) == 0 {
			continue
		}
		r.res.ConfigAdded = append(r.res.ConfigAdded, fmt.Sprintf("%s (%d default key(s))", part.label, len(part.added)))
		title := part.label
		if title == "curator" {
			title = "Curator"
		}
		r.say(fmt.Sprintf("  \u2713 %s settings now available (%s) \u2014 edit via `icebath config set`", title, strings.Join(part.added, ", ")))
	}
	return nil
}

// Turns the memory and skills write_mode tri state into the write_approval boolean
func migrateTo29(r *migrationRun) error {
	cfg := r.read()
	touched := false
	for _, subsystem := range []string{"memory", "skills"} {
		sub, ok := cfg[subsystem].(map[string]any)
		if !ok {
			continue
		}
		old, has := sub["write_mode"]
		if !has {
			continue
		}
		delete(sub, "write_mode")
		approve := false
		if s, isStr := old.(string); isStr {
			approve = strings.ToLower(strings.TrimFunc(s, unicode.IsSpace)) == "approve"
		}
		sub["write_approval"] = approve
		cfg[subsystem] = sub
		touched = true
		r.res.ConfigAdded = append(r.res.ConfigAdded, fmt.Sprintf("%s.write_mode \u2192 write_approval=%s", subsystem, PyStr(approve)))
	}
	if !touched {
		return nil
	}
	return r.commit(cfg, "", "  \u2713 Renamed write_mode \u2192 write_approval (boolean gate)")
}

// Folds a raised delegation.max_async_children into max_concurrent_children and drops it
func migrateTo33(r *migrationRun) error {
	cfg := r.read()
	deleg, ok := cfg["delegation"].(map[string]any)
	if !ok {
		return nil
	}
	oldAsync, has := deleg["max_async_children"]
	if !has {
		return nil
	}
	delete(deleg, "max_async_children")
	old, valid, err := pyIntOf(oldAsync)
	if err != nil {
		return err
	}
	if valid && old > 3 {
		cur := 3
		if v, present := deleg["max_concurrent_children"]; present {
			n, ok, err := pyIntOf(v)
			if err != nil {
				return err
			}
			if ok {
				cur = n
			}
		}
		if old > cur {
			deleg["max_concurrent_children"] = old
			r.res.ConfigAdded = append(r.res.ConfigAdded, fmt.Sprintf("delegation.max_concurrent_children=%d (folded from deprecated max_async_children)", old))
		}
	}
	cfg["delegation"] = deleg
	return r.commit(cfg, "", "  \u2713 Removed deprecated delegation.max_async_children \u2014 delegation.max_concurrent_children now caps background delegations too.")
}

// Resets display.personality and scrubs a rendered personality from agent.system_prompt once
func migrateTo34(r *migrationRun) error {
	p := r.src.Personality
	cfg := r.read()
	touched := false
	oldName := ""
	if display, ok := cfg["display"].(map[string]any); ok {
		var current any = ""
		if v, has := display["personality"]; has {
			current = v
		}
		if oldName = p.Normalize(current); oldName != "" {
			display["personality"] = ""
			cfg["display"] = display
			touched = true
		}
	}
	scrubbed := false
	if agent, ok := cfg["agent"].(map[string]any); ok {
		var current any = ""
		if v, has := agent["system_prompt"]; has {
			current = v
		}
		if manual := p.PromptText(current); manual != "" {
			for _, def := range p.Available(cfg) {
				if p.Render(def) == manual {
					agent["system_prompt"] = ""
					cfg["agent"] = agent
					touched = true
					scrubbed = true
					break
				}
			}
		}
	}
	if !touched {
		return nil
	}
	if err := r.commit(cfg, "display.personality=none (one-time reset)", ""); err != nil {
		return err
	}
	if oldName != "" {
		r.say(fmt.Sprintf("  \u2713 Personality reset to none (was '%s'). Personality state was previously saved inconsistently "+
			"across surfaces and could re-enable a personality you had turned off. Run /personality %s to turn it back on.", oldName, oldName))
	}
	if scrubbed {
		r.say("  \u2713 Removed personality text from agent.system_prompt (written by an older /personality). That field is now " +
			"reserved for manual system prompts; personalities live in display.personality.")
	}
	return nil
}

// Drops the removed bundled relay plugin from plugins.enabled with a warning
func migrateTo38(r *migrationRun) error {
	cfg := r.read()
	plugins, ok := cfg["plugins"].(map[string]any)
	if !ok {
		return nil
	}
	enabled := plugins["enabled"]
	removed := r.src.LegacyRelayPluginKeys(enabled)
	list, isList := enabled.([]any)
	if len(removed) == 0 || !isList {
		return nil
	}
	gone := map[string]bool{}
	for _, k := range removed {
		gone[k] = true
	}
	kept := []any{}
	for _, v := range list {
		if s, isStr := v.(string); !isStr || !gone[s] {
			kept = append(kept, v)
		}
	}
	plugins["enabled"] = kept
	cfg["plugins"] = plugins
	if err := r.src.Persist(r.h, cfg); err != nil {
		return err
	}
	message := "Removed legacy Relay plugin from plugins.enabled: " + strings.Join(removed, ", ") +
		". Configure a standard user or system Relay plugins.toml, or use ICEBATH_NEMO_RELAY_PLUGINS_TOML for an explicit user-file override."
	r.res.Warnings = append(r.res.Warnings, message)
	r.say("  \u26a0 " + message)
	return nil
}

// Strips the retired bfl toolset from saved toolset lists
func migrateTo39(r *migrationRun) error {
	cfg := r.read()
	changed := false
	for _, section := range []string{"platform_toolsets", "known_builtin_toolsets"} {
		mapping, ok := cfg[section].(map[string]any)
		if !ok {
			continue
		}
		for platform, toolsets := range mapping {
			list, isList := toolsets.([]any)
			if !isList || !pyContains(list, "bfl") {
				continue
			}
			kept := []any{}
			for _, ts := range list {
				if !pyEq(ts, "bfl") {
					kept = append(kept, ts)
				}
			}
			mapping[platform] = kept
			changed = true
		}
		if changed {
			cfg[section] = mapping
		}
	}
	if !changed {
		return nil
	}
	return r.commit(cfg, "removed retired 'bfl' toolset from saved toolset lists",
		"  \u2713 Removed the retired BFL FLUX 3 toolset from saved toolset lists \u2014 video generation now lives under `icebath tools` \u2192 Video Generation (Nous Subscription or FAL).")
}

// Removes the plugin era messaging section from every roster profile SOUL.md
func migrateTo41(r *migrationRun) error {
	if r.configOnly {
		return nil
	}
	var cleaned []string
	for _, profile := range r.src.BotRoster(r.h) {
		soul := filepath.Join(profile.Dir, "SOUL.md")
		text := ""
		if info, err := os.Stat(soul); err == nil && info.Mode().IsRegular() {
			data, err := os.ReadFile(soul)
			if err != nil {
				continue
			}
			text = strings.TrimPrefix(string(data), "\ufeff")
			if err := pyUTF8DecodeError([]byte(text)); err != nil {
				return err
			}
		}
		if !strings.Contains(text, r.src.BotProtocolHeading) {
			continue
		}
		if os.WriteFile(soul, []byte(r.src.StripLegacyBotProtocol(text)), 0o666) != nil {
			continue
		}
		cleaned = append(cleaned, profile.Name)
	}
	if len(cleaned) == 0 {
		return nil
	}
	names := strings.Join(cleaned, ", ")
	r.res.ConfigAdded = append(r.res.ConfigAdded, "removed legacy Bot Mode section from SOUL.md ("+names+")")
	r.say("  \u2713 Removed the plugin-era 'Messaging other agents' section from SOUL.md (" + names + ") \u2014 Bot Chat sessions now get the live roster instead.")
	return nil
}

// Sorted string forms of a toolset list with connections added
func withConnections(list []any) []any {
	set := map[string]bool{"connections": true}
	for _, v := range list {
		set[PyStr(v)] = true
	}
	out := make([]any, 0, len(set))
	for _, k := range sortedSetKeys(set) {
		out = append(out, k)
	}
	return out
}

func sortedSetKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Appends connections to explicit saved platform toolset lists that predate it
func migrateTo45(r *migrationRun) error {
	cfg := r.read()
	saved, ok := cfg["platform_toolsets"].(map[string]any)
	if !ok {
		return nil
	}
	for _, name := range r.src.ParseConfigStringList(dictAt(cfg, "agent")["disabled_toolsets"]) {
		if name == "connections" {
			return nil
		}
	}
	known := dictAt(cfg, "known_builtin_toolsets")
	explicit := r.src.ExplicitToolsetKeys()
	var enabledFor []string
	for platform, toolsets := range saved {
		list, isList := toolsets.([]any)
		if !isList || pyContains(list, "connections") || !r.src.ToolsetAllowedForPlatform("connections", platform) {
			continue
		}
		hasExplicit := false
		for _, ts := range list {
			if explicit[PyStr(ts)] {
				hasExplicit = true
				break
			}
		}
		if !hasExplicit {
			continue
		}
		offered, offeredList := known[platform].([]any)
		if offeredList && pyContains(offered, "connections") {
			continue
		}
		saved[platform] = withConnections(list)
		if offeredList {
			known[platform] = withConnections(offered)
		}
		enabledFor = append(enabledFor, platform)
	}
	if len(enabledFor) == 0 {
		return nil
	}
	cfg["platform_toolsets"] = saved
	if len(known) > 0 {
		cfg["known_builtin_toolsets"] = known
	}
	sort.Strings(enabledFor)
	platforms := strings.Join(enabledFor, ", ")
	return r.commit(cfg, "enabled the connections toolset for "+platforms,
		"  \u2713 Enabled the Connections toolset (Gmail, Linear, Notion, local MCP servers) for "+platforms+". Uncheck Connections in `icebath tools` to turn it off.")
}

// Carries the editor disabled flag on MCP servers over to enabled false
func migrateTo46(r *migrationRun) error {
	cfg := r.read()
	servers, ok := cfg["mcp_servers"].(map[string]any)
	if !ok {
		return nil
	}
	var turnedOff []string
	for name, raw := range servers {
		entry, isMap := raw.(map[string]any)
		if !isMap {
			continue
		}
		if flag, has := entry["disabled"]; has && r.src.ParseEnabledFlag(flag, false) {
			turnedOff = append(turnedOff, name)
		}
	}
	if len(turnedOff) == 0 {
		return nil
	}
	sort.Strings(turnedOff)
	for _, name := range turnedOff {
		entry := servers[name].(map[string]any)
		delete(entry, "disabled")
		entry["enabled"] = false
	}
	names := strings.Join(turnedOff, ", ")
	return r.commit(cfg, "mcp_servers: disabled \u2192 enabled: false ("+names+")",
		"  \u2713 Turned off MCP servers the profile editor had marked disabled: "+names+".")
}

// Drops saved sandbox images that still equal an old default so they follow the new one
func migrateTo48(r *migrationRun) error {
	for _, legacy := range LegacySandboxImages {
		for _, pair := range [][2]string{{"docker_image", legacy}, {"modal_image", legacy}, {"daytona_image", legacy}, {"singularity_image", "docker://" + legacy}} {
			key := pair[0]
			err := rewriteStaleDefault(pair[1], rewriteSpec{
				section: "terminal", key: key, remove: true,
				added:   fmt.Sprintf("terminal.%s unset (follows the default, %s)", key, DefaultSandboxImage),
				message: fmt.Sprintf("  \u2713 terminal.%s: was the old default; now follows the default sandbox image (%s)", key, DefaultSandboxImage),
			})(r)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Drops the seeded node24 Vercel runtime from config and its env mirror
func migrateTo49(r *migrationRun) error {
	err := rewriteStaleDefault(LegacyVercelRuntime, rewriteSpec{
		section: "terminal", key: "vercel_runtime", remove: true,
		added:   fmt.Sprintf("terminal.vercel_runtime unset (fresh sandboxes use terminal.vercel_image, %s)", DefaultVercelImage),
		message: fmt.Sprintf("  \u2713 terminal.vercel_runtime: was the old default; fresh sandboxes now use the managed image (%s)", DefaultVercelImage),
	})(r)
	if err != nil || r.configOnly {
		return err
	}
	if strings.TrimFunc(r.src.GetEnvValuePreferDotenv("TERMINAL_VERCEL_RUNTIME"), unicode.IsSpace) == LegacyVercelRuntime {
		if _, err := r.src.RemoveEnvValue("TERMINAL_VERCEL_RUNTIME"); err != nil {
			return err
		}
		r.say("  \u2713 Cleared TERMINAL_VERCEL_RUNTIME from .env (was the old default; the image is used instead)")
	}
	return nil
}

var retiredTirithKeys = []string{"tirith_enabled", "tirith_path", "tirith_timeout", "tirith_fail_open"}

// Drops the security tirith keys the removed scanner read
func migrateTo50(r *migrationRun) error {
	cfg := r.read()
	security, ok := cfg["security"].(map[string]any)
	if !ok {
		return nil
	}
	present := false
	for _, key := range retiredTirithKeys {
		if _, has := security[key]; has {
			delete(security, key)
			present = true
		}
	}
	if !present {
		return nil
	}
	return r.commit(cfg, "removed security.tirith_* (scanner no longer bundled)",
		"  \u2713 Removed security.tirith_* \u2014 the tirith scanner is no longer bundled with icebath.")
}

func always(any, bool) bool { return true }

// Ordered ladder of migration steps by target version
func migrationSteps() []migrationStep {
	return []migrationStep{
		{12, migrateTo12},
		{13, migrateTo13},
		{14, migrateTo14},
		{16, migrateTo16},
		{17, migrateTo17},
		{21, migrateTo21},
		{23, migrateTo23},
		{25, rewriteStaleDefault(24, rewriteSpec{section: "model_catalog", key: "ttl_hours", newValue: 1,
			added: "model_catalog.ttl_hours 24\u21921", message: "  \u2713 Lowered model_catalog.ttl_hours to 1 (hourly picker refresh)"})},
		{29, migrateTo29},
		{31, rewriteKey(rewriteSpec{section: "agent", key: "verify_on_stop", newValue: false, createSection: true,
			match: func(cur any, present bool) bool { return cur == nil || lowerIs("auto")(cur, present) },
			added: "agent.verify_on_stop=false",
			message: "  \u2713 Turned off verify-on-stop (agent.verify_on_stop: false). Set it to true to re-enable, or \"auto\" for the legacy " +
				"surface-aware behavior."})},
		{32, rewriteStaleDefault(true, rewriteSpec{section: "agent", key: "verify_on_stop", newValue: false,
			added: "agent.verify_on_stop=false",
			message: "  \u2713 Turned off verify-on-stop (agent.verify_on_stop: false) \u2014 the old default was written into your config as a literal " +
				"true. Set it to true again to re-enable, or \"auto\" for the legacy surface-aware behavior.",
			extraGuard: func(raw map[string]any) bool { return raw["verify_on_stop"] == true }})},
		{33, migrateTo33},
		{34, migrateTo34},
		{35, rewriteKey(rewriteSpec{section: "display", key: "background_process_notifications", newValue: "concise", match: lowerIs("all"),
			added: "display.background_process_notifications=concise (was: all)",
			message: "  \u2713 Background process notifications switched from 'all' to 'concise' \u2014 completions now show a one-line status message " +
				"instead of the raw output dump. Set display.background_process_notifications: all to restore the old behavior."})},
		{36, rewriteStaleDefault(50, rewriteSpec{section: "delegation", key: "max_iterations", newValue: 250,
			added: "delegation.max_iterations=250 (was: 50)",
			message: "  \u2713 Raised delegation.max_iterations from 50 to 250 \u2014 subagents now get a larger per-child tool-call budget so delegated " +
				"work finishes instead of truncating. Set delegation.max_iterations back to 50 to restore the old cap."})},
		{37, rewriteStaleDefault(3, rewriteSpec{section: "delegation", key: "max_concurrent_children", newValue: 10,
			added: "delegation.max_concurrent_children=10 (was: 3)",
			message: "  \u2713 Raised delegation.max_concurrent_children from 3 to 10 \u2014 independent delegated children now fan out wider in parallel. " +
				"Each child consumes API tokens independently; set delegation.max_concurrent_children back to 3 to restore the old cap."})},
		{38, migrateTo38},
		{39, migrateTo39},
		{40, rewriteStaleDefault(1, rewriteSpec{section: "model_catalog", key: "ttl_hours", remove: true,
			added:      "model_catalog.ttl_hours 1 \u2192 ttl_minutes 20 (default)",
			message:    "  \u2713 Model catalog now refreshes every 20 minutes (model_catalog.ttl_minutes)",
			extraGuard: func(raw map[string]any) bool { _, has := raw["ttl_minutes"]; return !has }})},
		{41, migrateTo41},
		{42, rewriteKey(rewriteSpec{section: "cron", key: "model_drift_guard", remove: true,
			match: func(cur any, _ bool) bool { return cur != nil },
			added: "removed cron.model_drift_guard",
			message: "  \u2713 Removed cron.model_drift_guard \u2014 unpinned cron jobs now keep running on the model/provider they were created under " +
				"when the global default changes, instead of being skipped. Pin a job or set cron.model to move it."})},
		{43, rewriteKey(rewriteSpec{section: "gateway", key: "multiplex_profile_allowlist", remove: true, match: always,
			added: "removed gateway.multiplex_profile_allowlist",
			message: "  \u2713 Removed gateway.multiplex_profile_allowlist \u2014 the multiplexing gateway now serves every profile under profiles/. " +
				"Delete or archive a profile you do not want served.",
			extraGuard: func(raw map[string]any) bool { _, has := raw["multiplex_profile_allowlist"]; return has }})},
		{44, rewriteStaleDefault(30, rewriteSpec{section: "curator", key: "stale_after_days", newValue: 14,
			added:   "curator.stale_after_days=14 (was: 30)",
			message: "  \u2713 curator.stale_after_days 30\u219214 \u2014 unused skills are flagged stale after two weeks."})},
		{44, rewriteStaleDefault(90, rewriteSpec{section: "curator", key: "archive_after_days", newValue: 30,
			added: "curator.archive_after_days=30 (was: 90)",
			message: "  \u2713 curator.archive_after_days 90\u219230 \u2014 skills unused for a month are archived to skills/.archive/ (recoverable with " +
				"`icebath curator restore`). Set it back to 90 to keep the old window."})},
		{45, migrateTo45},
		{46, migrateTo46},
		{47, rewriteStaleDefault(256000, rewriteSpec{section: "compression", key: "threshold_tokens", remove: true,
			added: "removed compression.threshold_tokens: 256000 (the old default)",
			message: "  \u2713 Removed compression.threshold_tokens: 256000 \u2014 the old default. Compaction follows compression.threshold (50% of the " +
				"window) again. Set threshold_tokens to a token count to cap it on purpose."})},
		{48, migrateTo48},
		{49, migrateTo49},
		{50, migrateTo50},
		{51, rewriteKey(rewriteSpec{section: "cron", key: "bot_chat_delivery_timeout_seconds", remove: true, match: always,
			added: "removed cron.bot_chat_delivery_timeout_seconds",
			message: "  \u2713 Removed cron.bot_chat_delivery_timeout_seconds \u2014 bot-chat deliveries are now admitted to the target profile's running " +
				"gateway and tracked by receipt, so cron no longer runs (or times out) a Bot Chat turn of its own.",
			extraGuard: func(raw map[string]any) bool { _, has := raw["bot_chat_delivery_timeout_seconds"]; return has }})},
		{52, migrateTo52},
	}
}

// Ladder a pass runs, replaced in tests to inject a failing step
var migrationLadder = migrationSteps

// Target versions of the ladder in run order
func MigrationVersions() []int {
	var out []int
	for _, s := range migrationSteps() {
		out = append(out, s.target)
	}
	return out
}

// Steps keyed by a legacy key, the only ones an unversioned config gets
var LegacyKeySteps = map[int]bool{12: true, 14: true, 16: true, 17: true, 29: true, 33: true, 38: true, 39: true, 42: true, 43: true, 46: true, 50: true, 51: true, 52: true}

// Legacy key steps every pass runs whatever the stamp says
var StampIndependentSteps = map[int]bool{46: true, 50: true}

// Runs one step, turning a panic from a malformed value into its error
func runStep(step migrationStep, r *migrationRun) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%v", p)
		}
	}()
	return step.run(r)
}

// Applies every step newer than the on disk version plus the stamp independent ones, warning on a failed step
func RunMigrations(h string, currentVer int, results *MigrationResults, quiet bool, opts MigrationOptions, src MigrationSources) {
	r := &migrationRun{h: h, res: results, quiet: quiet, src: src, configOnly: opts.ConfigOnly}
	for _, step := range migrationLadder() {
		due := currentVer < step.target || StampIndependentSteps[step.target]
		if !due || (!LegacyKeySteps[step.target] && opts.Unversioned) {
			continue
		}
		if err := runStep(step, r); err != nil {
			warning := fmt.Sprintf("config migration to v%d failed and was skipped: %v", step.target, err)
			results.Warnings = append(results.Warnings, warning)
			slog.Warn(warning)
			r.say("  \u26a0 " + warning)
		}
	}
}
