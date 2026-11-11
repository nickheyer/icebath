package gatewayrun

// Canonical command name of a typed slash name or alias, false when no command is registered under it
var ResolveCommand func(name string) (string, bool)

// Whether a destructive slash command confirmation is pending for the session
var SlashConfirmPending func(sessionKey string) bool
