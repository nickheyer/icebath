package provider

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"path"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/hostinfo"
)

// One downloadable file with its repo relative path, exact size, local name override and source repo
type AssetFile struct {
	Path      string
	SizeBytes int64
	Local     string
	Repo      string
	Revision  string
}

// On disk name, the override when set or the final path component
func (a AssetFile) LocalName() string {
	if a.Local != "" {
		return a.Local
	}
	return path.Base(a.Path)
}

// One downloadable build whose first file is the load target
type QuantVariant struct {
	Quant     string
	Files     []AssetFile
	Validated bool
	LazyBytes int64
}

// Model id of the first file stem with split part suffixes removed
func (v QuantVariant) ModelID() string {
	return ModelIDFromStem(strings.TrimSuffix(path.Base(v.Files[0].Path), ".gguf"))
}

func (v QuantVariant) SizeBytes() int64 {
	var total int64
	for _, f := range v.Files {
		total += f.SizeBytes
	}
	return total
}

// Pre download weights estimate, file bytes less the tensors the engine reads on demand
func (v QuantVariant) WeightsBytes() int64 { return v.SizeBytes() - v.LazyBytes }

// One INI launch default in catalog document order
type CatalogSetting struct {
	Key   string
	Value any
}

// One curated model family with the estimator inputs that price it before download
type CatalogEntry struct {
	ID, DisplayName, Description, Repo string
	Variants                           []QuantVariant
	NCtxTrain, FullLayers              int
	RecurrentLayers                    int
	PerLayerF16                        int64
	SWALayers, SWAWindow               int
	MoE, MTP                           bool
	MTPDraftDepth                      int
	NVocab                             int64
	Architecture                       string
	MMProj, Draft, MTPHead             *AssetFile
	DraftSpecType                      string
	DraftDepth                         int
	DraftMinEngine                     string
	Sampling                           []CatalogSetting
	MinEngine                          string
	Quality                            int
	DecodeFraction                     float64
	Recommend                          bool
}

// MTP spec decode runs from heads built into the model or shipped beside it
func (e CatalogEntry) MTPCapable() bool { return e.MTP || e.MTPHead != nil }

// Bytes of the vision projector and MTP head loaded beside the weights
func (e CatalogEntry) CompanionBytes() int64 {
	var total int64
	for _, a := range []*AssetFile{e.MMProj, e.MTPHead} {
		if a != nil {
			total += a.SizeBytes
		}
	}
	return total
}

// Estimator profile of a variant from the catalog's measured layer facts
func (e CatalogEntry) Profile(v QuantVariant) ModelProfile {
	var layers []ProfileLayer
	for i := 0; i < e.FullLayers; i++ {
		layers = append(layers, ProfileLayer{Kind: LayerFull, KVBytesPerTok: e.PerLayerF16})
	}
	for i := 0; i < e.SWALayers; i++ {
		layers = append(layers, ProfileLayer{Kind: LayerSWA, KVBytesPerTok: e.PerLayerF16})
	}
	for i := 0; i < e.RecurrentLayers; i++ {
		layers = append(layers, ProfileLayer{Kind: LayerRecurrent})
	}
	profile := NewModelProfile(v.ModelID(), v.WeightsBytes(), 0, e.NCtxTrain, layers)
	profile.SWAWindow, profile.MoE, profile.Architecture, profile.NVocab = e.SWAWindow, e.MoE, e.Architecture, e.NVocab
	profile.KVScale, profile.LazyBytes = 1.0, v.LazyBytes
	if e.MTPCapable() {
		profile.KVScale = 1.2
	}
	return profile
}

// Launch plan of a variant on this budget with companions priced as fixed overhead
func (e CatalogEntry) LaunchPlan(v QuantVariant, budget HardwareBudget) LaunchPlan {
	return PlanLaunch(AsLoaded(e.Profile(v), budget), budget, e.MTPCapable(), RuntimeOverheadBytes+e.CompanionBytes(), 0)
}

// Variant files then projector, draft and MTP head in the order a download job fetches them
func (e CatalogEntry) DownloadFiles(v QuantVariant) []AssetFile {
	return catalogWithExtras(v.Files, e.MMProj, e.Draft, e.MTPHead)
}

func (e CatalogEntry) DownloadBytes(v QuantVariant) int64 {
	var total int64
	for _, f := range e.DownloadFiles(v) {
		total += f.SizeBytes
	}
	return total
}

// Files a variant needs to count as downloaded, the optional draft excluded
func (e CatalogEntry) InstallFiles(v QuantVariant) []AssetFile {
	return catalogWithExtras(v.Files, e.MMProj, e.MTPHead)
}

// Copy of files followed by every present extra
func catalogWithExtras(files []AssetFile, extras ...*AssetFile) []AssetFile {
	out := append([]AssetFile(nil), files...)
	for _, a := range extras {
		if a != nil {
			out = append(out, *a)
		}
	}
	return out
}

// Build this machine should download with the UI reason key of the fit
type VariantChoice struct {
	Variant   QuantVariant
	ZeroSpill bool
	ReasonKey string
}

// Fits the entry's Q4 build to this machine, nil when physics refuses it
func SelectVariant(entry CatalogEntry, budget HardwareBudget) *VariantChoice {
	variant := entry.Variants[len(entry.Variants)-1]
	decision, ok := entry.LaunchPlan(variant, budget).Decision.(*WindowDecision)
	if !ok {
		return nil
	}
	if decision.Spilled() {
		return &VariantChoice{Variant: variant, ZeroSpill: false, ReasonKey: "smallest-fits-spilled"}
	}
	floor := entry.NCtxTrain
	if floor == 0 {
		floor = ContextFloor
	}
	reason := "best-fits"
	if decision.Window >= min(TargetWindow, floor) {
		reason = "best-large-window"
	}
	return &VariantChoice{Variant: variant, ZeroSpill: true, ReasonKey: reason}
}

// Variant fit the recommendation and the local models routes read, the module attribute hermes tests patch
var catalogSelectVariantFn = SelectVariant

// Memory bandwidth classes in GB per second for discrete cards, unified memory and spilled host DRAM
const (
	catalogDiscreteBandwidthGBs = 1000.0
	catalogUMABandwidthGBs      = 210.0
	catalogHostBandwidthGBs     = 80.0
)

// Predicted decode speed below which a model stops feeling pleasant for agentic use
const PleasantFloorTokS = 20.0

type catalogMeasuredKey struct {
	platform, backend, gpu, entry, quant string
	depth                                int
}

type catalogHardwareKey struct{ platform, backend, gpu string }

var catalogMeasuredDecodeTokS = map[catalogMeasuredKey]float64{
	{"win32", "cuda", "NVIDIA RTX Spark N1X", "qwen3.8-27b", "UD-Q4_K_M", 2}: 21.9,
}

var catalogProductDefaults = map[catalogHardwareKey]string{
	{"win32", "cuda", "NVIDIA RTX Spark N1X"}: "qwen3.8-27b",
}

// Platform, effective backend and recognized reference GPU name keying the shipped tables
func catalogHardwareKeyOf(budget HardwareBudget, backend string) catalogHardwareKey {
	gpu, _, _ := strings.Cut(budget.GPUName, " (")
	if budget.GPUPCIID != nil {
		gpu = ""
		if hostinfo.IsNvidiaN1XPCIID(int(*budget.GPUPCIID)) {
			gpu = "NVIDIA RTX Spark N1X"
		}
	}
	effective := backend
	if backend == "auto" && gpu != "" {
		effective = "cuda"
	}
	return catalogHardwareKey{budget.Platform, effective, gpu}
}

// Shipped measured baseline where matched, otherwise bandwidth over bytes read per token
func PredictedDecodeTokS(entry CatalogEntry, variant QuantVariant, budget HardwareBudget, spilled bool, backend string) float64 {
	hw := catalogHardwareKeyOf(budget, backend)
	key := catalogMeasuredKey{hw.platform, hw.backend, hw.gpu, entry.ID, variant.Quant, entry.MTPDraftDepth}
	if budget.UMA && entry.MTP && !spilled {
		if measured, ok := catalogMeasuredDecodeTokS[key]; ok {
			return measured
		}
	}
	bandwidth := catalogDiscreteBandwidthGBs
	if spilled {
		bandwidth = catalogHostBandwidthGBs
	} else if budget.UMA {
		bandwidth = catalogUMABandwidthGBs
	}
	bytesPerToken := math.Max(1.0, float64(variant.SizeBytes())*entry.DecodeFraction)
	return bandwidth * 1e9 / bytesPerToken
}

// Default pick for this machine with its reason key, nil when nothing resident clears the pleasant floor
func RecommendedEntry(budget HardwareBudget, entries []CatalogEntry, backend string) (*CatalogEntry, string) {
	if entries == nil {
		entries = Catalog()
	}
	type fit struct {
		entry  CatalogEntry
		choice *VariantChoice
	}
	var resident []fit
	anyFit := false
	for _, e := range entries {
		if !e.Recommend {
			continue
		}
		if c := catalogSelectVariantFn(e, budget); c != nil {
			anyFit = true
			if c.ZeroSpill {
				resident = append(resident, fit{e, c})
			}
		}
	}
	if !anyFit {
		return nil, ""
	}
	if def, ok := catalogProductDefaults[catalogHardwareKeyOf(budget, backend)]; ok {
		for _, r := range resident {
			if r.entry.ID == def {
				pick := r.entry
				return &pick, "product-default"
			}
		}
	}
	var best *fit
	for i := range resident {
		r := &resident[i]
		if PredictedDecodeTokS(r.entry, r.choice.Variant, budget, false, backend) < PleasantFloorTokS {
			continue
		}
		if best == nil || r.entry.Quality > best.entry.Quality ||
			(r.entry.Quality == best.entry.Quality && r.choice.Variant.SizeBytes() < best.choice.Variant.SizeBytes()) {
			best = r
		}
	}
	if best == nil {
		return nil, ""
	}
	pick := best.entry
	for _, r := range resident {
		if r.entry.Quality > pick.Quality {
			return &pick, "speed-gated-quality"
		}
	}
	return &pick, "best-quality-resident"
}

//go:embed local_runtime_catalog.json
var packagedCatalogJSON []byte

const (
	catalogURL           = "https://raw.githubusercontent.com/NousResearch/hermes-agent/main/hermes_cli/local_runtime/catalog.json"
	catalogSchemaVersion = 3
	catalogRefreshTTL    = 6 * time.Hour
)

// Running catalog, refresh state and the seams tests replace
var (
	lrCatalogMu           sync.Mutex
	catalogCurrent        []CatalogEntry
	catalogLoaded         bool
	catalogRefreshMu      sync.Mutex
	catalogLastAttempt    *time.Duration
	catalogMonotonicStart = time.Now()
	catalogMonotonic      = func() time.Duration { return time.Since(catalogMonotonicStart) }
	catalogFetch          = catalogFetchDocument
	catalogGo             = func(f func()) { go f() }
)

// Running catalog, the packaged document until a refresh swaps in a fetched one
func Catalog() []CatalogEntry {
	lrCatalogMu.Lock()
	defer lrCatalogMu.Unlock()
	if !catalogLoaded {
		entries, err := packagedCatalog()
		if err != nil {
			panic(fmt.Sprintf("packaged local runtime catalog: %v", err))
		}
		catalogCurrent, catalogLoaded = entries, true
	}
	return catalogCurrent
}

// Replaces the running catalog
func setCatalog(entries []CatalogEntry) {
	lrCatalogMu.Lock()
	catalogCurrent, catalogLoaded = entries, true
	lrCatalogMu.Unlock()
}

// Catalog parsed from the embedded catalog.json
func packagedCatalog() ([]CatalogEntry, error) {
	return loadCatalogJSON(bytes.TrimPrefix(packagedCatalogJSON, []byte("\xef\xbb\xbf")))
}

// Parses catalog JSON text with Python json semantics and settings kept in document order
func loadCatalogJSON(raw []byte) ([]CatalogEntry, error) {
	doc, ok := moaJSONLoads(string(raw))
	if !ok {
		return nil, errors.New("catalog document is not valid JSON")
	}
	var order struct {
		Models []map[string]json.RawMessage `json:"models"`
	}
	_ = json.Unmarshal(jsonSentinelSafe(raw), &order)
	return loadCatalog(doc, func(i int) []string {
		if i < len(order.Models) {
			return jsonTopLevelKeys(order.Models[i]["sampling"])
		}
		return nil
	})
}

// Python int of a catalog value
func catalogInt(v any) (int64, error) { return growthPyInt(v) }

// Required key of a decoded object, a KeyError when missing and a TypeError when not an object
func catalogKey(m any, key string) (any, error) {
	obj, ok := m.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("'%s' object is not subscriptable", lrPyTypeName(m))
	}
	v, ok := obj[key]
	if !ok {
		return nil, fmt.Errorf("KeyError: %s", pyRepr(key))
	}
	return v, nil
}

// String valued catalog field
func catalogString(v any, key string) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("catalog field %s must be a string, got %s", key, lrPyTypeName(v))
	}
	return s, nil
}

// Optional asset, nil for a falsy value
func catalogAsset(v any) (*AssetFile, error) {
	if !pyTruthy(v) {
		return nil, nil
	}
	rawPath, err := catalogKey(v, "path")
	if err != nil {
		return nil, err
	}
	p, err := catalogString(rawPath, "path")
	if err != nil {
		return nil, err
	}
	rawSize, err := catalogKey(v, "size_bytes")
	if err != nil {
		return nil, err
	}
	size, err := catalogInt(rawSize)
	if err != nil {
		return nil, err
	}
	obj := v.(map[string]any)
	asset := &AssetFile{Path: p, SizeBytes: size, Revision: "main"}
	for key, dst := range map[string]*string{"local": &asset.Local, "repo": &asset.Repo} {
		if raw, ok := obj[key]; ok && raw != nil {
			if *dst, err = catalogString(raw, key); err != nil {
				return nil, err
			}
		}
	}
	if raw, ok := obj["revision"]; ok {
		if asset.Revision, err = catalogString(raw, "revision"); err != nil {
			return nil, err
		}
	}
	return asset, nil
}

// Python float of a catalog value
func catalogFloat(v any) (float64, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case int:
		return float64(t), nil
	case float64:
		return t, nil
	case json.Number:
		f, err := strconv.ParseFloat(t.String(), 64)
		if err != nil || math.IsInf(f, 0) {
			return 0, errors.New("int too large to convert to float")
		}
		return f, nil
	case string:
		text := strings.TrimSpace(t)
		switch strings.ToLower(strings.TrimLeft(text, "+-")) {
		case "inf", "infinity":
			if strings.HasPrefix(text, "-") {
				return math.Inf(-1), nil
			}
			return math.Inf(1), nil
		case "nan":
			return math.NaN(), nil
		}
		f, err := strconv.ParseFloat(strings.ReplaceAll(text, "_", ""), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) || strings.Contains(text, "__") || strings.HasPrefix(text, "_") || strings.HasSuffix(text, "_") {
			return 0, fmt.Errorf("could not convert string to float: %s", pyRepr(t))
		}
		return f, nil
	}
	return 0, fmt.Errorf("float() argument must be a string or a real number, not '%s'", lrPyTypeName(v))
}

// Python dict of a sampling value in insertion order, objects keep their document key order
func catalogSettings(v any, order []string) ([]CatalogSetting, error) {
	switch t := v.(type) {
	case map[string]any:
		var out []CatalogSetting
		seen := map[string]bool{}
		for _, k := range order {
			if val, ok := t[k]; ok && !seen[k] {
				seen[k] = true
				out = append(out, CatalogSetting{k, val})
			}
		}
		for k, val := range t {
			if !seen[k] {
				out = append(out, CatalogSetting{k, val})
			}
		}
		return out, nil
	case []any, string:
		items, err := hfIterate(t)
		if err != nil {
			return nil, err
		}
		var out []CatalogSetting
		index := map[string]int{}
		for i, item := range items {
			pair, err := hfIterate(item)
			if err != nil {
				return nil, fmt.Errorf("cannot convert dictionary update sequence element #%d to a sequence", i)
			}
			if len(pair) != 2 {
				return nil, fmt.Errorf("dictionary update sequence element #%d has length %d; 2 is required", i, len(pair))
			}
			key := pyStr(pair[0])
			if at, ok := index[key]; ok {
				out[at].Value = pair[1]
				continue
			}
			index[key] = len(out)
			out = append(out, CatalogSetting{key, pair[1]})
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", lrPyTypeName(v))
}

// Parses a decoded catalog document, an error for a schema this build does not read
func loadCatalog(doc any, samplingOrder func(int) []string) ([]CatalogEntry, error) {
	obj, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", lrPyTypeName(doc))
	}
	rawVersion, present := obj["schema_version"]
	if !present {
		rawVersion = 0
	}
	version, err := catalogInt(rawVersion)
	if err != nil {
		return nil, err
	}
	if version != catalogSchemaVersion {
		shown := "None"
		if present {
			shown = pyRepr(rawVersion)
		}
		return nil, fmt.Errorf("catalog schema %s (this build reads %d)", shown, catalogSchemaVersion)
	}
	rawModels, err := catalogKey(obj, "models")
	if err != nil {
		return nil, err
	}
	models, err := hfIterate(rawModels)
	if err != nil {
		return nil, err
	}
	var entries []CatalogEntry
	for i, m := range models {
		entry, err := catalogEntryFrom(m, samplingOrder(i))
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// One catalog entry from its decoded object
func catalogEntryFrom(m any, samplingOrder []string) (CatalogEntry, error) {
	var e CatalogEntry
	rawVariants, err := catalogKey(m, "variants")
	if err != nil {
		return e, err
	}
	variants, err := hfIterate(rawVariants)
	if err != nil {
		return e, err
	}
	for _, v := range variants {
		variant, err := catalogVariantFrom(v)
		if err != nil {
			return e, err
		}
		e.Variants = append(e.Variants, variant)
	}
	obj := m.(map[string]any)
	field := func(key string, def any) (any, error) {
		if def == nil {
			return catalogKey(obj, key)
		}
		if v, ok := obj[key]; ok {
			return v, nil
		}
		return def, nil
	}
	ints := []struct {
		key string
		def any
		dst func(int64)
	}{
		{"n_ctx_train", nil, func(n int64) { e.NCtxTrain = int(n) }},
		{"full_layers", nil, func(n int64) { e.FullLayers = int(n) }},
		{"recurrent_layers", nil, func(n int64) { e.RecurrentLayers = int(n) }},
		{"per_layer_f16", nil, func(n int64) { e.PerLayerF16 = n }},
		{"swa_layers", 0, func(n int64) { e.SWALayers = int(n) }},
		{"swa_window", 0, func(n int64) { e.SWAWindow = int(n) }},
		{"mtp_draft_depth", 3, func(n int64) { e.MTPDraftDepth = int(n) }},
		{"n_vocab", 0, func(n int64) { e.NVocab = n }},
		{"quality", 0, func(n int64) { e.Quality = int(n) }},
		{"draft_depth", 3, func(n int64) { e.DraftDepth = int(n) }},
	}
	for _, f := range ints {
		raw, err := field(f.key, f.def)
		if err != nil {
			return e, err
		}
		n, err := catalogInt(raw)
		if err != nil {
			return e, err
		}
		f.dst(n)
	}
	for key, dst := range map[string]*bool{"moe": &e.MoE, "mtp": &e.MTP} {
		raw, _ := field(key, false)
		*dst = pyTruthy(raw)
	}
	rawRecommend, _ := field("recommend", true)
	e.Recommend = pyTruthy(rawRecommend)
	for _, s := range []struct {
		key, def string
		dst      *string
	}{{"min_engine", "", &e.MinEngine}, {"architecture", "", &e.Architecture},
		{"draft_spec_type", "draft-dspark", &e.DraftSpecType}, {"draft_min_engine", "", &e.DraftMinEngine}} {
		raw, _ := field(s.key, s.def)
		*s.dst = pyStr(raw)
	}
	rawFraction, _ := field("decode_fraction", 1.0)
	if e.DecodeFraction, err = catalogFloat(rawFraction); err != nil {
		return e, err
	}
	rawSampling, _ := field("sampling", map[string]any{})
	if e.Sampling, err = catalogSettings(rawSampling, samplingOrder); err != nil {
		return e, err
	}
	for _, s := range []struct {
		key string
		dst *string
	}{{"id", &e.ID}, {"display_name", &e.DisplayName}, {"description", &e.Description}, {"repo", &e.Repo}} {
		raw, err := catalogKey(obj, s.key)
		if err != nil {
			return e, err
		}
		if *s.dst, err = catalogString(raw, s.key); err != nil {
			return e, err
		}
	}
	for key, dst := range map[string]**AssetFile{"mmproj": &e.MMProj, "draft": &e.Draft, "mtp_head": &e.MTPHead} {
		if *dst, err = catalogAsset(obj[key]); err != nil {
			return e, err
		}
	}
	return e, nil
}

// One quant variant from its decoded object
func catalogVariantFrom(v any) (QuantVariant, error) {
	var q QuantVariant
	rawQuant, err := catalogKey(v, "quant")
	if err != nil {
		return q, err
	}
	if q.Quant, err = catalogString(rawQuant, "quant"); err != nil {
		return q, err
	}
	obj := v.(map[string]any)
	q.Validated = pyTruthy(obj["validated"])
	rawFiles, err := catalogKey(obj, "files")
	if err != nil {
		return q, err
	}
	files, err := hfIterate(rawFiles)
	if err != nil {
		return q, err
	}
	for _, f := range files {
		asset, err := catalogAsset(f)
		if err != nil {
			return q, err
		}
		if asset == nil {
			return q, errors.New("catalog variant lists an empty file")
		}
		q.Files = append(q.Files, *asset)
	}
	rawLazy, ok := obj["lazy_bytes"]
	if !ok {
		rawLazy = 0
	}
	q.LazyBytes, err = catalogInt(rawLazy)
	return q, err
}

// GETs the repo catalog with a 10 second timeout
func catalogFetchDocument() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "icebath-local-runtime")
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return io.ReadAll(resp.Body)
}

// Fetches the repo catalog and swaps it in memory, false when throttled or on any failure
func RefreshCatalog(force bool) bool {
	now := catalogMonotonic()
	catalogRefreshMu.Lock()
	if !force && catalogLastAttempt != nil && now-*catalogLastAttempt < catalogRefreshTTL {
		catalogRefreshMu.Unlock()
		return false
	}
	catalogLastAttempt = &now
	catalogRefreshMu.Unlock()
	fetched, err := catalogFetchAndLoad()
	if err != nil {
		slog.Debug(fmt.Sprintf("catalog refresh skipped: %v", err))
		return false
	}
	if !reflect.DeepEqual(fetched, Catalog()) {
		slog.Info(fmt.Sprintf("catalog refreshed from repo (%d models)", len(fetched)))
	}
	setCatalog(fetched)
	return true
}

// Fetched catalog decoded like json.load of the response bytes
func catalogFetchAndLoad() ([]CatalogEntry, error) {
	body, err := catalogFetch()
	if err != nil {
		return nil, err
	}
	text, err := hfJSONText(body)
	if err != nil {
		return nil, err
	}
	return loadCatalogJSON([]byte(text))
}

// Starts a background refresh unless one was attempted within the TTL
func RefreshCatalogSoon() {
	catalogRefreshMu.Lock()
	last := catalogLastAttempt
	catalogRefreshMu.Unlock()
	if last != nil && catalogMonotonic()-*last < catalogRefreshTTL {
		return
	}
	catalogGo(func() { RefreshCatalog(false) })
}

// Running catalog keyed by family id
func CatalogByID() map[string]CatalogEntry {
	out := map[string]CatalogEntry{}
	for _, e := range Catalog() {
		out[e.ID] = e
	}
	return out
}

// Entry and variant owning a staged model id, nil when no catalog build matches
func FindEntryForModel(modelID string) (*CatalogEntry, *QuantVariant) {
	for _, e := range Catalog() {
		for _, v := range e.Variants {
			if v.ModelID() == modelID {
				entry, variant := e, v
				return &entry, &variant
			}
		}
	}
	return nil, nil
}

// Entry owning a staged model id, nil when none does
func EntryForModel(modelID string) *CatalogEntry {
	entry, _ := FindEntryForModel(modelID)
	return entry
}
