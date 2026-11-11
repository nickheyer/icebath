package gateway

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func footerSecs(v float64) *float64 { return &v }

func TestModelShortDropsVendorPrefix(t *testing.T) {
	for _, c := range [][2]string{{"openai/gpt-5.4", "gpt-5.4"}, {"anthropic/claude-sonnet-4.6", "claude-sonnet-4.6"}, {"gpt-5.4", "gpt-5.4"}, {"", ""}} {
		if got := modelShort(c[0]); got != c[1] {
			t.Fatalf("%q got %q", c[0], got)
		}
	}
}

func TestHomeRelativeCwdCollapsesHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	sub := filepath.Join(tmp, "projects", "hermes")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := homeRelativeCwd(sub); got != "~/projects/hermes" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatFooterAllFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("TERMINAL_CWD", filepath.Join(tmp, "projects", "hermes"))
	if err := os.MkdirAll(filepath.Join(tmp, "projects", "hermes"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := FormatRuntimeFooter(context.Background(), FooterInputs{Model: "openrouter/openai/gpt-5.4", ContextTokens: 68000, ContextLength: 100000}, []string{"model", "context_pct", "cwd"})
	if err != nil || out != "gpt-5.4 · 68% · ~/projects/hermes" {
		t.Fatalf("got %q %v", out, err)
	}
}

func TestFormatFooterSkipsMissingContextLength(t *testing.T) {
	out, err := FormatRuntimeFooter(context.Background(), FooterInputs{Model: "openai/gpt-5.4", ContextTokens: 500, Cwd: "/tmp/wd"}, []string{"model", "context_pct", "cwd"})
	if err != nil || strings.Contains(out, "%") || !strings.Contains(out, "gpt-5.4") || !strings.Contains(out, "/tmp/wd") {
		t.Fatalf("got %q %v", out, err)
	}
}

func TestResolvePlatformOverrideWins(t *testing.T) {
	user := map[string]any{"display": map[string]any{
		"runtime_footer": map[string]any{"enabled": true, "fields": []any{"model"}},
		"platforms":      map[string]any{"slack": map[string]any{"runtime_footer": map[string]any{"enabled": false}}},
	}}
	if enabled, _ := ResolveFooterConfig(user, "telegram"); !enabled {
		t.Fatal("telegram off")
	}
	if enabled, _ := ResolveFooterConfig(user, "slack"); enabled {
		t.Fatal("slack on")
	}
}

func TestResolvePlatformCanAddFieldsOnly(t *testing.T) {
	user := map[string]any{"display": map[string]any{
		"runtime_footer": map[string]any{"enabled": true},
		"platforms":      map[string]any{"discord": map[string]any{"runtime_footer": map[string]any{"fields": []any{"context_pct"}}}},
	}}
	enabled, fields := ResolveFooterConfig(user, "telegram")
	if !enabled || !slices.Equal(fields, []string{"model", "context_pct", "cwd"}) {
		t.Fatalf("telegram %v %v", enabled, fields)
	}
	enabled, fields = ResolveFooterConfig(user, "discord")
	if !enabled || !slices.Equal(fields, []string{"context_pct"}) {
		t.Fatalf("discord %v %v", enabled, fields)
	}
}

func TestBuildFooterPerPlatformOffSuppresses(t *testing.T) {
	user := map[string]any{"display": map[string]any{
		"runtime_footer": map[string]any{"enabled": true},
		"platforms":      map[string]any{"slack": map[string]any{"runtime_footer": map[string]any{"enabled": false}}},
	}}
	out, err := BuildFooterLine(context.Background(), user, "slack", FooterInputs{Model: "openai/gpt-5.4", ContextTokens: 10, ContextLength: 100, Cwd: "/tmp"})
	if err != nil || out != "" {
		t.Fatalf("got %q %v", out, err)
	}
}

func TestFormatLatency(t *testing.T) {
	cases := []struct {
		seconds float64
		want    string
	}{{0.0, "<1s"}, {0.4, "<1s"}, {0.999, "<1s"}, {1.0, "1s"}, {22.0, "22s"}, {22.4, "22s"}, {59.4, "59s"},
		{59.6, "1m00s"}, {60.0, "1m00s"}, {65.0, "1m05s"}, {125.0, "2m05s"}, {3600.0, "60m00s"}}
	for _, c := range cases {
		if got := formatLatency(c.seconds); got != c.want {
			t.Fatalf("%v got %q", c.seconds, got)
		}
	}
}

// Footer of the latency field alone for the given measurement
func latencyOnly(t *testing.T, seconds *float64) string {
	t.Helper()
	out, err := FormatRuntimeFooter(context.Background(), FooterInputs{Model: "m", TurnSeconds: seconds}, []string{"latency"})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFormatFooterLatencySkippedWhenUnmeasured(t *testing.T) {
	if got := latencyOnly(t, nil); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatFooterLatencySkippedWhenNegative(t *testing.T) {
	if got := latencyOnly(t, footerSecs(-1.0)); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatFooterLatencyZeroRendersSubSecond(t *testing.T) {
	if got := latencyOnly(t, footerSecs(0.0)); got != "<1s" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatFooterLatencyInFieldOrder(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	out, err := FormatRuntimeFooter(context.Background(), FooterInputs{Model: "openai/gpt-5.4", ContextTokens: 68_000, ContextLength: 100_000, Cwd: tmp, TurnSeconds: footerSecs(65.0)},
		[]string{"model", "context_pct", "latency", "cwd"})
	if err != nil || out != "gpt-5.4 · 68% · 1m05s · ~" {
		t.Fatalf("got %q %v", out, err)
	}
}

func TestBuildFooterLineThreadsTurnSeconds(t *testing.T) {
	t.Setenv("TERMINAL_CWD", "")
	os.Unsetenv("TERMINAL_CWD")
	user := map[string]any{"display": map[string]any{"runtime_footer": map[string]any{"enabled": true, "fields": []any{"model", "latency"}}}}
	out, err := BuildFooterLine(context.Background(), user, "discord", FooterInputs{Model: "gpt-5.4", TurnSeconds: footerSecs(22.0)})
	if err != nil || out != "gpt-5.4 · 22s" {
		t.Fatalf("got %q %v", out, err)
	}
}

func TestDefaultBuildFooterLineIgnoresTurnSeconds(t *testing.T) {
	t.Setenv("TERMINAL_CWD", "")
	os.Unsetenv("TERMINAL_CWD")
	user := map[string]any{"display": map[string]any{"runtime_footer": map[string]any{"enabled": true}}}
	in := FooterInputs{Model: "openai/gpt-5.4", ContextTokens: 50_247, ContextLength: 1_000_000, Cwd: "/var/data"}
	baseline, err := BuildFooterLine(context.Background(), user, "discord", in)
	if err != nil {
		t.Fatal(err)
	}
	in.TurnSeconds = footerSecs(125.0)
	withTiming, err := BuildFooterLine(context.Background(), user, "discord", in)
	if err != nil || baseline != "gpt-5.4 · 5% · /var/data" || withTiming != baseline {
		t.Fatalf("baseline %q timing %q %v", baseline, withTiming, err)
	}
}

func TestFormatFooterServedModelIsOptInAndSkipsSameModel(t *testing.T) {
	ctx := context.Background()
	render := func(in FooterInputs, fields []string) string {
		out, err := FormatRuntimeFooter(ctx, in, fields)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if strings.Contains(render(FooterInputs{Model: "hermes-router", Cwd: "/x", ServedModel: "gpt-4o-2024-11-20"}, defaultFooterFields), "→") {
		t.Fatal("served model shown by default")
	}
	if got := render(FooterInputs{Model: "hermes-router", Cwd: "/x", ServedModel: "gpt-4o-2024-11-20"}, []string{"served_model"}); got != "hermes-router → gpt-4o-2024-11-20" {
		t.Fatalf("got %q", got)
	}
	if got := render(FooterInputs{Model: "qwen/qwen3.8-max", Cwd: "/x", RequestedModel: "gpt-5.6-sol", ServedModel: "qwen/qwen3.8-max"}, []string{"served_model"}); got != "gpt-5.6-sol → qwen/qwen3.8-max" {
		t.Fatalf("got %q", got)
	}
	if got := render(FooterInputs{Model: "gpt-5.4", Cwd: "/x"}, []string{"served_model"}); got != "" {
		t.Fatalf("got %q", got)
	}
}
