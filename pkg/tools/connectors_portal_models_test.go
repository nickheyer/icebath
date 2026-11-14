package tools

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestPortalWireValidatorsAndClientDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "tok")
	env := &Env{Home: home, Values: &sync.Map{}}

	old := NousPortalBaseURL
	NousPortalBaseURL = func(context.Context) (string, error) { return "https://portal.test/", nil }
	t.Cleanup(func() { NousPortalBaseURL = old })
	client := NewPortalConnectorClient(context.Background(), env, PortalOptions{})
	if origin, err := client.Origin(); err != nil || origin != "https://portal.test" {
		t.Fatalf("default origin %q %v", origin, err)
	}
	if token, err := client.AuthorizationToken(); err != nil || token != "tok" {
		t.Fatalf("default bearer %q %v", token, err)
	}
	t.Setenv("TOOL_GATEWAY_USER_TOKEN", "")
	if got := portalDefaultHeaderProvider(env); len(got) != 0 {
		t.Fatalf("no token must send no header %v", got)
	}

	tool := func(facet any) *pyjson.Object {
		return pyjson.ObjectOf("slug", "s", "name", "n", "description", "d", "facet", facet, "hints", []any{},
			"categories", []any{"Mail", "CRM"}, "noAuth", false, "deprecated", false)
	}
	got, err := wireConnectorTool(tool("telepathy"))
	if err != nil || got.Facet != "unclassified" || !reflect.DeepEqual(got.Categories, []string{"mail", "crm"}) {
		t.Fatalf("unknown facet and categories %#v %v", got, err)
	}
	if _, err := wireConnectorTool(tool(int64(3))); err == nil {
		t.Fatal("a non string facet must fail the facet literal")
	}

	for mode, check := range map[string]func(PolicyBody) bool{
		"unrestricted": func(b PolicyBody) bool { return b.Mode == "unrestricted" && b.Connectors == nil },
		"deny-all":     func(b PolicyBody) bool { return b.Mode == "deny-all" },
		"allow": func(b PolicyBody) bool {
			return reflect.DeepEqual(b.Connectors, []string{"gmail"}) && len(b.Tools) == 0
		},
		"deny": func(b PolicyBody) bool {
			return reflect.DeepEqual(b.DisabledConnectors, []string{"slack"}) && b.Tags != nil
		},
	} {
		o := pyjson.ObjectOf("mode", mode, "connectors", []any{"gmail"}, "disabled_connectors", []any{"slack"})
		if mode == "deny" {
			o.Set("tags", pyjson.ObjectOf("enable", []any{"x"}))
		}
		body, err := wirePolicyBody(o)
		if err != nil || !check(body) {
			t.Fatalf("%s body %#v %v", mode, body, err)
		}
	}
	if _, err := wirePolicyBody(pyjson.ObjectOf("mode", "allow")); err == nil {
		t.Fatal("an allow body without connectors must fail")
	}

	path := filepath.Join(home, "cached.json")
	for tools, want := range map[string]int{`{}`: 0, `""`: 0, `"ab"`: -1, `[]`: 0, `7`: -1} {
		body := `{"connector":"gmail","toolkit_version":"1","etag":"e","fetched_at":"12.5","tools":` + tools + `}`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		read := portalCachedTools(path)
		if (want < 0) != (read == nil) || (read != nil && (len(read.Tools) != want || read.FetchedAt != 12.5 || read.Source != "cache")) {
			t.Fatalf("cached tools %s read %#v", tools, read)
		}
	}
}
