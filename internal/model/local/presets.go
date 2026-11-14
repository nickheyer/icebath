package local

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/home"
)

const (
	decisionPrefix     = "# icebath-decision: "
	draftContextBytes  = int64(512 << 20)
	dflashSpecType     = "draft-dflash"
	dsparkSpecType     = "draft-dspark"
	defaultDraftDepth  = 3
	dsparkDraftNMax    = "3"
	presetKeyModel     = "model"
	presetKeyCtxSize   = "ctx-size"
	presetKeySpecType  = "spec-type"
	presetKeyOverrides = "override-tensor"
)

var flagToKey = map[string]string{
	"-c": "ctx-size", "-b": "batch-size", "-ub": "ubatch-size",
	"-ctk": "cache-type-k", "-ctv": "cache-type-v", "-fa": "flash-attn",
	"-ot": "override-tensor", "--spec-type": "spec-type", "--spec-draft-n-max": "spec-draft-n-max",
}

type PresetEntry struct {
	ModelID string
	Window  int
	Spilled bool
	Refusal string
	Keys    []INIPair
}

func (e PresetEntry) Key(name string) (string, bool) {
	for _, p := range e.Keys {
		if p.Key == name {
			return p.Value, true
		}
	}
	return "", false
}

func setKey(keys []INIPair, key, value string) []INIPair {
	for i := range keys {
		if keys[i].Key == key {
			keys[i].Value = value
			return keys
		}
	}
	return append(keys, INIPair{Key: key, Value: value})
}

func setDefaultKey(keys []INIPair, key, value string) []INIPair {
	for _, p := range keys {
		if p.Key == key {
			return keys
		}
	}
	return append(keys, INIPair{Key: key, Value: value})
}

func argsToKeys(args []string) []INIPair {
	keys := []INIPair{}
	for i := 0; i < len(args); {
		key, ok := flagToKey[args[i]]
		if !ok || i+1 >= len(args) {
			i++
			continue
		}
		keys = setKey(keys, key, args[i+1])
		i += 2
	}
	return keys
}

func (r *Runtime) assetPath(asset *AssetFile) string {
	if asset == nil {
		return ""
	}
	p := filepath.Join(r.AssetsDir(), asset.LocalName())
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

type companions struct {
	mmproj, mtpHead string
	nbytes          int64
}

func (r *Runtime) companionsOf(entry *CatalogEntry) companions {
	if entry == nil {
		return companions{}
	}
	c := companions{mmproj: r.assetPath(entry.MMProj), mtpHead: r.assetPath(entry.MTPHead)}
	if c.mmproj != "" {
		c.nbytes += entry.MMProj.SizeBytes
	}
	if c.mtpHead != "" {
		c.nbytes += entry.MTPHead.SizeBytes
	}
	return c
}

func runsMTP(entry *CatalogEntry, c companions) bool {
	return entry != nil && (entry.MTP || c.mtpHead != "")
}

func draftFits(draftPath string, p ModelProfile, b HardwareBudget, window int, overhead int64) bool {
	h, err := ReadGGUFHeader(draftPath)
	if err != nil {
		slog.Warn(fmt.Sprintf("draft omitted %s: %v", filepath.Base(draftPath), err))
		return false
	}
	draft := ProfileFromGGUF(h)
	draftNeed := FootprintBytes(draft, window, false, RuntimeOverheadBytes+UbLogitsBytes(draft.NVocab, false, false))
	return FootprintBytes(p, window, true, overhead)+draftNeed <= b.UsableVRAMBytes+b.RAMAvailableBytes &&
		CtxBytes(p, window, true)+overhead+draftNeed <= b.UsableVRAMBytes
}

func engineBuild(tag string) (int, bool) {
	if tag == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(tag, "b")))
	if err != nil {
		return 0, false
	}
	return n, true
}

func dflashExtraBytes(entry *CatalogEntry, p ModelProfile) int64 {
	copies := int64(entry.DraftDepth + 1)
	return entry.Draft.SizeBytes + p.RecurrentStateBytes*copies + draftContextBytes
}

func (r *Runtime) runsDFlash(entry *CatalogEntry) string {
	if entry == nil || entry.Draft == nil || entry.DraftSpecType != dflashSpecType {
		return ""
	}
	return r.assetPath(entry.Draft)
}

type dflashInput struct {
	fixedOverhead   int64
	requestedWindow int
	engineTag       string
	windowWithout   int
	residents       int
}

func (r *Runtime) dflashPlan(entry *CatalogEntry, p ModelProfile, b HardwareBudget, live *HardwareBudget, in dflashInput) *LaunchPlan {
	if r.runsDFlash(entry) == "" {
		return nil
	}
	build, ok := engineBuild(in.engineTag)
	minBuild, _ := engineBuild(entry.DraftMinEngine)
	if !ok || build < minBuild {
		return nil
	}
	lean := p
	lean.KVScale = 1.0
	overhead := in.fixedOverhead + dflashExtraBytes(entry, lean)
	plan := PlanLaunch(lean, b, PlanOptions{FixedOverhead: overhead, RequestedWindow: in.requestedWindow})
	if live != nil && live.CommitRoomBytes != nil {
		narrowed := *live
		narrowed.UsableVRAMBytes = UsableBesidePromptCache(*live, max(1, in.residents))
		live = &narrowed
	}
	if live != nil {
		now := PlanLaunch(lean, *live, PlanOptions{FixedOverhead: overhead})
		if now.Refusal != nil || now.Decision.Spilled() {
			return nil
		}
		plan = FitToFreeMemory(plan, lean, *live, false, overhead)
	}
	if plan.Refusal != nil || plan.Decision.Spilled() {
		return nil
	}
	decision := plan.Decision
	native := p.NCtxTrain
	if native == 0 {
		native = decision.Window
	}
	if in.requestedWindow != 0 && decision.Window < min(in.requestedWindow, native) {
		return nil
	}
	if decision.Window < min(in.windowWithout, native/2) {
		return nil
	}
	return &plan
}

type presetRequest struct {
	requestedWindow int
	readOverrides   bool
	live            *HardwareBudget
	residents       int
	engineTag       string
}

func (r *Runtime) presetForModel(gguf string, b HardwareBudget, q presetRequest) *PresetEntry {
	modelID := ModelIDFromStem(strings.TrimSuffix(filepath.Base(gguf), filepath.Ext(gguf)))
	header, err := ReadGGUFHeader(gguf)
	if err != nil {
		slog.Warn(fmt.Sprintf("preset skip %s: %v", filepath.Base(gguf), err))
		return nil
	}
	profile := AsLoaded(ProfileFromGGUF(header), b)
	entry := r.EntryForModel(modelID)
	comp := r.companionsOf(entry)
	isMTP := runsMTP(entry, comp)
	fixedOverhead := RuntimeOverheadBytes + comp.nbytes
	requested := q.requestedWindow
	if q.readOverrides {
		requested = r.LoadWindowOverrides()[modelID]
	}
	plan := PlanLaunch(profile, b, PlanOptions{MTPCapable: isMTP, FixedOverhead: fixedOverhead, RequestedWindow: requested})
	if q.live != nil {
		plan = FitToFreeMemory(plan, profile, *q.live, isMTP, fixedOverhead)
	}
	if plan.Refusal != nil {
		return &PresetEntry{ModelID: modelID, Refusal: plan.Refusal.Message}
	}
	decision := plan.Decision
	var dflash *LaunchPlan
	if !decision.Spilled() {
		dflash = r.dflashPlan(entry, profile, b, q.live, dflashInput{
			fixedOverhead: fixedOverhead, requestedWindow: requested, engineTag: q.engineTag,
			windowWithout: decision.Window, residents: q.residents,
		})
	}
	if dflash != nil {
		plan, decision = *dflash, dflash.Decision
	}
	depth := defaultDraftDepth
	if entry != nil {
		depth = entry.MTPDraftDepth
	}
	keys := argsToKeys(LaunchArgs(profile, *decision, LaunchOptions{
		FlashAttention: true, MTPCapable: isMTP && dflash == nil, MTPDraftDepth: depth,
		UMA: b.UMA, MTPPrefill: plan.MTPPrefill,
	}))
	keys = setKey(keys, presetKeyModel, gguf)
	footprint := FootprintBytes(plan.Profile, decision.Window, true, plan.OverheadBytes)
	if cache := PromptCacheBytes(q.live, footprint, q.residents); cache != nil {
		keys = setKey(keys, "cache-ram", strconv.FormatInt(*cache>>20, 10))
	}
	if profile.LazyBytes != 0 {
		keys = setKey(keys, "lazy-mode", "on")
	}
	if entry != nil && isMTP {
		keys = setKey(keys, "backend-sampling", "on")
		keys = setKey(keys, "spec-draft-backend-sampling", "on")
	}
	for _, p := range header.SamplingDefaults() {
		keys = setDefaultKey(keys, p.Key, p.Value)
	}
	if entry != nil {
		for _, p := range entry.Sampling {
			keys = setDefaultKey(keys, p.Key, p.Value)
		}
		if comp.mmproj != "" {
			keys = setKey(keys, "mmproj", comp.mmproj)
		}
		if comp.mtpHead != "" {
			keys = setKey(keys, "model-draft", comp.mtpHead)
		}
		if dflash != nil {
			keys = setKey(keys, "model-draft", r.runsDFlash(entry))
			keys = setKey(keys, presetKeySpecType, dflashSpecType)
			keys = setKey(keys, "spec-draft-n-max", strconv.Itoa(entry.DraftDepth))
			keys = setKey(keys, "parallel", "1")
		}
		if decision.Spilled() && entry.DraftSpecType == dsparkSpecType {
			if draftPath := r.assetPath(entry.Draft); draftPath != "" &&
				draftFits(draftPath, plan.Profile, b, decision.Window, plan.OverheadBytes) {
				keys = setKey(keys, "model-draft", draftPath)
				keys = setKey(keys, presetKeySpecType, dsparkSpecType)
				keys = setKey(keys, "spec-draft-n-max", dsparkDraftNMax)
			}
		}
	}
	return &PresetEntry{ModelID: modelID, Window: decision.Window, Spilled: decision.Spilled(), Keys: keys}
}

func (r *Runtime) staticProfile(gguf string, b HardwareBudget) (ModelProfile, string, bool) {
	modelID := ModelIDFromStem(strings.TrimSuffix(filepath.Base(gguf), filepath.Ext(gguf)))
	header, err := ReadGGUFHeader(gguf)
	if err != nil {
		slog.Debug(fmt.Sprintf("footprint skip %s: %v", filepath.Base(gguf), err))
		return ModelProfile{}, modelID, false
	}
	return AsLoaded(ProfileFromGGUF(header), b), modelID, true
}

func (r *Runtime) residentFootprint(gguf string, b HardwareBudget, window int, drafting bool) (int64, bool) {
	profile, modelID, ok := r.staticProfile(gguf, b)
	if !ok {
		return 0, false
	}
	entry := r.EntryForModel(modelID)
	comp := r.companionsOf(entry)
	fixedOverhead := RuntimeOverheadBytes + comp.nbytes
	var plan LaunchPlan
	if drafting && r.runsDFlash(entry) != "" {
		lean := profile
		lean.KVScale = 1.0
		plan = PlanLaunch(lean, b, PlanOptions{FixedOverhead: fixedOverhead + dflashExtraBytes(entry, lean), RequestedWindow: window})
	} else {
		plan = PlanLaunch(profile, b, PlanOptions{MTPCapable: runsMTP(entry, comp), FixedOverhead: fixedOverhead, RequestedWindow: window})
	}
	return FootprintBytes(plan.Profile, window, true, plan.OverheadBytes), true
}

func (r *Runtime) launchFootprint(gguf string, b HardwareBudget, engineTag string) (int64, bool) {
	profile, modelID, ok := r.staticProfile(gguf, b)
	if !ok {
		return 0, false
	}
	entry := r.EntryForModel(modelID)
	comp := r.companionsOf(entry)
	fixedOverhead := RuntimeOverheadBytes + comp.nbytes
	requested := r.LoadWindowOverrides()[modelID]
	plan := PlanLaunch(profile, b, PlanOptions{MTPCapable: runsMTP(entry, comp), FixedOverhead: fixedOverhead, RequestedWindow: requested})
	if plan.Refusal != nil {
		return 0, false
	}
	price := FootprintBytes(plan.Profile, plan.Decision.Window, true, plan.OverheadBytes)
	if !plan.Decision.Spilled() {
		if dflash := r.dflashPlan(entry, profile, b, nil, dflashInput{
			fixedOverhead: fixedOverhead, requestedWindow: requested, engineTag: engineTag,
			windowWithout: plan.Decision.Window, residents: 1,
		}); dflash != nil {
			price = max(price, FootprintBytes(dflash.Profile, dflash.Decision.Window, true, dflash.OverheadBytes))
		}
	}
	return price, true
}

func (r *Runtime) admittedResidencyCount(modelsDir string, b HardwareBudget, configured int, live *HardwareBudget, engineTag string) int {
	if configured <= 1 || b.UsableVRAMBytes <= 0 {
		return configured
	}
	var largest int64
	for _, gguf := range StagedIn(modelsDir, true) {
		if need, ok := r.launchFootprint(gguf, b, engineTag); ok && need != 0 {
			largest = max(largest, need)
		}
	}
	if largest <= 0 {
		return configured
	}
	usable := b.UsableVRAMBytes
	if live != nil && live.CommitRoomBytes != nil {
		usable = min(usable, live.UsableVRAMBytes)
	}
	return int(max(1, min(int64(configured), usable/largest)))
}

func (r *Runtime) planPresets(modelsDir string, b HardwareBudget, live *HardwareBudget, residents int, engineTag string) []PresetEntry {
	out := []PresetEntry{}
	for _, gguf := range StagedIn(modelsDir, true) {
		if e := r.presetForModel(gguf, b, presetRequest{
			readOverrides: true, live: live, residents: residents, engineTag: engineTag,
		}); e != nil {
			out = append(out, *e)
		}
	}
	return out
}

func (r *Runtime) generatePresets(ctx context.Context, modelsDir string, b HardwareBudget, presetPath string, live *HardwareBudget, residents int, engineTag string) ([]PresetEntry, error) {
	entries := r.planPresets(modelsDir, b, live, residents, engineTag)
	if err := writePresets(ctx, entries, presetPath); err != nil {
		return nil, err
	}
	return entries, nil
}

func writePresets(ctx context.Context, entries []PresetEntry, presetPath string) error {
	text, err := renderPresets(entries)
	if err != nil {
		return err
	}
	if err := fileutil.WriteFile(ctx, presetPath, []byte(text), fileutil.Perm(0o600), fileutil.TempPrefix("."+filepath.Base(presetPath)+"_")); err != nil {
		return err
	}
	sections := 0
	for _, e := range entries {
		if e.Keys != nil {
			sections++
		}
	}
	slog.Info(fmt.Sprintf("wrote %d preset sections to %s", sections, presetPath))
	return nil
}

type decisionRecord struct {
	ModelID string  `json:"model_id"`
	Window  int     `json:"window"`
	Spilled bool    `json:"spilled"`
	Refusal *string `json:"refusal"`
}

func renderPresets(entries []PresetEntry) (string, error) {
	sections := make([]string, 0, 2*len(entries))
	for _, e := range entries {
		rec := decisionRecord{ModelID: e.ModelID, Window: e.Window, Spilled: e.Spilled}
		if e.Refusal != "" {
			refusal := e.Refusal
			rec.Refusal = &refusal
		}
		fact, err := fileutil.EncodeJSON(rec, fileutil.JSONStyle{ASCII: true})
		if err != nil {
			return "", err
		}
		sections = append(sections, decisionPrefix+strings.TrimRight(string(fact), "\n")+"\n")
		if e.Keys != nil {
			lines := make([]string, len(e.Keys))
			for i, p := range e.Keys {
				lines[i] = p.Key + " = " + p.Value
			}
			sections = append(sections, "["+e.ModelID+"]\n"+strings.Join(lines, "\n")+"\n")
		}
	}
	return strings.Join(sections, "\n"), nil
}

type iniSection struct {
	name string
	keys []INIPair
}

func parseINI(text string) ([]iniSection, bool) {
	var sections []iniSection
	var defaults []INIPair
	seen := map[string]bool{}
	var current *[]INIPair
	lastKey := -1
	optionIndent := 0
	pendingBlank := 0
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		stripped := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if stripped == "" {
			pendingBlank++
			continue
		}
		if strings.HasPrefix(stripped, "#") || strings.HasPrefix(stripped, ";") {
			continue
		}
		if current != nil && lastKey >= 0 && indent > optionIndent {
			value := (*current)[lastKey].Value
			for ; pendingBlank > 0; pendingBlank-- {
				value += "\n"
			}
			(*current)[lastKey].Value = value + "\n" + stripped
			continue
		}
		pendingBlank = 0
		if strings.HasPrefix(stripped, "[") && strings.Contains(stripped[1:], "]") {
			name := stripped[1:strings.LastIndexByte(stripped, ']')]
			if name == "" {
				return nil, false
			}
			if name == "DEFAULT" {
				current, lastKey = &defaults, -1
				continue
			}
			if seen[name] {
				return nil, false
			}
			seen[name] = true
			sections = append(sections, iniSection{name: name, keys: []INIPair{}})
			current, lastKey = &sections[len(sections)-1].keys, -1
			continue
		}
		if current == nil {
			return nil, false
		}
		cut := strings.IndexAny(stripped, "=:")
		if cut < 0 {
			return nil, false
		}
		key := strings.ToLower(strings.TrimSpace(stripped[:cut]))
		value := strings.TrimSpace(stripped[cut+1:])
		for _, p := range *current {
			if p.Key == key {
				return nil, false
			}
		}
		*current = append(*current, INIPair{Key: key, Value: value})
		lastKey, optionIndent = len(*current)-1, indent
	}
	for i := range sections {
		for _, d := range defaults {
			sections[i].keys = setDefaultKey(sections[i].keys, d.Key, d.Value)
		}
	}
	return sections, true
}

func (r *Runtime) PresetsPath() string { return home.LlamaCppPresets.In(r.root) }

func (r *Runtime) ReadPresetDecisions(presetPath string) map[string]PresetEntry {
	if presetPath == "" {
		presetPath = r.PresetsPath()
	}
	out := map[string]PresetEntry{}
	text, err := fileutil.ReadText(presetPath)
	if err != nil {
		slog.Debug(fmt.Sprintf("preset read-back failed: %v", err))
		return out
	}
	sections, ok := parseINI(text)
	if !ok {
		slog.Debug("preset read-back failed: unparseable preset file")
		return out
	}
	recorded := map[string]map[string]json.RawMessage{}
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, decisionPrefix) {
			continue
		}
		var fact map[string]json.RawMessage
		var rec decisionRecord
		raw := strings.TrimPrefix(line, decisionPrefix)
		if err := json.Unmarshal([]byte(raw), &fact); err != nil {
			slog.Debug(fmt.Sprintf("preset read-back failed: %v", err))
			return out
		}
		if err := json.Unmarshal([]byte(raw), &rec); err != nil || fact["model_id"] == nil {
			slog.Debug("preset read-back failed: decision without a model id")
			return out
		}
		recorded[rec.ModelID] = fact
		if rec.Refusal != nil && *rec.Refusal != "" {
			out[rec.ModelID] = PresetEntry{ModelID: rec.ModelID, Window: rec.Window, Spilled: rec.Spilled, Refusal: *rec.Refusal}
		}
	}
	for _, s := range sections {
		e := PresetEntry{ModelID: s.name, Keys: s.keys}
		if ctxSize, ok := e.Key(presetKeyCtxSize); ok {
			n, err := strconv.Atoi(strings.TrimSpace(ctxSize))
			if err != nil {
				slog.Debug(fmt.Sprintf("preset read-back failed: %v", err))
				return out
			}
			e.Window = n
		}
		_, e.Spilled = e.Key(presetKeyOverrides)
		if raw, ok := recorded[s.name]["spilled"]; ok {
			var spilled bool
			if json.Unmarshal(raw, &spilled) == nil {
				e.Spilled = spilled
			}
		}
		out[s.name] = e
	}
	return out
}
