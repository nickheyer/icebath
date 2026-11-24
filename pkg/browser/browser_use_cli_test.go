package browser

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Env of a fresh profile home whose config.yaml holds the browser section
func browserEnv(t *testing.T, section map[string]any) *tools.Env {
	t.Helper()
	h := t.TempDir()
	writeBrowserSection(t, h, section)
	return &tools.Env{Home: h}
}

func staticExecProps() map[string]any {
	return jsonutil.Plain(BrowserExecSchema().Value("parameters")).(map[string]any)["properties"].(map[string]any)
}

// test_browser_real_profile.py TestBrowserExecSchemaGating test_local_arg_absent_without_consent
func TestExecSchemaLocalArgAbsentWithoutConsent(t *testing.T) {
	overrides := BrowserExecSchemaOverrides(browserEnv(t, map[string]any{"use_real_profile": false}))
	if _, ok := overrides.Get("parameters"); ok {
		t.Fatal("parameters overridden without consent")
	}
	if _, ok := staticExecProps()["local"]; ok {
		t.Fatal("static schema carries local")
	}
}

// test_browser_real_profile.py TestBrowserExecSchemaGating test_local_arg_present_with_consent
func TestExecSchemaLocalArgPresentWithConsent(t *testing.T) {
	overrides := BrowserExecSchemaOverrides(browserEnv(t, map[string]any{"use_real_profile": true}))
	params := jsonutil.Plain(overrides.Value("parameters")).(map[string]any)
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

// Consent reads the value as an on or off switch, a quoted false or no never consenting
func TestRealProfileConsentReadsOnOffWords(t *testing.T) {
	for _, c := range []struct {
		value any
		want  bool
	}{{"false", false}, {"no", false}, {"yes", true}, {0, false}, {"", false}, {nil, false}, {1, true}, {true, true}} {
		if got := realProfileConsented(browserEnv(t, map[string]any{"use_real_profile": c.value})); got != c.want {
			t.Fatalf("use_real_profile %#v consented %v", c.value, got)
		}
	}
}

// A usable lightpanda engine replaces the screenshot header with the text first lightpanda one
func TestExecDescriptionHeaderLightpanda(t *testing.T) {
	pin(t, &lightpandaFindBinary, func(*tools.Env) string { return "/opt/lightpanda" })
	pin(t, &lightpandaCloudProvider, func(*tools.Env) (BrowserProvider, error) { return nil, nil })
	desc := BrowserExecSchemaOverrides(browserEnv(t, map[string]any{"engine": "lightpanda"})).Value("description").(string)
	if desc != execHeaderBase+execHeaderLightpanda+execHelpersDigest || !strings.Contains(desc, "TargetAlreadyLoaded") {
		t.Fatalf("description %q", desc)
	}
}

// Both tools register under their toolsets with their static schemas and the exec rewriter
func TestBrowserToolsRegistered(t *testing.T) {
	for name, toolset := range map[string]string{"browser_exec": "browser-use", "browser_cdp": "browser-cdp"} {
		def := tools.Default.Get(name)
		if def == nil || def.Toolset != toolset || def.Schema.Value("name") != name {
			t.Fatalf("%s registered as %+v", name, def)
		}
	}
	if tools.Default.Get("browser_exec").Check != nil || tools.Default.Get("browser_cdp").Check == nil {
		t.Fatal("browser_exec is built in and browser_cdp needs a configured endpoint")
	}
	if tools.Default.Get("browser_exec").DynamicSchema == nil || tools.Default.Get("browser_cdp").DynamicSchema != nil {
		t.Fatal("only browser_exec tailors its schema per session")
	}
	if required := jsonutil.Plain(BrowserExecSchema().Value("parameters")).(map[string]any)["required"].([]any); len(required) != 1 || required[0] != "code" {
		t.Fatalf("exec schema requires %v", required)
	}
	if jsonutil.Plain(BrowserCDPSchema().Value("parameters")).(map[string]any)["required"].([]any)[0] != "method" {
		t.Fatal("cdp schema required")
	}
}

func TestBrowserExecSchemaCarriesTheReferenceWording(t *testing.T) {
	schema := BrowserExecSchema()
	if desc, _ := schema.Value("description").(string); !strings.HasPrefix(desc, "Drive a real browser for JS pages, logins, forms and clicks (Browser Use CLI). ") {
		t.Fatalf("description %q", desc[:80])
	}
	session := jsonutil.Plain(schema.Value("parameters")).(map[string]any)["properties"].(map[string]any)["session"].(map[string]any)
	if session["description"] != "Named isolated browser session — its own daemon and (on cloud backends) own browser, so concurrent tasks don't share tabs. Reuse the same name on every related call; omit for the shared default session." {
		t.Fatalf("session %q", session["description"])
	}
}
