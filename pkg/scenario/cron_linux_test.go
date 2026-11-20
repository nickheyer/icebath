package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// A cron job is created, listed, paused, resumed, run once through the agent, recorded and removed
func TestScenarioCronCreateListPauseResumeRunRemove(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Text{Text: "cron says hi"}}})
	s.Run("cron", "list").Exit(0).Has("No scheduled jobs in profile 'default'.")

	created := s.Run("cron", "create", "--name", "daily", "0 9 * * *", "say hi").Exit(0)
	created.Has("Name: daily", "Schedule: 0 9 * * *", "Next run: ")
	id := created.Match(`Created job: (\S+)`)[1]
	s.HasJSON(s.Read("cron/jobs.json"), `{"jobs": [{"id": "`+id+`", "name": "daily", "prompt": "say hi", "enabled": true, "state": "scheduled",
		"schedule": {"kind": "cron", "expr": "0 9 * * *", "display": "0 9 * * *"}, "repeat": {"times": null, "completed": 0},
		"last_run_at": null, "last_status": null}]}`)
	s.Run("cron", "list").Exit(0).Has("Scheduled Jobs (profile: default)", id+" [active]", "Name:", "daily", "Schedule:", "0 9 * * *", "Repeat:", "Deliver:", "local")

	s.Run("cron", "pause", id).Exit(0).Has("Paused job: daily (" + id + ")")
	s.HasJSON(s.Read("cron/jobs.json"), `{"jobs": [{"id": "`+id+`", "enabled": false, "state": "paused"}]}`)
	s.Run("cron", "list").Exit(0).Has(id + " [paused]")
	s.Run("cron", "resume", id).Exit(0).Has("Resumed job: daily ("+id+")", "Next run: ")
	s.HasJSON(s.Read("cron/jobs.json"), `{"jobs": [{"id": "`+id+`", "enabled": true, "state": "scheduled", "paused_at": null}]}`)
	s.Run("cron", "pause", "nope").Exit(1).Has("Failed to pause job: Job with ID or name 'nope' not found. Use cronjob(action='list') to inspect jobs.")

	slot := Dig(s.value(s.Read("cron/jobs.json")), "jobs", 0, "next_run_at")
	s.RunStep(Step{Args: []string{"cron", "run", id}, Timeout: 4 * time.Minute}).Exit(0).Has("Triggered job: daily ("+id+")", "Ran now: succeeded.")
	prompted := false
	for _, m := range s.LastMessages() {
		text, _ := Dig(m, "content").(string)
		prompted = prompted || (Dig(m, "role") == "user" && strings.Contains(text, "say hi"))
	}
	if !prompted {
		s.Failf("the job prompt did not reach the model as a user message")
	}
	after := s.value(s.Read("cron/jobs.json"))
	s.HasJSON(after, `{"jobs": [{"id": "`+id+`", "last_status": "ok", "last_error": null, "state": "scheduled", "repeat": {"completed": 0}}]}`)
	if Dig(after, "jobs", 0, "next_run_at") != slot || Dig(after, "jobs", 0, "last_run_at") == nil {
		s.Failf("a manual run stamps last_run_at and leaves the scheduled occurrence %v, got %v and last_run_at %v",
			slot, Dig(after, "jobs", 0, "next_run_at"), Dig(after, "jobs", 0, "last_run_at"))
	}
	s.Run("cron", "runs").Exit(0).Has("completed", "job="+id)
	outputs, _ := filepath.Glob(filepath.Join(s.Home, "cron", "output", id, "*.md"))
	if len(outputs) != 1 {
		s.Failf("the run left %d output files under cron/output/%s, want 1", len(outputs), id)
	}
	if text, _ := os.ReadFile(outputs[0]); !strings.Contains(string(text), "cron says hi") || !strings.Contains(string(text), "say hi") {
		s.Failf("the run output lacks the prompt or the answer\n%s", clip(string(text)))
	}

	s.Run("cron", "remove", id).Exit(0).Has("Removed job: daily (" + id + ")")
	s.SameJSON(Dig(s.value(s.Read("cron/jobs.json")), "jobs"), `[]`)
	s.Run("cron", "list").Exit(0).Has("No scheduled jobs in profile 'default'.")
}
