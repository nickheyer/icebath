package browser

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestToolsConfigBridgeExposesTheRuntime(t *testing.T) {
	hooks := tools.ToolsConfigHooks
	if hooks.LightpandaInstallHint() != lightpandaInstallHint || hooks.BakedChromiumFile() != bakedExecutableFile {
		t.Fatal("constants not forwarded")
	}
	env := &tools.Env{Home: t.TempDir()}
	pin(t, &findCLI, func(*tools.Env) []string { return []string{"py", "-m", "browser_harness.run"} })
	if got := hooks.FindBrowserUseCLI(env); len(got) != 3 {
		t.Fatalf("harness %v", got)
	}
	pin(t, &runtimePMExecutable, func() string { return "/pm/chromium/chrome" })
	if got := hooks.ChromiumExecutable(env); got != "/pm/chromium/chrome" {
		t.Fatalf("chromium %q", got)
	}
	if hooks.BrowserRegistry == nil || hooks.BrowserRegistry.Get(context.Background(), env, "no-such-browser") != nil {
		t.Fatal("browser registry view wrong")
	}
	if len(hooks.BrowserRegistry.List(context.Background(), env)) != len(BrowserProviderRegistry.ListProviders(context.Background(), cfgOf(env).homeDir())) {
		t.Fatal("browser registry listing differs")
	}
}

func TestToolsPostSetupChromiumPrintsTheImageHint(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	baked := filepath.Join(t.TempDir(), "chromium-executable-path")
	if err := os.WriteFile(baked, []byte("/opt/icebath/chromium/chrome"), 0o644); err != nil {
		t.Fatal(err)
	}
	pin(t, &bakedExecutableFile, baked)
	pin(t, &runtimePMExecutable, func() string { return "" })
	pin(t, &runtimePMHasNoChromium, func() bool { return false })
	pin(t, &runtimeIsTermux, func() bool { return false })
	pin(t, &findCLI, func(*tools.Env) []string { return []string{"harness"} })
	out := &bytes.Buffer{}
	con := &cliui.Console{In: strings.NewReader(""), Out: out, Getenv: func(string) (string, bool) { return "", false },
		StdinTTY: func() bool { return false }, StdoutTTY: func() bool { return false }}
	s := &tools.ToolsSetup{Ctx: context.Background(), Env: &tools.Env{Home: h}, Con: con}
	if code := s.RunPostSetupCommand(map[string]any{"post_setup_key": "chromium"}); code != 0 {
		t.Fatalf("exit %d output %q", code, out.String())
	}
	want := "  Running post-setup hook: chromium\n" +
		"✓     browser_exec ready (browser-harness, bundled with Icebath)\n" +
		"⚠     The image's Chromium is missing: pull the current Icebath image and recreate the container " +
		"(a volume mounted over the image's tool store hides it).\n" +
		"✓ Post-setup 'chromium' complete\n"
	if out.String() != want {
		t.Fatalf("output %q", out.String())
	}
}
