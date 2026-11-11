package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	takeoverSHA                      = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	takeoverStderr         io.Writer = os.Stderr
	takeoverPrepare                  = prepare
	takeoverRunFinishChild           = runFinishChild
	takeoverGitExecutable            = func(ctx context.Context) string { return gitExecutable(ctx, "") }
	takeoverHeadSHA                  = headSHA
	takeoverRecordStep               = updaterecord.RecordStep
	takeoverFinalize                 = updaterecord.FinalizePendingUpdateReceipt
	takeoverPopen                    = PopenPostCommit
	takeoverAcquireLock              = acquireTakeoverLock
)

// Takes the checkout lock and the update marker of a root, with the holder that refused and the release call
func acquireTakeoverLock(ctx context.Context, root string) (bool, *updaterecord.UpdateHolder, func(), error) {
	lock := updaterecord.NewUpdateLock("", root, true)
	held, err := lock.Acquire(ctx)
	return held, lock.Holder, func() { lock.Release(ctx) }, err
}

func takeoverIsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func takeoverRemove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Provisions the new dependency graph and returns the managed interpreter with its activation environment
func prepare(ctx context.Context, request *pyjson.Object) (string, []string, error) {
	root := pyrepr.Str(request.GetOr("root", nil))
	ensurePanel(root)
	tools.ConvertTreelessCheckoutFirst(ctx, root)
	PublishStage("Updating Python dependencies (PM)")
	if err := RefuseForeignOwnedVenv(ctx, root); err != nil {
		return "", nil, err
	}
	if _, err := ArmCompletion(ctx, root); err != nil {
		return "", nil, err
	}
	if err := armFleetObligation(ctx, root); err != nil {
		return "", nil, err
	}
	correlation := pyrepr.Str(request.GetOr("update_id", nil))
	repairMarker := filepath.Join(pm.InstallStateDir(ctx, root), ".repair-incomplete")
	err := func() error {
		leave := pm.WorkerContext(ctx, &correlation)
		defer leave()
		if err := pm.EnsureToolsForSync(ctx); err != nil {
			return err
		}
		var extras []string
		if !takeoverIsFile(pm.EnvironmentRuntimeFactsPath(ctx, root)) {
			var err error
			if extras, err = pm.LegacySelection(ctx, root); err != nil {
				return err
			}
		}
		repair := false
		if takeoverIsFile(repairMarker) {
			var err error
			if repair, err = pm.VenvIsCurrent(ctx, pm.VenvIsCurrentOptions{ProjectRoot: root}); err != nil {
				return err
			}
		}
		opts := pm.SyncVenvOptions{Extras: extras, Explicit: true, ProjectRoot: root, Repair: repair, EvictIncompatiblePlugins: !repair}
		if repair {
			opts.Extras = nil
		} else if len(extras) > 1 {
			opts.Carried = extras[1:]
		}
		if err := pm.SyncVenv(ctx, opts); err != nil {
			return err
		}
		if err := pm.CollectSupersededGenerations(ctx, root); err != nil {
			return err
		}
		var receipt any
		if last := pm.LastForUpdate(ctx, correlation, false); last != nil {
			receipt = last
		}
		request.Set("pm_receipt", receipt)
		return nil
	}()
	if err != nil {
		return "", nil, err
	}
	if err := pm.PublishLaunchers(ctx, root, true); err != nil {
		return "", nil, err
	}
	for _, marker := range []string{repairMarker, filepath.Join(root, ".update-incomplete"), filepath.Join(root, ".lazy-refresh-incomplete")} {
		if err := takeoverRemove(marker); err != nil {
			return "", nil, err
		}
	}
	python := pm.ResolveInstallBinary(root)
	if python == "" {
		return "", nil, errors.New("updated installation has no managed interpreter")
	}
	env, err := pm.ActivationEnvironment(ctx, root)
	return python, env, err
}

// Owes the fleet restart for the moved tree under the head the checkout rests on
func armFleetObligation(ctx context.Context, root string) error {
	_, err := updaterecord.ArmHostObligation(ctx, filepath.Join(home.Dir(ctx), updaterecord.ProfileMarkerName), takeoverHeadSHA(ctx, root), nil, "", "")
	return err
}

// HEAD of a checkout from git, else from the ref files git itself reads, empty when neither names one
func headSHA(ctx context.Context, root string) string {
	head, err := pysubprocess.RunWith(ctx, []string{takeoverGitExecutable(ctx), "-C", root, "rev-parse", "HEAD"},
		pysubprocess.Options{Timeout: 60 * time.Second})
	if err == nil && head.Returncode == 0 {
		if sha := pystr.Strip(pystr.DecodeUTF8Replace(head.Stdout)); takeoverSHA.MatchString(sha) {
			return sha
		}
	}
	gitDir, err := pm.CheckoutGitDir(root)
	if err != nil {
		return ""
	}
	common, err := updaterecord.GitCommonDir(root)
	if err != nil || common == "" {
		common = gitDir
	}
	read := func(path string) (string, bool) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", false
		}
		text, err := pystr.DecodeUTF8Sig(raw)
		return text, err == nil
	}
	text, ok := read(filepath.Join(gitDir, "HEAD"))
	if !ok {
		return ""
	}
	sha := pystr.Strip(text)
	if strings.HasPrefix(sha, "ref:") {
		ref := pystr.Strip(strings.TrimPrefix(sha, "ref:"))
		loose := pypath.Join(common, ref)
		if takeoverIsFile(loose) {
			if text, ok = read(loose); !ok {
				return ""
			}
			sha = pystr.Strip(text)
		} else {
			if text, ok = read(filepath.Join(common, "packed-refs")); !ok {
				return ""
			}
			sha = ""
			for _, line := range pystr.Splitlines(text, false) {
				if strings.HasSuffix(line, " "+ref) {
					sha = strings.SplitN(line, " ", 2)[0]
					break
				}
			}
		}
	}
	if takeoverSHA.MatchString(sha) {
		return sha
	}
	return ""
}

// Publishes the gateway exit code through a temp file renamed into place
func publishGatewayExitCode(ctx context.Context, code int) error {
	marker := filepath.Join(home.Dir(ctx), ".update_exit_code")
	temporary := marker + ".tmp"
	if err := os.WriteFile(temporary, []byte(fmt.Sprintf("%d\n", code)), 0o666); err != nil {
		return err
	}
	return os.Rename(temporary, marker)
}

// Writes the acknowledgement the waiting updater reads
func takeoverAcknowledge(result string, receiptHandled bool) error {
	body := pyjson.NewObject()
	body.Set("receipt_handled", receiptHandled)
	body.Set("resume_handled", false)
	text, err := pyjson.Dumps(body)
	if err != nil {
		return err
	}
	return os.WriteFile(result, []byte(text), 0o666)
}

// Records a failed takeover on the receipt, the gateway exit code and the acknowledgement
func recordFailure(ctx context.Context, request *pyjson.Object, result string, code int, detail string) error {
	takeoverRecordStep(ctx, "historical_takeover", false, detail)
	saved := takeoverFinalize(ctx, &code, detail)
	if pyrepr.Truthy(request.GetOr("gateway_mode", nil)) {
		if err := publishGatewayExitCode(ctx, code); err != nil {
			return err
		}
	}
	return takeoverAcknowledge(result, saved != "")
}

// Finishes an update a historical updater handed off, returning the status the waiting updater reports
func CmdUpdateTakeover(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	contextPath, result := pyrepr.Str(ns["context"]), pyrepr.Str(ns["result"])
	raw, err := os.ReadFile(contextPath)
	if err != nil {
		return 0, err
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return 0, err
	}
	decoded, err := pyjson.Loads(text)
	if err != nil {
		return 0, err
	}
	request, ok := decoded.(*pyjson.Object)
	if !ok {
		return 0, fmt.Errorf("the takeover request %s is not an object", contextPath)
	}
	root := pyrepr.Str(request.GetOr("root", nil))
	if _, stopped := request.Get("stopped_serves"); stopped {
		python := pm.ResolveInstallBinary(root)
		if python == "" {
			fmt.Fprintln(takeoverStderr, "Cannot resume stopped backends: no selected Icebath interpreter")
			return 1, nil
		}
		env, err := pm.ActivationEnvironment(ctx, root)
		if err != nil {
			return 0, err
		}
		argv := updateServeResumeArgv(python, contextPath, result)
		child := exec.CommandContext(ctx, argv[0], argv[1:]...)
		child.Dir, child.Env = root, env
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, w, takeoverStderr
		return takeoverStatus(child.Run())
	}
	held, holder, release, err := takeoverAcquireLock(ctx, root)
	if err != nil {
		return 0, err
	}
	if !held {
		fmt.Fprintln(takeoverStderr, updaterecord.DescribeHolder(holder))
		return 2, nil
	}
	defer release()
	oldReceipt, _ := request.GetOr("receipt", nil).(*pyjson.Object)
	if oldReceipt == nil {
		oldReceipt = pyjson.NewObject()
	}
	previous := ""
	if id, ok := oldReceipt.GetOr("update_id", nil).(string); ok {
		previous = id
	}
	updaterecord.BeginUpdateReceipt(ctx, oldReceipt, previous)
	var correlation any
	if id := updaterecord.CurrentCorrelationID(ctx); id != nil {
		correlation = *id
	}
	request.Set("update_id", correlation)
	python, env, err := takeoverPrepare(ctx, request)
	if err == nil {
		var data any
		if current := updaterecord.CurrentReceipt(ctx); current != nil {
			data = current.Data
		}
		request.Set("receipt", data)
		var encoded string
		if encoded, err = pyjson.Dumps(request); err == nil {
			err = os.WriteFile(contextPath, []byte(encoded), 0o666)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		fmt.Fprintf(takeoverStderr, "Update preparation failed: %s\n", launchErrorText(err))
		if err := recordFailure(ctx, request, result, 1, "historical takeover preparation failed: "+launchErrorText(err)); err != nil {
			return 0, err
		}
		return 1, nil
	}
	code, err := takeoverRunFinishChild(ctx, request, contextPath, result, python, env)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		recordOwedFinish(ctx, request, result, fmt.Sprintf("the update finish child could not run (%s)", launchErrorText(err)))
		return 0, nil
	}
	return code, nil
}

// Exit status of a finished child, a failure that is not an exit status returned as the error
func takeoverStatus(err error) (int, error) {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return pysubprocess.Returncode(exit), nil
	}
	return 0, err
}

// Runs the finish child holding the checkout custody and records a failure it did not acknowledge
func runFinishChild(ctx context.Context, request *pyjson.Object, contextPath, result, python string, env []string) (int, error) {
	root := pyrepr.Str(request.GetOr("root", nil))
	command := updateFinishArgv(python, contextPath, result)
	child, err := takeoverPopen(ctx, command, "update finish child", ChildOptions{Dir: root, Env: env})
	if err != nil {
		return 0, err
	}
	finished := make(chan error, 1)
	go func() { finished <- child.Wait() }()
	var waitErr error
	select {
	case waitErr = <-finished:
	case <-ctx.Done():
		_ = child.Process.Kill()
		<-finished
		return 0, ctx.Err()
	}
	code, err := takeoverStatus(waitErr)
	if err != nil {
		return 0, err
	}
	if code != 0 && !takeoverIsFile(result) {
		if err := recordFailure(ctx, request, result, code, fmt.Sprintf("completion child exited %d without acknowledgement", code)); err != nil {
			return 0, err
		}
	}
	return code, nil
}

// Records a finish child that could not run as an owed follow up of an installed update, never failing
func recordOwedFinish(ctx context.Context, request *pyjson.Object, result, detail string) {
	fmt.Fprintf(takeoverStderr, "  ⚠ The update is installed, but its finishing steps did not run: %s. "+
		"The next `icebath` launch finishes them.\n", detail)
	saved := ""
	func() {
		defer func() { _ = recover() }()
		takeoverRecordStep(ctx, "historical_takeover", false, "owed: "+detail)
		zero := 0
		saved = takeoverFinalize(ctx, &zero, "committed; finishing steps owed: "+detail)
	}()
	if pyrepr.Truthy(request.GetOr("gateway_mode", nil)) {
		_ = publishGatewayExitCode(ctx, 0)
	}
	_ = takeoverAcknowledge(result, saved != "")
}
