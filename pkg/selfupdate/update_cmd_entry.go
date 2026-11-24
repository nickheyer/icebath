//go:build !icebath_core && !icebath_noextras

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
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Inputs of the update command, its argparse namespace, the checkout it updates and its output streams
type UpdateInvocation struct {
	Namespace      map[string]any
	ProjectRoot    string
	Stdout, Stderr io.Writer
}

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
		text := anyutil.String(value)
		setChannel = &text
	}
	handled, err := HandleMetadataArgs(ctx, w, anyutil.IsTrue(args["install_id"]), setChannel, root)
	if err != nil {
		return true, err
	}
	if handled {
		return true, updateExit(0)
	}
	h := home.Dir(ctx)
	if config.ManagedSystem(ctx, h) != "" {
		config.ManagedError(ctx, h, "update Icebath Agent")
		return true, nil
	}
	if anyutil.IsTrue(args["plan"]) {
		printUpdatePlan(w, CollectRuntimeInventory(ctx))
		return true, nil
	}
	if anyutil.IsTrue(args["list_venv_holders"]) {
		holders := listVenvHolders(ctx)
		text, err := jsonutil.Encode(holders, jsonutil.Indent(2), jsonutil.ASCII())
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
	if anyutil.IsTrue(args["check"]) {
		var channel *string
		if anyutil.IsTrue(args["channel"]) {
			text := anyutil.String(args["channel"])
			channel = &text
		}
		return true, cmdUpdateCheck(ctx, w, resolveUpdateBranch(args), anyutil.IsTrue(args["branch"]), channel)
	}
	return false, nil
}

// Calls of the command boundary the tests replace, the retarget, preflight, output mirror and update lock
var (
	updateRetarget       = retargetToOwningInstall
	updatePreflight      = updatePreflightHandled
	updateInstallHangup  = gateway.InstallHangupProtection
	updateFinalizeOutput = gateway.FinalizeUpdateOutput
	updateAcquireLock    = acquireUpdateLock
	updateDescribeHolder = updaterecord.DescribeHolder
	updateRunImpl        = cmdUpdateImpl
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
	gatewayMode := anyutil.IsTrue(args["gateway"])
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
