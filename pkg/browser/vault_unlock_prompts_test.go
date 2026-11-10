package browser

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestVaultPromptCallbacksLiveInThePerThreadSlot(t *testing.T) {
	ctx := tools.WithThreadLocals(context.Background())
	if GetUnlockPromptCallback(ctx) != nil || GetCodePromptCallback(ctx) != nil || GetSaveLoginPromptCallback(ctx) != nil {
		t.Fatal("a fresh thread has no prompts")
	}
	SetUnlockPromptCallback(ctx, func(backend, display string) string { return "pw:" + backend + ":" + display })
	SetCodePromptCallback(ctx, func(site, hint string) string { return "code:" + site + ":" + hint })
	SetSaveLoginPromptCallback(ctx, func(origin, site string) map[string]string { return map[string]string{"identifier": origin, "password": site} })
	if GetUnlockPromptCallback(ctx)("op", "1Password") != "pw:op:1Password" || GetCodePromptCallback(ctx)("github", "sms") != "code:github:sms" {
		t.Fatal("the installed prompts must answer")
	}
	if saved := GetSaveLoginPromptCallback(ctx)("https://x", "x"); saved["identifier"] != "https://x" || saved["password"] != "x" {
		t.Fatalf("saved %v", saved)
	}
	other := tools.WithThreadLocals(context.Background())
	if GetUnlockPromptCallback(other) != nil {
		t.Fatal("another thread must not see the prompt")
	}
	SetUnlockPromptCallback(ctx, nil)
	SetCodePromptCallback(ctx, nil)
	SetSaveLoginPromptCallback(ctx, nil)
	if GetUnlockPromptCallback(ctx) != nil || GetCodePromptCallback(ctx) != nil || GetSaveLoginPromptCallback(ctx) != nil {
		t.Fatal("nil must clear the slot")
	}
}

func TestVaultPromptCallbacksPropagateToWorkerThreads(t *testing.T) {
	ctx := tools.WithThreadLocals(context.Background())
	SetUnlockPromptCallback(ctx, func(string, string) string { return "inherited" })
	worker := tools.PropagateContextToThread(ctx, nil, func(workerCtx context.Context, _ *tools.Env) (any, error) {
		cb := GetUnlockPromptCallback(workerCtx)
		if cb == nil {
			return "", nil
		}
		return cb("op", "1Password"), nil
	})
	got, err := worker()
	if err != nil || got != "inherited" {
		t.Fatalf("got %v %v", got, err)
	}
}
