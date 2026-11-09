package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
)

// Unsets each name for the test and restores its prior state afterwards
func envLoaderUnset(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		prev, had := os.LookupEnv(name)
		_ = os.Unsetenv(name)
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(name, prev)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
}

// Puts the whole process environment back to its contents at call time once the test ends
func restoreProcessEnv(t *testing.T) {
	t.Helper()
	before := os.Environ()
	t.Cleanup(func() {
		keep := map[string]bool{}
		for _, kv := range before {
			k, v, _ := strings.Cut(kv, "=")
			keep[k] = true
			if cur, ok := os.LookupEnv(k); !ok || cur != v {
				_ = os.Setenv(k, v)
			}
		}
		for _, kv := range os.Environ() {
			if k, _, _ := strings.Cut(kv, "="); !keep[k] {
				_ = os.Unsetenv(k)
			}
		}
	})
}

// Fresh publish record and warn once sets, no updater skip and a bridge that does nothing
func envLoaderIsolate(t *testing.T) {
	t.Helper()
	dotenvMu.Lock()
	prevPublished := dotenvPublished
	dotenvPublished = map[string]dotenvRecord{}
	dotenvMu.Unlock()
	envLoaderWarnMu.Lock()
	prevUTF32, prevSkip, prevUnreadable := warnedUTF32Paths, scopedSkipLogged, warnedUnreadablePaths
	warnedUTF32Paths, scopedSkipLogged, warnedUnreadablePaths = map[string]bool{}, map[string]bool{}, map[string]bool{}
	envLoaderWarnMu.Unlock()
	prevSkipFn, prevApply, prevBridge := skipExternalSecretSourcesFn, applyExternalSecretSourcesFn, terminalConfigBridgeFn
	skipExternalSecretSourcesFn = func() bool { return false }
	terminalConfigBridgeFn = func(context.Context) error { return nil }
	t.Cleanup(func() {
		dotenvMu.Lock()
		dotenvPublished = prevPublished
		dotenvMu.Unlock()
		envLoaderWarnMu.Lock()
		warnedUTF32Paths, scopedSkipLogged, warnedUnreadablePaths = prevUTF32, prevSkip, prevUnreadable
		envLoaderWarnMu.Unlock()
		skipExternalSecretSourcesFn, applyExternalSecretSourcesFn, terminalConfigBridgeFn = prevSkipFn, prevApply, prevBridge
	})
}

// Process home under a temp root with no managed scope and no backend selector, returning the root
func envLoaderRoot(t *testing.T) string {
	t.Helper()
	h := newTestHome(t)
	root := filepath.Dir(h)
	t.Setenv(config.EnvManagedDir, filepath.Join(root, "no_such_managed_dir"))
	config.InvalidateManagedCache()
	t.Cleanup(config.InvalidateManagedCache)
	envLoaderUnset(t, config.BackendEnv, "OP_SERVICE_ACCOUNT_TOKEN")
	ResetSecretSourceCache("")
	t.Cleanup(func() { ResetSecretSourceCache("") })
	envLoaderIsolate(t)
	return root
}

// Fresh profile directory named name under root
func envLoaderProfile(t *testing.T, root, name string) string {
	t.Helper()
	h := filepath.Join(root, name)
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	return h
}

func writeEnvBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readEnvBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Captures loader log records at debug and above
func captureEnvLoaderLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := envLoaderLog
	envLoaderLog = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { envLoaderLog = prev })
	return buf
}

func mustLoad(t *testing.T, opts DotenvLoadOptions) []string {
	t.Helper()
	loaded, err := LoadIcebathDotenv(context.Background(), opts)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return loaded
}

func expectEnv(t *testing.T, name, want string) {
	t.Helper()
	got, ok := os.LookupEnv(name)
	if !ok || got != want {
		t.Fatalf("%s = %q (set %v), want %q", name, got, ok, want)
	}
}

func expectUnset(t *testing.T, name string) {
	t.Helper()
	if v, ok := os.LookupEnv(name); ok {
		t.Fatalf("%s must be unset, got %q", name, v)
	}
}

func sameStrings(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func utf16LE(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func utf32LE(s string) []byte {
	out := []byte{0xFF, 0xFE, 0, 0}
	for _, r := range s {
		out = append(out, byte(r), byte(r>>8), byte(r>>16), byte(r>>24))
	}
	return out
}

func TestRecoveredUpdateRetrySkipsExternalSecretSources(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	envFile := filepath.Join(h, ".env")
	writeEnvBytes(t, envFile, []byte("UPDATE_RETRY_DOTENV=loaded\n"))
	envLoaderUnset(t, "UPDATE_RETRY_DOTENV")
	skipExternalSecretSourcesFn = func() bool { return true }
	var calls []string
	applyExternalSecretSourcesFn = func(p string) { calls = append(calls, p) }
	if loaded := mustLoad(t, DotenvLoadOptions{Home: h}); !sameStrings(loaded, envFile) {
		t.Fatalf("loaded %v", loaded)
	}
	expectEnv(t, "UPDATE_RETRY_DOTENV", "loaded")
	if len(calls) != 0 {
		t.Fatalf("external sources ran %v", calls)
	}
}

func TestUTF8BOMDoesNotMangleFirstKey(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	envFile := filepath.Join(h, ".env")
	writeEnvBytes(t, envFile, []byte("\xef\xbb\xbfFIRST_KEY=first-value\nSECOND_KEY=second-value\n"))
	envLoaderUnset(t, "FIRST_KEY", "SECOND_KEY", "\ufeffFIRST_KEY")
	if loaded := mustLoad(t, DotenvLoadOptions{Home: h}); !sameStrings(loaded, envFile) {
		t.Fatalf("loaded %v", loaded)
	}
	expectEnv(t, "FIRST_KEY", "first-value")
	expectEnv(t, "SECOND_KEY", "second-value")
	expectUnset(t, "\ufeffFIRST_KEY")
}

func TestBOMFirstKeyIsSeenByInstallerAndScrubAlike(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	envFile := filepath.Join(h, ".env")
	writeEnvBytes(t, envFile, []byte("\xef\xbb\xbfFIRST_KEY=first-value\n"+
		"export EXPORTED_KEY='quoted # not a comment'\n"+
		"COMMENTED_KEY=value # trailing comment\n"+
		"EMPTY_KEY=\n"))
	envLoaderUnset(t, "FIRST_KEY", "EXPORTED_KEY", "COMMENTED_KEY", "EMPTY_KEY", "\ufeffFIRST_KEY")
	mustLoad(t, DotenvLoadOptions{Home: h})
	installed := map[string]bool{}
	for _, k := range []string{"FIRST_KEY", "EXPORTED_KEY", "COMMENTED_KEY", "EMPTY_KEY"} {
		if _, ok := os.LookupEnv(k); ok {
			installed[k] = true
		}
	}
	scoped := config.LoadEnvFile(envFile)
	defined := envKeysDefinedInDotenv(envFile)
	if len(defined) != len(installed) || len(scoped) != len(installed) {
		t.Fatalf("defined %v installed %v scoped %v", defined, installed, scoped)
	}
	for k := range installed {
		if _, ok := scoped[k]; !ok || !defined[k] {
			t.Fatalf("key %s split between installer and scrub", k)
		}
	}
	if defined["\ufeffFIRST_KEY"] {
		t.Fatal("BOM leaked onto the first key")
	}
	if scoped["EXPORTED_KEY"] != os.Getenv("EXPORTED_KEY") || os.Getenv("EXPORTED_KEY") != "quoted # not a comment" {
		t.Fatalf("exported %q %q", scoped["EXPORTED_KEY"], os.Getenv("EXPORTED_KEY"))
	}
	if scoped["COMMENTED_KEY"] != os.Getenv("COMMENTED_KEY") || os.Getenv("COMMENTED_KEY") != "value" {
		t.Fatalf("commented %q %q", scoped["COMMENTED_KEY"], os.Getenv("COMMENTED_KEY"))
	}
}

func TestUTF8BOMPlusInvalidUTF8PreservesFirstKey(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	envFile := filepath.Join(h, ".env")
	writeEnvBytes(t, envFile, []byte("\xef\xbb\xbfANTHROPIC_API_KEY=sk-test-123\nBAD=caf\xe9\n"))
	envLoaderUnset(t, "ANTHROPIC_API_KEY", "BAD", "\ufeffANTHROPIC_API_KEY")
	if loaded := mustLoad(t, DotenvLoadOptions{Home: h}); !sameStrings(loaded, envFile) {
		t.Fatalf("loaded %v", loaded)
	}
	expectEnv(t, "ANTHROPIC_API_KEY", "sk-test-123")
	expectEnv(t, "BAD", "café")
	expectUnset(t, "\ufeffANTHROPIC_API_KEY")
}

func TestLatin1FallbackStreamHonorsOverride(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	envFile := filepath.Join(h, ".env")
	writeEnvBytes(t, envFile, []byte("OVERRIDE_PROBE=from-file\nLATIN1_VALUE=caf\xe9\n"))
	t.Setenv("OVERRIDE_PROBE", "from-shell")
	envLoaderUnset(t, "LATIN1_VALUE")
	if err := LoadDotenvFile(envFile, false); err != nil {
		t.Fatal(err)
	}
	expectEnv(t, "OVERRIDE_PROBE", "from-shell")
	expectEnv(t, "LATIN1_VALUE", "café")
	if err := LoadDotenvFile(envFile, true); err != nil {
		t.Fatal(err)
	}
	expectEnv(t, "OVERRIDE_PROBE", "from-file")
	expectEnv(t, "LATIN1_VALUE", "café")
}

func TestLatin1FallbackStreamPreservesInterpolation(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	envFile := filepath.Join(h, ".env")
	writeEnvBytes(t, envFile, []byte("FOO=bar\nBAR=${FOO}\nLATIN1_VALUE=caf\xe9\n"))
	envLoaderUnset(t, "FOO", "BAR", "LATIN1_VALUE")
	if loaded := mustLoad(t, DotenvLoadOptions{Home: h}); !sameStrings(loaded, envFile) {
		t.Fatalf("loaded %v", loaded)
	}
	expectEnv(t, "FOO", "bar")
	expectEnv(t, "BAR", "bar")
	expectEnv(t, "LATIN1_VALUE", "café")
}

func TestUTF16LEBOMPreservesNonASCIIValues(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	envFile := filepath.Join(h, ".env")
	writeEnvBytes(t, envFile, utf16LE("GREETING=café\nCJK_LABEL=日本語\n"))
	envLoaderUnset(t, "GREETING", "CJK_LABEL")
	if loaded := mustLoad(t, DotenvLoadOptions{Home: h}); !sameStrings(loaded, envFile) {
		t.Fatalf("loaded %v", loaded)
	}
	expectEnv(t, "GREETING", "café")
	expectEnv(t, "CJK_LABEL", "日本語")
	after := readEnvBytes(t, envFile)
	if !utf8.Valid(after) || len(after) == 0 {
		t.Fatalf("not clean UTF-8 %q", after)
	}
	if !bytes.Contains(after, []byte("café")) || !bytes.Contains(after, []byte("日本語")) || bytes.Contains(after, []byte("\xef\xbf\xbd")) {
		t.Fatalf("rewrite lost text %q", after)
	}
}

func TestUTF32LEBOMLeavesFileUntouched(t *testing.T) {
	envLoaderIsolate(t)
	logs := captureEnvLoaderLog(t)
	envFile := filepath.Join(t.TempDir(), ".env")
	raw := utf32LE("ICEBATH_TEST_KEY=hello_utf32\nSECOND_KEY=world\n")
	writeEnvBytes(t, envFile, raw)
	sanitizeEnvFileIfNeeded(envFile)
	if !bytes.Equal(readEnvBytes(t, envFile), raw) {
		t.Fatal("UTF-32 file was rewritten")
	}
	if !strings.Contains(logs.String(), "UTF-32") {
		t.Fatalf("no UTF-32 warning in %q", logs.String())
	}
}

func TestUTF32WarningFiresOncePerPath(t *testing.T) {
	envLoaderIsolate(t)
	logs := captureEnvLoaderLog(t)
	envFile := filepath.Join(t.TempDir(), ".env")
	raw := utf32LE("ICEBATH_TEST_KEY=hello_utf32\nSECOND_KEY=world\n")
	writeEnvBytes(t, envFile, raw)
	for range 3 {
		sanitizeEnvFileIfNeeded(envFile)
	}
	if n := strings.Count(logs.String(), "UTF-32"); n != 1 {
		t.Fatalf("%d UTF-32 warnings", n)
	}
	if !bytes.Equal(readEnvBytes(t, envFile), raw) {
		t.Fatal("UTF-32 file was rewritten")
	}
}

func TestPlainUTF8EnvRegression(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	envFile := filepath.Join(h, ".env")
	before := []byte("OPENAI_API_KEY=sk-plain\nSECOND_KEY=ok\n")
	writeEnvBytes(t, envFile, before)
	envLoaderUnset(t, "OPENAI_API_KEY", "SECOND_KEY")
	if loaded := mustLoad(t, DotenvLoadOptions{Home: h}); !sameStrings(loaded, envFile) {
		t.Fatalf("loaded %v", loaded)
	}
	expectEnv(t, "OPENAI_API_KEY", "sk-plain")
	expectEnv(t, "SECOND_KEY", "ok")
	if !bytes.Equal(readEnvBytes(t, envFile), before) {
		t.Fatal("plain file was rewritten")
	}
}

func TestCP1252EnvRegressionDoesNotCrash(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	envFile := filepath.Join(h, ".env")
	before := []byte("ASCII_KEY=ok\nLATIN1_VALUE=caf\xe9\n")
	writeEnvBytes(t, envFile, before)
	envLoaderUnset(t, "ASCII_KEY", "LATIN1_VALUE")
	if loaded := mustLoad(t, DotenvLoadOptions{Home: h}); !sameStrings(loaded, envFile) {
		t.Fatalf("loaded %v", loaded)
	}
	expectEnv(t, "ASCII_KEY", "ok")
	expectEnv(t, "LATIN1_VALUE", "café")
	if !bytes.Equal(readEnvBytes(t, envFile), before) {
		t.Fatal("cp1252 file was rewritten")
	}
}

func TestKnownKeysAbsentFromUserEnvAreCleared(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("OPENAI_BASE_URL=https://profile.example/v1\n"))
	t.Setenv("OPENAI_BASE_URL", "https://stale.example/v1")
	t.Setenv("ICEBATH_ACP_AUTH_METHOD", "cursor_login")
	t.Setenv("COPILOT_CLI_PATH", "/usr/bin/claude-code")
	t.Setenv("MY_SHELL_ONLY_VAR", "keep-me")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "OPENAI_BASE_URL", "https://profile.example/v1")
	expectUnset(t, "ICEBATH_ACP_AUTH_METHOD")
	expectUnset(t, "COPILOT_CLI_PATH")
	expectEnv(t, "MY_SHELL_ONLY_VAR", "keep-me")
}

func TestEmptyAssignmentInUserEnvIsPreserved(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("ICEBATH_ACP_AUTH_METHOD=\n"))
	t.Setenv("ICEBATH_ACP_AUTH_METHOD", "cursor_login")
	t.Setenv("COPILOT_CLI_PATH", "/usr/bin/sneaky")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "ICEBATH_ACP_AUTH_METHOD", "")
	expectUnset(t, "COPILOT_CLI_PATH")
}

func TestNoUserEnvDoesNotClearAnything(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	t.Setenv("ICEBATH_ACP_AUTH_METHOD", "cursor_login")
	t.Setenv("PATH", "/usr/bin:/bin")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "ICEBATH_ACP_AUTH_METHOD", "cursor_login")
	expectEnv(t, "PATH", "/usr/bin:/bin")
}

// Inherited routing key survives the scrub when the profile .env defines it as line
func knownKeyDefinedInUserEnvIsKept(t *testing.T, line string) {
	t.Helper()
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte(line))
	t.Setenv("ICEBATH_ACP_AUTH_METHOD", "cursor_login")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "ICEBATH_ACP_AUTH_METHOD", "claude_code_cli")
}

func TestKnownKeyExplicitlySetInUserEnvIsKept(t *testing.T) {
	knownKeyDefinedInUserEnvIsKept(t, "ICEBATH_ACP_AUTH_METHOD=claude_code_cli\n")
}

func TestExportPrefixedKnownKeyInUserEnvIsKept(t *testing.T) {
	knownKeyDefinedInUserEnvIsKept(t, "export ICEBATH_ACP_AUTH_METHOD=claude_code_cli\n")
}

func TestShellExportedCredentialsSurviveCleanup(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("SOME_OTHER_KEY=x\n"))
	envLoaderUnset(t, "SOME_OTHER_KEY")
	t.Setenv("OPENAI_API_KEY", "sk-from-shell")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-from-shell")
	t.Setenv("TELEGRAM_BOT_TOKEN", "12345:token-from-shell")
	t.Setenv("ICEBATH_ACP_AUTH_METHOD", "cursor_login")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "OPENAI_API_KEY", "sk-from-shell")
	expectEnv(t, "ANTHROPIC_API_KEY", "sk-ant-from-shell")
	expectEnv(t, "TELEGRAM_BOT_TOKEN", "12345:token-from-shell")
	expectUnset(t, "ICEBATH_ACP_AUTH_METHOD")
}

func TestCleanupScopeIsTheProfileManagedSet(t *testing.T) {
	for _, key := range profileManagedEnvKeys() {
		for _, suffix := range []string{"_API_KEY", "_TOKEN", "_SECRET"} {
			if strings.HasSuffix(key, suffix) {
				t.Fatalf("%s is credential shaped and must never be scrubbed at startup", key)
			}
		}
	}
}

// Home with optional config.yaml and .env set as the process home, the real terminal bridge restored
func seedTerminalHome(t *testing.T, configYAML, envText string) string {
	t.Helper()
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	if configYAML != "" {
		writeEnvBytes(t, filepath.Join(h, "config.yaml"), []byte(configYAML))
	}
	if envText != "" {
		writeEnvBytes(t, filepath.Join(h, ".env"), []byte(envText))
	}
	t.Setenv("ICEBATH_HOME", h)
	envLoaderUnsetTerminalBridge(t)
	terminalConfigBridgeFn = applyTerminalConfigToEnv
	config.ResetCache()
	return h
}

// Unsets every env var the terminal config bridge writes and restores each after the test
func envLoaderUnsetTerminalBridge(t *testing.T) {
	t.Helper()
	names := []string{"TERMINAL_DOCKER_IMAGE_PINNED"}
	for _, pair := range config.TerminalConfigEnvMap {
		names = append(names, pair[1])
	}
	envLoaderUnset(t, names...)
}

func TestConfigYAMLTerminalBackendOverridesStaleEnv(t *testing.T) {
	h := seedTerminalHome(t, "terminal:\n  backend: local\n", "TERMINAL_ENV=docker\n")
	envLoaderUnset(t, "TERMINAL_ENV")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "TERMINAL_ENV", "local")
}

func TestConfigYAMLTerminalBackendOverridesStaleShell(t *testing.T) {
	h := seedTerminalHome(t, "terminal:\n  backend: local\n", "")
	t.Setenv("TERMINAL_ENV", "docker")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "TERMINAL_ENV", "local")
}

func TestNoTerminalSectionLeavesEnvValueAlone(t *testing.T) {
	h := seedTerminalHome(t, "display:\n  streaming: true\n", "TERMINAL_ENV=docker\n")
	envLoaderUnset(t, "TERMINAL_ENV")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "TERMINAL_ENV", "docker")
}

func TestConfigYAMLTerminalOmittedKeyDoesNotClearEnv(t *testing.T) {
	h := seedTerminalHome(t, "terminal:\n  timeout: 600\n", "TERMINAL_ENV=docker\n")
	envLoaderUnset(t, "TERMINAL_ENV", "TERMINAL_TIMEOUT")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "TERMINAL_ENV", "docker")
	expectEnv(t, "TERMINAL_TIMEOUT", "600")
}

func TestOtherProfileHomeDoesNotBridgeProcessConfig(t *testing.T) {
	root := envLoaderRoot(t)
	processHome := envLoaderProfile(t, root, "process-home")
	writeEnvBytes(t, filepath.Join(processHome, "config.yaml"), []byte("terminal:\n  backend: local\n"))
	t.Setenv("ICEBATH_HOME", processHome)
	other := envLoaderProfile(t, root, "other-profile")
	writeEnvBytes(t, filepath.Join(other, ".env"), []byte("TERMINAL_ENV=docker\n"))
	envLoaderUnset(t, "TERMINAL_ENV")
	terminalConfigBridgeFn = func(context.Context) error {
		t.Error("the process config was bridged for another profile home")
		return nil
	}
	mustLoad(t, DotenvLoadOptions{Home: other})
	expectEnv(t, "TERMINAL_ENV", "docker")
}

func TestParentInjectedDashboardSessionTokenSurvivesDotenv(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("ICEBATH_DASHBOARD_SESSION_TOKEN=persisted-token\nICEBATH_DASHBOARD_PUBLIC_URL=http://127.0.0.1:1\n"))
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "link-token")
	t.Setenv("ICEBATH_DASHBOARD_PUBLIC_URL", "http://127.0.0.1:43123")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "ICEBATH_DASHBOARD_SESSION_TOKEN", "link-token")
	expectEnv(t, "ICEBATH_DASHBOARD_PUBLIC_URL", "http://127.0.0.1:1")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "ICEBATH_DASHBOARD_SESSION_TOKEN", "link-token")
}

func TestDotenvPublishedDashboardSessionTokenStillReloads(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "hermes")
	envLoaderUnset(t, "ICEBATH_DASHBOARD_SESSION_TOKEN")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("ICEBATH_DASHBOARD_SESSION_TOKEN=first\n"))
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "ICEBATH_DASHBOARD_SESSION_TOKEN", "first")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("ICEBATH_DASHBOARD_SESSION_TOKEN=second\n"))
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "ICEBATH_DASHBOARD_SESSION_TOKEN", "second")
}

const dotenvBasePath = "/usr/bin:/bin"

// Profile home with the boot PATH pinned and multiplexing off
func selfReferenceHome(t *testing.T) (string, string) {
	t.Helper()
	root := envLoaderRoot(t)
	t.Setenv("PATH", dotenvBasePath)
	t.Setenv("ICEBATH_MULTIPLEX_PROFILES", "0")
	return root, envLoaderProfile(t, root, "hermes")
}

func TestDotenvSameFileReloadedThreeTimesLeavesPathUnchanged(t *testing.T) {
	_, h := selfReferenceHome(t)
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("PATH=/x:${PATH}\nPREV=${PATH}\nCHAIN=/c:${PREV}\n"))
	envLoaderUnset(t, "PREV", "CHAIN")
	for i := range 3 {
		mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
		if os.Getenv("PATH") != "/x:"+dotenvBasePath || os.Getenv("CHAIN") != "/c:/x:"+dotenvBasePath {
			t.Fatalf("load %d: PATH %q CHAIN %q", i, os.Getenv("PATH"), os.Getenv("CHAIN"))
		}
	}
}

func TestDotenvAlternatingProjectEnvAndNoneStaysStable(t *testing.T) {
	root, h := selfReferenceHome(t)
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("PATH=/x:${PATH}\n"))
	project := filepath.Join(root, "project.env")
	writeEnvBytes(t, project, []byte("UNRELATED=1\n"))
	envLoaderUnset(t, "UNRELATED")
	for i := range 6 {
		opts := DotenvLoadOptions{Home: h, SkipExternalSecrets: true}
		if i%2 == 1 {
			opts.ProjectEnv = project
		}
		mustLoad(t, opts)
		if n := strings.Count(os.Getenv("PATH"), "/x:"); n != 1 {
			t.Fatalf("load %d: PATH %q", i, os.Getenv("PATH"))
		}
	}
}

func TestDotenvAlternatingHomesEachResolveAgainstBootPath(t *testing.T) {
	root, _ := selfReferenceHome(t)
	homeA, homeB := envLoaderProfile(t, root, "a"), envLoaderProfile(t, root, "b")
	writeEnvBytes(t, filepath.Join(homeA, ".env"), []byte("PATH=/a:${PATH}\n"))
	writeEnvBytes(t, filepath.Join(homeB, ".env"), []byte("PATH=/b:${PATH}\n"))
	var seen []string
	for _, h := range []string{homeA, homeB, homeA, homeB, homeA} {
		mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
		seen = append(seen, os.Getenv("PATH"))
	}
	a, b := "/a:"+dotenvBasePath, "/b:"+dotenvBasePath
	if !sameStrings(seen, a, b, a, b, a) {
		t.Fatalf("seen %v", seen)
	}
}

func TestDotenvVariableUnsetAtBootAndDefaultSyntaxResolveOnce(t *testing.T) {
	_, h := selfReferenceHome(t)
	envLoaderUnset(t, "FOO", "DEF")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("FOO=/f:${FOO}\nDEF=/d:${DEF:-seed}\n"))
	for range 3 {
		mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
	}
	expectEnv(t, "FOO", "/f:")
	expectEnv(t, "DEF", "/d:seed")
}

func TestDotenvValueChangedOutsideTheLoaderIsNotFrozen(t *testing.T) {
	_, h := selfReferenceHome(t)
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("PATH=/x:${PATH}\n"))
	mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
	_ = os.Setenv("PATH", "/opt/new:/bin")
	mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
	mustLoad(t, DotenvLoadOptions{Home: h, SkipExternalSecrets: true})
	expectEnv(t, "PATH", "/x:/opt/new:/bin")
}

func TestDotenvLaterLayerInTheSameLoadStillSeesTheEarlierLayer(t *testing.T) {
	root, h := selfReferenceHome(t)
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("XPATH=/x:${PATH}\n"))
	project := filepath.Join(root, "project.env")
	writeEnvBytes(t, project, []byte("YPATH=/p:${XPATH}\n"))
	envLoaderUnset(t, "XPATH", "YPATH")
	for range 3 {
		mustLoad(t, DotenvLoadOptions{Home: h, ProjectEnv: project, SkipExternalSecrets: true})
	}
	expectEnv(t, "XPATH", "/x:"+dotenvBasePath)
	expectEnv(t, "YPATH", "/p:/x:"+dotenvBasePath)
}

// Managed scope directory for the test, optionally pinning terminal.backend
func envLoaderManagedDir(t *testing.T, root, managedYAML string) string {
	t.Helper()
	md := filepath.Join(root, "managed")
	if err := os.MkdirAll(md, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvManagedDir, md)
	if managedYAML != "" {
		writeEnvBytes(t, filepath.Join(md, "config.yaml"), []byte(managedYAML))
	}
	config.InvalidateManagedCache()
	return md
}

// Profile home whose .env this process may not read
func unreadableProfile(t *testing.T, root string) (string, string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Fatal("read denial through file modes needs a non root user")
	}
	h := envLoaderProfile(t, root, "hermes")
	envFile := filepath.Join(h, ".env")
	writeEnvBytes(t, envFile, []byte("OPENAI_API_KEY=from-file\n"))
	if err := os.Chmod(envFile, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(envFile, 0o600) })
	return h, envFile
}

func TestEnvLoaderUnreadableUserEnvLoadsAsAbsentUnderAPinnedBackend(t *testing.T) {
	root := envLoaderRoot(t)
	envLoaderManagedDir(t, root, "terminal:\n  backend: busybox\n")
	logs := captureEnvLoaderLog(t)
	h, envFile := unreadableProfile(t, root)
	t.Setenv("OPENAI_API_KEY", "from-launcher")
	t.Setenv("ICEBATH_ACP_AUTH_METHOD", "from-launcher")
	if !DotenvUnreadable(envFile) {
		t.Fatal("the denied .env must read as unreadable")
	}
	for range 3 {
		if loaded := mustLoad(t, DotenvLoadOptions{Home: h}); len(loaded) != 0 {
			t.Fatalf("loaded %v", loaded)
		}
	}
	expectEnv(t, "OPENAI_API_KEY", "from-launcher")
	expectEnv(t, "ICEBATH_ACP_AUTH_METHOD", "from-launcher")
	if n := strings.Count(logs.String(), envFile); n != 1 {
		t.Fatalf("%d warnings naming the file in %q", n, logs.String())
	}
}

func TestEnvLoaderUnreadableUserEnvRaisesWithoutAPinnedBackend(t *testing.T) {
	for _, managedYAML := range []string{"", "terminal:\n  timeout: 5\n", "terminal:\n  backend:\n", "terminal:\n  backend: ''\n", "terminal:\n  backend: '   '\n"} {
		t.Run(strings.TrimSpace(managedYAML), func(t *testing.T) {
			root := envLoaderRoot(t)
			envLoaderManagedDir(t, root, managedYAML)
			h, _ := unreadableProfile(t, root)
			_, err := LoadIcebathDotenv(context.Background(), DotenvLoadOptions{Home: h})
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("want a permission error, got %v", err)
			}
		})
	}
}

func TestEnvLoaderUnreadableProjectEnvDoesNotReplaceLauncherKeys(t *testing.T) {
	root := envLoaderRoot(t)
	envLoaderManagedDir(t, root, "terminal:\n  backend: busybox\n")
	h, _ := unreadableProfile(t, root)
	project := filepath.Join(root, "project.env")
	writeEnvBytes(t, project, []byte("OPENAI_API_KEY=from-project\nLAB_PROJECT_ONLY=from-project\n"))
	t.Setenv("OPENAI_API_KEY", "from-launcher")
	envLoaderUnset(t, "LAB_PROJECT_ONLY")
	if loaded := mustLoad(t, DotenvLoadOptions{Home: h, ProjectEnv: project}); !sameStrings(loaded, project) {
		t.Fatalf("loaded %v", loaded)
	}
	expectEnv(t, "OPENAI_API_KEY", "from-launcher")
	expectEnv(t, "LAB_PROJECT_ONLY", "from-project")
}

func TestEnvLoaderDoesNotSplitConcatenatedText(t *testing.T) {
	envLoaderIsolate(t)
	token := "0123456789:test"
	corrupted := "TELEGRAM_BOT_TOKEN=" + token + "ANTHROPIC_API_KEY=sk-ant-test\n"
	envFile := filepath.Join(t.TempDir(), "x.env")
	writeEnvBytes(t, envFile, []byte(corrupted))
	sanitizeEnvFileIfNeeded(envFile)
	lines := universalReadlines(string(readEnvBytes(t, envFile)))
	if !sameStrings(lines, corrupted) {
		t.Fatalf("lines %q", lines)
	}
	if _, value, _ := strings.Cut(strings.TrimSpace(lines[0]), "="); value != token+"ANTHROPIC_API_KEY=sk-ant-test" {
		t.Fatalf("token %q", value)
	}
}

func TestEnvLoaderManagedEnvBeatsUserEnv(t *testing.T) {
	root := envLoaderRoot(t)
	md := envLoaderManagedDir(t, root, "")
	h := envLoaderProfile(t, root, "home")
	envLoaderUnset(t, "OPENAI_API_BASE")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("OPENAI_API_BASE=https://user.example/v1\n"))
	writeEnvBytes(t, filepath.Join(md, ".env"), []byte("OPENAI_API_BASE=https://org.example/v1\n"))
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "OPENAI_API_BASE", "https://org.example/v1")
	found := false
	for _, k := range ManagedDotenvKeys() {
		found = found || k == "OPENAI_API_BASE"
	}
	if !found {
		t.Fatal("the managed .env names must be recorded as policy")
	}
}

func TestEnvLoaderNoManagedEnvIsNoop(t *testing.T) {
	root := envLoaderRoot(t)
	envLoaderManagedDir(t, root, "")
	h := envLoaderProfile(t, root, "home")
	t.Setenv("SOME_VALUE", "from_shell")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("SOME_VALUE=from_user\n"))
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "SOME_VALUE", "from_user")
}

func TestEnvLoaderUserEnvOverridesShell(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, "home")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("FOO_TOKEN=from_user_env\n"))
	t.Setenv("FOO_TOKEN", "from_shell")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "FOO_TOKEN", "from_user_env")
}

func TestEnvLoaderOpEnvAutoloadsBootstrapTokenInCronContext(t *testing.T) {
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, ".icebath")
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte("FOO=bar\n"))
	writeEnvBytes(t, filepath.Join(h, ".op.env"), []byte("OP_SERVICE_ACCOUNT_TOKEN=test-token\n"))
	envLoaderUnset(t, "FOO")
	expectUnset(t, "OP_SERVICE_ACCOUNT_TOKEN")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "OP_SERVICE_ACCOUNT_TOKEN", "test-token")
}

func TestBootstrapEnvDotenvSelector(t *testing.T) {
	cases := []struct {
		shell    *string
		dotenv   string
		expected string
	}{
		{ptrTo("remote"), "ICEBATH_CONFIG_BACKEND=file\n", "file"},
		{ptrTo("remote"), "ICEBATH_CONFIG_BACKEND=\n", "file"},
		{nil, "ICEBATH_CONFIG_BACKEND=remote\n", "remote"},
		{ptrTo("remote"), "OTHER=1\n", "remote"},
	}
	for _, tc := range cases {
		t.Run(strings.TrimSpace(tc.dotenv), func(t *testing.T) {
			envLoaderRoot(t)
			h := os.Getenv("ICEBATH_HOME")
			envLoaderUnset(t, "ICEBATH_CONFIG_INSTANCE_ID", "OTHER")
			if tc.shell != nil {
				t.Setenv(config.BackendEnv, *tc.shell)
			}
			writeEnvBytes(t, filepath.Join(h, ".env"), []byte(tc.dotenv))
			if got := SelectedConfigBackendName(h); got != tc.expected {
				t.Fatalf("selected %q", got)
			}
			if err := config.BootstrapDeployment(context.Background(), h, ""); err != nil {
				t.Fatal(err)
			}
			selector := strings.ToLower(strings.TrimSpace(os.Getenv(config.BackendEnv)))
			if selector == "" {
				selector = "file"
			}
			if selector != tc.expected {
				t.Fatalf("published selector %q", os.Getenv(config.BackendEnv))
			}
			if tc.expected == "file" {
				backend, err := config.GetConfigBackend(context.Background())
				if err != nil || backend.Name() != "file" {
					t.Fatalf("backend %v %v", backend, err)
				}
			}
		})
	}
}

func ptrTo[T any](v T) *T { return &v }

// Bulk source named fakebulk supplying values with the given override default
func registerFakeBulkSource(t *testing.T, values map[string]string, overrideExisting bool) {
	t.Helper()
	cleanRegistry(t)
	src := makeSource("fakebulk", "bulk", values)
	src.spec.OverrideExistingDefault = overrideExisting
	if !RegisterSecretSource(src, true, false, "") {
		t.Fatal("fake source refused")
	}
}

// Process home whose config enables fakebulk and whose .env holds envText
func reloadRestoreHome(t *testing.T, envText, preserve string) string {
	t.Helper()
	root := envLoaderRoot(t)
	h := envLoaderProfile(t, root, ".icebath")
	secrets := "secrets:\n  fakebulk:\n    enabled: true\n"
	if preserve != "" {
		secrets += "  preserve_existing: [" + preserve + "]\n"
	}
	writeEnvBytes(t, filepath.Join(h, "config.yaml"), []byte(secrets))
	writeEnvBytes(t, filepath.Join(h, ".env"), []byte(envText))
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	envLoaderUnset(t, "GLM_API_KEY")
	return h
}

func TestSourceSecretSurvivesSecondLoadHermesDotenv(t *testing.T) {
	h := reloadRestoreHome(t, "GLM_API_KEY=__BITWARDEN_MANAGED__\n", "")
	registerFakeBulkSource(t, map[string]string{"GLM_API_KEY": "vault-value"}, true)
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "GLM_API_KEY", "vault-value")
	mustLoad(t, DotenvLoadOptions{Home: h})
	expectEnv(t, "GLM_API_KEY", "vault-value")
}

func TestReloadRestoreKeepsDotenvPrecedence(t *testing.T) {
	for _, tc := range []struct {
		override bool
		preserve string
	}{{false, ""}, {true, "GLM_API_KEY"}} {
		t.Run(tc.preserve, func(t *testing.T) {
			h := reloadRestoreHome(t, "", tc.preserve)
			registerFakeBulkSource(t, map[string]string{"GLM_API_KEY": "vault-value"}, tc.override)
			mustLoad(t, DotenvLoadOptions{Home: h})
			expectEnv(t, "GLM_API_KEY", "vault-value")
			writeEnvBytes(t, filepath.Join(h, ".env"), []byte("GLM_API_KEY=local\n"))
			mustLoad(t, DotenvLoadOptions{Home: h})
			expectEnv(t, "GLM_API_KEY", "local")
		})
	}
}

// Reference output of python-dotenv 1.2.4 DotEnv.parse and parse_variables for the same inputs
const pythonDotenvVectors = `{"parse": [["A=1\nB = two \nexport C=three # c\n", [["A", "1"], ["B", "two"], ["C", "three"]]], ["Q='single \\' quote' # x\nD=\"dq \\n esc \\t \\\\ \\x \\$\"\n", [["Q", "single ' quote"], ["D", "dq \n esc \t \\ \\x \\$"]]], ["M=\"multi\nline\"\nN='also\r\nmulti'\n", [["M", "multi\nline"], ["N", "also\r\nmulti"]]], ["BARE\nE=\nF= #comment\nG=val#nospace\nH=a \t# tab comment\n", [["BARE", null], ["E", ""], ["F", ""], ["G", "val#nospace"], ["H", "a"]]], ["=bad\nOK=1\n'quoted key'=v\nI=\"unterminated\nJ=2\n", [["OK", "1"], ["quoted key", "v"], ["J", "2"]]], ["K=\"x\" junk\nL=after\r\nCR=1\rLAST=end", [["L", "after"], ["CR", "1"], ["LAST", "end"]]], ["  # lead comment\n\n\texport   SP=v\u00a0\u3000\nU=a\u2003#c\n", [["SP", "v"], ["U", "a"]]], ["\ufeffBOM=1\nexport\nexport=7\n", [["BOM", "1"], ["export", null], ["export", "7"]]]], "expand": [["${A}", "1"], ["${A:-d}", "1"], ["${Z:-d}", "d"], ["${Z}", ""], ["${N:-d}", ""], ["$A ${A", "$A ${A"], ["${A:B}", "${A:B}"], ["x${A}y${Z:-}z", "x1yz"], ["${:-e}", "e"]]}`

func TestDotenvParserMatchesPythonDotenv(t *testing.T) {
	var vectors struct {
		Parse  [][2]json.RawMessage `json:"parse"`
		Expand [][2]string          `json:"expand"`
	}
	if err := json.Unmarshal([]byte(pythonDotenvVectors), &vectors); err != nil {
		t.Fatal(err)
	}
	logs := captureEnvLoaderLog(t)
	for _, pair := range vectors.Parse {
		var text string
		var want [][2]*string
		if err := json.Unmarshal(pair[0], &text); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(pair[1], &want); err != nil {
			t.Fatal(err)
		}
		got := parseDotenvText(text)
		if len(got) != len(want) {
			t.Fatalf("%q parsed %v want %d bindings", text, got, len(want))
		}
		for i, w := range want {
			if got[i].name != *w[0] || got[i].set != (w[1] != nil) || (w[1] != nil && got[i].value != *w[1]) {
				t.Fatalf("%q binding %d got %+v", text, i, got[i])
			}
		}
	}
	for _, line := range []string{"line 1", "line 4"} {
		if !strings.Contains(logs.String(), "python-dotenv could not parse statement starting at "+line) {
			t.Fatalf("missing warning for %s in %q", line, logs.String())
		}
	}
	if n := strings.Count(logs.String(), "could not parse"); n != 3 {
		t.Fatalf("%d parse warnings", n)
	}
	env := map[string]*string{"A": ptrTo("1"), "N": nil}
	lookup := func(name string) (string, bool) {
		v, ok := env[name]
		if !ok {
			return "", false
		}
		if v == nil {
			return "", true
		}
		return *v, true
	}
	for _, pair := range vectors.Expand {
		if got := expandDotenvVariables(pair[0], lookup); got != pair[1] {
			t.Fatalf("%q expanded to %q want %q", pair[0], got, pair[1])
		}
	}
}
