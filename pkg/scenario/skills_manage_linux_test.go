package scenario

import (
	"strings"
	"testing"
)

// The Skills hub backend over the TUI gateway lists, inspects, installs, refuses and reports bad params
func TestScenarioSkillsManageOverTheTUIGateway(t *testing.T) {
	s := Start(t, Setup{})
	p := s.SpawnStep(Step{Args: []string{"__icebath_tui_gateway_entry__"}, Pipe: true})
	p.Await(`"type": "gateway.ready"`)
	id := 0
	call := func(params map[string]any) map[string]any {
		id++
		return stdioRPC(s, p, id, "skills.manage", params)
	}

	if _, isDict := Dig(call(map[string]any{}), "result", "skills").(map[string]any); !isDict {
		s.Failf("list answered no skills mapping")
	}
	s.HasJSON(call(map[string]any{"action": "bogus"}), `{"error": {"code": 4017, "message": "unknown skills action: bogus"}}`)
	s.HasJSON(call(map[string]any{"action": []any{"list"}}), `{"error": {"code": 5024, "message": "unhashable type: 'list'"}}`)
	s.HasJSON(call(map[string]any{"action": "search", "query": 7}), `{"error": {"code": 5024, "message": "'int' object has no attribute 'strip'"}}`)
	s.HasJSON(call(map[string]any{"action": "install", "query": nil}), `{"error": {"code": 5024, "message": "'NoneType' object has no attribute 'rstrip'"}}`)
	s.HasJSON(call(map[string]any{"action": "inspect", "query": 3}), `{"error": {"code": 5024, "message": "argument of type 'int' is not iterable"}}`)
	s.HasJSON(call(map[string]any{"action": "browse", "page": "abc"}), `{"error": {"code": 5024, "message": "invalid literal for int() with base 10: 'abc'"}}`)
	s.HasJSON(call(map[string]any{"action": "browse", "query": 2}), `{"error": {"code": 5024, "message": "'int' object has no attribute 'isdigit'"}}`)
	s.HasJSON(call(map[string]any{"action": "profile-less", "profile": "ghost"}), `{"error": {"code": 4064, "message": "profile 'ghost' not found"}}`)

	s.HasJSON(call(map[string]any{"action": "inspect", "query": "official/security/1password"}),
		`{"result": {"info": {"name": "1password", "source": "official", "identifier": "official/security/1password"}}}`)
	s.HasJSON(call(map[string]any{"action": "install", "query": "official/security/1password"}),
		`{"result": {"installed": true, "name": "official/security/1password"}}`)
	if !s.Exists("skills/security/1password/SKILL.md") {
		s.Failf("the installed skill is not on disk")
	}
	again := call(map[string]any{"action": "install", "query": "official/security/1password"})
	s.HasJSON(again, `{"error": {"code": 5031, "message": "Use --force to reinstall.", "data": {"installed": false, "name": "official/security/1password"}}}`)
	if log, _ := Dig(again, "error", "data", "log").(string); !strings.Contains(log, "is already installed at") {
		s.Failf("the reinstall log lacks the warning\n%v", again)
	}
	missing := call(map[string]any{"action": "install", "query": "official/security/no-such-skill"})
	s.HasJSON(missing, `{"error": {"code": 5031, "data": {"installed": false, "name": "official/security/no-such-skill"}}}`)
	if log, _ := Dig(missing, "error", "data", "log").(string); !strings.Contains(log, "[bold red]Error:[/] Could not download 'official/security/no-such-skill'.") {
		s.Failf("the failed install log lacks the download error\n%v", missing)
	}
	s.HasJSON(call(map[string]any{"action": "list"}), `{"result": {"skills": {}}}`)
	p.CloseInput()
	p.Wait().Exit(0)
	fresh := s.SpawnStep(Step{Args: []string{"__icebath_tui_gateway_entry__"}, Pipe: true})
	fresh.Await(`"type": "gateway.ready"`)
	listed := stdioRPC(s, fresh, 1, "skills.manage", map[string]any{"action": "list"})
	if names, _ := Dig(listed, "result", "skills", "security").([]any); len(names) == 0 {
		s.Failf("a new gateway process does not list the installed security skill\n%v", listed)
	}
	fresh.CloseInput()
	fresh.Wait().Exit(0)
}
