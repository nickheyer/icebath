package provider

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type auxRejectedRoute struct {
	route, model string
	formatType   any
}

var (
	auxRejectedRoutesMu sync.Mutex
	auxRejectedRoutes   = map[auxRejectedRoute]bool{}
)

// Endpoint netloc when a base URL is known, else the lowercased provider name
func auxRouteKey(provider, baseURL string) (string, error) {
	parts, err := pyURLParse(baseURL, "")
	if err != nil {
		return "", err
	}
	if key := pystr.Lower(parts.netloc); key != "" {
		return key, nil
	}
	return pystr.Lower(pystr.Strip(provider)), nil
}

// Response format type carried in extra_body or top level kwargs, nil when absent
func auxResponseFormatType(kwargs map[string]any) any {
	var responseFormat any
	if extraBody, ok := kwargs["extra_body"].(map[string]any); ok {
		responseFormat = extraBody["response_format"]
	}
	if responseFormat == nil {
		responseFormat = kwargs["response_format"]
	}
	if m, ok := responseFormat.(map[string]any); ok {
		return m["type"]
	}
	return nil
}

// Error Python raises when an unhashable value would enter the rejection memo
func auxHashable(v any) error {
	switch v.(type) {
	case map[string]any, []any, []string, map[string]string:
		return fmt.Errorf("unhashable type: '%s'", pyrepr.TypeName(v))
	}
	return nil
}

// Provider profile unsupported response formats, a custom route on a profiled host gets that profile
func auxProfileUnsupportedFormats(ctx context.Context, provider, baseURL string) []string {
	name := pystr.Lower(pystr.Strip(provider))
	if name == "custom" && baseURL != "" {
		if inferred := inferProviderFromBaseURL(baseURL); inferred != "" {
			name = inferred
		}
	}
	profile, ok := GetProviderProfile(ctx, name)
	if !ok {
		return nil
	}
	return profile.UnsupportedResponseFormats
}

// Capability is per model on aggregators, so these markers name the route capability rather than one schema
var capabilityRejectionMarkers = []string{
	"unavailable", "does not support", "doesn't support", "not supported", "unsupported parameter", "unsupported_parameter",
	"unknown parameter", "unrecognized request argument", "unrecognized parameter", "extra inputs are not permitted",
}

// IsCapabilityRejection reports whether a structured output rejection speaks to the route capability rather than one schema
func IsCapabilityRejection(err error) bool {
	if err == nil {
		return false
	}
	lower := pystr.Lower(err.Error())
	if strings.Contains(lower, "invalid schema") {
		return false
	}
	return containsAnyStr(lower, capabilityRejectionMarkers...)
}

// RememberStructuredOutputRejection records the rejected response format type when the error names the capability
func RememberStructuredOutputRejection(provider, baseURL string, rejectedKwargs map[string]any, err error) error {
	formatType := auxResponseFormatType(rejectedKwargs)
	if !pyrepr.Truthy(formatType) || !IsCapabilityRejection(err) {
		return nil
	}
	route, rerr := auxRouteKey(provider, baseURL)
	if rerr != nil {
		return rerr
	}
	model := ""
	if v := rejectedKwargs["model"]; pyrepr.Truthy(v) {
		model = pyrepr.Str(v)
	}
	if herr := auxHashable(formatType); herr != nil {
		return herr
	}
	auxRejectedRoutesMu.Lock()
	auxRejectedRoutes[auxRejectedRoute{route, model, formatType}] = true
	auxRejectedRoutesMu.Unlock()
	return nil
}

// Drops every remembered response format rejection as a fresh process starts
func auxResetStructuredOutputRejections() {
	auxRejectedRoutesMu.Lock()
	auxRejectedRoutes = map[auxRejectedRoute]bool{}
	auxRejectedRoutesMu.Unlock()
}

// Profile or memo verdict on the response format type with the route key the memo uses
func auxResponseFormatKnownUnsupported(ctx context.Context, provider, baseURL, model string, formatType any) (bool, string, error) {
	if !pyrepr.Truthy(formatType) {
		return false, "", nil
	}
	known := false
	if s, ok := formatType.(string); ok {
		known = containsStr(auxProfileUnsupportedFormats(ctx, provider, baseURL), s)
	}
	route, err := auxRouteKey(provider, baseURL)
	if err != nil {
		return false, "", err
	}
	if !known {
		if herr := auxHashable(formatType); herr != nil {
			return false, "", herr
		}
		auxRejectedRoutesMu.Lock()
		known = auxRejectedRoutes[auxRejectedRoute{route, model, formatType}]
		auxRejectedRoutesMu.Unlock()
	}
	return known, route, nil
}

// WithoutUnsupportedResponseFormat drops a response_format the route and model are known to reject
func WithoutUnsupportedResponseFormat(ctx context.Context, extraBody map[string]any, provider, baseURL, model, task string) (map[string]any, error) {
	var formatType any
	if m, ok := extraBody["response_format"].(map[string]any); ok {
		formatType = m["type"]
	}
	known, route, err := auxResponseFormatKnownUnsupported(ctx, provider, baseURL, model, formatType)
	if err != nil {
		return nil, err
	}
	if !known {
		return extraBody, nil
	}
	label, routeLabel, modelLabel := task, route, model
	if label == "" {
		label = "call"
	}
	if routeLabel == "" {
		routeLabel = "provider"
	}
	if modelLabel == "" {
		modelLabel = "model"
	}
	slog.Info(fmt.Sprintf("Auxiliary %s: %s (%s) does not accept response_format %s; sending without it "+
		"(schema enforcement degrades to prompt compliance)", label, routeLabel, modelLabel, pyrepr.Str(formatType)))
	out := make(map[string]any, len(extraBody))
	for k, v := range extraBody {
		if k != "response_format" {
			out[k] = v
		}
	}
	return out, nil
}

// Copies kwargs without response_format at top level and in extra_body, nil when nothing was removed
func auxWithoutStructuredOutputFormat(kwargs map[string]any) map[string]any {
	retry := make(map[string]any, len(kwargs))
	for k, v := range kwargs {
		retry[k] = v
	}
	popped, had := retry["response_format"]
	delete(retry, "response_format")
	changed := had && popped != nil
	if extraBody, ok := retry["extra_body"].(map[string]any); ok {
		if _, has := extraBody["response_format"]; has {
			remaining := make(map[string]any, len(extraBody))
			for k, v := range extraBody {
				if k != "response_format" {
					remaining[k] = v
				}
			}
			if len(remaining) > 0 {
				retry["extra_body"] = remaining
			} else {
				delete(retry, "extra_body")
			}
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return retry
}
