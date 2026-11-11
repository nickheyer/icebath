package app

import "github.com/nickheyer/icebath/pkg/browser"

// Drops the profile's password manager session tokens when the CLI session closes
func lockVaultSessions(c *CLI) { browser.LockVault(c.ctx, nil) }
