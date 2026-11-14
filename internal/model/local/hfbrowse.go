package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	hfBase               = "https://huggingface.co"
	hfTimeout            = 15 * time.Second
	hfUserAgent          = "icebath-local-models"
	roughKVAndOverhead   = int64(4 << 30)
	hfCacheTTL           = 300 * time.Second
	hfCacheMax           = 128
	FitGPU               = "fits-gpu"
	FitNeedsRAM          = "needs-ram"
	FitTooBig            = "too-big"
	FitUnknown           = "unknown"
	hfSearchLimitMax     = 50
	hfSearchLimitMinimum = 1
)

var (
	hfQuantPattern = regexp.MustCompile(`(?i)(?:IQ|Q)\d[_A-Z0-9]*|F16|BF16|F32`)
	hfSplitPattern = regexp.MustCompile(`(?i)-(\d{5})-of-(\d{5})\.gguf$`)
)

type hfCached struct {
	at   time.Time
	body []byte
}

type HFModelHit struct {
	Repo      string
	Downloads int64
	Likes     int64
	Updated   string
	Gated     bool
}

type HFFileGroup struct {
	Label      string
	Paths      []string
	TotalBytes int64
	Fit        string
}

func (r *Runtime) hfGet(ctx context.Context, rawURL string) ([]byte, error) {
	now := time.Now()
	r.hfMu.Lock()
	if hit, ok := r.hfCache[rawURL]; ok && now.Sub(hit.at) < hfCacheTTL {
		r.hfMu.Unlock()
		return hit.body, nil
	}
	r.hfMu.Unlock()
	reqCtx, cancel := context.WithTimeout(ctx, hfTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", hfUserAgent)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Hugging Face answered HTTP %d for %s", resp.StatusCode, rawURL)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("Hugging Face returned a body that is not JSON for %s", rawURL)
	}
	r.hfMu.Lock()
	if len(r.hfCache) >= hfCacheMax {
		oldest, first := "", true
		for key, hit := range r.hfCache {
			if first || hit.at.Before(r.hfCache[oldest].at) {
				oldest, first = key, false
			}
		}
		delete(r.hfCache, oldest)
	}
	r.hfCache[rawURL] = hfCached{at: now, body: body}
	r.hfMu.Unlock()
	return body, nil
}

func jsonTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

func jsonInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	case bool:
		if t {
			return 1
		}
	}
	return 0
}

func jsonText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func (r *Runtime) SearchHFModels(ctx context.Context, query string, limit int) ([]HFModelHit, error) {
	limit = max(hfSearchLimitMinimum, min(limit, hfSearchLimitMax))
	rawURL := fmt.Sprintf("%s/api/models?search=%s&filter=gguf&sort=downloads&direction=-1&limit=%d",
		hfBase, url.QueryEscape(strings.TrimSpace(query)), limit)
	body, err := r.hfGet(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	var models []map[string]any
	if err := json.Unmarshal(body, &models); err != nil {
		return nil, err
	}
	out := make([]HFModelHit, 0, len(models))
	for _, m := range models {
		out = append(out, HFModelHit{
			Repo: jsonText(m["id"]), Downloads: jsonInt(m["downloads"]), Likes: jsonInt(m["likes"]),
			Updated: jsonText(m["lastModified"]), Gated: jsonTruthy(m["gated"]),
		})
	}
	return out, nil
}

func hfQuantLabel(name string) string {
	if m := hfQuantPattern.FindString(name); m != "" {
		return strings.ToUpper(m)
	}
	return name
}

func escapeRepo(repo string) string {
	parts := strings.Split(repo, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func (r *Runtime) HFRepoFiles(ctx context.Context, repo string) ([]HFFileGroup, error) {
	body, err := r.hfGet(ctx, fmt.Sprintf("%s/api/models/%s/tree/main?recursive=true", hfBase, escapeRepo(repo)))
	if err != nil {
		return nil, err
	}
	var files []map[string]any
	if err := json.Unmarshal(body, &files); err != nil {
		return nil, err
	}
	type part struct {
		index int
		path  string
		size  int64
	}
	var groups []HFFileGroup
	var stems []string
	splits := map[string][]part{}
	for _, f := range files {
		p := jsonText(f["path"])
		if !strings.HasSuffix(strings.ToLower(p), ".gguf") {
			continue
		}
		name := strings.ToLower(p[strings.LastIndexByte(p, '/')+1:])
		if strings.HasPrefix(name, "mmproj") || strings.HasPrefix(name, "dspark") || strings.HasPrefix(name, "mtp-") ||
			strings.Contains(name, "draft") {
			continue
		}
		size := jsonInt(f["size"])
		if m := hfSplitPattern.FindStringSubmatchIndex(p); m != nil {
			stem := p[:m[0]]
			index, _ := strconv.Atoi(p[m[2]:m[3]])
			if _, seen := splits[stem]; !seen {
				stems = append(stems, stem)
			}
			splits[stem] = append(splits[stem], part{index, p, size})
			continue
		}
		groups = append(groups, HFFileGroup{Label: hfQuantLabel(p), Paths: []string{p}, TotalBytes: size, Fit: FitUnknown})
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
		g := HFFileGroup{Label: hfQuantLabel(stem), Fit: FitUnknown}
		for _, p := range parts {
			g.Paths = append(g.Paths, p.path)
			g.TotalBytes += p.size
		}
		groups = append(groups, g)
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].TotalBytes > groups[j].TotalBytes })
	return groups, nil
}

func RoughFit(totalBytes int64, b HardwareBudget) string {
	need := totalBytes + roughKVAndOverhead
	switch {
	case need <= b.UsableVRAMBytes:
		return FitGPU
	case need <= b.UsableVRAMBytes+b.RAMAvailableBytes:
		return FitNeedsRAM
	}
	return FitTooBig
}

func (r *Runtime) PricedHFRepoFiles(ctx context.Context, repo string, b HardwareBudget) ([]HFFileGroup, error) {
	groups, err := r.HFRepoFiles(ctx, repo)
	if err != nil {
		return nil, err
	}
	for i := range groups {
		groups[i].Fit = RoughFit(groups[i].TotalBytes, b)
	}
	return groups, nil
}
