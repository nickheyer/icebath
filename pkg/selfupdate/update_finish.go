package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Hidden entry finishing an update a historical takeover prepared in the selected checkout
const UpdateFinishEntry = "update-finish"

// Runs the CLI main of this binary over an argv in process and returns its exit status, assigned at startup
var RunCLIHook func(argv []string) int

// Steps of a historical finish, each the host behaviour unless a hermes test replaces it
var (
	finishBuildProducts    = BuildUpdateProducts
	finishMaintenance      = RunPostUpdateMaintenance
	finishRestartFleet     = restartGatewayFleetAfterUpdate
	finishMergeResume      = resumeWindowsGatewaysAndMergeOutcome
	finishVerifyFleet      = verifyFleetAfterUpdate
	finishResumeWindows    = resumeWindowsGatewaysAfterUpdate
	finishWriteSourceStamp = WriteSourceStamp
)

// Command line running the finish child of a checkout binary on a request and result file pair
func updateFinishArgv(binary, contextPath, resultPath string) []string {
	return []string{binary, UpdateFinishEntry, contextPath, resultPath}
}

// str of an exception, its class name when the text is empty
func pyReason(err error) string {
	if text := launchErrorText(err); text != "" {
		return text
	}
	return launchErrorClass(err)
}

// True for an operator interrupt that a Python except Exception clause lets through
func isInterrupt(err error) bool {
	return errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, context.Canceled)
}

// Optional text of a request value, nil for None
func optionalText(value any) *string {
	if value == nil {
		return nil
	}
	text := pyrepr.Str(value)
	return &text
}

// Inputs of finishing a selected checkout after its code is committed
type finishUpdateOptions struct {
	Root                      string
	AssumeYes, GatewayMode    bool
	PreUpdateSnapshotID       *string
	HadDesktopAppBeforeUpdate bool
	PreUpdateVersion          *string
	Plan                      *UpdatePlan
	WindowsResume             map[string]any
	Followups                 *[][2]string
}

// Runs maintenance, the stamp and the fleet restart of a committed checkout, every failure owed and never fatal
func finishUpdate(ctx context.Context, w, errw io.Writer, o finishUpdateOptions) error {
	owed := o.Followups
	if owed == nil {
		owed = &[][2]string{}
	}
	runtimeSafe := false
	safe, err := finishMaintenance(ctx, PostUpdateMaintenance{
		AssumeYes: o.AssumeYes, GatewayMode: o.GatewayMode, PreUpdateSnapshotID: o.PreUpdateSnapshotID,
		HadDesktopAppBeforeUpdate: o.HadDesktopAppBeforeUpdate, PreUpdateVersion: o.PreUpdateVersion,
		Followups: owed, Stdout: w, Stderr: errw})
	if err != nil {
		if isInterrupt(err) {
			return err
		}
		reason := pyReason(err)
		updaterecord.RecordFollowup(ctx, "maintenance", reason, updaterecord.FollowupRetryDefault)
		*owed = append(*owed, [2]string{"maintenance", reason})
	} else {
		runtimeSafe = safe
	}
	tailOwed := false
	for _, step := range *owed {
		tailOwed = tailOwed || updaterecord.TailFollowups[step[0]]
	}
	updaterecord.RecordBuildStage(ctx, *owed)
	if !tailOwed {
		if _, err := finishWriteSourceStamp(o.Root, false); err != nil {
			if class := launchErrorClass(err); !pypath.IsOSError(err) && !osErrorClasses[class] && class != "ValueError" {
				return err
			}
			fmt.Fprintf(errw, "⚠ Source update completed, but the install stamp could not be written: %s\n", launchErrorText(err))
		}
		if err := ClearCompletion(ctx, o.Root); err != nil {
			return err
		}
	}
	if o.GatewayMode {
		updaterecord.WriteGatewayUpdateExitCode(ctx, true)
	}
	var updateID *string
	if run := updaterecord.CurrentReceipt(ctx); run != nil {
		if id, ok := run.Data.GetOr("update_id", nil).(string); ok {
			updateID = &id
		}
	}
	err = func() error {
		restarted, err := finishRestartFleet(ctx, w, o.Plan, o.GatewayMode)
		if err != nil {
			return err
		}
		finishMergeResume(ctx, w, restarted, o.WindowsResume, o.GatewayMode)
		var resume any
		if o.WindowsResume != nil {
			resume = o.WindowsResume
		}
		return finishVerifyFleet(ctx, w, restarted, o.Plan, resume, runtimeSafe && !tailOwed)
	}()
	if err != nil {
		if isInterrupt(err) {
			return err
		}
		return updaterecord.OweFollowup(ctx, updateID, "gateway_restart", pyReason(err), updaterecord.FollowupRetryDefault)
	}
	return nil
}

// Update plan of a finish request, nil when the request carries none
func restorePlan(data any) (*UpdatePlan, error) {
	if !pyrepr.Truthy(data) {
		return nil, nil
	}
	return UpdatePlanFromDict(data)
}

// Windows resume token of a request as the mutable map every resume step shares, nil for None
func requestResumeToken(value any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	if obj, ok := value.(*pyjson.Object); ok {
		return pyjson.Plain(obj).(map[string]any), nil
	}
	if m, ok := value.(map[string]any); ok {
		return m, nil
	}
	return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(value))
}

// Finishes a historical takeover in a fresh process of the selected checkout and writes its acknowledgement
func UpdateFinishMain(ctx context.Context, contextPath, resultPath string, stdout, stderr io.Writer) int {
	code, err := updateFinish(ctx, contextPath, resultPath, stdout, stderr)
	if err != nil {
		if isInterrupt(err) {
			return 130
		}
		fmt.Fprintf(stderr, "%s: %s\n", launchErrorClass(err), launchErrorText(err))
		return 1
	}
	return code
}

// Body of the finish child, the error an uncaught Python exception stands for
func updateFinish(ctx context.Context, contextPath, resultPath string, stdout, stderr io.Writer) (int, error) {
	decoded, err := readRequestFile(contextPath)
	if err != nil {
		return 0, err
	}
	rootValue, err := pyDictItem(decoded, "root")
	if err != nil {
		return 0, err
	}
	request := decoded.(*pyjson.Object)
	root := pyrepr.Str(rootValue)
	token, err := requestResumeToken(request.GetOr("windows_resume", nil))
	if err != nil {
		return 0, err
	}
	resumeArmed := false
	restarting := pyrepr.Truthy(request.GetOr("restart_update", false))
	cliStarted, begun := false, false
	code := 1
	beginFromRequest := func() error {
		id, err := pyDictItem(request, "update_id")
		if err != nil {
			return err
		}
		previous, _ := request.GetOr("receipt", nil).(*pyjson.Object)
		updaterecord.BeginUpdateReceipt(ctx, previous, pyrepr.Str(id))
		begun = updaterecord.CurrentReceipt(ctx) != nil
		return nil
	}
	body := func() error {
		if !restarting {
			if err := beginFromRequest(); err != nil {
				return err
			}
			if !begun {
				return runtimeErr("cannot create takeover receipt")
			}
			id := pyrepr.Str(request.GetOr("update_id", nil))
			if err := pm.AcceptWorkerReceipt(ctx, request.GetOr("pm_receipt", nil), &id); err != nil {
				return err
			}
			updaterecord.RecordStep(ctx, "historical_takeover", true, "continuing in the selected interpreter")
		}
		saved := fleetProjectRoot
		fleetProjectRoot = func() string { return root }
		defer func() { fleetProjectRoot = saved }()
		if restarting {
			argv, err := pyIterable(request.GetOr("argv", nil))
			if err != nil {
				return err
			}
			args := make([]string, 0, len(argv))
			for _, arg := range argv {
				args = append(args, pyrepr.Str(arg))
			}
			if len(args) > 0 {
				args = args[1:]
			}
			cliStarted = true
			if status := RunCLIHook(args); status != 0 {
				return &pm.SystemExit{Code: status}
			}
			return nil
		}
		plan, err := restorePlan(request.GetOr("plan", nil))
		if err != nil {
			return err
		}
		lock := updaterecord.NewUpdateLock("", root, true)
		acquired, err := lock.Acquire(ctx)
		if err != nil {
			return err
		}
		defer lock.Release(ctx)
		if !acquired && lock.Holder != nil {
			fmt.Fprintln(stderr, updaterecord.DescribeHolder(lock.Holder))
			return &pm.SystemExit{Code: 2}
		}
		resumeArmed = true
		desktopValue := request.GetOr("desktop", nil)
		desktop := pyrepr.Truthy(desktopValue)
		if desktopValue == nil {
			desktopDir := filepath.Join(root, "apps", "desktop")
			_, packaged := desktopPackagedExecutable(desktopDir)
			desktop = packaged || desktopDistExists(desktopDir)
		}
		if _, err := ArmCompletion(ctx, root); err != nil {
			if !pypath.IsOSError(err) {
				return err
			}
			fmt.Fprintf(stdout, "  ⚠ Could not record the owed source-update tail: %s\n", launchErrorText(err))
		}
		owed := [][2]string{}
		if err := finishBuildProducts(ctx, root, desktop); err != nil {
			if isInterrupt(err) {
				return err
			}
			reason := pyReason(err)
			updaterecord.RecordFollowup(ctx, "build", reason, updaterecord.FollowupRetryDefault)
			owed = append(owed, [2]string{"build", reason})
		}
		assumeYes, err := pyDictItem(request, "assume_yes")
		if err != nil {
			return err
		}
		gatewayMode, err := pyDictItem(request, "gateway_mode")
		if err != nil {
			return err
		}
		return finishUpdate(ctx, stdout, stderr, finishUpdateOptions{
			Root: root, AssumeYes: pyrepr.Truthy(assumeYes), GatewayMode: pyrepr.Truthy(gatewayMode),
			PreUpdateSnapshotID:       optionalText(request.GetOr("pre_update_snapshot_id", nil)),
			HadDesktopAppBeforeUpdate: desktop, PreUpdateVersion: optionalText(request.GetOr("pre_update_version", nil)),
			Plan: plan, WindowsResume: token, Followups: &owed})
	}
	interrupted := error(nil)
	if err := body(); err == nil {
		code = 0
	} else {
		var exit *pm.SystemExit
		switch {
		case errors.As(err, &exit):
			code = exit.Code
		case isInterrupt(err):
			interrupted = err
		default:
			updaterecord.RecordStep(ctx, "historical_completion", false, launchErrorText(err))
			fmt.Fprintf(stderr, "Update completion failed: %s\n", launchErrorText(err))
		}
	}
	idValue, err := pyDictItem(request, "update_id")
	if err != nil {
		return 0, err
	}
	updateID := pyrepr.Str(idValue)
	terminal := updaterecord.FinalizedReceipt(updateID)
	if code != 0 && pyrepr.Truthy(request.GetOr("gateway_mode", nil)) {
		body := []byte("1")
		if terminal != nil && terminal.GetOr("outcome", nil) == "success" {
			body = []byte("0")
		}
		if err := pm.DurableWriteBytes(filepath.Join(home.Dir(ctx), ".update_exit_code"), body); err != nil {
			return 0, err
		}
	}
	if restarting && !cliStarted {
		if err := beginFromRequest(); err != nil {
			return 0, err
		}
	}
	attempted := false
	if resumeArmed {
		if err := finishResumeWindows(ctx, stdout, token); err != nil {
			if isInterrupt(err) {
				return 0, err
			}
			attempted = true
			if err := updaterecord.OweFollowup(ctx, &updateID, "windows_resume", "Windows gateway recovery failed: "+launchErrorText(err), updaterecord.FollowupRetryDefault); err != nil {
				return 0, err
			}
		}
	}
	handled := cliStarted || (begun && updaterecord.CurrentReceipt(ctx) == nil)
	written := updaterecord.FinalizePendingUpdateReceipt(ctx, &code, "historical takeover completion")
	result := pyjson.ObjectOf(
		"resume_handled", attempted || len(token) == 0 || !pyrepr.Truthy(token["resume_needed"]),
		"receipt_handled", handled || written != "")
	text, err := pyjson.Dumps(result)
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(resultPath, []byte(text), 0o666); err != nil {
		return 0, err
	}
	if interrupted != nil {
		return 0, interrupted
	}
	return code, nil
}
