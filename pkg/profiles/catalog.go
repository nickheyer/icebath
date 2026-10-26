package profiles

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	routePrefixes = []string{"us", "eu", "apac", "global", "jp", "au", "ca"}
	vendorHeads   = []string{"anthropic", "openai", "google", "mistralai", "mistral", "meta", "amazon", "bedrock", "vertex"}
)

// Reduces a vendor or router specific model id to the bare id the catalog keys on
func Normalize(id string) string {
	id = strings.TrimSpace(strings.ToLower(id))
	id = trimRoute(id)
	id = trimVendor(id)
	id = strings.TrimPrefix(id, "models/")
	id = trimVersion(id)
	id = trimDate(id)
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

// Drops a leading region route such as us. or global.
func trimRoute(id string) string {
	for _, p := range routePrefixes {
		if strings.HasPrefix(id, p+".") {
			return id[len(p)+1:]
		}
	}
	return id
}

// Drops a leading vendor name followed by a dot or slash
func trimVendor(id string) string {
	for _, v := range vendorHeads {
		if strings.HasPrefix(id, v+".") || strings.HasPrefix(id, v+"/") {
			return id[len(v)+1:]
		}
	}
	return id
}

// Drops one trailing snapshot marker, an @ date, a -vN:M version, -latest or -preview
func trimVersion(id string) string {
	for _, tail := range []string{"-latest", "-preview"} {
		if strings.HasSuffix(id, tail) {
			return id[:len(id)-len(tail)]
		}
	}
	if at := strings.LastIndexByte(id, '@'); at >= 0 && allDigits(id[at+1:]) && len(id)-at-1 == 8 {
		return id[:at]
	}
	if v := strings.LastIndex(id, "-v"); v >= 0 && versionPair(id[v+2:]) {
		return id[:v]
	}
	return id
}

// Drops a trailing -20YYMMDD date, with any -vN or -vN:M after it
func trimDate(id string) string {
	for i := 0; i < len(id); i++ {
		if id[i] == '-' && datedTail(id[i+1:]) {
			return id[:i]
		}
	}
	return id
}

// Reports whether s is a 20YYMMDD date alone or followed by -vN or -vN:M
func datedTail(s string) bool {
	if len(s) < 8 || !strings.HasPrefix(s, "20") || !allDigits(s[:8]) {
		return false
	}
	rest := s[8:]
	if rest == "" {
		return true
	}
	if !strings.HasPrefix(rest, "-v") {
		return false
	}
	rest = rest[2:]
	n, m, hasColon := strings.Cut(rest, ":")
	if !allDigits(n) {
		return false
	}
	return !hasColon || allDigits(m)
}

// Reports whether s is digits, a colon and digits
func versionPair(s string) bool {
	n, m, ok := strings.Cut(s, ":")
	return ok && allDigits(n) && allDigits(m)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
