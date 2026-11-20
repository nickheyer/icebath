package scenario

import (
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Returns the system prompt and the tool names of the newest request with tools
func lastPromptAndTools(s *S) (string, []string) {
	s.t.Helper()
	system, _ := Dig(s.LastMessages(), 0, "content").(string)
	bodies := s.Model.MainRequests()
	var names []string
	for _, tool := range bodies[len(bodies)-1]["tools"].([]any) {
		name, _ := Dig(tool, "function", "name").(string)
		names = append(names, name)
	}
	return system, names
}

// The system prompt carries SOUL.md and one project context file, the flags narrow the rules and the toolsets
func TestScenarioPromptContextFilesAndToolsets(t *testing.T) {
	s := Start(t, Setup{
		Home: map[string]string{"SOUL.md": "You are Scenario Soul. SOUL-MARKER-5\n"},
		Work: map[string]string{
			"AGENTS.md":    "# Project rules\n\nAGENTS-MARKER-17 answer in one line.\n",
			"CLAUDE.md":    "CLAUDE-MARKER-31\n",
			".cursorrules": "CURSOR-MARKER-23\n",
		},
	})
	s.Run("chat", "-q", "one", "-Q").Exit(0)
	system, tools := lastPromptAndTools(s)
	for _, marker := range []string{"SOUL-MARKER-5", "AGENTS-MARKER-17"} {
		if !strings.Contains(system, marker) {
			s.Failf("the system prompt lacks %s", marker)
		}
	}
	for _, marker := range []string{"CLAUDE-MARKER-31", "CURSOR-MARKER-23"} {
		if strings.Contains(system, marker) {
			s.Failf("the system prompt holds %s although AGENTS.md wins the project context", marker)
		}
	}
	if !strings.Contains(strings.Join(tools, " "), "terminal") || !strings.Contains(strings.Join(tools, " "), "read_file") {
		s.Failf("the default toolsets offer %v", tools)
	}

	s.Write("${WORK}/AGENTS.override.md", "OVERRIDE-MARKER-41 personal rules.\n")
	s.Run("chat", "-q", "two", "-Q").Exit(0)
	if system, _ = lastPromptAndTools(s); !strings.Contains(system, "OVERRIDE-MARKER-41") || strings.Contains(system, "AGENTS-MARKER-17") {
		s.Failf("AGENTS.override.md does not replace AGENTS.md in the system prompt")
	}

	s.Run("chat", "--ignore-rules", "-q", "three", "-Q").Exit(0)
	system, _ = lastPromptAndTools(s)
	for _, marker := range []string{"SOUL-MARKER-5", "AGENTS-MARKER-17", "OVERRIDE-MARKER-41", "CURSOR-MARKER-23"} {
		if strings.Contains(system, marker) {
			s.Failf("--ignore-rules left %s in the system prompt", marker)
		}
	}

	s.Run("chat", "-t", "file", "-q", "four", "-Q").Exit(0)
	_, tools = lastPromptAndTools(s)
	offered := " " + strings.Join(tools, " ") + " "
	for _, want := range []string{" read_file ", " write_file ", " patch ", " search_files "} {
		if !strings.Contains(offered, want) {
			s.Failf("-t file does not offer%s: %v", want, tools)
		}
	}
	for _, refuse := range []string{" terminal ", " web_search ", " browser_exec ", " delegate_task ", " memory "} {
		if strings.Contains(offered, refuse) {
			s.Failf("-t file still offers%s: %v", refuse, tools)
		}
	}

	id := s.Run("chat", "--pass-session-id", "-q", "five", "-Q").Exit(0).Match(`(?m)^session_id: (\S+)$`)[1]
	if system, _ = lastPromptAndTools(s); !strings.Contains(system, id) {
		s.Failf("--pass-session-id did not put the session id %s into the system prompt", id)
	}
}

// Ctrl+C during a turn ends the quiet run with exit 130 and still names the session
func TestScenarioChatInterruptExits130(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Hang{Seconds: 120}}})
	turn := s.Spawn("chat", "-q", "Think for a long time", "-Q")
	if err := s.Model.WaitForRequests(1, 2*time.Minute, "main"); err != nil {
		s.Failf("the turn never reached the model: %v", err)
	}
	ended := turn.Stop()
	if ended.Code != 130 {
		s.Failf("an interrupted quiet turn exits 130, got %d\n%s", ended.Code, ended.dump())
	}
	if !strings.Contains(ended.Stderr, "session_id: ") {
		s.Failf("an interrupted quiet turn still prints its session id\n%s", ended.dump())
	}
}
