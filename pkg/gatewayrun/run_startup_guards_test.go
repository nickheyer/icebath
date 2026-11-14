package gatewayrun

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Captures records of the default logger at level and above for the test
func captureLevel(t *testing.T, level slog.Level) *logBuffer {
	buf := &logBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// Child process body of the stack dump case, installed handlers then parked until killed
func faulthandlerChild(dir string) {
	os.Setenv("ICEBATH_HOME", dir)
	allocGatewayRunner().startInstallFaulthandler(context.Background())
	fmt.Println("ready")
	time.Sleep(30 * time.Second)
}

func TestAgentBudgetLineMatchesEnforcedLimit(t *testing.T) {
	for _, tc := range []struct {
		maxTurns map[string]any
		expected string
	}{
		{map[string]any{"max_turns": int64(50)}, "50"},
		{map[string]any{}, "90"},
		{map[string]any{"max_turns": "unlimited"}, "unlimited"},
	} {
		t.Run(tc.expected, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			t.Setenv("ICEBATH_MAX_ITERATIONS", "90")
			gateway.BridgeMaxTurnsToEnv(tc.maxTurns)
			enforced := gateway.CurrentMaxIterations(context.Background())
			logs := captureLevel(t, slog.LevelInfo)
			logAgentBudget()
			var line string
			for _, l := range strings.Split(logs.String(), "\n") {
				if strings.Contains(l, "Agent budget") {
					line = l
					break
				}
			}
			if line == "" {
				t.Fatal(logs.String())
			}
			if !strings.Contains(line, "max_iterations="+tc.expected) {
				t.Fatal(line)
			}
			if strings.Contains(line, "unlimited") != (enforced == config.TurnLimitUnlimited) {
				t.Fatalf("%s enforced %d", line, enforced)
			}
		})
	}
}

// Compiled stand in for the installed session reset policy plugin
type sessionResetPolicyPlugin struct{}

func (sessionResetPolicyPlugin) Manifest() plugins.Manifest {
	return plugins.Manifest{Name: plugins.SessionResetPluginName, Source: "bundled", Kind: "standalone"}
}

func (sessionResetPolicyPlugin) Register(*plugins.Context) error { return nil }

var registerSessionResetPolicy sync.Once

func TestStartupWarnsOnTimedSessionResetUnlessPluginEnabled(t *testing.T) {
	for _, pluginEnabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[pluginEnabled], func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			os.MkdirAll(home, 0o755)
			t.Setenv("ICEBATH_HOME", home)
			cfg := "session_reset:\n  mode: idle\n  idle_minutes: 60\n"
			if pluginEnabled {
				registerSessionResetPolicy.Do(func() { plugins.RegisterBundled(sessionResetPolicyPlugin{}) })
				cfg += "plugins:\n  enabled:\n    - " + plugins.SessionResetPluginName + "\n"
			}
			if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(cfg), 0o644); err != nil {
				t.Fatal(err)
			}
			plugins.ResetPluginManagersForTests()
			t.Cleanup(plugins.ResetPluginManagersForTests)
			runner := allocGatewayRunner()
			runner.config = &gateway.GatewayConfig{MultiplexProfiles: new(false)}
			logs := captureWarnings(t)
			runner.startLogRetiredSessionReset(context.Background())
			var warned []string
			for _, l := range strings.Split(logs.String(), "\n") {
				if strings.Contains(l, "session_reset.mode: idle") {
					warned = append(warned, l)
				}
			}
			if (len(warned) == 0) != pluginEnabled {
				t.Fatalf("plugin enabled %v warned %v", pluginEnabled, warned)
			}
			if !pluginEnabled && !strings.Contains(warned[0], plugins.SessionResetPluginName) {
				t.Fatal(warned[0])
			}
		})
	}
}

// Loopback receiver recording each webhook delivery with its headers and body
type hookReceiver struct {
	mu   sync.Mutex
	hits map[string]http.Header
	body map[string][]byte
}

func TestLaunchProfileWebhooksRegisterSignedUnderMultiplex(t *testing.T) {
	recv := &hookReceiver{hits: map[string]http.Header{}, body: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)
		recv.mu.Lock()
		recv.hits[req.URL.Path], recv.body[req.URL.Path] = req.Header.Clone(), data
		recv.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	launchHome := filepath.Join(t.TempDir(), ".icebath")
	os.MkdirAll(launchHome, 0o755)
	os.WriteFile(filepath.Join(launchHome, ".env"), []byte("MY_HOOK_SECRET=s3cret-of-launch\n"), 0o644)
	os.WriteFile(filepath.Join(launchHome, "config.yaml"), []byte("hooks:\n  outbound:\n"+
		"    - {url: \""+server.URL+"/signed\", events: [on_session_end], secret_env: MY_HOOK_SECRET}\n"+
		"    - {url: \""+server.URL+"/plain\", events: [on_session_end]}\n"), 0o644)
	t.Setenv("ICEBATH_HOME", launchHome)
	os.Unsetenv("MY_HOOK_SECRET")
	plugins.InstallServices(&plugins.Services{GetSecret: provider.GetSecret})
	plugins.ResetPluginManagersForTests()
	plugins.ResetOutboundWebhooksForTests()
	t.Cleanup(func() {
		plugins.ResetOutboundWebhooksForTests()
		plugins.ResetPluginManagersForTests()
		plugins.InstallServices(nil)
	})
	logs := captureWarnings(t)
	provider.SetMultiplexActive(true)
	func() {
		defer provider.SetMultiplexActive(false)
		if provider.CurrentSecretScope(context.Background()) != nil {
			t.Fatal("startup has no turn scope yet")
		}
		registerLaunchProfileConfigHooks(context.Background())
	}()
	if strings.Contains(logs.String(), "registration failed") {
		t.Fatal(logs.String())
	}
	scoped := gateway.ProfileRuntimeScope(context.Background(), launchHome, true)
	plugins.GetPluginManager(scoped).InvokeHook(scoped, "on_session_end", map[string]any{"session_id": "s1"})
	if !plugins.FlushOutboundWebhooks(5 * time.Second) {
		t.Fatal("queue must drain")
	}
	recv.mu.Lock()
	defer recv.mu.Unlock()
	signed, plain := recv.hits["/signed"], recv.hits["/plain"]
	if signed == nil || plain == nil {
		t.Fatalf("both launch profile targets must deliver %v", recv.hits)
	}
	mac := hmac.New(sha256.New, []byte("s3cret-of-launch"))
	mac.Write(recv.body["/signed"])
	if signed.Get("X-Hermes-Signature-256") != "sha256="+hex.EncodeToString(mac.Sum(nil)) {
		t.Fatal(signed)
	}
	if plain.Get("X-Hermes-Signature-256") != "" {
		t.Fatal(plain)
	}
}

func TestRegistrationFailureIsAWarningNotDebug(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("hooks:\n  outbound:\n"+
		"    - {url: \"http://127.0.0.1:9/hook\", events: [on_session_end], secret_env: MY_HOOK_SECRET}\n"), 0o644)
	plugins.InstallServices(&plugins.Services{GetSecret: func(context.Context, string) (string, bool, error) {
		return "", false, errors.New("config exploded")
	}})
	plugins.ResetOutboundWebhooksForTests()
	t.Cleanup(func() {
		plugins.ResetOutboundWebhooksForTests()
		plugins.InstallServices(nil)
	})
	logs := captureWarnings(t)
	registerLaunchProfileConfigHooks(context.Background())
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "registration failed") {
		t.Fatal(logs.String())
	}
}
