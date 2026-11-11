package gatewayrun

import (
	"context"
	"encoding/json"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Seconds each vault prompt waits for a viewer, the main sidecar deadlines
const (
	vaultUnlockTimeoutS    = 120.0
	vaultSaveLoginTimeoutS = 180.0
	vaultCodeTimeoutS      = 180.0
)

// Installs the three vault prompts and the unlock owner on this thread for a local viewer turn and returns their restore
func vaultPromptScope(ctx context.Context, t *TurnRunner, a *agent.AIAgent) func() {
	if !localViewerTurn(t) {
		return func() {}
	}
	authority, sessionID := t.approvalOwner.authority, t.approvalOwner.sessionID
	ask := func(method string, params *pyjson.Object, timeout float64) string {
		live, ok := authority.Live(sessionID)
		if !ok || live == nil {
			return ""
		}
		return gateway.AskViewer(live.EventStream, sessionID, method, params, a.InterruptRequested, &timeout, true)
	}
	unlockPrompt := func(backend, displayName string) string {
		return ask("vault.unlock_prompt", pyjson.ObjectOf("backend", backend, "display_name", displayName), vaultUnlockTimeoutS)
	}
	saveLogin := func(origin, site string) map[string]string {
		raw := ask("vault.save_login", pyjson.ObjectOf("origin", origin, "site", site), vaultSaveLoginTimeoutS)
		if raw == "" {
			return nil
		}
		var data any
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			return nil
		}
		return vaultLoginAnswer(data)
	}
	codePrompt := func(site, hint string) string {
		return ask("vault.code", pyjson.ObjectOf("site", site, "hint", hint), vaultCodeTimeoutS)
	}
	prevUnlock, prevSave, prevCode := browser.GetUnlockPromptCallback(ctx), browser.GetSaveLoginPromptCallback(ctx), browser.GetCodePromptCallback(ctx)
	prevSession := browser.VaultSessionID(ctx)
	browser.SetUnlockPromptCallback(ctx, unlockPrompt)
	browser.SetSaveLoginPromptCallback(ctx, saveLogin)
	browser.SetCodePromptCallback(ctx, codePrompt)
	browser.SetVaultSessionID(ctx, &sessionID)
	return func() {
		browser.SetUnlockPromptCallback(ctx, prevUnlock)
		browser.SetSaveLoginPromptCallback(ctx, prevSave)
		browser.SetCodePromptCallback(ctx, prevCode)
		browser.SetVaultSessionID(ctx, prevSession)
	}
}

// Login a save login card answered, nil unless it is an object with a truthy password
func vaultLoginAnswer(data any) map[string]string {
	obj, ok := data.(map[string]any)
	if !ok || !pyrepr.Truthy(obj["password"]) {
		return nil
	}
	out := make(map[string]string, len(obj))
	for key, value := range obj {
		if s, isStr := value.(string); isStr {
			out[key] = s
		} else if value != nil {
			out[key] = pyrepr.Str(value)
		}
	}
	return out
}
