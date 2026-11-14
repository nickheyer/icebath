package scenario

import (
	"encoding/json"
	"strings"
	"testing"
)

// cron doctor reports a corrupt stored timezone per job and logs its warning once per value
func TestScenarioCronKnobsDoctorWarnsOncePerCorruptTimezone(t *testing.T) {
	s := Start(t, Setup{})
	s.Run("cron", "doctor").Exit(0).Has("Cron doctor found no issues", "No active jobs configured.")
	first := s.Run("cron", "create", "--name", "alpha", "0 9 * * *", "say hi").Exit(0).Match(`Created job: (\S+)`)[1]
	second := s.Run("cron", "create", "--name", "beta", "0 10 * * *", "say bye").Exit(0).Match(`Created job: (\S+)`)[1]
	var doc map[string]any
	if err := json.Unmarshal([]byte(s.Read("cron/jobs.json")), &doc); err != nil {
		s.Failf("jobs.json is not JSON: %v", err)
	}
	for _, job := range doc["jobs"].([]any) {
		job.(map[string]any)["timezone"] = "Mars/Olympus"
	}
	raw, _ := json.Marshal(doc)
	s.Write("cron/jobs.json", string(raw))
	r := s.Run("cron", "doctor").Exit(1)
	r.Has("Cron doctor found 2 issue(s) across 2 job(s):", first+" alpha", second+" beta",
		"- timezone 'Mars/Olympus' is not a valid IANA zone; the job falls back to the profile timezone",
		"Review the findings above, then run `icebath cron doctor` again.")
	warning := "Job timezone 'Mars/Olympus' is invalid ('No time zone found with key Mars/Olympus'); falling back to the profile setting. Fix or clear it with `icebath cron edit`."
	if n := strings.Count(s.Read("logs/agent.log"), warning); n != 1 {
		s.Failf("agent.log holds the corrupt timezone warning %d times, want 1\n%s", n, clip(s.Read("logs/agent.log")))
	}
}
