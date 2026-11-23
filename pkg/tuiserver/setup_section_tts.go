package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Seam over the engine install, its progress written to the setup job
var syncSpeechEngine = func(ctx context.Context, extra string, out io.Writer) error {
	_, err := pm.EnsureExtra(ctx, extra, pm.ExtraOptions{Output: out})
	return err
}

// Provisions a speech engine's extra in its own environment, printing the progress and outcome
func installSpeechEngine(ctx context.Context, extra string, out io.Writer) bool {
	if err := syncSpeechEngine(ctx, extra, out); err != nil {
		detail := err.Error()
		var install *pm.InstallError
		switch {
		case errors.As(err, &install):
			remedy := install.Remedy
			if remedy != "" {
				remedy = strings.ToUpper(remedy[:1]) + remedy[1:]
			}
			detail = strings.TrimSpace(install.Cause + "\n" + remedy)
		case pypath.IsOSError(err):
			detail = pypath.OSErrorText(err)
		}
		fmt.Fprintf(out, "Failed to install %s: %s\nRetry with /setup voice\n", extra, detail)
		return false
	}
	fmt.Fprintf(out, "%s installed and ready to speak.\n", extra)
	return true
}

func init() {
	setupParamActions["xai.oauth.login"] = func(ctx context.Context, h string, _ map[string]string, out io.Writer) bool {
		return provider.XAIOAuthSectionLogin(setupPoolEnv(ctx, h), out)
	}
	setupParamActions["tts.install.neutts"] = func(ctx context.Context, _ string, _ map[string]string, out io.Writer) bool {
		fmt.Fprintln(out, "Installing neutts Python package...")
		fmt.Fprintln(out, "This will also download the TTS model (~300MB) on first use.")
		return installSpeechEngine(ctx, "neutts", out)
	}
	setupParamActions["tts.install.kittentts"] = func(ctx context.Context, _ string, _ map[string]string, out io.Writer) bool {
		fmt.Fprintln(out, "Installing kittentts Python package (~25-80MB model downloaded on first use)...")
		return installSpeechEngine(ctx, "kittentts", out)
	}
}
