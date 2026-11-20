package agent

import (
	"net/http"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/provider"
)

var servedModelHeaders = []string{"x-litellm-model-id", "x-litellm-model-api-base"}

const servedModelHookMark = "_icebath_served_model_hook"

// Response hook installer seam over the SDK client event hooks the provider lane owns
var addClientResponseHook = provider.AddOpenAIClientResponseHook

// Served model of the last 2xx response guarded for the response hook goroutines
type servedModelState struct {
	mu    sync.Mutex
	model *string
}

var servedModelInitMu sync.Mutex

// First non empty served model header stripped, nil when the response carried none
func servedModelFromHeaders(headers http.Header) *string {
	if headers == nil {
		return nil
	}
	for _, name := range servedModelHeaders {
		if value := headers.Get(name); value != "" {
			stripped := strings.TrimSpace(value)
			return &stripped
		}
	}
	return nil
}

// Per agent served model slot, allocated once under the package lock
func (a *AIAgent) servedModelSlot() *servedModelState {
	servedModelInitMu.Lock()
	defer servedModelInitMu.Unlock()
	if a.servedModel == nil {
		a.servedModel = &servedModelState{}
	}
	return a.servedModel
}

// Proxy reported deployment of the last successful response, nil when it named none
func (a *AIAgent) LastServedModel() *string {
	slot := a.servedModelSlot()
	slot.mu.Lock()
	defer slot.mu.Unlock()
	return slot.model
}

// Records the served model header of every 2xx response on the client, one hook per client
func installServedModelCapture(a *AIAgent, client any) {
	slot := a.servedModelSlot()
	addClientResponseHook(client, servedModelHookMark, func(resp *http.Response) {
		if resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return
		}
		model := servedModelFromHeaders(resp.Header)
		slot.mu.Lock()
		slot.model = model
		slot.mu.Unlock()
	})
}

// Requested and served model of the turn result, the fallback route answering when no proxy header did
func resultModelFields(a *AIAgent) map[string]any {
	served := a.LastServedModel()
	requested := a.model
	if (served == nil || *served == "") && a.fallbackActivated {
		primary := strings.TrimSpace(pyStr(pyOr(a.primaryRuntime["model"], "")))
		if primary != "" && primary != a.model {
			requested, served = primary, &a.model
		}
	}
	var servedValue any
	if served != nil && *served != "" {
		servedValue = *served
	}
	return map[string]any{"requested_model": requested, "served_model": servedValue}
}
