package browser

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestVaultPromptCallbacksLiveInThePerThreadSlot(t *testing.T) {
	ctx := tools.WithThreadLocals(context.Background())
	if tools.GetUnlockPromptCallback(ctx) != nil || tools.GetCodePromptCallback(ctx) != nil || tools.GetSaveLoginPromptCallback(ctx) != nil {
		t.Fatal("a fresh thread has no prompts")
	}
	tools.SetUnlockPromptCallback(ctx, func(backend, display string) string { return "pw:" + backend + ":" + display })
	tools.SetCodePromptCallback(ctx, func(site, hint string) string { return "code:" + site + ":" + hint })
	tools.SetSaveLoginPromptCallback(ctx, func(origin, site string) map[string]string {
		return map[string]string{"identifier": origin, "password": site}
	})
	if tools.GetUnlockPromptCallback(ctx)("op", "1Password") != "pw:op:1Password" || tools.GetCodePromptCallback(ctx)("github", "sms") != "code:github:sms" {
		t.Fatal("the installed prompts must answer")
	}
	if saved := tools.GetSaveLoginPromptCallback(ctx)("https://x", "x"); saved["identifier"] != "https://x" || saved["password"] != "x" {
		t.Fatalf("saved %v", saved)
	}
	other := tools.WithThreadLocals(context.Background())
	if tools.GetUnlockPromptCallback(other) != nil {
		t.Fatal("another thread must not see the prompt")
	}
	tools.SetUnlockPromptCallback(ctx, nil)
	tools.SetCodePromptCallback(ctx, nil)
	tools.SetSaveLoginPromptCallback(ctx, nil)
	if tools.GetUnlockPromptCallback(ctx) != nil || tools.GetCodePromptCallback(ctx) != nil || tools.GetSaveLoginPromptCallback(ctx) != nil {
		t.Fatal("nil must clear the slot")
	}
}

func TestVaultPromptCallbacksPropagateToWorkerThreads(t *testing.T) {
	ctx := tools.WithThreadLocals(context.Background())
	tools.SetUnlockPromptCallback(ctx, func(string, string) string { return "inherited" })
	worker := tools.PropagateContextToThread(ctx, nil, func(workerCtx context.Context, _ *tools.Env) (any, error) {
		cb := tools.GetUnlockPromptCallback(workerCtx)
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
