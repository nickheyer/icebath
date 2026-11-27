package models

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"maps"
	"math/big"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// What a models listing says of one model
type Row struct {
	Name      string `json:"name"`
	Context   int    `json:"context_length,omitempty"`
	MaxOutput int    `json:"max_completion_tokens,omitempty"`
	// USD a token as the listing gives it, by prompt, completion, cache_read and cache_write, and USD a request
	Pricing map[string]string `json:"pricing,omitempty"`
	// Request fields the listing says the model takes, nil when it says nothing of them; the reasoning efforts it lists, and whether its
	// reasoning can't be turned off
	Parameters []string `json:"supported_parameters"`
	Efforts    []string `json:"supported_efforts,omitempty"`
	Mandatory  bool     `json:"mandatory,omitempty"`
}

// Price of the row for a million tokens, false when its listing gives none
func (r Row) Price() (provider.Price, bool) {
	million := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := new(big.Rat).SetString(strings.TrimSpace(r.Pricing[key])); ok && r.Pricing[key] != "" {
				return value.Mul(value, big.NewRat(1_000_000, 1)).FloatString(12)
			}
		}
		return ""
	}
	price := provider.Price{Input: million("prompt"), Output: million("completion"), CacheRead: million("cache_read", "cached_prompt", "input_cache_read"),
		CacheWrite: million("cache_write", "cache_creation", "input_cache_write")}
	if request, ok := new(big.Rat).SetString(strings.TrimSpace(r.Pricing["request"])); ok && r.Pricing["request"] != "" {
		price.Request = request.FloatString(12)
	}
	if price.Input == "" && price.Output == "" && price.Request == "" {
		return provider.Price{}, false
	}
	return price, true
}

// Keys a listing may give a model's context window under, and its most output
var (
	contextKeys = []string{"context_length", "context_window", "context_size", "max_context_length", "max_position_embeddings", "max_model_len",
		"max_input_tokens", "max_sequence_length", "max_seq_len", "n_ctx_train", "n_ctx", "ctx_size"}
	outputKeys = []string{"max_completion_tokens", "max_output_tokens", "max_tokens"}
)

// How long a remote endpoint's listing is used before it is read again, here and on disk
const endpointTTL = 5 * time.Minute

type endpointKey struct{ url, key string }

type endpointRows struct {
	rows map[string]Row
	at   time.Time
}

const endpointCache = "endpoint_model_metadata.json"

// Models an OpenAI style endpoint lists with what it says of each, read at the base and its v1 twin and kept five minutes for each address
// and key; LM Studio's own listing and llama.cpp's loaded windows are read on servers of this machine. Empty when nothing answers
func (m *Models) Endpoint(ctx context.Context, cfg *config.Config, base, key string) map[string]Row {
	normalized := strings.TrimRight(strings.TrimSpace(base), "/")
	if normalized == "" || provider.BaseHostMatches(normalized, "openrouter.ai") {
		return map[string]Row{}
	}
	s := m.of(cfg)
	local := Local(normalized)
	memo := endpointKey{normalized, fingerprint(key)}
	s.mu.Lock()
	held, ok := s.endpoints[memo]
	s.mu.Unlock()
	if ok && time.Since(held.at) < endpointTTL {
		return held.rows
	}
	if !local {
		if rows, ok := s.endpointDisk(normalized); ok {
			return s.rememberEndpoint(memo, rows)
		}
	}
	if s.blackholedHost(normalized) {
		return map[string]Row{}
	}
	headers := bearer(key)
	if local && m.ServerType(ctx, cfg, normalized, key) == "lm-studio" {
		if rows, err := lmstudioRows(ctx, normalized, headers); err == nil {
			return s.rememberEndpoint(memo, rows)
		} else if connectTimeout(err) {
			s.blackhole(normalized)
		}
	}
	candidates := []string{normalized}
	if twin := strings.TrimRight(strings.TrimSuffix(normalized, "/v1"), "/"); twin != normalized {
		candidates = append(candidates, twin)
	} else {
		candidates = append(candidates, normalized+"/v1")
	}
	var failed error
	for _, candidate := range candidates {
		if s.blackholedHost(normalized) {
			break
		}
		root := ipv4(candidate)
		reply, err := send(ctx, "GET", root+"/models", headers, nil, 10*time.Second, true)
		if err == nil && (reply.status == 401 || reply.status == 403) {
			slog.Debug("model metadata probe rejected, stopping candidate probing", "status", reply.status, "url", root+"/models")
			break
		}
		if err == nil && !reply.ok() {
			err = &StatusError{Status: reply.status, URL: root + "/models"}
		}
		if err != nil {
			failed = err
			if connectTimeout(err) {
				s.blackhole(normalized)
			}
			continue
		}
		payload := reply.object()
		rows := listingRows(payload)
		data, _ := payload["data"].([]any)
		for _, item := range data {
			if model, ok := item.(map[string]any); ok && text(model["owned_by"]) == "llamacpp" {
				llamacppWindows(ctx, rows, root, headers)
				break
			}
		}
		if len(rows) > 0 && !local {
			s.saveEndpoint(normalized, rows)
		}
		return s.rememberEndpoint(memo, rows)
	}
	if failed != nil {
		slog.Debug("failed to fetch model metadata", "url", normalized+"/models", "error", failed)
	}
	return s.rememberEndpoint(memo, map[string]Row{})
}

// Keeps a listing in memory, dropping those past their time
func (s *state) rememberEndpoint(key endpointKey, rows map[string]Row) map[string]Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	for other, held := range s.endpoints {
		if time.Since(held.at) >= endpointTTL {
			delete(s.endpoints, other)
		}
	}
	s.endpoints[key] = endpointRows{rows: rows, at: time.Now()}
	return rows
}

// Listing of a remote endpoint another process read in the last five minutes
func (s *state) endpointDisk(normalized string) (map[string]Row, bool) {
	entry, ok := readJSON(s.file(endpointCache))[normalized].(map[string]any)
	if !ok {
		return nil, false
	}
	at, ok := entry["at"].(float64)
	if !ok || float64(time.Now().Unix())-at >= endpointTTL.Seconds() {
		return nil, false
	}
	raw, err := json.Marshal(entry["models"])
	var rows map[string]Row
	if err != nil || json.Unmarshal(raw, &rows) != nil || rows == nil {
		return nil, false
	}
	return rows, true
}

// Writes a listing for other processes, dropping the listings past their time
func (s *state) saveEndpoint(normalized string, rows map[string]Row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.file(endpointCache)
	data := map[string]any{}
	now := float64(time.Now().UnixNano()) / 1e9
	for key, value := range readJSON(path) {
		if entry, ok := value.(map[string]any); ok {
			if at, ok := entry["at"].(float64); ok && now-at < endpointTTL.Seconds() {
				data[key] = entry
			}
		}
	}
	data[normalized] = map[string]any{"at": now, "models": rows}
	if err := writeJSON(path, data); err != nil {
		slog.Debug("failed to save endpoint model metadata disk cache", "error", err)
	}
}

// Short non secret digest of a key, empty for none
func fingerprint(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// Rows of an OpenAI style listing, by id and by the bare name of a vendor prefixed one
func listingRows(payload map[string]any) map[string]Row {
	rows := map[string]Row{}
	data, _ := payload["data"].([]any)
	for _, item := range data {
		model, ok := item.(map[string]any)
		if id := text(model["id"]); ok && id != "" {
			n, _ := firstInt(model, contextKeys)
			alias(rows, id, row(model, id, n))
		}
	}
	return rows
}

// Row of one listed model, with the window it was found to have
func row(model map[string]any, id string, window int) Row {
	r := Row{Name: cmp.Or(text(model["name"]), id), Context: window, Pricing: listedPrices(model)}
	r.MaxOutput, _ = firstInt(model, outputKeys)
	return r
}

// Keeps a row under its id, and under its name without the vendor when that is free
func alias(rows map[string]Row, id string, r Row) {
	rows[id] = r
	if _, bare, ok := strings.Cut(id, "/"); ok {
		if _, taken := rows[bare]; !taken {
			rows[bare] = r
		}
	}
}

// Rows of LM Studio's own listing, by key and id, each with the window its loaded instance runs
func lmstudioRows(ctx context.Context, normalized string, headers map[string]string) (map[string]Row, error) {
	target := LMStudioRoot(normalized) + "/api/v1/models"
	reply, err := send(ctx, "GET", target, headers, nil, 10*time.Second, true)
	if err != nil {
		return nil, err
	}
	if !reply.ok() {
		return nil, &StatusError{Status: reply.status, URL: target}
	}
	rows := map[string]Row{}
	models, _ := reply.object()["models"].([]any)
	for _, item := range models {
		model, ok := item.(map[string]any)
		id := cmp.Or(text(model["key"]), text(model["id"]))
		if !ok || id == "" {
			continue
		}
		r := row(model, id, loadedWindow(model))
		alias(rows, id, r)
		if other := text(model["id"]); other != "" && other != id {
			alias(rows, other, r)
		}
	}
	return rows, nil
}

// Window of the first loaded instance of an LM Studio model, 0 when none is loaded
func loadedWindow(model map[string]any) int {
	instances, _ := model["loaded_instances"].([]any)
	for _, item := range instances {
		instance, _ := item.(map[string]any)
		settings, _ := instance["config"].(map[string]any)
		if n := positive(settings["context_length"]); n > 0 {
			return n
		}
	}
	return 0
}

// Windows llama.cpp allocated for the models it has loaded, over what its listing says
func llamacppWindows(ctx context.Context, rows map[string]Row, root string, headers map[string]string) {
	base := strings.ReplaceAll(strings.TrimRight(root, "/"), "/v1", "")
	props := func(query url.Values) (*answer, error) {
		suffix := ""
		if len(query) > 0 {
			suffix = "?" + query.Encode()
		}
		reply, err := send(ctx, "GET", base+"/v1/props"+suffix, headers, nil, 10*time.Second, true)
		if err == nil && !reply.ok() {
			return send(ctx, "GET", base+"/props"+suffix, headers, nil, 10*time.Second, true)
		}
		return reply, err
	}
	allocated := func(reply *answer) int {
		settings, _ := reply.object()["default_generation_settings"].(map[string]any)
		return positive(settings["n_ctx"])
	}
	reply, err := props(nil)
	if err != nil {
		return
	}
	if reply.ok() {
		served := text(reply.object()["model_alias"])
		if r, ok := rows[served]; ok && allocated(reply) > 0 {
			r.Context = allocated(reply)
			rows[served] = r
		}
		return
	}
	listed, err := send(ctx, "GET", base+"/models", headers, nil, 10*time.Second, true)
	if err != nil || !listed.ok() {
		return
	}
	data, _ := listed.object()["data"].([]any)
	for _, item := range data[:min(len(data), 16)] {
		child, _ := item.(map[string]any)
		id := text(child["id"])
		status, _ := child["status"].(map[string]any)
		r, known := rows[id]
		if value := text(status["value"]); id == "" || !known || value != "loaded" && value != "ready" {
			continue
		}
		reply, err := props(url.Values{"model": {id}, "autoload": {"false"}})
		if err != nil || !reply.ok() {
			continue
		}
		if n := allocated(reply); n > 0 {
			r.Context = n
			rows[id] = r
		}
	}
}

// Context window an endpoint's listing gives the model: its row, the only row, or one whose id holds the model's or is held in it
func (m *Models) endpointContext(ctx context.Context, cfg *config.Config, model, base, key string) (int, bool) {
	rows := m.Endpoint(ctx, cfg, base, key)
	r, ok := rows[model]
	if !ok && len(rows) == 1 {
		for _, only := range rows {
			r, ok = only, true
		}
	} else if !ok && model != "" {
		for _, id := range slices.Sorted(maps.Keys(rows)) {
			if strings.Contains(id, model) || strings.Contains(model, id) {
				r, ok = rows[id], true
				break
			}
		}
	}
	return r.Context, ok && r.Context > 0
}

// Positive whole number of a decoded JSON number, 0 for anything else
func positive(v any) int {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case bool:
		if n {
			return 1
		}
	}
	return 0
}

// Whole number in the bounds a window can have, from a number or its text
func plausible(v any) (int, bool) {
	var n int
	switch t := v.(type) {
	case float64:
		n = int(t)
	case string:
		parsed, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(t), ",", ""))
		if err != nil {
			return 0, false
		}
		n = parsed
	default:
		return 0, false
	}
	return n, n >= 1024 && n <= 10_000_000
}

// First plausible number under any of the keys anywhere in the payload, nested objects walked depth first in key order
func firstInt(payload map[string]any, keys []string) (int, bool) {
	found, ok := 0, false
	walk(payload, func(object map[string]any) bool {
		for _, key := range slices.Sorted(maps.Keys(object)) {
			if slices.Contains(keys, strings.ToLower(key)) {
				if n, good := plausible(object[key]); good {
					found, ok = n, true
					return false
				}
			}
		}
		return true
	})
	return found, ok
}

// Visits every object nested in a value depth first, stopping when the visit says so
func walk(value any, visit func(map[string]any) bool) bool {
	switch v := value.(type) {
	case map[string]any:
		if !visit(v) {
			return false
		}
		for _, key := range slices.Sorted(maps.Keys(v)) {
			if !walk(v[key], visit) {
				return false
			}
		}
	case []any:
		for _, item := range v {
			if !walk(item, visit) {
				return false
			}
		}
	}
	return true
}

// Context window of a model object: its window keys at the top, else its max_tokens
func payloadWindow(payload map[string]any) (int, bool) {
	for _, key := range contextKeys {
		if n, ok := plausible(payload[key]); ok {
			return n, true
		}
	}
	if n := positive(payload["max_tokens"]); n > 0 {
		return n, true
	}
	return 0, false
}

// Divisors of the units a listing may price in
var priceUnits = map[string]float64{"per_token": 1, "per_1k_tokens": 1_000, "per_thousand_tokens": 1_000, "per_1m_tokens": 1_000_000,
	"per_million_tokens": 1_000_000}

// Names a listing may give each price under
var priceNames = []struct {
	field   string
	aliases []string
}{
	{"prompt", []string{"prompt", "input", "input_cost_per_token", "prompt_token_cost"}},
	{"completion", []string{"completion", "output", "output_cost_per_token", "completion_token_cost"}},
	{"request", []string{"request", "request_cost"}},
	{"cache_read", []string{"cache_read", "cached_prompt", "input_cache_read", "cache_read_cost_per_token"}},
	{"cache_write", []string{"cache_write", "cache_creation", "input_cache_write", "cache_write_cost_per_token"}},
}

// Prices of a listed model a token, from NovitaAI's and DeepInfra's own fields first, then the first object anywhere naming any price
func listedPrices(model map[string]any) map[string]string {
	perToken := func(source map[string]any, fields [][2]string, divisor float64) map[string]string {
		out := map[string]string{}
		for _, field := range fields {
			if n, ok := number(source[field[1]]); ok {
				out[field[0]] = strconv.FormatFloat(n/divisor, 'g', -1, 64)
			}
		}
		return out
	}
	novita := [][2]string{{"prompt", "input_token_price_per_m"}, {"completion", "output_token_price_per_m"}}
	for _, field := range novita {
		if model[field[1]] != nil {
			return perToken(model, novita, 10_000*1_000_000)
		}
	}
	if metadata, ok := model["metadata"].(map[string]any); ok {
		if listed, ok := metadata["pricing"].(map[string]any); ok {
			deepinfra := [][2]string{{"prompt", "input_tokens"}, {"completion", "output_tokens"}, {"cache_read", "cache_read_tokens"}}
			for _, field := range deepinfra {
				if _, has := listed[field[1]]; has {
					return perToken(listed, deepinfra, 1_000_000)
				}
			}
		}
	}
	var result map[string]string
	walk(model, func(object map[string]any) bool {
		lower := map[string]any{}
		for key, value := range object {
			lower[strings.ToLower(key)] = value
		}
		rates := map[string]float64{}
		for _, name := range priceNames {
			for _, alias := range name.aliases {
				if v, ok := lower[alias]; ok && v != nil && v != "" {
					if n, ok := number(v); ok {
						rates[name.field] = n
					}
					break
				}
			}
		}
		if len(rates) == 0 {
			return true
		}
		divisor, known := priceUnits[strings.ToLower(strings.TrimSpace(text(lower["unit"])))]
		if !known {
			// A quote of a tenth of a cent or more can't be a token's price: it is for a million
			divisor = 1
			for _, v := range rates {
				if v >= 0.001 {
					divisor = 1_000_000
					break
				}
			}
		}
		result = map[string]string{}
		for field, v := range rates {
			result[field] = strconv.FormatFloat(v/divisor, 'g', -1, 64)
		}
		return false
	})
	return result
}

// Text of a decoded JSON scalar, a number written out in full; empty for anything else
func Scalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// Number of a decoded value or its text
func number(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case string:
		n, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return n, err == nil
	}
	return 0, false
}
