package provider

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Provider names that route to the managed llama.cpp server
var LlamacppAliases = map[string]bool{"llamacpp": true, "llama.cpp": true, "llama-cpp": true}

// The llamacpp alias endpoint, APIKey empty for a detected server
type LlamacppEndpoint struct {
	BaseURL string
	APIKey  string
}

// Status read query naming one router child with autoload off so the read never reloads it
func childQuery(modelID string) string {
	return "model=" + hfQuote(modelID) + "&autoload=false"
}

// Liveness of the state file's supervisor child pid, false for zero or negative pids
func endpointPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return processPIDExists(pid)
}

// Recorded base URL and raw api_key of the managed server whose process identity still matches, err where Python propagates
func stateEndpoint(h string) (string, any, bool, error) {
	if home.RootOf(h) != home.Root() {
		return "", nil, false, nil
	}
	state := readLocalRuntimeState()
	baseURL, isStr := pyGetAny(state, "base_url", "").(string)
	if !isStr || baseURL == "" {
		return "", nil, false, nil
	}
	if isModernLocalRuntimeState(state) {
		if recordedProcess(state) == nil {
			return "", nil, false, nil
		}
	} else {
		proc, err := legacyRecordedProcess(state)
		if err != nil {
			return "", nil, false, err
		}
		if proc == nil {
			return "", nil, false, nil
		}
	}
	return baseURL, pyGetAny(state, "api_key", ""), true, nil
}

// api_key value as the provider runtime reads it, None standing for no key
func endpointAPIKeyText(raw any) string {
	if raw == nil {
		return ""
	}
	return pyStr(raw)
}

// Base URL and key of the managed server recorded for the installation owning home h
func managedLocalStateEndpoint(h string) (string, string, bool) {
	baseURL, apiKey, ok, err := stateEndpoint(h)
	if err != nil {
		slog.Warn("managed endpoint state check failed: " + err.Error())
		return "", "", false
	}
	return baseURL, endpointAPIKeyText(apiKey), ok
}

// Root URL and key of the managed router through the ownership guarded reader of the context home
func managedRoot(ctx context.Context) (base, apiKey string, ok bool) {
	defer func() {
		if recover() != nil {
			base, apiKey, ok = "", "", false
		}
	}()
	baseURL, rawKey, found, err := stateEndpoint(home.Dir(ctx))
	if err != nil || !found {
		return "", "", false
	}
	base = baseURL
	if i := strings.LastIndex(baseURL, "/v1"); i >= 0 {
		base = baseURL[:i]
	}
	if base == "" {
		return "", "", false
	}
	return base, pyStr(rawKey), true
}

var managedHTTPClient = &http.Client{}

// Authenticated GET against the managed router decoded as JSON, any transport or decode failure an error
func managedGetJSON(ctx context.Context, base, apiKey, route string, timeout time.Duration) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+route, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := managedHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
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
	body, ok := moaJSONLoads(text)
	if !ok {
		return nil, fmt.Errorf("Expecting value: line 1 column 1 (char 0)")
	}
	return body, nil
}

// Python int() of one detect_ports item
func endpointPortInt(v any) (int, error) {
	switch p := v.(type) {
	case bool:
		if p {
			return 1, nil
		}
		return 0, nil
	case int:
		return p, nil
	case int64:
		return int(p), nil
	case float64:
		if math.IsNaN(p) {
			return 0, fmt.Errorf("cannot convert float NaN to integer")
		}
		if math.IsInf(p, 0) {
			return 0, fmt.Errorf("cannot convert float infinity to integer")
		}
		return int(p), nil
	case string:
		if n, ok := pythonUnicodeIntText(p); ok {
			return int(n), nil
		}
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyRepr(p))
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", pyTypeName(v))
}

// The local_runtime.detect_ports of cfg converted the way the tuple of int(p) does
func endpointDetectPorts(cfg map[string]any) ([]int, error) {
	raw := cfg["local_runtime"]
	if !pyTruthy(raw) {
		return nil, nil
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(raw))
	}
	var items []any
	switch ports := section["detect_ports"].(type) {
	case nil:
	case []any:
		items = ports
	case string:
		for _, r := range ports {
			items = append(items, string(r))
		}
	case map[string]any:
		keys := make([]string, 0, len(ports))
		for k := range ports {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			items = append(items, k)
		}
	default:
		if pyTruthy(ports) {
			return nil, fmt.Errorf("'%s' object is not iterable", pyTypeName(ports))
		}
	}
	out := make([]int, 0, len(items))
	for _, item := range items {
		port, err := endpointPortInt(item)
		if err != nil {
			return nil, err
		}
		out = append(out, port)
	}
	return out, nil
}

// Managed first then detected endpoint for llamacpp aliases, waiting up to waitForBoot for a booting server
func ResolveLlamacppEndpoint(ctx context.Context, cfg map[string]any, h string, waitForBoot time.Duration) (*LlamacppEndpoint, error) {
	baseURL, apiKey, ok, err := stateEndpoint(h)
	if err != nil {
		return nil, err
	}
	if ok {
		return &LlamacppEndpoint{BaseURL: baseURL, APIKey: endpointAPIKeyText(apiKey)}, nil
	}
	ports, err := endpointDetectPorts(cfg)
	if err != nil {
		return nil, err
	}
	hit, err := detectServerFn(ports)
	if err != nil {
		return nil, err
	}
	if hit != nil && !hit.AuthRequired {
		return &LlamacppEndpoint{BaseURL: hit.BaseURL}, nil
	}
	if waitForBoot > 0 && bootInFlight(ctx, cfg, h) {
		kickManagedBoot(ctx, cfg, h)
		var pollErr error
		found := PollUntil(func() (*LlamacppEndpoint, bool) {
			baseURL, apiKey, ok, err := stateEndpoint(h)
			if err != nil {
				pollErr = err
				return nil, true
			}
			if !ok {
				return nil, false
			}
			return &LlamacppEndpoint{BaseURL: baseURL, APIKey: endpointAPIKeyText(apiKey)}, true
		}, waitForBoot, 250*time.Millisecond)
		return found, pollErr
	}
	return nil, nil
}

var detectServerFn = DetectServer

// Endpoint with the default eight second boot wait, any resolution error read as no endpoint like both hermes callers
func resolveLlamacppEndpoint(ctx context.Context, cfg map[string]any, h string) (string, string, bool) {
	return resolveLlamacppEndpointWithin(ctx, cfg, h, 8*time.Second)
}

// Endpoint waiting up to waitForBoot, any resolution error silently read as no endpoint as the provider callers do
func resolveLlamacppEndpointWithin(ctx context.Context, cfg map[string]any, h string, waitForBoot time.Duration) (string, string, bool) {
	endpoint, err := ResolveLlamacppEndpoint(ctx, cfg, h, waitForBoot)
	if err != nil || endpoint == nil {
		return "", "", false
	}
	return endpoint.BaseURL, endpoint.APIKey, true
}

var (
	kickManagedBootMu    sync.Mutex
	ensureLocalRuntimeFn = func(ctx context.Context, cfg map[string]any) (*LlamaServerSupervisor, error) {
		return EnsureLocalRuntime(ctx, cfg, false)
	}
	endpointInstalledEngine = func(ctx context.Context, backend string) (*Engine, error) {
		return InstalledEngine(ctx, backend, true)
	}
)

// cfg itself, or the config of home h when the caller passed none
func endpointConfig(cfg map[string]any, h string) map[string]any {
	if cfg != nil {
		return cfg
	}
	return loadConfigTree(h)
}

// Starts the managed server in the background under the caller's profile scope unless a kick is running
func kickManagedBoot(ctx context.Context, cfg map[string]any, h string) {
	if !kickManagedBootMu.TryLock() {
		return
	}
	scoped := home.WithOverride(context.WithoutCancel(ctx), h)
	go func() {
		defer kickManagedBootMu.Unlock()
		defer func() {
			if r := recover(); r != nil {
				slog.Warn(fmt.Sprintf("on-demand managed-server boot failed: %v", r))
			}
		}()
		if _, err := ensureLocalRuntimeFn(scoped, endpointConfig(cfg, h)); err != nil {
			slog.Warn(fmt.Sprintf("on-demand managed-server boot failed: %v", err))
		}
	}()
}

// Whether the managed runtime is enabled and PM holds an installed engine for its backend
func bootInFlight(ctx context.Context, cfg map[string]any, h string) (inFlight bool) {
	defer func() {
		if recover() != nil {
			inFlight = false
		}
	}()
	raw := endpointConfig(cfg, h)["local_runtime"]
	if !pyTruthy(raw) {
		return false
	}
	section, ok := raw.(map[string]any)
	if !ok || !pyTruthy(section["enabled"]) {
		return false
	}
	backend, err := hwConfiguredBackend(section)
	if err != nil {
		return false
	}
	engine, err := endpointInstalledEngine(ctx, backend)
	return err == nil && engine != nil
}
