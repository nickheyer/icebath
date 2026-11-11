package browser

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/vault"
)

const pageSecret = "s3cret-pw"

type pageCall struct {
	expression string
	secret     bool
}

// Plugin browser double holding every task's page and recording each evaluation
type vaultFakePage struct {
	provider.ProviderDefaults
	url        string
	calls      []pageCall
	failSecret bool
}

func (*vaultFakePage) Name() string        { return "fakepage" }
func (*vaultFakePage) HasPage(string) bool { return true }

func (p *vaultFakePage) Evaluate(_, expression string, secret bool) (any, error) {
	p.calls = append(p.calls, pageCall{expression, secret})
	if secret && p.failSecret {
		return nil, errors.New("page.evaluate failed: " + expression)
	}
	if strings.Contains(expression, "location.href") {
		return p.url, nil
	}
	if strings.Contains(expression, pageSecret) {
		return `{"filled": 1}`, nil
	}
	return loginControlsJSON, nil
}

func pageBackendFixture(t *testing.T) (context.Context, *tools.Env, *vaultFakePage, vault.VaultItemMeta) {
	t.Helper()
	ctx, env, store, _ := vaultStore(t)
	pin(t, &vaultEnsureSupervisor, func(context.Context, *tools.Env, string) vaultSupervisor {
		t.Error("CDP supervisor used while a page backend holds the task's page")
		return nil
	})
	fake := &vaultFakePage{url: "https://example.com/login"}
	fake.Self = fake
	BrowserPageBackendRegistry.ResetForTests()
	t.Cleanup(BrowserPageBackendRegistry.ResetForTests)
	if err := BrowserPageBackendRegistry.Register(fake, ""); err != nil {
		t.Fatal(err)
	}
	meta, err := store.AddItem("login", "Example", pyjson.ObjectOf("identifier_type", "email", "identifier", "a@example.com",
		"password", pageSecret), "https://example.com")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, env, fake, meta
}

func TestFillRunsOnThePageBackendAndRefusesAnotherOrigin(t *testing.T) {
	ctx, env, fake, meta := pageBackendFixture(t)
	raw, err := browserVaultFill(ctx, env, meta.ID, "")
	out := loadResult(t, raw, err)
	if anyGet(out, "success") != true || anyGet(out, "filled_fields") != int64(1) || strings.Contains(raw, pageSecret) {
		t.Fatalf("fill %s", raw)
	}
	secretCalls := 0
	for _, call := range fake.calls {
		if strings.Contains(call.expression, pageSecret) {
			secretCalls++
			if !call.secret {
				t.Fatal("the secret script ran outside the secret path")
			}
		}
	}
	if secretCalls != 1 {
		t.Fatalf("secret calls %d", secretCalls)
	}

	fake.calls = nil
	fake.url = "https://evil.example/login"
	raw, err = browserVaultFill(ctx, env, meta.ID, "")
	if out := loadResult(t, raw, err); anyGet(out, "error_type") != "origin_mismatch" {
		t.Fatalf("fill %s", raw)
	}
	for _, call := range fake.calls {
		if strings.Contains(call.expression, pageSecret) {
			t.Fatal("the secret reached a page on another origin")
		}
	}
}

func TestSecretFailureNeverEchoesTheScript(t *testing.T) {
	ctx, env, fake, meta := pageBackendFixture(t)
	fake.failSecret = true
	raw, err := browserVaultFill(ctx, env, meta.ID, "")
	if out := loadResult(t, raw, err); anyGet(out, "success") != false || strings.Contains(raw, pageSecret) {
		t.Fatalf("fill %s", raw)
	}
}
