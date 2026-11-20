package scenario

import (
	"archive/zip"
	"io"
	"os"
	"strings"
	"testing"
)

// A backup archive holds the configuration and data of the home and an import restores them into an empty home
func TestScenarioBackupAndImport(t *testing.T) {
	s := Start(t, Setup{Home: map[string]string{"memories/MEMORY.md": "The deploy key lives in vault slot 7\n"}})
	s.Run("config", "set", "agent.max_turns", "42").Exit(0)
	job := s.Run("cron", "create", "--name", "nightly", "0 3 * * *", "tidy up").Exit(0).Match(`Created job: (\S+)`)[1]
	task := s.Run("kanban", "create", "Write the report").Exit(0).Match(`Created (t_[0-9a-f]+)`)[1]

	archive := s.Work + "/backup.zip"
	made := s.Run("backup", "-o", archive).Exit(0)
	made.Has("Scanning ${ICEBATH_HOME} ...", "Backup complete: "+archive, "Files:", "Original:", "Compressed:", "Restore with: icebath import backup.zip")
	reader, err := zip.OpenReader(archive)
	if err != nil {
		s.Failf("the backup is not a zip archive: %v", err)
	}
	stored := map[string]string{}
	for _, f := range reader.File {
		if f.Name == "config.yaml" || f.Name == ".env" || f.Name == "memories/MEMORY.md" || f.Name == "cron/jobs.json" {
			rc, _ := f.Open()
			raw, _ := io.ReadAll(rc)
			rc.Close()
			stored[f.Name] = string(raw)
		} else {
			stored[f.Name] = ""
		}
	}
	reader.Close()
	for _, name := range []string{"config.yaml", ".env", "memories/MEMORY.md", "cron/jobs.json", "kanban.db"} {
		if _, ok := stored[name]; !ok {
			s.Failf("the backup lacks %s, it holds %d files", name, len(stored))
		}
	}
	if stored["config.yaml"] != s.Read("config.yaml") || stored["memories/MEMORY.md"] != "The deploy key lives in vault slot 7\n" {
		s.Failf("the archived files differ from the home")
	}

	restored := map[string]string{"ICEBATH_HOME": "${TMP}/restored"}
	target := s.Expand("${TMP}/restored")
	s.Run("import", s.Work+"/missing.zip").Exit(1).Has("Error: File not found: " + s.Work + "/missing.zip")
	s.Write("${WORK}/not-a-zip.zip", "plain text")
	s.Run("import", s.Work+"/not-a-zip.zip").Exit(1).Has("Error: Not a valid zip file: " + s.Work + "/not-a-zip.zip")
	imported := s.RunStep(Step{Args: []string{"import", archive}, Env: restored}).Exit(0)
	imported.Has("Backup contains ", " files", "Target: "+target, "Checking archive integrity ...", "Importing ", "Import complete: ", "files restored in")
	for name, want := range map[string]string{"config.yaml": stored["config.yaml"], ".env": stored[".env"], "memories/MEMORY.md": stored["memories/MEMORY.md"]} {
		raw, err := os.ReadFile(target + "/" + name)
		if err != nil || string(raw) != want {
			s.Failf("the import restored %s as %q (%v)", name, raw, err)
		}
	}
	if got := s.RunStep(Step{Args: []string{"config", "get", "agent.max_turns"}, Env: restored}).Exit(0); got.Stdout != "42\n" {
		s.Failf("the restored home reports agent.max_turns %q", got.Stdout)
	}
	s.RunStep(Step{Args: []string{"cron", "list"}, Env: restored}).Exit(0).Has(job, "nightly")
	s.RunStep(Step{Args: []string{"kanban", "list"}, Env: restored}).Exit(0).Has(task, "Write the report")

	again := s.RunStep(Step{Args: []string{"import", archive}, Env: restored, Stdin: "n\n"})
	again.Has("Warning: Target directory already has Icebath configuration.", "Importing will overwrite existing files with backup contents.", "Aborted.")
	s.RunStep(Step{Args: []string{"import", archive, "--force"}, Env: restored}).Exit(0).Has("Import complete: ")
	if strings.Contains(made.Text()+imported.Text(), "sk-fake-e2e") {
		s.Failf("backup or import printed the API key")
	}
}
