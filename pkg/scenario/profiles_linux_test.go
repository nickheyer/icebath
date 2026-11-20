package scenario

import (
	"os"
	"testing"
)

// A profile is cloned from the default, addressed with -p, made sticky, renamed, exported and deleted
func TestScenarioProfilesCreateUseRenameExportDelete(t *testing.T) {
	s := Start(t, Setup{})
	s.Set("P", "${ICEBATH_HOME}/profiles")
	s.Run("profile", "list").Exit(0).Has("Profile", "Model", "Gateway", "Alias", "◆default", "fake-model", "stopped").Lacks("work")

	s.Run("profile", "create", "work", "--clone", "--no-alias").Exit(0).Has("Profile 'work' created at ${P}/work",
		"Cloned config, .env, SOUL.md, and skills from default.", "Edit ${P}/work/.env for different API keys")
	s.SameJSON(Dig(s.YAML("${P}/work/config.yaml"), "model"), Dig(s.YAML("config.yaml"), "model"))
	if s.Read("${P}/work/.env") != s.Read(".env") {
		s.Failf("the cloned profile .env differs from the default one")
	}
	s.Run("profile", "list").Exit(0).Has("◆default", "work")
	if r := s.Run("-p", "work", "config", "path").Exit(0); r.Stdout != s.Expand("${P}/work/config.yaml\n") {
		s.Failf("-p work config path printed %q", r.Stdout)
	}
	s.Run("-p", "work", "config", "set", "agent.max_turns", "7").Exit(0).Has("✓ Set agent.max_turns = 7 in ${P}/work/config.yaml")
	s.HasJSON(s.YAML("${P}/work/config.yaml"), `{"agent": {"max_turns": 7}}`)
	if _, leaked := Dig(s.YAML("config.yaml"), "agent").(map[string]any)["max_turns"]; leaked {
		s.Failf("a setting of profile work reached the default profile")
	}
	s.Run("profile", "show", "work").Exit(0).Has("Profile: work", "Path:    ${P}/work", "Model:   fake-model (custom)", "Gateway: stopped", ".env:    exists", "SOUL.md: exists")

	s.Run("profile", "use", "work").Exit(0).Has("Switched to: work")
	if r := s.Run("config", "path").Exit(0); r.Stdout != s.Expand("${P}/work/config.yaml\n") {
		s.Failf("after profile use work, config path printed %q", r.Stdout)
	}
	s.Run("profile", "list").Exit(0).Has("◆work")
	s.Run("profile", "use", "default").Exit(0).Has("Switched to: default (~/.icebath)")
	if r := s.Run("config", "path").Exit(0); r.Stdout != s.Home+"/config.yaml\n" {
		s.Failf("after profile use default, config path printed %q", r.Stdout)
	}

	s.Run("profile", "rename", "work", "job").Exit(0).Has("Profile renamed: work → job", "Path: ${P}/job")
	if s.Exists("${P}/work") || !s.Exists("${P}/job/config.yaml") {
		s.Failf("the rename did not move the profile directory")
	}
	s.Run("profile", "export", "job", "-o", "${WORK}/job.tar.gz").Exit(0).Has("✓ Exported 'job' to ${WORK}/job.tar.gz")
	if info, err := os.Stat(s.Work + "/job.tar.gz"); err != nil || info.Size() == 0 {
		s.Failf("the profile export wrote no archive: %v", err)
	}
	s.Run("profile", "delete", "job", "-y").Exit(0).Has("Profile: job", "This will permanently delete:", "✓ Removed ${P}/job", "Profile 'job' deleted.")
	if s.Exists("${P}/job") {
		s.Failf("the deleted profile directory still exists")
	}
	s.Run("profile", "list").Exit(0).Has("◆default").Lacks("job")
	s.Run("profile", "delete", "nope", "-y").Exit(1).Has("Error: No profile named 'nope'. See your profiles with: icebath profile list")
	s.Run("profile", "create", "Bad Name!").Exit(1).Has("is not a valid profile name.", "(for example: bad-name)")
}
