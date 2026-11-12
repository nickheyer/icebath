package gateway

import (
	"context"
	"net/http"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Owner the message handler was bound from, the runner of a bound method and nil for a closure
func (b *BasePlatformAdapter) MessageHandlerOwner() any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.messageHandlerOwner
}

// Whether the runner installed a platform bound authorization callback
func (b *BasePlatformAdapter) HasAuthorizationCheck() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.authorizationCheck != nil
}

// Float from config extra clamped to the bounds, a non finite, negative or unparseable value reading as the default
func (b *BasePlatformAdapter) CoerceFloatExtra(key string, def, minValue float64, maxValue *float64) float64 {
	return b.coerceFloatExtra(key, def, minValue, maxValue)
}

// Allowlist entries of a YAML list, a JSON list literal or a comma separated scalar
func CoerceAllowSet(raw any) map[string]bool {
	out := map[string]bool{}
	if raw == nil {
		return out
	}
	raw = DecodeJSONListLiteral(raw)
	if items, ok := raw.([]any); ok {
		for _, item := range items {
			if part := pystr.Strip(pyrepr.Str(item)); part != "" {
				out[part] = true
			}
		}
		return out
	}
	for _, part := range strings.Split(pyrepr.Str(raw), ",") {
		if part = pystr.Strip(part); part != "" {
			out[part] = true
		}
	}
	return out
}

// Whether the line starts with a markdown table delimiter row
func IsTableSeparatorLine(line string) bool {
	return pyreMatch(TableSeparatorRE, line)
}

// Requests cancellation of the task, the asyncio Task cancel of hermes
func (t *adapterTask) Cancel() {
	if t.cancel != nil {
		t.cancel()
	}
}

// Blocks until the task finishes or ctx ends, the task error once finished
func (t *adapterTask) Wait(ctx context.Context) error {
	select {
	case <-t.done:
		return t.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Runs fn when the task finishes, at once for a finished task
func (t *adapterTask) AddDoneCallback(fn func()) { t.addDoneCallback(fn) }

// Publishes the connected but degraded runtime status while polling recovers
func (b *BasePlatformAdapter) MarkDegraded(ctx context.Context) { b.markDegraded(ctx) }

// Writes the runtime status of the adapter, failures logged and never raised
func (b *BasePlatformAdapter) WriteRuntimeStatusSafe(ctx context.Context, statusContext string, fields map[string]any) {
	b.writeRuntimeStatusSafe(ctx, statusContext, fields)
}

// Assigns the running flag directly the way adapters assign their running attribute
func (b *BasePlatformAdapter) SetRunning(on bool) {
	b.mu.Lock()
	b.running = on
	b.mu.Unlock()
}

// URL safety env of the readonly config for adapters outside the package
func URLSafetyEnv(ctx context.Context) *tools.Env { return urlSafetyEnv(ctx) }

// Whether a send error text names a transient network failure worth retrying
func IsRetryableErrorText(errText string) bool { return isRetryableError(errText) }

// Redirect target of a response for adapters outside the package, empty when it is not a redirect
func RedirectTargetFromResponse(resp *http.Response) string { return redirectTargetFromResponse(resp) }
