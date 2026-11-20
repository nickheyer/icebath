package updaterecord

import (
	"context"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/pm"
)

const gatewayExitCodeName = ".update_exit_code"

// File the gateway update watcher polls for the finished update's exit status
func GatewayExitCodePath(h string) string { return filepath.Join(h, gatewayExitCodeName) }

// Publishes 0 or 1 atomically for the gateway watcher, a run already committed as success never reads failed
func WriteGatewayUpdateExitCode(ctx context.Context, ok bool) {
	ok = ok || CommittedSuccess(ctx)
	body := []byte("1")
	if ok {
		body = []byte("0")
	}
	_ = pm.DurableWriteBytes(GatewayExitCodePath(home.Dir(ctx)), body)
}
