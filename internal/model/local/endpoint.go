package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/httpclient"
)

const (
	DefaultBootWait = 8 * time.Second
	bootPoll        = 250 * time.Millisecond
)

var llamaCppAliases = map[string]bool{"llamacpp": true, "llama.cpp": true, "llama-cpp": true}

func IsLlamaCppAlias(provider string) bool { return llamaCppAliases[provider] }

type Endpoint struct {
	BaseURL string
	APIKey  string
}

func (e Endpoint) String() string { return e.BaseURL }

func (e Endpoint) GoString() string { return fmt.Sprintf("local.Endpoint{BaseURL: %q}", e.BaseURL) }

func (e Endpoint) LogValue() slog.Value { return slog.StringValue(e.BaseURL) }

func childQuery(modelID string) string {
	return "model=" + url.QueryEscape(modelID) + "&autoload=false"
}

func (r *Runtime) stateEndpoint() *Endpoint {
	state := r.readState()
	baseURL, ok := state.text("base_url")
	if !ok || baseURL == "" {
		return nil
	}
	if state.modern() {
		if _, ok := recordedProcess(state); !ok {
			return nil
		}
	} else if _, _, ok := r.legacyRecordedProcess(state); !ok {
		return nil
	}
	key, _ := state.text("api_key")
	return &Endpoint{BaseURL: baseURL, APIKey: key}
}

func (r *Runtime) ManagedEndpoint() *Endpoint { return r.stateEndpoint() }

func (r *Runtime) managedRoot() (string, string, bool) {
	state := r.stateEndpoint()
	if state == nil {
		return "", "", false
	}
	base := state.BaseURL
	if i := strings.LastIndex(base, "/v1"); i >= 0 {
		base = base[:i]
	}
	if base == "" {
		return "", "", false
	}
	return base, state.APIKey, true
}

func (r *Runtime) managedGetJSON(ctx context.Context, base, apiKey, route string, timeout time.Duration) (any, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+route, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := httpclient.CheckStatus(resp); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Runtime) ResolveLlamaCppEndpoint(ctx context.Context, waitForBoot time.Duration) *Endpoint {
	if managed := r.stateEndpoint(); managed != nil {
		return managed
	}
	var ports []int
	if cfg, err := r.config(ctx); err == nil {
		ports = cfg.LocalRuntime.DetectPorts
	}
	if hit := r.DetectServer(ctx, ports); hit != nil && !hit.AuthRequired {
		return &Endpoint{BaseURL: hit.BaseURL}
	}
	if waitForBoot <= 0 || !r.bootInFlight(ctx) {
		return nil
	}
	r.kickManagedBoot(ctx)
	deadline := time.Now().Add(waitForBoot)
	for {
		if managed := r.stateEndpoint(); managed != nil {
			return managed
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(min(bootPoll, time.Until(deadline))):
		}
	}
}

func (r *Runtime) kickManagedBoot(ctx context.Context) {
	if !r.kicking.CompareAndSwap(false, true) {
		return
	}
	if !r.goDetached(ctx, func(bootCtx context.Context) {
		defer r.kicking.Store(false)
		r.Ensure(bootCtx, false)
	}) {
		r.kicking.Store(false)
	}
}

func (r *Runtime) bootInFlight(ctx context.Context) bool {
	cfg, err := r.config(ctx)
	if err != nil || !cfg.LocalRuntime.Enabled {
		return false
	}
	backend := string(cfg.LocalRuntime.Backend)
	if backend == "" {
		backend = defaultBackend
	}
	engine, err := r.InstalledEngine(ctx, backend, true)
	return err == nil && engine != nil
}
