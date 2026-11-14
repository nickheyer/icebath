package providers

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/wire"
)

var (
	lmValidEfforts  = []string{"none", "minimal", "low", "medium", "high", "xhigh"}
	lmEffortAliases = map[string]string{"off": "none", "on": "medium"}
	lmEffortClamp   = map[string]string{"max": "xhigh", "ultra": "xhigh"}
)

type lmStudioProfile struct {
	base
}

func newLMStudio() Profile {
	return &lmStudioProfile{base{spec: Spec{
		Name: "lmstudio", DisplayName: "LM Studio", SkipModelListing: true, SkipHealthCheck: true,
		EnvVars: []string{"LM_API_KEY", "LM_BASE_URL"},
	}}}
}

func lmAlias(v string) string {
	if a, ok := lmEffortAliases[v]; ok {
		return a
	}
	return v
}

func ResolveLMStudioEffort(r *wire.Reasoning, allowed []string) (string, bool) {
	effort := "medium"
	if r != nil {
		if !r.Enabled {
			effort = "none"
		} else {
			raw := lmAlias(reasoningEffortRaw(r))
			if c, ok := lmEffortClamp[raw]; ok {
				raw = c
			}
			if slices.Contains(lmValidEfforts, raw) {
				effort = raw
			}
		}
	}
	if len(allowed) > 0 && !slices.ContainsFunc(allowed, func(opt string) bool { return lmAlias(opt) == effort }) {
		return "", false
	}
	return effort, true
}

func (p *lmStudioProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	if !f.LMStudioProbed {
		nested, ok := wire.NestedReasoning(f.Reasoning)
		if !ok {
			return Extras{}, nil
		}
		return Extras{Body: map[string]json.RawMessage{"reasoning": nested}}, nil
	}
	if !f.SupportsReasoning {
		return Extras{}, nil
	}
	if effort, ok := ResolveLMStudioEffort(f.Reasoning, f.LMStudioOptions); ok {
		return effortTop(effort), nil
	}
	return Extras{}, nil
}

func NormalizeLMStudioBaseURL(baseURL string) string {
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, suffix := range []string{"/api/v1", "/api", "/v1"} {
		if strings.HasSuffix(root, suffix) {
			root = strings.TrimRight(strings.TrimSuffix(root, suffix), "/")
			break
		}
	}
	if root == "" {
		root = "http://127.0.0.1:1234"
	}
	return root + "/v1"
}

func (p *lmStudioProfile) RuntimeBaseURL(_ context.Context, apiKey auth.Secret, def, envURL string) (string, bool) {
	url := envURL
	if url == "" {
		url = def
	}
	return NormalizeLMStudioBaseURL(url), true
}
