package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

func resetMultiplex(t *testing.T) {
	t.Helper()
	SetMultiplexActive(false)
	t.Cleanup(func() { SetMultiplexActive(false) })
}

func secretOf(ctx context.Context, name string) (string, bool, error) { return GetSecret(ctx, name) }

func TestSecretScopeReadsEnviron(t *testing.T) {
	resetMultiplex(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	v, ok, err := secretOf(context.Background(), "ANTHROPIC_API_KEY")
	if err != nil || !ok || v != "sk-test" {
		t.Fatalf("got %q %t %v", v, ok, err)
	}
}

func TestSecretScopeMissingReturnsDefault(t *testing.T) {
	resetMultiplex(t)
	os.Unsetenv("NOPE_KEY")
	if _, ok, _ := secretOf(context.Background(), "NOPE_KEY"); ok {
		t.Fatal("missing must not be found")
	}
	if GetSecretStr(context.Background(), "NOPE_KEY", "fallback") != "fallback" {
		t.Fatal("default expected")
	}
}

func TestUnscopedReadRaisesUnderMultiplex(t *testing.T) {
	resetMultiplex(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-leaky")
	SetMultiplexActive(true)
	_, _, err := secretOf(context.Background(), "ANTHROPIC_API_KEY")
	var unscoped *UnscopedSecretError
	if err == nil || !isUnscoped(err, &unscoped) {
		t.Fatalf("UnscopedSecretError expected, got %v", err)
	}
}

func isUnscoped(err error, target **UnscopedSecretError) bool {
	u, ok := err.(*UnscopedSecretError)
	if ok {
		*target = u
	}
	return ok
}

func TestScopedMissingKeyReturnsDefaultNotEnviron(t *testing.T) {
	resetMultiplex(t)
	t.Setenv("OPENAI_API_KEY", "sk-other-profile")
	SetMultiplexActive(true)
	ctx := WithSecretScope(context.Background(), map[string]string{"ANTHROPIC_API_KEY": "sk-mine"}, "")
	if _, ok, err := secretOf(ctx, "OPENAI_API_KEY"); ok || err != nil {
		t.Fatal("a scoped miss must not fall through to the environment")
	}
	if GetSecretStr(ctx, "OPENAI_API_KEY", "d") != "d" {
		t.Fatal("default expected")
	}
}

func TestScopeHitWinsOverEnviron(t *testing.T) {
	resetMultiplex(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-environ")
	ctx := WithSecretScope(context.Background(), map[string]string{"ANTHROPIC_API_KEY": "sk-from-env-file"}, "")
	if v, _, _ := secretOf(ctx, "ANTHROPIC_API_KEY"); v != "sk-from-env-file" {
		t.Fatalf("got %q", v)
	}
}

func TestScopeMissAbsentEverywhereReturnsDefault(t *testing.T) {
	resetMultiplex(t)
	os.Unsetenv("NOPE_KEY")
	ctx := WithSecretScope(context.Background(), map[string]string{}, "")
	if _, ok, _ := secretOf(ctx, "NOPE_KEY"); ok {
		t.Fatal("not found expected")
	}
	if GetSecretStr(ctx, "NOPE_KEY", "d") != "d" {
		t.Fatal("default expected")
	}
}

func TestMultiplexOnStillAuthoritative(t *testing.T) {
	resetMultiplex(t)
	t.Setenv("OPENAI_API_KEY", "sk-other-profile")
	SetMultiplexActive(true)
	ctx := WithSecretScope(context.Background(), map[string]string{}, "")
	if _, ok, _ := secretOf(ctx, "OPENAI_API_KEY"); ok {
		t.Fatal("a scoped miss under multiplex fails closed")
	}
}

func TestScopedMissUnderForeignHomeReturnsDefault(t *testing.T) {
	resetMultiplex(t)
	newTestHome(t)
	t.Setenv("OPENAI_API_KEY", "sk-launch-profile")
	ctx := WithSecretScope(home.WithOverride(context.Background(), filepath.Join(t.TempDir(), "other-profile")), map[string]string{}, "")
	if !ServesRoutedProfile(ctx) {
		t.Fatal("a foreign override routes the task")
	}
	if _, ok, _ := secretOf(ctx, "OPENAI_API_KEY"); ok {
		t.Fatal("a routed miss must fail closed")
	}
	if GetSecretStr(ctx, "OPENAI_API_KEY", "d") != "d" {
		t.Fatal("default expected")
	}
}

func TestScopedMissUnderOwnHomeKeepsEnvOverlay(t *testing.T) {
	resetMultiplex(t)
	h := newTestHome(t)
	t.Setenv("OPENAI_API_KEY", "sk-own-env")
	ctx := WithSecretScope(home.WithOverride(context.Background(), h), map[string]string{}, "")
	if ServesRoutedProfile(ctx) {
		t.Fatal("the process home is not routed")
	}
	if v, _, _ := secretOf(ctx, "OPENAI_API_KEY"); v != "sk-own-env" {
		t.Fatalf("got %q", v)
	}
}

func TestScopeHitUnderForeignHomeStillWins(t *testing.T) {
	resetMultiplex(t)
	newTestHome(t)
	t.Setenv("OPENAI_API_KEY", "sk-launch-profile")
	ctx := WithSecretScope(home.WithOverride(context.Background(), filepath.Join(t.TempDir(), "other-profile")), map[string]string{"OPENAI_API_KEY": "sk-served-profile"}, "")
	if v, _, _ := secretOf(ctx, "OPENAI_API_KEY"); v != "sk-served-profile" {
		t.Fatalf("got %q", v)
	}
}

func TestStampedForeignScopeMissFailsClosedWithoutOverride(t *testing.T) {
	resetMultiplex(t)
	newTestHome(t)
	t.Setenv("OPENAI_API_KEY", "sk-launch-profile")
	ctx := WithSecretScope(context.Background(), map[string]string{}, filepath.Join(t.TempDir(), "other-profile"))
	if !ServesRoutedProfile(ctx) {
		t.Fatal("the profile home stamp routes the task")
	}
	if _, ok, _ := secretOf(ctx, "OPENAI_API_KEY"); ok {
		t.Fatal("fail closed expected")
	}
}

func TestStampedOwnHomeScopeKeepsEnvOverlay(t *testing.T) {
	resetMultiplex(t)
	h := newTestHome(t)
	t.Setenv("OPENAI_API_KEY", "sk-own-env")
	ctx := WithSecretScope(context.Background(), map[string]string{}, h)
	if ServesRoutedProfile(ctx) {
		t.Fatal("own home is not routed")
	}
	if v, _, _ := secretOf(ctx, "OPENAI_API_KEY"); v != "sk-own-env" {
		t.Fatalf("got %q", v)
	}
}

func TestNestedScopesRestore(t *testing.T) {
	resetMultiplex(t)
	SetMultiplexActive(true)
	outer := WithSecretScope(context.Background(), map[string]string{"K": "a"}, "")
	inner := WithSecretScope(outer, map[string]string{"K": "b"}, "")
	if v, _, _ := secretOf(inner, "K"); v != "b" {
		t.Fatal("inner scope wins")
	}
	if v, _, _ := secretOf(outer, "K"); v != "a" {
		t.Fatal("outer scope intact")
	}
}

func TestLoadEnvFileUnescapesQuotedValues(t *testing.T) {
	dir := t.TempDir()
	original := `tok"en\with spaces`
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("MY_TOKEN="+config.QuoteEnvValue(original)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := config.LoadEnvFile(filepath.Join(dir, ".env")); got["MY_TOKEN"] != original {
		t.Fatalf("got %q", got["MY_TOKEN"])
	}
}

func TestLoadEnvFileInlineComments(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]map[string]string{
		"PLAIN=abc123\nQUOTED='single quoted'\nEMPTY=\n":                    {"PLAIN": "abc123", "QUOTED": "single quoted", "EMPTY": ""},
		"KEY=value # comment\nTABBED=foo\t#tabbed\n":                        {"KEY": "value", "TABBED": "foo"},
		"KEY=foo#bar\nLEAD=#leading\n":                                      {"KEY": "foo#bar", "LEAD": "#leading"},
		"DQ=\"has # inside\" # trailing\nSQ='single # inside' # trailing\n": {"DQ": "has # inside", "SQ": "single # inside"},
		"KEY=\"a \\\" quote # x\" # trail\n":                                {"KEY": `a " quote # x`},
	}
	for text, want := range cases {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		got := config.LoadEnvFile(filepath.Join(dir, ".env"))
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%q: %s=%q want %q", text, k, got[k], v)
			}
		}
	}
}

func TestStripsUTF8BOMFromFirstKey(t *testing.T) {
	resetMultiplex(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("\xef\xbb\xbfANTHROPIC_API_KEY=sk-x\nOPENAI_API_KEY=sk-y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := config.LoadEnvFile(filepath.Join(dir, ".env"))
	if out["ANTHROPIC_API_KEY"] != "sk-x" || out["OPENAI_API_KEY"] != "sk-y" {
		t.Fatalf("BOM must be stripped: %v", out)
	}
	scope := BuildProfileSecretScope(dir)
	SetMultiplexActive(true)
	ctx := WithSecretScope(context.Background(), scope, "")
	if v, _, _ := secretOf(ctx, "ANTHROPIC_API_KEY"); v != "sk-x" {
		t.Fatal("scope must carry the key")
	}
}

func TestBuildProfileSecretScope(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("ANTHROPIC_API_KEY=sk-profile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope := BuildProfileSecretScope(dir)
	if len(scope) != 1 || scope["ANTHROPIC_API_KEY"] != "sk-profile" {
		t.Fatalf("got %v", scope)
	}
}

func TestBuildProfileSecretScopeIncludesHomeExternalSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("XIAOMI_API_KEY=placeholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretSourceCacheMu.Lock()
	secretValuesByHome[homeKey(dir)] = map[string]string{"XIAOMI_API_KEY": "sk-from-bitwarden"}
	secretSourceCacheMu.Unlock()
	t.Cleanup(func() { ResetSecretSourceCache(dir) })
	if scope := BuildProfileSecretScope(dir); scope["XIAOMI_API_KEY"] != "sk-from-bitwarden" || len(scope) != 1 {
		t.Fatalf("got %v", scope)
	}
}

func TestBuildProfileSecretScopeIgnoresOtherHomeExternalSecrets(t *testing.T) {
	root := t.TempDir()
	profile, other := filepath.Join(root, "profile"), filepath.Join(root, "other")
	_ = os.MkdirAll(profile, 0o700)
	_ = os.MkdirAll(other, 0o700)
	secretSourceCacheMu.Lock()
	secretValuesByHome[homeKey(other)] = map[string]string{"XIAOMI_API_KEY": "sk-other-profile"}
	secretSourceCacheMu.Unlock()
	t.Cleanup(func() { ResetSecretSourceCache(other) })
	if scope := BuildProfileSecretScope(profile); len(scope) != 0 {
		t.Fatalf("got %v", scope)
	}
}

func TestListenerVarsReadEnvironEvenWhenScopedMultiplex(t *testing.T) {
	resetMultiplex(t)
	names := []string{"API_SERVER_ENABLED", "API_SERVER_HOST", "API_SERVER_PORT", "API_SERVER_CORS_ORIGINS"}
	for _, n := range names {
		t.Setenv(n, "container-"+n)
	}
	SetMultiplexActive(true)
	ctx := WithSecretScope(context.Background(), map[string]string{"TELEGRAM_BOT_TOKEN": "scoped"}, "")
	for _, n := range names {
		if v, _, _ := secretOf(ctx, n); v != "container-"+n {
			t.Fatalf("%s must read the environment", n)
		}
	}
}

func TestAPIServerKeyStaysProfileScoped(t *testing.T) {
	resetMultiplex(t)
	t.Setenv("API_SERVER_KEY", "default-profile-key-0123456789abcdef")
	SetMultiplexActive(true)
	ctx := WithSecretScope(context.Background(), map[string]string{"OTHER": "x"}, "")
	if _, ok, _ := secretOf(ctx, "API_SERVER_KEY"); ok {
		t.Fatal("a credential never borrows the environment")
	}
	if IsGlobalEnv("API_SERVER_KEY") {
		t.Fatal("API_SERVER_KEY is a credential")
	}
}

func TestRelayRoutingStampsAndAuthMaterial(t *testing.T) {
	resetMultiplex(t)
	routing := []string{"GATEWAY_RELAY_URL", "GATEWAY_RELAY_ENDPOINT", "GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS", "GATEWAY_RELAY_PLATFORMS", "GATEWAY_RELAY_BOT_IDS",
		"GATEWAY_RELAY_ROUTE_KEYS", "GATEWAY_RELAY_INSTANCE_ID", "GATEWAY_RELAY_WAKE_URL", "GATEWAY_RELAY_DISPLAY_NAME"}
	auth := []string{"GATEWAY_RELAY_SECRET", "GATEWAY_RELAY_ID", "GATEWAY_RELAY_DELIVERY_KEY", "GATEWAY_RELAY_IDP_CLIENT_SECRET", "GATEWAY_RELAY_IDP_CLIENT_ID", "GATEWAY_RELAY_IDP_TOKEN_URL"}
	for _, n := range routing {
		t.Setenv(n, "deploy-"+n)
	}
	for _, n := range auth {
		t.Setenv(n, "cross-profile-credential")
	}
	SetMultiplexActive(true)
	ctx := WithSecretScope(context.Background(), map[string]string{"TELEGRAM_BOT_TOKEN": "scoped"}, "")
	for _, n := range routing {
		if v, _, _ := secretOf(ctx, n); v != "deploy-"+n {
			t.Fatalf("%s must read the environment", n)
		}
	}
	for _, n := range auth {
		if _, ok, _ := secretOf(ctx, n); ok || IsGlobalEnv(n) {
			t.Fatalf("%s must stay profile scoped", n)
		}
	}
}

func TestUnscopedSecretErrorSignature(t *testing.T) {
	err := NewUnscopedSecretError("SURPLUS_API_KEY", "get_secret('SURPLUS_API_KEY') with no scope")
	if err.SecretName != "SURPLUS_API_KEY" || !contains(err.Error(), "SURPLUS_API_KEY") || err.DeveloperDetail == "" {
		t.Fatalf("named secret leads the user sentence: %+v", err)
	}
	legacy := NewUnscopedSecretError("get_secret('X') called with no profile secret scope active.", "")
	if legacy.SecretName != "" || !hasPrefix(legacy.DeveloperDetail, "get_secret('X')") || contains(legacy.Error(), "get_secret") {
		t.Fatalf("legacy positional detail must not be read as a name: %+v", legacy)
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
