//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Brings the relay runtime to this version's pins with progress once Relay is switched on, leaving it absent otherwise
func refreshSwitchedOnRelayRuntime(ctx context.Context, w io.Writer) error {
	target, err := pm.CurrentTarget()
	if err != nil {
		return err
	}
	if _, ok := pm.NemoRelayWheel(target); !ok {
		return nil
	}
	venv, ready, err := pm.RuntimeEnvironmentReady(ctx)
	if err != nil || ready {
		return err
	}
	if _, err := os.Stat(pm.VenvPython(venv)); err != nil && !tools.RelayDemanded(ctx) {
		return nil
	}
	fmt.Fprintln(w)
	if err := tools.ProvisionNemoRelay(ctx, w); err != nil {
		fmt.Fprintf(w, "  ⚠ %s\n    Run `icebath pm install --extra nemo-relay` once the cause is fixed\n", err)
	}
	return nil
}
