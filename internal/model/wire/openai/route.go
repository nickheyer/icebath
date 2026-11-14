package openai

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

// Lowercase host of a base URL given with or without a scheme
func hostname(baseURL string) string {
	raw := strip(baseURL)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return strings.TrimRight(strings.ToLower(hostOf(raw)), ".")
}

// Reports whether a host is a domain or one of its subdomains
func hostWithin(host, domain string) bool {
	return host != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

// Responses backend family a base URL's host belongs to, generic for a host no rule names
func BackendForHost(baseURL string) wire.ResponsesBackend {
	host := hostname(baseURL)
	labels := strings.Split(host, ".")
	switch {
	case host == "api.x.ai":
		return wire.ResponsesXAI
	case hostWithin(host, "chatgpt.com") && strings.Contains(strings.ToLower(baseURL), "/backend-api/codex"):
		return wire.ResponsesCodex
	case host == "api.openai.com":
		return wire.ResponsesOpenAI
	case hostWithin(host, "services.ai.azure.com"):
		return wire.ResponsesAzureFoundry
	case hostWithin(host, "openai.azure.com"):
		return wire.ResponsesAzure
	case host == "opencode.ai":
		return wire.ResponsesOpenCode
	case host == "api.perplexity.ai":
		return wire.ResponsesPerplexity
	case host == "api.meta.ai":
		return wire.ResponsesMeta
	case len(labels) == 4 && labels[0] == "bedrock-mantle" && labels[1] != "" && labels[2] == "api" && labels[3] == "aws":
		return wire.ResponsesMantle
	}
	return wire.ResponsesGeneric
}

// Drops the reasoning items a failed request replayed without an exact issuer and model stamp, returning how many
func StripUnverifiedReasoning(messages []wire.Message, issuerKind, issuerModel string) int {
	removed := 0
	if issuerKind == "" || issuerModel == "" {
		return 0
	}
	for i := range messages {
		m := &messages[i]
		if m.Role != wire.RoleAssistant || len(m.ResponsesReasoning) == 0 {
			continue
		}
		var kept []json.RawMessage
		for _, raw := range m.ResponsesReasoning {
			item, err := wire.ParseValue(raw)
			if err != nil || !item.IsObject() || item.Get("type").Text() == "compaction" {
				kept = append(kept, raw)
				continue
			}
			stamp, model := item.Get("_issuer_kind"), item.Get("_issuer_model")
			kind := canonicalIssuer(stamp.Text())
			unstamped := stamp.Kind() <= wire.KindNull
			exact := !unstamped && kind == issuerKind && model.IsString() && model.Text() == issuerModel
			if !exact && (unstamped || kind == issuerKind) && modelCanReadBlob(issuerKind, model, issuerModel) {
				removed++
				continue
			}
			kept = append(kept, raw)
		}
		m.ResponsesReasoning = kept
	}
	return removed
}

// Reports whether a failure is the endpoint refusing the all-turns reasoning context
func AllTurnsRejected(err error) bool {
	var apiErr *wire.APIError
	if errors.As(err, &apiErr) && apiErr.Status != 400 && apiErr.Status != 0 {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "reasoning.context") || strings.Contains(text, "all_turns")
}
