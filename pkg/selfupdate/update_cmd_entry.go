package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Inputs of the update command, its argparse namespace, the checkout it updates and its output streams
type UpdateInvocation struct {
	Namespace      map[string]any
	ProjectRoot    string
	Stdout, Stderr io.Writer
}

// Installs the reminder row predicate and the manual serve retention every receipt finalize applies
func installServeObligationHooks() {
	updaterecord.SetServeObligationHooks(updaterecord.ServeObligationHooks{
		IsReminderRow: IsReminderRow,
		RetainReceiptManualServes: func(ctx context.Context, receipt map[string]any) []any {
			rows, err := RetainReceiptManualServes(ctx, receipt)
			if err != nil {
				panic(err)
			}
			return rows
		},
	})
}

func init() { installServeObligationHooks() }

// Directory names the bytecode sweep never descends into
var bytecodeSweepSkips = map[string]bool{"venv": true, ".venv": true, "node_modules": true, ".git": true, ".worktrees": true}

// Removes every __pycache__ directory under root and returns how many were removed
func clearBytecodeCache(root string) int {
	removed := 0
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		if filepath.Base(dir) == "__pycache__" {
			if os.RemoveAll(dir) == nil {
				removed++
			}
			return
		}
		for _, entry := range entries {
			if entry.IsDir() && !bytecodeSweepSkips[entry.Name()] {
				walk(filepath.Join(dir, entry.Name()))
			}
		}
	}
	walk(root)
	return removed
}

// Closes a still open receipt at the command boundary with the real exit code, never failing the exit
func finalizeUpdateReceipt(ctx context.Context, code int, reason string) {
	defer func() { _ = recover() }()
	updaterecord.FinalizePendingUpdateReceipt(ctx, &code, reason)
}

// Handles the metadata flags, managed refusal, plan, holder listing, admission gate and check, true when done
func updatePreflightHandled(ctx context.Context, w io.Writer, args map[string]any) (bool, error) {
	root := fleetProjectRoot()
	var setChannel *string
	if value := args["set_channel"]; value != nil {
		text := pyrepr.Str(value)
		setChannel = &text
	}
	handled, err := HandleMetadataArgs(ctx, w, pyrepr.Truthy(args["install_id"]), setChannel, root)
	if err != nil {
		return true, err
	}
	if handled {
		return true, updateExit(0)
	}
	h := home.Dir(ctx)
	if config.ManagedSystem(ctx, h) != "" {
		config.ManagedError(ctx, h, "update Icebath Agent")
		if !pyrepr.Truthy(args["plan"]) && !pyrepr.Truthy(args["check"]) && !pyrepr.Truthy(args["list_venv_holders"]) {
			recordStopWithoutReceipt(ctx, "managed_install", "refused")
		}
		return true, nil
	}
	if pyrepr.Truthy(args["plan"]) {
		printUpdatePlan(w, CollectRuntimeInventory(ctx))
		return true, nil
	}
	if pyrepr.Truthy(args["list_venv_holders"]) {
		holders := listVenvHolders(ctx)
		text, err := pyjson.Dumps(holders, pyjson.Indent(2))
		if err != nil {
			return true, err
		}
		fmt.Fprintln(w, text)
		if len(holders) > 0 {
			return true, updateExit(venvHoldersExit)
		}
		return true, nil
	}
	refusal, err := updateAdmission(ctx, root)
	if err != nil {
		return true, err
	}
	if refusal != nil {
		fmt.Fprintln(w, refusal.Message)
		recordRefusalReceipt(ctx, refusal)
		return true, updateExit(2)
	}
	if pyrepr.Truthy(args["check"]) {
		var channel *string
		if pyrepr.Truthy(args["channel"]) {
			text := pyrepr.Str(args["channel"])
			channel = &text
		}
		return true, cmdUpdateCheck(ctx, w, resolveUpdateBranch(args), pyrepr.Truthy(args["branch"]), channel)
	}
	return false, nil
}

// Calls of the command boundary the hermes tests replace, the retarget, preflight, output mirror and update lock
var (
	updateRetarget       = retargetToOwningInstall
	updatePreflight      = updatePreflightHandled
	updateInstallHangup  = gateway.InstallHangupProtection
	updateFinalizeOutput = gateway.FinalizeUpdateOutput
	updateAcquireLock    = acquireUpdateLock
	updateDescribeHolder = updaterecord.DescribeHolder
	updateRunImpl        = cmdUpdateImpl
)

var (
	_ updateLogMirror = (*gateway.UpdateOutputStream)(nil)
)

// Takes the cross process update lock of root, returning the holder when another update has it and the release call
func acquireUpdateLock(ctx context.Context, root string) (bool, *updaterecord.UpdateHolder, func(), error) {
	lock := updaterecord.NewUpdateLock("", root, true)
	acquired, err := lock.Acquire(ctx)
	return acquired, lock.Holder, func() { lock.Release(ctx) }, err
}

// Runs the update command and returns its exit status, an exit raised inside it becoming that status
func CmdUpdate(ctx context.Context, u UpdateInvocation) (int, error) {
	ctx = updaterecord.UpdateReceiptScope(ctx)
	if u.ProjectRoot != "" {
		saved := fleetProjectRoot
		fleetProjectRoot = func() string { return u.ProjectRoot }
		defer func() { fleetProjectRoot = saved }()
	}
	err := cmdUpdate(ctx, u)
	var exit *pm.SystemExit
	if errors.As(err, &exit) {
		return exit.Code, nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// Updates the install under hangup protection and the cross process update lock around the implementation
func cmdUpdate(ctx context.Context, u UpdateInvocation) error {
	args, root := u.Namespace, fleetProjectRoot()
	if err := updateRetarget(ctx, root); err != nil {
		return err
	}
	if handled, err := updatePreflight(ctx, u.Stdout, args); err != nil || handled {
		return err
	}
	gatewayMode := pyrepr.Truthy(args["gateway"])
	ioState := updateInstallHangup(u.Stdout, u.Stderr, gatewayMode)
	w := u.Stdout
	if ioState != nil {
		w = ioState.Stdout
	}
	acquired, holder, release, err := updateAcquireLock(ctx, root)
	if err != nil {
		return err
	}
	if !acquired {
		fmt.Fprintln(w, updateDescribeHolder(holder))
		updateFinalizeOutput(ioState)
		recordStopWithoutReceipt(ctx, "lock_held", "refused")
		return updateExit(updaterecord.UpdateExitConcurrent)
	}
	defer runUpdateExitHooks(u.Stderr)
	defer func() {
		release()
		updateFinalizeOutput(ioState)
	}()
	err = updateRunImpl(ctx, w, args, gatewayMode)
	var exit *pm.SystemExit
	switch {
	case err == nil:
		finalizeUpdateReceipt(ctx, 0, updaterecord.CommandBoundaryStopReason)
		return nil
	case errors.As(err, &exit):
		if exit.Code != 0 {
			if refusal := updaterecord.RefusalNotice("icebath update"); refusal != nil {
				fmt.Fprintln(w, *refusal)
			}
		}
		finalizeUpdateReceipt(ctx, exit.Code, fmt.Sprintf("sys.exit(%d)", exit.Code))
		if gatewayMode && exit.Code != 0 {
			updaterecord.WriteGatewayUpdateExitCode(ctx, false)
		}
		return err
	case isUpdateBoundaryError(err) && !isKeyboardInterrupt(err):
		if refusal := updaterecord.RefusalNotice("icebath update"); refusal != nil {
			fmt.Fprintln(w, *refusal)
		} else {
			fmt.Fprintf(w, "✗ Update failed: %s\n", launchErrorText(err))
		}
		finalizeUpdateReceipt(ctx, 1, launchErrorClass(err)+": "+launchErrorText(err))
		if gatewayMode {
			updaterecord.WriteGatewayUpdateExitCode(ctx, false)
		}
		return updateExit(1)
	default:
		if gatewayMode {
			updaterecord.WriteGatewayUpdateExitCode(ctx, false)
		}
		finalizeUpdateReceipt(ctx, 1, launchErrorClass(err)+": "+launchErrorText(err))
		return err
	}
}
