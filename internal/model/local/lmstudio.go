package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	LMStudioProbeTimeout = 5 * time.Second
	LMStudioLoadTimeout  = 120 * time.Second
	lmStudioListTimeout  = 10 * time.Second
	lmStudioProvider     = "lmstudio"
)

type AuthRejectedError struct {
	Provider string
	Status   int
}

func (e *AuthRejectedError) Error() string {
	return fmt.Sprintf("LM Studio rejected the request with HTTP %d.", e.Status)
}

func (e *AuthRejectedError) Code() string { return "auth_rejected" }

func LMStudioServerRoot(baseURL string) string {
	return stripSuffixes(strings.TrimRight(strings.TrimSpace(baseURL), "/"), "/api/v1", "/api", "/v1")
}

func lmStudioHeaders(ctx context.Context, key ProbeKey) map[string]string {
	headers := map[string]string{}
	if token := key.token(ctx); token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return headers
}

func (r *Runtime) lmStudioRawModels(ctx context.Context, key ProbeKey, baseURL string, timeout time.Duration) ([]any, error) {
	root := LMStudioServerRoot(baseURL)
	if root == "" {
		return nil, nil
	}
	if timeout <= 0 {
		timeout = LMStudioProbeTimeout
	}
	raw, err := r.catalogGet(ctx, http.MethodGet, root+"/api/v1/models", lmStudioHeaders(ctx, key), nil, timeout)
	if err != nil {
		var status *httpclient.StatusError
		if errors.As(err, &status) {
			if status.Code == http.StatusUnauthorized || status.Code == http.StatusForbidden {
				return nil, &AuthRejectedError{Provider: lmStudioProvider, Status: status.Code}
			}
			slog.Debug(fmt.Sprintf("LM Studio probe at %s failed with HTTP %d", root, status.Code))
			return nil, nil
		}
		slog.Debug(fmt.Sprintf("LM Studio probe at %s failed: %v", root, err))
		return nil, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		slog.Debug(fmt.Sprintf("LM Studio probe at %s failed: %v", root, err))
		return nil, nil
	}
	models, ok := payload["models"].([]any)
	if !ok {
		slog.Debug(fmt.Sprintf("LM Studio probe at %s returned malformed payload (no `models` list)", root))
		return nil, nil
	}
	return models, nil
}

func (r *Runtime) lmStudioRawModelsOrNil(ctx context.Context, key ProbeKey, baseURL string, timeout time.Duration) []any {
	models, err := r.lmStudioRawModels(ctx, key, baseURL, timeout)
	if err != nil {
		return nil
	}
	return models
}

func lmStudioEntryFor(models []any, model string) map[string]any {
	for _, raw := range models {
		if m, ok := raw.(map[string]any); ok && (m["key"] == model || m["id"] == model) {
			return m
		}
	}
	return nil
}

func (r *Runtime) ProbeLMStudioModels(ctx context.Context, key ProbeKey, baseURL string, timeout time.Duration) ([]string, bool, error) {
	raw, err := r.lmStudioRawModels(ctx, key, baseURL, timeout)
	if err != nil {
		return nil, false, err
	}
	if raw == nil {
		return nil, false, nil
	}
	keys := []string{}
	seen := map[string]bool{}
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok || strings.ToLower(strings.TrimSpace(jsonText(m["type"]))) == "embedding" {
			continue
		}
		id := m["key"]
		if !jsonTruthy(id) {
			id = m["id"]
		}
		k := ""
		if jsonTruthy(id) {
			k = strings.TrimSpace(jsonText(id))
		}
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys, true, nil
}

func (r *Runtime) FetchLMStudioModels(ctx context.Context, key ProbeKey, baseURL string, timeout time.Duration) ([]string, error) {
	models, _, err := r.ProbeLMStudioModels(ctx, key, baseURL, timeout)
	if err != nil {
		return nil, err
	}
	if models == nil {
		models = []string{}
	}
	return models, nil
}

type LMStudioLoadResult struct {
	ContextLength int
	LoadAttempted bool
	Rejected      bool
}

func positiveJSONInt(v any) (int, bool) {
	n, ok := v.(float64)
	if !ok || n <= 0 || n != float64(int64(n)) {
		return 0, false
	}
	return int(n), true
}

func lmStudioLoadedContext(entry map[string]any) (int, bool) {
	instances, ok := entry["loaded_instances"].([]any)
	if !ok {
		return 0, false
	}
	for _, instance := range instances {
		inst, _ := instance.(map[string]any)
		cfg, _ := inst["config"].(map[string]any)
		if n, ok := positiveJSONInt(cfg["context_length"]); ok {
			return n, true
		}
	}
	return 0, false
}

func (r *Runtime) EnsureLMStudioModelLoaded(ctx context.Context, model, baseURL string, key ProbeKey, targetContext *int, timeout time.Duration) LMStudioLoadResult {
	root := LMStudioServerRoot(baseURL)
	if root == "" {
		return LMStudioLoadResult{}
	}
	explicit := 0
	if targetContext != nil {
		if *targetContext <= 0 {
			return LMStudioLoadResult{}
		}
		explicit = *targetContext
	}
	target := lmStudioEntryFor(r.lmStudioRawModelsOrNil(ctx, key, baseURL, lmStudioListTimeout), model)
	if target == nil {
		return LMStudioLoadResult{}
	}
	if maxCtx, ok := positiveJSONInt(target["max_context_length"]); ok && explicit > 0 && explicit > maxCtx {
		return LMStudioLoadResult{Rejected: true}
	}
	if current, ok := lmStudioLoadedContext(target); ok {
		return LMStudioLoadResult{ContextLength: current}
	}
	instances, ok := target["loaded_instances"].([]any)
	if !ok || len(instances) > 0 {
		return LMStudioLoadResult{}
	}
	payload := map[string]any{"model": model, "echo_load_config": true}
	if explicit > 0 {
		payload["context_length"] = explicit
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return LMStudioLoadResult{LoadAttempted: true}
	}
	headers := lmStudioHeaders(ctx, key)
	headers["Content-Type"] = "application/json"
	if timeout <= 0 {
		timeout = LMStudioLoadTimeout
	}
	raw, err := r.catalogGet(ctx, http.MethodPost, root+"/api/v1/models/load", headers, body, timeout)
	if err != nil {
		return LMStudioLoadResult{LoadAttempted: true}
	}
	var response map[string]any
	_ = json.Unmarshal(raw, &response)
	if loadConfig, ok := response["load_config"].(map[string]any); ok {
		if applied, ok := positiveJSONInt(loadConfig["context_length"]); ok {
			return LMStudioLoadResult{ContextLength: applied, LoadAttempted: true}
		}
	}
	refreshed := r.lmStudioRawModelsOrNil(ctx, key, baseURL, lmStudioListTimeout)
	if refreshed == nil {
		return LMStudioLoadResult{LoadAttempted: true}
	}
	entry := lmStudioEntryFor(refreshed, model)
	if entry == nil {
		return LMStudioLoadResult{LoadAttempted: true}
	}
	contextLength, _ := lmStudioLoadedContext(entry)
	return LMStudioLoadResult{ContextLength: contextLength, LoadAttempted: true}
}

func (r *Runtime) LMStudioReasoningOptions(ctx context.Context, model, baseURL string, key ProbeKey, timeout time.Duration) []string {
	entry := lmStudioEntryFor(r.lmStudioRawModelsOrNil(ctx, key, baseURL, timeout), model)
	if entry == nil {
		return []string{}
	}
	caps, _ := entry["capabilities"].(map[string]any)
	reasoning, _ := caps["reasoning"].(map[string]any)
	opts, ok := reasoning["allowed_options"].([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, o := range opts {
		if s, ok := o.(string); ok {
			out = append(out, strings.ToLower(strings.TrimSpace(s)))
		}
	}
	return out
}
