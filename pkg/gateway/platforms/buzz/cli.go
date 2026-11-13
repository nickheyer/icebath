package buzz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	cliTimeout         = 30 * time.Second
	maxCLIMessageChars = 900
	cliTimeoutExitCode = 124
	cliUnknownExitCode = 4
)

// Exit code and captured output of one buzz CLI run
type cliResult struct {
	code           int
	stdout, stderr string
}

// Runs the buzz CLI as argv with its key passed only through the child environment, a timeout reported as exit 124
func runBuzz(ctx context.Context, cliPath string, args []string, relayURL, privateKey, authTag string, input *string) (cliResult, error) {
	env, err := tools.IcebathSubprocessEnvChecked(&tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}, false, nil)
	if err != nil {
		return cliResult{}, err
	}
	if real := env["ICEBATH_REAL_HOME"]; real != "" {
		env["HOME"] = real
	}
	env["BUZZ_RELAY_URL"] = relayURL
	env["BUZZ_PRIVATE_KEY"] = privateKey
	delete(env, "BUZZ_AUTH_TAG")
	if authTag != "" {
		env["BUZZ_AUTH_TAG"] = authTag
	}
	environ := make([]string, 0, len(env))
	for k, v := range env {
		environ = append(environ, k+"="+v)
	}
	cctx, cancel := context.WithTimeout(ctx, cliTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, cliPath, args...)
	cmd.Env = environ
	cmd.WaitDelay = 5 * time.Second
	if input != nil {
		cmd.Stdin = strings.NewReader(*input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return cliResult{}, ctx.Err()
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		sub := ""
		if len(args) > 0 {
			sub = args[0]
		}
		detail, _ := json.Marshal(map[string]string{"error": "timeout", "message": fmt.Sprintf("buzz %s timed out after %s", sub, cliTimeout)})
		return cliResult{code: cliTimeoutExitCode, stderr: string(detail)}, nil
	}
	res := cliResult{stdout: strings.ToValidUTF8(stdout.String(), "�"), stderr: strings.ToValidUTF8(stderr.String(), "�")}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		res.code = 0
	case errors.As(runErr, &exitErr):
		res.code = exitErr.ExitCode()
		if res.code < 0 {
			res.code = cliUnknownExitCode
		}
	default:
		return cliResult{}, runErr
	}
	return res, nil
}

// Untrusted CLI detail bounded in length with a local file path reduced to its base name
func boundedCLIMessage(message, redactPath string) string {
	if redactPath != "" {
		message = strings.ReplaceAll(message, redactPath, filepath.Base(redactPath))
	}
	runes := []rune(message)
	if len(runes) <= maxCLIMessageChars {
		return message
	}
	return string(runes[:maxCLIMessageChars-3]) + "..."
}

// Bounded human readable message of the CLI error contract on stderr
func cliErrorMessage(stderr string, code int, redactPath string) string {
	text := strip(stderr)
	if data, ok := jsonOr(text, nil).(map[string]any); ok {
		detail, _ := data["message"].(string)
		if strip(detail) != "" {
			label := "error"
			if category, ok := data["error"].(string); ok && strip(category) != "" {
				label = strip(category)
			}
			return boundedCLIMessage(fmt.Sprintf("%s: %s (exit %d)", label, strip(detail), code), redactPath)
		}
	}
	if text == "" {
		text = fmt.Sprintf("buzz CLI failed with exit code %d", code)
	}
	return boundedCLIMessage(text, redactPath)
}

// Event id of an accepted send receipt, or the reason the receipt is not one
func parseSendReceipt(stdout string) (string, string) {
	data, ok := jsonOr(stdout, nil).(map[string]any)
	if !ok {
		return "", "invalid CLI response"
	}
	if accepted, isBool := data["accepted"].(bool); isBool && !accepted {
		detail, _ := data["message"].(string)
		if strip(detail) == "" {
			detail = "message was not accepted"
		}
		return "", boundedCLIMessage(strip(detail), "")
	}
	eventID, _ := data["event_id"].(string)
	if accepted, isBool := data["accepted"].(bool); !isBool || !accepted || strip(eventID) == "" {
		return "", "invalid CLI response"
	}
	return strip(eventID), ""
}
