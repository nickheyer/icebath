package browser

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/vault"
)

const (
	guardPassword = "dummy-password-7"
	guardOTPSeed  = "JBSWY3DPEHPK3PXP"

	guardLoginControls = `[{"autocomplete": "username", "formIndex": 0, "index": 0, "label": "", "name": "user", "type": "text"}, ` +
		`{"autocomplete": "current-password", "formIndex": 0, "index": 1, "label": "", "name": "pw", "type": "password"}]`
	guardOTPControls = `[{"autocomplete": "one-time-code", "formIndex": 0, "index": 0, "label": "Authentication code", ` +
		`"name": "otp", "type": "text"}]`
	guardCardControls = `[{"autocomplete": "cc-number", "index": 0, "type": "text"}, {"autocomplete": "cc-exp", "index": 1, "type": "text"}, ` +
		`{"autocomplete": "cc-csc", "index": 2, "type": "text"}]`
	guardAddressControls = `[{"autocomplete": "address-line1", "index": 0, "type": "text"}, {"autocomplete": "postal-code", "index": 1, "type": "text"}]`
)

// The supervisor's view of one open tab, recording every secret bearing fill script as a write
type fakePage struct {
	href, controls string
	writes         []string
}

func (p *fakePage) focusPage(origin, _ string, _ float64) *pyjson.Object {
	current, _ := vault.NormalizeOrigin(p.href)
	return pyjson.ObjectOf("ok", origin != "" && current == origin, "url", p.href)
}

func (p *fakePage) evaluateRuntime(expression string, _, _ bool, _ float64) *pyjson.Object {
	if strings.TrimSpace(expression) == "window.location.href" {
		return pyjson.ObjectOf("ok", true, "result", p.href)
	}
	if strings.Contains(expression, "expectedOrigin") {
		p.writes = append(p.writes, expression)
		return pyjson.ObjectOf("ok", true, "result", `{"filled": 1}`)
	}
	return pyjson.ObjectOf("ok", true, "result", p.controls)
}

// The real local backend recording which secret kinds were resolved
type recordingBackend struct {
	*LocalLoginBackend
	resolved []string
}

func (b *recordingBackend) ResolvePassword(handle string) (string, error) {
	b.resolved = append(b.resolved, "password")
	return b.LocalLoginBackend.ResolvePassword(handle)
}

func (b *recordingBackend) ResolveOTP(handle string) (string, bool, error) {
	b.resolved = append(b.resolved, "otp")
	return b.LocalLoginBackend.ResolveOTP(handle)
}

func (b *recordingBackend) ResolveSecret(handle string) (map[string]string, error) {
	b.resolved = append(b.resolved, "secret")
	return b.LocalLoginBackend.ResolveSecret(handle)
}

type guardVault struct {
	ctx     context.Context
	env     *tools.Env
	store   *vault.VaultStore
	backend *recordingBackend
	asked   *[]string
}

func guardFixture(t *testing.T) guardVault {
	t.Helper()
	ctx, env, store, _ := vaultStore(t)
	backend := &recordingBackend{LocalLoginBackend: NewLocalLoginBackend(ctx)}
	backend.Self = backend
	asked := &[]string{}
	SetCodePromptCallback(ctx, func(site, _ string) string {
		*asked = append(*asked, site)
		return "000000"
	})
	t.Cleanup(func() {
		SetCodePromptCallback(ctx, nil)
		SetSaveLoginPromptCallback(ctx, nil)
	})
	pin(t, &loginBackendForHandle, func(context.Context, string) (LoginBackend, error) { return backend, nil })
	pin(t, &vaultCanPromptHere, func(context.Context, *tools.Env) bool { return true })
	return guardVault{ctx, env, store, backend, asked}
}

func onPage(t *testing.T, page *fakePage) {
	t.Helper()
	pin(t, &vaultEnsureSupervisor, func(context.Context, *tools.Env, string) vaultSupervisor { return page })
}

func guardLogin(t *testing.T, store *vault.VaultStore, otp bool) vault.VaultItemMeta {
	t.Helper()
	secret := pyjson.ObjectOf("identifier_type", "username", "identifier", "tester", "password", guardPassword)
	if otp {
		secret.Set("otp_secret", guardOTPSeed)
	}
	meta, err := store.AddItem("login", "Bank", secret, "https://bank.example")
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func refusedBeforeResolution(t *testing.T, out any, backend *recordingBackend, page *fakePage) {
	t.Helper()
	if anyGet(out, "success") != false || anyGet(out, "error_type") != "origin_mismatch" {
		t.Fatalf("not an origin refusal: %v", out)
	}
	if len(backend.resolved) != 0 || len(page.writes) != 0 {
		t.Fatalf("resolved %v writes %d", backend.resolved, len(page.writes))
	}
}

func TestOneTimeCodeSavedCodeIsNotMintedForAPageOnAnotherOrigin(t *testing.T) {
	v := guardFixture(t)
	meta := guardLogin(t, v.store, true)
	page := &fakePage{href: "https://elsewhere.example/verify", controls: guardOTPControls}
	onPage(t, page)
	text, err := browserVaultEnterCode(v.ctx, v.env, meta.ID, "t")
	refusedBeforeResolution(t, loadResult(t, text, err), v.backend, page)
	if len(*v.asked) != 0 {
		t.Fatalf("a refusal switched to asking the user: %v", *v.asked)
	}
}

func TestOneTimeCodeSavedCodeIsMintedOnTheSavedOriginAndPinnedToIt(t *testing.T) {
	v := guardFixture(t)
	meta := guardLogin(t, v.store, true)
	page := &fakePage{href: "https://bank.example/2fa", controls: guardOTPControls}
	onPage(t, page)
	text, err := browserVaultEnterCode(v.ctx, v.env, meta.ID, "t")
	out := loadResult(t, text, err)
	if anyGet(out, "success") != true || anyGet(out, "source") != "local" || anyGet(out, "origin") != "https://bank.example" {
		t.Fatalf("enter_code %s", text)
	}
	if !reflect.DeepEqual(v.backend.resolved, []string{"otp"}) || len(*v.asked) != 0 {
		t.Fatalf("resolved %v asked %v", v.backend.resolved, *v.asked)
	}
	if len(page.writes) != 1 || !strings.Contains(page.writes[0], `"https://bank.example"`) {
		t.Fatalf("writes %v", page.writes)
	}
}

func TestOneTimeCodeACodeWithoutASavedItemIsStillAskedOfTheUser(t *testing.T) {
	v := guardFixture(t)
	page := &fakePage{href: "https://elsewhere.example/verify", controls: guardOTPControls}
	onPage(t, page)
	text, err := browserVaultEnterCode(v.ctx, v.env, "", "t")
	out := loadResult(t, text, err)
	if anyGet(out, "success") != true || anyGet(out, "source") != "user" {
		t.Fatalf("enter_code %s", text)
	}
	if !reflect.DeepEqual(*v.asked, []string{"elsewhere.example"}) || len(v.backend.resolved) != 0 {
		t.Fatalf("asked %v resolved %v", *v.asked, v.backend.resolved)
	}
}

func TestFillPasswordIsNotResolvedForAPageOnAnotherOrigin(t *testing.T) {
	v := guardFixture(t)
	meta := guardLogin(t, v.store, false)
	page := &fakePage{href: "https://elsewhere.example/login", controls: guardLoginControls}
	onPage(t, page)
	text, err := browserVaultFill(v.ctx, v.env, meta.ID, "t")
	refusedBeforeResolution(t, loadResult(t, text, err), v.backend, page)
}

func TestFillCardIsRefusedBeforeTheUserIsAskedToConfirm(t *testing.T) {
	v := guardFixture(t)
	meta, err := v.store.AddItem("payment", "Card", pyjson.ObjectOf("card_number", "4111111111111111", "cardholder_name", "Test User",
		"exp_month", "07", "exp_year", "2029", "cvc", "123", "billing_postal_code", "94107"), "https://shop.example")
	if err != nil {
		t.Fatal(err)
	}
	page := &fakePage{href: "https://elsewhere.example/checkout", controls: guardCardControls}
	onPage(t, page)
	consents := 0
	pin(t, &vaultElicitConsent, func(context.Context, *tools.Env, string, string, tools.ElicitationOptions) string {
		consents++
		return "accept"
	})
	text, err := browserVaultFill(v.ctx, v.env, meta.ID, "t")
	refusedBeforeResolution(t, loadResult(t, text, err), v.backend, page)
	if consents != 0 {
		t.Fatal("the user was asked to confirm a card on the wrong origin")
	}
}

func TestFillAddressIsNotResolvedForAPageOnAnotherOrigin(t *testing.T) {
	v := guardFixture(t)
	meta, err := v.store.AddItem("address", "Home", pyjson.ObjectOf("address_line1", "1 Test Way", "city", "Testville", "state", "CA",
		"postal_code", "94107", "country", "US"), "https://shop.example")
	if err != nil {
		t.Fatal(err)
	}
	page := &fakePage{href: "https://elsewhere.example/shipping", controls: guardAddressControls}
	onPage(t, page)
	text, err := browserVaultFill(v.ctx, v.env, meta.ID, "t")
	refusedBeforeResolution(t, loadResult(t, text, err), v.backend, page)
}

func TestFillSaveLoginFillIsRefusedWhenThePageLeftTheSavedOrigin(t *testing.T) {
	v := guardFixture(t)
	page := &fakePage{href: "https://bank.example/login", controls: guardLoginControls}
	onPage(t, page)
	SetSaveLoginPromptCallback(v.ctx, func(string, string) map[string]string {
		page.href = "https://elsewhere.example/login"
		return map[string]string{"identifier": "tester", "password": guardPassword}
	})
	text, err := browserVaultSaveLogin(v.ctx, v.env, "", "t")
	out := loadResult(t, text, err)
	items, err := v.store.ListItems()
	if err != nil || len(items) != 1 || items[0].Origin != "https://bank.example" {
		t.Fatalf("items %+v %v", items, err)
	}
	refusedBeforeResolution(t, anyGet(out, "fill"), v.backend, page)
}
