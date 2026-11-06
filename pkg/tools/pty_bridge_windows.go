//go:build windows

package tools

import "context"

// Reports whether a POSIX pseudo terminal can be spawned, never on Windows
func PtyBridgeAvailable() bool { return false }

// Refuses on Windows where the dashboard uses the ConPTY bridge instead
func SpawnPtyBridge(ctx context.Context, env *Env, argv []string, o PtySpawnOptions) (TerminalBridge, error) {
	return nil, &PtyUnavailableError{Msg: "Pseudo-terminals are unavailable on this platform. Icebath Agent supports Windows only via WSL."}
}
