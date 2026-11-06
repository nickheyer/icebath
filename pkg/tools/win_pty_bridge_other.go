//go:build !windows

package tools

import "context"

// Reports whether a ConPTY bridge can be spawned, only on Windows
func WinPtyBridgeAvailable() bool { return false }

// Refuses off Windows where the POSIX pseudo terminal bridge serves the dashboard
func SpawnWinPtyBridge(ctx context.Context, env *Env, argv []string, o PtySpawnOptions) (TerminalBridge, error) {
	return nil, &PtyUnavailableError{Msg: "ConPTY is unavailable on this platform."}
}

// Spawns the dashboard terminal bridge of this platform, the POSIX pseudo terminal here
func SpawnTerminalBridge(ctx context.Context, env *Env, argv []string, o PtySpawnOptions) (TerminalBridge, error) {
	return SpawnPtyBridge(ctx, env, argv, o)
}

// Reports whether this platform's dashboard terminal bridge can spawn
func TerminalBridgeAvailable() bool { return PtyBridgeAvailable() }
