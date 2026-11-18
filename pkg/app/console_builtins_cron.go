//go:build !icebath_core && !icebath_noextras

package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
)

// Adds the cron rows to the console built in commands
func init() {
	consoleTierBuiltins = append(consoleTierBuiltins,
		consoleBuiltin{[]string{"cron", "list"}, "cron list [--all]", "List scheduled jobs.", consoleCronList, ""},
		consoleBuiltin{[]string{"cron", "status"}, "cron status", "Show cron scheduler status.", consoleSimpleCommand("cron status", consoleCronStatus), ""},
		consoleBuiltin{[]string{"cron", "pause"}, "cron pause <job>", "Pause a scheduled job.", consoleCronPause, "Pause this cron job?"},
		consoleBuiltin{[]string{"cron", "resume"}, "cron resume <job>", "Resume a paused cron job.", consoleCronResume, "Resume this cron job?"},
		consoleBuiltin{[]string{"cron", "run"}, "cron run <job>", "Run a job on the next scheduler tick.", consoleCronRun, "Trigger this cron job?"})
}

// One cron job line naming the action, the job, its id and its effective state
func consoleFormatJob(job *cron.Job, action string) string {
	jobID := job.ID
	if jobID == "" {
		jobID = "?"
	}
	name := job.Name
	if name == "" {
		name = "(unnamed)"
	}
	return fmt.Sprintf("%s job: %s (%s) [%s]", action, name, jobID, cron.EffectiveJobState(job))
}

// Cron scheduler status
func consoleCronStatus(ctx context.Context) (int, error) {
	return 0, cron.CronStatus(ctx, home.Dir(ctx), os.Stdout)
}

// Scheduled jobs, disabled ones too with --all
func consoleCronList(e *ConsoleEngine, args []string) (string, error) {
	ns, err := consoleParse("cron list", args, consoleFlag("--all"))
	if err != nil {
		return "", err
	}
	all, _ := ns["all"].(bool)
	return consoleCaptureOutput(func() (int, error) { return 0, cron.CronList(e.ctx, home.Dir(e.ctx), os.Stdout, all) })
}

// Runs a single job cron action on its one reference, an ambiguous name becoming a console error
func consoleCronJobAction(args []string, usage, action string, run func(ref string) (*cron.Job, error)) (string, error) {
	if len(args) != 1 {
		return "", &ConsoleCommandError{Msg: "Usage: " + usage}
	}
	job, err := run(args[0])
	var ambiguous *cron.AmbiguousJobReferenceError
	if errors.As(err, &ambiguous) {
		return "", &ConsoleCommandError{Msg: err.Error()}
	}
	if err != nil {
		return "", err
	}
	if job == nil {
		return "", &ConsoleCommandError{Msg: "Job not found: " + args[0]}
	}
	return consoleFormatJob(job, action), nil
}

// Pauses a job with the console as the recorded reason
func consoleCronPause(e *ConsoleEngine, args []string) (string, error) {
	reason := "paused from icebath console"
	return consoleCronJobAction(args, "cron pause <job>", "Paused", func(ref string) (*cron.Job, error) {
		return cron.PauseJob(e.ctx, home.Dir(e.ctx), ref, &reason)
	})
}

// Resumes a paused job or re-arms a one shot at --at or now
func consoleCronResume(e *ConsoleEngine, args []string) (string, error) {
	ns, err := consoleParse("cron resume", args, consolePositional("job", "", nil), consoleOption(nil, false, "--at"),
		consoleFlag("--run-now"))
	if err != nil {
		return "", err
	}
	at, runNow := consoleText(ns, "at"), ns["run_now"] == true
	if at != "" && runNow {
		return "", &ConsoleCommandError{Msg: "Use exactly one of --at or --run-now."}
	}
	h, ref := home.Dir(e.ctx), consoleText(ns, "job")
	var job *cron.Job
	switch {
	case runNow:
		job, err = cron.RearmOneshot(e.ctx, h, ref, cron.NowISO(h))
	case at != "":
		job, err = cron.RearmOneshot(e.ctx, h, ref, at)
	default:
		job, err = cron.ResumeJob(e.ctx, h, ref)
	}
	var ambiguous *cron.AmbiguousJobReferenceError
	var invalid *cron.ValueError
	if errors.As(err, &ambiguous) || errors.As(err, &invalid) {
		return "", &ConsoleCommandError{Msg: err.Error()}
	}
	if err != nil {
		return "", err
	}
	if job == nil {
		return "", &ConsoleCommandError{Msg: "Job not found: " + ref}
	}
	return consoleFormatJob(job, "Resumed"), nil
}

// Schedules a job for the next scheduler tick
func consoleCronRun(e *ConsoleEngine, args []string) (string, error) {
	return consoleCronJobAction(args, "cron run <job>", "Triggered", func(ref string) (*cron.Job, error) {
		return cron.TriggerJob(e.ctx, home.Dir(e.ctx), ref)
	})
}
