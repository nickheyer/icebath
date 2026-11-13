//go:build !goolm

package matrix

import (
	"context"
	"fmt"
	"log/slog"

	"maunium.net/go/mautrix"
)

// The build fact and the Go command that includes end to end encryption
const e2eeInstallHint = E2EEMissingFact

// False in this build, compiled without the goolm tag and so without an olm implementation
const E2EEBuiltIn = false

// Whether this binary carries an olm implementation, false without the goolm build tag
func checkE2EEDeps() bool { return E2EEBuiltIn }

// Cause named when encrypted events drop while E2EE is off
func encryptedDropOffCause() string {
	return e2eeInstallHint + ", then set MATRIX_E2EE_MODE=optional (or required)"
}

// Optional mode continues without encrypted rooms and required mode refuses to connect, this build having no olm
func (a *MatrixAdapter) connectSetupE2EE(ctx context.Context, client *mautrix.Client, stateStore mautrix.StateStore) bool {
	if a.e2eeMode == "optional" {
		slog.Warn(fmt.Sprintf("Matrix: E2EE optional but dependencies are missing. Continuing without "+
			"encrypted-room support. %s", e2eeInstallHint))
		a.mu.Lock()
		a.encryption = false
		a.mu.Unlock()
		return true
	}
	slog.Error(fmt.Sprintf("Matrix: E2EE is required but dependencies are missing. %s. Refusing to connect — "+
		"encrypted rooms would silently fail.", e2eeInstallHint))
	return abortConnect(client, nil)
}
