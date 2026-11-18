package tuiserver

import (
	"context"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Seam over the engine install
var syncSpeechEngine = func(ctx context.Context, extra string) error {
	return pm.SyncVenv(ctx, pm.SyncVenvOptions{Extras: []string{extra}, Explicit: true})
}

// Adds a speech engine to the dependency union of this install, printing the outcome
func installSpeechEngine(ctx context.Context, extra string, out io.Writer) bool {
	if err := syncSpeechEngine(ctx, extra); err != nil {
		detail := err.Error()
		if pypath.IsOSError(err) {
			detail = pypath.OSErrorText(err)
		}
		fmt.Fprintf(out, "Failed to install %s: %s\nRetry with /setup tts\n", extra, detail)
		return false
	}
	fmt.Fprintf(out, "%s installed. Restart Icebath to use it.\n", extra)
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
