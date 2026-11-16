package tuiserver

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
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
	return provider.PoolEnv{Store: provider.NewStore(h), Secrets: config.NewSecrets(h), Scope: provider.SecretScopeFrom(ctx),
		Config: provider.ConfigLoader(h), Ctx: ctx}
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
		_, found := pyshutil.Which(subject, nil)
		return found, nil
	case "nous.managed_tools":
		managed := tools.ManagedNousToolsEnabled(&tools.Env{Home: h, Config: cfg, Secrets: config.NewSecrets(h)}, false)
		return managed && provider.GetNousSubscriptionFeatures(setupPoolEnv(ctx, h), map[string]any(cfg), false).NousAuthPresent, nil
	case "xai.oauth":
		return pyrepr.Truthy(provider.GetXAIOAuthAuthStatus(setupPoolEnv(ctx, h))["logged_in"]), nil
	}
	return false, fmt.Errorf("unknown setup fact %s", name)
}

// Adds a speech engine to the dependency union of this install, printing the outcome
func setupInstallExtra(ctx context.Context, extra string) bool {
	if err := pm.SyncVenv(ctx, pm.SyncVenvOptions{Extras: []string{extra}, Explicit: true}); err != nil {
		detail := err.Error()
		if pypath.IsOSError(err) {
			detail = pypath.OSErrorText(err)
		}
		provider.SetupPrintError(fmt.Sprintf("Failed to install %s: %s", extra, detail))
		provider.SetupPrintInfo("Retry with /setup tts")
		return false
	}
	provider.SetupPrintSuccess(extra + " installed. Restart Icebath to use it.")
	return true
}

// Side effects a setup field can start, each printing through the setup output and reporting success
var setupActions = map[string]func(ctx context.Context, h string) bool{
	"xai.oauth.login": func(ctx context.Context, h string) bool {
		return provider.RunXAIOAuthLoginFromSetup(setupPoolEnv(ctx, h))
	},
	"tts.install.neutts": func(ctx context.Context, _ string) bool {
		provider.SetupPrintInfo("Installing neutts Python package...")
		provider.SetupPrintInfo("This will also download the TTS model (~300MB) on first use.")
		return setupInstallExtra(ctx, "neutts")
	},
	"tts.install.kittentts": func(ctx context.Context, _ string) bool {
		provider.SetupPrintInfo("Installing kittentts Python package (~25-80MB model downloaded on first use)...")
		return setupInstallExtra(ctx, "kittentts")
	},
}

var setupAnsiCodes = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// One running setup action, the lines it printed and its outcome once it ended
type setupJob struct {
	mu      sync.Mutex
	partial string
	lines   []any
	done    bool
	ok      bool
}

var (
	setupJobsMu   sync.Mutex
	setupJobs     = map[string]*setupJob{}
	setupJobCount int
	setupActionMu sync.Mutex
)

// Splits action output into lines without terminal styling, holding back an unfinished line
func (j *setupJob) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.partial += string(p)
	for {
		line, rest, whole := strings.Cut(j.partial, "\n")
		if !whole {
			return len(p), nil
		}
		j.partial = rest
		if text := strings.TrimSpace(setupAnsiCodes.ReplaceAllString(line, "")); text != "" {
			j.lines = append(j.lines, text)
		}
	}
}

// Ends the job with its outcome, keeping an unfinished last line
func (j *setupJob) finish(outcome bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if text := strings.TrimSpace(setupAnsiCodes.ReplaceAllString(j.partial, "")); text != "" {
		j.lines = append(j.lines, text)
	}
	j.partial, j.done, j.ok = "", true, outcome
}

// Lines printed past the count the caller already has, with the end state
func (j *setupJob) since(after int) ([]any, bool, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if after < 0 || after > len(j.lines) {
		after = len(j.lines)
	}
	return append([]any{}, j.lines[after:]...), j.done, j.ok
}

// Runs one action at a time with the setup output captured, a panic ending the job as failed
func (j *setupJob) run(ctx context.Context, h string, action func(ctx context.Context, h string) bool) {
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
	outcome = action(ctx, h)
}

// Starts a named setup side effect in the background and returns the job that follows it
func setupActionStart(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	h := home.Dir(ctx)
	name := strOr(params["action"])
	action, known := setupActions[name]
	if !known {
		return rpcErr(rid, 4001, "unknown setup action "+name, nil), nil
	}
	if config.IsManaged(ctx, h) {
		return rpcErr(rid, 4006, "managed install — the configuration is read-only", nil), nil
	}
	job := &setupJob{}
	setupJobsMu.Lock()
	setupJobCount++
	id := "setup-" + strconv.Itoa(setupJobCount)
	setupJobs[id] = job
	setupJobsMu.Unlock()
	go job.run(context.WithoutCancel(ctx), h, action)
	return ok(rid, map[string]any{"job": id}), nil
}

// Waits a short while for new output of a job and returns it, forgetting the job once its end is delivered
func setupActionPoll(ctx context.Context, rid any, params map[string]any) (map[string]any, error) {
	id := strOr(params["job"])
	setupJobsMu.Lock()
	job, known := setupJobs[id]
	setupJobsMu.Unlock()
	after, fits := sectionWriteValue("whole", params["after"])
	if !known || !fits {
		return rpcErr(rid, 4001, "unknown setup job "+id, nil), nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		lines, done, outcome := job.since(after.(int))
		if len(lines) > 0 || done || time.Now().After(deadline) || ctx.Err() != nil {
			if done {
				setupJobsMu.Lock()
				delete(setupJobs, id)
				setupJobsMu.Unlock()
			}
			return ok(rid, map[string]any{"lines": lines, "done": done, "ok": outcome}), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}
