package cron

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const noAgentWithoutScriptError = "no_agent=True requires a script — with no agent and no script " +
	"there is nothing for the job to run."

// Rejects monitor and no_agent combinations and command line scripts at both the create and update doors
func (s *Store) validateJobModeInvariants(monitorScript, monitorURL *string, noAgent bool, script *string) error {
	ms, mu, sc := deref(monitorScript), deref(monitorURL), deref(script)
	if ms != "" && mu != "" {
		return errors.New("monitor_script and monitor_url are mutually exclusive — a job " +
			"can only have one monitor source.")
	}
	if (ms != "" || mu != "") && noAgent {
		return errors.New("monitor_script/monitor_url cannot be combined with no_agent=True — " +
			"the whole point of a monitor job is to suppress or wake the AGENT " +
			"based on source changes. Use a plain no_agent script job instead.")
	}
	if noAgent && sc == "" {
		return errors.New(noAgentWithoutScriptError)
	}
	if sc != "" && s.scriptIsCommandLine(sc) {
		return fmt.Errorf("script must name a file under %s (e.g. 'watchdog.sh'), not a shell command line: "+
			"%s has no such file. Write the command into a script there and pass its filename.",
			home.Display(s.home)+"/scripts/", pyrepr.Repr(sc))
	}
	return nil
}

// Validates the timezone and grace knobs, grace only meaning something on a cron schedule
func validateScheduleKnobs(schedule *Schedule, timezone any, graceMinutes any) error {
	if _, err := NormalizeTimezone(timezone); err != nil {
		return err
	}
	minutes, err := NormalizeGraceMinutes(graceMinutes)
	if err != nil || minutes == nil {
		return err
	}
	var kind any
	if schedule != nil && schedule.Kind != "" {
		kind = schedule.Kind
	}
	if kind != "cron" {
		return fmt.Errorf("grace_minutes only applies to cron schedules (a fixed wall clock that can be "+
			"missed); this job's schedule kind is %s.", pyrepr.Repr(kind))
	}
	return nil
}

// Script text containing whitespace that resolves to no file is a shell command line
func (s *Store) scriptIsCommandLine(script string) bool {
	if !strings.ContainsFunc(strings.TrimSpace(script), unicode.IsSpace) {
		return false
	}
	path, ok := resolveScriptPath(s, script)
	if !ok {
		return true
	}
	st, err := os.Stat(path)
	return err != nil || !st.Mode().IsRegular()
}
