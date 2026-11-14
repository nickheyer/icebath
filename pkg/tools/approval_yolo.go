package tools

import "log/slog"

// MCPTransferSession moves a chat's MCP spend total to a new session key, registered by the mcp package
var MCPTransferSession func(oldKey, newKey string)

// Writes the bypass flag through the surface's persister, a failed write only costs the restore
func persistSessionYolo(persist func(enabled bool) error, enabled bool, sessionKey string) {
	if persist == nil {
		return
	}
	if err := persist(enabled); err != nil {
		slog.Default().Warn("failed to persist session yolo", "enabled", enabled, "session", sessionKey, "error", err)
	}
}

// ToggleSessionYolo flips or sets the bypass for a session and returns the new state
func ToggleSessionYolo(sessionKey string, enabled *bool, persisted bool, persist func(enabled bool) error) bool {
	state := false
	if enabled == nil {
		state = !(persisted || IsSessionYoloEnabled(sessionKey))
	} else {
		state = *enabled
	}
	persistSessionYolo(persist, state, sessionKey)
	if state {
		EnableSessionYolo(sessionKey)
	} else {
		DisableSessionYolo(sessionKey)
	}
	return state
}

// RestoreSessionYolo re arms a persisted bypass in a fresh process, true when this call turned it on
func RestoreSessionYolo(sessionKey string, persisted bool) bool {
	if sessionKey == "" || !persisted || yoloModeFrozen || IsSessionYoloEnabled(sessionKey) {
		return false
	}
	EnableSessionYolo(sessionKey)
	return true
}

// ApplyLaunchYolo seeds a gateway owned session's launch bypass once per session boundary
func ApplyLaunchYolo(sessionKey string) {
	if sessionKey == "" || yoloModeFrozen {
		return
	}
	if !MarkLaunchYoloApplied(sessionKey) {
		return
	}
	EnableSessionYolo(sessionKey)
}

// RestoreGatewayYolo restores the authoritative routing value, including off, before launch seeding
func RestoreGatewayYolo(sessionKey string, persisted *bool, launch bool) {
	if sessionKey == "" {
		return
	}
	if persisted == nil {
		if launch {
			ApplyLaunchYolo(sessionKey)
		}
		return
	}
	approvalState.Lock()
	defer approvalState.Unlock()
	if approvalState.launchYoloApplied[sessionKey] {
		return
	}
	approvalState.launchYoloApplied[sessionKey] = true
	if *persisted {
		approvalState.sessionYolo[sessionKey] = true
	} else {
		delete(approvalState.sessionYolo, sessionKey)
	}
}

// TransferSessionYolo moves the live bypass and the MCP spend total to a continuation session id
func TransferSessionYolo(oldKey, newKey string) {
	if MCPTransferSession != nil {
		MCPTransferSession(oldKey, newKey)
	}
	if oldKey == "" || newKey == "" || oldKey == newKey || !IsSessionYoloEnabled(oldKey) {
		return
	}
	EnableSessionYolo(newKey)
	DisableSessionYolo(oldKey)
}

// WithSessionYolo returns the model config of a session row being created carrying a live bypass
func WithSessionYolo(modelConfig map[string]any, sessionKey string) map[string]any {
	if !IsSessionYoloEnabled(sessionKey) {
		return modelConfig
	}
	out := map[string]any{}
	for k, v := range modelConfig {
		out[k] = v
	}
	out["yolo_mode"] = true
	return out
}
