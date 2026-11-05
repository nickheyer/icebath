package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	hfBase               = "https://huggingface.co"
	hfTimeout            = 15 * time.Second
	hfRoughKVAndOverhead = int64(4) << 30
	hfCacheTTL           = 300 * time.Second
	hfCacheMax           = 128
	hfUserAgent          = "icebath-local-models"
	hfFitUnknown         = "unknown"
)

type hfCacheEntry struct {
	at   time.Time
	data any
}

var (
	hfCacheMu  sync.Mutex
	hfCache    = map[string]hfCacheEntry{}
	hfGetJSON  = hfFetchJSON
	hfQuantRE  = regexp.MustCompile(`(?i)(?:IQ|Q)\p{Nd}[_A-Z0-9]*|F16|BF16|F32`)
	hfSplitRE  = regexp.MustCompile(`(?i)-(\p{Nd}{5})-of-(\p{Nd}{5})\.gguf$`)
	hfNowClock = time.Now
)

// HFModelHit is one Hugging Face repo that ships GGUF files
type HFModelHit struct {
	Repo      string `json:"repo"`
	Downloads int64  `json:"downloads"`
	Likes     int64  `json:"likes"`
	Updated   string `json:"updated"`
	Gated     bool   `json:"gated"`
}

// HFFileGroup is one downloadable quant, a single GGUF or every part of a split one
type HFFileGroup struct {
	Label      string   `json:"label"`
	Paths      []string `json:"paths"`
	TotalBytes int64    `json:"total_bytes"`
	Fit        string   `json:"fit"`
}

// GETs url as JSON through a five minute cache of at most 128 entries that evicts the oldest
func hfFetchJSON(url string) (any, error) {
	now := hfNowClock()
	hfCacheMu.Lock()
	hit, ok := hfCache[url]
	hfCacheMu.Unlock()
	if ok && now.Sub(hit.at) < hfCacheTTL {
		return hit.data, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), hfTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", hfUserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	text, err := hfJSONText(raw)
	if err != nil {
		return nil, err
	}
	data, ok := moaJSONLoads(text)
	if !ok {
		return nil, fmt.Errorf("invalid JSON from %s", url)
	}
	hfCacheMu.Lock()
	defer hfCacheMu.Unlock()
	if len(hfCache) >= hfCacheMax {
		oldest, first := "", true
		for k, e := range hfCache {
			if first || e.at.Before(hfCache[oldest].at) {
				oldest, first = k, false
			}
		}
		delete(hfCache, oldest)
	}
	hfCache[url] = hfCacheEntry{at: now, data: data}
	return data, nil
}

// Percent encodes s as Python urllib quote does with slash kept
func hfQuote(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = pyQuoteAll(p)
	}
	return strings.Join(parts, "/")
}

// Python iteration over a decoded JSON response, objects yield their keys
func hfIterate(v any) ([]any, error) {
	switch t := v.(type) {
	case []any:
		return t, nil
	case map[string]any:
		return loadIterate(t)
	case string:
		return loadIterate(t)
	}
	return nil, fmt.Errorf("'%s' object is not iterable", loadPyTypeName(v))
}

// Python int of a field or zero when falsy, an error where int raises
func hfInt(v any) (int64, error) {
	if !pyTruthy(v) {
		return 0, nil
	}
	if s, ok := v.(string); ok {
		n, ok := pythonUnicodeIntText(s)
		if !ok {
			return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyRepr(s))
		}
		return n, nil
	}
	n, valid, err := loadSlotInt(v)
	if err != nil {
		return 0, err
	}
	if !valid {
		if f, isFloat := v.(float64); isFloat && math.IsNaN(f) {
			return 0, fmt.Errorf("cannot convert float NaN to integer")
		}
		return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", loadPyTypeName(v))
	}
	return n, nil
}

// Python int of a decimal string whose digits may be any Unicode decimal digits
func pythonUnicodeIntText(s string) (int64, bool) {
	var ascii strings.Builder
	for _, r := range s {
		if unicode.IsDigit(r) && r > unicode.MaxASCII {
			ascii.WriteByte(byte('0' + unicodeDigitValue(r)))
			continue
		}
		ascii.WriteRune(r)
	}
	n, ok := pythonIntText(ascii.String())
	return int64(n), ok
}

// Hugging Face models shipping GGUF files that match query, most downloaded first
func SearchModels(query string, limit int) ([]HFModelHit, error) {
	url := fmt.Sprintf("%s/api/models?search=%s&filter=gguf&sort=downloads&direction=-1&limit=%d",
		hfBase, hfQuote(pyStripSpace(query)), max(1, min(limit, 50)))
	data, err := hfGetJSON(url)
	if err != nil {
		return nil, err
	}
	items, err := hfIterate(data)
	if err != nil {
		return nil, err
	}
	hits := make([]HFModelHit, 0, len(items))
	for _, item := range items {
		m, err := loadDict(item)
		if err != nil {
			return nil, err
		}
		repo := ""
		if v, ok := m["id"]; ok {
			repo = pyStr(v)
		}
		downloads, err := hfInt(m["downloads"])
		if err != nil {
			return nil, err
		}
		likes, err := hfInt(m["likes"])
		if err != nil {
			return nil, err
		}
		updated := ""
		if pyTruthy(m["lastModified"]) {
			updated = pyStr(m["lastModified"])
		}
		hits = append(hits, HFModelHit{Repo: repo, Downloads: downloads, Likes: likes, Updated: updated, Gated: pyTruthy(m["gated"])})
	}
	return hits, nil
}

// Uppercased quant token of filename, the filename itself when it names none
func hfQuantLabel(filename string) string {
	if m := hfQuantRE.FindString(filename); m != "" {
		return strings.ToUpper(m)
	}
	return filename
}

type hfSplitPart struct {
	index int64
	path  string
	size  int64
}

// Servable GGUFs of repo with split parts collapsed and companions dropped, largest first
func RepoFiles(repo string) ([]HFFileGroup, error) {
	data, err := hfGetJSON(fmt.Sprintf("%s/api/models/%s/tree/main?recursive=true", hfBase, hfQuote(repo)))
	if err != nil {
		return nil, err
	}
	files, err := hfIterate(data)
	if err != nil {
		return nil, err
	}
	var groups []HFFileGroup
	var stems []string
	splits := map[string][]hfSplitPart{}
	for _, item := range files {
		f, err := loadDict(item)
		if err != nil {
			return nil, err
		}
		path := ""
		if v, ok := f["path"]; ok {
			path = pyStr(v)
		}
		if !strings.HasSuffix(strings.ToLower(path), ".gguf") {
			continue
		}
		name := strings.ToLower(path[strings.LastIndex(path, "/")+1:])
		if strings.HasPrefix(name, "mmproj") || strings.HasPrefix(name, "dspark") || strings.HasPrefix(name, "mtp-") ||
			strings.Contains(name, "draft") {
			continue
		}
		size, err := hfInt(f["size"])
		if err != nil {
			return nil, err
		}
		if loc := hfSplitRE.FindStringSubmatchIndex(path); loc != nil {
			index, _ := pythonUnicodeIntText(path[loc[2]:loc[3]])
			stem := path[:loc[0]]
			if _, seen := splits[stem]; !seen {
				stems = append(stems, stem)
			}
			splits[stem] = append(splits[stem], hfSplitPart{index: index, path: path, size: size})
			continue
		}
		groups = append(groups, HFFileGroup{Label: hfQuantLabel(path), Paths: []string{path}, TotalBytes: size, Fit: hfFitUnknown})
	}
	for _, stem := range stems {
		parts := splits[stem]
		sort.Slice(parts, func(i, j int) bool {
			if parts[i].index != parts[j].index {
				return parts[i].index < parts[j].index
			}
			if parts[i].path != parts[j].path {
				return parts[i].path < parts[j].path
			}
			return parts[i].size < parts[j].size
		})
		group := HFFileGroup{Label: hfQuantLabel(stem), Fit: hfFitUnknown}
		for _, p := range parts {
			group.Paths = append(group.Paths, p.path)
			group.TotalBytes += p.size
		}
		groups = append(groups, group)
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].TotalBytes > groups[j].TotalBytes })
	return groups, nil
}

// Coarse pre download verdict from file size alone, fits-gpu, needs-ram or too-big
func RoughFit(totalBytes int64, budget HardwareBudget) string {
	need := totalBytes + hfRoughKVAndOverhead
	if need <= budget.UsableVRAMBytes {
		return "fits-gpu"
	}
	if need <= budget.UsableVRAMBytes+budget.RAMAvailableBytes {
		return "needs-ram"
	}
	return "too-big"
}

// Repo files each priced against budget
func PricedRepoFiles(repo string, budget HardwareBudget) ([]HFFileGroup, error) {
	groups, err := RepoFiles(repo)
	if err != nil {
		return nil, err
	}
	for i := range groups {
		groups[i].Fit = RoughFit(groups[i].TotalBytes, budget)
	}
	return groups, nil
}

// Decodes a JSON body in the encoding Python json detects from its BOM or leading NUL bytes
func hfJSONText(b []byte) (string, error) {
	encoding := "utf-8"
	switch {
	case bytes.HasPrefix(b, []byte{0, 0, 0xFE, 0xFF}):
		encoding, b = "utf-32-be", b[4:]
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE, 0, 0}):
		encoding, b = "utf-32-le", b[4:]
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		encoding, b = "utf-16-be", b[2:]
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		encoding, b = "utf-16-le", b[2:]
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		b = b[3:]
	case len(b) >= 4 && b[0] == 0 && b[1] != 0:
		encoding = "utf-16-be"
	case len(b) >= 4 && b[0] == 0:
		encoding = "utf-32-be"
	case len(b) >= 4 && b[1] == 0 && (b[2] != 0 || b[3] != 0):
		encoding = "utf-16-le"
	case len(b) >= 4 && b[1] == 0:
		encoding = "utf-32-le"
	case len(b) == 2 && b[0] == 0:
		encoding = "utf-16-be"
	case len(b) == 2 && b[1] == 0:
		encoding = "utf-16-le"
	}
	switch encoding {
	case "utf-8":
		if !utf8.Valid(b) {
			return "", fmt.Errorf("'utf-8' codec can't decode the response body")
		}
		return string(b), nil
	case "utf-16-be", "utf-16-le":
		if len(b)%2 != 0 {
			return "", fmt.Errorf("'%s' codec can't decode byte: truncated data", encoding)
		}
		units := make([]uint16, len(b)/2)
		for i := range units {
			if encoding == "utf-16-be" {
				units[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
			} else {
				units[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
			}
		}
		return string(utf16.Decode(units)), nil
	}
	if len(b)%4 != 0 {
		return "", fmt.Errorf("'%s' codec can't decode bytes: truncated data", encoding)
	}
	var sb strings.Builder
	for i := 0; i < len(b); i += 4 {
		var r uint32
		if encoding == "utf-32-be" {
			r = uint32(b[i])<<24 | uint32(b[i+1])<<16 | uint32(b[i+2])<<8 | uint32(b[i+3])
		} else {
			r = uint32(b[i+3])<<24 | uint32(b[i+2])<<16 | uint32(b[i+1])<<8 | uint32(b[i])
		}
		if r > unicode.MaxRune {
			return "", fmt.Errorf("'%s' codec can't decode bytes: code point not in range(0x110000)", encoding)
		}
		sb.WriteRune(rune(r))
	}
	return sb.String(), nil
}
