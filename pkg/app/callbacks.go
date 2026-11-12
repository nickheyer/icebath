package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/providerregistry"
)

// Repaints the CLI straight away so a secret panel state change shows
func invalidateCLI(c *CLI) { c.paintNow() }

// Drops stale draft input so Enter never stores it as the secret
func clearSecretInput(c *CLI) { c.clearSecretInputBuffer() }

// Saver of a captured secret, swapped while a gateway secret request captures the value instead
type secretSaver func(ctx context.Context, h, key, value string) (config.SecureSaveResult, error)

var (
	secretSaverMu     sync.Mutex
	secretSaverActive secretSaver = config.SaveEnvValueSecure
)

// Saver the next secret capture stores through
func currentSecretSaver() secretSaver {
	secretSaverMu.Lock()
	defer secretSaverMu.Unlock()
	return secretSaverActive
}

// Installs a saver and returns the one it replaced
func swapSecretSaver(saver secretSaver) secretSaver {
	secretSaverMu.Lock()
	defer secretSaverMu.Unlock()
	previous := secretSaverActive
	secretSaverActive = saver
	return previous
}

// Result of a secret capture that stored nothing
func skippedSecret(varName, reason, message string) map[string]any {
	return map[string]any{"success": true, "reason": reason, "stored_as": varName, "validated": false, "skipped": true, "message": message}
}

// Stores the value, or reports a skip when empty, and builds the callback result
func secretResult(c *CLI, varName, value string) (map[string]any, error) {
	if value == "" {
		cprint("\n" + cliui.Dim + "  ⏭ Secret entry skipped" + cliui.Reset)
		return skippedSecret(varName, "cancelled", "Secret setup was skipped."), nil
	}
	h := home.Dir(c.ctx)
	stored, err := currentSecretSaver()(c.ctx, h, varName, value)
	if err != nil {
		var valueErr *providerregistry.ValueError
		if errors.As(err, &valueErr) {
			cprint("\n  ✗ " + err.Error())
		}
		return nil, err
	}
	cprint("\n" + cliui.Dim + "  ✓ Stored secret in " + home.Display(h) + "/.env as " + varName + cliui.Reset)
	return map[string]any{"success": stored.Success, "stored_as": stored.StoredAs, "validated": stored.Validated, "skipped": false,
		"message": "Secret stored securely. The secret value was not exposed to the model."}, nil
}

// Prompts for a secret value through the TUI or the masked console prompt and stores it in the home .env
func promptForSecret(c *CLI, varName, prompt string, metadata map[string]any) (map[string]any, error) {
	if c.app == nil {
		value, err := console().MaskedSecret(prompt + " (hidden, ESC or empty Enter to skip): ")
		if err != nil {
			if !errors.Is(err, cliui.ErrInterrupt) && !errors.Is(err, cliui.ErrEOF) {
				return nil, err
			}
			value = ""
		}
		return secretResult(c, varName, value)
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	response := make(chan string, 1)
	c.uiMu.Lock()
	c.secretState = &secretModalState{VarName: varName, Prompt: prompt, Metadata: metadata, response: response}
	c.secretDeadline = monotonic() + 120
	c.uiMu.Unlock()
	c.ringBell(true, "secret needed ("+varName+")", "")
	clearSecretInput(c)
	invalidateCLI(c)
	timer := time.NewTimer(modalPollInterval)
	defer timer.Stop()
	for {
		select {
		case value := <-response:
			c.uiMu.Lock()
			c.secretState, c.secretDeadline = nil, 0
			c.uiMu.Unlock()
			invalidateCLI(c)
			return secretResult(c, varName, value)
		case <-timer.C:
		}
		c.uiMu.Lock()
		expired := c.secretDeadline-monotonic() <= 0
		c.uiMu.Unlock()
		if expired {
			break
		}
		invalidateCLI(c)
		timer.Reset(modalPollInterval)
	}
	c.uiMu.Lock()
	c.secretState, c.secretDeadline = nil, 0
	c.uiMu.Unlock()
	clearSecretInput(c)
	invalidateCLI(c)
	cprint("\n" + cliui.Dim + "  ⏱ Timeout — secret capture cancelled" + cliui.Reset)
	return skippedSecret(varName, "timeout", "Secret setup timed out and was skipped."), nil
}
