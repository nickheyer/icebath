package pm

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Designated requirement identifier that keeps TCC identity stable across managed Python generations
const managedPythonIdentifier = "ai.icebath.managed-python"

// Platform the signer runs on, replaced by tests the way they patch platform system
var signingPlatform = func() string { return runtime.GOOS }

// Ad hoc signs a managed Python with a stable identifier on macOS, false elsewhere or when signing fails
func SignManagedPython(python string) bool {
	if signingPlatform() != "darwin" {
		return false
	}
	path := os.Getenv("PATH")
	codesign := whichOn("codesign", &path)
	if codesign == "" {
		slog.Info("macOS codesign is unavailable; using the downloaded Python signature")
		return false
	}
	output := func(r processResult, fallback string) string {
		text := strings.ToValidUTF8(string(r.Stderr), "�")
		if text == "" {
			text = strings.ToValidUTF8(string(r.Stdout), "�")
		}
		if text == "" {
			text = fallback
		}
		return pystr.Strip(text)
	}
	signed, err := runProcess(processSpec{Argv: []string{codesign, "--force", "--deep", "--sign", "-", "--timestamp=none",
		"--identifier", managedPythonIdentifier, "--requirements", fmt.Sprintf(`=designated => identifier "%s"`, managedPythonIdentifier), python},
		Capture: true})
	if err != nil {
		slog.Warn(fmt.Sprintf("could not sign managed Python %s: %s", python, pyExceptionText(err)))
		return false
	}
	if signed.Code != 0 {
		slog.Warn(fmt.Sprintf("could not stably sign managed Python %s: %s", python, output(signed, "codesign failed")))
		return false
	}
	verified, err := runProcess(processSpec{Argv: []string{codesign, "--verify", "--deep", "--strict", python}, Capture: true})
	if err != nil {
		slog.Warn(fmt.Sprintf("could not sign managed Python %s: %s", python, pyExceptionText(err)))
		return false
	}
	if verified.Code != 0 {
		slog.Warn(fmt.Sprintf("macOS signature verification failed for managed Python %s: %s", python, output(verified, "verification failed")))
		return false
	}
	return true
}
