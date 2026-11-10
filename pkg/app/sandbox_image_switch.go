package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Asks once at TTY startup whether to switch the sandbox image, a failure logged at debug like the startup guard
func offerSandboxImageSwitch(c *CLI, print func(string)) {
	con := console()
	if _, err := offerSandboxImageSwitchWith(c.ctx, c.env, print, con.Input, con.Out); err != nil {
		cliLogger.Debug(fmt.Sprintf("sandbox image switch offer failed: %v", err))
	}
}

// Decision of the startup offer, nil when nothing was pending or the answer deferred it
func offerSandboxImageSwitchWith(ctx context.Context, env *tools.Env, cprint func(string), ask func(string) (string, error), stdout io.Writer) (*bool, error) {
	sw, err := tools.PendingSandboxImageSwitch(ctx, env)
	if err != nil || sw == nil {
		return nil, err
	}
	cprint("")
	cprint("☤ A new default sandbox image is available.")
	for _, line := range tools.SandboxImageSwitchExplainLines(sw) {
		cprint("  " + line)
	}
	answer, err := ask("  Switch now? [y = switch / n = keep the current image / Enter = ask later]: ")
	if err != nil {
		if errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, cliui.ErrEOF) {
			_, _ = fmt.Fprintln(stdout)
			return nil, nil
		}
		return nil, err
	}
	switch pystr.Lower(pystr.Strip(answer)) {
	case "y", "yes":
		if _, err := tools.DecideSandboxImageSwitch(ctx, env, sw, true); err != nil {
			return nil, err
		}
		cprint("  ✓ terminal.docker_image = " + sw.TargetImage + "; the sandbox is recreated on its next use.")
		approved := true
		return &approved, nil
	case "n", "no":
		if _, err := tools.DecideSandboxImageSwitch(ctx, env, sw, false); err != nil {
			return nil, err
		}
		cprint("  ✓ terminal.docker_image = " + sw.CurrentImage + "; unset it in config.yaml to be asked again.")
		kept := false
		return &kept, nil
	}
	cprint("  Later. `icebath config set terminal.docker_image <image>` decides it any time.")
	return nil, nil
}
