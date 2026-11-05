package pm

import (
	"context"
	_ "embed"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

//go:embed marker_eval.py
var markerEvalSource []byte

// Path of the script that evaluates one PEP 508 marker inside a runtime that ships packaging
func MarkerEvalScript(ctx context.Context) (string, error) {
	return materializeScript(ctx, "_marker_eval.py", markerEvalSource)
}

// Evaluates a marker in the isolated runtime, any failure leaving the marker ungated as true
func EvaluateMarkerInRuntime(ctx context.Context, marker string, environment map[string]string, opts RuntimeOptions) bool {
	script, err := MarkerEvalScript(ctx)
	if err != nil {
		return true
	}
	command, err := RuntimeCommand(ctx, script, []string{marker, pyJSONCompact(environment)}, opts)
	if err != nil {
		return true
	}
	env, err := RuntimeEnvironment(ctx)
	if err != nil {
		return true
	}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, command[0], command[1:]...)
	cmd.Env = EnvList(env)
	cmd.WaitDelay = time.Second
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return true
	}
	if !utf8.ValidString(stdout.String()) {
		return true
	}
	return pystr.Strip(universalNewlines(stdout.String())) == "1"
}
