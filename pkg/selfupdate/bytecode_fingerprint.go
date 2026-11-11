package selfupdate

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Stamp file holding the checkout fingerprint of the last bytecode sweep
const bytecodeFingerprintFile = ".bytecode-fingerprint"

// Persists the checkout fingerprint after a bytecode sweep through a tmp sibling, never failing
func recordBytecodeFingerprint() {
	root := fleetProjectRoot()
	fingerprint, ok := pm.ReadGitRevisionFingerprint(root)
	if !ok || fingerprint == "" {
		return
	}
	stamp := filepath.Join(root, bytecodeFingerprintFile)
	tmp := stamp + ".tmp"
	err := os.WriteFile(tmp, []byte(fingerprint), 0o666)
	if err == nil {
		err = os.Rename(tmp, stamp)
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not record bytecode fingerprint: %s", launchErrorText(err)))
	}
}
