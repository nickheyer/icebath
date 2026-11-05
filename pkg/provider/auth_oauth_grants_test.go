package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// ---- test_credential_pool_profile_oauth_fork.py ----

// Root home with an expired refreshable Anthropic row and a single use token endpoint double
type grantFleet struct {
	t      *testing.T
	root   string
	mu     sync.Mutex
	valid  map[string]bool
	spent  map[string]bool
	n      int
	log    []string
	client *http.Client
}

func newGrantFleet(t *testing.T) *grantFleet {
	t.Helper()
	root := newTestHome(t)
	f := &grantFleet{t: t, root: root, valid: map[string]bool{"sk-ant-ort-RT0": true}, spent: map[string]bool{}}
	writeJSONPath(t, filepath.Join(root, "auth.json"), map[string]any{
		"version": 1, "providers": map[string]any{},
		"credential_pool": map[string]any{
			"anthropic": []any{map[string]any{
				"id": "abc123", "label": "team-grant", "auth_type": "oauth", "priority": 0, "source": "manual:hermes_pkce",
				"access_token": "sk-ant-oat01-AT0", "refresh_token": "sk-ant-ort-RT0",
				"expires_at_ms": msFromNow(-3600), "base_url": "https://api.anthropic.com",
			}},
			"openai": []any{map[string]any{
				"id": "key001", "label": "static", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-static-key",
			}},
		},
	})
	var serverEnv PoolEnv
	anthropicTokenServer(t, &serverEnv, func(rt string) (map[string]any, int) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.spent[rt] || !f.valid[rt] {
			f.log = append(f.log, "REUSE")
			return map[string]any{"error": "invalid_grant", "error_description": "refresh_token_reused"}, 400
		}
		f.n++
		f.spent[rt] = true
		delete(f.valid, rt)
		newRT := fmt.Sprintf("sk-ant-ort-RT%d", f.n)
		f.valid[newRT] = true
		f.log = append(f.log, "ROTATE")
		return map[string]any{"access_token": fmt.Sprintf("sk-ant-oat01-AT%d", f.n), "refresh_token": newRT, "expires_in": 28800, "token_type": "Bearer"}, 200
	})
	f.client = serverEnv.HTTP
	resetOAuthHealState()
	t.Cleanup(resetOAuthHealState)
	return f
}

func resetOAuthHealState() {
	oauthHealMu.Lock()
	oauthHealCleanMarks = map[string]string{}
	oauthHealNotices = nil
	oauthHealMu.Unlock()
}

func msFromNow(seconds float64) int64 {
	return time.Now().Add(time.Duration(seconds * float64(time.Second))).UnixMilli()
}

func writeJSONPath(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSONPath(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func readText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Switches the process to the home and returns its pool env wired to the token endpoint double
func (f *grantFleet) use(h string) PoolEnv {
	f.t.Setenv(home.EnvHome, h)
	home.ResetKeyCache()
	config.ResetCache()
	oauthHealMu.Lock()
	oauthHealCleanMarks = map[string]string{}
	oauthHealMu.Unlock()
	env := testEnv(f.t, h)
	env.HTTP = f.client
	return env
}

func (f *grantFleet) profile(name string) string {
	f.use(f.root)
	dir := filepath.Join(f.root, "profiles", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

// Anthropic pool rows of the home's auth.json, nil when the file or the slice is absent
func (f *grantFleet) rows(h string) []any {
	path := filepath.Join(h, "auth.json")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	pool, _ := readJSONPath(f.t, path)["credential_pool"].(map[string]any)
	rows, _ := pool["anthropic"].([]any)
	return rows
}

func (f *grantFleet) serverLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.log...)
}

func (f *grantFleet) store(h string) map[string]any {
	return readJSONPath(f.t, filepath.Join(h, "auth.json"))
}

// Profile holding a verbatim copy of root's auth.json, optionally already rotated to the live pair
func (f *grantFleet) fork(name string, rotatedTo int) string {
	dir := f.profile(name)
	store := f.store(f.root)
	if rotatedTo > 0 {
		row := store["credential_pool"].(map[string]any)["anthropic"].([]any)[0].(map[string]any)
		row["access_token"] = fmt.Sprintf("sk-ant-oat01-AT%d", rotatedTo)
		row["refresh_token"] = fmt.Sprintf("sk-ant-ort-RT%d", rotatedTo)
		row["expires_at_ms"] = msFromNow(-60)
		f.mu.Lock()
		f.spent["sk-ant-ort-RT0"] = true
		delete(f.valid, "sk-ant-ort-RT0")
		f.valid[fmt.Sprintf("sk-ant-ort-RT%d", rotatedTo)] = true
		f.n = rotatedTo
		f.mu.Unlock()
	}
	writeJSONPath(f.t, filepath.Join(dir, "auth.json"), store)
	return dir
}

func rowField(rows []any, i int, key string) any {
	return rows[i].(map[string]any)[key]
}

func mustSelectToken(t *testing.T, env PoolEnv, provider, want string) PooledCredential {
	t.Helper()
	sel, ok := LoadPool(env, provider).Select("")
	if !ok || sel.AccessToken != want {
		t.Fatalf("selected %+v ok=%v, want access token %s", sel, ok, want)
	}
	return sel
}

func TestOAuthGrantsStripDropsDeviceCodeBlocksAndReports(t *testing.T) {
	newTestHome(t)
	pdir := filepath.Join(t.TempDir(), "p")
	os.MkdirAll(pdir, 0o700)
	writeJSONPath(t, filepath.Join(pdir, "auth.json"), map[string]any{
		"version":   1,
		"providers": map[string]any{"openai-codex": map[string]any{"access_token": "a", "refresh_token": "r"}, "nous": map[string]any{"agent_key": "k"}},
		"credential_pool": map[string]any{
			"xai-oauth": []any{map[string]any{"id": "x", "auth_type": "oauth", "access_token": "t", "refresh_token": "r"}},
			"anthropic": []any{
				map[string]any{"id": "legacy", "access_token": "sk-ant-oat01-legacy"},
				map[string]any{"id": "key", "auth_type": "api_key", "access_token": "sk-ant-api03-x"},
			},
		},
	})
	summary := StripClonedSingleUseOAuthGrants(pdir)
	store := readJSONPath(t, filepath.Join(pdir, "auth.json"))
	pool := sortedCopy(summary.Pool)
	if !reflect.DeepEqual(pool, []string{"anthropic", "xai-oauth"}) || !reflect.DeepEqual(summary.Providers, []string{"openai-codex"}) {
		t.Fatalf("summary %+v", summary)
	}
	cp := store["credential_pool"].(map[string]any)
	if _, ok := cp["xai-oauth"]; ok {
		t.Fatal("xai-oauth grant survived")
	}
	if ids := idsOf(cp["anthropic"].([]any)); !reflect.DeepEqual(ids, []string{"key"}) {
		t.Fatalf("anthropic rows %v", ids)
	}
	providers := store["providers"].(map[string]any)
	if _, ok := providers["openai-codex"]; ok {
		t.Fatal("codex block survived")
	}
	if _, ok := providers["nous"]; !ok {
		t.Fatal("agent key only nous block was stripped")
	}
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func idsOf(rows []any) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, anyString(r.(map[string]any)["id"]))
	}
	return out
}

func TestOAuthGrantsStripDropsClonedNousRefreshGrant(t *testing.T) {
	f := newGrantFleet(t)
	grant := func(extra map[string]any) map[string]any {
		out := map[string]any{"access_token": "AT1", "refresh_token": "RT1", "agent_key": "AK"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	rootStore := f.store(f.root)
	rootStore["providers"].(map[string]any)["nous"] = grant(nil)
	writeJSONPath(t, filepath.Join(f.root, "auth.json"), rootStore)
	pdir := f.profile("nousstrip")
	akRow := map[string]any{"id": "ak", "source": "device_code", "auth_type": "oauth", "agent_key": "PAK"}
	writeJSONPath(t, filepath.Join(pdir, "auth.json"), map[string]any{
		"version": 1, "providers": map[string]any{"nous": grant(nil)},
		"credential_pool": map[string]any{"nous": []any{grant(map[string]any{"id": "n", "source": "device_code", "auth_type": "oauth"}), akRow}},
	})
	summary := StripClonedSingleUseOAuthGrants(pdir)
	store := f.store(pdir)
	if !reflect.DeepEqual(summary.Pool, []string{"nous"}) || !reflect.DeepEqual(summary.Providers, []string{"nous"}) {
		t.Fatalf("summary %+v", summary)
	}
	if !reflect.DeepEqual(store["credential_pool"].(map[string]any)["nous"], []any{akRow}) {
		t.Fatalf("nous rows %v", store["credential_pool"])
	}
	if _, ok := store["providers"].(map[string]any)["nous"]; ok {
		t.Fatal("nous providers block survived")
	}
	LoadPool(f.use(pdir), "nous")
	for _, r := range f.store(pdir)["credential_pool"].(map[string]any)["nous"].([]any) {
		if r.(map[string]any)["refresh_token"] == "RT1" {
			t.Fatalf("root single use refresh token re-seeded into the profile %v", r)
		}
	}

	writeJSONPath(t, filepath.Join(f.root, "auth.json"), map[string]any{"version": 1, "providers": map[string]any{"nous": grant(nil)}})
	kid := f.profile("nousfork")
	writeJSONPath(t, filepath.Join(kid, "auth.json"), map[string]any{"version": 1, "providers": map[string]any{"nous": grant(nil)}})
	healed := healForkedSingleUseOAuthGrants(f.use(kid), "nous")
	if healed == nil || !healed.ProvidersBlock {
		t.Fatalf("providers block heal %+v", healed)
	}
	if _, ok := f.store(kid)["providers"].(map[string]any)["nous"]; ok {
		t.Fatal("forked nous block survived the heal")
	}

	rrow := grant(map[string]any{"id": "r1", "source": "device_code", "auth_type": "oauth", "last_refresh": "2026-01-01T00:00:00+00:00"})
	writeJSONPath(t, filepath.Join(f.root, "auth.json"), map[string]any{"version": 1, "credential_pool": map[string]any{"nous": []any{rrow}}})
	akid := f.profile("nousakid")
	akCopied := map[string]any{"id": "r1", "source": "device_code", "auth_type": "oauth", "access_token": "", "agent_key": "PAK", "last_refresh": "2026-06-01T00:00:00+00:00"}
	writeJSONPath(t, filepath.Join(akid, "auth.json"), map[string]any{"version": 1, "credential_pool": map[string]any{"nous": []any{akCopied}}})
	healForkedSingleUseOAuthGrants(f.use(akid), "nous")
	if got := f.store(akid)["credential_pool"].(map[string]any)["nous"]; !reflect.DeepEqual(got, []any{akCopied}) {
		t.Fatalf("agent key only row changed %v", got)
	}
	rootRows := f.store(f.root)["credential_pool"].(map[string]any)["nous"].([]any)
	if len(rootRows) != 1 || rowField(rootRows, 0, "id") != "r1" || rowField(rootRows, 0, "refresh_token") != "RT1" || rowField(rootRows, 0, "agent_key") != "AK" {
		t.Fatalf("root rows %v", rootRows)
	}
}

func TestOAuthGrantsStripIsANoopWithoutCredentials(t *testing.T) {
	newTestHome(t)
	got := StripClonedSingleUseOAuthGrants(t.TempDir())
	if !reflect.DeepEqual(got, ClonedGrantStrip{Pool: []string{}, Providers: []string{}, Files: []string{}}) {
		t.Fatalf("summary %+v", got)
	}
}

// Gives the root store an openai-codex pool row and a providers block
func seedCodexGrant(t *testing.T, root string) {
	fresh := msFromNow(3600)
	store := readJSONPath(t, filepath.Join(root, "auth.json"))
	store["credential_pool"].(map[string]any)["openai-codex"] = []any{map[string]any{
		"id": "cdx001", "label": "codex", "auth_type": "oauth", "priority": 0, "source": "manual:device_code",
		"access_token": "cdx-AT0", "refresh_token": "cdx-RT0", "expires_at_ms": fresh,
	}}
	store["providers"].(map[string]any)["openai-codex"] = map[string]any{
		"tokens":       map[string]any{"access_token": "cdx-AT0", "refresh_token": "cdx-RT0", "expires_at_ms": fresh},
		"last_refresh": float64(fresh) / 1000.0,
	}
	writeJSONPath(t, filepath.Join(root, "auth.json"), store)
}

func symlinkStore(target, alias string) error { return os.Symlink(target, alias) }

// Profile whose auth.json is the root store through the given link
func (f *grantFleet) sharedProfile(name string, link func(target, alias string) error) string {
	dir := f.profile(name)
	alias := filepath.Join(dir, "auth.json")
	os.Remove(alias)
	if err := link(filepath.Join(f.root, "auth.json"), alias); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func emptyStrip() ClonedGrantStrip {
	return ClonedGrantStrip{Pool: []string{}, Providers: []string{}, Files: []string{}}
}

func TestOAuthGrantsStripLeavesSharedRootAuthStoreUnchanged(t *testing.T) {
	for name, link := range map[string]func(string, string) error{"symlink": symlinkStore, "hardlink": os.Link} {
		t.Run(name, func(t *testing.T) {
			f := newGrantFleet(t)
			seedCodexGrant(t, f.root)
			before := readText(t, filepath.Join(f.root, "auth.json"))
			shared := f.sharedProfile("shared", link)
			if got := StripClonedSingleUseOAuthGrants(shared); !reflect.DeepEqual(got, emptyStrip()) {
				t.Fatalf("summary %+v", got)
			}
			if readText(t, filepath.Join(f.root, "auth.json")) != before {
				t.Fatal("shared root store was rewritten")
			}
		})
	}
}

func TestOAuthGrantsStripFailsClosedWhenRootStoreCannotBeResolved(t *testing.T) {
	f := newGrantFleet(t)
	seedCodexGrant(t, f.root)
	before := readText(t, filepath.Join(f.root, "auth.json"))
	shared := f.sharedProfile("shared", symlinkStore)
	orig := stripDefaultRootFn
	stripDefaultRootFn = func() (string, error) { return "", errors.New("root unavailable") }
	t.Cleanup(func() { stripDefaultRootFn = orig })
	if got := StripClonedSingleUseOAuthGrants(shared); !reflect.DeepEqual(got, emptyStrip()) {
		t.Fatalf("summary %+v", got)
	}
	if readText(t, filepath.Join(f.root, "auth.json")) != before {
		t.Fatal("root store mutated while its identity was unknown")
	}
}

func TestOAuthGrantsStripFailsClosedWhenStoreIdentityCheckErrors(t *testing.T) {
	f := newGrantFleet(t)
	seedCodexGrant(t, f.root)
	copied := f.profile("copied")
	os.WriteFile(filepath.Join(copied, "auth.json"), []byte(readText(t, filepath.Join(f.root, "auth.json"))), 0o600)
	before := readText(t, filepath.Join(copied, "auth.json"))
	orig := stripSameFileFn
	stripSameFileFn = func(a, b string) (bool, error) { return false, errors.New("stat unavailable") }
	t.Cleanup(func() { stripSameFileFn = orig })
	if got := StripClonedSingleUseOAuthGrants(copied); !reflect.DeepEqual(got, emptyStrip()) {
		t.Fatalf("summary %+v", got)
	}
	if readText(t, filepath.Join(copied, "auth.json")) != before {
		t.Fatal("copied store mutated after a failed identity check")
	}
}

func TestOAuthGrantsFirstProfileRotationDoesNotStrandRootOrSiblings(t *testing.T) {
	f := newGrantFleet(t)
	forge, atlas := f.profile("forge"), f.profile("atlas")
	mustSelectToken(t, f.use(forge), "anthropic", "sk-ant-oat01-AT1")
	if f.rows(forge) != nil || rowField(f.rows(f.root), 0, "refresh_token") != "sk-ant-ort-RT1" {
		t.Fatalf("forge rows %v root rows %v", f.rows(forge), f.rows(f.root))
	}
	for _, h := range []string{atlas, f.root, forge} {
		mustSelectToken(t, f.use(h), "anthropic", "sk-ant-oat01-AT1")
	}
	if got := f.serverLog(); !reflect.DeepEqual(got, []string{"ROTATE"}) {
		t.Fatalf("server log %v", got)
	}
	if f.rows(atlas) != nil || f.rows(forge) != nil {
		t.Fatal("a profile grew a local copy")
	}
}

func TestOAuthGrantsAgentInitResolverSeesSiblingRotation(t *testing.T) {
	f := newGrantFleet(t)
	forge, atlas := f.profile("forge"), f.profile("atlas")
	LoadPool(f.use(forge), "anthropic").Select("")
	if got := ResolveAnthropicToken(f.use(atlas), ""); got != "sk-ant-oat01-AT1" {
		t.Fatalf("resolved %q", got)
	}
}

func TestOAuthGrantsBorrowingProfileLoadPoolDoesNotMaterializeLocalCopy(t *testing.T) {
	f := newGrantFleet(t)
	fresh := f.profile("fresh")
	pool := LoadPool(f.use(fresh), "anthropic")
	if ids := entryIDsOf(pool); !reflect.DeepEqual(ids, []string{"abc123"}) {
		t.Fatalf("entries %v", ids)
	}
	if !reflect.DeepEqual(pool.borrowedRootIDs, map[string]bool{"abc123": true}) {
		t.Fatalf("borrowed ids %v", pool.borrowedRootIDs)
	}
	if f.rows(fresh) != nil {
		t.Fatal("borrowed rows materialized in the profile")
	}
}

func entryIDsOf(pool *Pool) []string {
	out := []string{}
	for _, e := range pool.Entries() {
		out = append(out, e.ID)
	}
	return out
}

func entrySourcesOf(pool *Pool) []string {
	out := []string{}
	for _, e := range pool.Entries() {
		out = append(out, e.Source)
	}
	return out
}

func writeAnthropicSingleton(t *testing.T, path, access, refresh string, expiresAt int64) {
	writeJSONPath(t, path, map[string]any{"accessToken": access, "refreshToken": refresh, "expiresAt": expiresAt})
}

// Root whose only anthropic grant is its PKCE singleton with anthropic as the active provider
func (f *grantFleet) rootSingletonOnly() {
	writeAnthropicSingleton(f.t, filepath.Join(f.root, ".anthropic_oauth.json"), "sk-ant-oat01-AT0", "sk-ant-ort-RT0", msFromNow(-3600))
	store := f.store(f.root)
	store["active_provider"] = "anthropic"
	delete(store["credential_pool"].(map[string]any), "anthropic")
	writeJSONPath(f.t, filepath.Join(f.root, "auth.json"), store)
}

func TestOAuthGrantsBorrowerPruneNeverDeletesRootSingletonGrant(t *testing.T) {
	f := newGrantFleet(t)
	f.rootSingletonOnly()
	if got := entrySourcesOf(LoadPool(f.use(f.root), "anthropic")); !reflect.DeepEqual(got, []string{"hermes_pkce"}) {
		t.Fatalf("root sources %v", got)
	}
	kid := f.profile("kid")
	pool := LoadPool(f.use(kid), "anthropic")
	if got := entrySourcesOf(pool); !reflect.DeepEqual(got, []string{"hermes_pkce"}) {
		t.Fatalf("borrowed root grant was pruned %v", got)
	}
	if rows := f.rows(f.root); len(rows) == 0 || rowField(rows, 0, "source") != "hermes_pkce" || f.rows(kid) != nil {
		t.Fatalf("root rows %v kid rows %v", rows, f.rows(kid))
	}
	if sel, ok := pool.Select(""); !ok || sel.AccessToken != "sk-ant-oat01-AT1" {
		t.Fatalf("selected %+v", sel)
	}
	if got := readJSONPath(t, filepath.Join(f.root, ".anthropic_oauth.json"))["refreshToken"]; got != "sk-ant-ort-RT1" {
		t.Fatalf("root singleton refresh %v", got)
	}
	if pathExists(filepath.Join(kid, ".anthropic_oauth.json")) || rowField(f.rows(f.root), 0, "refresh_token") != "sk-ant-ort-RT1" {
		t.Fatal("rotation did not commit at root")
	}
}

func TestOAuthGrantsProfileAuthAddOwnsOnlyItsOwnRows(t *testing.T) {
	f := newGrantFleet(t)
	kid := f.profile("kid")
	if _, err := LoadPool(f.use(kid), "anthropic").AddEntry(PooledCredential{Provider: "anthropic", ID: "own001", Label: "mine", AuthType: AuthTypeOAuth,
		Source: "manual:hermes_pkce", AccessToken: "sk-ant-oat01-MINE", RefreshToken: "rt-mine", Extra: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if got := idsOf(f.rows(kid)); !reflect.DeepEqual(got, []string{"own001"}) {
		t.Fatalf("borrowed root row was copied into the profile %v", got)
	}
	if got := idsOf(f.rows(f.root)); !reflect.DeepEqual(got, []string{"abc123"}) {
		t.Fatalf("root rows %v", got)
	}
}

func TestOAuthGrantsProfileFreshAddWithNoLoginAnywhereLandsInTheProfile(t *testing.T) {
	for _, provider := range sortedSingleUseProviders() {
		t.Run(provider, func(t *testing.T) {
			f := newGrantFleet(t)
			rootFile := filepath.Join(f.root, "auth.json")
			store := f.store(f.root)
			delete(store["credential_pool"].(map[string]any), provider)
			writeJSONPath(t, rootFile, store)
			kid := f.profile("kid")
			rootBefore := readText(t, rootFile)
			if _, err := LoadPool(f.use(kid), provider).AddEntry(PooledCredential{Provider: provider, ID: "fresh1", Label: "mine", AuthType: AuthTypeOAuth,
				Source: "manual:device_code", AccessToken: "at-fresh", RefreshToken: "rt-fresh", Extra: map[string]any{}}); err != nil {
				t.Fatal(err)
			}
			if !containsString(entryIDsOf(LoadPool(f.use(kid), provider)), "fresh1") {
				t.Fatal("fresh login unreadable after the add")
			}
			if !containsString(idsOf(f.store(kid)["credential_pool"].(map[string]any)[provider].([]any)), "fresh1") {
				t.Fatal("fresh login missing from the profile store")
			}
			if readText(t, rootFile) != rootBefore {
				t.Fatal("root store changed")
			}
		})
	}
}

func TestOAuthGrantsAnthropicAuthAddSurvivesTheNextLoadBesideARootSingleton(t *testing.T) {
	cases := map[string][]any{
		"root_pool_empty":  {},
		"root_api_key_row": {map[string]any{"id": "rootkey", "label": "root-key", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "root-api-key"}},
	}
	for name, rootRows := range cases {
		t.Run(name, func(t *testing.T) {
			f := newGrantFleet(t)
			store := f.store(f.root)
			store["credential_pool"].(map[string]any)["anthropic"] = rootRows
			writeJSONPath(t, filepath.Join(f.root, "auth.json"), store)
			singleton := filepath.Join(f.root, ".anthropic_oauth.json")
			writeAnthropicSingleton(t, singleton, "at-root", "rt-root", msFromNow(-3600))
			singletonBefore := readText(t, singleton)
			kid := f.profile("kid")
			if _, err := LoadPool(f.use(kid), "anthropic").AddEntry(PooledCredential{Provider: "anthropic", ID: "kid001", Label: "mine", AuthType: AuthTypeOAuth,
				Source: "manual:hermes_pkce", AccessToken: "at-kid", RefreshToken: "rt-kid", ExpiresAtMS: msFromNow(8 * 3600), Extra: map[string]any{}}); err != nil {
				t.Fatal(err)
			}
			if got := idsOf(f.rows(kid)); !reflect.DeepEqual(got, []string{"kid001"}) {
				t.Fatalf("kid rows %v", got)
			}
			if !containsString(entryIDsOf(LoadPool(f.use(kid), "anthropic")), "kid001") {
				t.Fatal("auth add row lost on the next load")
			}
			if got := idsOf(f.rows(kid)); !reflect.DeepEqual(got, []string{"kid001"}) {
				t.Fatalf("kid rows after reload %v", got)
			}
			if readText(t, singleton) != singletonBefore {
				t.Fatal("root singleton changed")
			}
		})
	}
}

func TestOAuthGrantsProfileAuthAddNeverCopiesRootsProviderLogin(t *testing.T) {
	cases := []struct {
		provider string
		login    map[string]any
	}{
		{"nous", map[string]any{"access_token": "at-root", "refresh_token": "rt-root", "agent_key": "ak-root"}},
		{"openai-codex", map[string]any{"tokens": map[string]any{"access_token": "at-root", "refresh_token": "rt-root"}}},
		{"xai-oauth", map[string]any{"tokens": map[string]any{"access_token": "at-root", "refresh_token": "rt-root"}}},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			f := newGrantFleet(t)
			store := f.store(f.root)
			delete(store["credential_pool"].(map[string]any), tc.provider)
			store["providers"].(map[string]any)[tc.provider] = tc.login
			writeJSONPath(t, filepath.Join(f.root, "auth.json"), store)
			kid := f.profile("kid")
			if _, err := LoadPool(f.use(kid), tc.provider).AddEntry(PooledCredential{Provider: tc.provider, ID: "mine01", Label: "mine", AuthType: AuthTypeOAuth,
				Source: "manual:device_code", AccessToken: "at-mine", RefreshToken: "rt-mine", Extra: map[string]any{}}); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(readText(t, filepath.Join(kid, "auth.json")), "rt-root") {
				t.Fatal("root refresh token copied by the add")
			}
			if !containsString(entryIDsOf(LoadPool(f.use(kid), tc.provider)), "mine01") {
				t.Fatal("added row lost on the next load")
			}
			if strings.Contains(readText(t, filepath.Join(kid, "auth.json")), "rt-root") {
				t.Fatal("root refresh token copied by the reload")
			}
		})
	}
}

func TestOAuthGrantsClassicModePersistIsUnchanged(t *testing.T) {
	f := newGrantFleet(t)
	mustSelectToken(t, f.use(f.root), "anthropic", "sk-ant-oat01-AT1")
	if rowField(f.rows(f.root), 0, "refresh_token") != "sk-ant-ort-RT1" {
		t.Fatalf("root rows %v", f.rows(f.root))
	}
}

func TestOAuthGrantsStaleTerminalVerdictCannotKillPeerTokenGeneration(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		t.Run(fmt.Sprintf("borrowed=%v", borrowed), func(t *testing.T) {
			f := newGrantFleet(t)
			h := f.root
			if borrowed {
				h = f.profile("stale-dead")
			}
			env := f.use(h)
			stale, staleBilling, peer, staleAdopter := LoadPool(env, "anthropic"), LoadPool(env, "anthropic"), LoadPool(env, "anthropic"), LoadPool(env, "anthropic")
			if got, ok := peer.TryRefreshMatching("", "abc123"); !ok || got.RefreshToken != "sk-ant-ort-RT1" {
				t.Fatalf("peer refresh %+v", got)
			}
			live := peer.entries[0]
			peer.persist(false, nil, nil)
			if !reflect.DeepEqual(peer.entries[0], live) {
				t.Fatal("a flush matching disk re-hydrated the live row")
			}
			adopted := staleAdopter.adopt(staleAdopter.entries[0], false, true)
			if adopted.RefreshToken != "sk-ant-ort-RT1" || !reflect.DeepEqual(adopted, staleAdopter.entries[0]) {
				t.Fatalf("stale adopter kept %+v", adopted)
			}
			stale.MarkExhaustedAndRotate(RotationRequest{StatusCode: 401, CredentialID: "abc123",
				ErrorContext: map[string]any{"reason": "invalid_grant", "message": "refresh token already used"}})
			r := f.rows(f.root)[0].(map[string]any)
			if r["refresh_token"] != "sk-ant-ort-RT1" || r["last_status"] == StatusDead {
				t.Fatalf("stale verdict killed the peer generation %v", r)
			}
			if sel, ok := stale.Select(""); !ok || sel.RefreshToken != "sk-ant-ort-RT1" {
				t.Fatalf("stale pool selected %+v", sel)
			}
			staleBilling.MarkExhaustedAndRotate(RotationRequest{StatusCode: 402, CredentialID: "abc123"})
			r = f.rows(f.root)[0].(map[string]any)
			if r["refresh_token"] != "sk-ant-ort-RT1" || r["last_status"] != StatusExhausted {
				t.Fatalf("billing verdict row %v", r)
			}
		})
	}
}

func TestOAuthGrantsTerminalVerdictForCurrentTokenGenerationStillPersists(t *testing.T) {
	f := newGrantFleet(t)
	pool := LoadPool(f.use(f.root), "anthropic")
	pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: 401, CredentialID: "abc123",
		ErrorContext: map[string]any{"reason": "invalid_grant", "message": "refresh token already used"}})
	r := f.rows(f.root)[0].(map[string]any)
	if r["refresh_token"] != "sk-ant-ort-RT0" || r["last_status"] != StatusDead {
		t.Fatalf("row %v", r)
	}
	if _, ok := pool.Select(""); ok {
		t.Fatal("a dead row was selected")
	}
}

func TestHealConsolidatesExistingForksToTheLiveCopy(t *testing.T) {
	f := newGrantFleet(t)
	forge := f.fork("forge", 1)
	atlas := f.fork("atlas", 0)
	if rowField(f.rows(forge), 0, "refresh_token") != "sk-ant-ort-RT1" || rowField(f.rows(atlas), 0, "refresh_token") != "sk-ant-ort-RT0" {
		t.Fatal("fork setup")
	}
	rec := &recordingHandler{}
	env := f.use(forge)
	env.Log = newRecordingLogger(rec)
	mustSelectToken(t, env, "anthropic", "sk-ant-oat01-AT2")
	if f.rows(forge) != nil || rowField(f.rows(f.root), 0, "refresh_token") != "sk-ant-ort-RT2" || rowField(f.rows(f.root), 0, "id") != "abc123" {
		t.Fatalf("forge rows %v root rows %v", f.rows(forge), f.rows(f.root))
	}
	healed := rec.containing("consolidated forked anthropic OAuth grant")
	if len(healed) != 1 || !strings.Contains(healed[0], "profile forge") || !strings.Contains(healed[0], "root updated") {
		t.Fatalf("heal records %v", healed)
	}
	for _, h := range []string{atlas, f.root, forge} {
		mustSelectToken(t, f.use(h), "anthropic", "sk-ant-oat01-AT2")
	}
	if f.rows(atlas) != nil || f.rows(forge) != nil {
		t.Fatal("a profile copy survived")
	}
	if got := f.serverLog(); !reflect.DeepEqual(got, []string{"ROTATE"}) {
		t.Fatalf("server log %v", got)
	}
	for _, h := range []string{forge, atlas} {
		if f.store(h)["credential_pool"].(map[string]any)["openai"].([]any)[0].(map[string]any)["access_token"] != "sk-static-key" {
			t.Fatal("api key row touched")
		}
	}
}

func TestHealIsIdempotentAndLogsOnce(t *testing.T) {
	f := newGrantFleet(t)
	kid := f.fork("kid", 0)
	rec := &recordingHandler{}
	env := f.use(kid)
	env.Log = newRecordingLogger(rec)
	LoadPool(env, "anthropic")
	if f.rows(kid) != nil {
		t.Fatal("fork survived")
	}
	notices := ConsumeOAuthHealNotices()
	if len(notices) != 1 || !strings.Contains(notices[0], "profile kid") {
		t.Fatalf("notices %v", notices)
	}
	rootBefore := readText(t, filepath.Join(f.root, "auth.json"))
	if got := healForkedSingleUseOAuthGrants(env, "anthropic"); got != nil {
		t.Fatalf("second heal %+v", got)
	}
	LoadPool(env, "anthropic")
	if got := ConsumeOAuthHealNotices(); len(got) != 0 {
		t.Fatalf("notices %v", got)
	}
	if readText(t, filepath.Join(f.root, "auth.json")) != rootBefore {
		t.Fatal("root rewritten by an idempotent heal")
	}
	if n := len(rec.containing("consolidated forked")); n != 1 {
		t.Fatalf("heal logged %d times", n)
	}
}

func TestHealNeverDeletesTheOnlySurvivingCopy(t *testing.T) {
	f := newGrantFleet(t)
	kid := f.fork("kid", 1)
	store := f.store(f.root)
	delete(store["credential_pool"].(map[string]any), "anthropic")
	writeJSONPath(t, filepath.Join(f.root, "auth.json"), store)
	mustSelectToken(t, f.use(kid), "anthropic", "sk-ant-oat01-AT2")
	if rows := f.rows(kid); len(rows) == 0 || rowField(rows, 0, "refresh_token") != "sk-ant-ort-RT2" {
		t.Fatalf("kid rows %v", rows)
	}
	if _, ok := f.store(f.root)["credential_pool"].(map[string]any)["anthropic"]; ok {
		t.Fatal("root regained the grant")
	}
}

func TestHealPreservesIndependentGrantsForSameAccount(t *testing.T) {
	for _, shape := range []string{"pool", "provider"} {
		for _, claims := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s-claims=%v", shape, claims), func(t *testing.T) {
				f := newGrantFleet(t)
				pair := func(tag string) map[string]any {
					access := tag
					if claims {
						access = jwtWithClaims(map[string]any{"sub": "same-account", "exp": time.Now().Unix() + 3600, "jti": tag})
					}
					return map[string]any{"access_token": access, "refresh_token": "independent-" + tag}
				}
				store := func(tag string) map[string]any {
					tokens := pair(tag)
					if shape == "provider" {
						return map[string]any{"version": 1, "providers": map[string]any{"openai-codex": map[string]any{"tokens": tokens}}}
					}
					rowMap := map[string]any{"id": tag, "auth_type": "oauth", "source": "manual:device_code"}
					for k, v := range tokens {
						rowMap[k] = v
					}
					return map[string]any{"version": 1, "providers": map[string]any{}, "credential_pool": map[string]any{"openai-codex": []any{rowMap}}}
				}
				rootFile := filepath.Join(f.root, "auth.json")
				kid := f.profile("independent")
				profileFile := filepath.Join(kid, "auth.json")
				writeJSONPath(t, rootFile, store("root-grant"))
				writeJSONPath(t, profileFile, store("profile-grant"))
				beforeRoot, beforeProfile := readText(t, rootFile), readText(t, profileFile)
				env := f.use(kid)
				if got := healForkedSingleUseOAuthGrants(env, "openai-codex"); got != nil {
					t.Fatalf("heal %+v", got)
				}
				if readText(t, rootFile) != beforeRoot || readText(t, profileFile) != beforeProfile {
					t.Fatal("an independent grant was rewritten")
				}
				if claims {
					for i := 0; i < 3; i++ {
						sel, ok := LoadPool(env, "openai-codex").Select("")
						if !ok || sel.RefreshToken != "independent-profile-grant" {
							t.Fatalf("selected %+v", sel)
						}
					}
					if readText(t, rootFile) != beforeRoot {
						t.Fatal("root changed")
					}
				}
			})
		}
	}
}

func utcStamp(offsetSeconds float64) string {
	return time.Now().Add(time.Duration(offsetSeconds * float64(time.Second))).UTC().Format("2006-01-02T15:04:05Z")
}

func TestHealRotatedForkMovesProviderBlockWithThePoolRow(t *testing.T) {
	f := newGrantFleet(t)
	store := func(tag string, expOff float64) map[string]any {
		tokens := map[string]any{"access_token": "at-" + tag, "refresh_token": "rt-" + tag}
		return map[string]any{"version": 1,
			"providers": map[string]any{"openai-codex": map[string]any{"tokens": tokens, "last_refresh": utcStamp(expOff - 3600)}},
			"credential_pool": map[string]any{"openai-codex": []any{map[string]any{"id": "shared-id", "auth_type": "oauth", "source": "device_code",
				"access_token": tokens["access_token"], "refresh_token": tokens["refresh_token"], "expires_at_ms": msFromNow(expOff)}}}}
	}
	rootFile := filepath.Join(f.root, "auth.json")
	kid := f.profile("rotated")
	writeJSONPath(t, rootFile, store("old", 100))
	writeJSONPath(t, filepath.Join(kid, "auth.json"), store("new", 3600))
	healed := healForkedSingleUseOAuthGrants(f.use(kid), "openai-codex")
	if healed == nil || !healed.Adopted {
		t.Fatalf("heal %+v", healed)
	}
	r, p := f.store(f.root), f.store(kid)
	if r["credential_pool"].(map[string]any)["openai-codex"].([]any)[0].(map[string]any)["refresh_token"] != "rt-new" {
		t.Fatal("root pool row kept the spent pair")
	}
	if r["providers"].(map[string]any)["openai-codex"].(map[string]any)["tokens"].(map[string]any)["refresh_token"] != "rt-new" {
		t.Fatal("root providers block kept the spent pair")
	}
	if _, ok := p["providers"].(map[string]any)["openai-codex"]; ok {
		t.Fatal("profile block survived")
	}
	if cp, ok := p["credential_pool"].(map[string]any); ok {
		if _, ok := cp["openai-codex"]; ok {
			t.Fatal("profile pool row survived")
		}
	}
	for _, e := range LoadPool(f.use(f.root), "openai-codex").Entries() {
		if e.RefreshToken != "rt-new" {
			t.Fatalf("root entry %+v", e)
		}
	}
}

func TestHealKeepsTheLaterIssuedForkEvenWhenItExpiresSooner(t *testing.T) {
	f := newGrantFleet(t)
	store := func(tag string, issuedOff, ttl float64) map[string]any {
		tokens := map[string]any{"access_token": "at-" + tag, "refresh_token": "rt-" + tag}
		issued := utcStamp(issuedOff)
		return map[string]any{"version": 1,
			"providers": map[string]any{"openai-codex": map[string]any{"tokens": tokens, "last_refresh": issued}},
			"credential_pool": map[string]any{"openai-codex": []any{map[string]any{"id": "shared-id", "auth_type": "oauth", "source": "device_code",
				"access_token": tokens["access_token"], "refresh_token": tokens["refresh_token"], "last_refresh": issued, "expires_at_ms": msFromNow(issuedOff + ttl)}}}}
	}
	kid := f.profile("short-ttl")
	writeJSONPath(t, filepath.Join(f.root, "auth.json"), store("old", -600, 3600))
	writeJSONPath(t, filepath.Join(kid, "auth.json"), store("new", -5, 900))
	healed := healForkedSingleUseOAuthGrants(f.use(kid), "openai-codex")
	if healed == nil || !healed.Adopted {
		t.Fatalf("heal %+v", healed)
	}
	r := f.store(f.root)
	if r["credential_pool"].(map[string]any)["openai-codex"].([]any)[0].(map[string]any)["refresh_token"] != "rt-new" ||
		r["providers"].(map[string]any)["openai-codex"].(map[string]any)["tokens"].(map[string]any)["refresh_token"] != "rt-new" {
		t.Fatalf("root %v", r)
	}
}

func TestHealLeavesADifferentAccountAlone(t *testing.T) {
	f := newGrantFleet(t)
	jwt := func(sub string) string {
		return jwtWithClaims(map[string]any{"sub": sub, "exp": time.Now().Unix() + 3600})
	}
	rootStore := f.store(f.root)
	rootStore["credential_pool"].(map[string]any)["xai-oauth"] = []any{map[string]any{"id": "rootx", "auth_type": "oauth", "priority": 0,
		"source": "manual:device_code", "access_token": jwt("alice"), "refresh_token": "xr-alice"}}
	writeJSONPath(t, filepath.Join(f.root, "auth.json"), rootStore)
	kid := f.profile("kid")
	writeJSONPath(t, filepath.Join(kid, "auth.json"), map[string]any{"version": 1, "providers": map[string]any{},
		"credential_pool": map[string]any{"xai-oauth": []any{
			map[string]any{"id": "kidx", "auth_type": "oauth", "priority": 0, "source": "manual:device_code", "access_token": jwt("bob"), "refresh_token": "xr-bob"},
			map[string]any{"id": "kidk", "auth_type": "api_key", "priority": 1, "source": "manual", "access_token": "xai-static"},
		}}})
	LoadPool(f.use(kid), "xai-oauth")
	if got := idsOf(f.store(kid)["credential_pool"].(map[string]any)["xai-oauth"].([]any)); !reflect.DeepEqual(got, []string{"kidx", "kidk"}) {
		t.Fatalf("kid rows %v", got)
	}
	if f.store(f.root)["credential_pool"].(map[string]any)["xai-oauth"].([]any)[0].(map[string]any)["refresh_token"] != "xr-alice" {
		t.Fatal("root grant changed")
	}
}

func TestHealPKCESingletonShapeCommitsLivePairToRootSingleton(t *testing.T) {
	f := newGrantFleet(t)
	f.rootSingletonOnly()
	LoadPool(f.use(f.root), "anthropic")
	kid := f.profile("kid")
	rootAuth := filepath.Join(f.root, "auth.json")
	st, _ := os.Stat(rootAuth)
	os.WriteFile(filepath.Join(kid, "auth.json"), []byte(readText(t, rootAuth)), 0o600)
	os.Chtimes(filepath.Join(kid, "auth.json"), st.ModTime(), st.ModTime())
	writeAnthropicSingleton(t, filepath.Join(kid, ".anthropic_oauth.json"), "sk-ant-oat01-AT1", "sk-ant-ort-RT1", msFromNow(-60))
	kstore := f.store(kid)
	krow := kstore["credential_pool"].(map[string]any)["anthropic"].([]any)[0].(map[string]any)
	krow["access_token"], krow["refresh_token"], krow["expires_at_ms"] = "sk-ant-oat01-AT1", "sk-ant-ort-RT1", msFromNow(-60)
	writeJSONPath(t, filepath.Join(kid, "auth.json"), kstore)
	f.mu.Lock()
	f.spent["sk-ant-ort-RT0"] = true
	f.valid = map[string]bool{"sk-ant-ort-RT1": true}
	f.n = 1
	f.mu.Unlock()

	mustSelectToken(t, f.use(kid), "anthropic", "sk-ant-oat01-AT2")
	if pathExists(filepath.Join(kid, ".anthropic_oauth.json")) || f.rows(kid) != nil {
		t.Fatal("profile copy survived")
	}
	if got := readJSONPath(t, filepath.Join(f.root, ".anthropic_oauth.json"))["refreshToken"]; got != "sk-ant-ort-RT2" {
		t.Fatalf("root singleton refresh %v", got)
	}
	mustSelectToken(t, f.use(f.root), "anthropic", "sk-ant-oat01-AT2")
	if got := f.serverLog(); !reflect.DeepEqual(got, []string{"ROTATE"}) {
		t.Fatalf("server log %v", got)
	}
}

func TestHealIsANoopInClassicMode(t *testing.T) {
	f := newGrantFleet(t)
	env := f.use(f.root)
	before := readText(t, filepath.Join(f.root, "auth.json"))
	if got := healForkedSingleUseOAuthGrants(env, "anthropic"); got != nil {
		t.Fatalf("heal %+v", got)
	}
	if readText(t, filepath.Join(f.root, "auth.json")) != before {
		t.Fatal("classic root rewritten")
	}
}

func TestHealSkipsProfileAuthJSONSymlinkedToTheRootStore(t *testing.T) {
	f := newGrantFleet(t)
	seedCodexGrant(t, f.root)
	before := readText(t, filepath.Join(f.root, "auth.json"))
	shared := f.sharedProfile("shared", symlinkStore)
	if got := healForkedSingleUseOAuthGrants(f.use(shared), "openai-codex"); got != nil {
		t.Fatalf("heal %+v", got)
	}
	if readText(t, filepath.Join(f.root, "auth.json")) != before {
		t.Fatal("shared store rewritten")
	}
	if st, err := os.Lstat(filepath.Join(shared, "auth.json")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("alias replaced")
	}
	if got := ConsumeOAuthHealNotices(); len(got) != 0 {
		t.Fatalf("notices %v", got)
	}
	store := f.store(f.root)
	if got := idsOf(store["credential_pool"].(map[string]any)["openai-codex"].([]any)); !reflect.DeepEqual(got, []string{"cdx001"}) {
		t.Fatalf("codex rows %v", got)
	}
	if store["providers"].(map[string]any)["openai-codex"].(map[string]any)["tokens"].(map[string]any)["refresh_token"] != "cdx-RT0" {
		t.Fatal("codex block changed")
	}
}

func TestHealSkipsProfileAuthJSONHardlinkedToTheRootStore(t *testing.T) {
	f := newGrantFleet(t)
	seedCodexGrant(t, f.root)
	before := readText(t, filepath.Join(f.root, "auth.json"))
	shared := f.sharedProfile("twin", os.Link)
	if got := healForkedSingleUseOAuthGrants(f.use(shared), "openai-codex"); got != nil {
		t.Fatalf("heal %+v", got)
	}
	if readText(t, filepath.Join(f.root, "auth.json")) != before {
		t.Fatal("shared store rewritten")
	}
	if !SameAuthStore(filepath.Join(shared, "auth.json"), filepath.Join(f.root, "auth.json")) {
		t.Fatal("hardlink broken")
	}
}

func TestHealLeavesAnAliasedAnthropicSingletonAlone(t *testing.T) {
	f := newGrantFleet(t)
	rootSingleton := filepath.Join(f.root, ".anthropic_oauth.json")
	writeAnthropicSingleton(t, rootSingleton, "AT-shared", "RT-shared", msFromNow(3600))
	kid := f.profile("kid")
	writeJSONPath(t, filepath.Join(kid, "auth.json"), map[string]any{"providers": map[string]any{}, "credential_pool": map[string]any{}})
	os.Symlink(rootSingleton, filepath.Join(kid, ".anthropic_oauth.json"))
	before := readText(t, rootSingleton)
	if got := healForkedSingleUseOAuthGrants(f.use(kid), "anthropic"); got != nil {
		t.Fatalf("heal %+v", got)
	}
	if st, err := os.Lstat(filepath.Join(kid, ".anthropic_oauth.json")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("aliased singleton unlinked")
	}
	if readText(t, rootSingleton) != before {
		t.Fatal("root singleton rewritten")
	}
}

func TestHealPersistedMarkStillReHealsWhenTheRootStoreGainsAGrant(t *testing.T) {
	f := newGrantFleet(t)
	rootFile := filepath.Join(f.root, "auth.json")
	store := f.store(f.root)
	delete(store["credential_pool"].(map[string]any), "anthropic")
	writeJSONPath(t, rootFile, store)
	kid := f.profile("kid2")
	writeJSONPath(t, filepath.Join(kid, "auth.json"), map[string]any{"version": 1, "providers": map[string]any{},
		"credential_pool": map[string]any{"openai": []any{map[string]any{"id": "k1", "label": "static", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-local"}}}})
	fork := func() map[string]any {
		return map[string]any{"id": "abc123", "label": "team-grant", "auth_type": "oauth", "priority": 0, "source": "manual:hermes_pkce",
			"access_token": "sk-ant-oat01-AT0", "refresh_token": "sk-ant-ort-RT0", "expires_at_ms": msFromNow(3600), "base_url": "https://api.anthropic.com"}
	}
	kidStore := f.store(kid)
	kidStore["credential_pool"].(map[string]any)["anthropic"] = []any{fork()}
	writeJSONPath(t, filepath.Join(kid, "auth.json"), kidStore)

	env := f.use(kid)
	if got := healForkedSingleUseOAuthGrants(env, "anthropic"); got != nil {
		t.Fatalf("heal %+v", got)
	}
	if len(f.rows(kid)) == 0 {
		t.Fatal("the only surviving copy must not be stripped")
	}
	marked := readText(t, oauthHealCleanMarkPath(env))

	store["credential_pool"].(map[string]any)["anthropic"] = []any{fork()}
	writeJSONPath(t, rootFile, store)
	if got := healForkedSingleUseOAuthGrants(f.use(kid), "anthropic"); got == nil {
		t.Fatal("the persisted mark skipped a heal that had become necessary")
	}
	if len(f.rows(kid)) != 0 {
		t.Fatal("the fork survived in the profile store")
	}
	env = f.use(kid)
	if got := healForkedSingleUseOAuthGrants(env, "anthropic"); got != nil {
		t.Fatalf("clean heal %+v", got)
	}
	if readText(t, oauthHealCleanMarkPath(env)) == marked {
		t.Fatal("clean mark not re-stamped")
	}
}

func TestOAuthGrantsMergeSnapshotKeepsLiveGenerations(t *testing.T) {
	snapshot := map[string]any{
		"credential_pool": map[string]any{
			"openai-codex": []any{
				map[string]any{"id": "kept", "auth_type": "oauth", "label": "snap-label", "access_token": "old-at", "refresh_token": "old-rt"},
				map[string]any{"id": "spent", "auth_type": "oauth", "access_token": "gone-at", "refresh_token": "gone-rt"},
				map[string]any{"id": "key", "auth_type": "api_key", "access_token": "snap-key"},
			},
			"openrouter": []any{map[string]any{"id": "or", "auth_type": "api_key", "access_token": "or-snap"}},
		},
		"providers": map[string]any{"xai-oauth": map[string]any{"tokens": map[string]any{"refresh_token": "old-x"}}, "nous": map[string]any{"agent_key": "k"}},
	}
	live := map[string]any{
		"credential_pool": map[string]any{"openai-codex": []any{
			map[string]any{"id": "kept", "auth_type": "oauth", "label": "live-label", "access_token": "new-at", "refresh_token": "new-rt"},
			map[string]any{"id": "fresh", "auth_type": "oauth", "access_token": "f-at", "refresh_token": "f-rt"},
			map[string]any{"auth_type": "oauth", "access_token": "noid-at", "refresh_token": "noid-rt"},
		}},
		"providers": map[string]any{"openai-codex": map[string]any{"tokens": map[string]any{"access_token": "c", "refresh_token": "live-c"}}},
	}
	got := MergeSnapshotAuthPreservingLiveSingleUseGrants(snapshot, live)
	rows := got["credential_pool"].(map[string]any)["openai-codex"].([]any)
	if ids := idsOf(rows); !reflect.DeepEqual(ids, []string{"", "fresh", "kept", "key"}) {
		t.Fatalf("codex rows %v", ids)
	}
	kept := rows[2].(map[string]any)
	if kept["label"] != "snap-label" || kept["refresh_token"] != "new-rt" || kept["access_token"] != "new-at" {
		t.Fatalf("kept row %v", kept)
	}
	if got["credential_pool"].(map[string]any)["openrouter"].([]any)[0].(map[string]any)["access_token"] != "or-snap" {
		t.Fatal("ordinary credentials not restored from the snapshot")
	}
	providers := got["providers"].(map[string]any)
	if _, ok := providers["xai-oauth"]; ok {
		t.Fatal("snapshot only refresh grant revived")
	}
	if providers["openai-codex"].(map[string]any)["tokens"].(map[string]any)["refresh_token"] != "live-c" || providers["nous"] == nil {
		t.Fatalf("providers %v", providers)
	}
	if snapshot["credential_pool"].(map[string]any)["openai-codex"].([]any)[0].(map[string]any)["refresh_token"] != "old-rt" {
		t.Fatal("snapshot input mutated")
	}
}

func TestOAuthGrantsIdentityAndCounterpart(t *testing.T) {
	codex := jwtWithClaims(map[string]any{"sub": "s", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": " acct "}})
	if got := oauthIdentity(map[string]any{"access_token": codex}); got != "acct" {
		t.Fatalf("identity %q", got)
	}
	if got := oauthIdentity(map[string]any{"access_token": "sk-ant-oat01-x", "id_token": jwtWithClaims(map[string]any{"email": "a@b"})}); got != "a@b" {
		t.Fatalf("identity %q", got)
	}
	if got := oauthIdentity(map[string]any{"access_token": "sk-ant-oat01-x"}); got != "" {
		t.Fatalf("identity %q", got)
	}
	rootRows := []any{map[string]any{"id": "k", "auth_type": "api_key", "access_token": "shared"}, map[string]any{"id": "o", "auth_type": "oauth", "access_token": "a1", "refresh_token": "r1"}}
	if findRootCounterpart(map[string]any{"id": "x", "refresh_token": "r1"}, rootRows) != 1 || findRootCounterpart(map[string]any{"access_token": "shared"}, rootRows) != -1 {
		t.Fatal("counterpart lineage")
	}
}
