package providers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/wire"
)

var (
	glmVersion      = regexp.MustCompile(`^glm-(\d+)(?:\.(\d+))?`)
	glm53Tokens     = []string{"glm-5.3", "glm-5-3", "glm-5p3"}
	glm52Tokens     = append([]string{"glm-5.2", "glm-5-2", "glm-5p2"}, glm53Tokens...)
	zaiCodingModels = []string{"glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5v-turbo", "glm-4.7"}
	ZaiEndpoints    = []ZaiEndpoint{
		{"global", "https://api.z.ai/api/paas/v4", []string{"glm-5"}, "Global"},
		{"cn", "https://open.bigmodel.cn/api/paas/v4", []string{"glm-5"}, "China"},
		{"coding-global", "https://api.z.ai/api/coding/paas/v4", zaiCodingModels, "Global (Coding Plan)"},
		{"coding-cn", "https://open.bigmodel.cn/api/coding/paas/v4", zaiCodingModels, "China (Coding Plan)"},
	}
)

const (
	zaiProbeFailureTTL = 300 * time.Second
	zaiProbeTimeout    = 8 * time.Second
)

type ZaiEndpoint struct {
	ID      string
	BaseURL string
	Models  []string
	Label   string
}

type ZaiDetection struct {
	ID, BaseURL, Model, Label string
}

type zaiProfile struct {
	base
	d      Deps
	mu     sync.Mutex
	failed map[string]time.Time
}

func newZai(d Deps) Profile {
	return &zaiProfile{d: d, failed: map[string]time.Time{}, base: base{spec: Spec{
		Name: "zai", Aliases: []string{"glm", "z-ai", "z.ai", "zhipu"}, EnvVars: []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"},
		DisplayName: "Z.AI (GLM)", Description: "Z.AI / GLM — Zhipu AI models", SignupURL: "https://z.ai/",
		FallbackModels: []string{"glm-5.2", "glm-5", "glm-4-9b"}, BaseURL: "https://api.z.ai/api/paas/v4",
		DefaultAuxModel: "glm-4.5-flash",
	}}}
}

func hasToken(model string, tokens []string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return slices.ContainsFunc(tokens, func(t string) bool { return strings.Contains(m, t) })
}

func glmSupportsThinking(model string) bool {
	m := glmVersion.FindStringSubmatch(strings.ToLower(strings.TrimSpace(model)))
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor := 0
	if m[2] != "" {
		minor, _ = strconv.Atoi(m[2])
	}
	return major > 4 || (major == 4 && minor >= 5)
}

func glm52Effort(r *wire.Reasoning, model string) string {
	effort := wire.RequestedEffort(r)
	if effort == "" || effort == "none" {
		return ""
	}
	family, floor := wire.EffortsGLM52.Efforts(), "high"
	if hasToken(model, glm53Tokens) {
		family, floor = wire.EffortsGLM53.Efforts(), "low"
	}
	if clamped := wire.ClampEffort(effort, family.Levels, family.Round); slices.Contains(family.Levels, clamped) {
		return clamped
	}
	return floor
}

func (p *zaiProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	out := Extras{Body: map[string]json.RawMessage{}, TopLevel: map[string]json.RawMessage{}}
	is52 := hasToken(f.Model, glm52Tokens)
	if !glmSupportsThinking(f.Model) && !is52 {
		return out, nil
	}
	if r := f.Reasoning; r != nil {
		enabled := r.Enabled
		if !enabled && hasToken(f.Model, glm53Tokens) {
			enabled = true
			out.TopLevel["reasoning_effort"] = wire.Quote("low")
		}
		if enabled {
			out.Body["thinking"] = json.RawMessage(`{"type":"enabled","clear_thinking":false}`)
		} else {
			out.Body["thinking"] = json.RawMessage(`{"type":"disabled"}`)
		}
	}
	if is52 {
		if effort := glm52Effort(f.Reasoning, f.Model); effort != "" {
			out.TopLevel["reasoning_effort"] = wire.Quote(effort)
		}
	}
	return out, nil
}

func (p *zaiProfile) probe(ctx context.Context, key string, ep ZaiEndpoint) (ZaiDetection, bool) {
	for _, model := range ep.Models {
		body := (&wire.Dict{}).SetString("model", model).SetBool("stream", false).SetInt("max_tokens", 1).
			Set("messages", json.RawMessage(`[{"role":"user","content":"ping"}]`)).JSON()
		pctx, cancel := context.WithTimeout(ctx, zaiProbeTimeout)
		req, err := http.NewRequestWithContext(pctx, http.MethodPost, ep.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.d.Net.Client.Do(req)
		if err != nil {
			cancel()
			slog.Debug(fmt.Sprintf("Z.AI endpoint probe: %s model=%s failed: %v", ep.ID, model, err))
			continue
		}
		resp.Body.Close()
		cancel()
		if resp.StatusCode == http.StatusOK {
			slog.Debug(fmt.Sprintf("Z.AI endpoint probe: %s (%s) model=%s OK", ep.ID, ep.BaseURL, model))
			return ZaiDetection{ID: ep.ID, BaseURL: ep.BaseURL, Model: model, Label: ep.Label}, true
		}
		slog.Debug(fmt.Sprintf("Z.AI endpoint probe: %s model=%s returned %d", ep.ID, model, resp.StatusCode))
	}
	return ZaiDetection{}, false
}

func (p *zaiProfile) Detect(ctx context.Context, key string) (ZaiDetection, bool) {
	type result struct {
		i   int
		det ZaiDetection
		ok  bool
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result, len(ZaiEndpoints))
	for i, ep := range ZaiEndpoints {
		go func() {
			det, ok := p.probe(ctx, key, ep)
			results <- result{i, det, ok}
		}()
	}
	done := make([]bool, len(ZaiEndpoints))
	found := make([]*ZaiDetection, len(ZaiEndpoints))
	first := func(requireDone bool) (ZaiDetection, bool) {
		for i := range ZaiEndpoints {
			if requireDone && !done[i] {
				return ZaiDetection{}, false
			}
			if found[i] != nil {
				return *found[i], true
			}
		}
		return ZaiDetection{}, false
	}
	for range ZaiEndpoints {
		r := <-results
		done[r.i] = true
		if r.ok {
			det := r.det
			found[r.i] = &det
		}
		if det, ok := first(true); ok {
			return det, true
		}
	}
	return first(false)
}

func (p *zaiProfile) RuntimeBaseURL(ctx context.Context, apiKey auth.Secret, def, envURL string) (string, bool) {
	if envURL != "" {
		return envURL, true
	}
	key := apiKey.Reveal()
	if key == "" {
		return def, true
	}
	sum := sha256.Sum256([]byte(key))
	keyHash := hex.EncodeToString(sum[:])[:16]
	if raw, ok, err := p.d.Store.State(ctx, "zai"); err == nil && ok {
		if v, perr := wire.ParseValue(raw); perr == nil {
			cached := v.Get("detected_endpoint")
			if cached.Get("base_url").Truthy() && cached.Get("key_hash").Text() == keyHash {
				return cached.Get("base_url").Display(), true
			}
		}
	}
	p.mu.Lock()
	until, failed := p.failed[keyHash]
	p.mu.Unlock()
	if failed && until.After(time.Now()) {
		return def, true
	}
	det, ok := p.Detect(ctx, key)
	if !ok || det.BaseURL == "" {
		p.mu.Lock()
		p.failed[keyHash] = time.Now().Add(zaiProbeFailureTTL)
		p.mu.Unlock()
		return def, true
	}
	endpoint := (&wire.Dict{}).SetString("base_url", det.BaseURL).SetString("endpoint_id", det.ID).SetString("model", det.Model).
		SetString("label", det.Label).SetString("key_hash", keyHash).JSON()
	err := p.d.Store.Update(ctx, "zai", func(state json.RawMessage) (json.RawMessage, error) {
		d := &wire.Dict{}
		if v, perr := wire.ParseValue(state); perr == nil && v.IsObject() {
			d = wire.DictOf(v)
		}
		return d.Set("detected_endpoint", endpoint).JSON(), nil
	})
	if err != nil {
		slog.Warn(fmt.Sprintf("Z.AI: could not persist detected endpoint (%v); will re-probe next start", err))
	}
	slog.Info(fmt.Sprintf("Z.AI: auto-detected endpoint %s (%s)", det.Label, det.BaseURL))
	return det.BaseURL, true
}
