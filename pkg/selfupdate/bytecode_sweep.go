package selfupdate

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Clears stale bytecode caches once when the checkout fingerprint changed since the last sweep, never failing
func SweepStaleBytecodeIfCheckoutChanged() {
	root := fleetProjectRoot()
	fingerprint, ok := pm.ReadGitRevisionFingerprint(root)
	if !ok || fingerprint == "" {
		return
	}
	recorded := ""
	if raw, err := os.ReadFile(filepath.Join(root, bytecodeFingerprintFile)); err == nil {
		text, err := pystr.DecodeUTF8Sig(raw)
		if err != nil {
			slog.Debug(fmt.Sprintf("Stale-bytecode launch sweep failed: %s", err.Error()))
			return
		}
		recorded = pystr.Strip(text)
	}
	if recorded == fingerprint {
		return
	}
	if removed := clearBytecodeCache(root); removed > 0 {
		shown := recorded
		if shown == "" {
			shown = "unknown"
		}
		suffix := "ies"
		if removed == 1 {
			suffix = "y"
		}
		slog.Info(fmt.Sprintf("Checkout changed since last launch (%s -> %s): cleared %d stale __pycache__ director%s",
			shown, fingerprint, removed, suffix))
	}
	recordBytecodeFingerprint()
}
