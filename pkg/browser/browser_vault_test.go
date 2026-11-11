package browser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/vault"
)

// Login backend double whose behaviours a test sets one by one
type fakeLoginBackend struct {
	LoginBackendDefaults
	name, display, prefix string
	needsUnlock           bool
	unlocked              func() bool
	owns                  func(handle string) bool
	listItems             func() ([]vault.VaultItemMeta, error)
	getMeta               func(handle string) (*vault.VaultItemMeta, error)
	resolvePassword       func(handle string) (string, error)
	resolveOTP            func(handle string) (string, bool, error)
	resolveSecret         func(handle string) (map[string]string, error)
	fillContext           func() (func() error, error)
}

func newFakeBackend(name, display string) *fakeLoginBackend {
	b := &fakeLoginBackend{name: name, display: display, prefix: name + ":"}
	b.Self = b
	return b
}

func (b *fakeLoginBackend) Name() string        { return b.name }
func (b *fakeLoginBackend) DisplayName() string { return b.display }
func (b *fakeLoginBackend) Prefix() string      { return b.prefix }
func (b *fakeLoginBackend) NeedsUnlock() bool   { return b.needsUnlock }

func (b *fakeLoginBackend) IsUnlocked() bool {
	if b.unlocked != nil {
		return b.unlocked()
	}
	return true
}

func (b *fakeLoginBackend) Owns(handle string) bool {
	if b.owns != nil {
		return b.owns(handle)
	}
	return strings.HasPrefix(handle, b.prefix)
}

func (b *fakeLoginBackend) ListItems() ([]vault.VaultItemMeta, error) {
	if b.listItems != nil {
		return b.listItems()
	}
	return nil, nil
}

func (b *fakeLoginBackend) GetMeta(handle string) (*vault.VaultItemMeta, error) {
	if b.getMeta != nil {
		return b.getMeta(handle)
	}
	return nil, nil
}

func (b *fakeLoginBackend) ResolvePassword(handle string) (string, error) {
	if b.resolvePassword != nil {
		return b.resolvePassword(handle)
	}
	return "", errors.New("resolve_password is not defined on this backend")
}

func (b *fakeLoginBackend) ResolveOTP(handle string) (string, bool, error) {
	if b.resolveOTP != nil {
		return b.resolveOTP(handle)
	}
	return "", false, nil
}

func (b *fakeLoginBackend) ResolveSecret(handle string) (map[string]string, error) {
	if b.resolveSecret != nil {
		return b.resolveSecret(handle)
	}
	return b.LoginBackendDefaults.ResolveSecret(handle)
}

func (b *fakeLoginBackend) FillContext() (func() error, error) {
	if b.fillContext != nil {
		return b.fillContext()
	}
	return nil, nil
}

func loadResult(t *testing.T, text string, err error) *pyjson.Object {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	parsed, perr := pyjson.Loads(text)
	if perr != nil {
		t.Fatalf("%v in %s", perr, text)
	}
	obj, ok := pyjson.AsObject(parsed)
	if !ok {
		t.Fatalf("not an object: %s", text)
	}
	return obj
}

// Profile home with a vault store under a temp dir that the local backend reads
func vaultStore(t *testing.T) (context.Context, *tools.Env, *vault.VaultStore, string) {
	t.Helper()
	ctx, env := vaultProfile(t)
	dir := filepath.Join(t.TempDir(), "vault")
	store := vault.NewVaultStore(ctx, dir)
	pin(t, &localVaultStore, func(context.Context) *vault.VaultStore { return store })
	t.Cleanup(func() { tools.ClearVaultRedactionValues(ctx) })
	return ctx, env, store, dir
}

func addLogin(t *testing.T, store *vault.VaultStore, origin, password string) vault.VaultItemMeta {
	t.Helper()
	meta, err := store.AddItem("login", "Example login", pyjson.ObjectOf("identifier_type", "email", "identifier", "user@example.com",
		"password", password, "origin", origin), origin)
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func cardSecret() *pyjson.Object {
	return pyjson.ObjectOf("card_number", "4111111111111111", "cardholder_name", "A User", "exp_month", "7", "exp_year", "2029",
		"cvc", "123", "billing_postal_code", "94110")
}

func addressSecret() *pyjson.Object {
	return pyjson.ObjectOf("address_line1", "1 Main St", "city", "Springfield", "postal_code", "12345", "country", "US")
}

func isVaultError(err error) bool {
	var verr *vault.VaultError
	return errors.As(err, &verr)
}

func TestVaultStoreRoundtripEncryptDecrypt(t *testing.T) {
	_, _, store, _ := vaultStore(t)
	meta := addLogin(t, store, "https://example.com", "s3cret-pw")
	secret, err := store.ResolveSecret(meta.ID)
	if err != nil || jsonText(secret) != `{"password": "s3cret-pw"}` {
		t.Fatalf("secret %v %v", secret, err)
	}
	if meta.Identifier != "user@example.com" || meta.IdentifierType != "email" {
		t.Fatalf("meta %+v", meta)
	}
}

func TestVaultStoreVaultFileNeverContainsPassword(t *testing.T) {
	_, _, store, dir := vaultStore(t)
	addLogin(t, store, "https://example.com", "s3cret-pw")
	blob, err := os.ReadFile(filepath.Join(dir, "vault.json.enc"))
	if err != nil || strings.Contains(string(blob), "s3cret-pw") {
		t.Fatalf("blob carries the password or is unreadable: %v", err)
	}
}

func TestVaultStoreListingIsPasswordFree(t *testing.T) {
	_, _, store, _ := vaultStore(t)
	meta := addLogin(t, store, "https://example.com", "s3cret-pw")
	items, err := store.ListItems()
	if err != nil || len(items) != 1 {
		t.Fatalf("items %v %v", items, err)
	}
	dumped := jsonText(items[0].ToDict())
	if strings.Contains(dumped, "s3cret-pw") || strings.Contains(dumped, "password") {
		t.Fatalf("listing %s", dumped)
	}
	if items[0].Identifier != "user@example.com" || items[0].IdentifierType != "email" || items[0].ID != meta.ID ||
		items[0].Origin != "https://example.com" {
		t.Fatalf("item %+v", items[0])
	}
}

func TestVaultStoreRemoveItem(t *testing.T) {
	_, _, store, _ := vaultStore(t)
	meta := addLogin(t, store, "https://example.com", "s3cret-pw")
	if removed, err := store.RemoveItem(meta.ID); err != nil || !removed {
		t.Fatalf("first remove %v %v", removed, err)
	}
	if removed, err := store.RemoveItem(meta.ID); err != nil || removed {
		t.Fatalf("second remove %v %v", removed, err)
	}
	if items, err := store.ListItems(); err != nil || len(items) != 0 {
		t.Fatalf("items %v %v", items, err)
	}
}

func TestVaultStoreLoginRequiresOrigin(t *testing.T) {
	_, _, store, _ := vaultStore(t)
	_, err := store.AddItem("login", "x", pyjson.ObjectOf("identifier_type", "email", "identifier", "a@b.c", "password", "p"), "")
	if !isVaultError(err) {
		t.Fatalf("err %v", err)
	}
}

func TestVaultStoreAllKindsSupported(t *testing.T) {
	_, _, store, _ := vaultStore(t)
	if _, err := store.AddItem("payment", "Card", cardSecret(), "https://shop.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddItem("address", "Home", addressSecret(), ""); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListItems()
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, m := range items {
		kinds = append(kinds, m.Kind)
	}
	sort.Strings(kinds)
	if !reflect.DeepEqual(kinds, []string{"address", "payment"}) {
		t.Fatalf("kinds %v", kinds)
	}
}

func TestVaultStoreCheckoutKindsKeepOnlyCanonicalFields(t *testing.T) {
	_, _, store, _ := vaultStore(t)
	withNote := cardSecret()
	withNote.Set("note", "personal")
	meta, err := store.AddItem("payment", "Card", withNote, "")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := store.ResolveSecret(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := secret.Get("note"); kept {
		t.Fatal("a stray key was stored")
	}
	withoutCVC := cardSecret()
	withoutCVC.Delete("cvc")
	if _, err := store.AddItem("payment", "Card", withoutCVC, ""); !isVaultError(err) || !strings.Contains(err.Error(), "cvc") {
		t.Fatalf("err %v", err)
	}
}

func TestVaultStoreUnknownKindRejected(t *testing.T) {
	_, _, store, _ := vaultStore(t)
	if _, err := store.AddItem("totp", "x", pyjson.NewObject(), ""); !isVaultError(err) {
		t.Fatalf("err %v", err)
	}
}

func TestVaultStoreHasItems(t *testing.T) {
	_, _, store, _ := vaultStore(t)
	if store.HasItems() {
		t.Fatal("empty store has items")
	}
	addLogin(t, store, "https://example.com", "s3cret-pw")
	if !store.HasItems() {
		t.Fatal("store has no items after an add")
	}
}

func TestVaultStoreNormalizeOrigin(t *testing.T) {
	for in, want := range map[string]string{"https://Example.com:443/login?x=1": "https://example.com",
		"http://localhost:8931/": "http://localhost:8931", "http://site.test:80": "http://site.test"} {
		if got, err := vault.NormalizeOrigin(in); err != nil || got != want {
			t.Fatalf("%s gave %q %v", in, got, err)
		}
	}
	if _, err := vault.NormalizeOrigin("example.com"); !isVaultError(err) {
		t.Fatalf("err %v", err)
	}
}

func TestVaultStoreScrubSecretFromText(t *testing.T) {
	out := vault.ScrubSecretFromText("boom hunter22x at me@x.io", pyjson.ObjectOf("password", "hunter22x", "identifier", "me@x.io"))
	if strings.Contains(out, "hunter22x") || strings.Contains(out, "me@x.io") {
		t.Fatalf("out %s", out)
	}
}

func ctrl(mod func(*LoginControl)) LoginControl {
	form := 0
	c := LoginControl{FormIndex: &form, Type: "text"}
	if mod != nil {
		mod(&c)
	}
	return c
}

func TestClassifierAutocompleteExactMatchMapsToken(t *testing.T) {
	for _, token := range []string{"username", "email", "tel", "current-password"} {
		res := ClassifyLoginControl(ctrl(func(c *LoginControl) { c.Autocomplete = token }))
		if res == nil || res.Token != token {
			t.Fatalf("%s gave %+v", token, res)
		}
	}
}

func TestClassifierNewPasswordAutocompleteExcluded(t *testing.T) {
	if ClassifyLoginControl(ctrl(func(c *LoginControl) { c.Autocomplete, c.Type = "new-password", "password" })) != nil {
		t.Fatal("new-password classified")
	}
}

func TestClassifierOneTimeCodeExcluded(t *testing.T) {
	if ClassifyLoginControl(ctrl(func(c *LoginControl) { c.Autocomplete = "one-time-code" })) != nil {
		t.Fatal("one-time-code classified")
	}
}

func TestClassifierLabelNewPasswordExcluded(t *testing.T) {
	for _, label := range []string{"New password", "Confirm Password", "create-password", "Repeat  password"} {
		if ClassifyLoginControl(ctrl(func(c *LoginControl) { c.Type, c.Label = "password", label })) != nil {
			t.Fatal(label)
		}
	}
}

func TestClassifierPasswordTypeMapsCurrentPassword(t *testing.T) {
	if res := ClassifyLoginControl(ctrl(func(c *LoginControl) { c.Type = "password" })); res == nil || res.Token != "current-password" {
		t.Fatalf("res %+v", res)
	}
}

func TestClassifierEmailTelTypesMapTokens(t *testing.T) {
	if res := ClassifyLoginControl(ctrl(func(c *LoginControl) { c.Type = "email" })); res == nil || res.Token != "email" {
		t.Fatalf("email %+v", res)
	}
	if res := ClassifyLoginControl(ctrl(func(c *LoginControl) { c.Type = "tel" })); res == nil || res.Token != "tel" {
		t.Fatalf("tel %+v", res)
	}
}

func TestClassifierLabelHeuristics(t *testing.T) {
	if res := ClassifyLoginControl(ctrl(func(c *LoginControl) { c.Label = "E-mail address" })); res == nil || res.Token != "email" {
		t.Fatalf("email %+v", res)
	}
	if res := ClassifyLoginControl(ctrl(func(c *LoginControl) { c.Name = "mobile_number" })); res == nil || res.Token != "tel" {
		t.Fatalf("tel %+v", res)
	}
	if res := ClassifyLoginControl(ctrl(func(c *LoginControl) { c.Label = "Username or account" })); res == nil || res.Token != "username" {
		t.Fatalf("username %+v", res)
	}
}

func TestClassifierUnmatchedReturnsNone(t *testing.T) {
	if ClassifyLoginControl(ctrl(func(c *LoginControl) { c.Label = "Search the docs" })) != nil {
		t.Fatal("search box classified")
	}
}

func TestClassifierSelectPasswordFillPicksBestPassword(t *testing.T) {
	user := ClassifiedLoginControl{ctrl(func(c *LoginControl) { c.Autocomplete = "username" }), 100, "username"}
	pwHeur := ClassifiedLoginControl{ctrl(func(c *LoginControl) { c.Index, c.Type = 1, "password" }), 90, "current-password"}
	pwExact := ClassifiedLoginControl{ctrl(func(c *LoginControl) { c.Index, c.Autocomplete = 3, "current-password" }), 100, "current-password"}
	fills := SelectPasswordFill([]ClassifiedLoginControl{user, pwHeur, pwExact}, "p")
	if len(fills) != 1 || anyGet(fills[0], "index") != int64(3) || anyGet(fills[0], "token") != "current-password" {
		t.Fatalf("fills %v", fills)
	}
}

func TestClassifierSelectPasswordFillRequiresPasswordField(t *testing.T) {
	user := ClassifiedLoginControl{ctrl(func(c *LoginControl) { c.Autocomplete = "username" }), 100, "username"}
	if fills := SelectPasswordFill([]ClassifiedLoginControl{user}, "p"); len(fills) != 0 {
		t.Fatalf("fills %v", fills)
	}
}

func TestClassifierSelectPasswordFillSingleFieldOnly(t *testing.T) {
	pw1 := ClassifiedLoginControl{ctrl(func(c *LoginControl) { c.Index, c.Type = 1, "password" }), 90, "current-password"}
	pw2 := ClassifiedLoginControl{ctrl(func(c *LoginControl) { c.Index, c.Type = 2, "password" }), 90, "current-password"}
	fills := SelectPasswordFill([]ClassifiedLoginControl{pw1, pw2}, "p")
	if len(fills) != 1 || anyGet(fills[0], "index") != int64(1) {
		t.Fatalf("fills %v", fills)
	}
}

func onePasswordFill() []*pyjson.Object {
	return []*pyjson.Object{pyjson.ObjectOf("index", int64(0), "token", "current-password", "value", "x")}
}

func TestClassifierBuildFillJsLeavesNoDomMarkerAndBindsTargetToInspection(t *testing.T) {
	js := BuildFillJS(onePasswordFill(), "https://example.com", "")
	for _, absent := range []string{"vaultSecret", "data-vault-secret", "elements[f.index]"} {
		if strings.Contains(js, absent) {
			t.Fatalf("%s in the fill script", absent)
		}
	}
	if !strings.Contains(js, "[data-icebath-vault-slot=") || !strings.Contains(js, "nonce + ':' + f.index") {
		t.Fatal("the fill does not resolve targets by the inspection stamp")
	}
	if !strings.Contains(js, `f.token === "current-password" && el.type !== "password"`) {
		t.Fatal("a password fill may land in a text box")
	}
	if strings.Index(js, `removeAttribute("data-icebath-vault-slot")`) <= strings.Index(js, "setter.set.call") {
		t.Fatal("stamps are not stripped after the write")
	}
}

func TestClassifierBuildFillJsAssertsOriginBeforeAnyWrite(t *testing.T) {
	js := BuildFillJS(onePasswordFill(), "https://example.com", "")
	for _, present := range []string{`"https://example.com"`, "window.location.origin", "origin_changed"} {
		if !strings.Contains(js, present) {
			t.Fatalf("%s missing", present)
		}
	}
	if strings.Index(js, "origin_changed") >= strings.Index(js, "querySelectorAll") {
		t.Fatal("the origin assert runs after an element lookup")
	}
}

func pinPageOrigin(t *testing.T, origin string) {
	t.Helper()
	pin(t, &vaultCurrentPageOrigin, func(context.Context, *tools.Env, string) string { return origin })
}

const loginControlsJSON = `[{"autocomplete": "email", "formIndex": 0, "index": 0, "label": "", "name": "email", "type": "email"}, ` +
	`{"autocomplete": "current-password", "formIndex": 0, "index": 1, "label": "", "name": "pw", "type": "password"}]`

const passwordControlJSON = `[{"autocomplete": "current-password", "formIndex": 0, "index": 0, "label": "", "name": "pw", "type": "password"}]`

// Page double answering the location read with href and every inspection with controls
func pinPage(t *testing.T, href, controls string) {
	t.Helper()
	pin(t, &vaultEvalJS, func(_ context.Context, _ *tools.Env, _ string, expression string) *pyjson.Object {
		if strings.Contains(expression, "location.href") {
			return pyjson.ObjectOf("success", true, "result", href)
		}
		return pyjson.ObjectOf("success", true, "result", controls)
	})
}

func pinSecretEval(t *testing.T, result string) *[]string {
	t.Helper()
	scripts := &[]string{}
	pin(t, &vaultEvalJSSecret, func(_ context.Context, _ *tools.Env, _ string, expression string) *pyjson.Object {
		*scripts = append(*scripts, expression)
		return pyjson.ObjectOf("success", true, "result", result)
	})
	return scripts
}

func TestBrowserVaultToolsCheckFnFollowsTheBrowserNotTheItemCount(t *testing.T) {
	_, env, _, _ := vaultStore(t)
	pin(t, &vaultCDPOverrideRaw, func(*tools.Env) string { return "" })
	pin(t, &browserExecAvailable, func(*tools.Env) bool { return true })
	if !checkVaultAvailable(env) {
		t.Fatal("an empty vault hides the tool although a browser exists")
	}
	browserExecAvailable = func(*tools.Env) bool { return false }
	if checkVaultAvailable(env) {
		t.Fatal("the tool shows without any browser")
	}
	vaultCDPOverrideRaw = func(*tools.Env) string { return "ws://127.0.0.1:9222/devtools/browser/x" }
	if !checkVaultAvailable(env) {
		t.Fatal("a configured CDP endpoint is a browser too")
	}
}

func TestBrowserVaultToolsListReturnsIdentifierNeverPassword(t *testing.T) {
	ctx, env, store, _ := vaultStore(t)
	addLogin(t, store, "https://example.com", "s3cret-pw")
	text, err := browserVaultList(ctx, env)
	out := loadResult(t, text, err)
	first := anyGet(out, "items").([]any)[0]
	if anyGet(out, "success") != true || !strings.HasPrefix(anyGet(first, "handle").(string), "vault_") ||
		anyGet(first, "identifier") != "user@example.com" || anyGet(first, "identifier_type") != "email" {
		t.Fatalf("list %s", text)
	}
	if strings.Contains(text, "s3cret-pw") {
		t.Fatal("listing carries the password")
	}
}

func TestBrowserVaultToolsFillRefusedOnOriginMismatch(t *testing.T) {
	ctx, env, store, _ := vaultStore(t)
	meta := addLogin(t, store, "https://example.com", "s3cret-pw")
	pinPageOrigin(t, "https://evil.com")
	text, err := browserVaultFill(ctx, env, meta.ID, "")
	out := loadResult(t, text, err)
	if anyGet(out, "success") != false || anyGet(out, "error_type") != "origin_mismatch" || strings.Contains(text, "s3cret-pw") {
		t.Fatalf("fill %s", text)
	}
}

func managerMeta() *vault.VaultItemMeta {
	return &vault.VaultItemMeta{ID: "op:multi", Kind: "login", Label: "Amazon", Origin: "https://amazon.co.uk",
		CreatedAt: "2026-01-01T00:00:00Z", IdentifierType: "username", Identifier: "jane@example.com",
		AllowedOrigins: []string{"https://amazon.co.uk", "https://www.amazon.co.uk", "https://eu.account.amazon.com"}}
}

func managerBackend(t *testing.T, ownContext bool) *fakeLoginBackend {
	t.Helper()
	meta := managerMeta()
	backend := newFakeBackend("onepassword", "1Password")
	backend.getMeta = func(handle string) (*vault.VaultItemMeta, error) {
		if handle == meta.ID {
			return meta, nil
		}
		return nil, nil
	}
	backend.resolvePassword = func(string) (string, error) { return "s3cret-pw", nil }
	if ownContext {
		backend.fillContext = func() (func() error, error) { return func() error { return nil }, nil }
	}
	pin(t, &loginBackendForHandle, func(context.Context, string) (LoginBackend, error) { return backend, nil })
	return backend
}

func TestBrowserVaultToolsFillAllowedOnEverySavedOriginOfAMultiWebsiteItem(t *testing.T) {
	ctx, env := vaultProfile(t)
	t.Cleanup(func() { tools.ClearVaultRedactionValues(ctx) })
	managerBackend(t, true)
	pinPageOrigin(t, "https://www.amazon.co.uk")
	pin(t, &vaultEvalJS, func(context.Context, *tools.Env, string, string) *pyjson.Object {
		return pyjson.ObjectOf("success", true, "result", loginControlsJSON)
	})
	scripts := pinSecretEval(t, `{"filled": 1}`)
	raw, err := browserVaultFill(ctx, env, "op:multi", "")
	out := loadResult(t, raw, err)
	if anyGet(out, "success") != true || anyGet(out, "origin") != "https://www.amazon.co.uk" || anyGet(out, "filled_fields") != int64(1) {
		t.Fatalf("fill %s", raw)
	}
	if !strings.Contains((*scripts)[0], "https://www.amazon.co.uk") || strings.Contains(raw, "s3cret-pw") {
		t.Fatal("the in-page check is not bound to the matched origin or the result carries the password")
	}
}

func TestBrowserVaultToolsFillStillRefusedOnOriginNotSavedOnTheItem(t *testing.T) {
	ctx, env := vaultProfile(t)
	managerBackend(t, false)
	pinPageOrigin(t, "https://payments.amazon.co.uk")
	text, err := browserVaultFill(ctx, env, "op:multi", "")
	out := loadResult(t, text, err)
	if anyGet(out, "success") != false || anyGet(out, "error_type") != "origin_mismatch" || strings.Contains(text, "s3cret-pw") {
		t.Fatalf("fill %s", text)
	}
}

func TestBrowserVaultToolsFillUnknownHandle(t *testing.T) {
	ctx, env, _, _ := vaultStore(t)
	text, err := browserVaultFill(ctx, env, "vault_nope", "")
	if out := loadResult(t, text, err); anyGet(out, "success") != false {
		t.Fatalf("fill %s", text)
	}
}

func TestBrowserVaultToolsFillSuccessReturnsCountsOnly(t *testing.T) {
	ctx, env, store, _ := vaultStore(t)
	meta := addLogin(t, store, "https://example.com", "s3cret-pw")
	pinPage(t, "https://example.com/login", loginControlsJSON)
	scripts := pinSecretEval(t, `{"filled": 1}`)
	raw, err := browserVaultFill(ctx, env, meta.ID, "")
	out := loadResult(t, raw, err)
	out.Pop("next")
	if got := jsonText(out); got != `{"success": true, "filled_fields": 1, "backend": "local", "kind": "login", "origin": "https://example.com"}` {
		t.Fatalf("fill %s", got)
	}
	if strings.Contains(raw, "s3cret-pw") || len(*scripts) != 1 {
		t.Fatalf("raw %s scripts %d", raw, len(*scripts))
	}
	script := (*scripts)[0]
	if !strings.Contains(script, "s3cret-pw") || strings.Contains(script, `"index": 0`) || strings.Contains(script, "user@example.com") {
		t.Fatal("the secret script targets more than the password field")
	}
}

func TestBrowserVaultToolsFillToctouNavigationWritesNothing(t *testing.T) {
	ctx, env, store, _ := vaultStore(t)
	meta := addLogin(t, store, "https://example.com", "s3cret-pw")
	pinPage(t, "https://example.com/login", passwordControlJSON)
	pin(t, &vaultEvalJSSecret, func(_ context.Context, _ *tools.Env, _ string, expression string) *pyjson.Object {
		if !strings.Contains(expression, "window.location.origin") || !strings.Contains(expression, `"https://example.com"`) {
			t.Error("the evaluated script lacks the origin assert")
		}
		return pyjson.ObjectOf("success", true, "result", `{"refused": "origin_changed", "found": "https://evil.com"}`)
	})
	raw, err := browserVaultFill(ctx, env, meta.ID, "")
	out := loadResult(t, raw, err)
	if anyGet(out, "success") != false || anyGet(out, "error_type") != "origin_changed" || strings.Contains(raw, "s3cret-pw") {
		t.Fatalf("fill %s", raw)
	}
	if filled, ok := out.Get("filled_fields"); ok && filled != int64(0) {
		t.Fatalf("filled %v", filled)
	}
}

func TestBrowserVaultToolsSecretEvalFailsClosedWithoutSupervisor(t *testing.T) {
	ctx, env, store, _ := vaultStore(t)
	meta := addLogin(t, store, "https://example.com", "s3cret-pw")
	pinPage(t, "https://example.com/login", passwordControlJSON)
	pin(t, &vaultSessionForTask, func(string) map[string]any { return map[string]any{} })
	registryCalls := []string{}
	pin(t, &vaultRegisteredSup, func(taskID string) vaultSupervisor {
		registryCalls = append(registryCalls, "get "+taskID)
		return nil
	})
	started := 0
	pin(t, &vaultStartSupervisor, func(_ *tools.Env, taskID, cdpURL, _ string, _ float64) (vaultSupervisor, error) {
		started++
		registryCalls = append(registryCalls, "get_or_start "+taskID+" "+cdpURL)
		return nil, nil
	})
	raw, err := browserVaultFill(ctx, env, meta.ID, "")
	out := loadResult(t, raw, err)
	if anyGet(out, "success") != false || anyGet(out, "error_type") != "supervisor_required" {
		t.Fatalf("fill %s", raw)
	}
	if started != 0 {
		t.Fatal("a supervisor was started without a CDP endpoint")
	}
	if strings.Contains(strings.Join(registryCalls, "\n"), "s3cret-pw") || strings.Contains(raw, "s3cret-pw") {
		t.Fatal("the password reached the registry or the result")
	}
}

func TestBrowserVaultToolsVaultCanaryRedactedFromBrowserCdpResults(t *testing.T) {
	ctx, env, store, _ := vaultStore(t)
	canary := "plain sentence nobody would flag 7"
	meta := addLogin(t, store, "https://example.com", canary)
	pinPage(t, "https://example.com/login", passwordControlJSON)
	pinSecretEval(t, `{"filled": 1}`)
	text, err := browserVaultFill(ctx, env, meta.ID, "")
	if out := loadResult(t, text, err); anyGet(out, "success") != true {
		t.Fatalf("fill %s", text)
	}
	cdpResult := pyjson.ObjectOf("result", pyjson.ObjectOf("type", "string", "value", canary), "description", "input value is "+canary)
	scrubbed, err := pyjson.Dumps(redactCDPOutput(env, cdpResult, nil, nil, 0), pyjson.EnsureASCII(false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(scrubbed, canary) || !strings.Contains(scrubbed, "«redacted-vault-secret»") {
		t.Fatalf("scrubbed %s", scrubbed)
	}
	if strings.Contains(tools.RedactSensitiveText("page text: "+canary), canary) {
		t.Fatal("the generic browser result scrub left the canary")
	}
}

func TestBrowserVaultToolsPaymentFillRequiresConfirmationThenFillsCardFields(t *testing.T) {
	ctx, env, store, _ := vaultStore(t)
	meta, err := store.AddItem("payment", "Visa", cardSecret(), "https://shop.test")
	if err != nil {
		t.Fatal(err)
	}
	pinPage(t, "https://shop.test/checkout", `[{"autocomplete": "cc-number", "index": 0, "type": "text"}, `+
		`{"label": "Expiry (MM/YY)", "index": 1, "type": "text"}, {"label": "CVC", "index": 2, "type": "text"}, `+
		`{"autocomplete": "email", "index": 3, "type": "email"}]`)
	scripts := pinSecretEval(t, `{"filled": 3}`)
	answer := "decline"
	pin(t, &vaultElicitConsent, func(context.Context, *tools.Env, string, string, tools.ElicitationOptions) string { return answer })
	text, err := browserVaultFill(ctx, env, meta.ID, "")
	declined := loadResult(t, text, err)
	if anyGet(declined, "success") != false || anyGet(declined, "error_type") != "payment_declined" || len(*scripts) != 0 {
		t.Fatalf("declined %s", text)
	}
	answer = "accept"
	raw, err := browserVaultFill(ctx, env, meta.ID, "")
	out := loadResult(t, raw, err)
	if anyGet(out, "success") != true || jsonText(anyGet(out, "fields")) != `["cc-csc", "cc-exp", "cc-number"]` {
		t.Fatalf("fill %s", raw)
	}
	if strings.Contains(raw, "4111111111111111") || strings.Contains(raw, "123") {
		t.Fatalf("the result carries a card value: %s", raw)
	}
	if len(*scripts) != 1 || !strings.Contains((*scripts)[0], "4111111111111111") || !strings.Contains((*scripts)[0], "07/29") ||
		strings.Contains((*scripts)[0], `"index": 3`) {
		t.Fatalf("scripts %v", *scripts)
	}
	if strings.Contains(tools.RedactSensitiveText("dom says 4111111111111111"), "4111111111111111") {
		t.Fatal("the card number is not registered for redaction")
	}
}

func TestVaultHardeningReadBlockVaultDirAndContents(t *testing.T) {
	h := filepath.Join(t.TempDir(), "hermes_home")
	vaultDir := filepath.Join(h, "vault")
	if err := os.MkdirAll(vaultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(vaultDir, "vault.key"), []byte("k"), 0o644)
	os.WriteFile(filepath.Join(vaultDir, "vault.json.enc"), []byte("blob"), 0o644)
	env := &tools.Env{Home: h}
	for _, target := range []string{vaultDir, filepath.Join(vaultDir, "vault.key"), filepath.Join(vaultDir, "vault.json.enc")} {
		if tools.ReadBlockError(env, target) == "" {
			t.Fatalf("expected read deny for %s", target)
		}
	}
}

func TestVaultHardeningReadBlockLeavesSiblingDirsAlone(t *testing.T) {
	h := filepath.Join(t.TempDir(), "hermes_home")
	other := filepath.Join(h, "vaults-notes")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(other, "notes.txt")
	os.WriteFile(notes, []byte("hi"), 0o644)
	if msg := tools.ReadBlockError(&tools.Env{Home: h}, notes); msg != "" {
		t.Fatalf("sibling dir blocked: %s", msg)
	}
}

func TestEveryRegisteredToolSchemaDeclaresOpenaiStyleParameters(t *testing.T) {
	missing := []string{}
	for _, name := range tools.Default.Names() {
		def := tools.Default.Get(name)
		schema := def.Schema
		if schema == nil && def.SchemaBuilder != nil {
			schema = def.SchemaBuilder()
		}
		_, hasParameters := schema["parameters"]
		_, hasInputSchema := schema["input_schema"]
		if !hasParameters || hasInputSchema {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		t.Fatalf("schemas without parameters: %v", missing)
	}
}

func TestSaveLoginPromptSavesToPageOriginAndNeverEchoesThePassword(t *testing.T) {
	ctx, env, store, _ := vaultStore(t)
	seen := map[string]string{}
	SetSaveLoginPromptCallback(ctx, func(origin, site string) map[string]string {
		seen["origin"], seen["site"] = origin, site
		return map[string]string{"identifier": "tek@acme.test", "password": "hunter2-very-secret"}
	})
	defer SetSaveLoginPromptCallback(ctx, nil)
	pinPageOrigin(t, "https://acme.test")
	pin(t, &vaultFillFn, func(context.Context, *tools.Env, string, string) (string, error) {
		return `{"success": true, "filled_fields": 1}`, nil
	})
	pin(t, &vaultCanPromptHere, func(context.Context, *tools.Env) bool { return true })
	text, err := browserVaultSaveLogin(ctx, env, "", "t1")
	out := loadResult(t, text, err)
	if anyGet(out, "success") != true || anyGet(out, "identifier") != "tek@acme.test" || strings.Contains(text, "hunter2") {
		t.Fatalf("save %s", text)
	}
	if !reflect.DeepEqual(seen, map[string]string{"origin": "https://acme.test", "site": "acme.test"}) {
		t.Fatalf("seen %v", seen)
	}
	items, err := store.ListItems()
	if err != nil || len(items) != 1 || items[0].Origin != "https://acme.test" || items[0].Identifier != "tek@acme.test" {
		t.Fatalf("items %+v %v", items, err)
	}
}

func TestSaveLoginPromptDeclinedOrHeadlessStoresNothing(t *testing.T) {
	ctx, env, store, _ := vaultStore(t)
	pinPageOrigin(t, "https://acme.test")
	SetSaveLoginPromptCallback(ctx, func(string, string) map[string]string { return nil })
	defer SetSaveLoginPromptCallback(ctx, nil)
	canPrompt := true
	pin(t, &vaultCanPromptHere, func(context.Context, *tools.Env) bool { return canPrompt })
	text, err := browserVaultSaveLogin(ctx, env, "", "")
	declined := loadResult(t, text, err)
	canPrompt = false
	text, err = browserVaultSaveLogin(ctx, env, "", "")
	headless := loadResult(t, text, err)
	if anyGet(declined, "error_type") != "save_declined" || anyGet(headless, "error_type") != "prompt_unavailable" {
		t.Fatalf("declined %v headless %v", declined, headless)
	}
	if items, err := store.ListItems(); err != nil || len(items) != 0 {
		t.Fatalf("items %v %v", items, err)
	}
}

func enabledBackendNames(t *testing.T, ctx context.Context) []string {
	t.Helper()
	backends, err := EnabledLoginBackends(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, b := range backends {
		names = append(names, b.Name())
	}
	return names
}

func TestManagerAutoDetectionInstalledManagerIsASourceWithoutConfigAndConfigCanOptOut(t *testing.T) {
	ctx, _ := vaultProfile(t)
	installed := true
	pin(t, &isLoginBackendInstalledFn, func(context.Context, string) (bool, error) { return installed, nil })
	cfg := map[string]any{}
	pin(t, &vaultCfgFn, func(context.Context) (map[string]any, error) { return cfg, nil })
	names := enabledBackendNames(t, ctx)
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"bitwarden", "local", "onepassword"}) {
		t.Fatalf("names %v", names)
	}
	cfg = map[string]any{"bitwarden": map[string]any{"enabled": false}}
	names = enabledBackendNames(t, ctx)
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"local", "onepassword"}) {
		t.Fatalf("names %v", names)
	}
	installed, cfg = false, map[string]any{}
	if names := enabledBackendNames(t, ctx); !reflect.DeepEqual(names, []string{"local"}) {
		t.Fatalf("names %v", names)
	}
}

func TestEveryVaultToolIsInTheBrowserToolset(t *testing.T) {
	listed := map[string]bool{}
	for _, name := range tools.Toolsets["browser"].Tools {
		listed[name] = true
	}
	if def := tools.Default.Get("browser_vault"); def == nil || def.Toolset != "browser" || !listed["browser_vault"] {
		t.Fatalf("browser_vault is not offered by the browser toolset: %+v", def)
	}
	for _, name := range tools.Default.Names() {
		if strings.HasPrefix(name, "browser_vault_") && !listed[name] {
			t.Fatalf("%s is registered but missing from the browser toolset", name)
		}
	}
}

func TestTwoFactorTotpMatchesRfc6238VectorAndSeedNormalisation(t *testing.T) {
	seed := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	totp := func(seed string, digits, period int, at float64) string {
		code, err := vault.TOTPAt(seed, digits, period, at)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}
	if got := totp(seed, 8, 30, 59); got != "94287082" {
		t.Fatalf("8 digit vector %s", got)
	}
	if got := totp(seed, 6, 30, 1111111109); got != "081804" {
		t.Fatalf("6 digit vector %s", got)
	}
	if got, err := vault.NormalizeOTPSecret("otpauth://totp/GitHub:tek?secret=jbsw y3dp ehpk3pxp&issuer=GitHub"); err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("normalized %q %v", got, err)
	}
	if _, err := vault.NormalizeOTPSecret("not base32!"); !isVaultError(err) {
		t.Fatalf("err %v", err)
	}
	stored, err := vault.NormalizeOTPSecret("otpauth://totp/x?secret=" + strings.Repeat("GEZDGNBVGY3TQOJQ", 2) + "&digits=8&period=30&algorithm=SHA256")
	if err != nil || !strings.HasSuffix(stored, "|8|30|SHA256") {
		t.Fatalf("stored %q %v", stored, err)
	}
	if got := totp("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQGEZA|8|30|SHA256", 6, 30, 59); got != "46119246" {
		t.Fatalf("sha256 vector %s", got)
	}
	if totp(seed+"|6|60|SHA1", 6, 30, 119) != totp(seed, 6, 60, 119) {
		t.Fatal("stored period is not honoured")
	}
	if _, err := vault.NormalizeOTPSecret("otpauth://hotp/x?secret=JBSWY3DPEHPK3PXP&counter=1"); !isVaultError(err) {
		t.Fatalf("err %v", err)
	}
}

// Page double answering inspections with controls and every other read with href
func pinCodePage(t *testing.T, href, controls string) {
	t.Helper()
	pin(t, &vaultFocusBoundOrigin, func(context.Context, *tools.Env, string, string, string) string { return "" })
	pin(t, &vaultEvalJS, func(_ context.Context, _ *tools.Env, _ string, expression string) *pyjson.Object {
		if strings.Contains(expression, "querySelectorAll") {
			return pyjson.ObjectOf("success", true, "result", controls)
		}
		return pyjson.ObjectOf("success", true, "result", href)
	})
}

func TestTwoFactorSavedAuthenticatorKeyMintsCodesWithoutAsking(t *testing.T) {
	ctx, env, store, _ := vaultStore(t)
	meta, err := store.AddItem("login", "gh", pyjson.ObjectOf("identifier_type", "username", "identifier", "tek", "password", "pw",
		"otp_secret", "JBSWY3DPEHPK3PXP"), "https://github.com")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetMeta(meta.ID); err != nil || got == nil || !got.HasOTP {
		t.Fatalf("meta %+v %v", got, err)
	}
	asked := []string{}
	SetCodePromptCallback(ctx, func(site, _ string) string {
		asked = append(asked, site)
		return "000000"
	})
	defer SetCodePromptCallback(ctx, nil)
	pinCodePage(t, "https://github.com/sessions/two-factor",
		`[{"index": 0, "type": "text", "name": "otp", "label": "Authentication code", "autocomplete": "one-time-code"}]`)
	scripts := pinSecretEval(t, `{"filled": 1}`)
	raw, err := browserVaultEnterCode(ctx, env, meta.ID, "t")
	out := loadResult(t, raw, err)
	if anyGet(out, "success") != true || anyGet(out, "source") != "local" || len(asked) != 0 {
		t.Fatalf("enter_code %s asked %v", raw, asked)
	}
	code := regexp.MustCompile(`"value": "(\d{6})"`).FindStringSubmatch((*scripts)[len(*scripts)-1])
	if code == nil || strings.Contains(raw, code[1]) {
		t.Fatalf("the code is missing from the page script or present in the result: %v", code)
	}
}

func TestTwoFactorWithoutAKeyTheUserIsAskedAndSplitBoxesGetOneDigitEach(t *testing.T) {
	ctx, env := vaultProfile(t)
	t.Cleanup(func() { tools.ClearVaultRedactionValues(ctx) })
	answer := "246 810"
	SetCodePromptCallback(ctx, func(string, string) string { return answer })
	defer SetCodePromptCallback(ctx, nil)
	boxes := []string{}
	for i := range 6 {
		boxes = append(boxes, jsonText(pyjson.ObjectOf("index", int64(i), "type", "tel", "name", "digit"+string(rune('0'+i)), "label", "",
			"autocomplete", "one-time-code", "formIndex", int64(0), "maxLength", int64(1))))
	}
	pin(t, &vaultCanPromptHere, func(context.Context, *tools.Env) bool { return true })
	pinCodePage(t, "https://acme.test/2fa", "["+strings.Join(boxes, ", ")+"]")
	scripts := pinSecretEval(t, `{"filled": 6}`)
	text, err := browserVaultEnterCode(ctx, env, "", "t")
	out := loadResult(t, text, err)
	answer = ""
	text, err = browserVaultEnterCode(ctx, env, "", "t")
	declined := loadResult(t, text, err)
	if anyGet(out, "success") != true || anyGet(out, "source") != "user" || anyGet(out, "filled_fields") != int64(6) {
		t.Fatalf("enter_code %v", out)
	}
	digits := []string{}
	for _, m := range regexp.MustCompile(`"value": "(\d)"`).FindAllStringSubmatch((*scripts)[0], -1) {
		digits = append(digits, m[1])
	}
	if strings.Join(digits, "") != "246810" || len(digits) != 6 {
		t.Fatalf("digits %v", digits)
	}
	if anyGet(declined, "error_type") != "code_declined" {
		t.Fatalf("declined %v", declined)
	}
}

func TestTwoFactorSeveralCodeLikeInputsThatAreNotADigitWidgetGetOneField(t *testing.T) {
	one := 1
	ctl := func(i, form int, maxLength *int, score int) ClassifiedLoginControl {
		return ClassifiedLoginControl{LoginControl{FormIndex: &form, Index: i, Name: "code", Type: "text", MaxLength: maxLength}, score, "one-time-code"}
	}
	scattered := []ClassifiedLoginControl{ctl(0, 0, nil, 70), ctl(3, 0, nil, 70), ctl(7, 0, nil, 70), ctl(9, 1, nil, 70), ctl(12, 0, nil, 100)}
	if got := jsonText(BuildOTPFills(scattered, "246810")); got != `[{"index": 12, "token": "one-time-code", "value": "246810"}]` {
		t.Fatalf("scattered %s", got)
	}
	build := func(index func(i int) int, form func(i int) int, n int) []ClassifiedLoginControl {
		out := []ClassifiedLoginControl{}
		for i := range n {
			out = append(out, ctl(index(i), form(i), &one, 70))
		}
		return out
	}
	same := func(int) int { return 0 }
	if n := len(BuildOTPFills(build(func(i int) int { return i }, func(i int) int { return i % 2 }, 6), "246810")); n != 1 {
		t.Fatalf("different forms split into %d", n)
	}
	if n := len(BuildOTPFills(build(func(i int) int { return i * 2 }, same, 6), "246810")); n != 1 {
		t.Fatalf("non adjacent boxes split into %d", n)
	}
	if n := len(BuildOTPFills(build(func(i int) int { return i }, same, 5), "246810")); n != 1 {
		t.Fatalf("five boxes split into %d", n)
	}
	values := []string{}
	for _, f := range BuildOTPFills(build(func(i int) int { return i + 4 }, same, 6), "246810") {
		values = append(values, anyGet(f, "value").(string))
	}
	if strings.Join(values, ",") != "2,4,6,8,1,0" {
		t.Fatalf("widget values %v", values)
	}
}

func TestTwoFactorNoCodeFieldPointsAtPasskeyOrDeviceApproval(t *testing.T) {
	ctx, env := vaultProfile(t)
	pinCodePage(t, "https://acme.test/approve", `[{"index": 0, "type": "text", "name": "q", "label": "Search", "autocomplete": ""}]`)
	text, err := browserVaultEnterCode(ctx, env, "", "t")
	if out := loadResult(t, text, err); anyGet(out, "error_type") != "no_code_field" {
		t.Fatalf("enter_code %s", text)
	}
}
