package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
)

const (
	// MoAMarkerPrefix starts a hidden one shot MoA turn message
	MoAMarkerPrefix = "__ICEBATH_MOA_TURN_V1__"
	// DefaultMoAPresetName names the preset a legacy flat MoA config becomes
	DefaultMoAPresetName = "default"
)

var (
	defaultMoAReferenceModels = []map[string]any{
		{"provider": "openai-codex", "model": "gpt-5.5"},
		{"provider": "openrouter", "model": "deepseek/deepseek-v4-pro"},
	}
	defaultMoAAggregator = map[string]any{"provider": "openrouter", "model": "anthropic/claude-opus-4.8"}
	// Preset fields mirrored at the top level of the normalized config
	moaFlatPresetKeys = []string{"reference_models", "aggregator", "reference_temperature", "aggregator_temperature",
		"reference_timeout", "degraded_reference_policy", "fanout", "enabled"}
	moaAggregatorSlotFields = map[string]bool{"provider": true, "model": true, "reasoning_effort": true}
)

// Default reference slots each marked enabled
func defaultMoAReferenceSlots() []any {
	out := make([]any, 0, len(defaultMoAReferenceModels))
	for _, slot := range defaultMoAReferenceModels {
		out = append(out, map[string]any{"provider": slot["provider"], "model": slot["model"], "enabled": true})
	}
	return out
}

// Copy of the default aggregator slot
func defaultMoAAggregatorSlot() map[string]any {
	return map[string]any{"provider": defaultMoAAggregator["provider"], "model": defaultMoAAggregator["model"]}
}

// Decimal value of a Unicode decimal digit rune, -1 for anything else
func moaDigitValue(r rune) int {
	if r >= '0' && r <= '9' {
		return int(r - '0')
	}
	if !unicode.IsDigit(r) {
		return -1
	}
	for _, rg := range unicode.Nd.R16 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10
		}
	}
	for _, rg := range unicode.Nd.R32 {
		if r >= rune(rg.Lo) && r <= rune(rg.Hi) {
			return int(r-rune(rg.Lo)) % 10
		}
	}
	return -1
}

// ASCII form of numeric text with Unicode digits mapped and single underscores between digits removed, false when misplaced
func moaNumericASCII(s string) (string, bool) {
	runes := []rune(s)
	var b strings.Builder
	for i, r := range runes {
		if r == '_' {
			if i == 0 || i == len(runes)-1 || moaDigitValue(runes[i-1]) < 0 || moaDigitValue(runes[i+1]) < 0 {
				return "", false
			}
			continue
		}
		if d := moaDigitValue(r); d >= 0 {
			b.WriteByte(byte('0' + d))
			continue
		}
		if r > unicode.MaxASCII {
			return "", false
		}
		b.WriteRune(r)
	}
	return b.String(), true
}

// Python float of a string, false where float raises
func moaPyFloatText(s string) (float64, bool) {
	text, ok := moaNumericASCII(pyStripSpace(s))
	if !ok || text == "" || strings.ContainsAny(text, "xXpP") {
		return 0, false
	}
	body := strings.ToLower(strings.TrimLeft(text, "+-"))
	if len(text)-len(body) > 1 {
		return 0, false
	}
	switch body {
	case "nan":
		return math.NaN(), true
	case "inf", "infinity":
		if strings.HasPrefix(text, "-") {
			return math.Inf(-1), true
		}
		return math.Inf(1), true
	}
	for _, r := range body {
		if !strings.ContainsRune("0123456789.e+-", r) {
			return 0, false
		}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		if ne, isNum := err.(*strconv.NumError); isNum && ne.Err == strconv.ErrRange {
			return f, true
		}
		return 0, false
	}
	return f, true
}

// Python int of a string in base ten, false where int raises
func moaPyIntText(s string) (*big.Int, bool) {
	text, ok := moaNumericASCII(pyStripSpace(s))
	if !ok {
		return nil, false
	}
	digits := strings.TrimPrefix(strings.TrimPrefix(text, "+"), "-")
	if len(text)-len(digits) > 1 || digits == "" || strings.Trim(digits, "0123456789") != "" {
		return nil, false
	}
	n, ok := new(big.Int).SetString(digits, 10)
	if ok && strings.HasPrefix(text, "-") {
		n.Neg(n)
	}
	return n, ok
}

// Python float of a value, false where float raises
func moaPyFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float64:
		return v, true
	case json.Number:
		return moaPyFloatText(v.String())
	case string:
		return moaPyFloatText(v)
	}
	return 0, false
}

// Python int of a value, a float string falling back through float like int(float(value)), false where both raise
func moaPyInt(value any) (*big.Int, bool) {
	switch v := value.(type) {
	case bool:
		if v {
			return big.NewInt(1), true
		}
		return big.NewInt(0), true
	case int:
		return big.NewInt(int64(v)), true
	case int64:
		return big.NewInt(v), true
	case uint64:
		return new(big.Int).SetUint64(v), true
	case string:
		if n, ok := moaPyIntText(v); ok {
			return n, true
		}
	case json.Number:
		if n, ok := moaPyIntText(v.String()); ok {
			return n, true
		}
	}
	f, ok := moaPyFloat(value)
	if !ok || math.IsInf(f, 0) || math.IsNaN(f) {
		return nil, false
	}
	n, _ := big.NewFloat(f).Int(nil)
	return n, true
}

// Finite float of a value, nil when unset, blank, invalid or not positive where required
func moaCoerceFloat(value any, positive bool) any {
	if value == nil || value == "" {
		return nil
	}
	f, ok := moaPyFloat(value)
	if !ok || math.IsInf(f, 0) || math.IsNaN(f) || (positive && f <= 0) {
		return nil
	}
	return f
}

// Integer of a value, zero when unset, blank or invalid
func moaCoerceInt(value any) *big.Int {
	if value == nil || value == "" {
		return big.NewInt(0)
	}
	n, ok := moaPyInt(value)
	if !ok {
		return big.NewInt(0)
	}
	return n
}

// Finite positive advisor timeout, nil to inherit the auxiliary reference timeout
func coerceMoAReferenceTimeout(value any) any {
	if _, isBool := value.(bool); isBool {
		return nil
	}
	return moaCoerceFloat(value, true)
}

// Fanout cadence normalized to per_iteration, user_turn or every_n with N of at least two
func coerceMoAFanout(value any) string {
	everyN := func(n *big.Int) string {
		switch {
		case n.Cmp(big.NewInt(2)) >= 0:
			return "every_n:" + n.String()
		case n.Cmp(big.NewInt(1)) == 0:
			return "per_iteration"
		}
		return "user_turn"
	}
	if m, ok := value.(map[string]any); ok {
		mode := strings.ToLower(pyStripSpace(pyOrStr(m["mode"])))
		if mode == "every_n" {
			return everyN(moaCoerceInt(m["n"]))
		}
		value = mode
	}
	text, ok := value.(string)
	if !ok {
		return "user_turn"
	}
	mode := strings.ToLower(pyStripSpace(text))
	if mode == "per_iteration" || mode == "user_turn" {
		return mode
	}
	if strings.HasPrefix(mode, "every_n") {
		if _, rest, sep := strings.Cut(mode, ":"); sep {
			return everyN(moaCoerceInt(pyStripSpace(rest)))
		}
		return everyN(big.NewInt(0))
	}
	return "user_turn"
}

// CoercePrivacyFilter normalizes the MoA privacy filter to off, display or full
func CoercePrivacyFilter(value any) string {
	if value == true {
		return "full"
	}
	if value == nil || value == false {
		return ""
	}
	mode := strings.ToLower(pyStripSpace(pyStr(value)))
	switch mode {
	case "display", "full":
		return mode
	case "true", "on", "yes", "1":
		return "full"
	}
	return ""
}

// Reasoning effort config of a level, nil for empty or unrecognized input and enabled false for a disable spelling
func ParseReasoningEffort(effort any) map[string]any { return config.ParseReasoningEffort(effort) }

// Canonical per slot reasoning effort, empty when unset or invalid
func cleanMoAReasoningEffort(value any) string {
	if value == nil || value == true {
		return ""
	}
	parsed := ParseReasoningEffort(value)
	if parsed == nil {
		return ""
	}
	if parsed["enabled"] == false {
		return "none"
	}
	effort, _ := parsed["effort"].(string)
	return effort
}

// Boolean of a config value with yes and no spellings, the default when unset or unrecognized
func coerceMoABool(value any, def bool) bool {
	switch v := value.(type) {
	case nil:
		return def
	case bool:
		return v
	case string:
		switch strings.ToLower(pyStripSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		return def
	}
	return config.PyTruthy(value)
}

// Problem a slot would be dropped for, empty when complete and valid
func moaSlotProblem(slot any) string {
	m, ok := slot.(map[string]any)
	if !ok {
		return "must be an object with 'provider' and 'model'"
	}
	providerName := pyStripSpace(pyOrStr(m["provider"]))
	model := pyStripSpace(pyOrStr(m["model"]))
	switch {
	case providerName == "" && model == "":
		return "provider and model are required"
	case providerName == "":
		return "provider is required"
	case model == "":
		return fmt.Sprintf("model is required (provider '%s' has no model selected)", providerName)
	case strings.ToLower(providerName) == "moa":
		return "the Mixture of Agents provider cannot be used inside a preset (recursive MoA)"
	}
	return ""
}

// Canonical slot with provider, model and effort, nil for any slot with a problem
func cleanMoASlot(slot any, includeEnabled bool) map[string]any {
	if moaSlotProblem(slot) != "" {
		return nil
	}
	m := slot.(map[string]any)
	clean := map[string]any{"provider": pyStripSpace(pyStr(m["provider"])), "model": pyStripSpace(pyStr(m["model"]))}
	if effort := cleanMoAReasoningEffort(m["reasoning_effort"]); effort != "" {
		clean["reasoning_effort"] = effort
	}
	if includeEnabled {
		clean["enabled"] = coerceMoABool(m["enabled"], true)
	}
	return clean
}

// Sentinel strings standing in for the NaN and infinity literals Python json accepts
const (
	moaJSONNaN    = "\x00icebath-json-NaN"
	moaJSONInf    = "\x00icebath-json-Infinity"
	moaJSONNegInf = "\x00icebath-json--Infinity"
)

// Python json.loads of text including NaN and Infinity literals, integers kept exact
func moaJSONLoads(text string) (any, bool) {
	var src bytes.Buffer
	inString, escaped := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			src.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			src.WriteByte(c)
		case strings.HasPrefix(text[i:], "NaN"):
			src.Write(moaJSONSentinel(moaJSONNaN))
			i += len("NaN") - 1
		case strings.HasPrefix(text[i:], "-Infinity"):
			src.Write(moaJSONSentinel(moaJSONNegInf))
			i += len("-Infinity") - 1
		case strings.HasPrefix(text[i:], "Infinity"):
			src.Write(moaJSONSentinel(moaJSONInf))
			i += len("Infinity") - 1
		default:
			src.WriteByte(c)
		}
	}
	if !utf8.Valid(src.Bytes()) {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(src.Bytes()))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err == nil {
		return nil, false
	}
	return moaJSONRestore(out), true
}

// Sentinels back to floats, integers to int where they fit and other numbers to float
func moaJSONRestore(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, item := range t {
			t[k] = moaJSONRestore(item)
		}
		return t
	case []any:
		for i, item := range t {
			t[i] = moaJSONRestore(item)
		}
		return t
	case string:
		switch t {
		case moaJSONNaN:
			return math.NaN()
		case moaJSONInf:
			return math.Inf(1)
		case moaJSONNegInf:
			return math.Inf(-1)
		}
		return t
	case json.Number:
		text := t.String()
		if !strings.ContainsAny(text, ".eE") {
			if n, err := strconv.ParseInt(text, 10, 64); err == nil {
				return int(n)
			}
			return t
		}
		f, _ := strconv.ParseFloat(text, 64)
		return f
	}
	return v
}

// Reference models as a list, a JSON string decoded and a single mapping wrapped
func moaReferenceSlots(raw any) []any {
	if s, ok := raw.(string); ok {
		decoded, ok := moaJSONLoads(s)
		if !ok {
			decoded = []any{}
		}
		raw = decoded
	}
	if list, ok := raw.([]any); ok {
		return list
	}
	if m, ok := raw.(map[string]any); ok {
		return []any{m}
	}
	return []any{}
}

// Preset names in config file order, the order Python iterates the preset dict
func moaPresetOrder(presets map[string]any) []string {
	return config.MappingKeys(presets)
}

// ValidateMoAPayload returns the problems normalizing would silently repair, empty when safe to save
func ValidateMoAPayload(raw any) []string {
	m, ok := raw.(map[string]any)
	if !ok {
		return []string{"MoA config must be an object"}
	}
	presets, ok := m["presets"].(map[string]any)
	if !ok || len(presets) == 0 {
		presets = map[string]any{DefaultMoAPresetName: m}
	}
	problems := []string{}
	for _, name := range moaPresetOrder(presets) {
		label := pyStripSpace(name)
		if label == "" {
			label = "(unnamed)"
		}
		preset, ok := presets[name].(map[string]any)
		if !ok {
			problems = append(problems, fmt.Sprintf("preset '%s': must be an object", label))
			continue
		}
		refs, isList := preset["reference_models"].([]any)
		if !isList {
			refs = []any{}
			if single, isMap := preset["reference_models"].(map[string]any); isMap {
				refs = []any{single}
			}
		}
		allBad := true
		for index, slot := range refs {
			if issue := moaSlotProblem(slot); issue != "" {
				problems = append(problems, fmt.Sprintf("preset '%s' reference %d: %s", label, index+1, issue))
			} else {
				allBad = false
			}
		}
		if allBad {
			problems = append(problems, fmt.Sprintf("preset '%s': needs at least one complete reference model", label))
		}
		if issue := moaSlotProblem(preset["aggregator"]); issue != "" {
			problems = append(problems, fmt.Sprintf("preset '%s' aggregator: %s", label, issue))
		}
	}
	return problems
}

// One preset with cleaned slots, defaults for missing slots and coerced numeric, policy and fanout fields
func normalizeMoAPreset(raw any) map[string]any {
	m, ok := raw.(map[string]any)
	if !ok {
		m = map[string]any{}
	}
	refs := []any{}
	for _, item := range moaReferenceSlots(m["reference_models"]) {
		if clean := cleanMoASlot(item, true); clean != nil {
			refs = append(refs, clean)
		}
	}
	if len(refs) == 0 {
		refs = defaultMoAReferenceSlots()
	}
	aggregator := cleanMoASlot(m["aggregator"], false)
	if aggregator == nil {
		aggregator = defaultMoAAggregatorSlot()
	}
	policy := "loud"
	if config.PyTruthy(m["degraded_reference_policy"]) {
		policy = pyStr(m["degraded_reference_policy"])
	}
	policy = strings.ToLower(pyStripSpace(policy))
	if policy != "loud" && policy != "silent" {
		policy = "loud"
	}
	return map[string]any{
		"enabled":                   coerceMoABool(m["enabled"], true),
		"reference_models":          refs,
		"aggregator":                aggregator,
		"reference_temperature":     moaCoerceFloat(m["reference_temperature"], false),
		"aggregator_temperature":    moaCoerceFloat(m["aggregator_temperature"], false),
		"reference_timeout":         coerceMoAReferenceTimeout(m["reference_timeout"]),
		"degraded_reference_policy": policy,
		"fanout":                    coerceMoAFanout(m["fanout"]),
	}
}

// IsPresetFieldPath reports whether split config path parts name a preset field or an aggregator slot field
func IsPresetFieldPath(parts []string) bool {
	if (len(parts) != 4 && len(parts) != 5) || parts[0] != "moa" || parts[1] != "presets" || !containsString(moaFlatPresetKeys, parts[3]) {
		return false
	}
	if parts[2] == "" || parts[2] != pyStripSpace(parts[2]) {
		return false
	}
	return len(parts) == 4 || (parts[3] == "aggregator" && moaAggregatorSlotFields[parts[4]])
}

// Validated MoA config with named presets, a default and active name and the flattened default preset view
func normalizeMoAConfig(raw any) map[string]any {
	m, ok := raw.(map[string]any)
	if !ok {
		m = map[string]any{}
	}
	presets := map[string]any{}
	var order []string
	if rawPresets, ok := m["presets"].(map[string]any); ok {
		for _, name := range config.MappingKeys(rawPresets) {
			if clean := pyStripSpace(name); clean != "" {
				if _, seen := presets[clean]; !seen {
					order = append(order, clean)
				}
				presets[clean] = normalizeMoAPreset(rawPresets[name])
			}
		}
	}
	config.SetMappingKeys(presets, order)
	if len(presets) == 0 {
		presets[DefaultMoAPresetName] = normalizeMoAPreset(m)
	}
	defaultName := pyStripSpace(pyOrStr(m["default_preset"]))
	if _, has := presets[defaultName]; defaultName == "" || !has {
		defaultName = moaPresetOrder(presets)[0]
	}
	activeName := pyStripSpace(pyOrStr(m["active_preset"]))
	if _, has := presets[activeName]; !has {
		activeName = ""
	}
	out := map[string]any{"default_preset": defaultName, "active_preset": activeName, "presets": presets}
	defaultPreset := presets[defaultName].(map[string]any)
	for _, key := range moaFlatPresetKeys {
		out[key] = deepCopyAny(defaultPreset[key])
	}
	out["privacy_filter"] = CoercePrivacyFilter(m["privacy_filter"])
	return out
}

// Normalized moa config block with presets, default preset and settings
func NormalizeMoAConfig(raw any) map[string]any { return normalizeMoAConfig(raw) }

// Preset names of a MoA config section in listing order
func moaPresetNames(moa any) ([]string, error) {
	return moaPresetOrder(anyMap(normalizeMoAConfig(moa)["presets"])), nil
}

// MoA preset lookup failure carrying the actionable message, classified as a missing preset
type moaPresetMissingError struct {
	message string
	preset  string
}

func (e *moaPresetMissingError) Error() string { return e.message }

func (e *moaPresetMissingError) Unwrap() error { return &MoAPresetNotFoundError{Preset: e.preset} }

// ResolveMoAPreset returns a copy of the named preset, the configured default when name is empty
func ResolveMoAPreset(raw any, name string) (map[string]any, error) {
	cfg := normalizeMoAConfig(raw)
	presetName := name
	if presetName == "" {
		presetName = pyOrStr(cfg["default_preset"])
	}
	if presetName == "" {
		presetName = DefaultMoAPresetName
	}
	presetName = pyStripSpace(presetName)
	presets := anyMap(cfg["presets"])
	preset, ok := presets[presetName]
	if !ok {
		available := strings.Join(moaPresetOrder(presets), ", ")
		if available == "" {
			available = "(none)"
		}
		return nil, &moaPresetMissingError{preset: presetName, message: fmt.Sprintf(
			"MoA preset '%s' was not found. Available presets: %s. Run `icebath moa list`.", presetName, available)}
	}
	return deepCopyAny(preset).(map[string]any), nil
}

// Aggregator provider and model of a preset in a full config, false when the preset is missing
func resolveMoaAggregator(cfg map[string]any, preset string) (string, string, bool) {
	var moa any = map[string]any{}
	if config.PyTruthy(cfg["moa"]) {
		moa = cfg["moa"]
	}
	resolved, err := ResolveMoAPreset(moa, preset)
	if err != nil {
		return "", "", false
	}
	agg := anyMap(resolved["aggregator"])
	return pyStripSpace(pyOrStr(agg["provider"])), pyStripSpace(pyOrStr(agg["model"])), true
}

// SkipDeepMerge reports whether the missing config recursion must not descend into the named preset map
func SkipDeepMerge(prefix, key string) bool {
	return prefix == "moa" && key == "presets"
}

// ApplyUserMoAPresets makes an explicit user preset map authoritative over the merged defaults
func ApplyUserMoAPresets(merged map[string]any, raw any) {
	rawMap, ok := raw.(map[string]any)
	if !ok {
		return
	}
	rawMoA, ok := rawMap["moa"].(map[string]any)
	if !ok {
		return
	}
	rawPresets, has := rawMoA["presets"]
	if !has {
		return
	}
	presets, ok := rawPresets.(map[string]any)
	if !ok {
		return
	}
	mergedMoA, ok := merged["moa"].(map[string]any)
	if !ok {
		return
	}
	mergedMoA["presets"] = config.Clone(presets)
}

// ExactMoAPresetName returns the text when it names an enabled preset, empty otherwise
func ExactMoAPresetName(raw any, text string) string {
	wanted := pyStripSpace(text)
	if wanted == "" {
		return ""
	}
	preset, ok := anyMap(normalizeMoAConfig(raw)["presets"])[wanted].(map[string]any)
	if !ok {
		return ""
	}
	if enabled, has := preset["enabled"]; has && !config.PyTruthy(enabled) {
		return ""
	}
	return wanted
}

// Python non strict urlsafe base64 decoding that skips foreign characters and stops at complete padding
func moaURLSafeB64Decode(text string) ([]byte, bool) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out []byte
	quad, pads := 0, 0
	var left byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch c {
		case '-':
			c = '+'
		case '_':
			c = '/'
		}
		if c == '=' {
			if quad >= 2 {
				pads++
				if quad+pads >= 4 {
					return out, true
				}
			}
			continue
		}
		v := strings.IndexByte(alphabet, c)
		if v < 0 {
			continue
		}
		pads = 0
		b := byte(v)
		switch quad {
		case 0:
			left = b
		case 1:
			out = append(out, left<<2|b>>4)
			left = b & 0x0f
		case 2:
			out = append(out, left<<4|b>>2)
			left = b & 0x03
		case 3:
			out = append(out, left<<6|b)
		}
		quad = (quad + 1) % 4
	}
	return out, quad == 0
}

// DecodeMoATurn splits a hidden one shot MoA marker into its prompt and normalized preset, nil when not a marker
func DecodeMoATurn(message string) (string, map[string]any, error) {
	if !strings.HasPrefix(message, MoAMarkerPrefix) {
		return message, nil, nil
	}
	encoded := pyStripSpace(message[len(MoAMarkerPrefix):])
	for _, r := range encoded {
		if r > unicode.MaxASCII {
			return message, nil, nil
		}
	}
	raw, ok := moaURLSafeB64Decode(encoded)
	if !ok || !utf8.Valid(raw) {
		return message, nil, nil
	}
	decoded, ok := moaJSONLoads(string(raw))
	if !ok {
		return message, nil, nil
	}
	payload, ok := decoded.(map[string]any)
	if !ok {
		return "", nil, fmt.Errorf("'%s' object has no attribute 'get'", config.PyTypeName(decoded))
	}
	cfg := payload["config"]
	if !config.PyTruthy(cfg) {
		cfg = map[string]any{}
	}
	return pyOrStr(payload["prompt"]), normalizeMoAPreset(cfg), nil
}

// MoAUsage is the usage line of the moa slash command
func MoAUsage() string {
	return "Usage: /moa <prompt>  (runs one prompt through the default MoA preset, then restores your model; pick a preset from the model picker to switch for the session)"
}

// JSON string literal of a sentinel, the NUL escaped as JSON requires
func moaJSONSentinel(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}
