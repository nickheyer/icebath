package observability

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Installs the shipped provider names the setup rows name and restores the catalog afterwards
func withProviderNames(t *testing.T, names ...string) {
	t.Helper()
	setCatalog(t, "provider_names", names...)
}

// Event and failure class of every provider setup row
func setupRows(rows []Mark) [][2]string {
	var out [][2]string
	for _, r := range rows {
		if r.Name == ProviderSetupMark {
			out = append(out, [2]string{r.Data["event"], r.Data["failure_class"]})
		}
	}
	return out
}

func TestOAuthSessionEndings(t *testing.T) {
	cases := []struct {
		sess   map[string]any
		ending [2]string
	}{
		{map[string]any{"status": "approved"}, [2]string{"completed", "none"}},
		{map[string]any{"status": "expired"}, [2]string{"abandoned", "none"}},
		{map[string]any{"status": "error", "reason": "timeout"}, [2]string{"abandoned", "none"}},
		{map[string]any{"status": "denied"}, [2]string{"failed", "auth"}},
		{map[string]any{"status": "denied", "reason": "user_declined"}, [2]string{"abandoned", "none"}},
		{map[string]any{"status": "pending", "cancelled": true}, [2]string{"abandoned", "none"}},
		{map[string]any{"status": "error", "reason": "anon_unreachable"}, [2]string{"failed", "network"}},
	}
	for i, c := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			m := captureMarks(t)
			withProviderNames(t, "nous")
			saved := webSetupSurface
			webSetupSurface = func() string { return "desktop" }
			t.Cleanup(func() { webSetupSurface = saved })
			ctx := context.Background()
			flow := BeginOAuthSetup(ctx, "nous", "")
			AttachOAuthSetup(c.sess, flow)
			SettleOAuthSetup(ctx, c.sess, false)
			SettleOAuthSetup(ctx, c.sess, false)
			got := setupRows(m.snapshot())
			if len(got) != 2 || got[0] != [2]string{"started", "none"} || got[1] != c.ending {
				t.Fatalf("rows %v want started then %v", got, c.ending)
			}
			for _, r := range m.snapshot() {
				if r.Data["provider"] != "nous" || r.Data["surface"] != "desktop" {
					t.Fatalf("row %v", r.Data)
				}
			}
		})
	}
}

func TestPendingOAuthSessionDoesNotSettle(t *testing.T) {
	m := captureMarks(t)
	withProviderNames(t, "xai")
	saved := webSetupSurface
	webSetupSurface = func() string { return "dashboard" }
	t.Cleanup(func() { webSetupSurface = saved })
	sess := map[string]any{"status": "pending"}
	AttachOAuthSetup(sess, BeginOAuthSetup(context.Background(), "xai", ""))
	SettleOAuthSetup(context.Background(), sess, false)
	got := setupRows(m.snapshot())
	if len(got) != 1 || got[0][0] != "started" {
		t.Fatalf("rows %v", got)
	}
}

func TestAPIKeyEnvMapsToProviderOnly(t *testing.T) {
	savedTool, savedReg := ToolProviderEnvKeysHook, ProviderForAPIKeyEnvHook
	t.Cleanup(func() { ToolProviderEnvKeysHook, ProviderForAPIKeyEnvHook = savedTool, savedReg })
	ToolProviderEnvKeysHook = func() []string { return []string{"GEMINI_API_KEY", "ELEVENLABS_API_KEY"} }
	ProviderForAPIKeyEnvHook = func(envVar string) (string, bool) {
		return map[string]string{"GEMINI_API_KEY": "gemini", "XAI_API_KEY": "xai"}[envVar], envVar == "GEMINI_API_KEY" || envVar == "XAI_API_KEY"
	}
	if p, ok := ProviderForAPIKeyEnv("OPENROUTER_API_KEY", false); !ok || p != "openrouter" {
		t.Fatalf("openrouter %q %v", p, ok)
	}
	if _, ok := ProviderForAPIKeyEnv("GITHUB_TOKEN_FOR_TOOLS_XYZ", false); ok {
		t.Fatal("unknown variable mapped")
	}
	for _, shared := range []string{"GITHUB_TOKEN", "GH_TOKEN", "HF_TOKEN", "GEMINI_API_KEY"} {
		if _, ok := ProviderForAPIKeyEnv(shared, false); ok {
			t.Fatalf("%s mapped", shared)
		}
	}
	if p, ok := ProviderForAPIKeyEnv("GEMINI_API_KEY", true); !ok || p != "gemini" {
		t.Fatalf("connecting gemini %q %v", p, ok)
	}
}

func TestOffThreadSetupRecordsInTheOwningProfile(t *testing.T) {
	captureMarks(t)
	withProviderNames(t, "xai")
	var mu sync.Mutex
	var seen []string
	recordProcessMarksSaved = func(ctx context.Context, rows []Mark) int {
		mu.Lock()
		seen = append(seen, home.Dir(ctx))
		mu.Unlock()
		return len(rows)
	}
	base := t.TempDir()
	homeA, homeB := filepath.Join(base, "a"), filepath.Join(base, "b")
	for _, h := range []string{homeA, homeB, homeA} {
		flow := BeginProviderSetup(context.Background(), "dashboard", "xai", h)
		if flow == nil || filepath.Dir(flow.Marker) != setupMarkersDir(h) {
			t.Fatalf("flow %+v", flow)
		}
		FinishProviderSetup(context.Background(), flow, "completed", "none")
	}
	want := fmt.Sprint([]string{homeA, homeA, homeB, homeB, homeA, homeA})
	if fmt.Sprint(seen) != want {
		t.Fatalf("seen %v want %s", seen, want)
	}
}
