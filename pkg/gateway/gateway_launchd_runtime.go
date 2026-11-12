package gateway

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/internal/pyplist"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Label prefix of one off runtime jobs, never the installed ai.icebath service prefix fleet scans read
const launchdRuntimeLabelPrefix = "ai.icebath.runtime."

const launchdRuntimePollInterval = 0.5

var (
	launchdRuntimePidRe      = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
	launchdRuntimeExitCodeRe = regexp.MustCompile(`(?m)^\s*last exit code = (-?\d+)`)
	launchdRuntimeRun        = func(ctx context.Context, timeout time.Duration, argv ...string) (CommandResult, error) {
		return RunServiceCommand(ctx, timeout, argv...)
	}
	launchdRuntimeGetuid   = os.Getuid
	launchdRuntimeNewLabel = func() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }
)

// Seconds up to limit that stay inside the caller deadline, zero once it has passed
func launchdRuntimeBudget(deadline, limit float64) float64 {
	return max(0, min(limit, deadline-pythread.Monotonic()))
}

// Captured launchctl run, false when the budget is spent or the command fails to run or times out
func launchdRuntimeLaunchctl(ctx context.Context, timeout float64, args ...string) (CommandResult, bool) {
	if timeout <= 0 {
		return CommandResult{}, false
	}
	res, err := launchdRuntimeRun(ctx, time.Duration(timeout*float64(time.Second)), append([]string{"launchctl"}, args...)...)
	return res, err == nil
}

// Poll and pid view of a launched runtime job that the ensure loop reads
type launchdRuntimeJob struct {
	ctx      context.Context
	target   string
	deadline float64
	pid      int
	status   *int
	checked  float64
}

// Exit status once launchd records one, polling launchctl print at most every half second
func (j *launchdRuntimeJob) Poll() (int, bool) {
	if j.status != nil {
		return *j.status, true
	}
	if pythread.Monotonic()-j.checked < launchdRuntimePollInterval {
		return 0, false
	}
	j.checked = pythread.Monotonic()
	done, ok := launchdRuntimeLaunchctl(j.ctx, launchdRuntimeBudget(j.deadline, 5), "print", j.target)
	if !ok {
		return 0, false
	}
	if done.ReturnCode != 0 {
		status := 1
		j.status = &status
		return status, true
	}
	if m := launchdRuntimePidRe.FindStringSubmatch(done.Stdout); m != nil {
		if wrapper, err := strconv.Atoi(m[1]); err == nil {
			j.recordGatewayPid(wrapper)
		}
		return 0, false
	}
	if m := launchdRuntimeExitCodeRe.FindStringSubmatch(done.Stdout); m != nil {
		if status, err := strconv.Atoi(m[1]); err == nil {
			j.status = &status
			return status, true
		}
	}
	return 0, false
}

// Gateway pid recorded for the job, zero before launchd reports one
func (j *launchdRuntimeJob) Pid() int { return j.pid }

// Records the osascript wrapper's only child as the gateway pid, the wrapper itself when pgrep finds none
func (j *launchdRuntimeJob) recordGatewayPid(wrapper int) {
	timeout := launchdRuntimeBudget(j.deadline, 5)
	if (j.pid != 0 && j.pid != wrapper) || timeout == 0 {
		return
	}
	var out []string
	if res, err := launchdRuntimeRun(j.ctx, time.Duration(timeout*float64(time.Second)), "pgrep", "-P", strconv.Itoa(wrapper)); err == nil {
		out = strings.Fields(res.Stdout)
	}
	j.pid = wrapper
	if len(out) > 0 {
		if child, err := strconv.Atoi(out[0]); err == nil {
			j.pid = child
		}
	}
}

// Unloads earlier runtime jobs whose gateway exited, never touching a running one
func removeFinishedJobs(ctx context.Context, deadline float64) {
	listed, ok := launchdRuntimeLaunchctl(ctx, launchdRuntimeBudget(deadline, 10), "list")
	if !ok || listed.ReturnCode != 0 {
		return
	}
	for _, line := range strings.Split(strings.ReplaceAll(listed.Stdout, "\r\n", "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "-" && strings.HasPrefix(fields[2], launchdRuntimeLabelPrefix) {
			launchdRuntimeLaunchctl(ctx, launchdRuntimeBudget(deadline, 10), "remove", fields[2])
		}
	}
}

// Runs command as a one off job in the user's GUI launchd domain, nil when launchd cannot take it
func startGatewayJob(ctx context.Context, command []string, env map[string]string, cwd, logPath string, deadline float64) (SpawnedGateway, error) {
	removeFinishedJobs(ctx, deadline)
	label := launchdRuntimeLabelPrefix + launchdRuntimeNewLabel()
	domain := "gui/" + strconv.Itoa(launchdRuntimeGetuid())
	wrapped := append([]string{"/usr/bin/env", "XPC_SERVICE_NAME=0"}, command...)
	plist := filepath.Join(filepath.Dir(logPath), "."+label+".plist")
	envVars := make(map[string]string, len(env))
	for k, v := range env {
		envVars[k] = v
	}
	payload, dumpErr := pyplist.DumpXML(map[string]any{
		"Label":                label,
		"ProgramArguments":     LaunchdProgramArguments(wrapped, logPath, logPath),
		"EnvironmentVariables": envVars,
		"WorkingDirectory":     cwd,
		"RunAtLoad":            true,
	})
	handle, err := os.OpenFile(plist, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer os.Remove(plist)
	if dumpErr != nil {
		handle.Close()
		return nil, dumpErr
	}
	_, err = handle.Write(payload)
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	done, ok := launchdRuntimeLaunchctl(ctx, launchdRuntimeBudget(deadline, 10), "bootstrap", domain, plist)
	if !ok || done.ReturnCode != 0 {
		return nil, nil
	}
	return &launchdRuntimeJob{ctx: ctx, target: domain + "/" + label, deadline: deadline}, nil
}
