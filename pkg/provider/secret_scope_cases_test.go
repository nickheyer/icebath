package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Dotenv file holding text, its parsed assignments compared against want
func loadEnvFileCase(t *testing.T, text string, want map[string]string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := config.LoadEnvFile(path); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestEnvFileParsingLoadEnvFileSingleQuotesAndPlainValues(t *testing.T) {
	loadEnvFileCase(t, "PLAIN=abc123\nQUOTED='single quoted'\nEMPTY=\n", map[string]string{"PLAIN": "abc123", "QUOTED": "single quoted", "EMPTY": ""})
}

func TestEnvFileParsingInlineCommentStrippedFromUnquotedValue(t *testing.T) {
	loadEnvFileCase(t, "KEY=value # comment\nTABBED=foo\t#tabbed\n", map[string]string{"KEY": "value", "TABBED": "foo"})
}

func TestEnvFileParsingHashWithoutPrecedingWhitespaceIsNotAComment(t *testing.T) {
	loadEnvFileCase(t, "KEY=foo#bar\nLEAD=#leading\n", map[string]string{"KEY": "foo#bar", "LEAD": "#leading"})
}

func TestEnvFileParsingInlineCommentAfterQuotedValue(t *testing.T) {
	loadEnvFileCase(t, "DQ=\"has # inside\" # trailing\nSQ='single # inside' # trailing\n", map[string]string{"DQ": "has # inside", "SQ": "single # inside"})
}

func TestEnvFileParsingInlineCommentWithEscapedQuoteInsideValue(t *testing.T) {
	loadEnvFileCase(t, "KEY=\"a \\\" quote # x\" # trail\n", map[string]string{"KEY": "a \" quote # x"})
}

func TestEnvFileParsingRoundTripWriterValueWithTrailingComment(t *testing.T) {
	original := "we#ird \"tok\\en\" # not a comment"
	loadEnvFileCase(t, "MY_TOKEN="+config.QuoteEnvValue(original)+" # rotated 2026-08\n", map[string]string{"MY_TOKEN": original})
}

var relayRoutingVars = []string{
	"GATEWAY_RELAY_URL", "GATEWAY_RELAY_ENDPOINT", "GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS", "GATEWAY_RELAY_PLATFORMS",
	"GATEWAY_RELAY_BOT_IDS", "GATEWAY_RELAY_ROUTE_KEYS", "GATEWAY_RELAY_INSTANCE_ID", "GATEWAY_RELAY_WAKE_URL", "GATEWAY_RELAY_DISPLAY_NAME",
}

var relayAuthVars = []string{
	"GATEWAY_RELAY_SECRET", "GATEWAY_RELAY_ID", "GATEWAY_RELAY_DELIVERY_KEY",
	"GATEWAY_RELAY_IDP_CLIENT_SECRET", "GATEWAY_RELAY_IDP_CLIENT_ID", "GATEWAY_RELAY_IDP_TOKEN_URL",
}

func TestRelayRoutingStampGlobalsRoutingStampsReadEnvironEvenWhenScopedMultiplex(t *testing.T) {
	resetMultiplex(t)
	for _, name := range relayRoutingVars {
		t.Setenv(name, "deploy-"+strings.ToLower(name))
	}
	SetMultiplexActive(true)
	ctx := WithSecretScope(context.Background(), map[string]string{"TELEGRAM_BOT_TOKEN": "scoped"}, "")
	for _, name := range relayRoutingVars {
		if v, ok, err := GetSecret(ctx, name); err != nil || !ok || v != "deploy-"+strings.ToLower(name) {
			t.Fatalf("%s = %q %t %v", name, v, ok, err)
		}
	}
}

func TestRelayRoutingStampGlobalsRelayAuthMaterialStaysProfileScoped(t *testing.T) {
	resetMultiplex(t)
	for _, name := range relayAuthVars {
		t.Setenv(name, "cross-profile-credential")
	}
	SetMultiplexActive(true)
	ctx := WithSecretScope(context.Background(), map[string]string{"OTHER": "x"}, "")
	for _, name := range relayAuthVars {
		if v, ok, err := GetSecret(ctx, name); err != nil || ok || v != "" {
			t.Fatalf("%s borrowed the environ value: %q %t %v", name, v, ok, err)
		}
	}
	SetMultiplexActive(false)
	for _, name := range relayAuthVars {
		if IsGlobalEnv(name) {
			t.Fatalf("%s is treated as a global", name)
		}
	}
}

func TestSecretScopeAcrossExecutorThreadsScopedReadWorksInDaemonPoolWorker(t *testing.T) {
	resetMultiplex(t)
	t.Setenv("SURPLUS_API_KEY", "env-key")
	SetMultiplexActive(true)
	ctx := WithSecretScope(context.Background(), map[string]string{"SURPLUS_API_KEY": "scope-key"}, "")
	pool := tools.NewDaemonPool(1, "secret-scope-test")
	defer pool.Shutdown(true, false)
	read := func(name string) (any, error) {
		future, err := pool.Submit(ctx, func(ctx context.Context) (any, error) {
			v, ok, err := GetSecret(ctx, name)
			if err != nil || !ok {
				return nil, err
			}
			return v, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return future.Result(10 * time.Second)
	}
	if seen, err := read("SURPLUS_API_KEY"); err != nil || seen != "scope-key" {
		t.Fatalf("worker saw %v %v", seen, err)
	}
	t.Setenv("OPENAI_API_KEY", "env-leak")
	if seen, err := read("OPENAI_API_KEY"); err != nil || seen != nil {
		t.Fatalf("a scoped miss borrowed the env in the worker: %v %v", seen, err)
	}
}

func TestStrIsAUserSentenceAndDeveloperDetailRidesTheNotes(t *testing.T) {
	resetMultiplex(t)
	SetMultiplexActive(true)
	_, _, err := GetSecret(context.Background(), "OPENROUTER_API_KEY")
	unscoped, ok := err.(*UnscopedSecretError)
	if !ok {
		t.Fatalf("UnscopedSecretError expected, got %v", err)
	}
	text := unscoped.Error()
	if !strings.Contains(text, "OPENROUTER_API_KEY") {
		t.Fatalf("the sentence does not name the secret: %s", text)
	}
	for _, jargon := range []string{"set_secret_scope", "WithSecretScope", "Workstream", "developer-guide", "os.environ"} {
		if strings.Contains(text, jargon) {
			t.Fatalf("user sentence carries %q: %s", jargon, text)
		}
	}
	if !strings.Contains(unscoped.DeveloperDetail, "WithSecretScope") {
		t.Fatalf("the developer detail does not name the fix: %s", unscoped.DeveloperDetail)
	}
}

func TestRuntimeProviderUsesScopeGetenvTwoProfilesIsolated(t *testing.T) {
	resetMultiplex(t)
	SetMultiplexActive(true)
	ctxA := WithSecretScope(context.Background(), map[string]string{"OPENAI_API_KEY": "sk-A"}, "")
	if got := GetSecretStr(ctxA, "OPENAI_API_KEY", ""); got != "sk-A" {
		t.Fatalf("profile A read %q", got)
	}
	ctxB := WithSecretScope(context.Background(), map[string]string{"OPENAI_API_KEY": "sk-B"}, "")
	if got := GetSecretStr(ctxB, "OPENAI_API_KEY", ""); got != "sk-B" {
		t.Fatalf("profile B read %q", got)
	}
}

// Counts dotenv parses of the memo for the test, the cache cleared before and after
func countScopeEnvParses(t *testing.T) *int {
	t.Helper()
	config.InvalidateEnvFileCache("")
	calls := 0
	var previous func(string) map[string]string
	previous = config.SwapParseEnvText(func(text string) map[string]string {
		calls++
		return previous(text)
	})
	t.Cleanup(func() {
		config.SwapParseEnvText(previous)
		config.InvalidateEnvFileCache("")
	})
	return &calls
}

// Same length, different content, only the mtime distinguishes the versions
func rewriteSameSize(t *testing.T, path, text string) {
	t.Helper()
	previous, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil || current.Size() != previous.Size() {
		t.Fatalf("rewrite changed the size: %v %v", err, current)
	}
	bumped := previous.ModTime().Add(10 * time.Second)
	if err := os.Chtimes(path, bumped, bumped); err != nil {
		t.Fatal(err)
	}
}

func TestUnchangedFileIsParsedOnceAndEachCallerGetsItsOwnDict(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte("A=1\nB=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := countScopeEnvParses(t)
	previousSources := secretSourceValues
	secretSourceValues = func(string) map[string]string { return map[string]string{"EXT": "vault"} }
	t.Cleanup(func() { secretSourceValues = previousSources })
	want := map[string]string{"A": "1", "B": "2"}
	first := config.LoadEnvFile(env)
	first["INJECTED"] = "must-not-persist"
	for range 5 {
		if got := config.LoadEnvFile(env); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v", got)
		}
	}
	if scope := BuildProfileSecretScope(dir); scope["EXT"] != "vault" {
		t.Fatalf("scope %v", scope)
	}
	if got := config.LoadEnvFile(env); !reflect.DeepEqual(got, want) {
		t.Fatalf("layering poisoned the memo: %v", got)
	}
	if *calls != 1 {
		t.Fatalf("expected one parse, got %d", *calls)
	}
}

func TestEditsAndDeletionAreSeen(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(env, []byte("KEY=aaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := countScopeEnvParses(t)
	if got := config.LoadEnvFile(env); got["KEY"] != "aaa" {
		t.Fatalf("got %v", got)
	}
	rewriteSameSize(t, env, "KEY=bbb\n")
	if got := config.LoadEnvFile(env); got["KEY"] != "bbb" || *calls != 2 {
		t.Fatalf("got %v after %d parses", got, *calls)
	}
	if err := os.Remove(env); err != nil {
		t.Fatal(err)
	}
	if got := config.LoadEnvFile(env); len(got) != 0 {
		t.Fatalf("deleted file still reads %v", got)
	}
	if err := os.WriteFile(env, []byte("KEY=ccc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := config.LoadEnvFile(env); got["KEY"] != "ccc" || *calls != 3 {
		t.Fatalf("got %v after %d parses", got, *calls)
	}
}

func TestSameSizeRewriteInsideOneTimestampTickIsSeen(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(env, []byte("TUI_RELOAD_PROBE=one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := countScopeEnvParses(t)
	stamp, err := os.Stat(env)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if got := config.LoadEnvFile(env); got["TUI_RELOAD_PROBE"] != "one" {
			t.Fatalf("got %v", got)
		}
	}
	if *calls != 1 {
		t.Fatalf("unchanged bytes parsed %d times", *calls)
	}
	if err := os.WriteFile(env, []byte("TUI_RELOAD_PROBE=two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(env, stamp.ModTime(), stamp.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := config.LoadEnvFile(env); got["TUI_RELOAD_PROBE"] != "two" || *calls != 2 {
		t.Fatalf("got %v after %d parses", got, *calls)
	}
}
