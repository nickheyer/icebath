package browser

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

func browserEnv(section map[string]any) *tools.Env {
	return &tools.Env{Config: config.Tree{"browser": section}}
}

func staticExecProps() map[string]any {
	return BrowserExecSchema()["parameters"].(map[string]any)["properties"].(map[string]any)
}

// test_browser_real_profile.py TestBrowserExecSchemaGating test_local_arg_absent_without_consent
func TestExecSchemaLocalArgAbsentWithoutConsent(t *testing.T) {
	overrides := BrowserExecSchemaOverrides(browserEnv(map[string]any{"use_real_profile": false}))
	if _, ok := overrides["parameters"]; ok {
		t.Fatal("parameters overridden without consent")
	}
	if _, ok := staticExecProps()["local"]; ok {
		t.Fatal("static schema carries local")
	}
}

// test_browser_real_profile.py TestBrowserExecSchemaGating test_local_arg_present_with_consent
func TestExecSchemaLocalArgPresentWithConsent(t *testing.T) {
	overrides := BrowserExecSchemaOverrides(browserEnv(map[string]any{"use_real_profile": true}))
	params := overrides["parameters"].(map[string]any)
	local, ok := params["properties"].(map[string]any)["local"].(map[string]any)
	if !ok || local["type"] != "boolean" || local["default"] != false {
		t.Fatalf("local %v", local)
	}
	if _, ok := staticExecProps()["local"]; ok {
		t.Fatal("static schema mutated")
	}
	for _, r := range params["required"].([]any) {
		if r == "local" {
			t.Fatal("local required")
		}
	}
}

// Consent reads the value as Python bool() does, a quoted false string consenting and zero not
func TestRealProfileConsentIsPythonTruthiness(t *testing.T) {
	for _, c := range []struct {
		value any
		want  bool
	}{{"false", true}, {"no", true}, {0, false}, {"", false}, {nil, false}, {1, true}} {
		if got := realProfileConsented(browserEnv(map[string]any{"use_real_profile": c.value})); got != c.want {
			t.Fatalf("use_real_profile %#v consented %v", c.value, got)
		}
	}
}

// A usable lightpanda engine replaces the screenshot header with the text first lightpanda one
func TestExecDescriptionHeaderLightpanda(t *testing.T) {
	pin(t, &lightpandaFindBinary, func(*tools.Env) string { return "/opt/lightpanda" })
	pin(t, &lightpandaCloudProvider, func(*tools.Env) (BrowserProvider, error) { return nil, nil })
	desc := BrowserExecSchemaOverrides(browserEnv(map[string]any{"engine": "lightpanda"}))["description"].(string)
	if desc != execHeaderBase+execHeaderLightpanda+execHelpersDigest || !strings.Contains(desc, "TargetAlreadyLoaded") {
		t.Fatalf("description %q", desc)
	}
}

// Both tools register under their toolsets with their static schemas and the exec rewriter
func TestBrowserToolsRegistered(t *testing.T) {
	for name, toolset := range map[string]string{"browser_exec": "browser-use", "browser_cdp": "browser-cdp"} {
		def := tools.Default.Get(name)
		if def == nil || def.Toolset != toolset || def.Schema["name"] != name {
			t.Fatalf("%s registered as %+v", name, def)
		}
	}
	if tools.Default.Get("browser_exec").Check != nil || tools.Default.Get("browser_cdp").Check == nil {
		t.Fatal("browser_exec is built in and browser_cdp needs a configured endpoint")
	}
	if tools.Default.Get("browser_exec").DynamicSchema == nil || tools.Default.Get("browser_cdp").DynamicSchema != nil {
		t.Fatal("only browser_exec tailors its schema per session")
	}
	if required := BrowserExecSchema()["parameters"].(map[string]any)["required"].([]any); len(required) != 1 || required[0] != "code" {
		t.Fatalf("exec schema requires %v", required)
	}
	if BrowserCDPSchema()["parameters"].(map[string]any)["required"].([]any)[0] != "method" {
		t.Fatal("cdp schema required")
	}
}
