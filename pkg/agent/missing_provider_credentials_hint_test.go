package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Temp home holding the config text, bound as the process home and the tool env of the context
func credentialHintHome(t *testing.T, config string) (context.Context, string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return tools.WithToolEnv(context.Background(), &tools.Env{Home: h}), h
}

// Removes an environment variable for the test, restored when it ends
func unsetEnvForTest(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// Compression pinned to the provider with no credentials anywhere, the real call_llm ladder refusal
func auxLadderRefusal(t *testing.T, ctx context.Context) error {
	t.Helper()
	_, err := provider.CallLLM(ctx, map[string]any{"task": "compression", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "max_tokens": 5})
	var unavailable *provider.AuxiliaryClientUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("call_llm returned %T %v, want AuxiliaryClientUnavailable", err, err)
	}
	return err
}

func TestAuxLadderNamesRegistryRemedyForExplicitProvider(t *testing.T) {
	cases := []struct{ provider, expected, forbidden string }{
		{"minimax-oauth", "icebath auth add minimax-oauth", "MINIMAX-OAUTH_API_KEY"},
		{"alibaba", "Set the DASHSCOPE_API_KEY environment variable", "ALIBABA_API_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			unsetEnvForTest(t, "MINIMAX_API_KEY", "DASHSCOPE_API_KEY")
			ctx, _ := credentialHintHome(t, "model:\n  provider: "+tc.provider+"\n  default: test-model\nauxiliary:\n  compression:\n    provider: "+tc.provider+"\n    model: test-model\n")
			err := auxLadderRefusal(t, ctx)
			if !strings.Contains(err.Error(), tc.expected) {
				t.Fatalf("%q lacks %q", err.Error(), tc.expected)
			}
			if strings.Contains(err.Error(), tc.forbidden) {
				t.Fatalf("%q names the invented %q", err.Error(), tc.forbidden)
			}
		})
	}
}

func TestMainInitSharesHelperAndNoRegistryProviderGetsAnInventedEnvVar(t *testing.T) {
	ctx, _ := credentialHintHome(t, "")
	a := &AIAgent{provider: "minimax-oauth", model: "m", requestedProvider: "minimax-oauth"}
	timeout := 60.0
	_, err := routedClientKwargs(ctx, a, nil, &timeout)
	if err == nil || !strings.Contains(err.Error(), "icebath auth add minimax-oauth") {
		t.Fatalf("agent init error %v", err)
	}
	for _, row := range provider.CanonicalRegistryRows(ctx) {
		invented := strings.ToUpper(row.Name) + "_API_KEY"
		message := provider.MissingProviderCredentialsMessage(ctx, row.Name)
		if strings.Contains(message, invented) && !slices.Contains(row.KeyEnv, invented) {
			t.Fatalf("%s: %q names %s which nothing reads", row.Name, message, invented)
		}
		if len(row.KeyEnv) == 0 && strings.Contains(message, "_API_KEY environment variable") {
			t.Fatalf("%s: %q invents an env var", row.Name, message)
		}
	}
}

// Persists a Codex OAuth pool the way mark_exhausted leaves it after a 429
func writeExhaustedCodexPoolEntries(t *testing.T, h string, count int, resetAt float64) {
	t.Helper()
	entries := make([]any, 0, count)
	for i := range count {
		entries = append(entries, map[string]any{"provider": "openai-codex", "id": "codex-" + string(rune('0'+i)), "label": "acct" + string(rune('0'+i)),
			"auth_type": "oauth", "priority": i, "source": "manual", "access_token": "eyJ.fake." + string(rune('0'+i)), "refresh_token": "rt",
			"last_status": "exhausted", "last_status_at": float64(time.Now().Unix()) - 60, "last_error_code": 429,
			"last_error_reason": "usage_limit_reached", "last_error_reset_at": resetAt})
	}
	raw, err := json.Marshal(map[string]any{"version": 1, "credential_pool": map[string]any{"openai-codex": entries}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestExhaustedOauthPoolReportsCooldownNotMissingCredentials(t *testing.T) {
	ctx, h := credentialHintHome(t, "model:\n  provider: openai-codex\n  default: test-model\nauxiliary:\n  compression:\n    provider: openai-codex\n    model: test-model\n")
	t.Setenv("HOME", h)
	unsetEnvForTest(t, "OPENAI_API_KEY")
	resetAt := float64(time.Now().Unix()) + 3*3600
	writeExhaustedCodexPoolEntries(t, h, 2, resetAt)
	expectedTime := time.Unix(int64(resetAt), 0).Local().Format("2006-01-02 15:04")
	a := &AIAgent{provider: "openai-codex", model: "test-model", requestedProvider: "openai-codex"}
	timeout := 60.0
	_, err := routedClientKwargs(ctx, a, nil, &timeout)
	if err == nil {
		t.Fatal("agent init resolved a client from an exhausted pool")
	}
	message := err.Error()
	if !strings.Contains(message, "all 2 credentials are cooling down") || !strings.Contains(message, expectedTime) || strings.Contains(message, "no credentials were found") {
		t.Fatalf("agent init message %q", message)
	}
	auxErr := auxLadderRefusal(t, ctx)
	if !strings.Contains(auxErr.Error(), "cooling down") || strings.Contains(auxErr.Error(), "no credentials were found") {
		t.Fatalf("aux ladder message %q", auxErr.Error())
	}
}

func TestElapsedCooldownOrEmptyPoolKeepsMissingCredentialsText(t *testing.T) {
	ctx, h := credentialHintHome(t, "")
	if msg := provider.MissingProviderCredentialsMessage(ctx, "openai-codex"); strings.Contains(msg, "cooling down") {
		t.Fatalf("no pool called cooling down: %q", msg)
	}
	writeExhaustedCodexPoolEntries(t, h, 1, float64(time.Now().Unix())-60)
	if msg := provider.MissingProviderCredentialsMessage(ctx, "openai-codex"); strings.Contains(msg, "cooling down") {
		t.Fatalf("elapsed pool called cooling down: %q", msg)
	}
}
