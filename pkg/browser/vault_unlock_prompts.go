package browser

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tools"
)

// UnlockPrompt asks the master password of an external password manager, empty when cancelled
type UnlockPrompt func(backendName, displayName string) string

// SaveLoginPrompt asks the identifier and password to save for a site, nil when the user declines
type SaveLoginPrompt func(origin, site string) map[string]string

// CodePrompt asks the one time code a site sent, empty when declined
type CodePrompt func(site, hint string) string

const (
	unlockPromptSlot    = "vault_unlock_prompt"
	codePromptSlot      = "vault_code_prompt"
	saveLoginPromptSlot = "vault_save_login_prompt"
)

// Registers the current surface's masked master password prompt in the per thread slot
func SetUnlockPromptCallback(ctx context.Context, cb UnlockPrompt) {
	if cb == nil {
		tools.ThreadLocalSet(ctx, unlockPromptSlot, nil)
		return
	}
	tools.ThreadLocalSet(ctx, unlockPromptSlot, cb)
}

// The master password prompt of the current thread, nil in headless contexts
func GetUnlockPromptCallback(ctx context.Context) UnlockPrompt {
	cb, _ := tools.ThreadLocalGet(ctx, unlockPromptSlot).(UnlockPrompt)
	return cb
}

// Registers the surface's enter the code prompt in the per thread slot
func SetCodePromptCallback(ctx context.Context, cb CodePrompt) {
	if cb == nil {
		tools.ThreadLocalSet(ctx, codePromptSlot, nil)
		return
	}
	tools.ThreadLocalSet(ctx, codePromptSlot, cb)
}

// The one time code prompt of the current thread, nil in headless contexts
func GetCodePromptCallback(ctx context.Context) CodePrompt {
	cb, _ := tools.ThreadLocalGet(ctx, codePromptSlot).(CodePrompt)
	return cb
}

// Registers the surface's save this login prompt in the per thread slot
func SetSaveLoginPromptCallback(ctx context.Context, cb SaveLoginPrompt) {
	if cb == nil {
		tools.ThreadLocalSet(ctx, saveLoginPromptSlot, nil)
		return
	}
	tools.ThreadLocalSet(ctx, saveLoginPromptSlot, cb)
}

// The save login prompt of the current thread, nil in headless contexts
func GetSaveLoginPromptCallback(ctx context.Context) SaveLoginPrompt {
	cb, _ := tools.ThreadLocalGet(ctx, saveLoginPromptSlot).(SaveLoginPrompt)
	return cb
}

// Hands the three prompt slots to propagated worker threads like the approval and sudo callbacks
func init() {
	tools.RegisterThreadCallback(tools.ThreadCallback{
		Get: func(ctx context.Context) any {
			if cb := GetUnlockPromptCallback(ctx); cb != nil {
				return cb
			}
			return nil
		},
		Set: func(ctx context.Context, cb any) { v, _ := cb.(UnlockPrompt); SetUnlockPromptCallback(ctx, v) }})
	tools.RegisterThreadCallback(tools.ThreadCallback{
		Get: func(ctx context.Context) any {
			if cb := GetCodePromptCallback(ctx); cb != nil {
				return cb
			}
			return nil
		},
		Set: func(ctx context.Context, cb any) { v, _ := cb.(CodePrompt); SetCodePromptCallback(ctx, v) }})
	tools.RegisterThreadCallback(tools.ThreadCallback{
		Get: func(ctx context.Context) any {
			if cb := GetSaveLoginPromptCallback(ctx); cb != nil {
				return cb
			}
			return nil
		},
		Set: func(ctx context.Context, cb any) { v, _ := cb.(SaveLoginPrompt); SetSaveLoginPromptCallback(ctx, v) }})
}
