package tuiserver

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

var setupActionRegistry = &handlerRegistry{}

func init() {
	setupActionRegistry.scopedMethod("setup.action.start", catch(5042, setupActionStart))
	setupActionRegistry.scopedMethod("setup.action.poll", catch(5043, setupActionPoll))
	setupActionRegistry.install()
	addLongHandlers("setup.action.poll")
}

// Credential environment of a home so logins and status reads follow the profile
func setupPoolEnv(ctx context.Context, h string) provider.PoolEnv {
	return provider.PoolEnv{Store: provider.NewStore(h), Secrets: config.NewSecrets(h), Scope: provider.SecretScopeFrom(ctx), Ctx: ctx}
}

// Truth of one named fact about the home or the host, a prefixed name taking its subject after the colon
func setupFact(ctx context.Context, h string, cfg config.Tree, name string) (bool, error) {
	kind, subject, _ := strings.Cut(name, ":")
	switch kind {
	case "env":
		value, _, err := config.GetEnvValue(ctx, h, subject)
		return value != "", err
	case "extra":
		return pm.ExtraSupported(ctx, subject)
	case "module":
		return provider.SetupModuleInstalled(ctx, subject), nil
	case "binary":
		_, lookErr := exec.LookPath(subject)
		return lookErr == nil, nil
	case "xai.oauth":
		return anyutil.IsTrue(provider.GetXAIOAuthAuthStatus(setupPoolEnv(ctx, h))["logged_in"]), nil
	}
	return false, fmt.Errorf("unknown setup fact %s", name)
}

var setupAnsiCodes = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// One running setup action, the lines it printed, its outcome once it ended and the transport that started it
type setupJob struct {
	mu      sync.Mutex
	partial string
	lines   []any
	done    bool
	ok      bool
	owner   tuihost.Transport
	cancel  context.CancelFunc
}

var (
	setupJobsMu   sync.Mutex
	setupJobs     = map[string]*setupJob{}
	setupJobCount int
	setupActionMu sync.Mutex
)

// Reports a terminal so installers redraw their progress in place instead of printing a line per step
func (j *setupJob) IsTerminal() bool { return true }

// Visible text of a line a carriage return redraws, without terminal styling
func setupLineText(line string) string {
	if at := strings.LastIndexByte(line, '\r'); at >= 0 {
		line = line[at+1:]
	}
	return strings.TrimSpace(setupAnsiCodes.ReplaceAllString(line, ""))
}

// Splits action output into finished lines, holding back the line being redrawn
func (j *setupJob) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.partial += string(p)
	for {
		line, rest, whole := strings.Cut(j.partial, "\n")
		if !whole {
			if at := strings.LastIndexByte(j.partial, '\r'); at >= 0 {
				j.partial = j.partial[at:]
			}
			return len(p), nil
		}
		j.partial = rest
		if text := setupLineText(line); text != "" {
			j.lines = append(j.lines, text)
		}
	}
}

// Ends the job with its outcome, keeping an unfinished last line
func (j *setupJob) finish(outcome bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if text := setupLineText(j.partial); text != "" {
		j.lines = append(j.lines, text)
	}
	j.partial, j.done, j.ok = "", true, outcome
}

// Lines printed past the count the caller already has, the line being redrawn and the end state
func (j *setupJob) since(after int) ([]any, string, bool, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if after < 0 || after > len(j.lines) {
		after = len(j.lines)
	}
	return append([]any{}, j.lines[after:]...), setupLineText(j.partial), j.done, j.ok
}

// Runs one action at a time printing to the job, a panic ending the job as failed
func (j *setupJob) run(ctx context.Context, h string, action func(ctx context.Context, h string, out io.Writer) bool) {
	setupActionMu.Lock()
	defer setupActionMu.Unlock()
	restore := provider.WithSetupOutput(j)
	outcome := false
	defer func() {
		if failure := recover(); failure != nil {
			fmt.Fprintf(j, "%v\n", failure)
		}
		restore()
		j.finish(outcome)
	}()
	outcome = action(ctx, h, j)
}

// Starts a named setup side effect in the background and returns the job that follows it
func setupActionStart(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	p, err := tuigateway.SetupActionStart.Params(params)
	if err != nil {
		return rpcErr(rid, 4001, err.Error(), nil), nil
	}
	h := home.Dir(ctx)
	action, known := setupActionFor(string(p.Action), p.Params)
	if !known {
		return rpcErr(rid, 4001, "unknown setup action "+string(p.Action), nil), nil
	}
	if config.IsManaged(ctx, h) {
		return rpcErr(rid, 4006, "managed install — the configuration is read-only", nil), nil
	}
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	job := &setupJob{owner: tuihost.CurrentTransport(ctx), cancel: cancel}
	setupJobsMu.Lock()
	setupJobCount++
	id := "setup-" + strconv.Itoa(setupJobCount)
	setupJobs[id] = job
	setupJobsMu.Unlock()
	go job.run(jobCtx, h, action)
	return ok(rid, map[string]any{"job": id}), nil
}

// Cancels and forgets the setup actions a departing transport started
func releaseSetupForTransport(transport tuihost.Transport) {
	if transport == nil {
		return
	}
	setupJobsMu.Lock()
	for id, job := range setupJobs {
		if job.owner == transport {
			job.cancel()
			delete(setupJobs, id)
		}
	}
	setupJobsMu.Unlock()
}

// Waits a short while for new output of a job and returns it, forgetting the job once its end is delivered
func setupActionPoll(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	p, err := tuigateway.SetupActionPoll.Params(params)
	if err != nil {
		return rpcErr(rid, 4001, err.Error(), nil), nil
	}
	id := p.Job
	setupJobsMu.Lock()
	job, known := setupJobs[id]
	setupJobsMu.Unlock()
	if !known || p.After < 0 {
		return rpcErr(rid, 4001, "unknown setup job "+id, nil), nil
	}
	shown, seen := p.Live != nil, ""
	if shown {
		seen = *p.Live
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		lines, live, done, outcome := job.since(int(p.After))
		if len(lines) > 0 || done || shown && live != seen || time.Now().After(deadline) || ctx.Err() != nil {
			if done {
				setupJobsMu.Lock()
				delete(setupJobs, id)
				setupJobsMu.Unlock()
			}
			return ok(rid, map[string]any{"lines": lines, "live": live, "done": done, "ok": outcome}), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}
