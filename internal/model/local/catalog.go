package local

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/hostinfo"
)

//go:embed catalog.json
var packagedCatalog []byte

const (
	catalogURL           = "https://raw.githubusercontent.com/nickheyer/icebath/main/internal/model/local/catalog.json"
	catalogSchemaVersion = 3
	catalogRefreshTTL    = 6 * time.Hour
	catalogFetchTimeout  = 10 * time.Second
	catalogUserAgent     = "icebath-local-runtime"
	PleasantFloorTokS    = 20.0
	discreteBandwidthGBs = 1000.0
	umaBandwidthGBs      = 210.0
	hostBandwidthGBs     = 80.0
	n1xGPUName           = "NVIDIA RTX Spark N1X"
)

type hardwareKey struct{ platform, backend, gpu string }

type measuredKey struct {
	hardwareKey
	entry, quant string
	depth        int
}

var measuredDecodeTokS = map[measuredKey]float64{
	{hardwareKey{"win32", "cuda", n1xGPUName}, "qwen3.8-27b", "UD-Q4_K_M", 2}: 21.9,
}

var productDefaults = map[hardwareKey]string{
	{"win32", "cuda", n1xGPUName}: "qwen3.8-27b",
}

type AssetFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Local     string `json:"local,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Revision  string `json:"revision,omitempty"`
}

func (a AssetFile) LocalName() string {
	if a.Local != "" {
		return a.Local
	}
	return path.Base(a.Path)
}

type QuantVariant struct {
	Quant     string      `json:"quant"`
	Files     []AssetFile `json:"files"`
	Validated bool        `json:"validated,omitempty"`
	LazyBytes int64       `json:"lazy_bytes,omitempty"`
}

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

func (v QuantVariant) WeightsBytes() int64 { return v.SizeBytes() - v.LazyBytes }

type CatalogEntry struct {
	ID              string
	DisplayName     string
	Description     string
	Repo            string
	Variants        []QuantVariant
	NCtxTrain       int
	FullLayers      int
	RecurrentLayers int
	PerLayerF16     int64
	SWALayers       int
	SWAWindow       int
	MoE             bool
	MTP             bool
	MTPDraftDepth   int
	NVocab          int
	Architecture    string
	MMProj          *AssetFile
	Draft           *AssetFile
	DraftSpecType   string
	DraftDepth      int
	DraftMinEngine  string
	MTPHead         *AssetFile
	Sampling        []INIPair
	MinEngine       string
	Quality         int
	DecodeFraction  float64
	Recommend       bool
}

func (e *CatalogEntry) MTPCapable() bool { return e.MTP || e.MTPHead != nil }

func (e *CatalogEntry) CompanionBytes() int64 {
	var total int64
	for _, a := range []*AssetFile{e.MMProj, e.MTPHead} {
		if a != nil {
			total += a.SizeBytes
		}
	}
	return total
}

func (e *CatalogEntry) Profile(v QuantVariant) ModelProfile {
	layers := make([]Layer, 0, e.FullLayers+e.SWALayers+e.RecurrentLayers)
	for i := 0; i < e.FullLayers; i++ {
		layers = append(layers, Layer{Kind: LayerFull, PerToken: e.PerLayerF16})
	}
	for i := 0; i < e.SWALayers; i++ {
		layers = append(layers, Layer{Kind: LayerSWA, PerToken: e.PerLayerF16})
	}
	for i := 0; i < e.RecurrentLayers; i++ {
		layers = append(layers, Layer{Kind: LayerRecurrent})
	}
	scale := 1.0
	if e.MTPCapable() {
		scale = 1.2
	}
	return ModelProfile{
		Name: v.ModelID(), WeightsBytes: v.WeightsBytes(), NCtxTrain: e.NCtxTrain, Layers: layers,
		SWAWindow: e.SWAWindow, MoE: e.MoE, Architecture: e.Architecture, NVocab: e.NVocab,
		KVScale: scale, LazyBytes: v.LazyBytes,
	}
}

func (e *CatalogEntry) LaunchPlan(v QuantVariant, b HardwareBudget) LaunchPlan {
	return PlanLaunch(AsLoaded(e.Profile(v), b), b, PlanOptions{
		MTPCapable: e.MTPCapable(), FixedOverhead: RuntimeOverheadBytes + e.CompanionBytes(),
	})
}

func (e *CatalogEntry) DownloadFiles(v QuantVariant) []AssetFile {
	out := append([]AssetFile(nil), v.Files...)
	for _, a := range []*AssetFile{e.MMProj, e.Draft, e.MTPHead} {
		if a != nil {
			out = append(out, *a)
		}
	}
	return out
}

func (e *CatalogEntry) DownloadBytes(v QuantVariant) int64 {
	var total int64
	for _, f := range e.DownloadFiles(v) {
		total += f.SizeBytes
	}
	return total
}

func (e *CatalogEntry) InstallFiles(v QuantVariant) []AssetFile {
	out := append([]AssetFile(nil), v.Files...)
	for _, a := range []*AssetFile{e.MMProj, e.MTPHead} {
		if a != nil {
			out = append(out, *a)
		}
	}
	return out
}

type VariantChoice struct {
	Variant   QuantVariant
	ZeroSpill bool
	ReasonKey string
}

func SelectVariant(e *CatalogEntry, b HardwareBudget) *VariantChoice {
	variant := e.Variants[len(e.Variants)-1]
	plan := e.LaunchPlan(variant, b)
	if plan.Refusal != nil {
		return nil
	}
	if plan.Decision.Spilled() {
		return &VariantChoice{Variant: variant, ReasonKey: "smallest-fits-spilled"}
	}
	native := e.NCtxTrain
	if native == 0 {
		native = Floor
	}
	reason := "best-fits"
	if plan.Decision.Window >= min(TargetWindow, native) {
		reason = "best-large-window"
	}
	return &VariantChoice{Variant: variant, ZeroSpill: true, ReasonKey: reason}
}

func budgetHardwareKey(b HardwareBudget, backend string) hardwareKey {
	gpu, _, _ := strings.Cut(b.GPUName, " (")
	if b.GPUPCIID != 0 {
		gpu = ""
		if hostinfo.IsNvidiaN1XPCIID(b.GPUPCIID) {
			gpu = n1xGPUName
		}
	}
	if backend == "auto" && gpu != "" {
		backend = "cuda"
	}
	return hardwareKey{platform: b.Platform, backend: backend, gpu: gpu}
}

func PredictedDecodeTokS(e *CatalogEntry, v QuantVariant, b HardwareBudget, spilled bool, backend string) float64 {
	key := measuredKey{budgetHardwareKey(b, backend), e.ID, v.Quant, e.MTPDraftDepth}
	if measured, ok := measuredDecodeTokS[key]; ok && b.UMA && e.MTP && !spilled {
		return measured
	}
	bandwidth := discreteBandwidthGBs
	switch {
	case spilled:
		bandwidth = hostBandwidthGBs
	case b.UMA:
		bandwidth = umaBandwidthGBs
	}
	return bandwidth * 1e9 / max(1.0, float64(v.SizeBytes())*e.DecodeFraction)
}

func RecommendedEntry(b HardwareBudget, entries []CatalogEntry, backend string) (*CatalogEntry, string) {
	type fit struct {
		entry  *CatalogEntry
		choice *VariantChoice
	}
	var resident []fit
	for i := range entries {
		e := &entries[i]
		if !e.Recommend {
			continue
		}
		if c := SelectVariant(e, b); c != nil && c.ZeroSpill {
			resident = append(resident, fit{e, c})
		}
	}
	productDefault := productDefaults[budgetHardwareKey(b, backend)]
	for _, f := range resident {
		if f.entry.ID == productDefault {
			return f.entry, "product-default"
		}
	}
	var pick *fit
	for i := range resident {
		f := &resident[i]
		if PredictedDecodeTokS(f.entry, f.choice.Variant, b, false, backend) < PleasantFloorTokS {
			continue
		}
		if pick == nil || f.entry.Quality > pick.entry.Quality ||
			(f.entry.Quality == pick.entry.Quality && f.choice.Variant.SizeBytes() < pick.choice.Variant.SizeBytes()) {
			pick = f
		}
	}
	if pick == nil {
		return nil, ""
	}
	for _, f := range resident {
		if f.entry.Quality > pick.entry.Quality {
			return pick.entry, "speed-gated-quality"
		}
	}
	return pick.entry, "best-quality-resident"
}

type catalogDocument struct {
	SchemaVersion *json.Number    `json:"schema_version"`
	Models        []catalogRecord `json:"models"`
}

type catalogRecord struct {
	ID              *string        `json:"id"`
	DisplayName     *string        `json:"display_name"`
	Description     *string        `json:"description"`
	Repo            *string        `json:"repo"`
	Variants        []QuantVariant `json:"variants"`
	NCtxTrain       *int           `json:"n_ctx_train"`
	FullLayers      *int           `json:"full_layers"`
	RecurrentLayers *int           `json:"recurrent_layers"`
	PerLayerF16     *int64         `json:"per_layer_f16"`
	SWALayers       int            `json:"swa_layers"`
	SWAWindow       int            `json:"swa_window"`
	MoE             bool           `json:"moe"`
	MTP             bool           `json:"mtp"`
	MTPDraftDepth   *int           `json:"mtp_draft_depth"`
	NVocab          int            `json:"n_vocab"`
	Sampling        orderedPairs   `json:"sampling"`
	MinEngine       string         `json:"min_engine"`
	Quality         int            `json:"quality"`
	DecodeFraction  *float64       `json:"decode_fraction"`
	Architecture    string         `json:"architecture"`
	Recommend       *bool          `json:"recommend"`
	DraftSpecType   *string        `json:"draft_spec_type"`
	DraftDepth      *int           `json:"draft_depth"`
	DraftMinEngine  string         `json:"draft_min_engine"`
	MMProj          *AssetFile     `json:"mmproj"`
	Draft           *AssetFile     `json:"draft"`
	MTPHead         *AssetFile     `json:"mtp_head"`
}

type orderedPairs []INIPair

func (o *orderedPairs) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		*o = nil
		return nil
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return errors.New("sampling is not an object")
	}
	var out orderedPairs
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		value := string(raw)
		var text string
		if json.Unmarshal(raw, &text) == nil {
			value = text
		}
		key := keyTok.(string)
		replaced := false
		for i := range out {
			if out[i].Key == key {
				out[i].Value, replaced = value, true
			}
		}
		if !replaced {
			out = append(out, INIPair{Key: key, Value: value})
		}
	}
	*o = out
	return nil
}

func normalizeAsset(a *AssetFile) *AssetFile {
	if a == nil || a.Path == "" {
		return nil
	}
	if a.Revision == "" {
		a.Revision = "main"
	}
	return a
}

func parseCatalog(data []byte) ([]CatalogEntry, error) {
	var doc catalogDocument
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &doc); err != nil {
		return nil, err
	}
	schema, version := "None", int64(0)
	if doc.SchemaVersion != nil {
		schema = doc.SchemaVersion.String()
		parsed, err := doc.SchemaVersion.Int64()
		if err != nil {
			return nil, fmt.Errorf("catalog schema %s: %w", schema, err)
		}
		version = parsed
	}
	if version != catalogSchemaVersion {
		return nil, fmt.Errorf("catalog schema %s (this build reads %d)", schema, catalogSchemaVersion)
	}
	entries := make([]CatalogEntry, 0, len(doc.Models))
	for _, m := range doc.Models {
		if m.ID == nil || m.DisplayName == nil || m.Description == nil || m.Repo == nil || m.Variants == nil ||
			m.NCtxTrain == nil || m.FullLayers == nil || m.RecurrentLayers == nil || m.PerLayerF16 == nil {
			return nil, errors.New("catalog model is missing a required field")
		}
		variants := make([]QuantVariant, len(m.Variants))
		for i, v := range m.Variants {
			if len(v.Files) == 0 {
				return nil, fmt.Errorf("catalog model %s has a variant without files", *m.ID)
			}
			for j := range v.Files {
				normalizeAsset(&v.Files[j])
			}
			variants[i] = v
		}
		e := CatalogEntry{
			ID: *m.ID, DisplayName: *m.DisplayName, Description: *m.Description, Repo: *m.Repo, Variants: variants,
			NCtxTrain: *m.NCtxTrain, FullLayers: *m.FullLayers, RecurrentLayers: *m.RecurrentLayers,
			PerLayerF16: *m.PerLayerF16, SWALayers: m.SWALayers, SWAWindow: m.SWAWindow, MoE: m.MoE, MTP: m.MTP,
			MTPDraftDepth: 3, NVocab: m.NVocab, Architecture: m.Architecture, MMProj: normalizeAsset(m.MMProj),
			Draft: normalizeAsset(m.Draft), DraftSpecType: "draft-dspark", DraftDepth: 3, DraftMinEngine: m.DraftMinEngine,
			MTPHead: normalizeAsset(m.MTPHead), Sampling: m.Sampling, MinEngine: m.MinEngine, Quality: m.Quality,
			DecodeFraction: 1.0, Recommend: true,
		}
		if m.MTPDraftDepth != nil {
			e.MTPDraftDepth = *m.MTPDraftDepth
		}
		if m.DecodeFraction != nil {
			e.DecodeFraction = *m.DecodeFraction
		}
		if m.Recommend != nil {
			e.Recommend = *m.Recommend
		}
		if m.DraftSpecType != nil {
			e.DraftSpecType = *m.DraftSpecType
		}
		if m.DraftDepth != nil {
			e.DraftDepth = *m.DraftDepth
		}
		entries = append(entries, e)
	}
	return entries, nil
}

func (r *Runtime) Catalog() []CatalogEntry {
	r.catMu.Lock()
	defer r.catMu.Unlock()
	return r.catalog
}

func (r *Runtime) RefreshCatalog(ctx context.Context, force bool) bool {
	now := time.Now()
	r.catMu.Lock()
	if !force && !r.catTried.IsZero() && now.Sub(r.catTried) < catalogRefreshTTL {
		r.catMu.Unlock()
		return false
	}
	r.catTried = now
	r.catMu.Unlock()
	fetched, err := r.fetchCatalog(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("catalog refresh skipped: %v", err))
		return false
	}
	r.catMu.Lock()
	changed := !reflect.DeepEqual(fetched, r.catalog)
	r.catalog = fetched
	r.catMu.Unlock()
	if changed {
		slog.Info(fmt.Sprintf("catalog refreshed from repo (%d models)", len(fetched)))
	}
	return true
}

func (r *Runtime) fetchCatalog(ctx context.Context) ([]CatalogEntry, error) {
	reqCtx, cancel := context.WithTimeout(ctx, catalogFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", catalogUserAgent)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseCatalog(body)
}

func (r *Runtime) RefreshCatalogSoon() {
	r.catMu.Lock()
	last := r.catTried
	r.catMu.Unlock()
	if !last.IsZero() && time.Since(last) < catalogRefreshTTL {
		return
	}
	r.goBackground(func(ctx context.Context) { r.RefreshCatalog(ctx, false) })
}

func (r *Runtime) CatalogByID() map[string]CatalogEntry {
	out := map[string]CatalogEntry{}
	for _, e := range r.Catalog() {
		out[e.ID] = e
	}
	return out
}

func (r *Runtime) FindEntryForModel(modelID string) (*CatalogEntry, *QuantVariant) {
	entries := r.Catalog()
	for i := range entries {
		for j := range entries[i].Variants {
			if entries[i].Variants[j].ModelID() == modelID {
				return &entries[i], &entries[i].Variants[j]
			}
		}
	}
	return nil, nil
}

func (r *Runtime) EntryForModel(modelID string) *CatalogEntry {
	entry, _ := r.FindEntryForModel(modelID)
	return entry
}
