package scenario

import (
	"encoding/json"
	"fmt"
	"testing"
)

// Sends one JSON-RPC request to the stdio TUI gateway and returns its answer
func stdioRPC(s *S, p *Proc, id int, method string, params map[string]any) map[string]any {
	s.t.Helper()
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	p.Send(string(req) + "\n")
	line := p.Await(fmt.Sprintf(`(?m)^(\{"jsonrpc": "2\.0", "id": %d, .*\})$`, id))[1]
	answer, _ := s.value(line).(map[string]any)
	return answer
}

// The Plugins hub backend over the TUI gateway lists, toggles, configures and removes desktop halves on this host
func TestScenarioPluginsManageHubAndDesktopHalves(t *testing.T) {
	s := Start(t, Setup{Home: map[string]string{
		"desktop-plugins/chip/plugin.js":                 "export default {}\n",
		"desktop-plugins/paired/plugin.js":               "export default {}\n",
		"desktop-plugins/paired/.hermes-package.json":    `{"package": "paired", "source": "/src/paired", "sourceMtimeMs": 1}`,
		"cache/plugin-catalog.json":                      `{"entries": []}`,
		"desktop-plugins/.keep-root-for-listing/note.md": "staging folders start with a dot\n",
	}})
	p := s.SpawnStep(Step{Args: []string{"__icebath_tui_gateway_entry__"}, Pipe: true})
	p.Await(`"type": "gateway.ready"`)
	id := 0
	call := func(params map[string]any) map[string]any {
		id++
		return stdioRPC(s, p, id, "plugins.manage", params)
	}

	listed := call(map[string]any{})
	rows, _ := Dig(listed, "result", "plugins").([]any)
	user, _ := Dig(listed, "result", "user_count").(float64)
	bundled, _ := Dig(listed, "result", "bundled_count").(float64)
	if len(rows) == 0 || int(user+bundled) != len(rows) {
		s.Failf("list answered %d rows with user %v and bundled %v", len(rows), user, bundled)
	}
	var cleanup map[string]any
	for _, row := range rows {
		if r, _ := row.(map[string]any); r["key"] == "disk-cleanup" {
			cleanup = r
		}
	}
	s.HasJSON(cleanup, `{"name": "disk-cleanup", "source": "bundled", "status": "not enabled", "portable": false, "has_desktop_half": false, "servers": [], "tools": []}`)

	s.HasJSON(call(map[string]any{"action": "toggle"}), `{"error": {"code": 4019, "message": "plugins.toggle requires a 'key' or 'name'"}}`)
	s.HasJSON(call(map[string]any{"action": "settings", "key": "disk-cleanup", "values": map[string]any{"nope": 1}}),
		`{"error": {"code": 4021, "message": "'nope' is not declared in the plugin's config_schema"}}`)
	s.HasJSON(call(map[string]any{"action": "settings", "key": "ghost", "values": map[string]any{}}), `{"error": {"code": 4020, "message": "plugin 'ghost' not found"}}`)
	s.HasJSON(call(map[string]any{"action": "settings", "key": "disk-cleanup"}),
		`{"error": {"code": 4019, "message": "plugins.settings requires a 'key' and a 'values' mapping"}}`)
	s.HasJSON(call(map[string]any{"action": "presence", "names": []any{"no-such-catalog-plugin"}}), `{"result": {"presence": []}}`)
	if _, isList := Dig(call(map[string]any{"action": "onboarding"}), "result", "onboarding").([]any); !isList {
		s.Failf("onboarding answered no list")
	}
	for action, message := range map[string]string{
		"install":         "plugins.install requires 'identifier', 'repo', or 'catalog_name'",
		"inspect":         "plugins.inspect requires 'identifier', 'repo', or 'catalog_name'",
		"install_desktop": "plugins.manage install_desktop requires 'identifier', 'repo', or 'catalog_name'",
		"remove_desktop":  "plugins.manage remove_desktop requires a 'name'",
		"update":          "plugins.update requires a 'name'",
		"remove":          "plugins.remove requires a 'name'",
	} {
		s.HasJSON(call(map[string]any{"action": action}), `{"error": {"code": 4019, "message": "`+message+`"}}`)
	}
	s.HasJSON(call(map[string]any{"action": "remove", "name": "ghost"}), `{"error": {"code": 5026}}`)
	s.HasJSON(call(map[string]any{"action": "bogus"}), `{"error": {"code": 4017, "message": "unknown plugins action: bogus"}}`)

	s.HasJSON(call(map[string]any{"action": "remove_desktop", "name": " chip "}),
		`{"result": {"ok": true, "plugin_name": "chip", "path": "${ICEBATH_HOME}/desktop-plugins/chip", "unpaired_from": null}}`)
	if s.Exists("desktop-plugins/chip") {
		s.Failf("the removed desktop plugin is still on disk")
	}
	s.HasJSON(call(map[string]any{"action": "remove_desktop", "name": "paired"}),
		`{"error": {"code": 5026, "message": "'paired' is the desktop half of an agent plugin — uninstall that plugin instead."}}`)
	if !s.Exists("desktop-plugins/paired/plugin.js") {
		s.Failf("a refused removal deleted the package half")
	}
	s.HasJSON(call(map[string]any{"action": "remove_desktop", "key": "paired", "force": true}),
		`{"result": {"ok": true, "plugin_name": "paired", "unpaired_from": "paired"}}`)
	s.HasJSON(call(map[string]any{"action": "remove_desktop", "name": "../escape"}),
		`{"error": {"code": 5026, "message": "'../escape' is not a desktop plugin folder name: it must be a plain folder name, not a path."}}`)
	s.HasJSON(call(map[string]any{"action": "remove_desktop", "name": ".keep-root-for-listing"}),
		`{"error": {"code": 5026, "message": "'.keep-root-for-listing' is not a desktop plugin folder name: it must be a plain folder name, not a path."}}`)
	s.HasJSON(call(map[string]any{"action": "remove_desktop", "name": "ghost"}), `{"error": {"code": 5026, "message": "No desktop plugin named 'ghost' is installed."}}`)
	s.HasJSON(call(map[string]any{"action": "install_desktop", "identifier": "https://example.invalid/fx/chip.git", "ref": "main"}),
		`{"error": {"code": 5026, "message": "Installing a desktop plugin needs the commit the install dialog reviewed."}}`)
	s.HasJSON(call(map[string]any{"action": "install_desktop", "catalog_name": "no-such-catalog-plugin", "ref": "ABCDEF0123456789abcdef0123456789abcdef01"}),
		`{"error": {"code": 5026, "message": "'no-such-catalog-plugin' is not in the Icebath plugin catalog."}}`)
	p.CloseInput()
	p.Wait().Exit(0)
}

// The Plugins hub toggle enables a bundled plugin and answers with its listing row
func TestScenarioPluginsManageToggle(t *testing.T) {
	s := Start(t, Setup{Home: map[string]string{"cache/plugin-catalog.json": `{"entries": []}`}})
	s.Finding("SF-2")
	p := s.SpawnStep(Step{Args: []string{"__icebath_tui_gateway_entry__"}, Pipe: true})
	p.Await(`"type": "gateway.ready"`)
	toggled := stdioRPC(s, p, 1, "plugins.manage", map[string]any{"action": "toggle", "name": "disk-cleanup", "enable": true})
	s.HasJSON(toggled, `{"result": {"ok": true, "name": "disk-cleanup", "plugin": {"key": "disk-cleanup", "status": "enabled"}}}`)
	p.CloseInput()
	p.Wait().Exit(0)
}
