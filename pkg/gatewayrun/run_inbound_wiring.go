package gatewayrun

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Canonical command name of a typed slash name or alias, false when no command is registered under it
var ResolveCommand func(name string) (string, bool)

// Whether a destructive slash command confirmation is pending for the session
var SlashConfirmPending func(sessionKey string) bool

// Confirm id of the slash confirm pending for the session, false when none is
var SlashConfirmGetPending func(sessionKey string) (string, bool)

// Runs the pending slash confirm handler with a choice, empty when stale, superseded or already resolved
var SlashConfirmResolve func(ctx context.Context, sessionKey, confirmID, choice string) string

// Drops a pending slash confirm older than the default timeout, true when it did
var SlashConfirmClearIfStale func(sessionKey string) bool

// Records a pending slash confirm whose handler runs with the chosen answer, superseding any prior one
var SlashConfirmRegister func(sessionKey, confirmID, command string, handler func(ctx context.Context, choice string) (string, error))

// Writes one dotted config key into config.yaml, false when the write did not land
var SaveConfigValue func(ctx context.Context, keyPath string, value any) bool

// The init prompt for the session's active directory, its AGENTS.md read when present
var BuildInitPromptForCwd func(env *tools.Env, cwd, extra string, sessionKey *string) string

// Canonical name, busy policy and busy handler key of a registered command, false when none is registered
var ResolveCommandBusy func(name string) (canonical, busyPolicy, busyHandler string, ok bool)
