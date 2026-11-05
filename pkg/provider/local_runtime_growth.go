package provider

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
)

// Path of the machine scoped file holding each model's granted window
func WindowOverridesPath() string {
	return filepath.Join(RuntimesRoot(), "window_overrides.json")
}

type windowOverride struct {
	modelID string
	window  int64
}

// Granted windows in file order, nil on any read or coercion problem
func loadWindowOverridesOrdered() []windowOverride {
	raw, err := os.ReadFile(WindowOverridesPath())
	if err != nil {
		return nil
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if !utf8.Valid(raw) {
		return nil
	}
	v, ok := moaJSONLoads(string(raw))
	if !ok {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := []windowOverride{}
	for _, key := range jsonTopLevelKeys(raw) {
		n, err := growthPyInt(m[key])
		if err != nil {
			return nil
		}
		out = append(out, windowOverride{key, n})
	}
	return out
}

// Python int of a decoded JSON value, failing where Python raises
func growthPyInt(v any) (int64, error) {
	if num, ok := v.(json.Number); ok {
		text := num.String()
		if !strings.ContainsAny(text, ".eE") {
			return ggufParseIntString(text)
		}
		f, _ := strconv.ParseFloat(text, 64)
		return ggufInt(f)
	}
	return ggufInt(v)
}

// Top level object keys of valid JSON text in first appearance order
func jsonTopLevelKeys(raw []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(jsonSentinelSafe(raw)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	seen := map[string]bool{}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return keys
		}
		key, _ := tok.(string)
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return keys
		}
	}
	return keys
}

// JSON text with bare NaN and Infinity literals outside strings replaced by null
func jsonSentinelSafe(raw []byte) []byte {
	var out bytes.Buffer
	inString, escaped := false, false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			out.WriteByte(c)
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
			out.WriteByte(c)
		case bytes.HasPrefix(raw[i:], []byte("-Infinity")):
			out.WriteString("null")
			i += len("-Infinity") - 1
		case bytes.HasPrefix(raw[i:], []byte("Infinity")):
			out.WriteString("null")
			i += len("Infinity") - 1
		case bytes.HasPrefix(raw[i:], []byte("NaN")):
			out.WriteString("null")
			i += len("NaN") - 1
		default:
			out.WriteByte(c)
		}
	}
	return out.Bytes()
}

// Model id to granted window, empty on any read problem
func LoadWindowOverrides() map[string]int {
	out := map[string]int{}
	for _, o := range loadWindowOverridesOrdered() {
		out[o.modelID] = int(o.window)
	}
	return out
}

// Writes the overrides as Python json.dumps with indent one and ASCII escapes
func writeWindowOverrides(overrides []windowOverride) error {
	path := WindowOverridesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	text := "{}"
	if len(overrides) > 0 {
		lines := make([]string, len(overrides))
		for i, o := range overrides {
			lines[i] = " " + pyJSONQuote(o.modelID) + ": " + strconv.FormatInt(o.window, 10)
		}
		text = "{\n" + strings.Join(lines, ",\n") + "\n}"
	}
	return os.WriteFile(path, []byte(text), 0o644)
}

// Records a model's granted window, keeping every other model's grant
func SaveWindowOverride(modelID string, window int) error {
	overrides := loadWindowOverridesOrdered()
	found := false
	for i := range overrides {
		if overrides[i].modelID == modelID {
			overrides[i].window = int64(window)
			found = true
		}
	}
	if !found {
		overrides = append(overrides, windowOverride{modelID, int64(window)})
	}
	return writeWindowOverrides(overrides)
}

// Drops a model's growth state, leaving the file untouched when it has none
func ClearWindowOverride(modelID string) error {
	overrides := loadWindowOverridesOrdered()
	for i, o := range overrides {
		if o.modelID == modelID {
			return writeWindowOverrides(append(overrides[:i:i], overrides[i+1:]...))
		}
	}
	return nil
}

// File name without its last suffix as pathlib stem computes it
func pyPathStem(p string) string {
	name := filepath.Base(ggufPathStr(p))
	if i := strings.LastIndex(name, "."); i > 0 && i < len(name)-1 {
		return name[:i]
	}
	return name
}

// True when the URL is the server the managed runtime state file points at
func IsManagedEndpoint(baseURL string) bool {
	stateURL, _, ok := managedLocalStateEndpoint(home.Process())
	return ok && strings.TrimRight(baseURL, "/") == strings.TrimRight(stateURL, "/")
}

// Seams over the runtime pieces one growth evaluation consults, replaced by tests
var localGrowthDeps = struct {
	getSupervisor      func() *LlamaServerSupervisor
	isIdle             func(sup *LlamaServerSupervisor, modelID string) bool
	isManagedEndpoint  func(string) bool
	stagedModels       func() []string
	readHeader         func(string) (*GGUFHeader, error)
	profileFromHeader  func(*GGUFHeader) (ModelProfile, error)
	probeBudget        func(planning bool) (HardwareBudget, error)
	launchBudget       func(capacity HardwareBudget, ownBytes int64) *HardwareBudget
	presetForModel     func(gguf string, budget HardwareBudget, mtpCapable map[string]bool, requestedWindow int, live *HardwareBudget, residents int, engineTag string) (*PresetEntry, error)
	readPresets        func(presetPath string) map[string]*PresetEntry
	residentFootprint  func(gguf string, budget HardwareBudget, window int, drafting bool) (int64, bool, error)
	refreshLocalServer func() bool
}{
	getSupervisor:      getLocalSupervisor,
	isIdle:             (*LlamaServerSupervisor).IsIdle,
	isManagedEndpoint:  IsManagedEndpoint,
	stagedModels:       stagedModels,
	readHeader:         ReadGGUFHeader,
	profileFromHeader:  ProfileFromGGUF,
	probeBudget:        ProbeBudget,
	launchBudget:       bootstrapLaunchBudget,
	presetForModel:     PresetForModel,
	readPresets:        readPresetDecisions,
	residentFootprint:  residentFootprint,
	refreshLocalServer: refreshLocalRuntime,
}

// Evaluates and executes one growth step, returning the new window when the ladder granted one
func MaybeGrowWindow(modelID, baseURL string, sessionTokens, currentWindow int, measuredDecodeTokS *float64) (int, bool, error) {
	deps := localGrowthDeps
	sup := deps.getSupervisor()
	if sup == nil || !deps.isManagedEndpoint(baseURL) {
		return 0, false, nil
	}
	gguf := ""
	for _, p := range deps.stagedModels() {
		if ModelIDFromStem(pyPathStem(p)) == modelID {
			gguf = p
			break
		}
	}
	if gguf == "" {
		return 0, false, nil
	}
	header, err := deps.readHeader(gguf)
	var profile ModelProfile
	if err == nil {
		profile, err = deps.profileFromHeader(header)
	}
	if err != nil {
		if !GGUFReadSkippable(err) {
			return 0, false, err
		}
		slog.Debug("growth skip: unreadable gguf", "model", modelID, "err", err)
		return 0, false, nil
	}
	serverIdle := deps.isIdle(sup, modelID)
	budget, err := deps.probeBudget(true)
	if err != nil {
		return 0, false, err
	}
	decision := DecideGrowth(AsLoaded(profile, budget), budget, currentWindow, sessionTokens,
		measuredDecodeTokS, serverIdle, true, true)
	if decision.Action != GrowthActionGrow || decision.NextWindow == 0 {
		slog.Debug("growth", "model", modelID, "action", decision.Action, "reason", decision.Reason)
		return 0, false, nil
	}
	running := deps.readPresets("")[modelID]
	drafting := running != nil && running.Keys["spec-type"] == "draft-dflash"
	ownBytes, _, err := deps.residentFootprint(gguf, budget, currentWindow, drafting)
	if err != nil {
		return 0, false, err
	}
	live := deps.launchBudget(budget, ownBytes)
	plan, err := deps.presetForModel(gguf, budget, map[string]bool{}, decision.NextWindow, live, 1, "")
	if err != nil {
		return 0, false, err
	}
	if plan == nil || plan.Refusal != "" || plan.Window < decision.NextWindow {
		slog.Debug("growth: the next rung does not fit beside other programs' GPU memory", "model", modelID)
		return 0, false, nil
	}
	slog.Info("context growth", "model", modelID, "reason", decision.Reason)
	if err := SaveWindowOverride(modelID, decision.NextWindow); err != nil {
		return 0, false, err
	}
	if !deps.refreshLocalServer() {
		slog.Warn("growth: server refresh failed; compression proceeds", "model", modelID)
		return 0, false, nil
	}
	materialized := deps.readPresets("")[modelID]
	if materialized == nil || materialized.Window < decision.NextWindow {
		slog.Warn("growth: refreshed preset did not grant the requested window", "model", modelID)
		return 0, false, nil
	}
	return materialized.Window, true, nil
}
