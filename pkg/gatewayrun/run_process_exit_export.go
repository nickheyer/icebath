package gatewayrun

import "context"

// Releases the pid file and lock, stamps the clean exit, flushes metrics and logs, then hard exits with code
func ExitAfterGracefulShutdown(ctx context.Context, exitCode int) {
	exitAfterGracefulShutdown(ctx, exitCode)
}
