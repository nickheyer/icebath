//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/pm"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
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
		text, err := textutil.DecodeUTF8(textutil.TrimBOM(raw))
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("Stale-bytecode launch sweep failed: %s", err.Error()))
			return
		}
		recorded = strings.TrimSpace(text)
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
		pkgLog().Info(fmt.Sprintf("Checkout changed since last launch (%s -> %s): cleared %d stale __pycache__ director%s",
			shown, fingerprint, removed, suffix))
	}
	recordBytecodeFingerprint()
}
