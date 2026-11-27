package credentials

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	zaiconfig "github.com/nickheyer/icebath/internal/plugins/defaults/modelproviders/zai/config"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// One of Z.AI's billing endpoints, with the models its probe tries in order
type endpoint struct {
	id, base, label string
	models          []string
}

var codingModels = []string{"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5v-turbo", "glm-4.7"}

// Z.AI's endpoints in the order a key that works on several is given the first
var endpoints = []endpoint{
	{"global", "https://api.z.ai/api/paas/v4", "Global", []string{"glm-5"}},
	{"cn", "https://open.bigmodel.cn/api/paas/v4", "China", []string{"glm-5"}},
	{"coding-global", "https://api.z.ai/api/coding/paas/v4", "Global (Coding Plan)", codingModels},
	{"coding-cn", "https://open.bigmodel.cn/api/coding/paas/v4", "China (Coding Plan)", codingModels},
}

// How long a key no endpoint took is not probed again, and how long one probe may take
const (
	probeCooldown = 5 * time.Minute
	probeWait     = 8 * time.Second
)

// A Z.AI key, sent to the endpoint it belongs to: the one a probe of every endpoint finds, kept by the key's hash, unless the provider is set
// to an address of its own
type source struct {
	keyed credentials.Keyed
	path  string
	mu    sync.Mutex
	found map[string]string
	// When a probe of each key found no endpoint
	failed map[string]time.Time
}

func Register(registry *credentials.Registry, cfg *config.Config) {
	registry.Register(&source{keyed: credentials.Keyed{Provider: zaiconfig.Name, Variables: []string{zaiconfig.APIKeyEnv, zaiconfig.ZAIAPIKeyEnv,
		zaiconfig.ZDotAIAPIKeyEnv}}, path: filepath.Join(cfg.Dir(), "cache", "zai_endpoints.json"), failed: map[string]time.Time{}})
}

func (s *source) Credential(account credentials.Account) (credentials.Credential, bool) {
	credential, ok := s.keyed.Credential(account)
	if !ok {
		return credential, false
	}
	if base := strings.TrimSpace(account.Provider.Settings.GetString(providerconfig.BaseURL)); base != "" && base != zaiconfig.BaseURL {
		return credential, true
	}
	credential.BaseURL = s.endpoint(credential.APIKey)
	return credential, true
}

// Endpoints found for the keys, read from the file once
func (s *source) load() {
	if s.found != nil {
		return
	}
	s.found = map[string]string{}
	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, &s.found)
	}
}

// Endpoint the key belongs to: the one found before, else a probe's, Z.AI's global one when no probe finds any
func (s *source) endpoint(key string) string {
	sum := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(sum[:])[:16]
	s.mu.Lock()
	s.load()
	if base := s.found[hash]; base != "" {
		s.mu.Unlock()
		slog.Debug("Z.AI: using cached endpoint " + base)
		return base
	}
	if at, ok := s.failed[hash]; ok && time.Since(at) < probeCooldown {
		s.mu.Unlock()
		return zaiconfig.BaseURL
	}
	s.mu.Unlock()
	found := detect(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if found == nil {
		slog.Debug("Z.AI: probe failed, falling back to default " + zaiconfig.BaseURL)
		s.failed[hash] = time.Now()
		return zaiconfig.BaseURL
	}
	s.found[hash] = found.base
	raw, _ := json.Marshal(s.found)
	if err := fsx.Write(s.path, raw, 0o600); err != nil {
		slog.Warn(fmt.Sprintf("Z.AI: could not persist detected endpoint (%s); will re-probe next start", err))
	}
	slog.Info(fmt.Sprintf("Z.AI: auto-detected endpoint %s (%s)", found.label, found.base))
	return found.base
}

// Endpoint of the highest priority that answers the key, every endpoint probed at once
func detect(key string) *endpoint {
	ctx, cancel := context.WithTimeout(context.Background(), probeWait)
	defer cancel()
	client := &http.Client{Timeout: probeWait}
	type outcome struct {
		at int
		ok bool
	}
	results := make(chan outcome, len(endpoints))
	for i, ep := range endpoints {
		go func() { results <- outcome{i, probe(ctx, client, key, ep)} }()
	}
	done, ok := make([]bool, len(endpoints)), make([]bool, len(endpoints))
	for range endpoints {
		o := <-results
		done[o.at], ok[o.at] = true, o.ok
		for i := range endpoints {
			if !done[i] {
				break
			}
			if ok[i] {
				return &endpoints[i]
			}
		}
	}
	for i := range endpoints {
		if ok[i] {
			return &endpoints[i]
		}
	}
	return nil
}

// Whether the endpoint answers the key for any of its models
func probe(ctx context.Context, client *http.Client, key string, ep endpoint) bool {
	for _, model := range ep.models {
		body, _ := json.Marshal(map[string]any{"model": model, "stream": false, "max_tokens": 1, "messages": []any{map[string]any{"role": "user",
			"content": "ping"}}})
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.base+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			continue
		}
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			slog.Debug(fmt.Sprintf("Z.AI endpoint probe: %s model=%s failed: %s", ep.id, model, err))
			continue
		}
		response.Body.Close()
		if response.StatusCode == http.StatusOK {
			slog.Debug(fmt.Sprintf("Z.AI endpoint probe: %s (%s) model=%s OK", ep.id, ep.base, model))
			return true
		}
		slog.Debug(fmt.Sprintf("Z.AI endpoint probe: %s model=%s returned %d", ep.id, model, response.StatusCode))
	}
	return false
}
