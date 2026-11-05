package provider

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
)

// Launch flags the policy owns mapped to their INI keys
var presetFlagToKey = map[string]string{
	"-c": "ctx-size", "-b": "batch-size", "-ub": "ubatch-size",
	"-ctk": "cache-type-k", "-ctv": "cache-type-v", "-fa": "flash-attn",
	"-ot": "override-tensor", "--spec-type": "spec-type", "--spec-draft-n-max": "spec-draft-n-max",
}

// Launch decision for one staged model, Order lists Keys in INI order
type PresetEntry struct {
	ModelID string
	Window  int
	Spilled bool
	Refusal string
	Keys    map[string]string
	Order   []string
}

// Sets key keeping its first position like a Python dict assignment
func (p *PresetEntry) set(key, value string) {
	if p.Keys == nil {
		p.Keys = map[string]string{}
	}
	if _, ok := p.Keys[key]; !ok {
		p.Order = append(p.Order, key)
	}
	p.Keys[key] = value
}

// Sets key only when absent like dict.setdefault
func (p *PresetEntry) setDefault(key, value string) {
	if _, ok := p.Keys[key]; !ok {
		p.set(key, value)
	}
}

// Policy owned launch flags as INI keys, every other flag left out
func presetArgsToKeys(args []string, into *PresetEntry) {
	for i := 0; i < len(args); {
		key, ok := presetFlagToKey[args[i]]
		if !ok {
			i++
			continue
		}
		into.set(key, args[i+1])
		i += 2
	}
}

// On disk path of a downloaded companion asset, empty when absent
func presetAssetPath(asset *AssetFile) string {
	if asset == nil {
		return ""
	}
	p := filepath.Join(localAssetsDir(), asset.LocalName())
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// Companion files a catalog entry loads beside its weights as far as they are on disk
type presetCompanions struct {
	mmproj, mtpHead string
	nbytes          int64
}

func presetCompanionsOf(entry *CatalogEntry) presetCompanions {
	if entry == nil {
		return presetCompanions{}
	}
	c := presetCompanions{mmproj: presetAssetPath(entry.MMProj), mtpHead: presetAssetPath(entry.MTPHead)}
	if c.mmproj != "" {
		c.nbytes += entry.MMProj.SizeBytes
	}
	if c.mtpHead != "" {
		c.nbytes += entry.MTPHead.SizeBytes
	}
	return c
}

// MTP spec decode runs from built in heads or a shipped head on disk
func presetRunsMTP(entry *CatalogEntry, c presetCompanions) bool {
	return entry != nil && (entry.MTP || c.mtpHead != "")
}

// Draft admitted only when it neither shrinks the window nor displaces the target's GPU buffers
func presetDraftFits(path string, profile ModelProfile, budget HardwareBudget, window int, overhead int64) (bool, error) {
	header, err := presetReadHeader(path)
	var draft ModelProfile
	if err == nil {
		draft, err = presetProfileFromGGUF(header)
	}
	if err != nil {
		if GGUFReadSkippable(err) {
			slog.Warn(fmt.Sprintf("draft omitted %s: %v", filepath.Base(path), err))
			return false, nil
		}
		return false, err
	}
	need := FootprintBytes(draft, window, false, RuntimeOverheadBytes+UbLogitsBytes(draft.NVocab, false, false))
	return FootprintBytes(profile, window, true, overhead)+need <= budget.UsableVRAMBytes+budget.RAMAvailableBytes &&
		CtxBytes(profile, window, true)+overhead+need <= budget.UsableVRAMBytes, nil
}

// The draft model's own KV and compute buffers beside the target
const presetDraftContextBytes = int64(512) << 20

// llama.cpp build number of a release tag, false when absent or unparseable
func presetEngineBuild(tag string) (int64, bool) {
	if tag == "" {
		return 0, false
	}
	n, err := ggufParseIntString(strings.TrimPrefix(tag, "b"))
	return n, err == nil
}

// Draft weights, one recurrent state copy per draft position and the draft's own context
func presetDFlashExtraBytes(entry *CatalogEntry, profile ModelProfile) int64 {
	copies := int64(entry.DraftDepth + 1)
	return entry.Draft.SizeBytes + profile.RecurrentStateBytes*copies + presetDraftContextBytes
}

// The entry's DFlash draft on disk, empty when it has none
func presetRunsDFlash(entry *CatalogEntry) string {
	if entry == nil || entry.Draft == nil || entry.DraftSpecType != "draft-dflash" {
		return ""
	}
	return presetAssetPath(entry.Draft)
}

// Profile a plan priced, the input profile when the plan carries none
func presetPlanProfile(plan LaunchPlan, fallback ModelProfile) ModelProfile {
	if plan.Profile != nil {
		return *plan.Profile
	}
	return fallback
}

// Launch plan with the DFlash draft in place of MTP, nil when it does not fit or shrinks the window too far
func presetDFlashPlan(entry *CatalogEntry, profile ModelProfile, budget HardwareBudget, live *HardwareBudget,
	fixedOverhead int64, requestedWindow int, engineTag string, windowWithout, residents int) (*LaunchPlan, error) {
	if presetRunsDFlash(entry) == "" {
		return nil, nil
	}
	build, ok := presetEngineBuild(engineTag)
	minimum, minOK := presetEngineBuild(entry.DraftMinEngine)
	if !minOK {
		minimum = 0
	}
	if !ok || build < minimum {
		return nil, nil
	}
	lean := profile
	lean.KVScale = 1.0
	overhead := fixedOverhead + presetDFlashExtraBytes(entry, lean)
	plan := PlanLaunch(lean, budget, false, overhead, requestedWindow)
	if live != nil && live.CommitRoomBytes != nil {
		narrowed := *live
		usable, err := UsableBesidePromptCache(*live, residents)
		if err != nil {
			return nil, err
		}
		narrowed.UsableVRAMBytes = usable
		live = &narrowed
	}
	if live != nil {
		now := PlanLaunch(lean, *live, false, overhead, 0)
		d, ok := now.Decision.(*WindowDecision)
		if !ok || d.Spilled() {
			return nil, nil
		}
		plan = FitToFreeMemory(plan, lean, *live, false, overhead)
	}
	decision, ok := plan.Decision.(*WindowDecision)
	if !ok || decision.Spilled() {
		return nil, nil
	}
	native := profile.NCtxTrain
	if native == 0 {
		native = decision.Window
	}
	if requestedWindow != 0 && decision.Window < min(requestedWindow, native) {
		return nil, nil
	}
	if decision.Window < min(windowWithout, native/2) {
		return nil, nil
	}
	return &plan, nil
}

// Header reader, profile builder, catalog lookup, staged listing and launch footprint every preset pass goes through
var (
	presetReadHeader        = ReadGGUFHeader
	presetProfileFromGGUF   = ProfileFromGGUF
	presetEntryForModel     = EntryForModel
	presetStagedIn          = func(modelsDir string) []string { return stagedIn(modelsDir, true) }
	presetLaunchFootprintOf = presetLaunchFootprint
)

// Stem of a path like PurePath.stem
func presetStem(p string) string { return pyPathStem(p) }

// Launch decision for one staged model, nil with a nil error when its header is unreadable
func PresetForModel(gguf string, budget HardwareBudget, mtpCapable map[string]bool, requestedWindow int,
	live *HardwareBudget, residents int, engineTag string) (*PresetEntry, error) {
	modelID := ModelIDFromStem(presetStem(gguf))
	header, err := presetReadHeader(gguf)
	var profile ModelProfile
	if err == nil {
		profile, err = presetProfileFromGGUF(header)
	}
	if err != nil {
		if GGUFReadSkippable(err) {
			slog.Warn(fmt.Sprintf("preset skip %s: %v", filepath.Base(gguf), err))
			return nil, nil
		}
		return nil, err
	}
	profile = AsLoaded(profile, budget)
	entry := presetEntryForModel(modelID)
	companions := presetCompanionsOf(entry)
	isMTP := mtpCapable[modelID]
	if entry != nil {
		isMTP = presetRunsMTP(entry, companions)
	}
	fixedOverhead := RuntimeOverheadBytes + companions.nbytes
	requested := requestedWindow
	if requestedWindow == 0 {
		requested = LoadWindowOverrides()[modelID]
	}
	plan := PlanLaunch(profile, budget, isMTP, fixedOverhead, requested)
	if live != nil {
		plan = FitToFreeMemory(plan, profile, *live, isMTP, fixedOverhead)
	}
	switch d := plan.Decision.(type) {
	case *PhysicsRefusal:
		return &PresetEntry{ModelID: modelID, Refusal: d.Message}, nil
	case *WindowDecision:
	default:
		return nil, errors.New("launch plan carries no decision")
	}
	decision := plan.Decision.(*WindowDecision)
	var dflash *LaunchPlan
	if !decision.Spilled() {
		if dflash, err = presetDFlashPlan(entry, profile, budget, live, fixedOverhead, requested, engineTag, decision.Window, residents); err != nil {
			return nil, err
		}
	}
	if dflash != nil {
		plan, decision = *dflash, dflash.Decision.(*WindowDecision)
	}
	depth := DefaultMTPDraftDepth
	if entry != nil {
		depth = entry.MTPDraftDepth
	}
	out := &PresetEntry{ModelID: modelID, Window: decision.Window, Spilled: decision.Spilled()}
	presetArgsToKeys(LaunchArgs(profile, decision, true, isMTP && dflash == nil, depth, budget.UMA, plan.MTPPrefill), out)
	out.set("model", gguf)
	planProfile := presetPlanProfile(plan, profile)
	if cache := PromptCacheBytes(live, FootprintBytes(planProfile, decision.Window, true, plan.OverheadBytes), residents); cache != nil {
		out.set("cache-ram", strconv.FormatInt(*cache>>20, 10))
	}
	if profile.LazyBytes != 0 {
		out.set("lazy-mode", "on")
	}
	if entry != nil && isMTP {
		out.set("backend-sampling", "on")
		out.set("spec-draft-backend-sampling", "on")
	}
	sampling, err := header.SamplingDefaults()
	if err != nil {
		return nil, err
	}
	for _, key := range presetSamplingOrder(header, sampling) {
		out.setDefault(key, sampling[key])
	}
	if entry == nil {
		return out, nil
	}
	for _, s := range entry.Sampling {
		out.setDefault(s.Key, pyStr(s.Value))
	}
	if companions.mmproj != "" {
		out.set("mmproj", companions.mmproj)
	}
	if companions.mtpHead != "" {
		out.set("model-draft", companions.mtpHead)
	}
	if dflash != nil {
		out.set("model-draft", presetRunsDFlash(entry))
		out.set("spec-type", "draft-dflash")
		out.set("spec-draft-n-max", strconv.Itoa(entry.DraftDepth))
		out.set("parallel", "1")
	}
	draftPath := ""
	if decision.Spilled() && entry.DraftSpecType == "draft-dspark" {
		draftPath = presetAssetPath(entry.Draft)
	}
	if draftPath != "" {
		fits, err := presetDraftFits(draftPath, planProfile, budget, decision.Window, plan.OverheadBytes)
		if err != nil {
			return nil, err
		}
		if fits {
			out.set("model-draft", draftPath)
			out.set("spec-type", "draft-dspark")
			out.set("spec-draft-n-max", "3")
		}
	}
	return out, nil
}

// INI keys of the header's sampling defaults in metadata order
func presetSamplingOrder(header *GGUFHeader, sampling map[string]string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, key := range header.metadataOrder() {
		if !strings.HasPrefix(key, "general.sampling.") {
			continue
		}
		name, ok := ggufSamplingINIKey[key[strings.LastIndex(key, ".")+1:]]
		if _, present := sampling[name]; ok && present && !seen[name] {
			seen[name] = true
			keys = append(keys, name)
		}
	}
	return keys
}

// Bytes one staged model holds while loaded at window, false when its header is unreadable
func residentFootprint(gguf string, budget HardwareBudget, window int, drafting bool) (int64, bool, error) {
	header, err := presetReadHeader(gguf)
	var profile ModelProfile
	if err == nil {
		profile, err = presetProfileFromGGUF(header)
	}
	if err != nil {
		if !GGUFReadSkippable(err) {
			return 0, false, err
		}
		slog.Debug(fmt.Sprintf("footprint skip %s: %v", filepath.Base(gguf), err))
		return 0, false, nil
	}
	profile = AsLoaded(profile, budget)
	entry := presetEntryForModel(ModelIDFromStem(presetStem(gguf)))
	companions := presetCompanionsOf(entry)
	fixedOverhead := RuntimeOverheadBytes + companions.nbytes
	var plan LaunchPlan
	if drafting && presetRunsDFlash(entry) != "" {
		lean := profile
		lean.KVScale = 1.0
		plan = PlanLaunch(lean, budget, false, fixedOverhead+presetDFlashExtraBytes(entry, lean), window)
		profile = lean
	} else {
		plan = PlanLaunch(profile, budget, presetRunsMTP(entry, companions), fixedOverhead, window)
	}
	return FootprintBytes(presetPlanProfile(plan, profile), window, true, plan.OverheadBytes), true, nil
}

// Resident bytes at the window this policy grants, false when unreadable or refused outright
func presetLaunchFootprint(gguf string, budget HardwareBudget, engineTag string) (int64, bool, error) {
	header, err := presetReadHeader(gguf)
	var profile ModelProfile
	if err == nil {
		profile, err = presetProfileFromGGUF(header)
	}
	if err != nil {
		if !GGUFReadSkippable(err) {
			return 0, false, err
		}
		slog.Debug(fmt.Sprintf("footprint skip %s: %v", filepath.Base(gguf), err))
		return 0, false, nil
	}
	profile = AsLoaded(profile, budget)
	modelID := ModelIDFromStem(presetStem(gguf))
	entry := presetEntryForModel(modelID)
	companions := presetCompanionsOf(entry)
	fixedOverhead := RuntimeOverheadBytes + companions.nbytes
	requested := LoadWindowOverrides()[modelID]
	plan := PlanLaunch(profile, budget, presetRunsMTP(entry, companions), fixedOverhead, requested)
	decision, ok := plan.Decision.(*WindowDecision)
	if !ok {
		return 0, false, nil
	}
	price := FootprintBytes(presetPlanProfile(plan, profile), decision.Window, true, plan.OverheadBytes)
	if !decision.Spilled() {
		dflash, err := presetDFlashPlan(entry, profile, budget, nil, fixedOverhead, requested, engineTag, decision.Window, 1)
		if err != nil {
			return 0, false, err
		}
		if dflash != nil {
			d := dflash.Decision.(*WindowDecision)
			price = max(price, FootprintBytes(presetPlanProfile(*dflash, profile), d.Window, true, dflash.OverheadBytes))
		}
	}
	return price, true, nil
}

// Models the card may hold resident at once, raised above one only while the largest fits twice
func AdmittedResidencyCount(modelsDir string, budget HardwareBudget, configured int, live *HardwareBudget, engineTag string) (int, error) {
	if configured <= 1 || budget.UsableVRAMBytes <= 0 {
		return configured, nil
	}
	var largest int64
	for _, gguf := range presetStagedIn(modelsDir) {
		need, ok, err := presetLaunchFootprintOf(gguf, budget, engineTag)
		if err != nil {
			return 0, err
		}
		if ok && need != 0 {
			largest = max(largest, need)
		}
	}
	if largest <= 0 {
		return configured, nil
	}
	usable := budget.UsableVRAMBytes
	if live != nil && live.CommitRoomBytes != nil {
		usable = min(usable, live.UsableVRAMBytes)
	}
	return int(max(1, min(int64(configured), pyFloorDiv(usable, largest)))), nil
}

// Launch decision of every staged model with unreadable headers skipped
func PlanPresets(modelsDir string, budget HardwareBudget, mtpCapable map[string]bool, live *HardwareBudget,
	residents int, engineTag string) ([]*PresetEntry, error) {
	var out []*PresetEntry
	for _, gguf := range presetStagedIn(modelsDir) {
		entry, err := PresetForModel(gguf, budget, mtpCapable, 0, live, residents, engineTag)
		if err != nil {
			return nil, err
		}
		if entry != nil {
			out = append(out, entry)
		}
	}
	return out, nil
}

// Plans every staged model and writes one INI where refused models get no section
func GeneratePresets(modelsDir string, budget HardwareBudget, presetPath string, mtpCapable map[string]bool,
	live *HardwareBudget, residents int, engineTag string) ([]*PresetEntry, error) {
	entries, err := PlanPresets(modelsDir, budget, mtpCapable, live, residents, engineTag)
	if err != nil {
		return nil, err
	}
	return entries, WritePresets(entries, presetPath)
}

// Atomically writes the rendered INI with owner only permissions
func WritePresets(entries []*PresetEntry, presetPath string) error {
	if err := config.AtomicWrite(presetPath, []byte(RenderPresets(entries)), 0o600); err != nil {
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

// Python json.dumps of a string with ASCII escapes
func presetJSONString(s string) string {
	var b strings.Builder
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
		case r < 0x20 || (r >= 0x7f && r < 0x10000):
			if r == 0x7f {
				b.WriteRune(r)
				continue
			}
			fmt.Fprintf(&b, `\u%04x`, r)
		case r >= 0x10000:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// INI text with a decision comment per model and a section for each admitted one
func RenderPresets(entries []*PresetEntry) string {
	var sections []string
	for _, e := range entries {
		refusal := "null"
		if e.Refusal != "" {
			refusal = presetJSONString(e.Refusal)
		}
		sections = append(sections, fmt.Sprintf("# hermes-decision: {\"model_id\": %s, \"window\": %d, \"spilled\": %t, \"refusal\": %s}\n",
			presetJSONString(e.ModelID), e.Window, e.Spilled, refusal))
		if e.Keys != nil {
			lines := make([]string, 0, len(e.Order))
			for _, k := range presetKeyOrder(e) {
				lines = append(lines, k+" = "+e.Keys[k])
			}
			sections = append(sections, fmt.Sprintf("[%s]\n%s\n", e.ModelID, strings.Join(lines, "\n")))
		}
	}
	return strings.Join(sections, "\n")
}

// Order followed by any keys set without it
func presetKeyOrder(e *PresetEntry) []string {
	seen := map[string]bool{}
	var keys []string
	for _, k := range e.Order {
		if _, ok := e.Keys[k]; ok && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range e.Keys {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	return keys
}

// Launch decisions the running server was given, read back from the preset INI, partial on a malformed record
func readPresetDecisions(presetPath string) map[string]*PresetEntry {
	if presetPath == "" {
		presetPath = filepath.Join(RuntimesRoot(), "presets.ini")
	}
	out := map[string]*PresetEntry{}
	if err := presetReadBack(presetPath, out); err != nil {
		slog.Debug(fmt.Sprintf("preset read-back failed: %v", err))
	}
	return out
}

// Fills out from the INI and its decision comments, stopping at the first Python exception
func presetReadBack(presetPath string, out map[string]*PresetEntry) error {
	data, err := os.ReadFile(presetPath)
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return errors.New("'utf-8' codec can't decode the preset file")
	}
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	doc, err := parseAWSINI(text)
	if err != nil {
		return err
	}
	recorded := map[string]map[string]any{}
	for _, line := range pySplitLines(text) {
		rest, ok := strings.CutPrefix(line, "# hermes-decision: ")
		if !ok {
			continue
		}
		decoded, ok := moaJSONLoads(rest)
		if !ok {
			return errors.New("decision comment is not valid JSON")
		}
		fact, ok := decoded.(map[string]any)
		if !ok {
			return errors.New("decision comment is not an object")
		}
		rawID, ok := fact["model_id"]
		if !ok {
			return errors.New("KeyError: 'model_id'")
		}
		id := pyStr(rawID)
		recorded[id] = fact
		if pyTruthy(fact["refusal"]) {
			entry, err := presetEntryFromFact(fact)
			if err != nil {
				return err
			}
			out[id] = entry
		}
	}
	order := presetSectionKeyOrder(text)
	for _, section := range doc.order {
		keys := doc.flat[section]
		window := int64(0)
		if raw, ok := keys["ctx-size"]; ok {
			if window, err = ggufParseIntString(raw); err != nil {
				return err
			}
		}
		_, spilled := keys["override-tensor"]
		if fact, ok := recorded[section]; ok {
			if raw, ok := fact["spilled"]; ok {
				spilled = pyTruthy(raw)
			}
		}
		entry := &PresetEntry{ModelID: section, Window: int(window), Spilled: spilled}
		for _, k := range order[section] {
			if v, ok := keys[k]; ok {
				entry.set(k, v)
			}
		}
		for k, v := range keys {
			entry.setDefault(k, v)
		}
		out[section] = entry
	}
	return nil
}

// PresetEntry(**fact), an error where Python raises TypeError on unknown or missing fields
func presetEntryFromFact(fact map[string]any) (*PresetEntry, error) {
	allowed := map[string]bool{"model_id": true, "window": true, "spilled": true, "refusal": true, "keys": true}
	for k := range fact {
		if !allowed[k] {
			return nil, fmt.Errorf("PresetEntry.__init__() got an unexpected keyword argument %s", pyRepr(k))
		}
	}
	for _, k := range []string{"model_id", "window", "spilled"} {
		if _, ok := fact[k]; !ok {
			return nil, fmt.Errorf("PresetEntry.__init__() missing 1 required positional argument: %s", pyRepr(k))
		}
	}
	window, err := growthPyInt(fact["window"])
	if err != nil {
		return nil, err
	}
	entry := &PresetEntry{ModelID: pyStr(fact["model_id"]), Window: int(window), Spilled: pyTruthy(fact["spilled"]), Refusal: pyStr(fact["refusal"])}
	if keys, ok := fact["keys"].(map[string]any); ok {
		for k, v := range keys {
			entry.set(k, pyStr(v))
		}
	}
	return entry, nil
}

// Lowercased option names of each section in file order, DEFAULT options appended after the section's own
func presetSectionKeyOrder(text string) map[string][]string {
	order := map[string][]string{}
	var defaults []string
	current := ""
	for _, line := range pySplitLines(text) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") || (len(line) > 0 && (line[0] == ' ' || line[0] == '\t') && current != "") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			current = trimmed[1 : len(trimmed)-1]
			continue
		}
		i := strings.IndexAny(line, "=:")
		if i < 0 || current == "" {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(line[:i]))
		if current == "DEFAULT" {
			defaults = append(defaults, name)
		} else {
			order[current] = append(order[current], name)
		}
	}
	for section := range order {
		order[section] = append(order[section], defaults...)
	}
	return order
}
