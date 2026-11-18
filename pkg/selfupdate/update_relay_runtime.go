package selfupdate

import (
	"context"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/relayhost"
)

// Provisions the relay runtime the updated binary pins, replaced by tests
var maintProvisionRelayRuntime = relayhost.ProvisionRuntime

// Provisions the relay runtime after an update, a failure printed with what provisions it next
func provisionRelayRuntimeAfterUpdate(ctx context.Context, w io.Writer) error {
	err := maintProvisionRelayRuntime(ctx, w)
	if err == nil {
		return nil
	}
	next := "the next icebath start provisions it in the background"
	if !pm.LazyInstallsAllowedFor(ctx) {
		next = "run `icebath pm install` to provision it"
	}
	fmt.Fprintf(w, "  ⚠ %s; %s\n", err, next)
	return err
}
