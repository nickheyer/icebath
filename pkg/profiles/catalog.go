package profiles

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Dollars per million tokens of each usage kind, zero where the endpoint charges nothing or the rate is unknown
type Pricing struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// Dollars the usage cost at these rates, reasoning tokens counting as output already
func (p Pricing) Cost(u protocol.Usage) float64 {
	const million = 1e6
	return (float64(u.Input)*p.Input + float64(u.Output)*p.Output + float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite) / million
}

// Reports whether any rate is set
func (p Pricing) Known() bool { return p != Pricing{} }

// What a model accepts on the request side
type Capabilities struct {
	Tools     bool `json:"tools"`
	Vision    bool `json:"vision"`
	Thinking  bool `json:"thinking"`
	Documents bool `json:"documents"`
}

//go:embed catalog.json
var catalogJSON []byte

type catalogFile struct {
	Updated string  `json:"updated"`
	Models  []Model `json:"models"`
}

var (
	catalogOnce sync.Once
	catalog     catalogFile
	catalogMu   sync.RWMutex
	extra       []Model
)

func loadCatalog() {
	catalogOnce.Do(func() {
		if err := json.Unmarshal(catalogJSON, &catalog); err != nil {
			panic("profiles: embedded catalog: " + err.Error())
		}
	})
}

// Date the built in catalog rates were last checked
func CatalogUpdated() string {
	loadCatalog()
	return catalog.Updated
}

// Every model the built in catalog and any added lists describe, added entries first
func Catalog() []Model {
	loadCatalog()
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	out := make([]Model, 0, len(extra)+len(catalog.Models))
	out = append(out, extra...)
	out = append(out, catalog.Models...)
	return out
}

// Adds models whose metadata wins over the built in catalog for the same id
func AddCatalog(models ...Model) {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	extra = append(append([]Model(nil), models...), extra...)
}

// Reads a catalog JSON file, a list or an object with a models list, and adds it
func LoadCatalog(r io.Reader) ([]Model, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var list []Model
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return nil, fmt.Errorf("profiles: catalog: %w", err)
		}
	} else {
		var f catalogFile
		if err := json.Unmarshal([]byte(trimmed), &f); err != nil {
			return nil, fmt.Errorf("profiles: catalog: %w", err)
		}
		list = f.Models
	}
	for i, m := range list {
		if m.ID == "" {
			return nil, fmt.Errorf("profiles: catalog entry %d has no id", i)
		}
	}
	AddCatalog(list...)
	return list, nil
}

func LoadCatalogFile(path string) ([]Model, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return LoadCatalog(f)
}

var (
	datedSuffix = regexp.MustCompile(`-(20\d{6})(-v\d+(:\d+)?)?$`)
	versionTail = regexp.MustCompile(`(@\d{8}|-v\d+:\d+|-latest|-preview)$`)
	routePrefix = regexp.MustCompile(`^(us|eu|apac|global|jp|au|ca)\.`)
	vendorHead  = regexp.MustCompile(`^(anthropic|openai|google|mistralai|mistral|meta|amazon|bedrock|vertex)[./]`)
)

// Reduces a vendor or router specific model id to the bare id the catalog keys on
func Normalize(id string) string {
	id = strings.TrimSpace(strings.ToLower(id))
	id = routePrefix.ReplaceAllString(id, "")
	id = vendorHead.ReplaceAllString(id, "")
	id = strings.TrimPrefix(id, "models/")
	id = versionTail.ReplaceAllString(id, "")
	id = datedSuffix.ReplaceAllString(id, "")
	id = strings.ReplaceAll(id, ".", "-")
	id = strings.TrimSuffix(id, "-0")
	return id
}

// Catalog entry for a model id in any vendor spelling, matching the longest known id the normalized form starts with
func Describe(id string) (Model, bool) {
	want := Normalize(id)
	if want == "" {
		return Model{}, false
	}
	best, found := Model{}, false
	for _, m := range Catalog() {
		key := Normalize(m.ID)
		if key == want {
			return m, true
		}
		if strings.HasPrefix(want, key+"-") && len(key) > len(Normalize(best.ID)) {
			best, found = m, true
		}
	}
	return best, found
}

// Fills the context, output limit, pricing and capabilities a listing left blank from the catalog
func Enrich(models []Model) []Model {
	out := make([]Model, len(models))
	for i, m := range models {
		known, ok := Describe(m.ID)
		if !ok {
			out[i] = m
			continue
		}
		if m.Name == "" {
			m.Name = known.Name
		}
		if m.Context == 0 {
			m.Context = known.Context
		}
		if m.MaxOutput == 0 {
			m.MaxOutput = known.MaxOutput
		}
		if m.Pricing == nil {
			m.Pricing = known.Pricing
		}
		if m.Capabilities == nil {
			m.Capabilities = known.Capabilities
		}
		out[i] = m
	}
	return out
}
