package plugins

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func capConsentRun(t *testing.T, tty bool, answers ...string) (bool, string, int) {
	t.Helper()
	ttyBackup, inputBackup := isTTY, inputLine
	t.Cleanup(func() { isTTY, inputLine = ttyBackup, inputBackup })
	isTTY = func() bool { return tty }
	asked := 0
	inputLine = func(string) (string, error) {
		asked++
		if asked <= len(answers) {
			return answers[asked-1], nil
		}
		return "", nil
	}
	var out bytes.Buffer
	granted, err := capabilityConsent(context.Background(), &Console{Out: &out}, "capplug", []string{"tools.override"}, "install")
	if err != nil {
		t.Fatal(err)
	}
	return granted, out.String(), asked
}

func capGranted(t *testing.T, h, capability string) bool {
	cfg, err := readUserConfig(h)
	if err != nil {
		t.Fatal(err)
	}
	return CapabilityGranted(cfg, "capplug", capability)
}

func TestConsentFlow(t *testing.T) {
	t.Run("yes_records_grant", func(t *testing.T) {
		h := c6Home(t, nil)
		if granted, out, _ := capConsentRun(t, true, "y"); !granted || !capGranted(t, h, "tools.override") || !strings.Contains(out, "Granted: tools.override") {
			t.Fatalf("%v %q", granted, out)
		}
	})
	t.Run("decline_leaves_ungranted", func(t *testing.T) {
		h := c6Home(t, nil)
		if granted, out, _ := capConsentRun(t, true, "n"); granted || capGranted(t, h, "tools.override") || !strings.Contains(out, "Declined. capplug stays enabled") {
			t.Fatalf("%v %q", granted, out)
		}
	})
	t.Run("non_interactive_fails_closed", func(t *testing.T) {
		h := c6Home(t, nil)
		granted, out, asked := capConsentRun(t, false)
		if granted || capGranted(t, h, "tools.override") || asked != 0 || !strings.Contains(out, "Non-interactive session: capabilities NOT granted") {
			t.Fatalf("%v %d %q", granted, asked, out)
		}
	})
	t.Run("already_granted_skips_prompt", func(t *testing.T) {
		h := c6Home(t, nil)
		if err := RecordConsent(context.Background(), h, "capplug", []string{"tools.override"}, []string{"tools.override"}); err != nil {
			t.Fatal(err)
		}
		if granted, out, asked := capConsentRun(t, true); !granted || asked != 0 || out != "" {
			t.Fatalf("%v %d %q", granted, asked, out)
		}
	})
}

func TestToolOverrideGrantAndDeclaredCapabilities(t *testing.T) {
	h := c6Home(t, nil)
	var out bytes.Buffer
	yes := true
	if err := resolveToolOverrideGrant(context.Background(), &Console{Out: &out}, "web/exa", &yes); err != nil {
		t.Fatal(err)
	}
	cfg, _ := readUserConfig(h)
	if pluginEntry(cfg, "web/exa")["allow_tool_override"] != true || !strings.Contains(out.String(), "plugins.entries.web/exa.allow_tool_override: true") {
		t.Fatalf("%v %q", cfg, out.String())
	}
	no := false
	out.Reset()
	resolveToolOverrideGrant(context.Background(), &Console{Out: &out}, "web/exa", &no)
	cfg, _ = readUserConfig(h)
	if pluginEntry(cfg, "web/exa")["allow_tool_override"] != false || !strings.Contains(out.String(), "--allow-tool-override") {
		t.Fatalf("%v %q", cfg, out.String())
	}
	if got := declaredCapabilitiesFromManifest(map[string]any{"capabilities": []any{"tools.override"}}, ""); strings.Join(got, ",") != "tools.override" {
		t.Fatalf("%v", got)
	}
	if got := declaredCapabilitiesFromManifest(nil, "x"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}
