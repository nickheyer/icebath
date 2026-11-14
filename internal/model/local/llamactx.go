package local

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const llamaContextTimeout = 3 * time.Second

func contextProbe(ctx context.Context, client *http.Client, header http.Header, rawURL string) (int, map[string]any, error) {
	reqCtx, cancel := context.WithTimeout(ctx, llamaContextTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, err
	}
	for key, values := range header {
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		body = map[string]any{}
	}
	return resp.StatusCode, body, nil
}

func nonzeroInt(v any) (int, bool) {
	n, ok := v.(float64)
	if !ok || n == 0 {
		return 0, false
	}
	return int(n), true
}

func ModelIDMatches(candidate, lookup string) bool {
	if candidate == lookup {
		return true
	}
	i := strings.LastIndexByte(candidate, '/')
	return i >= 0 && candidate[i+1:] == lookup
}

func LlamaCppContext(ctx context.Context, client *http.Client, header http.Header, serverURL, model string) (int, bool) {
	for _, propsPath := range []string{"/props?model=" + url.QueryEscape(model) + "&autoload=false", "/props"} {
		status, body, err := contextProbe(ctx, client, header, serverURL+propsPath)
		if err != nil {
			return 0, false
		}
		if status != http.StatusOK {
			continue
		}
		if settings, ok := body["default_generation_settings"].(map[string]any); ok {
			if n, ok := nonzeroInt(settings["n_ctx"]); ok {
				return n, true
			}
		}
	}
	return RouterLaunchCtx(ctx, client, header, serverURL, model)
}

func RouterLaunchCtx(ctx context.Context, client *http.Client, header http.Header, serverURL, model string) (int, bool) {
	status, body, err := contextProbe(ctx, client, header, serverURL+"/models")
	if err != nil || status != http.StatusOK {
		return 0, false
	}
	entries, _ := body["data"].([]any)
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || !ModelIDMatches(jsonText(entry["id"]), model) {
			continue
		}
		statusField, _ := entry["status"].(map[string]any)
		rawArgs, _ := statusField["args"].([]any)
		args := make([]string, len(rawArgs))
		for i, a := range rawArgs {
			args[i] = jsonText(a)
		}
		for i := 0; i+1 < len(args); i++ {
			if args[i] != "--ctx-size" && args[i] != "-c" {
				continue
			}
			if !isASCIIDigits(args[i+1]) {
				continue
			}
			if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
				return n, true
			}
		}
	}
	return 0, false
}

func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
