package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const zaiProbeFailureTTL = 300 * time.Second

var (
	zaiProbeMu          sync.Mutex
	zaiProbeFailedUntil = map[string]float64{}
)

// zaiEndpoint is one Z.AI billing endpoint with the models its probe tries in order
type zaiEndpoint struct {
	ID          string
	BaseURL     string
	ProbeModels []string
	Label       string
}

var zaiCodingProbeModels = []string{"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5v-turbo", "glm-4.7"}

// Z.AI endpoints in detection priority order
var ZaiEndpoints = []zaiEndpoint{
	{"global", "https://api.z.ai/api/paas/v4", []string{"glm-5"}, "Global"},
	{"cn", "https://open.bigmodel.cn/api/paas/v4", []string{"glm-5"}, "China"},
	{"coding-global", "https://api.z.ai/api/coding/paas/v4", zaiCodingProbeModels, "Global (Coding Plan)"},
	{"coding-cn", "https://open.bigmodel.cn/api/coding/paas/v4", zaiCodingProbeModels, "China (Coding Plan)"},
}

// Client for the Z.AI endpoint probes, swappable so tests aim it at a local server
var zaiHTTPClientFn = func(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: oauthTransport(oauthVerify{TLS: PlatformTLSConfig()})}
}

// Endpoint detection the base URL resolver runs, swappable so tests script the probe outcome
var detectZaiEndpointFn = detectZaiEndpoint

// Probes one Z.AI endpoint with each candidate model in order, nil when none answers 200
func probeSingleZaiEndpoint(ctx context.Context, client *http.Client, apiKey string, ep zaiEndpoint) map[string]string {
	for _, model := range ep.ProbeModels {
		body, _ := json.Marshal(map[string]any{"model": model, "stream": false, "max_tokens": 1,
			"messages": []any{map[string]any{"role": "user", "content": "ping"}}})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			slog.Debug(fmt.Sprintf("Z.AI endpoint probe: %s model=%s failed: %s", ep.ID, model, err))
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			slog.Debug(fmt.Sprintf("Z.AI endpoint probe: %s (%s) model=%s OK", ep.ID, ep.BaseURL, model))
			return map[string]string{"id": ep.ID, "base_url": ep.BaseURL, "model": model, "label": ep.Label}
		}
		slog.Debug(fmt.Sprintf("Z.AI endpoint probe: %s model=%s returned %d", ep.ID, model, resp.StatusCode))
	}
	return nil
}

// Probes every Z.AI endpoint in parallel and returns the highest priority one that works
func detectZaiEndpoint(ctx context.Context, apiKey string, timeout time.Duration) map[string]string {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	endpoints := ZaiEndpoints
	type outcome struct {
		id     string
		result map[string]string
	}
	ch := make(chan outcome, len(endpoints))
	client := zaiHTTPClientFn(timeout)
	for _, ep := range endpoints {
		go func(ep zaiEndpoint) { ch <- outcome{ep.ID, probeSingleZaiEndpoint(ctx, client, apiKey, ep)} }(ep)
	}
	done := map[string]bool{}
	results := map[string]map[string]string{}
	firstReady := func(requireDone bool) map[string]string {
		for _, ep := range endpoints {
			if requireDone && !done[ep.ID] {
				return nil
			}
			if r, ok := results[ep.ID]; ok {
				return r
			}
		}
		return nil
	}
	for range endpoints {
		o := <-ch
		done[o.id] = true
		if o.result != nil {
			results[o.id] = o.result
		}
		if winner := firstReady(true); winner != nil {
			return winner
		}
	}
	return firstReady(false)
}

// Z.AI base URL from the per key cache in auth.json, else a probe that persists its success
func resolveZaiBaseURLDetected(ctx context.Context, h, apiKey, defaultURL string) string {
	sum := sha256.Sum256([]byte(apiKey))
	keyHash := hex.EncodeToString(sum[:])[:16]
	store := NewStore(h)
	if data, err := store.Load(); err == nil {
		cached := anyMap(store.ProviderState(data, "zai")["detected_endpoint"])
		if base := pyOrStr(cached["base_url"]); base != "" && pyStr(tokensOr(cached, "key_hash", "")) == keyHash {
			slog.Debug(fmt.Sprintf("Z.AI: using cached endpoint %s", base))
			return base
		}
	}
	zaiProbeMu.Lock()
	until := zaiProbeFailedUntil[keyHash]
	zaiProbeMu.Unlock()
	if until > nowEpoch() {
		return defaultURL
	}
	detected := detectZaiEndpointFn(ctx, apiKey, 8*time.Second)
	if detected == nil || detected["base_url"] == "" {
		slog.Debug(fmt.Sprintf("Z.AI: probe failed, falling back to default %s", defaultURL))
		zaiProbeMu.Lock()
		zaiProbeFailedUntil[keyHash] = nowEpoch() + zaiProbeFailureTTL.Seconds()
		zaiProbeMu.Unlock()
		return defaultURL
	}
	detectedEndpoint := map[string]any{
		"base_url": detected["base_url"], "endpoint_id": detected["id"], "model": detected["model"],
		"label": detected["label"], "key_hash": keyHash,
	}
	err := func() error {
		unlock, err := store.Lock(AuthLockTimeout)
		if err != nil {
			return err
		}
		defer unlock()
		data, err := store.Load()
		if err != nil {
			return err
		}
		state := cloneMap(store.ProviderState(data, "zai"))
		if state == nil {
			state = map[string]any{}
		}
		state["detected_endpoint"] = detectedEndpoint
		if err := store.StoreProviderState(data, "zai", state, false); err != nil {
			return err
		}
		return store.Save(data)
	}()
	if err != nil {
		store.Log.Warn(fmt.Sprintf("Z.AI: could not persist detected endpoint (%s); will re-probe next start", err))
	}
	store.Log.Info(fmt.Sprintf("Z.AI: auto-detected endpoint %s (%s)", detected["label"], detected["base_url"]))
	return detected["base_url"]
}
