package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Keeps the fallback entries that are dicts naming both a provider and a model
func fallbackEntries(fallbackModel []map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, f := range fallbackModel {
		if f != nil && pyrepr.Truthy(f["provider"]) && pyrepr.Truthy(f["model"]) {
			out = append(out, f)
		}
	}
	return out
}

// Rebinds the stable credential pool entry id from the current pool and key, cleared without a pool
func syncCredentialPoolEntryID(a *AIAgent) {
	defer func() {
		if recover() != nil {
			a.credentialPoolEntryID = ""
		}
	}()
	a.credentialPoolEntryID = ""
	if a.credentialPool != nil {
		a.credentialPoolEntryID = a.credentialPool.EntryIDForAPIKey(a.apiKey)
	}
}

// Sets up the ordered backup chain tried when the primary is exhausted and prints it unless quiet
func initFallbackChain(a *AIAgent, fallbackModel []map[string]any) {
	syncCredentialPoolEntryID(a)
	a.fallbackChain = fallbackEntries(fallbackModel)
	a.fallbackIndex = 0
	chain := a.fallbackChain
	if len(chain) == 0 || a.quietMode {
		return
	}
	labels := make([]string, len(chain))
	for i, f := range chain {
		labels[i] = fmt.Sprintf("%s (%s)", pyrepr.Str(f["model"]), pyrepr.Str(f["provider"]))
	}
	if len(chain) == 1 {
		_, _ = fmt.Fprintln(statusStdout, "\U0001f504 Fallback model: "+labels[0])
		return
	}
	_, _ = fmt.Fprintf(statusStdout, "\U0001f504 Fallback chain (%d providers): %s\n", len(chain), strings.Join(labels, " → "))
}

// First fallback entry, the legacy single fallback model external callers read
func (a *AIAgent) FallbackModel() map[string]any {
	if len(a.fallbackChain) == 0 {
		return nil
	}
	return a.fallbackChain[0]
}

// Gives an init time fallback its own api mode, a detection failure keeping the primary mode
func recomputeInitFallbackAPIMode(ctx context.Context, a *AIAgent, fbBaseURL string) {
	mode, err := provider.FallbackAPIModeResolved(ctx, a, a.provider, a.model, fbBaseURL)
	if err != nil {
		runAgentLog().Debug("Init-time fallback api_mode detection failed for "+a.provider, "error", err)
		return
	}
	a.apiMode = mode
	clear(a.transportCache)
}

var _ provider.FallbackAPIModeAgent = (*AIAgent)(nil)

func (a *AIAgent) IsAzureOpenAIURL(baseURL string) bool { return a.isAzureOpenAIURL(&baseURL) }

func (a *AIAgent) IsDirectOpenAIURL(baseURL string) bool { return a.isDirectOpenAIURL(&baseURL) }

func (a *AIAgent) ProviderModelRequiresResponsesAPI(model, providerName string) bool {
	return providerModelRequiresResponsesAPI(a.homeContext(), model, providerName)
}
