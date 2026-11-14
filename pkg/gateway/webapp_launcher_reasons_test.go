package gateway

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Root home whose Webapp is off, failing on any build or gateway start
func reasonsHome(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	saved := wl
	t.Cleanup(func() { wl = saved })
	wl.build = func(context.Context, map[string]any) error { t.Fatal("built a Webapp that is off"); return nil }
	wl.ensure = func(context.Context, string) error { t.Fatal("started a gateway for a Webapp that is off"); return nil }
	return root
}

// Runs the webapp command for a home and returns its exit code and stdout
func runReasons(t *testing.T, h string, ns map[string]any) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code, err := CmdWebapp(context.Background(), h, ns, "", &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	return code, out.String()
}

// Writes the home's config
func writeReasonsConfig(t *testing.T, h, config string) {
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTheLauncherPrintsWhyAMalformedWebappBlockKeepsItOff(t *testing.T) {
	for _, c := range []struct{ block, key string }{
		{"webapp:\n  enabled: true\n  port: 70000\n", "webapp.port"},
		{"webapp:\n  enabled: true\n  host: 192.168.1.20\n", "webapp.host"},
		{"webapp:\n  enabled: 'yes'\n", "webapp.enabled"},
	} {
		h := reasonsHome(t)
		writeReasonsConfig(t, h, c.block)
		code, out := runReasons(t, h, webappArgs(nil))
		if code != 1 || !strings.Contains(out, c.key) || strings.Contains(out, "Set webapp.enabled: true") {
			t.Fatalf("%q code %d out %q", c.block, code, out)
		}
	}
}

func TestTheLauncherStillSaysHowToTurnAnAbsentWebappOn(t *testing.T) {
	h := reasonsHome(t)
	writeReasonsConfig(t, h, "")
	if code, out := runReasons(t, h, webappArgs(nil)); code != 1 || !strings.Contains(out, "Set webapp.enabled: true") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestStatusWithoutAGatewayPrintsWhyTheWebappIsOff(t *testing.T) {
	h := reasonsHome(t)
	wl.control = func(string, string, map[string]any) (map[string]any, error) {
		return nil, &gatewayUnreachable{"no gateway"}
	}
	writeReasonsConfig(t, h, "webapp:\n  enabled: true\n  port: 70000\n")
	code, out := runReasons(t, h, webappArgs(map[string]any{"status": true}))
	if code != 1 || !strings.Contains(out, "webapp.port") || strings.Contains(out, "Set webapp.enabled: true") {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestStatusOfAGatewayThatServesNoWebappReadsTheCurrentConfig(t *testing.T) {
	for _, c := range []struct{ block, expected string }{
		{"webapp:\n  enabled: true\n  port: 70000\n", "webapp.port"},
		{"webapp:\n  enabled: true\n", webappRestartCommand},
	} {
		h := reasonsHome(t)
		wl.control = func(string, string, map[string]any) (map[string]any, error) {
			return map[string]any{"ok": true, "result": map[string]any{"state": "off", "url": nil, "reason": nil}}, nil
		}
		writeReasonsConfig(t, h, c.block)
		_, out := runReasons(t, h, webappArgs(map[string]any{"status": true}))
		if !strings.Contains(out, c.expected) || strings.Contains(out, "Set webapp.enabled: true") {
			t.Fatalf("%q out %q", c.block, out)
		}
	}
}
