package scenario

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const scenarioSkill = "---\nname: scenario-echo\ndescription: Repeats the request back to the user for scenario checks.\n---\n\n# Scenario Echo\n\nRepeat the request back to the user.\n"

// A bundled optional skill is installed from the local official source, listed, kept on a second install and removed
func TestScenarioSkillsInstallFromLocalSource(t *testing.T) {
	s := Start(t, Setup{})
	s.Run("skills", "list").Exit(0).Has("Installed Skills", "Name", "Category", "Source", "Trust", "Status", "0 hub-installed").Lacks("1password")

	installed := s.Run("skills", "install", "official/security/1password", "--yes").Exit(0)
	installed.Has("Fetching: official/security/1password", "Installed: security/1password", "Files: ", "SKILL.md")
	stored := s.Read("skills/security/1password/SKILL.md")
	if !strings.HasPrefix(stored, "---\n") || !strings.Contains(stored, "name: 1password") {
		s.Failf("the installed SKILL.md lacks its frontmatter\n%s", clip(stored))
	}
	s.Run("skills", "list").Exit(0).Has("1password", "security", "official", "builtin", "1 hub-installed")

	s.Run("skills", "install", "official/security/1password", "--yes").Has("'1password' is already installed at", "Use --force to reinstall.")
	s.Run("skills", "install", "official/security/1password", "--yes", "--force").Exit(0).Has("Installed: security/1password")
	s.Run("skills", "install", "official/security/no-such-skill", "--yes").Exit(1).Has("Error: Could not download 'official/security/no-such-skill'.")

	s.Run("skills", "uninstall", "1password", "--yes").Exit(0)
	if s.Exists("skills/security/1password/SKILL.md") {
		s.Failf("the skill directory survived the uninstall")
	}
	s.Run("skills", "list").Exit(0).Has("0 hub-installed").Lacks("1password")
}

// A skill address on a loopback host is refused before any request leaves the process
func TestScenarioSkillsInstallRefusesLoopbackAddress(t *testing.T) {
	s := Start(t, Setup{})
	hub := s.Peer("hub", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		w.Write([]byte(scenarioSkill))
	}))
	refused := s.Run("skills", "install", "${PEER_hub}/scenario-echo/SKILL.md", "--yes").Exit(1)
	refused.Has("Fetching: ${PEER_hub}/scenario-echo/SKILL.md", "Error: Could not download '${PEER_hub}/scenario-echo/SKILL.md'.").Lacks("Installed: ")
	if got := hub.Requests(); len(got) != 0 {
		s.Failf("the loopback peer received %d requests, the address guard lets none through", len(got))
	}
	found := false
	filepath.WalkDir(filepath.Join(s.Home, "skills"), func(path string, d os.DirEntry, err error) error {
		found = found || strings.Contains(path, "scenario-echo")
		return nil
	})
	if found {
		s.Failf("a refused skill left files under skills")
	}
}
