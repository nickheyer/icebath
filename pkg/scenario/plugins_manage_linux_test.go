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

// The Plugins hub backend over the TUI gateway lists, toggles and configures plugins on this host
func TestScenarioPluginsManageHub(t *testing.T) {
	s := Start(t, Setup{Home: map[string]string{
		"cache/plugin-catalog.json": `{"entries": []}`,
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
	s.HasJSON(cleanup, `{"name": "disk-cleanup", "source": "bundled", "status": "not enabled", "portable": false, "servers": [], "tools": []}`)

	s.HasJSON(call(map[string]any{"action": "toggle"}), `{"error": {"code": 4019, "message": "plugins.toggle requires a 'key' or 'name'"}}`)
	s.HasJSON(call(map[string]any{"action": "settings", "key": "disk-cleanup", "values": map[string]any{"nope": 1}}),
		`{"error": {"code": 4021, "message": "'nope' is not declared in the plugin's config_schema"}}`)
	s.HasJSON(call(map[string]any{"action": "settings", "key": "ghost", "values": map[string]any{}}), `{"error": {"code": 4020, "message": "plugin 'ghost' not found"}}`)
	s.HasJSON(call(map[string]any{"action": "settings", "key": "disk-cleanup"}),
		`{"error": {"code": 4019, "message": "plugins.settings requires a 'key' and a 'values' mapping"}}`)
	s.HasJSON(call(map[string]any{"action": "presence", "names": []any{"no-such-catalog-plugin"}}), `{"result": {"presence": []}}`)
	for action, message := range map[string]string{
		"install": "plugins.install requires 'identifier', 'repo', or 'catalog_name'",
		"inspect": "plugins.inspect requires 'identifier', 'repo', or 'catalog_name'",
		"update":  "plugins.update requires a 'name'",
		"remove":  "plugins.remove requires a 'name'",
	} {
		s.HasJSON(call(map[string]any{"action": action}), `{"error": {"code": 4019, "message": "`+message+`"}}`)
	}
	s.HasJSON(call(map[string]any{"action": "remove", "name": "ghost"}), `{"error": {"code": 5026}}`)
	s.HasJSON(call(map[string]any{"action": "bogus"}), `{"error": {"code": 4017, "message": "unknown plugins action: bogus"}}`)

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
